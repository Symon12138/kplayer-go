package webconsole

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// newTestHandler returns a console handler backed by the embedded static
// assets and a backend whose URL is given ("" uses the default config path).
func newTestHandler(t *testing.T, backend string) http.Handler {
	t.Helper()
	cfg := Config{BackendURL: backend}
	return NewHandler(cfg)
}

func TestNewHandlerServesIndex(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:4156")
	req := httptest.NewRequest(http.MethodGet, MountPrefix+"/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s/ = %d, want %d", MountPrefix, rec.Code, http.StatusOK)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `app.js`) || !strings.Contains(body, `styles.css`) {
		t.Fatalf("index.html does not reference the assets: %q", body)
	}
}

func TestRedirectMountRoot(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:4156")
	req := httptest.NewRequest(http.MethodGet, MountPrefix, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET %s = %d, want %d", MountPrefix, rec.Code, http.StatusMovedPermanently)
	}
	if loc := rec.Header().Get("Location"); loc != MountPrefix+"/" {
		t.Fatalf("Location = %q, want %q", loc, MountPrefix+"/")
	}
}

func TestAssetsServedWithContentTypes(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:4156")
	cases := []struct {
		path, wantCT string
	}{
		{MountPrefix + "/styles.css", "text/css; charset=utf-8"},
		{MountPrefix + "/app.js", "text/javascript; charset=utf-8"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", c.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != c.wantCT {
			t.Fatalf("GET %s content-type = %q, want %q", c.path, ct, c.wantCT)
		}
	}
}

func TestUnknownAssetReturns404(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:4156")
	for _, p := range []string{
		MountPrefix + "/nope.txt",
		MountPrefix + "/static/nope.js",
		MountPrefix + "/app.js/extra",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestTraversalIsRejected(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:4156")
	for _, p := range []string{
		MountPrefix + "/../webconsole.go",
		MountPrefix + "/..%2Fwebconsole.go",
		MountPrefix + "/%2e%2e/webconsole.go",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 (traversal blocked)", p, rec.Code)
		}
	}
}

// backendRecorder captures the requests that a fake REST backend receives so
// tests can assert on the rewritten path and injected headers.
type backendRecorder struct {
	mu      sync.Mutex
	paths   []string
	hosts   []string
	headers []http.Header
}

func newBackend(t *testing.T) (*httptest.Server, *backendRecorder) {
	t.Helper()
	rec := &backendRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.hosts = append(rec.hosts, r.Host)
		rec.headers = append(rec.headers, r.Header.Clone())
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (b *backendRecorder) last() (string, http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.paths) == 0 {
		return "", http.Header{}
	}
	return b.paths[len(b.paths)-1], b.headers[len(b.headers)-1]
}

func (b *backendRecorder) lastHost() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.hosts) == 0 {
		return ""
	}
	return b.hosts[len(b.hosts)-1]
}

func TestAPIProxyRewritesPath(t *testing.T) {
	srv, rec := newBackend(t)
	h := newTestHandler(t, srv.URL)

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/output/list", nil)
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	if recw.Code != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200 (body %q)", recw.Code, recw.Body.String())
	}
	gotPath, _ := rec.last()
	if gotPath != "/output/list" {
		t.Fatalf("backend received path %q, want %q", gotPath, "/output/list")
	}
}

func TestAPIProxyPostsBody(t *testing.T) {
	srv, _ := newBackend(t)
	h := newTestHandler(t, srv.URL)

	req := httptest.NewRequest(http.MethodPost, APIPrefix+"/play/pause", strings.NewReader(`{"x":1}`))
	req.Header.Set("Content-Type", "application/json")
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	if recw.Code != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200", recw.Code)
	}
}

func TestAPIConfiguredAuthTokenInjected(t *testing.T) {
	srv, rec := newBackend(t)
	h := NewHandler(Config{BackendURL: srv.URL, AuthToken: "Bearer secret"})

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/play/information", nil)
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	_, hdrs := rec.last()
	if got := hdrs.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer secret")
	}
}

func TestAPIXConsoleTokenMappedToAuthorization(t *testing.T) {
	srv, rec := newBackend(t)
	h := newTestHandler(t, srv.URL)

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/alarm/list", nil)
	req.Header.Set("X-Console-Token", "Bearer ui-token")
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	_, hdrs := rec.last()
	if got := hdrs.Get("Authorization"); got != "Bearer ui-token" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer ui-token")
	}
}

func TestAPIConfiguredTokenWinsOverConsoleToken(t *testing.T) {
	srv, rec := newBackend(t)
	h := NewHandler(Config{BackendURL: srv.URL, AuthToken: "Bearer server-token"})

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/output/list", nil)
	req.Header.Set("X-Console-Token", "Bearer ui-token")
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	_, hdrs := rec.last()
	if got := hdrs.Get("Authorization"); got != "Bearer server-token" {
		t.Fatalf("Authorization = %q, want configured %q", got, "Bearer server-token")
	}
}

func TestAPIProxyBackendUnreachable(t *testing.T) {
	// A server that is closed before the request dials the backend.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	h := newTestHandler(t, addr)
	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/play/duration", nil)
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	if recw.Code != http.StatusBadGateway {
		t.Fatalf("proxy returned %d, want %d (502)", recw.Code, http.StatusBadGateway)
	}
	if ct := recw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error content-type = %q, want application/json", ct)
	}
	if body := recw.Body.String(); !strings.Contains(body, "message") {
		t.Fatalf("error body should contain a message field: %q", body)
	}
}

func TestDefaultConfigUsesDefaultBackend(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.BackendURL != "http://127.0.0.1:4156" {
		t.Fatalf("DefaultConfig.BackendURL = %q, want default", cfg.BackendURL)
	}
}

func TestEmptyBackendURLFallsBack(t *testing.T) {
	// NewHandler must not panic and must still serve static assets when the
	// backend URL is empty.
	h := NewHandler(Config{})
	req := httptest.NewRequest(http.MethodGet, MountPrefix+"/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
}

func TestInvalidBackendURLFallsBack(t *testing.T) {
	h := NewHandler(Config{BackendURL: "://not-a-url"})
	req := httptest.NewRequest(http.MethodGet, MountPrefix+"/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
}

func TestCustomStaticFS(t *testing.T) {
	mem := fstest.MapFS{
		"hello.txt": &fstest.MapFile{Data: []byte("hi")},
	}
	h := NewHandler(Config{StaticFS: fs.FS(mem)})
	req := httptest.NewRequest(http.MethodGet, MountPrefix+"/hello.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hello.txt = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hi" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "hi")
	}
}

func TestReverseProxyUsesBackendSchemeAndHost(t *testing.T) {
	srv, rec := newBackend(t)
	h := newTestHandler(t, srv.URL)

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/resource/current", nil)
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	if recw.Code != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200", recw.Code)
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	gotPath, _ := rec.last()
	if gotPath != "/resource/current" {
		t.Fatalf("backend received %q, want /resource/current", gotPath)
	}
	if u.Host == "" {
		t.Fatal("backend host is empty")
	}
}

func TestAPIProxySetsBackendHost(t *testing.T) {
	// The outbound request's Host header must be the backend's host, not the
	// client-facing one.
	srv, rec := newBackend(t)
	h := newTestHandler(t, srv.URL)

	req := httptest.NewRequest(http.MethodGet, APIPrefix+"/play/status", nil)
	req.Host = "console.example.com"
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	if recw.Code != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200 (body %q)", recw.Code, recw.Body.String())
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.lastHost(); got != u.Host {
		t.Fatalf("backend received Host %q, want %q", got, u.Host)
	}
}

func TestAPIProxyExactPrefixForwardsToRoot(t *testing.T) {
	// /console/api (no trailing slash) must be proxied to the backend root
	// path rather than being served as a static asset.
	srv, rec := newBackend(t)
	h := newTestHandler(t, srv.URL)

	req := httptest.NewRequest(http.MethodGet, APIPrefix, nil)
	recw := httptest.NewRecorder()
	h.ServeHTTP(recw, req)

	if recw.Code != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200 (body %q)", recw.Code, recw.Body.String())
	}
	gotPath, _ := rec.last()
	if gotPath != "/" {
		t.Fatalf("backend received path %q, want %q", gotPath, "/")
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if gotHost := rec.lastHost(); gotHost != u.Host {
		t.Fatalf("backend received Host %q, want %q", gotHost, u.Host)
	}
}
