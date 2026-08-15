package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

// Service combines validated storage, database bookkeeping and audio
// transcoding into the operations the API layer calls.
type Service struct {
	store      *Store
	repo       *Repository
	transcoder *Transcoder
	log        *slog.Logger
}

func NewService(store *Store, repo *Repository, transcoder *Transcoder, log *slog.Logger) *Service {
	return &Service{
		store:      store,
		repo:       repo,
		transcoder: transcoder,
		log:        log.With(slog.String("component", "media")),
	}
}

func (s *Service) Store() *Store           { return s.store }
func (s *Service) Transcoder() *Transcoder { return s.transcoder }

// Upload stores an admin-supplied file.
//
// When VOICE is requested and the source is not already OGG/Opus, the file is
// transcoded and both artefacts are kept: the original for the admin to
// re-download, and the converted file as the one actually sent. Sending the
// original as a plain attachment instead would silently turn an intended voice
// note into a music file, so a missing transcoder is a hard error here.
func (s *Service) Upload(ctx context.Context, src io.Reader, filename string, kind domain.MediaKind, adminID *uuid.UUID) (*domain.MediaFile, error) {
	stored, err := s.store.Save(src, filename, kind)
	if err != nil {
		return nil, err
	}

	probe := s.transcoder.Inspect(ctx, stored.AbsolutePath)

	record := &domain.MediaFile{
		OriginalName: SafeDownloadName(filename, ""),
		StoredName:   stored.StoredName,
		RelativePath: stored.RelativePath,
		MimeType:     stored.MimeType,
		SizeBytes:    stored.Size,
		Kind:         stored.Kind,
		Checksum:     stored.Checksum,
		DurationMS:   probe.DurationMS,
		Width:        probe.Width,
		Height:       probe.Height,
		UploadedBy:   adminID,
	}

	if err := s.repo.Create(ctx, nil, record); err != nil {
		_ = s.store.Remove(stored.RelativePath)
		return nil, err
	}
	record.URL = s.store.PublicURL(record.ID)

	if kind != domain.MediaVoice {
		return record, nil
	}

	converted, err := s.ensureVoice(ctx, record, adminID)
	if err != nil {
		return nil, err
	}
	return converted, nil
}

// ensureVoice returns a media record guaranteed to be OGG/Opus, transcoding if
// necessary and linking the derivative back to its source.
func (s *Service) ensureVoice(ctx context.Context, source *domain.MediaFile, adminID *uuid.UUID) (*domain.MediaFile, error) {
	absPath, err := s.store.AbsPath(source.RelativePath)
	if err != nil {
		return nil, err
	}

	if s.transcoder.IsVoiceReady(ctx, absPath) {
		return source, nil
	}

	if !s.transcoder.Available() {
		return nil, fmt.Errorf(
			"%w: install ffmpeg to send voice messages, or upload an OGG/Opus file",
			ErrFFmpegMissing)
	}

	tmpPath, err := s.transcoder.ToVoiceOpus(ctx, absPath)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpPath)

	tmpFile, err := os.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("open converted audio: %w", err)
	}
	defer tmpFile.Close()

	baseName := strings.TrimSuffix(source.OriginalName, extOf(source.OriginalName))
	if baseName == "" {
		baseName = "voice"
	}

	stored, err := s.store.Save(tmpFile, baseName+".ogg", domain.MediaVoice)
	if err != nil {
		return nil, fmt.Errorf("store converted audio: %w", err)
	}

	probe := s.transcoder.Inspect(ctx, stored.AbsolutePath)

	record := &domain.MediaFile{
		OriginalName: baseName + ".ogg",
		StoredName:   stored.StoredName,
		RelativePath: stored.RelativePath,
		MimeType:     "audio/ogg; codecs=opus",
		SizeBytes:    stored.Size,
		Kind:         domain.MediaVoice,
		Checksum:     stored.Checksum,
		DurationMS:   probe.DurationMS,
		SourceFileID: &source.ID,
		UploadedBy:   adminID,
	}

	if err := s.repo.Create(ctx, nil, record); err != nil {
		_ = s.store.Remove(stored.RelativePath)
		return nil, err
	}
	record.URL = s.store.PublicURL(record.ID)

	s.log.Info("audio converted for voice message",
		slog.String("source_id", source.ID.String()),
		slog.String("voice_id", record.ID.String()),
		slog.Int64("bytes", record.SizeBytes))

	return record, nil
}

// SaveInbound persists a file received from the provider.
func (s *Service) SaveInbound(ctx context.Context, data []byte, filename, mimeType string) (*domain.MediaFile, error) {
	if len(data) == 0 {
		return nil, ErrEmptyFile
	}

	kind := KindForMIME(mimeType)
	name := SafeDownloadName(filename, ExtensionForMIME(mimeType))
	if extOf(name) == "" {
		name += ExtensionForMIME(mimeType)
	}

	stored, err := s.store.Save(newByteReader(data), name, kind)
	if err != nil {
		return nil, err
	}

	probe := s.transcoder.Inspect(ctx, stored.AbsolutePath)

	record := &domain.MediaFile{
		OriginalName: name,
		StoredName:   stored.StoredName,
		RelativePath: stored.RelativePath,
		MimeType:     stored.MimeType,
		SizeBytes:    stored.Size,
		Kind:         stored.Kind,
		Checksum:     stored.Checksum,
		DurationMS:   probe.DurationMS,
		Width:        probe.Width,
		Height:       probe.Height,
	}

	if err := s.repo.Create(ctx, nil, record); err != nil {
		_ = s.store.Remove(stored.RelativePath)
		return nil, err
	}
	record.URL = s.store.PublicURL(record.ID)
	return record, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*domain.MediaFile, error) {
	m, err := s.repo.GetByID(ctx, id)
	if err != nil || m == nil {
		return nil, err
	}
	m.URL = s.store.PublicURL(m.ID)
	return m, nil
}

func (s *Service) List(ctx context.Context, kind string, limit, offset int) ([]domain.MediaFile, int, error) {
	items, total, err := s.repo.List(ctx, kind, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	for i := range items {
		items[i].URL = s.store.PublicURL(items[i].ID)
	}
	return items, total, nil
}

var ErrMediaInUse = errors.New("media file is still referenced by a template")

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	count, err := s.repo.InUse(ctx, id)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w (%d templates)", ErrMediaInUse, count)
	}

	relPath, err := s.repo.Delete(ctx, id)
	if err != nil {
		if sqlite.IsForeignKeyViolation(err) {
			return ErrMediaInUse
		}
		return err
	}

	if err := s.store.Remove(relPath); err != nil {
		// The row is gone; a leftover blob is a housekeeping issue, not a
		// failure the operator needs to retry.
		s.log.Warn("media row deleted but file remained",
			slog.String("path", relPath), slog.String("error", err.Error()))
	}
	return nil
}

// OpenContent returns a reader plus the metadata needed to serve the file.
func (s *Service) OpenContent(ctx context.Context, id uuid.UUID) (*os.File, *domain.MediaFile, error) {
	record, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if record == nil {
		return nil, nil, nil
	}

	f, err := s.store.Open(record.RelativePath)
	if err != nil {
		return nil, nil, err
	}
	return f, record, nil
}

func extOf(name string) string {
	idx := strings.LastIndexByte(name, '.')
	if idx < 0 {
		return ""
	}
	return name[idx:]
}

// newByteReader avoids the extra copy strings.NewReader(string(b)) would make.
func newByteReader(b []byte) io.Reader { return &byteReader{data: b} }

type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
