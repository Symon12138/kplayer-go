package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrInvalid is the sentinel returned for any invalid engine configuration.
// The server layer maps it to HTTP 400.
var ErrInvalid = errors.New("engine: invalid configuration")

const (
	// ConfigFile is the engine configuration file, relative to the working
	// directory, mirroring management.json for the management layer.
	ConfigFile = "engine.json"

	defaultFFmpegPath    = "ffmpeg"
	defaultCodec         = "libx264"
	defaultProbeInterval = 2 * time.Second
	defaultStopTimeout   = 5 * time.Second
)

// DefaultConfig returns the engine configuration with all defaults applied.
// It carries no outputs: playback stays rejected (ErrInvalid) until outputs
// are configured through UpdateConfig / POST /engine/ffmpeg.
func DefaultConfig() Config {
	return Config{FFmpegPath: DetectFFmpeg(), ProbeInterval: defaultProbeInterval}
}

// DetectFFmpeg locates the ffmpeg binary so the operator never has to
// configure the path by hand. It checks the process PATH first (exec.LookPath),
// then a set of common installation directories, and finally falls back to
// the bare name so the exec layer can still resolve it through PATH at spawn
// time (the error then surfaces when ffmpeg is genuinely missing).
func DetectFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	exe := "ffmpeg"
	if runtime.GOOS == "windows" {
		exe = "ffmpeg.exe"
	}
	for _, dir := range []string{
		"/usr/bin", "/usr/local/bin", "/opt/ffmpeg/bin", "/usr/lib/ffmpeg/bin",
		"C:/ffmpeg/bin", "C:/Program Files/ffmpeg/bin",
		"C:/Program Files (x86)/ffmpeg/bin",
	} {
		p := filepath.Join(dir, exe)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return defaultFFmpegPath
}

// normalize applies the defaults to a configuration without requiring it to
// be complete. Validate builds on it and adds the requirements.
func normalize(cfg Config) Config {
	cfg.FFmpegPath = strings.TrimSpace(cfg.FFmpegPath)
	// An empty path or the bare default name is upgraded to the detected
	// absolute path, so a fresh install works without any configuration.
	if cfg.FFmpegPath == "" || cfg.FFmpegPath == defaultFFmpegPath {
		cfg.FFmpegPath = DetectFFmpeg()
	}
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = defaultProbeInterval
	}
	outputs := make([]OutputConfig, len(cfg.Outputs))
	for i, o := range cfg.Outputs {
		o.Codec = strings.TrimSpace(o.Codec)
		if o.Codec == "" {
			o.Codec = defaultCodec
		}
		outputs[i] = o
	}
	cfg.Outputs = outputs
	return cfg
}

// Validate normalizes cfg and checks its requirements:
//
//   - at least one output with a non-empty URL;
//   - width/height, bitrate and fps, when provided (> 0), are positive and
//     width and height are set together (a zero value omits the option);
//   - filters pass through verbatim without syntax validation.
//
// It returns the normalized configuration on success.
func Validate(cfg Config) (Config, error) {
	cfg = normalize(cfg)
	if len(cfg.Outputs) == 0 {
		return Config{}, fmt.Errorf("%w: at least one output is required", ErrInvalid)
	}
	for i, o := range cfg.Outputs {
		if strings.TrimSpace(o.URL) == "" {
			return Config{}, fmt.Errorf("%w: output %d: empty URL", ErrInvalid, i)
		}
		if o.Width < 0 || o.Height < 0 {
			return Config{}, fmt.Errorf("%w: output %d: width and height must not be negative", ErrInvalid, i)
		}
		if (o.Width > 0) != (o.Height > 0) {
			return Config{}, fmt.Errorf("%w: output %d: width and height must be set together", ErrInvalid, i)
		}
		if o.BitrateKbps < 0 {
			return Config{}, fmt.Errorf("%w: output %d: bitrate must not be negative", ErrInvalid, i)
		}
		if o.FPS < 0 {
			return Config{}, fmt.Errorf("%w: output %d: fps must not be negative", ErrInvalid, i)
		}
	}
	return cfg, nil
}

// loadConfig reads the engine configuration from path. A missing file
// yields the default configuration without error; a corrupt file is an
// error.
func loadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("engine: read config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("engine: parse config %q: %w", path, err)
	}
	return normalize(cfg), nil
}

// Load reads the engine configuration from ConfigFile in the working
// directory; see loadConfig.
func Load() (Config, error) {
	return loadConfig(ConfigFile)
}

// persistConfig writes cfg atomically to path: temp file, fsync, rename
// (the same pattern the management store uses). A crash mid-write leaves
// either the old or the new file, never a truncated mix.
func persistConfig(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("engine: mkdir %q: %w", dir, err)
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("engine: marshal config: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("engine: create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("engine: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("engine: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("engine: close temp file: %w", err)
	}
	// On Windows, renaming over a freshly written target can intermittently
	// fail with "Access is denied": the OS or an antivirus scanner holds the
	// target open for a moment. A short, bounded retry rides that out.
	var renameErr error
	for attempt := 0; attempt < 4; attempt++ {
		renameErr = os.Rename(tmpName, path)
		if renameErr == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if renameErr != nil {
		return fmt.Errorf("engine: rename temp file onto %q: %w", path, renameErr)
	}
	committed = true
	return nil
}
