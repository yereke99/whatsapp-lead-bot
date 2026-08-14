// Package media validates, stores and serves uploaded files, and converts
// audio into the format WhatsApp renders as a voice note.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
)

var (
	ErrTooLarge     = errors.New("file exceeds the maximum allowed size")
	ErrEmptyFile    = errors.New("file is empty")
	ErrUnsupported  = errors.New("file type is not allowed")
	ErrMimeMismatch = errors.New("file content does not match its extension")
)

// typeRule describes one permitted upload type.
type typeRule struct {
	kind       domain.MediaKind
	extensions []string
	// mimePrefixes are the acceptable sniffed content types. Sniffing is the
	// authority; the client-supplied Content-Type header is never trusted.
	mimePrefixes  []string
	canonicalMIME string
}

// allowlist is deliberately exhaustive. Anything not listed here is rejected,
// which keeps scripts, archives with executable payloads and unknown binary
// formats out of the media directory.
var allowlist = []typeRule{
	{domain.MediaImage, []string{".jpg", ".jpeg"}, []string{"image/jpeg"}, "image/jpeg"},
	{domain.MediaImage, []string{".png"}, []string{"image/png"}, "image/png"},
	{domain.MediaImage, []string{".webp"}, []string{"image/webp"}, "image/webp"},
	{domain.MediaImage, []string{".gif"}, []string{"image/gif"}, "image/gif"},

	{domain.MediaVideo, []string{".mp4", ".m4v"}, []string{"video/mp4"}, "video/mp4"},
	{domain.MediaVideo, []string{".mov"}, []string{"video/quicktime", "video/mp4"}, "video/quicktime"},
	{domain.MediaVideo, []string{".3gp"}, []string{"video/3gpp", "video/mp4"}, "video/3gpp"},
	{domain.MediaVideo, []string{".webm"}, []string{"video/webm"}, "video/webm"},

	{domain.MediaAudio, []string{".mp3"}, []string{"audio/mpeg"}, "audio/mpeg"},
	{domain.MediaAudio, []string{".wav"}, []string{"audio/wave", "audio/wav", "audio/x-wav"}, "audio/wav"},
	{domain.MediaAudio, []string{".m4a"}, []string{"audio/mp4", "video/mp4", "audio/x-m4a"}, "audio/mp4"},
	{domain.MediaAudio, []string{".aac"}, []string{"audio/aac", "audio/mpeg"}, "audio/aac"},
	{domain.MediaVoice, []string{".ogg", ".oga", ".opus"}, []string{"audio/ogg", "application/ogg"}, "audio/ogg; codecs=opus"},

	{domain.MediaDocument, []string{".pdf"}, []string{"application/pdf"}, "application/pdf"},
	{domain.MediaDocument, []string{".txt"}, []string{"text/plain"}, "text/plain; charset=utf-8"},
	{domain.MediaDocument, []string{".csv"}, []string{"text/plain", "text/csv"}, "text/csv"},
	{domain.MediaDocument, []string{".doc"}, []string{"application/msword", "application/x-ole-storage"}, "application/msword"},
	{domain.MediaDocument, []string{".xls"}, []string{"application/vnd.ms-excel", "application/x-ole-storage"}, "application/vnd.ms-excel"},
	{domain.MediaDocument, []string{".ppt"}, []string{"application/vnd.ms-powerpoint", "application/x-ole-storage"}, "application/vnd.ms-powerpoint"},
	// Office XML formats are ZIP containers, so sniffing reports zip.
	{domain.MediaDocument, []string{".docx"}, []string{"application/zip"},
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	{domain.MediaDocument, []string{".xlsx"}, []string{"application/zip"},
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	{domain.MediaDocument, []string{".pptx"}, []string{"application/zip"},
		"application/vnd.openxmlformats-officedocument.presentationml.presentation"},
}

// Store owns the media directory on disk.
type Store struct {
	root       string
	maxBytes   int64
	publicBase string
}

func NewStore(root string, maxUploadMB int64, publicBase string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve media root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create media root: %w", err)
	}
	return &Store{
		root:       abs,
		maxBytes:   maxUploadMB << 20,
		publicBase: strings.TrimRight(publicBase, "/"),
	}, nil
}

func (s *Store) Root() string    { return s.root }
func (s *Store) MaxBytes() int64 { return s.maxBytes }

// StoredFile is the result of persisting an upload.
type StoredFile struct {
	RelativePath string
	AbsolutePath string
	StoredName   string
	MimeType     string
	Kind         domain.MediaKind
	Size         int64
	Checksum     string
}

// Save validates and writes an upload.
//
// Validation order matters: the size cap is enforced while streaming so a
// hostile client cannot exhaust disk before the check runs, and the content
// type is decided by sniffing the first bytes rather than by trusting the
// filename or the client's header.
func (s *Store) Save(src io.Reader, originalName string, declaredKind domain.MediaKind) (*StoredFile, error) {
	ext := normalizedExt(originalName)
	if ext == "" {
		return nil, fmt.Errorf("%w: file has no extension", ErrUnsupported)
	}

	rules := rulesForExt(ext)
	if len(rules) == 0 {
		return nil, fmt.Errorf("%w: %s files are not accepted", ErrUnsupported, ext)
	}

	relDir := filepath.Join(time.Now().UTC().Format("2006"), time.Now().UTC().Format("01"))
	if err := os.MkdirAll(filepath.Join(s.root, relDir), 0o750); err != nil {
		return nil, fmt.Errorf("create media directory: %w", err)
	}

	// The stored name is generated, never derived from user input, which
	// removes path traversal and filename collision as a class of problem.
	storedName := uuid.NewString() + ext
	relPath := filepath.Join(relDir, storedName)
	absPath := filepath.Join(s.root, relPath)

	dst, err := os.OpenFile(absPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create media file: %w", err)
	}

	cleanup := func() {
		dst.Close()
		os.Remove(absPath)
	}

	hasher := sha256.New()
	sniff := make([]byte, 0, 512)

	// Read one byte past the limit so an oversized file is detected rather
	// than silently truncated.
	limited := io.LimitReader(src, s.maxBytes+1)
	buf := make([]byte, 64<<10)
	var written int64

	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			if len(sniff) < 512 {
				need := 512 - len(sniff)
				if need > n {
					need = n
				}
				sniff = append(sniff, buf[:need]...)
			}
			if written+int64(n) > s.maxBytes {
				cleanup()
				return nil, ErrTooLarge
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				cleanup()
				return nil, fmt.Errorf("write media file: %w", err)
			}
			hasher.Write(buf[:n])
			written += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			cleanup()
			return nil, fmt.Errorf("read upload: %w", readErr)
		}
	}

	if written == 0 {
		cleanup()
		return nil, ErrEmptyFile
	}

	sniffed := strings.ToLower(strings.TrimSpace(http.DetectContentType(sniff)))
	if idx := strings.IndexByte(sniffed, ';'); idx >= 0 {
		sniffed = strings.TrimSpace(sniffed[:idx])
	}

	rule, ok := matchRule(rules, sniffed)
	if !ok {
		cleanup()
		return nil, fmt.Errorf("%w: %s content declared as %s", ErrMimeMismatch, sniffed, ext)
	}

	// An explicitly requested kind may only narrow within the same family,
	// e.g. an .ogg upload marked as VOICE rather than AUDIO.
	kind := rule.kind
	if declaredKind != "" && compatibleKind(rule.kind, declaredKind) {
		kind = declaredKind
	}

	if err := dst.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("flush media file: %w", err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(absPath)
		return nil, fmt.Errorf("close media file: %w", err)
	}

	return &StoredFile{
		RelativePath: filepath.ToSlash(relPath),
		AbsolutePath: absPath,
		StoredName:   storedName,
		MimeType:     rule.canonicalMIME,
		Kind:         kind,
		Size:         written,
		Checksum:     hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// SaveBytes persists an in-memory payload, used for inbound media pulled from
// the provider and for transcoded audio.
func (s *Store) SaveBytes(data []byte, originalName string, kind domain.MediaKind) (*StoredFile, error) {
	return s.Save(strings.NewReader(string(data)), originalName, kind)
}

// AbsPath resolves a stored relative path and refuses anything that escapes
// the media root.
func (s *Store) AbsPath(relativePath string) (string, error) {
	if relativePath == "" {
		return "", errors.New("empty media path")
	}
	// Reject absolute paths and traversal before joining.
	clean := filepath.Clean("/" + filepath.FromSlash(relativePath))
	abs := filepath.Join(s.root, clean)

	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("media path %q escapes the storage root", relativePath)
	}
	return abs, nil
}

// Open returns a reader for a stored file.
func (s *Store) Open(relativePath string) (*os.File, error) {
	abs, err := s.AbsPath(relativePath)
	if err != nil {
		return nil, err
	}
	return os.Open(abs)
}

// Remove deletes a stored file. A missing file is not an error: the row may
// have outlived the blob after a restore.
func (s *Store) Remove(relativePath string) error {
	abs, err := s.AbsPath(relativePath)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// PublicURL builds the browser-facing url for a media id.
func (s *Store) PublicURL(id uuid.UUID) string {
	if s.publicBase != "" {
		return fmt.Sprintf("%s/api/media/%s/content", s.publicBase, id)
	}
	return fmt.Sprintf("/api/media/%s/content", id)
}

func normalizedExt(name string) string {
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(strings.TrimSpace(name))))
	// Guard against names like "photo.jpg.exe" reaching this far with a
	// disallowed extension, and against ".." fragments.
	if strings.ContainsAny(ext, `/\:`) {
		return ""
	}
	return ext
}

func rulesForExt(ext string) []typeRule {
	var out []typeRule
	for _, r := range allowlist {
		for _, e := range r.extensions {
			if e == ext {
				out = append(out, r)
			}
		}
	}
	return out
}

func matchRule(rules []typeRule, sniffed string) (typeRule, bool) {
	for _, r := range rules {
		for _, prefix := range r.mimePrefixes {
			if sniffed == prefix {
				return r, true
			}
		}
	}
	// Plain-text formats sniff as text/plain with a charset suffix already
	// stripped; treat any text/* as text for the text-ish document rules.
	if strings.HasPrefix(sniffed, "text/") {
		for _, r := range rules {
			for _, prefix := range r.mimePrefixes {
				if strings.HasPrefix(prefix, "text/") {
					return r, true
				}
			}
		}
	}
	return typeRule{}, false
}

func compatibleKind(detected, requested domain.MediaKind) bool {
	if detected == requested {
		return true
	}
	// Audio and voice share container formats and differ only in intent.
	return (detected == domain.MediaAudio && requested == domain.MediaVoice) ||
		(detected == domain.MediaVoice && requested == domain.MediaAudio)
}

// ExtensionForMIME suggests a filename extension for a provider-supplied MIME
// type, used when inbound media arrives without a usable filename.
func ExtensionForMIME(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if idx := strings.IndexByte(mimeType, ';'); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}

	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/3gpp":
		return ".3gp"
	case "audio/ogg", "application/ogg", "audio/opus":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/wav", "audio/wave", "audio/x-wav":
		return ".wav"
	case "application/pdf":
		return ".pdf"
	}

	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// KindForMIME maps a provider MIME type onto a storage kind.
func KindForMIME(mimeType string) domain.MediaKind {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return domain.MediaImage
	case strings.HasPrefix(mimeType, "video/"):
		return domain.MediaVideo
	case strings.Contains(mimeType, "ogg") || strings.Contains(mimeType, "opus"):
		return domain.MediaVoice
	case strings.HasPrefix(mimeType, "audio/"):
		return domain.MediaAudio
	default:
		return domain.MediaDocument
	}
}

// SafeDownloadName produces a filename safe to place in a Content-Disposition
// header and on any filesystem.
func SafeDownloadName(name, fallbackExt string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 32 || r == 127:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.Trim(name, " .")

	if name == "" {
		name = "file" + fallbackExt
	}
	if len(name) > 120 {
		ext := filepath.Ext(name)
		name = name[:120-len(ext)] + ext
	}
	return name
}
