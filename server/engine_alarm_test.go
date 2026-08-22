package server

// Engine-exit integration tests: the production newManagementHandlerWithEngine
// wires the real FFmpegEngine.OnExit callback into the alarm service and the
// webhook dispatcher. These tests drive a real engine whose "ffmpeg" binary is
// a fake shell script (see fakeFFmpegScript), so no ffmpeg is needed and no
// file is written to the repository working directory (the handler pins
// management.json there, so the tests chdir into a temp dir like
// TestNewManagementHandlerWithEngineWiring).

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bytelang/kplayer/engine"
	"github.com/bytelang/kplayer/management"
	"github.com/tidwall/gjson"
)

// fakeFFmpegScript writes a POSIX shell script that impersonates ffmpeg for
// the engine-exit tests: the duration probe (-hide_banner -i <source>) prints
// a Duration line on stderr and exits 1 like real ffmpeg; the playback run
// emits a few -progress lines and then exits per mode ("crash" exits 3 after
// writing an error line, "clean" exits 0). The script waits for its own
// sleep children, so no orphan process is left behind. Windows cannot run
// this: os/exec cannot execute .bat files directly and no POSIX signal can
// be delivered to terminate a fake ffmpeg, so the scenario is skipped there.
func fakeFFmpegScript(t *testing.T, mode string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg needs a POSIX shell: os/exec cannot run .bat files directly and Windows cannot deliver POSIX signals, so the exit paths cannot be exercised")
	}
	var body string
	switch mode {
	case "crash":
		body = `#!/bin/sh
if [ "$1" = "-hide_banner" ]; then
  echo "  Duration: 00:01:00.00, start: 0.000000, bitrate: 1024 kb/s" >&2
  exit 1
fi
echo "frame=1"
echo "fps=25.0"
echo "bitrate=512.0kbits/s"
echo "out_time_us=40000"
echo "progress=continue"
sleep 0.05
echo "Error: simulated ffmpeg crash" >&2
exit 3
`
	case "clean":
		body = `#!/bin/sh
if [ "$1" = "-hide_banner" ]; then
  echo "  Duration: 00:01:00.00, start: 0.000000, bitrate: 1024 kb/s" >&2
  exit 1
fi
echo "frame=1"
echo "fps=25.0"
echo "bitrate=512.0kbits/s"
echo "out_time_us=40000"
echo "progress=continue"
sleep 0.2
echo "frame=2"
echo "fps=25.0"
echo "bitrate=512.0kbits/s"
echo "out_time_us=80000"
echo "progress=continue"
exit 0
`
	default:
		t.Fatalf("unknown fake ffmpeg mode %q", mode)
	}
	path := filepath.Join(t.TempDir(), "fake-ffmpeg.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg script: %v", err)
	}
	return path
}

// newEngineExitHandler builds the production handler wired to a real
// FFmpegEngine whose "ffmpeg" is the fake script, plus a playable source
// file. It returns the handler, the engine, the source path and a channel
// that receives once the engine's OnExit callback has run. The callback is
// wrapped with an observer so the tests can wait for it deterministically;
// the production closure still runs unchanged.
func newEngineExitHandler(t *testing.T, mode string) (*managementHandler, *engine.FFmpegEngine, string, chan struct{}) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	ffmpeg := fakeFFmpegScript(t, mode)
	source := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(source, []byte("fake video data"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	fe := engine.NewFFmpegEngine(engine.Config{
		FFmpegPath: ffmpeg,
		Outputs:    []engine.OutputConfig{{URL: "rtmp://127.0.0.1:1935/live/test"}},
	})
	h, err := newManagementHandlerWithEngine(&fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fe, NewEffectManager(effectFile))
	if err != nil {
		t.Fatalf("newManagementHandlerWithEngine: %v", err)
	}

	orig := fe.OnExit
	onExitCalled := make(chan struct{}, 1)
	fe.OnExit = func(code int, exitErr error) {
		if orig != nil {
			orig(code, exitErr)
		}
		onExitCalled <- struct{}{}
	}
	return h, fe, source, onExitCalled
}

// waitEngineExit waits until the engine has exited and the OnExit callback
// has run, failing the test after a deadline.
func waitEngineExit(t *testing.T, fe *engine.FFmpegEngine, onExitCalled chan struct{}) {
	t.Helper()
	select {
	case <-onExitCalled:
	case <-time.After(15 * time.Second):
		t.Fatalf("engine OnExit callback not invoked: %+v", fe.Status())
	}
}

// streamAlarmTitles returns the titles of all alarms in the store.
func streamAlarmTitles(h *managementHandler) []string {
	alarms := h.alarms.List()
	titles := make([]string, 0, len(alarms))
	for _, a := range alarms {
		titles = append(titles, a.Title)
	}
	return titles
}

// TestEngineExitRaisesStreamAlarm proves the production OnExit wiring: an
// abnormal ffmpeg exit raises a "Stream engine exited" warning alarm, visible
// through GET /alarm, and the engine status carries the exit code.
func TestEngineExitRaisesStreamAlarm(t *testing.T) {
	h, fe, source, onExitCalled := newEngineExitHandler(t, "crash")

	// Play the source through the player API: the adapter starts the real
	// engine, whose fake ffmpeg crashes with exit code 3.
	code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{
		"path": source, "name": "crash source",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /media: status %d body %s", code, body)
	}
	mediaID := gjson.Get(body, "id").String()
	code, body = perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play: status %d body %s", code, body)
	}

	waitEngineExit(t, fe, onExitCalled)
	if st := fe.Status(); st.Running || st.ExitCode != 3 {
		t.Fatalf("engine status after crash = %+v, want stopped with exit code 3", st)
	}

	// The alarm must be visible through the same endpoint the console uses.
	code, body = perform(t, h, http.MethodGet, "/alarm", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /alarm: status %d body %s", code, body)
	}
	var found *gjson.Result
	gjson.Get(body, "alarms").ForEach(func(_, v gjson.Result) bool {
		if v.Get("title").String() == "Stream engine exited" {
			found = &v
			return false
		}
		return true
	})
	if found == nil {
		t.Fatalf("no \"Stream engine exited\" alarm in %s (store: %v)", body, streamAlarmTitles(h))
	}
	if lvl := found.Get("level").String(); lvl != string(management.AlarmLevelWarning) {
		t.Fatalf("alarm level = %q, want %q", lvl, management.AlarmLevelWarning)
	}
	if msg := found.Get("message").String(); !strings.Contains(msg, "exit code 3") {
		t.Fatalf("alarm message = %q, want it to carry the exit code", msg)
	}
}

// TestEngineCleanExitRaisesNoAlarm proves the exit-0 branch of the wiring: a
// normal engine stop (the stream ending) raises no alarm and dispatches
// nothing.
func TestEngineCleanExitRaisesNoAlarm(t *testing.T) {
	h, fe, source, onExitCalled := newEngineExitHandler(t, "clean")

	code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{
		"path": source, "name": "clean source",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /media: status %d body %s", code, body)
	}
	mediaID := gjson.Get(body, "id").String()
	code, body = perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play: status %d body %s", code, body)
	}

	// Wait for the OnExit callback (which runs the production closure), then
	// give any stray delivery a moment before asserting absence.
	waitEngineExit(t, fe, onExitCalled)
	if st := fe.Status(); st.Running || st.ExitCode != 0 {
		t.Fatalf("engine status after clean exit = %+v, want stopped with exit code 0", st)
	}
	time.Sleep(100 * time.Millisecond)
	for _, title := range streamAlarmTitles(h) {
		if title == "Stream engine exited" {
			t.Fatalf("clean engine exit raised a \"Stream engine exited\" alarm")
		}
	}
}

// TestStatusAggregatesEngine proves GET /status merges the engine status into
// the aggregated snapshot when an engine is injected, and omits the key
// without one (the legacy contract stays unchanged).
func TestStatusAggregatesEngine(t *testing.T) {
	fake := &fakeEngine{status: engine.Status{
		Running: true, Pid: 4242, SourcePath: "/videos/a.mp4",
		OutputURLs:  []string{"rtmp://127.0.0.1:1935/live/test"},
		BitrateKbps: 2500, FPS: 25, Frame: 100, Progress: 42.5,
	}}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	code, body := perform(t, h, http.MethodGet, "/status", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /status with engine: status %d body %s", code, body)
	}
	if !gjson.Get(body, "scheduler").IsObject() {
		t.Fatalf("GET /status: missing scheduler key in %s", body)
	}
	if !gjson.Get(body, "engine.running").Bool() {
		t.Fatalf("GET /status: engine.running = false in %s", body)
	}
	if got := gjson.Get(body, "engine.pid").Int(); got != 4242 {
		t.Fatalf("GET /status: engine.pid = %d, want 4242", got)
	}
	if got := gjson.Get(body, "engine.sourcePath").String(); got != "/videos/a.mp4" {
		t.Fatalf("GET /status: engine.sourcePath = %q", got)
	}

	// Without an engine the key is omitted.
	h2 := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	code, body = perform(t, h2, http.MethodGet, "/status", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /status without engine: status %d body %s", code, body)
	}
	if gjson.Get(body, "engine").Exists() {
		t.Fatalf("GET /status without engine: unexpected engine key in %s", body)
	}
}