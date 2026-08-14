package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/internal/media"
)

func (s *Server) handleListMedia(w http.ResponseWriter, r *http.Request) {
	kind := strings.ToUpper(httpx.QueryString(r, "kind"))
	if kind != "" && !domain.ValidMediaKind(kind) {
		kind = ""
	}

	limit := httpx.QueryInt(r, "limit", 50, 1, 200)
	offset := httpx.QueryInt(r, "offset", 0, 0, 1_000_000)

	items, total, err := s.deps.Media.List(r.Context(), kind, limit, offset)
	if err != nil {
		httpx.Internal(w, s.log, err, "list media")
		return
	}
	if items == nil {
		items = []domain.MediaFile{}
	}
	httpx.Paged(w, items, total, limit, offset)
}

// handleUploadMedia accepts a multipart upload.
//
// The declared kind only narrows within a family (an .ogg file may be marked
// VOICE rather than AUDIO); the actual type is decided by sniffing the
// content, never by the filename or the client's Content-Type.
func (s *Server) handleUploadMedia(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	maxBytes := s.deps.Media.Store().MaxBytes()
	// Leave headroom for multipart framing on top of the file itself.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))

	// Keep only a small part in memory; the rest spools to a temp file.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.Fail(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge,
				fmt.Sprintf("Файл тым үлкен (шегі %d МБ)", maxBytes>>20))
			return
		}
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, "Файлды оқу мүмкін болмады")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "«file» өрісі қажет")
		return
	}
	defer file.Close()

	if header.Size > maxBytes {
		httpx.Fail(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge,
			fmt.Sprintf("Файл тым үлкен (шегі %d МБ)", maxBytes>>20))
		return
	}

	kind := domain.MediaKind(strings.ToUpper(strings.TrimSpace(r.FormValue("kind"))))
	if kind != "" && !domain.ValidMediaKind(string(kind)) {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Файл түрі жарамсыз")
		return
	}

	record, err := s.deps.Media.Upload(r.Context(), file, header.Filename, kind, adminID(r))
	if err != nil {
		switch {
		case errors.Is(err, media.ErrTooLarge):
			httpx.Fail(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge,
				fmt.Sprintf("Файл тым үлкен (шегі %d МБ)", maxBytes>>20))
		case errors.Is(err, media.ErrUnsupported):
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation,
				"Бұл файл түріне рұқсат жоқ")
		case errors.Is(err, media.ErrMimeMismatch):
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation,
				"Файл мазмұны кеңейтіміне сәйкес келмейді")
		case errors.Is(err, media.ErrEmptyFile):
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Файл бос")
		case errors.Is(err, media.ErrFFmpegMissing):
			httpx.Fail(w, http.StatusServiceUnavailable, httpx.CodeNotConfigured,
				"Дауыстық хабарлама үшін ffmpeg қажет немесе OGG/Opus файлын жүктеңіз")
		default:
			httpx.Internal(w, s.log, err, "upload media")
		}
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionMediaUploaded,
		EntityType: "media",
		EntityID:   record.ID.String(),
		Summary:    "Файл жүктелді: " + record.OriginalName,
		New: map[string]any{
			"kind": record.Kind,
			"size": record.SizeBytes,
			"mime": record.MimeType,
		},
	})

	httpx.JSON(w, http.StatusCreated, record)
}

// handleMediaContent streams a stored file to the browser.
func (s *Server) handleMediaContent(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	file, record, err := s.deps.Media.OpenContent(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "open media")
		return
	}
	if record == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Файл табылмады")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		httpx.Internal(w, s.log, err, "stat media")
		return
	}

	h := w.Header()
	h.Set("Content-Type", record.MimeType)
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// nosniff plus an explicit disposition stops a stored file from being
	// interpreted as script in the admin's browser.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=86400")

	disposition := "inline"
	if httpx.QueryBool(r, "download") || record.Kind == domain.MediaDocument {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s",
		disposition, urlEncode(record.OriginalName)))

	// ServeContent handles range requests, which audio and video players need
	// in order to seek.
	http.ServeContent(w, r, record.OriginalName, record.CreatedAt, file)
}

func (s *Server) handleDeleteMedia(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Media.Delete(r.Context(), id); err != nil {
		if errors.Is(err, media.ErrMediaInUse) {
			httpx.Fail(w, http.StatusConflict, httpx.CodeConflict,
				"Файл шаблондарда қолданылуда, алдымен шаблонды өзгертіңіз")
			return
		}
		httpx.Internal(w, s.log, err, "delete media")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionMediaDeleted,
		EntityType: "media",
		EntityID:   id.String(),
		Summary:    "Файл жойылды",
	})

	httpx.NoContent(w)
}

// urlEncode percent-encodes a filename for RFC 5987 Content-Disposition.
func urlEncode(name string) string {
	var b strings.Builder
	for _, c := range []byte(name) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
