package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytelang/kplayer/engine"
	"github.com/bytelang/kplayer/management"
)

// streamFile persists the multi-stream task definitions in the working
// directory, mirroring engine.json for the single engine.
const streamFile = "stream.json"

// StreamTask describes one independent push stream: its own content source
// (a media library item) and its own list of output platforms. Each task
// runs its own ffmpeg process, so different tasks can push different
// content to different platforms at the same time.
type StreamTask struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	// MediaID references a media library entry (single file or directory);
	// PlaylistID references a playlist. Exactly one is set: the task's
	// content source. Playlists expand to their items (directories expand
	// to their files in file-name order).
	MediaID    string                `json:"mediaId,omitempty"`
	PlaylistID string                `json:"playlistId,omitempty"`
	FFmpegPath string                `json:"ffmpegPath,omitempty"`
	Outputs    []engine.OutputConfig `json:"outputs"`
	// ReconnectInterval is the delay in seconds before the engine
	// automatically restarts the stream after an abnormal exit (0 disables).
	ReconnectInterval int       `json:"reconnectInterval,omitempty"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// StreamView is the REST representation of a task plus its live engine
// status.
type StreamView struct {
	StreamTask
	Running     bool    `json:"running"`
	Pid         int     `json:"pid,omitempty"`
	SourcePath  string  `json:"sourcePath,omitempty"`
	BitrateKbps int     `json:"bitrateKbps,omitempty"`
	FPS         float64 `json:"fps,omitempty"`
	Frame       int64   `json:"frame,omitempty"`
	Progress    float64 `json:"progress,omitempty"`
	LastError   string  `json:"lastError,omitempty"`
}

// StreamManager owns the stream tasks and their ffmpeg engine instances.
// The task definitions persist to stream.json; engine instances are created
// per task on Start and never persist their own config file (the manager is
// the source of truth for task configuration).
type StreamManager struct {
	mu    sync.Mutex
	file  string
	tasks map[string]*StreamTask
	order []string
	engs  map[string]*engine.FFmpegEngine
	// mediaSvc is used to expand directory media into play queues; nil
	// falls back to playing the raw path.
	mediaSvc *management.MediaService
	// resolveMedia maps a media library id to its media entry (a file or a
	// directory).
	resolveMedia func(ctx context.Context, mediaID string) (*management.Media, error)
}

// NewStreamManager loads the persisted tasks (if any) and returns the
// manager. A missing or corrupt file starts with an empty task list.
func NewStreamManager(file string, resolveMedia func(ctx context.Context, mediaID string) (*management.Media, error)) *StreamManager {
	m := &StreamManager{
		file:         file,
		tasks:        map[string]*StreamTask{},
		engs:         map[string]*engine.FFmpegEngine{},
		resolveMedia: resolveMedia,
		mediaSvc:     management.NewMediaService(nil),
	}
	if raw, err := os.ReadFile(file); err == nil {
		var data struct {
			Tasks []*StreamTask `json:"tasks"`
		}
		if json.Unmarshal(raw, &data) == nil {
			for _, t := range data.Tasks {
				if t == nil || t.ID == "" {
					continue
				}
				m.tasks[t.ID] = t
				m.order = append(m.order, t.ID)
			}
		}
	}
	return m
}

func (m *StreamManager) persistLocked() error {
	if m.file == "" {
		return nil
	}
	tasks := make([]*StreamTask, 0, len(m.order))
	for _, id := range m.order {
		tasks = append(tasks, m.tasks[id])
	}
	raw, err := json.MarshalIndent(map[string]interface{}{"tasks": tasks}, "", "  ")
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

func newStreamID() string {
	return fmt.Sprintf("s%x", time.Now().UnixNano())
}

// splitContent splits a task content reference into its media or playlist
// id. A "pl:" prefix marks a playlist; anything else is a media id.
func splitContent(content string) (mediaID, playlistID string) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "pl:") {
		return "", strings.TrimSpace(strings.TrimPrefix(content, "pl:"))
	}
	return content, ""
}

// Create adds a task and persists the definitions. content is either a
// media library id (single file or directory) or, prefixed with "pl:",
// a playlist id; exactly one source must be provided.
func (m *StreamManager) Create(name, content string, ffmpegPath string, reconnectInterval int, outputs []engine.OutputConfig) (*StreamTask, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("stream: %w: empty name", engine.ErrInvalid)
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("stream: %w: empty content", engine.ErrInvalid)
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("stream: %w: at least one output is required", engine.ErrInvalid)
	}
	for i, o := range outputs {
		if strings.TrimSpace(o.URL) == "" {
			return nil, fmt.Errorf("stream: %w: output %d: empty URL", engine.ErrInvalid, i)
		}
	}
	mediaID, playlistID := splitContent(content)
	t := &StreamTask{
		ID:                newStreamID(),
		Name:              strings.TrimSpace(name),
		MediaID:           mediaID,
		PlaylistID:        playlistID,
		FFmpegPath:        strings.TrimSpace(ffmpegPath),
		ReconnectInterval: reconnectInterval,
		Outputs:           outputs,
		Enabled:           true,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	return t, m.persistLocked()
}

// Update replaces the task definition. A running engine is stopped and
// discarded so the new outputs apply on the next Start.
func (m *StreamManager) Update(id, name, content, ffmpegPath string, reconnectInterval int, outputs []engine.OutputConfig) (*StreamTask, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("stream: %w: empty name", engine.ErrInvalid)
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("stream: %w: empty content", engine.ErrInvalid)
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("stream: %w: at least one output is required", engine.ErrInvalid)
	}
	for i, o := range outputs {
		if strings.TrimSpace(o.URL) == "" {
			return nil, fmt.Errorf("stream: %w: output %d: empty URL", engine.ErrInvalid, i)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, fmt.Errorf("stream %q: not found", id)
	}
	eng := m.engs[id]
	delete(m.engs, id)
	mediaID, playlistID := splitContent(content)
	t.Name = strings.TrimSpace(name)
	t.MediaID = mediaID
	t.PlaylistID = playlistID
	t.FFmpegPath = strings.TrimSpace(ffmpegPath)
	t.ReconnectInterval = reconnectInterval
	t.Outputs = outputs
	t.UpdatedAt = time.Now()
	if err := m.persistLocked(); err != nil {
		return nil, err
	}
	if eng != nil {
		_ = eng.Stop(context.Background())
	}
	return t, nil
}

// Delete removes a task, stopping its engine first.
func (m *StreamManager) Delete(id string) error {
	m.mu.Lock()
	eng := m.engs[id]
	delete(m.tasks, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	err := m.persistLocked()
	m.mu.Unlock()
	if eng != nil {
		_ = eng.Stop(context.Background())
	}
	return err
}

func (m *StreamManager) Get(id string) (*StreamTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, fmt.Errorf("stream %q: not found", id)
	}
	return t, nil
}

func (m *StreamManager) List() []*StreamTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*StreamTask, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.tasks[id])
	}
	return out
}

func (m *StreamManager) taskEngine(t *StreamTask) *engine.FFmpegEngine {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.engs[t.ID]; ok {
		return e
	}
	ffmpeg := t.FFmpegPath
	if ffmpeg == "" {
		// Auto-detect: the same default the main engine uses, so a fresh
		// deployment needs no manual ffmpeg path anywhere.
		ffmpeg = engine.DetectFFmpeg()
	}
	e := engine.NewFFmpegEngine(engine.Config{
		FFmpegPath:        ffmpeg,
		Outputs:           t.Outputs,
		ReconnectInterval: t.ReconnectInterval,
	})
	m.engs[t.ID] = e
	return e
}

// Start begins the task's stream: resolves the media entry (a single file
// or a directory expanded into its sorted video queue) and starts the
// task's own ffmpeg process with the task's outputs.
func (m *StreamManager) Start(ctx context.Context, id string) error {
	t, err := m.Get(id)
	if err != nil {
		return err
	}
	if m.resolveMedia == nil {
		return fmt.Errorf("stream %q: media resolver unavailable", id)
	}
	media, err := m.resolveMedia(ctx, t.MediaID)
	if err != nil {
		return fmt.Errorf("stream %q: resolve media: %w", id, err)
	}
	items, err := m.expandMedia(ctx, media)
	if err != nil {
		return fmt.Errorf("stream %q: expand media: %w", id, err)
	}
	eng := m.taskEngine(t)
	// StartQueue 单元素与 Start 等价，但会携带媒体的外挂音频/字幕。
	return eng.StartQueue(ctx, items, 0, false)
}

// expandMedia returns the concrete play items of a media entry: a single
// file keeps itself; a directory expands into its video files in the
// entry's sort order (see MediaService.Expand).
func (m *StreamManager) expandMedia(ctx context.Context, media *management.Media) ([]engine.PlayItem, error) {
	if m.mediaSvc != nil {
		expanded, err := m.mediaSvc.Expand(media)
		if err != nil {
			return nil, err
		}
		out := make([]engine.PlayItem, 0, len(expanded))
		for _, item := range expanded {
			out = append(out, engine.PlayItem{
				Path:         item.Path,
				AudioPath:    item.AudioPath,
				SubtitlePath: item.SubtitlePath,
			})
		}
		return out, nil
	}
	// Fallback when no media service is wired: play the raw path.
	return []engine.PlayItem{{Path: media.Path, AudioPath: media.AudioPath, SubtitlePath: media.SubtitlePath}}, nil
}

// Stop halts the task's stream. Stopping a task that is not running is a
// no-op (idempotent), so the REST endpoint can be called safely anytime.
func (m *StreamManager) Stop(ctx context.Context, id string) error {
	m.mu.Lock()
	eng := m.engs[id]
	m.mu.Unlock()
	if eng == nil {
		if _, err := m.Get(id); err != nil {
			return err
		}
		return nil
	}
	return eng.Stop(ctx)
}

// View returns the task plus its live status.
func (m *StreamManager) View(id string) (StreamView, error) {
	t, err := m.Get(id)
	if err != nil {
		return StreamView{}, err
	}
	m.mu.Lock()
	eng := m.engs[id]
	m.mu.Unlock()
	v := StreamView{StreamTask: *t}
	if eng != nil {
		st := eng.Status()
		v.Running = st.Running
		v.Pid = st.Pid
		v.SourcePath = st.SourcePath
		v.BitrateKbps = st.BitrateKbps
		v.FPS = st.FPS
		v.Frame = st.Frame
		v.Progress = st.Progress
		v.LastError = st.LastError
	}
	return v, nil
}

func (m *StreamManager) Views() []StreamView {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()
	out := make([]StreamView, 0, len(ids))
	for _, id := range ids {
		if v, err := m.View(id); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// handleStream serves the /stream REST endpoints for multi-stream tasks.
func (h *managementHandler) handleStream(w http.ResponseWriter, r *http.Request) {
	if h.streams == nil {
		h.writeError(w, http.StatusNotFound, "stream manager not enabled")
		return
	}
	id, action := resourcePath(r.URL.Path, "/stream")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"streams": h.streams.Views()})
	case r.Method == http.MethodGet && id != "" && action == "":
		v, err := h.streams.View(id)
		h.respondResult(w, v, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req struct {
			Name              string                `json:"name"`
			MediaID           string                `json:"mediaId"`
			FFmpegPath        string                `json:"ffmpegPath,omitempty"`
			ReconnectInterval int                   `json:"reconnectInterval,omitempty"`
			Outputs           []engine.OutputConfig `json:"outputs"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.streams.Create(req.Name, req.MediaID, req.FFmpegPath, req.ReconnectInterval, req.Outputs)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "replace":
		var req struct {
			ID                string                `json:"id"`
			Name              string                `json:"name"`
			MediaID           string                `json:"mediaId"`
			FFmpegPath        string                `json:"ffmpegPath,omitempty"`
			ReconnectInterval int                   `json:"reconnectInterval,omitempty"`
			Outputs           []engine.OutputConfig `json:"outputs"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "replace")
		}
		item, err := h.streams.Update(req.ID, req.Name, req.MediaID, req.FFmpegPath, req.ReconnectInterval, req.Outputs)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "start":
		err := h.streams.Start(r.Context(), routeTarget(id, action, "start"))
		h.writeOK(w, err)
	case r.Method == http.MethodPost && id != "" && action == "stop":
		err := h.streams.Stop(r.Context(), routeTarget(id, action, "stop"))
		h.writeOK(w, err)
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.streams.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}
