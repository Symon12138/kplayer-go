package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultVideoExts is the extension allowlist used when ScanOptions.Extensions
// is empty. It covers the common container formats ffmpeg can decode; the
// engine itself does not filter by extension, so other formats still play.
var DefaultVideoExts = []string{
	".mp4", ".mkv", ".flv", ".avi", ".mov", ".wmv", ".ts", ".m4v", ".webm", ".rmvb",
	".mpeg", ".mpg", ".m2ts", ".mts", ".3gp", ".f4v", ".vob", ".rm", ".asf",
}

// ScanOptions controls directory scanning.
type ScanOptions struct {
	// Extensions filters files by (lower-cased) extension; empty means
	// DefaultVideoExts. Entries may be given with or without the leading dot.
	Extensions []string
	// IncludeSubdirs walks the directory tree recursively. The zero value
	// (false) scans only the top-level directory and does not descend into
	// subdirectories.
	IncludeSubdirs bool
	// Probe enables optional ffprobe metadata enrichment. Probing is best
	// effort: when ffprobe is missing or fails, the scan still succeeds and
	// the affected entries are simply not marked as probed.
	Probe bool
	// ProbeTimeout bounds each ffprobe invocation; default 10s.
	ProbeTimeout time.Duration
}

// ScanResult reports the outcome of a scan+merge operation.
type ScanResult struct {
	Added   []*Media `json:"added"`
	Updated []*Media `json:"updated"`
	Skipped int      `json:"skipped"`
}

// ScanDirectory walks root and returns one Media entry per matching file.
// It never fails because of a single unreadable file: such files are skipped.
// The context can be used to cancel a long scan.
func ScanDirectory(ctx context.Context, root string, opts ScanOptions) ([]*Media, error) {
	exts := opts.Extensions
	if len(exts) == 0 {
		exts = DefaultVideoExts
	}
	extSet := make(map[string]struct{}, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		extSet[e] = struct{}{}
	}
	if len(extSet) == 0 {
		return nil, fmt.Errorf("scan: %w: no extensions configured", ErrInvalid)
	}

	out := make([]*Media, 0)

	probeTimeout := opts.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 10 * time.Second
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("scan: resolve root %q: %w", root, err)
	}
	if fi, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("scan: root %q: %w", abs, err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("scan: root %q is not a directory", abs)
	}

	walkFn := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries, keep walking
		}
		if d.IsDir() {
			if path != abs && !opts.IncludeSubdirs {
				return filepath.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		ext := strings.ToLower(filepath.Ext(path))
		if _, ok := extSet[ext]; !ok {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		m := &Media{
			Name:    d.Name(),
			Path:    path,
			Ext:     ext,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if opts.Probe {
			if err := probeMedia(ctx, m, probeTimeout); err == nil {
				m.Probed = true
			}
		}
		out = append(out, m)
		return nil
	}

	// WalkDir descends in lexical order, giving deterministic output.
	if err := filepath.WalkDir(abs, walkFn); err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return nil, err
		}
		return nil, fmt.Errorf("scan: walk %q: %w", abs, err)
	}
	return out, nil
}

// probeMedia enriches m with ffprobe metadata (duration, resolution,
// bitrate). It returns ErrProbeUnavailable when ffprobe is not installed and
// a descriptive error when probing fails; in both cases the scan should
// continue without the metadata.
func probeMedia(ctx context.Context, m *Media, timeout time.Duration) error {
	path, err := exec.LookPath("ffprobe")
	if err != nil {
		return ErrProbeUnavailable
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, path,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		m.Path,
	)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("probe %q: %w", m.Path, err)
	}

	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return fmt.Errorf("probe %q: parse output: %w", m.Path, err)
	}

	if v := parsed.Format.Duration; v != "" {
		if sec, err := strconv.ParseFloat(v, 64); err == nil {
			m.Duration = sec
		}
	}
	if v := parsed.Format.BitRate; v != "" {
		if rate, err := strconv.ParseInt(v, 10, 64); err == nil {
			m.Bitrate = rate
		}
	}
	for _, s := range parsed.Streams {
		if s.CodecType == "video" {
			m.Width = s.Width
			m.Height = s.Height
			break
		}
	}
	return nil
}
