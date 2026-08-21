// Package webconsole provides a static operations console for KPlayer.
//
// The console is a single-page frontend (HTML/CSS/JS, pure ASCII, no CDN
// dependencies) embedded in the binary with Go 1.16+ "embed". It talks to
// the KPlayer HTTP API exclusively through relative /console/api/... paths
// which this package rewrites to the backend (grpc-gateway) endpoints.
//
// A parent package can mount it with either of:
//
//	mux.Handle("/console/", webconsole.Handler)
//
// or with a custom configuration:
//
//	mux.Handle("/console/", webconsole.NewHandler(webconsole.Config{
//		BackendURL: "http://127.0.0.1:4156",
//		AuthToken:  "my-token",
//	}))
//
// The embedded assets are also exposed directly through FS for parent
// packages that want to serve the files themselves.
package webconsole

import (
	"bytes"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
)

// FS exposes the embedded static assets of the console. It is exported so
// parent packages can mount the raw files themselves.
//
//go:embed static
var FS embed.FS

const (
	// MountPrefix is the path prefix under which the console is served.
	MountPrefix = "/console"
	// APIPrefix is the path prefix under which requests are proxied to
	// the KPlayer HTTP API.
	APIPrefix = MountPrefix + "/api"
)

// Config controls the behaviour of the console handler.
type Config struct {
	// BackendURL is the base URL of the KPlayer HTTP API (grpc-gateway).
	// Empty means http://127.0.0.1:4156 (the default HTTP port).
	BackendURL string
	// AuthToken, when non-empty, is injected as the Authorization header
	// on every proxied API request. When empty, the client supplied
	// X-Console-Token header (set by the console settings dialog) is
	// mapped to Authorization instead.
	AuthToken string
	// StaticFS overrides the embedded static assets (mainly for tests).
	StaticFS fs.FS
}

// DefaultConfig returns a Config with sensible defaults. It honours the
// KPLAYER_CONSOLE_BACKEND and KPLAYER_CONSOLE_TOKEN environment variables.
func DefaultConfig() Config {
	backend := os.Getenv("KPLAYER_CONSOLE_BACKEND")
	if backend == "" {
		backend = "http://127.0.0.1:4156"
	}
	return Config{
		BackendURL: backend,
		AuthToken:  os.Getenv("KPLAYER_CONSOLE_TOKEN"),
	}
}

// Handler is the default console handler, ready to be mounted by a parent
// package (e.g. mux.Handle("/console/", webconsole.Handler)).
var Handler http.Handler = NewHandler(DefaultConfig())

// NewHandler builds an http.Handler that serves the console static assets
// under /console/ and proxies /console/api/* to the KPlayer HTTP API.
func NewHandler(cfg Config) http.Handler {
	if cfg.BackendURL == "" {
		cfg.BackendURL = "http://127.0.0.1:4156"
	}
	backend, err := url.Parse(cfg.BackendURL)
	if err != nil || backend.Host == "" {
		// Fall back to the default endpoint on invalid configuration.
		backend, _ = url.Parse("http://127.0.0.1:4156")
	}

	static := cfg.StaticFS
	if static == nil {
		static, err = fs.Sub(FS, "static")
		if err != nil {
			panic("webconsole: embedded static assets missing: " + err.Error())
		}
	}

	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.Director = func(req *http.Request) {
		// Rewrite /console/api/<path> to /<path> on the backend.
		p := strings.TrimPrefix(req.URL.Path, APIPrefix)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		req.URL.Path = p
		req.URL.RawPath = ""
		req.URL.Scheme = backend.Scheme
		req.URL.Host = backend.Host
		// The outbound HTTP request's Host header must target the backend;
		// keep it in sync with URL.Host so the backend sees its own host
		// rather than the client-facing one.
		req.Host = backend.Host

		// Authorization handling: a configured token wins, otherwise
		// honour the token entered in the console UI.
		if cfg.AuthToken != "" {
			req.Header.Set("Authorization", cfg.AuthToken)
		} else if token := req.Header.Get("X-Console-Token"); token != "" {
			req.Header.Set("Authorization", token)
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		body, _ := json.Marshal(map[string]interface{}{
			"code":    http.StatusBadGateway,
			"message": "console: cannot reach KPlayer API backend: " + err.Error(),
		})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(body)
	}

	return &consoleHandler{static: static, proxy: proxy}
}

type consoleHandler struct {
	static fs.FS
	proxy  *httputil.ReverseProxy
}

func (h *consoleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == MountPrefix:
		http.Redirect(w, r, MountPrefix+"/", http.StatusMovedPermanently)
	case r.URL.Path == MountPrefix+"/":
		h.serveAsset(w, r, "index.html")
	case r.URL.Path == APIPrefix || strings.HasPrefix(r.URL.Path, APIPrefix+"/"):
		h.proxy.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, MountPrefix+"/"):
		h.serveAsset(w, r, strings.TrimPrefix(r.URL.Path, MountPrefix+"/"))
	default:
		http.NotFound(w, r)
	}
}

// serveAsset serves a single file from the embedded static tree with an
// explicit content type so behaviour is identical across Go versions.
func (h *consoleHandler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	f, err := h.static.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// http.ServeContent requires an io.ReadSeeker, but fs.File only
	// guarantees Read/Stat/Close. Read the asset into memory (static assets
	// are small) and serve from a bytes.Reader so the code compiles against
	// every Go version and still honours conditional GET headers.
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "failed to read asset", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(data))
}

func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml; charset=utf-8"
	default:
		if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
			return ct
		}
		return "application/octet-stream"
	}
}
