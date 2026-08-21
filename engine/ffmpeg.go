// Package engine implements a real playback engine that drives the ffmpeg
// binary as a subprocess: decode, encode, push. It is pure standard library
// and depends on nothing else in the project; the server layer depends on
// it (server → engine), never the other way around.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OutputConfig describes one ffmpeg output of the engine: the push URL plus
// the encode parameters applied to that output stream. A zero value for
// Width/Height/BitrateKbps/FPS means the corresponding option is omitted
// from the command line.
type OutputConfig struct {
	URL         string `json:"url"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	BitrateKbps int    `json:"bitrateKbps,omitempty"`
	FPS         int    `json:"fps,omitempty"`
	Codec       string `json:"codec,omitempty"`
	// AudioChannels and AudioSampleRate map to ffmpeg's -ac and -ar for
	// this output (0 omits the option).
	AudioChannels   int `json:"audioChannels,omitempty"`
	AudioSampleRate int `json:"audioSampleRate,omitempty"`
	// HWAccel is passed through verbatim as ffmpeg's -hwaccel input option
	// (empty disables hardware acceleration). ffmpeg only accepts -hwaccel
	// on the input side, so the first non-empty value among the outputs is
	// used; the value itself is never interpreted.
	HWAccel string `json:"hwAccel,omitempty"`
	// Filters is a verbatim filter chain (for example
	// "drawtext=text='hello':x=10") applied with -vf; it is passed through
	// without syntax validation.
	Filters string `json:"filters,omitempty"`
	// AudioFilters is a verbatim audio filter chain applied with -af (for
	// example "volume=1.5,adelay=300|300" to boost the volume or shift the
	// audio relative to the picture for A/V sync). It is passed through
	// without syntax validation.
	AudioFilters string `json:"audioFilters,omitempty"`
}

// Config is the engine configuration. Defaults are applied by Validate
// (config.go): FFmpegPath "ffmpeg", ProbeInterval 2s, Codec "libx264".
type Config struct {
	FFmpegPath string         `json:"ffmpegPath,omitempty"`
	Outputs    []OutputConfig `json:"outputs"`
	// ProbeInterval is the period of the engine's status probing. ffmpeg
	// -progress lines are folded into Status as they arrive; the interval
	// is reserved for periodic re-probing (batch 2) and kept in the
	// configuration contract so the value round-trips through the REST API.
	ProbeInterval time.Duration `json:"probeInterval"`
	// ReconnectInterval is the delay in seconds before the engine
	// automatically restarts the stream after an abnormal ffmpeg exit
	// (0 disables auto-reconnect). This mirrors the output
	// reconnect_internal behaviour of the original KPlayer: when a push
	// connection drops, the stream is retried instead of ending.
	ReconnectInterval int `json:"reconnectInterval,omitempty"`
}

// Status is a point-in-time snapshot of the engine.
type Status struct {
	Running     bool      `json:"running"`
	Pid         int       `json:"pid"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	StoppedAt   time.Time `json:"stoppedAt,omitempty"`
	ExitCode    int       `json:"exitCode"`
	LastError   string    `json:"lastError,omitempty"`
	SourcePath  string    `json:"sourcePath,omitempty"`
	OutputURLs  []string  `json:"outputURLs,omitempty"`
	Uptime      string    `json:"uptime,omitempty"`
	BitrateKbps int       `json:"bitrateKbps"`
	FPS         float64   `json:"fps"`
	Frame       int64     `json:"frame"`
	// Progress is the playback progress in percent (0-100), derived from
	// ffmpeg -progress out_time against the probed source duration.
	Progress float64 `json:"progress"`
	// Paused reports that the stream was suspended by Pause and can be
	// resumed at the remembered position with Continue.
	Paused bool `json:"paused"`
}

// PlayItem is one playback unit of the engine queue: the source file plus
// optional auxiliary inputs merged into the push (an external audio track
// and a subtitle file burned into the picture). An empty AudioPath or
// SubtitlePath keeps the source's own streams.
type PlayItem struct {
	Path          string
	AudioPath     string
	SubtitlePath  string
}

// Engine is the playback engine contract the server layer depends on. A nil
// engine keeps the legacy stub playback path; a non-nil engine owns the
// ffmpeg subprocess lifecycle.
type Engine interface {
	Start(ctx context.Context, source string) error
	// StartAt starts playback from the given offset in seconds (0 = from
	// the beginning). It is the primitive behind Start.
	StartAt(ctx context.Context, source string, seekSeconds float64) error
	// StartQueue plays the given items one after another: when an item
	// ends normally (EOF) the next one starts automatically; after the
	// last item the queue either loops back to the first (loop=true) or
	// the stream stops. Auxiliary inputs of each item (external audio /
	// burned-in subtitles) are merged per item.
	StartQueue(ctx context.Context, items []PlayItem, seekSeconds float64, loop bool) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context, source string) error
	// Pause suspends a running stream (ffmpeg stops pushing; the position
	// is remembered). Continue resumes from that position; Skip restarts
	// the current source from the beginning. All three are no-ops when the
	// engine is not playing.
	Pause(ctx context.Context) error
	Continue(ctx context.Context) error
	Skip(ctx context.Context) error
	Status() Status
	UpdateConfig(cfg Config) error
	// Apply makes the saved configuration take effect on the running
	// stream: when a stream is playing it is restarted with the new
	// configuration; otherwise the configuration applies on the next Start.
	Apply(ctx context.Context) error
	Config() Config
}

// FFmpegEngine runs ffmpeg as a managed subprocess. All exported methods
// are safe for concurrent use.
type FFmpegEngine struct {
	// lifeMu serializes the lifecycle transitions (Start/Stop/Restart) so
	// two overlapping transitions can never race the exit watcher.
	lifeMu sync.Mutex
	// mu guards cfg, cmd, exited, status, source, duration and stderrTail.
	mu sync.Mutex

	cfg        Config
	configFile string

	// OnExit, when set, is invoked on a dedicated goroutine after the
	// ffmpeg process exits, normally or abnormally, with the exit code and
	// the Wait error (nil on a clean exit). The callback is fire-and-
	// forget: it must not block, and the engine never waits for it.
	OnExit func(exitCode int, err error)

	// stopTimeout bounds the graceful SIGTERM phase of Stop.
	stopTimeout time.Duration

	cmd        *exec.Cmd
	procCancel context.CancelFunc
	exited     chan struct{} // closed once Wait returned and status was finalized
	status     Status
	source     string
	duration   time.Duration // probed source duration driving Progress
	lastSeek   float64       // last seek offset, reused by auto-reconnect
	elapsedSec float64       // media seconds elapsed since the seek offset (from -progress out_time)

	// queue is the continuous-playback list: when it has more than one
	// entry the engine advances to the next item after a normal EOF and
	// wraps around when loop is set. queueIdx points at the current item;
	// aux holds the current item's auxiliary inputs (guarded by mu).
	queue    []PlayItem
	queueIdx int
	loop     bool
	aux      PlayItem

	// Pause bookkeeping: the source and absolute media position a Pause
	// suspends at, so Continue can resume exactly there.
	resumeSource string
	resumeSeek   float64

	// intentionalStop marks stops requested through Stop/Pause/Restart:
	// the exit watcher skips auto-reconnect after an intentional stop so a
	// paused or stopped stream stays paused or stopped.
	intentionalStop bool

	// stderrTail is a bounded ring of the most recent stderr lines,
	// surfaced through Status.LastError when the process exits.
	stderrTail []string
	maxTail    int

	// cmdBuilder constructs the ffmpeg process. It is a test seam: the
	// lifecycle tests replace it with the helper-process pattern so no
	// real ffmpeg binary is needed and the tests run identically on
	// Windows and Linux.
	cmdBuilder func(ctx context.Context, ffmpeg string, args []string) *exec.Cmd
}

// NewFFmpegEngine returns an engine with the normalized configuration.
// Normalization fills the defaults but does not require completeness (an
// engine may be created with no outputs and configured later through
// UpdateConfig).
func NewFFmpegEngine(cfg Config) *FFmpegEngine {
	return &FFmpegEngine{
		cfg:         normalize(cfg),
		configFile:  ConfigFile,
		stopTimeout: defaultStopTimeout,
		maxTail:     20,
		cmdBuilder: func(ctx context.Context, ffmpeg string, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, ffmpeg, args...)
		},
	}
}

// Start launches ffmpeg for source. The source file must exist. If a
// process is already running it is stopped first. The caller's ctx only
// gates the start operation: once the process is spawned its lifetime is
// owned by the engine (Stop / process exit), so an HTTP request context
// expiring never kills a running stream.
// Start starts playback from the beginning of the source.
func (e *FFmpegEngine) Start(ctx context.Context, source string) error {
	return e.StartAt(ctx, source, 0)
}

// StartAt starts playback of one source from the given offset. It resets
// the queue to the single item.
func (e *FFmpegEngine) StartAt(ctx context.Context, source string, seekSeconds float64) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("%w: empty source path", ErrInvalid)
	}
	e.mu.Lock()
	e.queue = []PlayItem{{Path: source}}
	e.queueIdx = 0
	e.loop = false
	e.mu.Unlock()
	return e.startItem(ctx, PlayItem{Path: source}, seekSeconds)
}

// StartQueue plays items one after another (see the Engine interface
// documentation). A single item behaves exactly like StartAt.
func (e *FFmpegEngine) StartQueue(ctx context.Context, items []PlayItem, seekSeconds float64, loop bool) error {
	if len(items) == 0 {
		return fmt.Errorf("%w: empty play queue", ErrInvalid)
	}
	for i := range items {
		items[i].Path = strings.TrimSpace(items[i].Path)
		items[i].AudioPath = strings.TrimSpace(items[i].AudioPath)
		items[i].SubtitlePath = strings.TrimSpace(items[i].SubtitlePath)
		if items[i].Path == "" {
			return fmt.Errorf("%w: empty source path in queue item %d", ErrInvalid, i)
		}
	}
	e.mu.Lock()
	e.queue = append([]PlayItem(nil), items...)
	e.queueIdx = 0
	e.loop = loop
	e.mu.Unlock()
	return e.startItem(ctx, items[0], seekSeconds)
}

// startItem launches ffmpeg for one queue item. It is the shared core of
// StartAt and StartQueue.
func (e *FFmpegEngine) startItem(ctx context.Context, item PlayItem, seekSeconds float64) error {
	source := item.Path
	if err := ctx.Err(); err != nil {
		return err
	}
	// Remote sources (http/https) skip the local existence check; ffmpeg
	// pulls them directly.
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("engine: source %q: %w", source, err)
		}
	}

	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()

	e.mu.Lock()
	running := e.status.Running
	cfg := e.cfg
	e.mu.Unlock()

	if running {
		if err := e.stopLocked(ctx); err != nil {
			return err
		}
	}
	if len(cfg.Outputs) == 0 {
		return fmt.Errorf("%w: no outputs configured", ErrInvalid)
	}
	args, err := buildArgsWithAux(cfg, item, seekSeconds)
	if err != nil {
		return err
	}
	// Best-effort duration probe: Progress is out_time over the duration.
	// A probe failure (missing binary, unreadable source) leaves the
	// duration unknown and Progress at 0; it never fails the start.
	duration, _ := e.probeDuration(ctx, cfg.FFmpegPath, source)
	if err := ctx.Err(); err != nil {
		return err
	}

	procCtx, procCancel := context.WithCancel(context.Background())
	cmd := e.cmdBuilder(procCtx, cfg.FFmpegPath, args)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		procCancel()
		return fmt.Errorf("engine: ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		procCancel()
		return fmt.Errorf("engine: ffmpeg stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		procCancel()
		return fmt.Errorf("engine: start ffmpeg: %w", err)
	}

	outputs := make([]string, len(cfg.Outputs))
	for i, o := range cfg.Outputs {
		outputs[i] = o.URL
	}
	exited := make(chan struct{})

	e.mu.Lock()
	e.cmd = cmd
	e.procCancel = procCancel
	e.exited = exited
	e.source = source
	e.duration = duration
	e.lastSeek = seekSeconds
	e.elapsedSec = 0
	e.resumeSource = ""
	e.resumeSeek = 0
	e.intentionalStop = false
	e.aux = item
	e.status = Status{
		Running:    true,
		Pid:        cmd.Process.Pid,
		StartedAt:  time.Now(),
		SourcePath: source,
		OutputURLs: outputs,
	}
	e.stderrTail = nil
	e.mu.Unlock()

	log.Printf("engine: ffmpeg started (pid %d) for %q -> %d output(s)", cmd.Process.Pid, source, len(outputs))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.readProgress(stdout) }()
	go func() { defer wg.Done(); e.readStderr(stderr) }()
	go e.watchExit(cmd, procCancel, exited, &wg)
	return nil
}

// Stop terminates the process: SIGTERM followed by SIGKILL after the
// graceful timeout (5s by default). Windows cannot deliver SIGTERM, so the
// process is killed directly. Stop returns once the process is reaped and
// the status finalized; it is a no-op when nothing is running.
func (e *FFmpegEngine) Stop(ctx context.Context) error {
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()
	return e.stopLocked(ctx)
}

func (e *FFmpegEngine) stopLocked(ctx context.Context) error {
	e.mu.Lock()
	cmd := e.cmd
	cancel := e.procCancel
	exited := e.exited
	running := e.status.Running
	e.mu.Unlock()

	if !running || cmd == nil || cmd.Process == nil {
		return nil
	}

	e.mu.Lock()
	e.intentionalStop = true
	e.mu.Unlock()

	// Graceful phase. On Windows SIGTERM cannot be delivered to another
	// process; kill directly (TerminateProcess) instead.
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// The process exited between the state check and the signal:
		// fall through to the wait so the lost race is not an error.
		_ = cmd.Process.Kill()
	}

	timeout := e.stopTimeout
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		cancel()
	case <-timer.C:
		cancel()
	}
	// Kill phase: the canceled CommandContext SIGKILLs the process; wait
	// for the watcher to reap it.
	select {
	case <-exited:
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("engine: ffmpeg process %d did not exit after kill", cmd.Process.Pid)
	}
}

// Restart stops the running process (if any) and starts a new one for
// source.
func (e *FFmpegEngine) Restart(ctx context.Context, source string) error {
	if err := e.Stop(ctx); err != nil {
		return err
	}
	return e.Start(ctx, source)
}

// Pause suspends a running stream: ffmpeg is stopped (the push ends) and
// the playback position is remembered so Continue resumes exactly there.
// Pause is a no-op when nothing is running.
func (e *FFmpegEngine) Pause(ctx context.Context) error {
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()

	e.mu.Lock()
	if !e.status.Running {
		e.mu.Unlock()
		return nil
	}
	src := e.source
	seek := e.lastSeek + e.elapsedSec
	e.mu.Unlock()

	if err := e.stopLocked(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.resumeSource = src
	e.resumeSeek = seek
	e.status.Paused = true
	e.mu.Unlock()
	return nil
}

// Continue resumes the stream paused by Pause from the remembered
// position. It is a no-op when nothing is paused.
func (e *FFmpegEngine) Continue(ctx context.Context) error {
	e.lifeMu.Lock()
	e.mu.Lock()
	src := e.resumeSource
	seek := e.resumeSeek
	e.mu.Unlock()
	e.lifeMu.Unlock()

	if src == "" {
		return nil
	}
	// StartAt clears the resume bookkeeping: the fresh process owns the
	// state from here on.
	return e.StartAt(ctx, src, seek)
}

// Skip restarts the current source from the beginning, or starts it when
// nothing is playing. It is a no-op when there is no known source.
func (e *FFmpegEngine) Skip(ctx context.Context) error {
	e.lifeMu.Lock()
	e.mu.Lock()
	src := e.source
	e.mu.Unlock()
	e.lifeMu.Unlock()

	if src == "" {
		return nil
	}
	return e.StartAt(ctx, src, 0)
}

// Status returns a snapshot of the engine state.
func (e *FFmpegEngine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.status
	st.OutputURLs = append([]string(nil), st.OutputURLs...)
	if st.Running && !st.StartedAt.IsZero() {
		st.Uptime = time.Since(st.StartedAt).Truncate(time.Second).String()
	}
	return st
}

// UpdateConfig replaces the engine configuration: it is validated and
// persisted atomically, then applied. When a process is running it is
// restarted with the new configuration using the most recent source; with
// no source (or nothing running) only the configuration is replaced.
// UpdateConfig validates and persists the engine configuration. It never
// touches a running stream: the saved configuration takes effect on the
// next Start, or immediately through Apply. This lets an operator fix a
// mistake before it disrupts an ongoing stream.
func (e *FFmpegEngine) UpdateConfig(cfg Config) error {
	cfg, err := Validate(cfg)
	if err != nil {
		return err
	}
	if err := persistConfig(e.configFile, cfg); err != nil {
		return err
	}

	e.mu.Lock()
	e.cfg = cfg
	e.mu.Unlock()
	return nil
}

// Apply makes the saved configuration take effect on the running stream:
// when a stream is playing it is restarted with the new configuration;
// otherwise the configuration applies on the next Start.
func (e *FFmpegEngine) Apply(ctx context.Context) error {
	e.mu.Lock()
	source := e.source
	running := e.status.Running
	e.mu.Unlock()

	if running && source != "" {
		return e.Restart(ctx, source)
	}
	return nil
}

// Config returns the current configuration.
func (e *FFmpegEngine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg := e.cfg
	cfg.Outputs = append([]OutputConfig(nil), cfg.Outputs...)
	return cfg
}

// watchExit finalizes the status once the process exits and reports the
// exit through OnExit on a dedicated goroutine. The stderr scanner is
// drained first so LastError carries the complete tail.
func (e *FFmpegEngine) watchExit(cmd *exec.Cmd, procCancel context.CancelFunc, exited chan struct{}, scanners *sync.WaitGroup) {
	err := cmd.Wait()
	procCancel()
	scanners.Wait()

	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	e.mu.Lock()
	if e.cmd != cmd {
		// A newer process replaced this one before the exit was observed
		// (overlapping lifecycle calls); the newer process owns the status.
		onExit := e.OnExit
		e.mu.Unlock()
		close(exited)
		if onExit != nil {
			go onExit(code, err)
		}
		return
	}
	e.status.Running = false
	e.status.StoppedAt = time.Now()
	e.status.ExitCode = code
	intentional := e.intentionalStop
	if code != 0 {
		if tail := strings.Join(e.stderrTail, "\n"); tail != "" {
			e.status.LastError = tail
			log.Printf("engine: ffmpeg exited with code %d: %s", code, tail)
		} else if err != nil {
			e.status.LastError = err.Error()
		}
	}
	e.cmd = nil
	e.procCancel = nil
	onExit := e.OnExit
	e.mu.Unlock()

	close(exited)
	if onExit != nil {
		go onExit(code, err)
	}

	// Queue advance: after a clean end of an item (EOF, exit code 0) that
	// was not an intentional stop, move to the next queue item; after the
	// last item the queue either wraps (loop) or the stream ends.
	if code == 0 && !intentional {
		e.mu.Lock()
		q := e.queue
		idx := e.queueIdx
		loop := e.loop
		e.mu.Unlock()
		if len(q) > 1 {
			next := idx + 1
			if next >= len(q) {
				if loop {
					next = 0
				} else {
					next = -1
				}
			}
			if next >= 0 {
				item := q[next]
				e.mu.Lock()
				e.queueIdx = next
				e.mu.Unlock()
				log.Printf("engine: queue advance to %q (item %d/%d)", item.Path, next+1, len(q))
				time.AfterFunc(time.Second, func() {
					ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := e.startItem(ctx2, item, 0); err != nil {
						log.Printf("engine: queue advance failed: %v", err)
					}
				})
			} else {
				e.mu.Lock()
				e.queue = nil
				e.mu.Unlock()
				log.Printf("engine: queue finished, stream ended")
			}
		}
	}

	// Auto-reconnect: after an abnormal exit that was not an intentional
	// stop, restart the stream with the same source and seek offset after
	// the configured delay, mirroring the output reconnect_internal
	// behaviour of the original KPlayer.
	if code != 0 {
		e.mu.Lock()
		interval := e.cfg.ReconnectInterval
		src := e.source
		seek := e.lastSeek
		intentional := e.intentionalStop
		e.mu.Unlock()
		if interval > 0 && src != "" && !intentional {
			log.Printf("engine: reconnect in %ds after exit code %d", interval, code)
			time.AfterFunc(time.Duration(interval)*time.Second, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := e.StartAt(ctx, src, seek); err != nil {
					log.Printf("engine: reconnect failed: %v", err)
				}
			})
		}
	}
}

// readProgress consumes the ffmpeg -progress key=value lines from stdout
// and folds them into the status. The pipe closes when the process exits,
// ending the scan.
func (e *FFmpegEngine) readProgress(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		e.mu.Lock()
		applyProgressLine(&e.status, e.duration, line)
		if d, ok := progressOutTime(line); ok {
			e.elapsedSec = d.Seconds()
		}
		e.mu.Unlock()
	}
}

// progressOutTime extracts the media-out time carried by one ffmpeg
// -progress line (out_time_us / out_time_ms / out_time) and reports
// whether the line carried one.
func progressOutTime(line string) (time.Duration, bool) {
	sep := strings.IndexByte(line, '=')
	if sep < 0 {
		return 0, false
	}
	key := strings.TrimSpace(line[:sep])
	value := strings.TrimSpace(line[sep+1:])
	switch key {
	case "out_time_us", "out_time_ms":
		if us, err := strconv.ParseInt(value, 10, 64); err == nil {
			return time.Duration(us) * time.Microsecond, true
		}
	case "out_time":
		if d, err := parseOutTime(value); err == nil {
			return d, true
		}
	}
	return 0, false
}

// readStderr keeps the tail of ffmpeg's stderr; it surfaces through
// Status.LastError when the process exits abnormally.
func (e *FFmpegEngine) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		e.mu.Lock()
		e.stderrTail = append(e.stderrTail, line)
		if len(e.stderrTail) > e.maxTail {
			e.stderrTail = e.stderrTail[len(e.stderrTail)-e.maxTail:]
		}
		e.mu.Unlock()
	}
}

// probeDuration reads the source duration by asking ffmpeg to probe the
// input only (no output specified): ffmpeg prints "Duration: HH:MM:SS.xx"
// on stderr and exits without decoding. Best-effort: failures return 0.
func (e *FFmpegEngine) probeDuration(ctx context.Context, ffmpeg, source string) (time.Duration, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := e.cmdBuilder(probeCtx, ffmpeg, []string{"-hide_banner", "-i", source})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// ffmpeg exits non-zero when no output is specified; the duration line
	// on stderr is what matters, so the exit code is ignored.
	_ = cmd.Run()
	return parseDurationLine(stderr.String())
}

// parseDurationLine extracts "Duration: HH:MM:SS.xx" from ffmpeg probe
// stderr output.
func parseDurationLine(output string) (time.Duration, error) {
	idx := strings.Index(output, "Duration:")
	if idx < 0 {
		return 0, errors.New("engine: no duration in ffmpeg probe output")
	}
	rest := strings.TrimSpace(output[idx+len("Duration:"):])
	end := strings.IndexByte(rest, ',')
	if end < 0 {
		end = len(rest)
	}
	return parseOutTime(rest[:end])
}

// buildArgs renders the ffmpeg command line for cfg and source (a single
// item without auxiliary inputs); see buildArgsWithAux for the full form.
func buildArgs(cfg Config, source string, seekSeconds float64) ([]string, error) {
	return buildArgsWithAux(cfg, PlayItem{Path: source}, seekSeconds)
}

// buildArgsWithAux renders the ffmpeg command line for cfg and one play
// item, merging an external audio track and a burned-in subtitle when the
// item carries them:
//
//	ffmpeg -y -re [-hwaccel <accel>] [-ss N] -i <video> [-i <audio>]
//	  [-i <subtitle>] -progress pipe:1 -nostats -loglevel error
//	  [-map 0:v:0 [-map 1:a:0]] [-vf [<filters>,]<subtitles=...>]
//	  [-c:v <codec> -b:v <bitrate>k -r <fps> -s WxH] [-c:a aac -b:a 128k]
//	  -f flv <url> ...
//
// RTMP/FLV has no subtitle track, so a subtitle file is burned into the
// picture with the subtitles filter (libass). An external audio replaces
// the source's own audio track. One output parameter group is appended per
// configured output; the last argument of the command is the last URL.
func buildArgsWithAux(cfg Config, item PlayItem, seekSeconds float64) ([]string, error) {
	args := []string{"-y", "-re"}
	accel := ""
	for _, o := range cfg.Outputs {
		if strings.TrimSpace(o.HWAccel) != "" {
			accel = strings.TrimSpace(o.HWAccel)
			break
		}
	}
	if accel != "" {
		args = append(args, "-hwaccel", accel)
	}
	// Input seek: -ss before -i seeks the demuxer quickly; a non-integer
	// offset needs -ss after -i for frame accuracy, but for streaming
	// starts a whole-second offset before the input is fast and reliable.
	if seekSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(seekSeconds, 'f', -1, 64))
	}
	args = append(args, "-i", item.Path)
	extAudio := strings.TrimSpace(item.AudioPath) != ""
	extSub := strings.TrimSpace(item.SubtitlePath) != ""
	if extAudio {
		args = append(args, "-i", strings.TrimSpace(item.AudioPath))
	}
	if extSub {
		args = append(args, "-i", strings.TrimSpace(item.SubtitlePath))
	}
	args = append(args, "-progress", "pipe:1", "-nostats", "-loglevel", "error")

	// The subtitle file is burned into the video with the subtitles filter;
	// it is appended after the output's own filter chain.
	var subFilter string
	if extSub {
		subFilter = "subtitles=" + escapeFilterPath(strings.TrimSpace(item.SubtitlePath))
	}

	for _, o := range cfg.Outputs {
		if extAudio {
			// External audio replaces the source's audio track.
			args = append(args, "-map", "0:v:0", "-map", "1:a:0")
		} else {
			args = append(args, "-map", "0")
		}
		filters := strings.TrimSpace(o.Filters)
		if subFilter != "" {
			if filters != "" {
				filters = filters + "," + subFilter
			} else {
				filters = subFilter
			}
		}
		if filters != "" {
			args = append(args, "-vf", filters)
		}
		codec := o.Codec
		if codec == "" {
			codec = defaultCodec
		}
		args = append(args, "-c:v", codec)
		if o.BitrateKbps > 0 {
			args = append(args, "-b:v", strconv.Itoa(o.BitrateKbps)+"k")
		}
		if o.FPS > 0 {
			args = append(args, "-r", strconv.Itoa(o.FPS))
		}
		if o.Width > 0 && o.Height > 0 {
			args = append(args, "-s", fmt.Sprintf("%dx%d", o.Width, o.Height))
		}
		if extAudio {
			// FLV/RTMP accepts AAC; transcode the external track.
			args = append(args, "-c:a", "aac", "-b:a", "128k")
		}
		if af := strings.TrimSpace(o.AudioFilters); af != "" {
			args = append(args, "-af", af)
		}
		if o.AudioChannels > 0 {
			args = append(args, "-ac", strconv.Itoa(o.AudioChannels))
		}
		if o.AudioSampleRate > 0 {
			args = append(args, "-ar", strconv.Itoa(o.AudioSampleRate))
		}
		args = append(args, "-f", "flv", o.URL)
	}
	return args, nil
}

// escapeFilterPath makes a file path safe inside a ffmpeg filter argument:
// colons and commas (which the filter graph treats as separators) are
// backslash-escaped and Windows separators are normalized to '/'.
func escapeFilterPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.ReplaceAll(p, ":", "\\:")
	p = strings.ReplaceAll(p, ",", "\\,")
	return p
}

// applyProgressLine folds one ffmpeg -progress "key=value" line into st.
// Unknown keys and malformed values are ignored. out_time_us and
// out_time_ms both carry microseconds (ffmpeg quirk: the "ms" name lies);
// out_time carries "HH:MM:SS.microseconds".
func applyProgressLine(st *Status, duration time.Duration, line string) {
	sep := strings.IndexByte(line, '=')
	if sep < 0 {
		return
	}
	key := strings.TrimSpace(line[:sep])
	value := strings.TrimSpace(line[sep+1:])
	switch key {
	case "frame":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			st.Frame = n
		}
	case "fps":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			st.FPS = f
		}
	case "bitrate":
		st.BitrateKbps = parseBitrate(value)
	case "out_time_us", "out_time_ms":
		if us, err := strconv.ParseInt(value, 10, 64); err == nil {
			st.Progress = progressPercent(time.Duration(us)*time.Microsecond, duration)
		}
	case "out_time":
		if d, err := parseOutTime(value); err == nil {
			st.Progress = progressPercent(d, duration)
		}
	}
}

// parseBitrate converts an ffmpeg bitrate value ("512.0kbits/s",
// "98765.4bits/s") to whole kbit/s. Unparseable values yield 0.
func parseBitrate(value string) int {
	mult := 1.0
	switch {
	case strings.HasSuffix(value, "kbits/s"):
		value = strings.TrimSuffix(value, "kbits/s")
	case strings.HasSuffix(value, "bits/s"):
		value = strings.TrimSuffix(value, "bits/s")
		mult = 1.0 / 1000
	default:
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}
	return int(f * mult)
}

// parseOutTime parses ffmpeg's "HH:MM:SS.microseconds" time format.
func parseOutTime(value string) (time.Duration, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("engine: invalid time %q", value)
	}
	hours, errH := strconv.Atoi(parts[0])
	minutes, errM := strconv.Atoi(parts[1])
	sec, errS := strconv.ParseFloat(parts[2], 64)
	if errH != nil || errM != nil || errS != nil {
		return 0, fmt.Errorf("engine: invalid time %q", value)
	}
	return time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(sec*float64(time.Second)), nil
}

// progressPercent converts elapsed playback time to a percentage (0-100)
// of the probed source duration. An unknown duration yields 0.
func progressPercent(elapsed, total time.Duration) float64 {
	if total <= 0 || elapsed <= 0 {
		return 0
	}
	p := float64(elapsed) / float64(total) * 100
	if p > 100 {
		return 100
	}
	return p
}