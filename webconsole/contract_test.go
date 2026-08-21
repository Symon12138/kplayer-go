package webconsole

// Contract check between the console frontend and the backend REST API.
// The console JS modules talk to the backend exclusively through the
// /console/api/... prefix, which the proxy rewrites to the backend root,
// so every slash-prefixed fragment inside their string literals must be
// served by a real backend route. This pins that invariant so a
// frontend/backend mismatch (calling an endpoint that does not exist)
// fails the build instead of surfacing at runtime.

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// consolePathRe extracts slash-prefixed fragments from the quoted string
// literals ('...' and "...") of the console JS sources. The character set
// covers path segments plus query punctuation, so query-bearing fragments
// are validated by their root prefix instead of being missed.
var consolePathRe = regexp.MustCompile(`['"](/[A-Za-z0-9_\-/{}.:?=]*)['"]`)

// consoleAPIRoots are the backend path roots the console may call. Keep
// this list in sync with the backend routes: the management REST roots in
// server/management_api.go (managementHandler.ServeHTTP) and the
// grpc-gateway roots in server/http_server.go (/play /resource /output
// /plugin /websocket /v1/operations plus the /console mount).
var consoleAPIRoots = []string{
	// management REST roots
	"/status", "/media", "/playlist", "/task", "/alarm", "/scheduler",
	"/player", "/output-group", "/failover", "/health-policy", "/cache-task",
	"/scene-template", "/webhook", "/audit", "/auth", "/user", "/node",
	"/instance", "/remote-command", "/config-snapshot", "/config-template",
	"/industry-template", "/smart-rule", "/metrics", "/suggestion", "/engine",
	"/stream", "/effects",
	// grpc-gateway roots
	"/play", "/resource", "/output", "/plugin", "/websocket",
	"/v1/operations", "/console",
}

// consoleNonAPIFragments are extracted fragments that are not API calls.
// Each is an explicit exception so the root-prefix assertion stays strict
// for everything else: they are either HTML input placeholders (server-side
// file paths) or path tails that the JS concatenates onto a whitelisted
// root (the id is spliced in between, e.g. '/stream/' + id + '/stop').
var consoleNonAPIFragments = map[string]string{
	"/data/audio.mp3": "HTML input placeholder (media register dialog) for an external audio path, not an API call",
	"/data/sub.srt":   "HTML input placeholder (media/effects dialogs) for a subtitle path, not an API call",
	"/data/logo.png":  "HTML input placeholder (image watermark param) for a logo path, not an API call",
	"/update":         "tail of '/media/' + id + '/update' and '/playlist/' + id + '/update' concatenations",
	"/run":            "tail of '/task/' + id + '/run' concatenation",
	"/start":          "tail of '/stream/' + id + '/start' concatenation",
	"/stop":           "tail of '/stream/' + id + '/stop' concatenation",
	"/replace":        "tail of '/stream/' + id + '/replace' concatenation",
	"/enabled":        "tail of '/user/' + id + '/enabled' concatenation",
	"/password":       "tail of '/user/' + id + '/password' concatenation",
	"/restore":        "tail of '/config-snapshot/' + id + '/restore' concatenation",
	"/deliveries":     "tail of '/webhook/' + id + '/deliveries' concatenation",
}

func TestConsoleContract(t *testing.T) {
	entries, err := fs.ReadDir(FS, "static")
	if err != nil {
		t.Fatalf("read static dir: %v", err)
	}
	var sources []string
	var walk func(prefix string, entries []fs.DirEntry)
	walk = func(prefix string, entries []fs.DirEntry) {
		for _, e := range entries {
			name := prefix + e.Name()
			if e.IsDir() {
				sub, err := fs.ReadDir(FS, "static/"+name)
				if err != nil {
					t.Fatalf("read static/%s: %v", name, err)
				}
				walk(name+"/", sub)
				continue
			}
			if strings.HasSuffix(name, ".js") {
				src, err := fs.ReadFile(FS, "static/"+name)
				if err != nil {
					t.Fatalf("read static/%s: %v", name, err)
				}
				sources = append(sources, name+"\n"+string(src))
			}
		}
	}
	walk("", entries)

	if len(sources) == 0 {
		t.Fatal("no console JS sources found under static/")
	}

	seen := make(map[string]bool)
	total := 0
	for _, body := range sources {
		for _, m := range consolePathRe.FindAllStringSubmatch(body, -1) {
			total++
			frag := m[1]
			if frag == "/" || frag == "/console/api" {
				// "/" is the bare path separator used in string concatenations
				// and "/console/api" is the proxy prefix itself; neither is a
				// backend route.
				continue
			}
			seen[frag] = true
			if _, ok := consoleNonAPIFragments[frag]; ok {
				continue
			}
			if !underConsoleAPIRoot(frag) {
				t.Errorf("console JS calls %q, which is not served by any backend root; add the root to consoleAPIRoots (only if the backend serves it) or list the fragment in consoleNonAPIFragments (only if it is not an API call)", frag)
			}
		}
	}

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("contract: %d path fragments extracted (%d distinct): %s", total, len(keys), strings.Join(keys, " "))

	// The exception list must mirror the extraction exactly: an entry that
	// no longer matches would hide a removed or renamed call site.
	for frag := range consoleNonAPIFragments {
		if !seen[frag] {
			t.Errorf("consoleNonAPIFragments lists %q but it is no longer extracted from the console JS; remove the stale entry", frag)
		}
	}
}

// underConsoleAPIRoot reports whether frag is a backend route path: either
// a root itself or a sub-path of one. The match mirrors the backend
// dispatch (exact root or root + "/" prefix).
func underConsoleAPIRoot(frag string) bool {
	for _, root := range consoleAPIRoots {
		if frag == root || strings.HasPrefix(frag, root+"/") {
			return true
		}
	}
	return false
}
