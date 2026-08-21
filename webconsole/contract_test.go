package webconsole

// Contract check between the console frontend and the backend REST API.
// static/app.js talks to the backend exclusively through the /console/api/...
// prefix, which the proxy rewrites to the backend root, so every
// slash-prefixed fragment inside its string literals must be served by a
// real backend route. This pins that invariant so a frontend/backend
// mismatch (calling an endpoint that does not exist) fails the build
// instead of surfacing at runtime.

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// consolePathRe extracts slash-prefixed fragments from the quoted string
// literals ('...' and "...") of app.js. The character set covers path
// segments plus the query punctuation used by /metrics fragments
// (e.g. "/metrics/failure-rate?mediaId="), so query-bearing fragments are
// still validated by their root prefix instead of being missed.
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
	"/stream",
	// grpc-gateway roots
	"/play", "/resource", "/output", "/plugin", "/websocket",
	"/v1/operations", "/console",
}

// consoleNonAPIFragments are extracted fragments that are not API calls.
// Each is an explicit exception so the root-prefix assertion stays strict
// for everything else: they are either HTML placeholders or path tails
// that app.js concatenates onto a whitelisted root (the id is spliced in
// between, e.g. '/webhook/' + id + '/deliveries').
var consoleNonAPIFragments = map[string]string{
	"/data/media/audio.mp3": "HTML input placeholder (media edit dialog) for an external audio path, not an API call",
	"/data/media/sub.srt":   "HTML input placeholder (media edit dialog) for a subtitle path, not an API call",
	"/update":               "tail of '/media/' + id + '/update' and '/playlist/' + id + '/update' concatenations",
	"/run":                  "tail of '/task/' + id + '/run' concatenation",
	"/enabled":              "tail of '/<root>/' + id + '/enabled' concatenations (output-group, scene-template, webhook, user, node, config-template, industry-template, smart-rule)",
	"/running":              "tail of '/cache-task/' + id + '/running'",
	"/done":                 "tail of '/cache-task/' + id + '/done'",
	"/failed":               "tail of '/cache-task/' + id + '/failed' and '/remote-command/' + id + '/failed'",
	"/sent":                 "tail of '/remote-command/' + id + '/sent'",
	"/success":              "tail of '/remote-command/' + id + '/success'",
	"/restore":              "tail of '/config-snapshot/' + id + '/restore'",
	"/duplicate":            "tail of '/scene-template/' + id + '/duplicate'",
	"/deliveries":           "tail of '/webhook/' + id + '/deliveries'",
	"/password":             "tail of '/user/' + id + '/password'",
	"/heartbeat":            "tail of '/node/' + id + '/heartbeat'",
	"/expand":               "tail of '/config-template/' + id + '/expand'",
	"/deploy":               "tail of '/industry-template/' + id + '/deploy'",
	"/generate":             "tail of '/smart-rule/' + id + '/generate'",
	"/start":                "tail of '/stream/' + id + '/start'",
	"/stop":                 "tail of '/stream/' + id + '/stop'",
	"/replace":              "tail of '/stream/' + id + '/replace'",
	"/generate-and-apply":   "tail of '/smart-rule/' + id + '/generate-and-apply'",
	"/approve":              "tail of '/suggestion/' + id + '/approve'",
	"/reject":               "tail of '/suggestion/' + id + '/reject'",
}

func TestConsoleContract(t *testing.T) {
	src, err := FS.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read static/app.js: %v", err)
	}
	body := string(src)

	// Pin the suggestion payload contract: the recommendation flow used to
	// post the media id under a different key that the backend silently
	// dropped; media_id is the agreed field.
	if !strings.Contains(body, "payload: { media_id") {
		t.Fatalf("static/app.js no longer posts suggestions with 'payload: { media_id ...'")
	}

	seen := make(map[string]bool)
	total := 0
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
			t.Errorf("static/app.js calls %q, which is not served by any backend root; add the root to consoleAPIRoots (only if the backend serves it) or list the fragment in consoleNonAPIFragments (only if it is not an API call)", frag)
		}
	}

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("contract: %d path fragments extracted from static/app.js (%d distinct): %s", total, len(keys), strings.Join(keys, " "))

	// The exception list must mirror the extraction exactly: an entry that
	// no longer matches would hide a removed or renamed call site.
	for frag := range consoleNonAPIFragments {
		if !seen[frag] {
			t.Errorf("consoleNonAPIFragments lists %q but it is no longer extracted from static/app.js; remove the stale entry", frag)
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
