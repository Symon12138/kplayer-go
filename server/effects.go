package server

// Effects (插件/效果) management: a persistent, ordered list of effects
// mirroring the official KPlayer plugin.lists model (multiple plugins, each
// with type + params). Effects are rendered into ffmpeg filter chains and
// applied to the engine's first output, so the console can layer several
// watermarks / subtitles / audio adjustments at once.

import (
	"github.com/bytelang/kplayer/engine"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EffectType identifies the built-in effect kinds; each maps to ffmpeg
// filter parameters.
type EffectType string

const (
	// EffectTextWatermark draws text on the picture (drawtext).
	EffectTextWatermark EffectType = "text-watermark"
	// EffectImageWatermark overlays an image (overlay).
	EffectImageWatermark EffectType = "image-watermark"
	// EffectSubtitle burns a subtitle file into the picture (subtitles).
	EffectSubtitle EffectType = "subtitle"
	// EffectAudio adjusts the output audio (volume / A-V sync delay).
	EffectAudio EffectType = "audio"
	// EffectMarquee scrolls a text marquee across the picture (drawtext with
	// a time-based x expression).
	EffectMarquee EffectType = "marquee"
	// EffectVideoAdjust adjusts brightness/contrast/saturation (eq filter).
	EffectVideoAdjust EffectType = "video-adjust"
	// EffectTranscodePreset applies a resolution/bitrate preset to the
	// output (handled at apply time, not as a filter).
	EffectTranscodePreset EffectType = "transcode-preset"
)

// Effect is one entry of the effect list.
type Effect struct {
	ID      string            `json:"id"`
	Type    EffectType        `json:"type"`
	Name    string            `json:"name"`
	Enabled bool              `json:"enabled"`
	Params  map[string]string `json:"params,omitempty"`
}

// effectFile persists the effect list in the working directory.
const effectFile = "effects.json"

// EffectManager owns the persistent effect list.
type EffectManager struct {
	mu      sync.Mutex
	file    string
	effects []*Effect
}

// NewEffectManager loads the persisted effect list (missing/corrupt file
// starts empty).
func NewEffectManager(file string) *EffectManager {
	m := &EffectManager{file: file, effects: []*Effect{}}
	if raw, err := os.ReadFile(file); err == nil {
		var data struct {
			Effects []*Effect `json:"effects"`
		}
		if json.Unmarshal(raw, &data) == nil && data.Effects != nil {
			m.effects = data.Effects
		}
	}
	return m
}

// List returns the ordered effect list.
func (m *EffectManager) List() []*Effect {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Effect(nil), m.effects...)
}

// Set replaces the whole list and persists it.
func (m *EffectManager) Set(effects []*Effect) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if effects == nil {
		effects = []*Effect{}
	}
	for _, e := range effects {
		if e.ID == "" {
			e.ID = fmt.Sprintf("e%x", time.Now().UnixNano())
		}
	}
	m.effects = effects
	raw, err := json.MarshalIndent(map[string]interface{}{"effects": effects}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(m.file)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := os.WriteFile(tmpName, raw, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, m.file)
}

// Render converts the enabled effects into a video filter chain (-vf) and
// an audio filter chain (-af). Effects apply in list order.
func (m *EffectManager) Render() (vf, af string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var vfParts, afParts []string
	for _, e := range m.effects {
		if !e.Enabled {
			continue
		}
		p := e.Params
		if p == nil {
			p = map[string]string{}
		}
		switch e.Type {
		case EffectTextWatermark:
			text := strings.TrimSpace(p["text"])
			if text == "" {
				return "", "", fmt.Errorf("effects: %w: text watermark needs text", ErrEffectInvalid)
			}
			xy := map[string]string{
				"tl": "x=20:y=20", "tr": "x=w-tw-20:y=20", "bl": "x=20:y=h-th-20",
				"br": "x=w-tw-20:y=h-th-20", "c": "x=(w-text_w)/2:y=(h-text_h)/2",
			}[p["position"]]
			if xy == "" {
				xy = "x=20:y=20"
			}
			size := orDefault(p["font_size"], "28")
			color := orDefault(p["color"], "white")
			opacity := orDefault(p["opacity"], "1")
			esc := strings.ReplaceAll(text, "'", "\\'")
			vfParts = append(vfParts, "drawtext=text='"+esc+"':fontsize="+size+":fontcolor="+color+"@"+opacity+":"+xy)
		case EffectImageWatermark:
			img := strings.TrimSpace(p["path"])
			if img == "" {
				return "", "", fmt.Errorf("effects: %w: image watermark needs path", ErrEffectInvalid)
			}
			xy := map[string]string{
				"tl": "20:20", "tr": "main_w-overlay_w-20:20", "bl": "20:main_h-overlay_h-20",
				"br": "main_w-overlay_w-20:main_h-overlay_h-20", "c": "(main_w-overlay_w)/2:(main_h-overlay_h)/2",
			}[p["position"]]
			if xy == "" {
				xy = "20:20"
			}
			vfParts = append(vfParts, "overlay="+xy)
		case EffectSubtitle:
			sub := strings.TrimSpace(p["path"])
			if sub == "" {
				return "", "", fmt.Errorf("effects: %w: subtitle needs path", ErrEffectInvalid)
			}
			style := "FontSize=" + orDefault(p["font_size"], "18")
			if c := strings.TrimSpace(p["color"]); c != "" {
				style += ",PrimaryColour=&H" + bgrColor(c) + "&"
			}
			style += ",Alignment=" + orDefault(p["alignment"], "2")
			vfParts = append(vfParts, "subtitles="+escapeFilterPath(sub)+":force_style='"+style+"'")
		case EffectAudio:
			if vol := strings.TrimSpace(p["volume"]); vol != "" && vol != "100" {
				afParts = append(afParts, fmt.Sprintf("volume=%.2f", parsePercent(vol)))
			}
			if delay := strings.TrimSpace(p["delay_ms"]); delay != "" && delay != "0" {
				afParts = append(afParts, "adelay=all="+delay)
			}
		case EffectMarquee:
			text := strings.TrimSpace(p["text"])
			if text == "" {
				return "", "", fmt.Errorf("effects: %w: marquee needs text", ErrEffectInvalid)
			}
			speed := orDefault(p["speed"], "60")
			pos := orDefault(p["position"], "top")
			y := "20"
			if pos == "bottom" {
				y = "h-th-20"
			} else if pos == "middle" {
				y = "(h-text_h)/2"
			}
			esc := strings.ReplaceAll(text, "'", "'")
			vfParts = append(vfParts, fmt.Sprintf(
				"drawtext=text='%s':fontsize=%s:fontcolor=%s@%s:x=w-mod(t*%s\\,w+text_w):y=%s",
				esc, orDefault(p["font_size"], "24"), orDefault(p["color"], "white"),
				orDefault(p["opacity"], "1"), speed, y))
		case EffectVideoAdjust:
			parts := []string{}
			if v := strings.TrimSpace(p["brightness"]); v != "" {
				parts = append(parts, "brightness="+v)
			}
			if v := strings.TrimSpace(p["contrast"]); v != "" {
				parts = append(parts, "contrast="+v)
			}
			if v := strings.TrimSpace(p["saturation"]); v != "" {
				parts = append(parts, "saturation="+v)
			}
			if len(parts) == 0 {
				return "", "", fmt.Errorf("effects: %w: video adjust needs a parameter", ErrEffectInvalid)
			}
			vfParts = append(vfParts, "eq="+strings.Join(parts, ":"))
		case EffectTranscodePreset:
			// Applied by handleEffects to the output parameters; no filter.
			continue
		default:
			return "", "", fmt.Errorf("effects: %w: unknown type %q", ErrEffectInvalid, e.Type)
		}
	}
	return strings.Join(vfParts, ","), strings.Join(afParts, ","), nil
}

// escapeFilterPath makes a file path safe inside a ffmpeg filter argument:
// colons and commas are backslash-escaped and Windows separators are
// normalized to '/'.
func escapeFilterPath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.ReplaceAll(path, ":", "\\:")
	path = strings.ReplaceAll(path, ",", "\\,")
	return path
}

// transcodePreset maps a preset name to output encode parameters.
func transcodePreset(name string) (width, height, bitrateKbps, fps int, ok bool) {
	switch name {
	case "1080p":
		return 1920, 1080, 4000, 25, true
	case "720p":
		return 1280, 720, 2500, 25, true
	case "480p":
		return 854, 480, 1200, 25, true
	case "360p":
		return 640, 360, 800, 25, true
	}
	return 0, 0, 0, 0, false
}

// orDefault returns v when non-empty, else def.
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

// parsePercent converts a "0-300" percent string to a 0-3 scale factor.
func parsePercent(v string) float64 {
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return 1
	}
	return float64(n) / 100
}

// bgrColor converts a #RRGGBB color to the ASS &HB BGGR format.
func bgrColor(c string) string {
	c = strings.TrimPrefix(strings.TrimSpace(c), "#")
	if len(c) != 6 {
		return "FFFFFF"
	}
	return strings.ToUpper(c[4:6] + c[2:4] + c[0:2])
}

// ErrEffectInvalid reports an invalid effect definition.
var ErrEffectInvalid = fmt.Errorf("effects: invalid")

// mergeEffectFilters makes the global effect chain the BASE of every
// output's filter graph: the rendered -vf/-af strings run first and each
// output's own filters append after them. Nil/empty effects return the
// outputs unchanged.
func mergeEffectFilters(outputs []engine.OutputConfig, vf, af string) []engine.OutputConfig {
	if len(outputs) == 0 || (vf == "" && af == "") {
		return outputs
	}
	out := make([]engine.OutputConfig, len(outputs))
	for i, o := range outputs {
		out[i] = o
		out[i].Filters = joinFilters(vf, out[i].Filters)
		out[i].AudioFilters = joinFilters(af, out[i].AudioFilters)
	}
	return out
}

func joinFilters(base, extra string) string {
	if extra == "" {
		return base
	}
	return base + "," + extra
}

// handleEffects serves GET/POST /effects: the effect list and its
// application to the engine output.
func (h *managementHandler) handleEffects(w http.ResponseWriter, r *http.Request) {
	if h.effects == nil {
		h.writeError(w, http.StatusNotFound, "effects manager not enabled")
		return
	}
	switch {
	case r.Method == http.MethodGet:
		vf, af, err := h.effects.Render()
		if err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{
			"effects": h.effects.List(),
			"vf":      vf,
			"af":      af,
		})
	case r.Method == http.MethodPost:
		var req struct {
			Effects []*Effect `json:"effects"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if err := h.effects.Set(req.Effects); err != nil {
			h.writeManagementError(w, err)
			return
		}
		vf, af, err := h.effects.Render()
		if err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Apply to the engine's first output (the main push path).
		cfg := h.engine.Config()
		if len(cfg.Outputs) == 0 {
			h.writeError(w, http.StatusBadRequest, "engine has no outputs configured")
			return
		}
		// Transcode preset: the last enabled preset wins and rewrites the
		// output encode parameters.
		for _, e := range h.effects.List() {
			if !e.Enabled || e.Type != EffectTranscodePreset {
				continue
			}
			if w, h, b, f, ok := transcodePreset(strings.TrimSpace(e.Params["preset"])); ok {
				cfg.Outputs[0].Width = w
				cfg.Outputs[0].Height = h
				cfg.Outputs[0].BitrateKbps = b
				cfg.Outputs[0].FPS = f
			}
		}
		out := cfg.Outputs[0]
		out.Filters = vf
		out.AudioFilters = af
		cfg.Outputs[0] = out
		if err := h.engine.UpdateConfig(cfg); err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": true, "effects": h.effects.List(), "vf": vf, "af": af,
		})
	default:
		http.NotFound(w, r)
	}
}