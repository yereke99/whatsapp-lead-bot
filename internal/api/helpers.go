package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// contextWithTimeout bounds a handler's downstream work without outliving the
// request itself.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func defaultExportFilename(prefix, tz string) string {
	return fmt.Sprintf("%s_%s.csv", prefix, timex.FormatIn(time.Now().UTC(), tz, "2006-01-02_1504"))
}

// staticHandler serves the admin single-page app.
//
// Unknown paths fall back to index.html so client-side routing works on a hard
// refresh, but anything under /api/ returns a JSON 404 rather than the app
// shell, which would otherwise confuse a mistyped API call.
func (s *Server) staticHandler() http.Handler {
	root := s.cfg.HTTP.WebDir
	fileServer := http.FileServer(http.Dir(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Эндпоинт табылмады")
			return
		}

		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			clean = "index.html"
		}
		// filepath.Clean cannot fully normalise an encoded traversal, so any
		// remaining parent reference is rejected outright.
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, ".."+string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}

		full := filepath.Join(root, clean)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			// Unknown route: hand the app shell to the client router.
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}

		// Fingerprinting is not used, so assets must revalidate to avoid a
		// stale dashboard after deployment.
		switch filepath.Ext(clean) {
		case ".js", ".css":
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		case ".png", ".jpg", ".jpeg", ".svg", ".webp", ".ico", ".woff2":
			w.Header().Set("Cache-Control", "public, max-age=86400")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}

		fileServer.ServeHTTP(w, r)
	})
}
