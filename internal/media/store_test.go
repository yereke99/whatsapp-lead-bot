package media

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayran/whatsapp-automation/internal/domain"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := NewStore(t.TempDir(), 4, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// Minimal valid file headers, enough for content sniffing to identify them.
var (
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{0x20}, 600)...)
	pngBytes  = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0x00}, 600)...)
	oggBytes  = append([]byte{'O', 'g', 'g', 'S', 0x00, 0x02}, bytes.Repeat([]byte{0x00}, 600)...)
	pdfBytes  = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x20}, 600)...)
)

func TestSaveAcceptsAllowedTypes(t *testing.T) {
	store := newTestStore(t)

	cases := []struct {
		name     string
		filename string
		data     []byte
		wantKind domain.MediaKind
	}{
		{"jpeg", "photo.jpg", jpegBytes, domain.MediaImage},
		{"png", "banner.png", pngBytes, domain.MediaImage},
		{"ogg", "voice.ogg", oggBytes, domain.MediaVoice},
		{"pdf", "guide.pdf", pdfBytes, domain.MediaDocument},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, err := store.Save(bytes.NewReader(tc.data), tc.filename, "")
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if stored.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", stored.Kind, tc.wantKind)
			}
			if stored.Size != int64(len(tc.data)) {
				t.Errorf("Size = %d, want %d", stored.Size, len(tc.data))
			}
			if stored.Checksum == "" {
				t.Error("checksum was not computed")
			}

			abs, err := store.AbsPath(stored.RelativePath)
			if err != nil {
				t.Fatalf("AbsPath: %v", err)
			}
			if _, err := os.Stat(abs); err != nil {
				t.Errorf("stored file is missing: %v", err)
			}
		})
	}
}

// TestSaveRejectsDisallowedExtensions is the first line of defence against a
// script or binary being uploaded and later served.
func TestSaveRejectsDisallowedExtensions(t *testing.T) {
	store := newTestStore(t)

	cases := []string{
		"payload.exe",
		"script.sh",
		"page.html",
		"code.js",
		"archive.zip",
		"noextension",
		"photo.jpg.exe",
	}

	for _, filename := range cases {
		_, err := store.Save(bytes.NewReader(jpegBytes), filename, "")
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("Save(%q) = %v, want ErrUnsupported", filename, err)
		}
	}
}

// TestSaveRejectsContentTypeMismatch is what stops an executable renamed to
// .jpg from being accepted on the strength of its filename alone.
func TestSaveRejectsContentTypeMismatch(t *testing.T) {
	store := newTestStore(t)

	machO := append([]byte{0xCF, 0xFA, 0xED, 0xFE}, bytes.Repeat([]byte{0x00}, 600)...)

	_, err := store.Save(bytes.NewReader(machO), "innocent.jpg", "")
	if !errors.Is(err, ErrMimeMismatch) {
		t.Errorf("Save = %v, want ErrMimeMismatch", err)
	}
}

func TestSaveRejectsEmptyFile(t *testing.T) {
	store := newTestStore(t)

	if _, err := store.Save(bytes.NewReader(nil), "empty.jpg", ""); !errors.Is(err, ErrEmptyFile) {
		t.Errorf("Save = %v, want ErrEmptyFile", err)
	}
}

func TestSaveEnforcesSizeLimit(t *testing.T) {
	store, err := NewStore(t.TempDir(), 1, "") // 1 MiB
	if err != nil {
		t.Fatal(err)
	}

	oversized := append(jpegBytes, bytes.Repeat([]byte{0x20}, 2<<20)...)

	if _, err := store.Save(bytes.NewReader(oversized), "big.jpg", ""); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Save = %v, want ErrTooLarge", err)
	}

	// The rejected upload must not be left behind on disk.
	entries, _ := filepath.Glob(filepath.Join(store.Root(), "*", "*", "*"))
	if len(entries) != 0 {
		t.Errorf("an oversized upload left %d file(s) on disk", len(entries))
	}
}

// TestStoredNameIsGenerated confirms the user's filename never reaches the
// filesystem, which removes traversal and collision as a class of problem.
func TestStoredNameIsGenerated(t *testing.T) {
	store := newTestStore(t)

	stored, err := store.Save(bytes.NewReader(jpegBytes), "../../etc/passwd.jpg", "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if strings.Contains(stored.StoredName, "..") || strings.Contains(stored.StoredName, "/") {
		t.Errorf("stored name derived from user input: %q", stored.StoredName)
	}
	if !strings.HasSuffix(stored.StoredName, ".jpg") {
		t.Errorf("extension was not preserved: %q", stored.StoredName)
	}

	abs, err := store.AbsPath(stored.RelativePath)
	if err != nil {
		t.Fatalf("AbsPath: %v", err)
	}
	if !strings.HasPrefix(abs, store.Root()) {
		t.Errorf("stored path escaped the media root: %q", abs)
	}
}

func TestAbsPathRejectsTraversal(t *testing.T) {
	store := newTestStore(t)

	cases := []string{
		"../../../etc/passwd",
		"..%2f..%2fetc/passwd",
		"/etc/passwd",
		"2026/01/../../../../etc/passwd",
		"",
	}

	for _, path := range cases {
		abs, err := store.AbsPath(path)
		if err != nil {
			continue // rejected outright, which is the desired outcome
		}
		if !strings.HasPrefix(abs, store.Root()) {
			t.Errorf("AbsPath(%q) escaped the root: %q", path, abs)
		}
	}
}

func TestVoiceKindNarrowing(t *testing.T) {
	store := newTestStore(t)

	// An .ogg upload defaults to VOICE but may be stored as AUDIO on request.
	stored, err := store.Save(bytes.NewReader(oggBytes), "clip.ogg", domain.MediaAudio)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if stored.Kind != domain.MediaAudio {
		t.Errorf("Kind = %v, want AUDIO", stored.Kind)
	}

	// A declared kind from another family must not override the sniffed one.
	image, err := store.Save(bytes.NewReader(jpegBytes), "photo.jpg", domain.MediaVideo)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if image.Kind != domain.MediaImage {
		t.Errorf("Kind = %v, want IMAGE: a mismatched declaration must be ignored", image.Kind)
	}
}

func TestSafeDownloadName(t *testing.T) {
	cases := map[string]string{
		"normal.pdf":       "normal.pdf",
		"../../etc/passwd": "passwd",
		`we"ird:name*.pdf`: "we_ird_name_.pdf",
		"":                 "file",
		"   ":              "file",
		"...":              "file",
		"файл құжат.pdf":   "файл құжат.pdf",
	}

	for input, want := range cases {
		if got := SafeDownloadName(input, ""); got != want {
			t.Errorf("SafeDownloadName(%q) = %q, want %q", input, got, want)
		}
	}

	long := strings.Repeat("a", 300) + ".pdf"
	if got := SafeDownloadName(long, ""); len(got) > 120 {
		t.Errorf("long name was not truncated: %d chars", len(got))
	}
}

func TestExtensionForMIME(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":             ".jpg",
		"image/png":              ".png",
		"audio/ogg; codecs=opus": ".ogg",
		"audio/mpeg":             ".mp3",
		"video/mp4":              ".mp4",
		"application/pdf":        ".pdf",
	}

	for mime, want := range cases {
		if got := ExtensionForMIME(mime); got != want {
			t.Errorf("ExtensionForMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestKindForMIME(t *testing.T) {
	cases := map[string]domain.MediaKind{
		"image/jpeg":             domain.MediaImage,
		"video/mp4":              domain.MediaVideo,
		"audio/ogg; codecs=opus": domain.MediaVoice,
		"audio/mpeg":             domain.MediaAudio,
		"application/pdf":        domain.MediaDocument,
		"":                       domain.MediaDocument,
	}

	for mime, want := range cases {
		if got := KindForMIME(mime); got != want {
			t.Errorf("KindForMIME(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	store := newTestStore(t)

	stored, err := store.Save(bytes.NewReader(jpegBytes), "photo.jpg", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Remove(stored.RelativePath); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := store.Remove(stored.RelativePath); err != nil {
		t.Errorf("removing a missing file should not error: %v", err)
	}
}
