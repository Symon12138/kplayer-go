package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytelang/kplayer/engine"
	"github.com/bytelang/kplayer/management"
	outputprovider "github.com/bytelang/kplayer/module/output/provider"
	playprovider "github.com/bytelang/kplayer/module/play/provider"
	resourceprovider "github.com/bytelang/kplayer/module/resource/provider"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// Fake engine
// ---------------------------------------------------------------------------

// fakeEngine is an in-memory Engine implementation for the /engine REST and
// player-adapter tests: it records the calls the handler makes and serves
// scripted status and configuration values.
type fakeEngine struct {
	mu sync.Mutex

	cfg     engine.Config
	status  engine.Status
	started []string
	seeks   []float64
	stops   int
	updates []engine.Config

	startErr  error
	stopErr   error
	updateErr error
}

func (f *fakeEngine) Start(_ context.Context, source string) error {
	return f.StartAt(context.Background(), source, 0)
}

func (f *fakeEngine) StartAt(_ context.Context, source string, seekSeconds float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, source)
	f.seeks = append(f.seeks, seekSeconds)
	f.status.Running = true
	f.status.Paused = false
	f.status.SourcePath = source
	f.status.Pid = 4242
	return nil
}

func (f *fakeEngine) StartQueue(_ context.Context, items []engine.PlayItem, seekSeconds float64, loop bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	for _, it := range items {
		f.started = append(f.started, it.Path)
		f.seeks = append(f.seeks, seekSeconds)
	}
	f.status.Running = true
	f.status.Paused = false
	if len(items) > 0 {
		f.status.SourcePath = items[0].Path
	}
	f.status.Pid = 4242
	return nil
}

func (f *fakeEngine) Stop(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stops++
	f.status.Running = false
	return nil
}

func (f *fakeEngine) Restart(_ context.Context, source string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Running = true
	f.status.SourcePath = source
	f.status.Pid = 4243
	return nil
}

func (f *fakeEngine) Pause(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Paused = true
	f.status.Running = false
	return nil
}

func (f *fakeEngine) Continue(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Paused = false
	f.status.Running = true
	return nil
}

func (f *fakeEngine) Skip(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Paused = false
	f.status.Running = true
	return nil
}

func (f *fakeEngine) Status() engine.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeEngine) Apply(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status.Running && f.status.SourcePath != "" {
		f.status.Running = true
		f.status.SourcePath = f.status.SourcePath
		f.status.Pid = 4243
	}
	return nil
}

func (f *fakeEngine) UpdateConfig(cfg engine.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Validation mirrors the real engine so the handler's 400 mapping is
	// exercised end to end.
	cfg, err := engine.Validate(cfg)
	if err != nil {
		return err
	}
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, cfg)
	f.cfg = cfg
	return nil
}

func (f *fakeEngine) Config() engine.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

// ---------------------------------------------------------------------------
// Test helper
// ---------------------------------------------------------------------------

// newTestManagementHandlerWithEngine mirrors newTestManagementHandler but
// injects an engine into the player adapter, matching the production
// newManagementHandlerWithEngine wiring.
func newTestManagementHandlerWithEngine(t *testing.T, play playprovider.ProviderI, resource resourceprovider.ProviderI, output outputprovider.ProviderI, authOn bool, authToken string, eng engine.Engine) *managementHandler {
	t.Helper()
	store, err := management.OpenStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	adapter := &playerAdapter{play: play, resource: resource, store: store, engine: eng}
	outputAdapter := &outputFailoverAdapter{output: output}
	users := management.NewUserService(store)
	sessions := management.NewSessionService(store)
	h := &managementHandler{
		store:             store,
		media:             management.NewMediaService(store),
		playlists:         management.NewPlaylistService(store),
		tasks:             management.NewTaskService(store),
		alarms:            management.NewAlarmService(store),
		outputGroups:      management.NewOutputGroupService(store),
		failovers:         management.NewOutputFailoverService(store),
		healthPolicies:    management.NewHealthPolicyService(store),
		cacheTasks:        management.NewCacheTaskService(store),
		sceneTemplates:    management.NewSceneTemplateService(store),
		webhooks:          management.NewWebhookService(store),
		nodes:             management.NewNodeService(store),
		instances:         management.NewInstanceService(store),
		remoteCommands:    management.NewRemoteCommandService(store),
		configSnapshots:   management.NewConfigSnapshotService(store),
		configTemplates:   management.NewConfigTemplateService(store),
		industryTemplates: management.NewIndustryTemplateService(store),
		smartRules:        management.NewSmartRuleService(store),
		metrics:           management.NewMetricsService(store),
		suggestions:       management.NewSuggestionService(store),
		playEvents:        management.NewPlayEventService(store),
		audit:             management.NewAuditService(store),
		users:             users,
		sessions:          sessions,
		auth:              management.NewAuthService(store, users, sessions, managementSessionTTL),
		sessionTTL:        managementSessionTTL,
		player:            adapter,
		engine:            eng,
		authOn:            authOn,
		authToken:         authToken,
	}
	h.status = &providerStatus{play: play, resource: resource, output: output}
	h.scheduler = management.NewScheduler(store, adapter,
		management.WithTickInterval(20*time.Millisecond),
		management.WithPlayEventHandler(func(ev management.PlayEvent) {
			_, _ = h.playEvents.Record(ev)
		}))
	h.failoverMonitor = management.NewFailoverMonitor(store, outputAdapter, outputAdapter)
	h.webhookDispatcher = management.NewWebhookDispatcher(store)
	return h
}

// ---------------------------------------------------------------------------
// /engine REST endpoints
// ---------------------------------------------------------------------------

func TestEngineConfigGet(t *testing.T) {
	fake := &fakeEngine{cfg: engine.Config{
		FFmpegPath: "ffmpeg", ProbeInterval: 2 * time.Second,
		Outputs: []engine.OutputConfig{{URL: "rtmp://127.0.0.1:1935/live/test", Codec: "libx264"}},
	}}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	code, body := perform(t, h, http.MethodGet, "/engine/ffmpeg", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /engine/ffmpeg: status %d body %s", code, body)
	}
	if got := gjson.Get(body, "config.ffmpegPath").String(); got != "ffmpeg" {
		t.Fatalf("config.ffmpegPath = %q, want ffmpeg", got)
	}
	if got := gjson.Get(body, "config.outputs.0.url").String(); got != "rtmp://127.0.0.1:1935/live/test" {
		t.Fatalf("config.outputs.0.url = %q", got)
	}
}

func TestEngineConfigUpdate(t *testing.T) {
	fake := &fakeEngine{}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	code, body := perform(t, h, http.MethodPost, "/engine/ffmpeg", "", jsonBody(t, map[string]interface{}{
		"ffmpegPath": "ffmpeg",
		"outputs": []map[string]interface{}{
			{"url": "rtmp://127.0.0.1:1935/live/test", "width": 1920, "height": 1080, "bitrateKbps": 2500, "fps": 25},
		},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /engine/ffmpeg: status %d body %s", code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /engine/ffmpeg: ok missing in %s", body)
	}
	if got := gjson.Get(body, "config.outputs.0.bitrateKbps").Int(); got != 2500 {
		t.Fatalf("config.outputs.0.bitrateKbps = %d, want 2500", got)
	}

	fake.mu.Lock()
	if len(fake.updates) != 1 {
		fake.mu.Unlock()
		t.Fatalf("UpdateConfig calls = %d, want 1", len(fake.updates))
	}
	upd := fake.updates[0]
	fake.mu.Unlock()
	if upd.FFmpegPath == "" {
		t.Fatalf("updated FFmpegPath empty: the bare default should be upgraded to the detected path")
	}
	if upd.Outputs[0].Width != 1920 || upd.Outputs[0].Height != 1080 || upd.Outputs[0].FPS != 25 {
		t.Fatalf("updated output = %+v", upd.Outputs[0])
	}
	if upd.Outputs[0].Codec != "libx264" {
		t.Fatalf("updated Codec = %q, want default libx264", upd.Outputs[0].Codec)
	}
}

func TestEngineConfigUpdateInvalid(t *testing.T) {
	fake := &fakeEngine{}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	// No outputs: 400 with the ErrInvalid mapping.
	code, body := perform(t, h, http.MethodPost, "/engine/ffmpeg", "", jsonBody(t, map[string]interface{}{"outputs": []interface{}{}}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /engine/ffmpeg empty outputs: status %d body %s, want 400", code, body)
	}
	if !strings.Contains(body, "at least one output") {
		t.Fatalf("POST /engine/ffmpeg empty outputs: unexpected body %q", body)
	}
	// Empty URL: 400.
	code, body = perform(t, h, http.MethodPost, "/engine/ffmpeg", "", jsonBody(t, map[string]interface{}{
		"outputs": []map[string]interface{}{{"url": "  "}},
	}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /engine/ffmpeg empty URL: status %d body %s, want 400", code, body)
	}
	// Malformed JSON: 400.
	code, _ = perform(t, h, http.MethodPost, "/engine/ffmpeg", "", "{not json")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /engine/ffmpeg malformed JSON: status %d, want 400", code)
	}

	fake.mu.Lock()
	updates := len(fake.updates)
	fake.mu.Unlock()
	if updates != 0 {
		t.Fatalf("UpdateConfig calls = %d, want 0 after invalid requests", updates)
	}
}

func TestEngineStatusEndpoint(t *testing.T) {
	fake := &fakeEngine{status: engine.Status{
		Running: true, Pid: 4242, SourcePath: "/videos/a.mp4",
		OutputURLs:  []string{"rtmp://127.0.0.1:1935/live/test"},
		BitrateKbps: 2500, FPS: 25, Frame: 100, Progress: 42.5, Uptime: "1m23s",
	}}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	code, body := perform(t, h, http.MethodGet, "/engine/status", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /engine/status: status %d body %s", code, body)
	}
	if !gjson.Get(body, "status.running").Bool() {
		t.Fatalf("status.running = false in %s", body)
	}
	if got := gjson.Get(body, "status.pid").Int(); got != 4242 {
		t.Fatalf("status.pid = %d, want 4242", got)
	}
	if got := gjson.Get(body, "status.fps").Float(); got != 25 {
		t.Fatalf("status.fps = %v, want 25", got)
	}
	if got := gjson.Get(body, "status.progress").Float(); got != 42.5 {
		t.Fatalf("status.progress = %v, want 42.5", got)
	}
	if got := gjson.Get(body, "status.outputURLs.0").String(); got != "rtmp://127.0.0.1:1935/live/test" {
		t.Fatalf("status.outputURLs.0 = %q", got)
	}
}

func TestEngineEndpointsDisabledWithoutEngine(t *testing.T) {
	// A handler without an injected engine keeps the stub path: the
	// /engine routes report 404 instead of touching a nil engine.
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	code, _ := perform(t, h, http.MethodGet, "/engine/status", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /engine/status without engine: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodGet, "/engine/ffmpeg", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /engine/ffmpeg without engine: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/engine/ffmpeg", "", jsonBody(t, map[string]interface{}{
		"outputs": []map[string]interface{}{{"url": "rtmp://x/live"}},
	}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /engine/ffmpeg without engine: status %d, want 404", code)
	}
}

// ---------------------------------------------------------------------------
// Permission mapping
// ---------------------------------------------------------------------------

func TestEnginePermissionMapping(t *testing.T) {
	// The /engine prefix maps to the media resource: the engine config and
	// status drive the playback pipeline.
	if got := permissionResource("/engine/ffmpeg"); got != management.ResourceMedia {
		t.Fatalf("permissionResource(/engine/ffmpeg) = %q, want %q", got, management.ResourceMedia)
	}
	if got := permissionResource("/engine/status"); got != management.ResourceMedia {
		t.Fatalf("permissionResource(/engine/status) = %q, want %q", got, management.ResourceMedia)
	}
}

func TestEngineRolePermissions(t *testing.T) {
	fake := &fakeEngine{cfg: engine.Config{
		FFmpegPath: "ffmpeg", ProbeInterval: 2 * time.Second,
		Outputs: []engine.OutputConfig{{URL: "rtmp://127.0.0.1:1935/live/test"}},
	}}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)
	seedUser(t, h, "ops", "password123", management.RoleOperator, true)
	seedUser(t, h, "watch", "password123", management.RoleAuditor, true)
	operator := login(t, h, "ops", "password123")
	auditor := login(t, h, "watch", "password123")

	// The auditor can read engine state but not write the configuration.
	code, body := perform(t, h, http.MethodGet, "/engine/status", "Bearer "+auditor, "")
	if code != http.StatusOK {
		t.Fatalf("auditor GET /engine/status: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/engine/ffmpeg", "Bearer "+auditor, "")
	if code != http.StatusOK {
		t.Fatalf("auditor GET /engine/ffmpeg: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/engine/ffmpeg", "Bearer "+auditor, jsonBody(t, map[string]interface{}{
		"outputs": []map[string]interface{}{{"url": "rtmp://x/live"}},
	}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /engine/ffmpeg: status %d body %s, want 403", code, body)
	}
	if !strings.Contains(body, "permission denied") {
		t.Fatalf("auditor POST /engine/ffmpeg: unexpected body %q", body)
	}

	// The operator may write the engine configuration.
	code, body = perform(t, h, http.MethodPost, "/engine/ffmpeg", "Bearer "+operator, jsonBody(t, map[string]interface{}{
		"outputs": []map[string]interface{}{{"url": "rtmp://127.0.0.1:1935/live/ops", "bitrateKbps": 3000}},
	}))
	if code != http.StatusOK {
		t.Fatalf("operator POST /engine/ffmpeg: status %d body %s, want 200", code, body)
	}
}

// ---------------------------------------------------------------------------
// playerAdapter wiring with an engine
// ---------------------------------------------------------------------------

// TestNewManagementHandlerWithEngineWiring guards the production
// constructor: the injected engine must reach both the handler (for the
// /engine endpoints) and the player adapter (for playback). The constructor
// pins management.json in the working directory, so the test runs in a
// temp dir.
func TestNewManagementHandlerWithEngineWiring(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	fake := &fakeEngine{}
	h, err := newManagementHandlerWithEngine(&fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)
	if err != nil {
		t.Fatalf("newManagementHandlerWithEngine: %v", err)
	}
	if h.engine != fake {
		t.Fatalf("handler engine not injected")
	}
	if h.player.engine != fake {
		t.Fatalf("player adapter engine not injected")
	}
	// The /engine routes answer through the injected engine.
	code, body := perform(t, h, http.MethodGet, "/engine/status", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /engine/status: status %d body %s", code, body)
	}
}

func TestPlayerPlayWithEngineStartsFfmpeg(t *testing.T) {
	fake := &fakeEngine{}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	mediaID := addMedia(t, h)
	code, body := perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play: status %d body %s", code, body)
	}

	fake.mu.Lock()
	started := append([]string(nil), fake.started...)
	fake.mu.Unlock()
	// The media service stores the cleaned path (filepath.Clean), which
	// uses backslashes on Windows.
	if len(started) != 1 || started[0] != filepath.Clean("/videos/test.mp4") {
		t.Fatalf("engine.Start sources = %v, want [%s]", started, filepath.Clean("/videos/test.mp4"))
	}
	// The stub resource provider must not have been touched.
	if calls := h.player.resource.(*fakeResourceProvider).seekSnapshot(); len(calls) != 0 {
		t.Fatalf("stub ResourceSeek calls = %d, want 0 with engine", len(calls))
	}
}

func TestPlayerPlaylistWithEngineStartsFirstItem(t *testing.T) {
	fake := &fakeEngine{}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	m1 := addMedia(t, h)
	// A second media with a distinct path (addMedia hardcodes one path,
	// and the media service dedupes by path).
	code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{
		"path": "/videos/second.mp4", "name": "second",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /media second: status %d body %s", code, body)
	}
	m2 := gjson.Get(body, "id").String()
	if m2 == "" {
		t.Fatalf("POST /media second: no id in %s", body)
	}
	_, body = perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "programme", "items": []string{m1, m2},
	}))
	plID := gjson.Get(body, "id").String()
	if plID == "" {
		t.Fatalf("POST /playlist: no id in %s", body)
	}

	code, body = perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"playlistId": plID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play playlist: status %d body %s", code, body)
	}
	fake.mu.Lock()
	started := append([]string(nil), fake.started...)
	fake.mu.Unlock()
	// Batch 1 plays only the first item of the programme (cleaned path).
	if len(started) != 1 || started[0] != filepath.Clean("/videos/test.mp4") {
		t.Fatalf("engine.Start sources = %v, want [%s] (first playlist item)", started, filepath.Clean("/videos/test.mp4"))
	}
}

func TestPlayerPlayWithEngineStartError(t *testing.T) {
	fake := &fakeEngine{startErr: errors.New("engine: boom")}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	mediaID := addMedia(t, h)
	code, body := perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusInternalServerError {
		t.Fatalf("POST /player/play with failing engine: status %d body %s, want 500", code, body)
	}
	if !strings.Contains(body, "boom") {
		t.Fatalf("POST /player/play: unexpected body %q", body)
	}
}

func TestPlayerStopWithEngine(t *testing.T) {
	fake := &fakeEngine{}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	code, body := perform(t, h, http.MethodPost, "/player/stop", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /player/stop: status %d body %s", code, body)
	}
	fake.mu.Lock()
	stops := fake.stops
	fake.mu.Unlock()
	if stops != 1 {
		t.Fatalf("engine.Stop calls = %d, want 1", stops)
	}

	// A failing engine stop surfaces as a 500.
	fake.mu.Lock()
	fake.stopErr = errors.New("engine: stop boom")
	fake.mu.Unlock()
	code, body = perform(t, h, http.MethodPost, "/player/stop", "", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("POST /player/stop with failing engine: status %d body %s, want 500", code, body)
	}
}

// TestPlayerControlWithEngine drives the real engine semantics through the
// REST endpoints: Pause suspends (running false, paused true), Continue
// resumes (paused cleared), Skip restarts the queued item, and Stop halts
// the process.
func TestPlayerControlWithEngine(t *testing.T) {
	fake := &fakeEngine{}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	// Idle controls are safe no-ops; idle seek reports a precondition
	// conflict because there is no playback to jump.
	for _, action := range []string{"pause", "continue", "skip"} {
		code, body := perform(t, h, http.MethodPost, "/player/"+action, "", "")
		if code != http.StatusOK {
			t.Fatalf("POST /player/%s (idle): status %d body %s", action, code, body)
		}
	}
	code, body := perform(t, h, http.MethodPost, "/player/seek", "", jsonBody(t, map[string]interface{}{"seekSeconds": 5}))
	if code != http.StatusConflict {
		t.Fatalf("POST /player/seek (idle): status %d, want 409 (body %s)", code, body)
	}

	// Play a media item so the engine has a source and a queue.
	mediaID := addTestMedia(t, h, "/videos/a.mp4", "a")
	code, body = perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play: status %d body %s", code, body)
	}

	// Pause suspends the stream and marks it paused.
	code, body = perform(t, h, http.MethodPost, "/player/pause", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /player/pause: status %d body %s", code, body)
	}
	fake.mu.Lock()
	paused := fake.status.Paused
	stopped := fake.status.Running
	fake.mu.Unlock()
	if !paused || stopped {
		t.Fatalf("after pause: paused=%v running=%v, want paused=true running=false", paused, stopped)
	}

	// Continue resumes and clears the paused mark.
	code, body = perform(t, h, http.MethodPost, "/player/continue", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /player/continue: status %d body %s", code, body)
	}
	fake.mu.Lock()
	paused = fake.status.Paused
	running := fake.status.Running
	fake.mu.Unlock()
	if paused || !running {
		t.Fatalf("after continue: paused=%v running=%v, want paused=false running=true", paused, running)
	}

	// Skip restarts the current queue item from the beginning.
	code, body = perform(t, h, http.MethodPost, "/player/skip", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /player/skip: status %d body %s", code, body)
	}
	fake.mu.Lock()
	started := len(fake.started)
	fake.mu.Unlock()
	if started < 2 {
		t.Fatalf("engine starts after play+skip = %d, want >= 2", started)
	}

	// Seek with an active source restarts from the given offset.
	code, body = perform(t, h, http.MethodPost, "/player/seek", "", jsonBody(t, map[string]interface{}{"seekSeconds": 30}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/seek: status %d body %s", code, body)
	}
	fake.mu.Lock()
	last := fake.started[len(fake.started)-1]
	lastSeek := fake.seeks[len(fake.seeks)-1]
	fake.mu.Unlock()
	if last != "/videos/a.mp4" {
		t.Fatalf("seek restarted source %q, want /videos/a.mp4", last)
	}
	if lastSeek != 30 {
		t.Fatalf("seek offset = %v, want 30", lastSeek)
	}

	// Stop halts the engine.
	code, body = perform(t, h, http.MethodPost, "/player/stop", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /player/stop: status %d body %s", code, body)
	}
	fake.mu.Lock()
	running = fake.status.Running
	fake.mu.Unlock()
	if running {
		t.Fatalf("engine still running after stop")
	}
}

func addTestMedia(t *testing.T, h *managementHandler, path, name string) string {
	t.Helper()
	code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": path, "name": name}))
	if code != http.StatusOK {
		t.Fatalf("POST /media: status %d body %s", code, body)
	}
	id := gjson.Get(body, "id").String()
	if id == "" {
		t.Fatalf("POST /media: no id in %s", body)
	}
	return id
}

// TestEngineUpdateConfigJSONRoundTrip pins the wire representation of the
// engine configuration (ProbeInterval travels as nanoseconds, matching the
// Go time.Duration JSON encoding).
func TestEngineUpdateConfigJSONRoundTrip(t *testing.T) {
	fake := &fakeEngine{}
	h := newTestManagementHandlerWithEngine(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "", fake)

	code, body := perform(t, h, http.MethodPost, "/engine/ffmpeg", "", jsonBody(t, map[string]interface{}{
		"probeInterval": int64(3 * time.Second),
		"outputs":       []map[string]interface{}{{"url": "rtmp://x/live"}},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /engine/ffmpeg: status %d body %s", code, body)
	}
	if got := gjson.Get(body, "config.probeInterval").Int(); got != int64(3*time.Second) {
		t.Fatalf("config.probeInterval = %d, want %d", got, int64(3*time.Second))
	}
	// The response body must be re-decodable into engine.Config.
	var resp struct {
		Config engine.Config `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response not decodable into engine.Config: %v", err)
	}
	if resp.Config.ProbeInterval != 3*time.Second {
		t.Fatalf("decoded ProbeInterval = %v, want 3s", resp.Config.ProbeInterval)
	}
}
