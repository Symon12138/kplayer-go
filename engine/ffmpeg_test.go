package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration validation
// ---------------------------------------------------------------------------

func TestConfigValidate(t *testing.T) {
	// An empty configuration has no outputs: invalid.
	if _, err := Validate(Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate(Config{}) error = %v, want ErrInvalid", err)
	}

	invalid := []Config{
		// Output with an empty URL.
		{Outputs: []OutputConfig{{URL: "  "}}},
		// Negative dimensions.
		{Outputs: []OutputConfig{{URL: "rtmp://x/live", Width: -1, Height: 1080}}},
		// Width without height.
		{Outputs: []OutputConfig{{URL: "rtmp://x/live", Width: 1920}}},
		// Negative bitrate.
		{Outputs: []OutputConfig{{URL: "rtmp://x/live", BitrateKbps: -100}}},
		// Negative fps.
		{Outputs: []OutputConfig{{URL: "rtmp://x/live", FPS: -25}}},
	}
	for i, cfg := range invalid {
		if _, err := Validate(cfg); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid case %d: error = %v, want ErrInvalid", i, err)
		}
	}

	// A normal configuration normalizes defaults and passes.
	cfg, err := Validate(Config{Outputs: []OutputConfig{{URL: "rtmp://127.0.0.1:1935/live/test"}}})
	if err != nil {
		t.Fatalf("Validate(normal) error = %v", err)
	}
	if cfg.FFmpegPath == "" {
		t.Fatalf("FFmpegPath empty: the default should auto-detect the binary")
	}
	if cfg.ProbeInterval != 2*time.Second {
		t.Fatalf("ProbeInterval = %v, want default 2s", cfg.ProbeInterval)
	}
	if len(cfg.Outputs) != 1 || cfg.Outputs[0].Codec != "libx264" {
		t.Fatalf("output codec = %+v, want default libx264", cfg.Outputs)
	}
	// Provided values survive normalization.
	cfg, err = Validate(Config{
		FFmpegPath: "/usr/bin/ffmpeg",
		Outputs:    []OutputConfig{{URL: "rtmp://x/live", Codec: "h264_nvenc", Filters: "drawtext=text='hi'"}},
	})
	if err != nil {
		t.Fatalf("Validate(custom) error = %v", err)
	}
	if cfg.FFmpegPath != "/usr/bin/ffmpeg" || cfg.Outputs[0].Codec != "h264_nvenc" || cfg.Outputs[0].Filters != "drawtext=text='hi'" {
		t.Fatalf("custom values lost: %+v", cfg)
	}
}

func TestBuildArgs(t *testing.T) {
	cfg, err := Validate(Config{Outputs: []OutputConfig{
		{URL: "rtmp://127.0.0.1:1935/live/a", Width: 1920, Height: 1080, BitrateKbps: 2500, FPS: 25, HWAccel: "vaapi"},
		{URL: "rtmp://127.0.0.1:1935/live/b", Filters: "drawtext=text='hi'"},
	}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	args, err := buildArgs(cfg, "/videos/a.mp4", 0)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	want := []string{
		"-y", "-re", "-hwaccel", "vaapi",
		"-i", "/videos/a.mp4", "-progress", "pipe:1", "-nostats", "-loglevel", "error",
		"-map", "0", "-c:v", "libx264", "-b:v", "2500k", "-r", "25", "-s", "1920x1080", "-f", "flv", "rtmp://127.0.0.1:1935/live/a",
		"-map", "0", "-vf", "drawtext=text='hi'", "-c:v", "libx264", "-f", "flv", "rtmp://127.0.0.1:1935/live/b",
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("buildArgs =\n  %s\nwant\n  %s", strings.Join(args, " "), strings.Join(want, " "))
	}

	// Zero-valued options are omitted; the last argument is the last URL.
	cfg, _ = Validate(Config{Outputs: []OutputConfig{{URL: "rtmp://x/live"}}})
	args, _ = buildArgs(cfg, "src.mp4", 0)
	if len(args) != 16 || args[len(args)-1] != "rtmp://x/live" {
		t.Fatalf("single output args = %v", args)
	}

	// Seek offset and audio options are rendered when set.
	cfg, _ = Validate(Config{Outputs: []OutputConfig{{
		URL:             "rtmp://x/live",
		AudioChannels:   2,
		AudioSampleRate: 44100,
	}}})
	args, _ = buildArgs(cfg, "src.mp4", 37)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-ss", "37", "-ac", "2", "-ar", "44100"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("buildArgs missing %q in %s", want, joined)
		}
	}
}

// ---------------------------------------------------------------------------
// Progress parsing
// ---------------------------------------------------------------------------

func TestParseProgress(t *testing.T) {
	st := Status{}
	text := `frame=123
fps=25.0
bitrate=512.0kbits/s
out_time_us=1234567
progress=continue
frame=125
fps=25.2
out_time=00:00:01.235000
progress=continue
`
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		applyProgressLine(&st, 10*time.Second, line)
	}

	if st.Frame != 125 {
		t.Fatalf("Frame = %d, want 125", st.Frame)
	}
	if st.FPS != 25.2 {
		t.Fatalf("FPS = %v, want 25.2", st.FPS)
	}
	if st.BitrateKbps != 512 {
		t.Fatalf("BitrateKbps = %d, want 512", st.BitrateKbps)
	}
	// Progress = out_time / duration: 1.235s of 10s -> 12.35%.
	if st.Progress < 12.34 || st.Progress > 12.36 {
		t.Fatalf("Progress = %v, want ~12.35", st.Progress)
	}

	// Later lines win: a huge out_time clamps progress at 100.
	applyProgressLine(&st, 10*time.Second, "frame=1000")
	applyProgressLine(&st, 10*time.Second, "out_time_us=987654321")
	if st.Frame != 1000 {
		t.Fatalf("Frame = %d, want 1000", st.Frame)
	}
	if st.Progress != 100 {
		t.Fatalf("Progress = %v, want clamped 100", st.Progress)
	}
	// Progress stays 0 without a probed duration.
	empty := Status{}
	applyProgressLine(&empty, 0, "out_time_us=5000000")
	if empty.Progress != 0 {
		t.Fatalf("Progress without duration = %v, want 0", empty.Progress)
	}

	// Malformed lines and unknown keys are ignored ("bitrate=N/A" is a
	// legitimate ffmpeg value handled by parseBitrate, not malformed).
	before := Status{Frame: 7, FPS: 30, BitrateKbps: 100, Progress: 50}
	after := before
	for _, line := range []string{"", "garbage", "frame=abc", "fps=", "out_time_us=x", "unknown=1"} {
		applyProgressLine(&after, 10*time.Second, line)
	}
	if after.Frame != before.Frame || after.FPS != before.FPS || after.BitrateKbps != before.BitrateKbps || after.Progress != before.Progress {
		t.Fatalf("malformed lines mutated status: %+v", after)
	}
}

func TestParseBitrate(t *testing.T) {
	cases := map[string]int{
		"512.0kbits/s":   512,
		" 2500.6kbits/s": 2500,
		"98765.4bits/s":  98,
		"N/A":            0,
		"0.0kbits/s":     0,
		"garbage":        0,
	}
	for in, want := range cases {
		if got := parseBitrate(in); got != want {
			t.Fatalf("parseBitrate(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseDurationLine(t *testing.T) {
	d, err := parseDurationLine("  Duration: 00:01:30.50, start: 0.000000, bitrate: 1024 kb/s\n")
	if err != nil {
		t.Fatalf("parseDurationLine: %v", err)
	}
	if d != 90*time.Second+500*time.Millisecond {
		t.Fatalf("duration = %v, want 1m30.5s", d)
	}
	if _, err := parseDurationLine("no duration here"); err == nil {
		t.Fatalf("parseDurationLine without duration: want error")
	}
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()

	// A missing file yields the default configuration without error.
	cfg, err := loadConfig(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("loadConfig(missing) error = %v", err)
	}
	if cfg.FFmpegPath == "" || len(cfg.Outputs) != 0 {
		t.Fatalf("default config = %+v", cfg)
	}

	// A written file round-trips through persistConfig/loadConfig.
	path := filepath.Join(dir, "engine.json")
	want, err := Validate(Config{Outputs: []OutputConfig{
		{URL: "rtmp://127.0.0.1:1935/live/test", Width: 1280, Height: 720, BitrateKbps: 1500, FPS: 30},
	}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := persistConfig(path, want); err != nil {
		t.Fatalf("persistConfig: %v", err)
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig(written) error = %v", err)
	}
	if got.FFmpegPath != want.FFmpegPath || len(got.Outputs) != 1 || got.Outputs[0].URL != want.Outputs[0].URL || got.Outputs[0].BitrateKbps != 1500 {
		t.Fatalf("round-trip config mismatch: %+v vs %+v", got, want)
	}

	// A corrupt file is an error.
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := loadConfig(corrupt); err == nil {
		t.Fatalf("loadConfig(corrupt): want error")
	}
}

func TestUpdateConfigPersistsAndRestarts(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("GO_FFMPEG_HELPER_MODE", "normal")
	eng, source := newTestEngine(t, Config{})

	if err := eng.Start(context.Background(), source); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunning(t, eng)
	oldPid := eng.Status().Pid

	cfg, err := Validate(Config{Outputs: []OutputConfig{{URL: "rtmp://127.0.0.1:1935/live/updated", BitrateKbps: 3000}}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := eng.UpdateConfig(cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	// Save must NOT touch the running stream: the process keeps its old
	// outputs until Apply is called (mistake-safe two-step flow).
	st := eng.Status()
	if !st.Running {
		t.Fatalf("engine stopped after UpdateConfig")
	}
	if st.Pid != oldPid {
		t.Fatalf("pid changed after UpdateConfig (save must not restart)")
	}
	if len(st.OutputURLs) != 1 || st.OutputURLs[0] == "rtmp://127.0.0.1:1935/live/updated" {
		t.Fatalf("OutputURLs changed before Apply = %v", st.OutputURLs)
	}
	// The configuration was persisted atomically.
	raw, err := os.ReadFile(eng.configFile)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("parse persisted config: %v", err)
	}
	if len(persisted.Outputs) != 1 || persisted.Outputs[0].BitrateKbps != 3000 {
		t.Fatalf("persisted config = %+v", persisted)
	}
	if got := eng.Config(); got.Outputs[0].BitrateKbps != 3000 {
		t.Fatalf("Config() = %+v", got)
	}

	// Apply restarts the running stream with the saved configuration.
	if err := eng.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	waitRunning(t, eng)
	st = eng.Status()
	if !st.Running {
		t.Fatalf("engine not running after Apply")
	}
	if st.Pid == oldPid {
		t.Fatalf("pid unchanged after Apply restart")
	}
	if len(st.OutputURLs) != 1 || st.OutputURLs[0] != "rtmp://127.0.0.1:1935/live/updated" {
		t.Fatalf("OutputURLs after Apply = %v", st.OutputURLs)
	}

	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestApplyNotRunningIsNoOp(t *testing.T) {
	eng := NewFFmpegEngine(Config{})
	eng.configFile = filepath.Join(t.TempDir(), "engine.json")

	cfg := Config{Outputs: []OutputConfig{{URL: "rtmp://x/live"}}}
	if err := eng.UpdateConfig(cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	// Nothing is playing: Apply is a no-op, the config applies on next Start.
	if err := eng.Apply(context.Background()); err != nil {
		t.Fatalf("Apply (not running): %v", err)
	}
	if st := eng.Status(); st.Running {
		t.Fatalf("engine running after Apply without Start")
	}
}

func TestUpdateConfigNotRunning(t *testing.T) {
	eng := NewFFmpegEngine(Config{})
	eng.configFile = filepath.Join(t.TempDir(), "engine.json")

	// No process is running: the update only replaces the configuration.
	cfg := Config{Outputs: []OutputConfig{{URL: "rtmp://x/live"}}}
	if err := eng.UpdateConfig(cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if st := eng.Status(); st.Running {
		t.Fatalf("engine running after UpdateConfig without Start")
	}
	if got := eng.Config(); len(got.Outputs) != 1 || got.Outputs[0].URL != "rtmp://x/live" {
		t.Fatalf("Config() = %+v", got)
	}
	if _, err := os.Stat(eng.configFile); err != nil {
		t.Fatalf("config file not persisted: %v", err)
	}

	// An invalid update is rejected and leaves the config untouched.
	if err := eng.UpdateConfig(Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("UpdateConfig(empty) error = %v, want ErrInvalid", err)
	}
	if got := eng.Config(); len(got.Outputs) != 1 {
		t.Fatalf("config changed by failed update: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle (helper-process pattern)
// ---------------------------------------------------------------------------

// TestHelperProcess is not a real test: with GO_WANT_HELPER_PROCESS=1 it
// impersonates the ffmpeg binary (the standard helper-process pattern), so
// the lifecycle tests need no real ffmpeg and behave identically on Windows
// and Linux. The engine spawns the test binary itself with
// "-test.run=TestHelperProcess --" prefixed to the ffmpeg arguments.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args[1:]
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	// The engine probes the source with "-hide_banner -i <source>" before
	// the main run: mirror ffmpeg's probe output and non-zero exit.
	if len(args) >= 2 && args[0] == "-hide_banner" && args[1] == "-i" {
		fmt.Fprintln(os.Stderr, "  Duration: 00:01:00.00, start: 0.000000, bitrate: 1024 kb/s")
		os.Exit(1)
	}
	switch os.Getenv("GO_FFMPEG_HELPER_MODE") {
	case "crash":
		// Emit a few progress lines, then die with a non-zero code.
		for i := 0; i < 3; i++ {
			fmt.Fprintf(os.Stdout, "frame=%d\nfps=25.0\nbitrate=512.0kbits/s\nout_time_us=%d\nprogress=continue\n", i, i*40000)
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "Error: simulated ffmpeg crash")
		os.Exit(3)
	default:
		// Normal run: emit progress lines until signalled. SIGTERM exits 0
		// (Unix); Windows never delivers SIGTERM, so Stop kills directly
		// there.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		go func() {
			<-sig
			os.Exit(0)
		}()
		frame := int64(0)
		for {
			fmt.Fprintf(os.Stdout, "frame=%d\nfps=25.0\nbitrate=512.0kbits/s\nout_time_us=%d\nprogress=continue\n", frame, frame*40000)
			frame++
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// newTestEngine builds an FFmpegEngine whose "ffmpeg" is the test binary
// itself, configured with one output and a real source file in a temp dir.
func newTestEngine(t *testing.T, cfg Config) (*FFmpegEngine, string) {
	t.Helper()
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = os.Args[0]
	}
	cfg.Outputs = []OutputConfig{{URL: "rtmp://127.0.0.1:1935/live/test", BitrateKbps: 2500, FPS: 25, Width: 1920, Height: 1080}}
	source := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(source, []byte("fake video data"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	eng := NewFFmpegEngine(cfg)
	eng.configFile = filepath.Join(t.TempDir(), "engine.json")
	eng.cmdBuilder = func(ctx context.Context, ffmpeg string, args []string) *exec.Cmd {
		helperArgs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		return exec.CommandContext(ctx, os.Args[0], helperArgs...)
	}
	return eng, source
}

// waitRunning polls until the engine reports a running process with
// progress, failing the test after a deadline.
func waitRunning(t *testing.T, eng *FFmpegEngine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := eng.Status()
		if st.Running && st.Frame > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine did not reach running state: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEngineLifecycleGracefulStop(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("GO_FFMPEG_HELPER_MODE", "normal")
	eng, source := newTestEngine(t, Config{})

	onExit := make(chan int, 1)
	eng.OnExit = func(code int, err error) { onExit <- code }

	if err := eng.Start(context.Background(), source); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunning(t, eng)

	st := eng.Status()
	if st.Pid <= 0 {
		t.Fatalf("Pid = %d, want > 0", st.Pid)
	}
	if st.SourcePath != source {
		t.Fatalf("SourcePath = %q, want %q", st.SourcePath, source)
	}
	if len(st.OutputURLs) != 1 || st.OutputURLs[0] != "rtmp://127.0.0.1:1935/live/test" {
		t.Fatalf("OutputURLs = %v", st.OutputURLs)
	}
	if st.FPS != 25.0 {
		t.Fatalf("FPS = %v, want 25.0", st.FPS)
	}
	if st.BitrateKbps != 512 {
		t.Fatalf("BitrateKbps = %d, want 512", st.BitrateKbps)
	}
	// The helper probe reports a 60s duration, so 1.6s of out_time is a
	// few percent in.
	if st.Progress <= 0 {
		t.Fatalf("Progress = %v, want > 0", st.Progress)
	}
	if st.Uptime == "" {
		t.Fatalf("Uptime empty while running")
	}

	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st = eng.Status()
	if st.Running {
		t.Fatalf("engine still running after Stop")
	}
	if st.StoppedAt.IsZero() {
		t.Fatalf("StoppedAt zero after Stop")
	}
	if st.ExitCode == 0 && runtime.GOOS == "windows" {
		// On Windows Stop kills the process (no SIGTERM delivery), so the
		// exit code cannot be the graceful 0.
		t.Fatalf("unexpected graceful exit code 0 on Windows")
	}
	if st.ExitCode != 0 && runtime.GOOS != "windows" {
		t.Fatalf("graceful stop exit code = %d, want 0", st.ExitCode)
	}

	// The OnExit callback fired with the exit.
	select {
	case code := <-onExit:
		if runtime.GOOS != "windows" && code != 0 {
			t.Fatalf("OnExit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("OnExit callback not invoked")
	}

	// A second Stop is a no-op, not an error.
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestEngineLifecycleCrashInvokesOnExit(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("GO_FFMPEG_HELPER_MODE", "crash")
	eng, source := newTestEngine(t, Config{})

	onExit := make(chan int, 1)
	eng.OnExit = func(code int, err error) { onExit <- code }

	if err := eng.Start(context.Background(), source); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case code := <-onExit:
		if code != 3 {
			t.Fatalf("OnExit code = %d, want 3", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("OnExit callback not invoked after crash")
	}

	st := eng.Status()
	if st.Running {
		t.Fatalf("engine still running after crash")
	}
	if st.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", st.ExitCode)
	}
	if !strings.Contains(st.LastError, "simulated ffmpeg crash") {
		t.Fatalf("LastError = %q, want the stderr tail", st.LastError)
	}
	if st.StoppedAt.IsZero() {
		t.Fatalf("StoppedAt zero after crash")
	}
}

func TestEngineStartRejectsMissingSource(t *testing.T) {
	eng := NewFFmpegEngine(Config{Outputs: []OutputConfig{{URL: "rtmp://x/live"}}})
	err := eng.Start(context.Background(), filepath.Join(t.TempDir(), "missing.mp4"))
	if err == nil {
		t.Fatalf("Start with missing source succeeded, want error")
	}
	if st := eng.Status(); st.Running {
		t.Fatalf("engine running after failed Start")
	}
}

func TestEngineStartRejectsNoOutputs(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(source, []byte("data"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	eng := NewFFmpegEngine(Config{})
	if err := eng.Start(context.Background(), source); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Start without outputs error = %v, want ErrInvalid", err)
	}
}

func TestEnginePauseContinueSkip(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("GO_FFMPEG_HELPER_MODE", "normal")
	eng, source := newTestEngine(t, Config{})

	// Pause with nothing running is a no-op.
	if err := eng.Pause(context.Background()); err != nil {
		t.Fatalf("Pause (idle): %v", err)
	}
	if st := eng.Status(); st.Paused {
		t.Fatalf("Paused set by idle Pause")
	}
	// Continue with nothing paused is a no-op.
	if err := eng.Continue(context.Background()); err != nil {
		t.Fatalf("Continue (idle): %v", err)
	}
	// Skip with no known source is a no-op.
	if err := eng.Skip(context.Background()); err != nil {
		t.Fatalf("Skip (idle): %v", err)
	}

	if err := eng.Start(context.Background(), source); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunning(t, eng)

	// Pause stops the process and remembers the position.
	pos := eng.Status().Progress
	if err := eng.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	st := eng.Status()
	if st.Running {
		t.Fatalf("engine still running after Pause")
	}
	if !st.Paused {
		t.Fatalf("Paused not set after Pause")
	}
	if pos <= 0 {
		t.Fatalf("Progress = %v before Pause, want > 0 (position to resume from)", pos)
	}

	// Continue restarts a process from the remembered position.
	if err := eng.Continue(context.Background()); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	waitRunning(t, eng)
	st = eng.Status()
	if st.Paused {
		t.Fatalf("Paused still set after Continue")
	}
	if !st.Running {
		t.Fatalf("engine not running after Continue")
	}
	if st.SourcePath != source {
		t.Fatalf("SourcePath after Continue = %q, want %q", st.SourcePath, source)
	}

	// Skip restarts the same source from the beginning.
	if err := eng.Skip(context.Background()); err != nil {
		t.Fatalf("Skip: %v", err)
	}
	waitRunning(t, eng)
	if st := eng.Status(); !st.Running {
		t.Fatalf("engine not running after Skip")
	}

	// Pause then Stop: the remembered position must not auto-reconnect.
	eng.cfg = eng.Config()
	eng.mu.Lock()
	eng.cfg.ReconnectInterval = 1
	eng.mu.Unlock()
	if err := eng.Pause(context.Background()); err != nil {
		t.Fatalf("Pause before Stop: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	if st := eng.Status(); st.Running {
		t.Fatalf("engine auto-reconnected after Pause despite ReconnectInterval")
	}
}
