package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytelang/kplayer/engine"
	"github.com/bytelang/kplayer/management"
	outputprovider "github.com/bytelang/kplayer/module/output/provider"
	playprovider "github.com/bytelang/kplayer/module/play/provider"
	resourceprovider "github.com/bytelang/kplayer/module/resource/provider"
	server "github.com/bytelang/kplayer/types/server"
	svrproto "github.com/bytelang/kplayer/types/server"
)

const managementDataFile = "management.json"

// managementSessionTTL is the lifetime of a console login session. Token
// expiry is enforced by the session service (an expired token is dropped
// on the next validation), so the handler only passes the TTL to the auth
// service at construction.
const managementSessionTTL = 24 * time.Hour

// managementHandler is the single-machine REST API used by the embedded
// operations console. It shares one local Store with the scheduler and the
// failover monitor.
type managementHandler struct {
	store             *management.Store
	media             *management.MediaService
	playlists         *management.PlaylistService
	tasks             *management.TaskService
	alarms            *management.AlarmService
	outputGroups      *management.OutputGroupService
	failovers         *management.OutputFailoverService
	healthPolicies    *management.HealthPolicyService
	cacheTasks        *management.CacheTaskService
	sceneTemplates    *management.SceneTemplateService
	webhooks          *management.WebhookService
	nodes             *management.NodeService
	instances         *management.InstanceService
	remoteCommands    *management.RemoteCommandService
	configSnapshots   *management.ConfigSnapshotService
	configTemplates   *management.ConfigTemplateService
	industryTemplates *management.IndustryTemplateService
	smartRules        *management.SmartRuleService
	metrics           *management.MetricsService
	suggestions       *management.SuggestionService
	playEvents        *management.PlayEventService
	audit             *management.AuditService
	users             *management.UserService
	sessions          *management.SessionService
	auth              *management.AuthService
	sessionTTL        time.Duration
	webhookDispatcher *management.WebhookDispatcher
	scheduler         *management.Scheduler
	failoverMonitor   *management.FailoverMonitor
	player            *playerAdapter
	engine            engine.Engine
	streams           *StreamManager
	effects           *EffectManager
	status            statusReader
	authOn            bool
	authToken         string
}

type statusReader interface {
	Status(context.Context) statusSnapshot
}

type statusSnapshot struct {
	Current     interface{}            `json:"current,omitempty"`
	Duration    interface{}            `json:"duration,omitempty"`
	Information interface{}            `json:"information,omitempty"`
	Resources   interface{}            `json:"resources,omitempty"`
	Outputs     interface{}            `json:"outputs,omitempty"`
	Scheduler   map[string]interface{} `json:"scheduler"`
	// Engine carries the ffmpeg engine status snapshot when an engine is
	// injected; without one the key is omitted so the legacy /status
	// contract is unchanged.
	Engine interface{} `json:"engine,omitempty"`
}

func newManagementHandler(play playprovider.ProviderI, resource resourceprovider.ProviderI, output outputprovider.ProviderI, authOn bool, authToken string) (*managementHandler, error) {
	return newManagementHandlerWithEngine(play, resource, output, authOn, authToken, nil, NewEffectManager(effectFile))
}

// newManagementHandlerWithEngine is newManagementHandler plus the ffmpeg
// playback engine: when eng is non-nil the player adapter drives real
// ffmpeg subprocesses instead of the stub core, and the /engine REST
// endpoints expose the engine configuration and status. A nil engine keeps
// the legacy stub playback path.
func newManagementHandlerWithEngine(play playprovider.ProviderI, resource resourceprovider.ProviderI, output outputprovider.ProviderI, authOn bool, authToken string, eng engine.Engine, effects *EffectManager) (*managementHandler, error) {
	store, err := management.OpenStore(managementDataFile)
	if err != nil {
		return nil, err
	}

	adapter := &playerAdapter{play: play, resource: resource, store: store, engine: eng}
	outputAdapter := &outputFailoverAdapter{output: output}
	users := management.NewUserService(store)
	sessions := management.NewSessionService(store)
	result := &managementHandler{
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
	result.streams = NewStreamManager(streamFile, func(ctx context.Context, mediaID string) (*management.Media, error) {
		return result.media.Get(mediaID)
	}, func(ctx context.Context, playlistID string) ([]*management.Media, error) {
		pl, err := result.playlists.Get(playlistID)
		if err != nil {
			return nil, err
		}
		out := make([]*management.Media, 0, len(pl.Items))
		for _, it := range pl.Items {
			m, err := result.media.Get(it.MediaID)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		return out, nil
	})
	result.effects = effects
	if eng != nil && result.effects != nil {
		result.streams.SetEffectSource(func() (string, string) {
			vf, af, err := result.effects.Render()
			if err != nil {
				return "", ""
			}
			return vf, af
		})
	}
	result.status = &providerStatus{play: play, resource: resource, output: output}
	// The webhook dispatcher delivers domain events to the subscribed
	// endpoints in the background. It is created before the scheduler and
	// failover error callbacks because those closures dispatch events
	// through it. Its internal failures (for example a store write error
	// while recording a delivery outcome) surface as alarms; per-delivery
	// HTTP failures are recorded as failed deliveries instead.
	dispatcher := management.NewWebhookDispatcher(store, management.WithWebhookErrorHandler(func(dispatchErr error) {
		_, _ = result.alarms.Raise(management.AlarmLevelWarning, "Webhook delivery failed", dispatchErr.Error())
	}))
	result.webhookDispatcher = dispatcher
	result.scheduler = management.NewScheduler(store, adapter, management.WithErrorHandler(func(scheduleErr error) {
		_, _ = result.alarms.Raise(management.AlarmLevelWarning, "Scheduled playback failed", scheduleErr.Error())
		// A scheduled playback failure is a material failure: notify
		// webhook subscribers asynchronously, with the error as payload.
		dispatcher.Dispatch(context.Background(), management.EventMaterialFailed, map[string]interface{}{"error": scheduleErr.Error()})
	}), management.WithPlayEventHandler(func(ev management.PlayEvent) {
		// Bridge scheduler play events into the playback log backing the
		// metrics and recommendation endpoints. A failed Record (store
		// write error) is ignored so the callback stays fast and safe for
		// concurrent use; the closure captures result through this handler.
		_, _ = result.playEvents.Record(ev)
	}))
	// The failover monitor shares the store and drives enabled failovers
	// through the same output adapter; its errors surface as alarms.
	result.failoverMonitor = management.NewFailoverMonitor(store, outputAdapter, outputAdapter, management.WithMonitorErrorHandler(func(monitorErr error) {
		_, _ = result.alarms.Raise(management.AlarmLevelWarning, "Output failover failed", monitorErr.Error())
		// A monitor error means an output went offline: notify webhook
		// subscribers asynchronously, with the error as payload.
		dispatcher.Dispatch(context.Background(), management.EventOutputDisconnected, map[string]interface{}{"error": monitorErr.Error()})
	}))
	// The engine exit callback bridges abnormal ffmpeg exits into the
	// operations plane: an alarm plus a webhook notification. The callback
	// runs on the engine's exit watcher goroutine and must stay non-blocking
	// (fire-and-forget per the engine contract), so Raise and Dispatch are
	// invoked directly and Dispatch delivers in its own goroutine.
	if fe, ok := eng.(*engine.FFmpegEngine); ok {
		fe.OnExit = func(exitCode int, exitErr error) {
			// A clean stop (user stop or the stream ending) is not an
			// incident: no alarm, no dispatch.
			if exitCode == 0 {
				return
			}
			errMsg := ""
			if exitErr != nil {
				errMsg = exitErr.Error()
			}
			_, _ = result.alarms.Raise(management.AlarmLevelWarning, "Stream engine exited", fmt.Sprintf("exit code %d: %s", exitCode, errMsg))
			// An abnormal engine exit interrupts the stream: notify webhook
			// subscribers asynchronously, with the exit details as payload.
			dispatcher.Dispatch(context.Background(), management.EventEngineExited, map[string]interface{}{"exitCode": exitCode, "error": errMsg})
		}
	}
	return result, nil
}

func (h *managementHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /auth/login and /auth/logout are exempt from the authentication gate:
	// login is the credential endpoint itself, and logout revokes the
	// credential it carries (body token or Authorization header), so both
	// must work without an existing session. Every other route is
	// authenticated first — legacy fixed token or session bearer — and then
	// checked against the permission matrix; see authenticate and
	// requirePermission.
	if !authEndpoint(r) {
		role, ok := h.authenticate(w, r)
		if !ok {
			return
		}
		if !h.requirePermission(w, r, role) {
			return
		}
	}

	switch {
	case r.URL.Path == "/status":
		h.handleStatus(w, r)
	case r.URL.Path == "/auth/login" && r.Method == http.MethodPost:
		h.handleAuthLogin(w, r)
	case r.URL.Path == "/auth/logout" && r.Method == http.MethodPost:
		h.handleAuthLogout(w, r)
	case r.URL.Path == "/auth/me" && r.Method == http.MethodGet:
		h.handleAuthMe(w, r)
	case r.URL.Path == "/user" || strings.HasPrefix(r.URL.Path, "/user/"):
		h.handleUser(w, r)
	case r.URL.Path == "/media" || strings.HasPrefix(r.URL.Path, "/media/"):
		h.handleMedia(w, r)
	case r.URL.Path == "/playlist" || strings.HasPrefix(r.URL.Path, "/playlist/"):
		h.handlePlaylist(w, r)
	case r.URL.Path == "/task" || strings.HasPrefix(r.URL.Path, "/task/"):
		h.handleTask(w, r)
	case r.URL.Path == "/alarm" || strings.HasPrefix(r.URL.Path, "/alarm/"):
		h.handleAlarm(w, r)
	case r.URL.Path == "/scheduler" || strings.HasPrefix(r.URL.Path, "/scheduler/"):
		h.handleScheduler(w, r)
	case r.URL.Path == "/player" || strings.HasPrefix(r.URL.Path, "/player/"):
		h.handlePlayer(w, r)
	case r.URL.Path == "/output-group" || strings.HasPrefix(r.URL.Path, "/output-group/"):
		h.handleOutputGroup(w, r)
	case r.URL.Path == "/failover" || strings.HasPrefix(r.URL.Path, "/failover/"):
		h.handleFailover(w, r)
	case r.URL.Path == "/health-policy" || strings.HasPrefix(r.URL.Path, "/health-policy/"):
		h.handleHealthPolicy(w, r)
	case r.URL.Path == "/cache-task" || strings.HasPrefix(r.URL.Path, "/cache-task/"):
		h.handleCacheTask(w, r)
	case r.URL.Path == "/scene-template" || strings.HasPrefix(r.URL.Path, "/scene-template/"):
		h.handleSceneTemplate(w, r)
	case r.URL.Path == "/webhook" || strings.HasPrefix(r.URL.Path, "/webhook/"):
		h.handleWebhook(w, r)
	case r.URL.Path == "/audit" || strings.HasPrefix(r.URL.Path, "/audit/"):
		h.handleAudit(w, r)
	case r.URL.Path == "/node" || strings.HasPrefix(r.URL.Path, "/node/"):
		h.handleNode(w, r)
	case r.URL.Path == "/instance" || strings.HasPrefix(r.URL.Path, "/instance/"):
		h.handleInstance(w, r)
	case r.URL.Path == "/remote-command" || strings.HasPrefix(r.URL.Path, "/remote-command/"):
		h.handleRemoteCommand(w, r)
	case r.URL.Path == "/config-snapshot" || strings.HasPrefix(r.URL.Path, "/config-snapshot/"):
		h.handleConfigSnapshot(w, r)
	case r.URL.Path == "/config-template" || strings.HasPrefix(r.URL.Path, "/config-template/"):
		h.handleConfigTemplate(w, r)
	case r.URL.Path == "/industry-template" || strings.HasPrefix(r.URL.Path, "/industry-template/"):
		h.handleIndustryTemplate(w, r)
	case r.URL.Path == "/smart-rule" || strings.HasPrefix(r.URL.Path, "/smart-rule/"):
		h.handleSmartRule(w, r)
	case r.URL.Path == "/metrics" || strings.HasPrefix(r.URL.Path, "/metrics/"):
		h.handleMetrics(w, r)
	case r.URL.Path == "/engine" || strings.HasPrefix(r.URL.Path, "/engine/"):
		h.handleEngine(w, r)
	case r.URL.Path == "/stream" || strings.HasPrefix(r.URL.Path, "/stream/"):
		h.handleStream(w, r)
	case r.URL.Path == "/effects" || strings.HasPrefix(r.URL.Path, "/effects/"):
		h.handleEffects(w, r)
	case r.URL.Path == "/suggestion" || strings.HasPrefix(r.URL.Path, "/suggestion/"):
		h.handleSuggestion(w, r)
	default:
		http.NotFound(w, r)
	}
}

// authEndpoint reports whether the request targets one of the endpoints
// that must not require authentication: POST /auth/login (the credential
// endpoint) and POST /auth/logout (which revokes the credential it
// carries, in the body or the Authorization header). /auth/me requires a
// session by definition and stays behind the gate.
func authEndpoint(r *http.Request) bool {
	switch {
	case r.URL.Path == "/auth/login" && r.Method == http.MethodPost:
		return true
	case r.URL.Path == "/auth/logout" && r.Method == http.MethodPost:
		return true
	}
	return false
}

// authenticate resolves the caller's role for one request, writing a 401
// and returning false when the request is not authenticated. It replaces
// the legacy authAllowed check and reproduces its fixed-token semantics:
//
//  1. With auth enabled, a request carrying the configured token verbatim
//     passes and is treated as admin: the pre-session behaviour, kept for
//     existing clients.
//  2. A session bearer token (Authorization: Bearer <token>) is validated
//     against the session store, and the caller's role comes from the
//     session's user. A session whose user has been deleted since login is
//     rejected: its principal no longer exists, so default-deny applies.
//  3. With auth enabled anything else is rejected (fail closed; an empty
//     configured token never authorises anyone, as before).
//
// With auth disabled every request passes, preserving the legacy
// pass-through: a missing or invalid token is treated as admin (the
// single-machine default), while a valid bearer token still narrows the
// role so session callers get permission checks.
func (h *managementHandler) authenticate(w http.ResponseWriter, r *http.Request) (management.UserRole, bool) {
	header := r.Header.Get(server.AUTHORIZATION_METADATA_KEY)
	if h.authOn && header != "" && header == h.authToken {
		return management.RoleAdmin, true
	}
	if token, ok := bearerToken(header); ok {
		session, err := h.auth.Authenticate(token)
		if err == nil {
			user, userErr := h.users.Get(session.UserID)
			if userErr == nil {
				return user.Role, true
			}
		}
		if h.authOn {
			h.writeError(w, http.StatusUnauthorized, "authentication failed")
			return "", false
		}
		// Auth disabled: an invalid bearer is ignored like any other header
		// value and the request passes as admin.
		return management.RoleAdmin, true
	}
	if h.authOn {
		h.writeError(w, http.StatusUnauthorized, "authentication failed")
		return "", false
	}
	return management.RoleAdmin, true
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header value. A value without the bearer scheme, or with an empty token,
// reports ok=false.
func bearerToken(header string) (string, bool) {
	const scheme = "Bearer "
	if !strings.HasPrefix(header, scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}

// permissionResource maps a management path prefix to the resource that
// guards it in the permission matrix. Resources without a dedicated matrix
// entry map to the closest one (documented per mapping). Paths with no
// mapping — the /auth endpoints, which are gated by authentication itself —
// are exempt from the permission check.
func permissionResource(p string) string {
	switch {
	case p == "/user" || strings.HasPrefix(p, "/user/"):
		return management.ResourceUser
	case p == "/media" || strings.HasPrefix(p, "/media/"):
		return management.ResourceMedia
	case p == "/playlist" || strings.HasPrefix(p, "/playlist/"):
		return management.ResourcePlaylist
	case p == "/task" || strings.HasPrefix(p, "/task/"):
		return management.ResourceTask
	case p == "/alarm" || strings.HasPrefix(p, "/alarm/"):
		// Alarms surface scheduling failures, so they sit under the task
		// resource in the permission matrix.
		return management.ResourceTask
	case p == "/scheduler" || strings.HasPrefix(p, "/scheduler/"):
		// The scheduler lifecycle controls task execution.
		return management.ResourceTask
	case p == "/player" || strings.HasPrefix(p, "/player/"):
		// Playback control drives the media pipeline.
		return management.ResourceMedia
	case p == "/output-group" || strings.HasPrefix(p, "/output-group/"):
		return management.ResourceOutput
	case p == "/failover" || strings.HasPrefix(p, "/failover/"):
		return management.ResourceOutput
	case p == "/health-policy" || strings.HasPrefix(p, "/health-policy/"):
		// Health policies govern output health checks.
		return management.ResourceOutput
	case p == "/cache-task" || strings.HasPrefix(p, "/cache-task/"):
		// Cache tasks prime media for playback.
		return management.ResourceMedia
	case p == "/scene-template" || strings.HasPrefix(p, "/scene-template/"):
		return management.ResourceScene
	case p == "/webhook" || strings.HasPrefix(p, "/webhook/"):
		return management.ResourceWebhook
	case p == "/node" || strings.HasPrefix(p, "/node/"):
		// The node and instance registries describe the playback hosts the
		// outputs run on: operations-side control, like the output groups.
		return management.ResourceOutput
	case p == "/instance" || strings.HasPrefix(p, "/instance/"):
		return management.ResourceOutput
	case p == "/remote-command" || strings.HasPrefix(p, "/remote-command/"):
		// Remote commands drive the playback processes on managed nodes.
		return management.ResourceOutput
	case p == "/config-snapshot" || strings.HasPrefix(p, "/config-snapshot/"):
		// Configuration snapshots and templates manage the configuration
		// the scheduled tasks run against.
		return management.ResourceTask
	case p == "/config-template" || strings.HasPrefix(p, "/config-template/"):
		return management.ResourceTask
	case p == "/industry-template" || strings.HasPrefix(p, "/industry-template/"):
		// Industry templates, smart rules and suggestions orchestrate
		// schedules and scenes.
		return management.ResourceScene
	case p == "/smart-rule" || strings.HasPrefix(p, "/smart-rule/"):
		return management.ResourceScene
	case p == "/suggestion" || strings.HasPrefix(p, "/suggestion/"):
		return management.ResourceScene
	case p == "/metrics" || strings.HasPrefix(p, "/metrics/"):
		// Metrics are read-only aggregates over playback events.
		return management.ResourceAudit
	case p == "/engine" || strings.HasPrefix(p, "/engine/"):
		// The engine configuration and status drive the playback pipeline.
		return management.ResourceMedia
	case p == "/audit" || strings.HasPrefix(p, "/audit/"):
		return management.ResourceAudit
	}
	return ""
}

// requestAction maps the HTTP method to a matrix action: GET reads,
// everything else writes. Every write route of the management API is a
// POST, PUT or DELETE, so the mapping is exact. Three POST routes are
// read-mapped despite their method: POST /config-template/{id}/expand,
// POST /smart-rule/{id}/generate and POST /suggestion/recommend only
// compute over the store (template expansion, playlist preview and media
// recommendation persist nothing), so they stay available to read-only
// roles like GET. POST /smart-rule/{id}/generate-and-apply is deliberately
// not included: it persists the generated playlist via ApplyGenerated and
// remains a write.
func requestAction(r *http.Request) string {
	switch {
	case r.Method == http.MethodGet:
		return management.ActionRead
	case strings.HasSuffix(r.URL.Path, "/expand"),
		strings.HasSuffix(r.URL.Path, "/generate"),
		r.URL.Path == "/suggestion/recommend":
		return management.ActionRead
	}
	return management.ActionWrite
}

// requirePermission enforces the permission matrix for one request, writing
// a 403 and returning false when the caller's role is denied. The check
// runs once in ServeHTTP for every route, so the individual handlers need
// no per-route permission logic.
func (h *managementHandler) requirePermission(w http.ResponseWriter, r *http.Request, role management.UserRole) bool {
	resource := permissionResource(r.URL.Path)
	if resource == "" || management.CanAccess(role, resource, requestAction(r)) {
		return true
	}
	h.writeError(w, http.StatusForbidden, "permission denied")
	return false
}

func (h *managementHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	snapshot := statusSnapshot{}
	if h.status != nil {
		snapshot = h.status.Status(r.Context())
	}
	snapshot.Scheduler = map[string]interface{}{"running": h.scheduler.Running()}
	if h.engine != nil {
		snapshot.Engine = h.engine.Status()
	}
	h.writeJSON(w, http.StatusOK, snapshot)
}

func (h *managementHandler) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	session, err := h.auth.Login(req.Username, req.Password)
	if err != nil {
		if errors.Is(err, management.ErrInvalidCredentials) {
			// One message for every failure mode (unknown user, disabled
			// user, wrong password), so the API does not leak which users
			// exist (no username enumeration).
			h.writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		h.writeManagementError(w, err)
		return
	}
	user, err := h.users.Get(session.UserID)
	if err != nil {
		// The user was deleted between Login and this lookup: the session
		// exists but its principal is gone, so the login cannot complete.
		h.writeError(w, http.StatusInternalServerError, "session user missing")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":     session.ID,
		"username":  session.Username,
		"role":      user.Role,
		"expiresAt": session.ExpiresAt,
	})
}

func (h *managementHandler) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	// The body is optional: logout may carry the token in the Authorization
	// header instead, so an empty body is not a decode error.
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		h.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		token, _ = bearerToken(r.Header.Get(server.AUTHORIZATION_METADATA_KEY))
	}
	if token == "" {
		h.writeError(w, http.StatusBadRequest, "logout token is required")
		return
	}
	h.writeOK(w, h.auth.Logout(token))
}

func (h *managementHandler) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r.Header.Get(server.AUTHORIZATION_METADATA_KEY))
	if !ok {
		if !h.authOn {
			// Auth disabled (the default for a private single-machine
			// console): every request passes as admin, so report the
			// anonymous admin principal and let the frontend enter
			// directly instead of forcing a login screen.
			h.writeJSON(w, http.StatusOK, map[string]interface{}{
				"username":     "admin",
				"role":         management.RoleAdmin,
				"authRequired": false,
			})
			return
		}
		h.writeError(w, http.StatusUnauthorized, "authentication failed")
		return
	}
	session, err := h.auth.Authenticate(token)
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, "authentication failed")
		return
	}
	user, err := h.users.Get(session.UserID)
	if err != nil {
		// The session's user was deleted: the session has no valid
		// principal, so it is reported as unauthenticated.
		h.writeError(w, http.StatusUnauthorized, "authentication failed")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"username":  session.Username,
		"role":      user.Role,
		"expiresAt": session.ExpiresAt,
	})
}

// userView is the user representation exposed by the REST API. The stored
// user carries a password hash; it is never serialised.
type userView struct {
	ID        string              `json:"id"`
	Username  string              `json:"username"`
	Role      management.UserRole `json:"role"`
	Enabled   bool                `json:"enabled"`
	CreatedAt time.Time           `json:"createdAt"`
	UpdatedAt time.Time           `json:"updatedAt"`
}

func toUserView(u *management.User) userView {
	return userView{ID: u.ID, Username: u.Username, Role: u.Role, Enabled: u.Enabled, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
}

type userRequest struct {
	ID       string              `json:"id"`
	Username string              `json:"username"`
	Password string              `json:"password"`
	Role     management.UserRole `json:"role"`
	Enabled  bool                `json:"enabled"`
}

func (h *managementHandler) handleUser(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/user")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		users := make([]userView, 0, len(h.users.List()))
		for _, u := range h.users.List() {
			users = append(users, toUserView(u))
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"users": users})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.users.Get(id)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, toUserView(item))
	case r.Method == http.MethodPost && id == "":
		var req userRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.users.Create(management.UserSpec{Username: req.Username, Password: req.Password, Role: req.Role, Enabled: req.Enabled})
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, toUserView(item))
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req userRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.users.Update(req.ID, management.UserSpec{Username: req.Username, Role: req.Role, Enabled: req.Enabled})
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, toUserView(item))
	case r.Method == http.MethodPost && id != "" && action == "password":
		var req struct {
			Password string `json:"password"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		h.writeOK(w, h.users.SetPassword(id, req.Password))
	case r.Method == http.MethodPost && id != "" && action == "enabled":
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		h.writeOK(w, h.users.SetEnabled(id, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.users.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handleMedia(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/media")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"media": h.media.ListSorted()})
	case r.Method == http.MethodGet && action == "" && id != "" && id != "list":
		item, err := h.media.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "scan"):
		var req struct {
			Root           string   `json:"root"`
			Extensions     []string `json:"extensions"`
			IncludeSubdirs *bool    `json:"includeSubdirs"`
			Probe          bool     `json:"probe"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Root) == "" {
			h.writeError(w, http.StatusBadRequest, "scan root is required")
			return
		}
		recursive := true
		if req.IncludeSubdirs != nil {
			recursive = *req.IncludeSubdirs
		}
		result, err := h.media.Scan(r.Context(), req.Root, management.ScanOptions{
			Extensions:     req.Extensions,
			IncludeSubdirs: recursive,
			Probe:          req.Probe,
		})
		h.respondResult(w, result, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req struct {
			Path         string   `json:"path"`
			Name         string   `json:"name"`
			Tags         []string `json:"tags"`
			SortBy       string   `json:"sortBy"`
			AudioPath    string   `json:"audioPath"`
			SubtitlePath string   `json:"subtitlePath"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		item := &management.Media{
			Path:         req.Path,
			Name:         req.Name,
			Tags:         req.Tags,
			SortBy:       management.MediaSortOrder(req.SortBy),
			AudioPath:    req.AudioPath,
			SubtitlePath: req.SubtitlePath,
		}
		err := h.media.Add(item)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && isRoute(id, action, "update"):
		var req struct {
			Name         string `json:"name"`
			SortBy       string `json:"sortBy"`
			AudioPath    string `json:"audioPath"`
			SubtitlePath string `json:"subtitlePath"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		target := routeTarget(id, action, "update")
		err := h.media.Update(target, func(m *management.Media) error {
			if req.Name != "" {
				m.Name = req.Name
			}
			if req.SortBy != "" {
				m.SortBy = management.MediaSortOrder(req.SortBy)
			}
			m.AudioPath = req.AudioPath
			m.SubtitlePath = req.SubtitlePath
			return validateMediaLite(m)
		})
		item, getErr := h.media.Get(target)
		if err == nil {
			err = getErr
		}
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		target := routeTarget(id, action, "remove")
		h.writeOK(w, h.media.Delete(target))
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/playlist")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"playlists": h.playlists.ListSorted()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.playlists.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req playlistRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.playlists.Create(req.Name, req.Desc, req.Items, req.Loop)
		if err == nil && req.Mode != "" {
			switch management.PlayMode(req.Mode) {
			case management.PlayModeOrder, management.PlayModeLoop, management.PlayModeRandom, management.PlayModeRandomLoop:
				err = h.playlists.Update(item.ID, func(p *management.Playlist) error {
					p.Mode = management.PlayMode(req.Mode)
					return nil
				})
			default:
				err = fmt.Errorf("playlist: %w: invalid mode %q", management.ErrInvalid, req.Mode)
			}
		}
		if err == nil && req.FallbackPlaylistID != "" {
			// PlaylistService.Create takes no fallback reference, so it is
			// attached in a second write. A failure (bad reference) is
			// reported while the playlist itself already exists.
			err = h.playlists.SetFallback(item.ID, req.FallbackPlaylistID)
		}
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "items":
		var req struct {
			MediaID string `json:"mediaId"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		err := h.playlists.AddItem(id, req.MediaID)
		item, getErr := h.playlists.Get(id)
		if err == nil {
			err = getErr
		}
		h.respondResult(w, item, err)
	case (r.Method == http.MethodPut && id != "" && action == "") || (r.Method == http.MethodPost && isRoute(id, action, "update")):
		var req playlistRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID != "" {
			id = req.ID
		}
		var itemIDs []*management.PlaylistItem
		if req.Items != nil {
			var err error
			itemIDs, err = h.resolvePlaylistItems(req.Items)
			if err != nil {
				h.writeManagementError(w, err)
				return
			}
		}
		err := h.playlists.Update(id, func(item *management.Playlist) error {
			if req.Name != "" {
				item.Name = req.Name
			}
			item.Desc = req.Desc
			item.Loop = req.Loop
			if req.Mode != "" {
				switch management.PlayMode(req.Mode) {
				case management.PlayModeOrder, management.PlayModeLoop, management.PlayModeRandom, management.PlayModeRandomLoop:
					item.Mode = management.PlayMode(req.Mode)
				default:
					return fmt.Errorf("playlist: %w: invalid mode %q", management.ErrInvalid, req.Mode)
				}
			}
			if req.Items != nil {
				item.Items = itemIDs
			}
			return nil
		})
		// The fallback reference is replaced unconditionally: this route has
		// replace semantics, so omitting fallbackPlaylistId clears it.
		// SetFallback is an independent write transaction, so a failure here
		// (for example a missing reference) is reported even though the
		// update itself already took effect.
		if err == nil {
			err = h.playlists.SetFallback(id, req.FallbackPlaylistID)
		}
		item, getErr := h.playlists.Get(id)
		if err == nil {
			err = getErr
		}
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && action == "items":
		mediaID := path.Base(r.URL.Path)
		err := h.playlists.RemoveItem(id, mediaID)
		item, getErr := h.playlists.Get(id)
		if err == nil {
			err = getErr
		}
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.playlists.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type playlistRequest struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Desc               string   `json:"desc"`
	Items              []string `json:"items"`
	Mode               string   `json:"mode"`
	Loop               bool     `json:"loop"`
	FallbackPlaylistID string   `json:"fallbackPlaylistId,omitempty"`
}

func (h *managementHandler) resolvePlaylistItems(ids []string) ([]*management.PlaylistItem, error) {
	snapshot, err := h.store.Snapshot()
	if err != nil {
		return nil, err
	}
	items := make([]*management.PlaylistItem, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := management.ResolveMedia(snapshot, id); err != nil {
			return nil, err
		}
		items = append(items, &management.PlaylistItem{MediaID: id})
	}
	return items, nil
}

func (h *managementHandler) handleTask(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/task")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"tasks": h.tasks.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.tasks.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req taskRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.tasks.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "replace"):
		var req taskRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "replace")
		}
		item, err := h.tasks.Replace(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.tasks.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodPost && isRoute(id, action, "run"):
		target := routeTarget(id, action, "run")
		item, err := h.tasks.Get(target)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		// 立即执行一次：开播动作播放节目单（从头播放最新内容），关播动作停止推流。
		switch item.Action {
		case management.TaskActionStop:
			err = h.player.Stop(r.Context())
		default:
			req := management.PlayRequest{PlaylistID: item.PlaylistID, MediaID: item.MediaID}
			err = h.player.Play(r.Context(), req)
		}
		h.writeOK(w, err)
	case r.Method == http.MethodPost && isRoute(id, action, "remove"):
		var req struct {
			ID string `json:"id"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "remove")
		}
		h.writeOK(w, h.tasks.Delete(req.ID))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.tasks.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type taskRequest struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Type              management.TaskType `json:"type"`
	Interval          int                 `json:"interval"`
	Cron              string              `json:"cron"`
	Action            management.TaskAction `json:"action,omitempty"`
	PlaylistID        string              `json:"playlistId"`
	MediaID           string              `json:"mediaId"`
	SceneTemplateID   string              `json:"sceneTemplateId,omitempty"`
	Loop              bool                `json:"loop"`
	Priority          management.Priority `json:"priority,omitempty"`
	Interrupt         bool                `json:"interrupt,omitempty"`
	InterruptDuration int                 `json:"interruptDuration,omitempty"`
	Enabled           bool                `json:"enabled"`
}

func (r taskRequest) spec() management.TaskSpec {
	return management.TaskSpec{Name: r.Name, Type: r.Type, Interval: r.Interval, Cron: r.Cron, Action: r.Action, PlaylistID: r.PlaylistID, MediaID: r.MediaID, SceneTemplateID: r.SceneTemplateID, Loop: r.Loop, Priority: r.Priority, Interrupt: r.Interrupt, InterruptDuration: r.InterruptDuration, Enabled: r.Enabled}
}

func (h *managementHandler) handleOutputGroup(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/output-group")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"groups": h.outputGroups.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.outputGroups.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req outputGroupRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.outputGroups.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req outputGroupRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.outputGroups.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.outputGroups.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodPost && id != "" && action == "outputs":
		var req struct {
			Unique string `json:"unique"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		err := h.outputGroups.AddOutput(id, req.Unique)
		item, getErr := h.outputGroups.Get(id)
		if err == nil {
			err = getErr
		}
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && action == "outputs":
		unique := path.Base(r.URL.Path)
		err := h.outputGroups.RemoveOutput(id, unique)
		item, getErr := h.outputGroups.Get(id)
		if err == nil {
			err = getErr
		}
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.outputGroups.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type outputGroupRequest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Platform    string   `json:"platform"`
	Region      string   `json:"region"`
	Business    string   `json:"business"`
	Outputs     []string `json:"outputs"`
	Enabled     bool     `json:"enabled"`
}

func (r outputGroupRequest) spec() management.OutputGroupSpec {
	return management.OutputGroupSpec{Name: r.Name, Description: r.Description, Platform: r.Platform, Region: r.Region, Business: r.Business, Outputs: r.Outputs, Enabled: r.Enabled}
}

func (h *managementHandler) handleFailover(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/failover")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"failovers": h.failovers.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.failovers.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req failoverRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.failovers.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req failoverRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.failovers.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.failovers.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.failovers.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type failoverRequest struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	PrimaryUnique    string                    `json:"primaryUnique"`
	BackupUnique     string                    `json:"backupUnique"`
	Policy           management.FailoverPolicy `json:"policy"`
	ThresholdSeconds int                       `json:"thresholdSeconds"`
	Enabled          bool                      `json:"enabled"`
}

func (r failoverRequest) spec() management.OutputFailoverSpec {
	return management.OutputFailoverSpec{Name: r.Name, PrimaryUnique: r.PrimaryUnique, BackupUnique: r.BackupUnique, Policy: r.Policy, ThresholdSeconds: r.ThresholdSeconds, Enabled: r.Enabled}
}

func (h *managementHandler) handleHealthPolicy(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/health-policy")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"healthPolicies": h.healthPolicies.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.healthPolicies.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req healthPolicyRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.healthPolicies.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req healthPolicyRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.healthPolicies.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.healthPolicies.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.healthPolicies.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type healthPolicyRequest struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	MaxRetries         int    `json:"maxRetries"`
	RetryWindowSeconds int    `json:"retryWindowSeconds"`
	AutoSkipOnFailure  bool   `json:"autoSkipOnFailure"`
	Enabled            bool   `json:"enabled"`
}

func (r healthPolicyRequest) spec() management.HealthPolicySpec {
	return management.HealthPolicySpec{Name: r.Name, MaxRetries: r.MaxRetries, RetryWindowSeconds: r.RetryWindowSeconds, AutoSkipOnFailure: r.AutoSkipOnFailure, Enabled: r.Enabled}
}

func (h *managementHandler) handleCacheTask(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/cache-task")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"cacheTasks": h.cacheTasks.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.cacheTasks.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req cacheTaskRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.cacheTasks.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req cacheTaskRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.cacheTasks.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "running"):
		item, err := h.cacheTasks.MarkRunning(routeTarget(id, action, "running"))
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "done"):
		item, err := h.cacheTasks.MarkDone(routeTarget(id, action, "done"))
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "failed"):
		var req struct {
			ID   string `json:"id"`
			Note string `json:"note"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "failed")
		}
		item, err := h.cacheTasks.MarkFailed(req.ID, req.Note)
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.cacheTasks.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type cacheTaskRequest struct {
	ID      string                 `json:"id"`
	MediaID string                 `json:"mediaId"`
	Note    string                 `json:"note"`
	Status  management.CacheStatus `json:"status"`
}

func (r cacheTaskRequest) spec() management.CacheTaskSpec {
	return management.CacheTaskSpec{MediaID: r.MediaID, Note: r.Note, Status: r.Status}
}

func (h *managementHandler) handleSceneTemplate(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/scene-template")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"sceneTemplates": h.sceneTemplates.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.sceneTemplates.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req sceneTemplateRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.sceneTemplates.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req sceneTemplateRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.sceneTemplates.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.sceneTemplates.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodPost && isRoute(id, action, "duplicate"):
		item, err := h.sceneTemplates.Duplicate(routeTarget(id, action, "duplicate"))
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.sceneTemplates.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type sceneTemplateRequest struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Kind    management.SceneKind `json:"kind"`
	Params  map[string]string    `json:"params"`
	Enabled bool                 `json:"enabled"`
}

func (r sceneTemplateRequest) spec() management.SceneTemplateSpec {
	return management.SceneTemplateSpec{Name: r.Name, Kind: r.Kind, Params: r.Params, Enabled: r.Enabled}
}

func (h *managementHandler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/webhook")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"webhooks": h.webhooks.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.webhooks.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodGet && id != "" && action == "deliveries":
		// WebhookService keeps delivery records only inside the store
		// document, so history is read from a snapshot and filtered here.
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"deliveries": h.listDeliveries(id)})
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req webhookRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.webhooks.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req webhookRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.webhooks.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.webhooks.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.webhooks.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type webhookRequest struct {
	ID      string                    `json:"id"`
	Name    string                    `json:"name"`
	URL     string                    `json:"url"`
	Events  []management.WebhookEvent `json:"events"`
	Enabled bool                      `json:"enabled"`
}

func (r webhookRequest) spec() management.WebhookSubscriptionSpec {
	return management.WebhookSubscriptionSpec{Name: r.Name, URL: r.URL, Events: r.Events, Enabled: r.Enabled}
}

// listDeliveries returns the recorded deliveries for one subscription in
// document order (oldest first). Delivery records survive subscription
// deletion, so history stays queryable after the endpoint is removed.
func (h *managementHandler) listDeliveries(subscriptionID string) []*management.WebhookDelivery {
	snapshot, err := h.store.Snapshot()
	if err != nil {
		return make([]*management.WebhookDelivery, 0)
	}
	out := make([]*management.WebhookDelivery, 0)
	for _, del := range snapshot.WebhookDeliveries {
		if del.SubscriptionID == subscriptionID {
			out = append(out, del)
		}
	}
	return out
}

func (h *managementHandler) handleAudit(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/audit")
	switch {
	case r.Method == http.MethodGet && id == "" && action == "":
		// Optional query parameters narrow the listing; empty filter
		// fields do not constrain their dimension, so a bare GET /audit
		// returns the whole log.
		q := r.URL.Query()
		filter := management.AuditFilter{
			Operator: q.Get("operator"),
			Action:   q.Get("action"),
			Result:   management.AuditResult(q.Get("result")),
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"audit": h.audit.ListFiltered(filter)})
	case r.Method == http.MethodPost && isRoute(id, action, "prune"):
		var req struct {
			MaxEntries int `json:"maxEntries"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		count, err := h.audit.Prune(req.MaxEntries)
		h.respondResult(w, map[string]interface{}{"removed": count}, err)
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handleNode(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/node")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"nodes": h.nodes.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.nodes.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req nodeRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.nodes.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req nodeRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.nodes.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "heartbeat":
		item, err := h.nodes.Heartbeat(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.nodes.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.nodes.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type nodeRequest struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Address string                `json:"address"`
	Status  management.NodeStatus `json:"status"`
	Enabled bool                  `json:"enabled"`
}

func (r nodeRequest) spec() management.NodeSpec {
	return management.NodeSpec{Name: r.Name, Address: r.Address, Status: r.Status, Enabled: r.Enabled}
}

func (h *managementHandler) handleInstance(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/instance")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		q := r.URL.Query()
		if nodeID := q.Get("nodeId"); nodeID != "" {
			h.writeJSON(w, http.StatusOK, map[string]interface{}{"instances": h.instances.ListByNode(nodeID)})
		} else {
			h.writeJSON(w, http.StatusOK, map[string]interface{}{"instances": h.instances.List()})
		}
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.instances.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req instanceRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.instances.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req instanceRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.instances.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "status"):
		var req struct {
			ID     string                    `json:"id"`
			Status management.InstanceStatus `json:"status"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "status")
		}
		item, err := h.instances.SetStatus(req.ID, req.Status)
		h.respondResult(w, item, err)
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.instances.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type instanceRequest struct {
	ID        string                    `json:"id"`
	NodeID    string                    `json:"nodeId"`
	Name      string                    `json:"name"`
	Status    management.InstanceStatus `json:"status"`
	ChannelID string                    `json:"channelId,omitempty"`
}

func (r instanceRequest) spec() management.InstanceSpec {
	return management.InstanceSpec{NodeID: r.NodeID, Name: r.Name, Status: r.Status, ChannelID: r.ChannelID}
}

func (h *managementHandler) handleRemoteCommand(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/remote-command")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		q := r.URL.Query()
		if nodeID := q.Get("nodeId"); nodeID != "" {
			h.writeJSON(w, http.StatusOK, map[string]interface{}{"commands": h.remoteCommands.ListByNode(nodeID)})
		} else {
			h.writeJSON(w, http.StatusOK, map[string]interface{}{"commands": h.remoteCommands.List()})
		}
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.remoteCommands.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req remoteCommandRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.remoteCommands.Enqueue(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "sent":
		item, err := h.remoteCommands.MarkSent(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "success":
		item, err := h.remoteCommands.MarkSuccess(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "failed":
		var req struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = id
		}
		item, err := h.remoteCommands.MarkFailed(req.ID, req.Error)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id == "purge" && action == "":
		var req struct {
			MaxKeep int `json:"maxKeep"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		count, err := h.remoteCommands.PurgeTerminal(req.MaxKeep)
		h.respondResult(w, map[string]interface{}{"removed": count}, err)
	default:
		http.NotFound(w, r)
	}
}

type remoteCommandRequest struct {
	NodeID     string                  `json:"nodeId"`
	InstanceID string                  `json:"instanceId,omitempty"`
	Action     management.RemoteAction `json:"action"`
}

func (r remoteCommandRequest) spec() management.RemoteCommandSpec {
	return management.RemoteCommandSpec{NodeID: r.NodeID, InstanceID: r.InstanceID, Action: r.Action}
}

func (h *managementHandler) handleConfigSnapshot(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/config-snapshot")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"snapshots": h.configSnapshots.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.configSnapshots.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req struct {
			Operator    string `json:"operator"`
			Description string `json:"description"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.configSnapshots.Create(req.Operator, req.Description)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id != "" && action == "restore":
		h.writeOK(w, h.configSnapshots.Restore(id))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.configSnapshots.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handleConfigTemplate(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/config-template")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"templates": h.configTemplates.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.configTemplates.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req configTemplateRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.configTemplates.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req configTemplateRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.configTemplates.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "expand"):
		var req struct {
			ID     string            `json:"id"`
			Params map[string]string `json:"params"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "expand")
		}
		item, err := h.configTemplates.Get(req.ID)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		body, err := management.Expand(item, req.Params)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"body": json.RawMessage(body)})
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.configTemplates.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.configTemplates.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

// configTemplateRequest carries the template body as a raw JSON value: the
// console posts body as a JSON object and the service stores it verbatim.
type configTemplateRequest struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Body    json.RawMessage `json:"body"`
	Enabled bool            `json:"enabled"`
}

func (r configTemplateRequest) spec() management.ConfigTemplateSpec {
	return management.ConfigTemplateSpec{Name: r.Name, Type: r.Type, Body: r.Body, Enabled: r.Enabled}
}

func (h *managementHandler) handleIndustryTemplate(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/industry-template")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"templates": h.industryTemplates.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.industryTemplates.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req industryTemplateRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.industryTemplates.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req industryTemplateRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.industryTemplates.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "deploy"):
		var req struct {
			ID     string            `json:"id"`
			Params map[string]string `json:"params"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "deploy")
		}
		item, err := h.industryTemplates.Get(req.ID)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		result, err := management.Deploy(item, req.Params, h.store)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"result": result})
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.industryTemplates.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.industryTemplates.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type industryTemplateRequest struct {
	ID                string                       `json:"id"`
	Name              string                       `json:"name"`
	Description       string                       `json:"description"`
	PlaylistName      string                       `json:"playlistName"`
	MediaPlaceholders []string                     `json:"mediaPlaceholders"`
	SceneKinds        []management.SceneKind       `json:"sceneKinds"`
	Task              *management.IndustryTaskSpec `json:"task"`
	Enabled           bool                         `json:"enabled"`
}

func (r industryTemplateRequest) spec() management.IndustryTemplateSpec {
	return management.IndustryTemplateSpec{
		Name:              r.Name,
		Description:       r.Description,
		PlaylistName:      r.PlaylistName,
		MediaPlaceholders: r.MediaPlaceholders,
		SceneKinds:        r.SceneKinds,
		Task:              r.Task,
		Enabled:           r.Enabled,
	}
}

func (h *managementHandler) handleSmartRule(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/smart-rule")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"rules": h.smartRules.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.smartRules.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req smartRuleRequest
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.smartRules.Create(req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "update"):
		var req smartRuleRequest
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "update")
		}
		item, err := h.smartRules.Update(req.ID, req.spec())
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "generate"):
		var req struct {
			ID     string   `json:"id"`
			Recent []string `json:"recent"`
			Limit  int      `json:"limit"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "generate")
		}
		rule, err := h.smartRules.Get(req.ID)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		// Generation is a pure preview: the rule filters the library and the
		// result is returned without persisting anything (ApplyGenerated is
		// the explicit write step). A positive limit caps the preview length
		// client-side; GeneratePlaylist itself is bounded by the rule's
		// MaxItems.
		mediaIDs, err := management.GeneratePlaylist(rule, h.media.List(), req.Recent, time.Now())
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		if req.Limit > 0 && len(mediaIDs) > req.Limit {
			mediaIDs = mediaIDs[:req.Limit]
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"mediaIds": mediaIDs})
	case r.Method == http.MethodPost && isRoute(id, action, "generate-and-apply"):
		var req struct {
			ID           string   `json:"id"`
			Recent       []string `json:"recent"`
			PlaylistName string   `json:"playlistName"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "generate-and-apply")
		}
		rule, err := h.smartRules.Get(req.ID)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		mediaIDs, err := management.GeneratePlaylist(rule, h.media.List(), req.Recent, time.Now())
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		playlist, err := management.ApplyGenerated(h.store, req.PlaylistName, mediaIDs)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"playlist": playlist})
	case r.Method == http.MethodPost && isRoute(id, action, "enabled"):
		var req struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "enabled")
		}
		h.writeOK(w, h.smartRules.SetEnabled(req.ID, req.Enabled))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.smartRules.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

type smartRuleRequest struct {
	ID             string                `json:"id"`
	Name           string                `json:"name"`
	Description    string                `json:"description"`
	TimeSlots      []management.TimeSlot `json:"timeSlots"`
	Tags           []string              `json:"tags"`
	MaxDurationSec int                   `json:"maxDurationSec"`
	AvoidRepeat    bool                  `json:"avoidRepeat"`
	RepeatLookback int                   `json:"repeatLookback"`
	MaxItems       int                   `json:"maxItems"`
	Enabled        bool                  `json:"enabled"`
}

func (r smartRuleRequest) spec() management.SmartRuleSpec {
	return management.SmartRuleSpec{
		Name:           r.Name,
		Description:    r.Description,
		TimeSlots:      r.TimeSlots,
		Tags:           r.Tags,
		MaxDurationSec: r.MaxDurationSec,
		AvoidRepeat:    r.AvoidRepeat,
		RepeatLookback: r.RepeatLookback,
		MaxItems:       r.MaxItems,
		Enabled:        r.Enabled,
	}
}

// dayCountView and playbackSummaryView are the REST shapes of the metrics
// aggregates: the service types carry no json tags, so the handler maps
// them to the camelCase shapes the console consumes.
type dayCountView struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type playbackSummaryView struct {
	TotalPlays  int     `json:"totalPlays"`
	Successes   int     `json:"successes"`
	Failures    int     `json:"failures"`
	SuccessRate float64 `json:"successRate"`
}

func (h *managementHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/metrics")
	if action == "" {
		action = id
	}
	switch {
	case r.Method == http.MethodGet && action == "failure-rate":
		mediaID := r.URL.Query().Get("mediaId")
		rate, plays, failures, err := h.metrics.MediaFailureRate(mediaID)
		h.respondResult(w, map[string]interface{}{"rate": rate, "plays": plays, "failures": failures}, err)
	case r.Method == http.MethodGet && action == "trend":
		days := 7
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				h.writeError(w, http.StatusBadRequest, "days must be an integer")
				return
			}
			days = n
		}
		counts, err := h.metrics.OutputStabilityTrend(days)
		if err != nil {
			h.writeManagementError(w, err)
			return
		}
		view := make([]dayCountView, 0, len(counts))
		for _, c := range counts {
			view = append(view, dayCountView{Date: c.Date, Count: c.Count})
		}
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"days": view})
	case r.Method == http.MethodGet && action == "summary":
		sum := h.metrics.PlaybackSummary()
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"summary": playbackSummaryView{
			TotalPlays:  sum.TotalPlays,
			Successes:   sum.Successes,
			Failures:    sum.Failures,
			SuccessRate: sum.SuccessRate,
		}})
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handleSuggestion(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/suggestion")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"suggestions": h.suggestions.List()})
	case r.Method == http.MethodGet && id != "" && action == "":
		item, err := h.suggestions.Get(id)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && id == "recommend" && action == "":
		// RecommendMedia is a pure ranking query: nothing is persisted, so
		// the route is read-mapped in requestAction.
		var req struct {
			Recent []string `json:"recent"`
			Limit  int      `json:"limit"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		mediaIDs, err := management.RecommendMedia(h.store, req.Recent, req.Limit)
		h.respondResult(w, map[string]interface{}{"mediaIds": mediaIDs}, err)
	case r.Method == http.MethodPost && (id == "" || isRoute(id, action, "add")):
		var req struct {
			Kind    management.SuggestionKind `json:"kind"`
			Title   string                    `json:"title"`
			Payload map[string]string         `json:"payload"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.suggestions.Create(req.Kind, req.Title, req.Payload)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "approve"):
		var req struct {
			ID           string `json:"id"`
			PlaylistName string `json:"playlistName"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "approve")
		}
		item, err := h.suggestions.Approve(req.ID, req.PlaylistName)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "reject"):
		var req struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "reject")
		}
		item, err := h.suggestions.Reject(req.ID, req.Reason)
		h.respondResult(w, item, err)
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handleAlarm(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/alarm")
	switch {
	case r.Method == http.MethodGet && (id == "" || isRoute(id, action, "list")):
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"alarms": h.alarms.List()})
	case r.Method == http.MethodPost && isRoute(id, action, "raise"):
		var req struct {
			Level   management.AlarmLevel `json:"level"`
			Title   string                `json:"title"`
			Message string                `json:"message"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		item, err := h.alarms.Raise(req.Level, req.Title, req.Message)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "resolve"):
		var req struct {
			ID string `json:"id"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "resolve")
		}
		item, err := h.alarms.Resolve(req.ID)
		h.respondResult(w, item, err)
	case r.Method == http.MethodPost && isRoute(id, action, "resolve-all"):
		count, err := h.alarms.ResolveAll()
		h.respondResult(w, map[string]interface{}{"resolved": count}, err)
	case r.Method == http.MethodPost && isRoute(id, action, "delete"):
		var req struct {
			ID string `json:"id"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.ID == "" {
			req.ID = routeTarget(id, action, "delete")
		}
		h.writeOK(w, h.alarms.Delete(req.ID))
	case r.Method == http.MethodDelete && id != "" && (action == "" || isRoute(id, action, "remove")):
		h.writeOK(w, h.alarms.Delete(routeTarget(id, action, "remove")))
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handleScheduler(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/scheduler")
	if id != "" && action == "" {
		action = id
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"running": h.scheduler.Running()})
	case r.Method == http.MethodPost && action == "start":
		err := h.scheduler.Start()
		h.respondResult(w, map[string]interface{}{"running": h.scheduler.Running()}, err)
	case r.Method == http.MethodPost && action == "stop":
		h.scheduler.Stop()
		h.writeJSON(w, http.StatusOK, map[string]interface{}{"running": false})
	default:
		http.NotFound(w, r)
	}
}

func (h *managementHandler) handlePlayer(w http.ResponseWriter, r *http.Request) {
	id, action := resourcePath(r.URL.Path, "/player")
	if action == "" {
		action = id
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var err error
	switch action {
	case "play":
		var req struct {
			MediaID         string  `json:"mediaId"`
			PlaylistID      string  `json:"playlistId"`
			SceneTemplateID string  `json:"sceneTemplateId"`
			SeekSeconds     float64 `json:"seekSeconds"`
			Random          bool    `json:"random"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		if req.MediaID == "" && req.PlaylistID == "" {
			h.writeError(w, http.StatusBadRequest, "play request target is empty")
			return
		}
		// sceneTemplateId is advisory metadata for the playback backend: it
		// rides along in the PlayRequest but the adapter does not consume it
		// (rendering the scene is core work, outside this layer).
		err = h.player.Play(r.Context(), management.PlayRequest{
			MediaID:         req.MediaID,
			PlaylistID:      req.PlaylistID,
			SceneTemplateID: req.SceneTemplateID,
			SeekSeconds:     req.SeekSeconds,
			Random:          req.Random,
		})
	case "interrupt":
		var req struct {
			MediaID    string `json:"mediaId"`
			PlaylistID string `json:"playlistId"`
			Duration   int    `json:"duration"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		// duration defaults to 0: a one-shot interrupt that plays until the
		// Player ends it. A positive duration makes the interrupt timed: the
		// pre-interrupt target is restored once it elapses.
		err = h.scheduler.Interrupt(management.PlayRequest{MediaID: req.MediaID, PlaylistID: req.PlaylistID}, req.Duration)
	case "pause":
		err = h.player.Pause(r.Context())
	case "continue":
		err = h.player.Continue(r.Context())
	case "skip":
		err = h.player.Skip(r.Context())
	case "seek":
		var req struct {
			SeekSeconds float64 `json:"seekSeconds"`
			Seconds     float64 `json:"seconds"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		sec := req.SeekSeconds
		if sec == 0 {
			sec = req.Seconds
		}
		err = h.player.Seek(r.Context(), sec)
	case "stop":
		err = h.player.Stop(r.Context())
	default:
		http.NotFound(w, r)
		return
	}
	h.writeOK(w, err)
}

func isRoute(id, action, expected string) bool {
	return id == expected || action == expected
}

func routeTarget(id, action, route string) string {
	if id == route {
		return action
	}
	if action == route {
		return id
	}
	return id
}

func (h *managementHandler) decode(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (h *managementHandler) respondResult(w http.ResponseWriter, value interface{}, err error) {
	if err != nil {
		h.writeManagementError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, value)
}

func (h *managementHandler) writeOK(w http.ResponseWriter, err error) {
	if err != nil {
		h.writeManagementError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *managementHandler) writeManagementError(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	switch {
	case errors.Is(err, management.ErrInvalid):
		statusCode = http.StatusBadRequest
	case errors.Is(err, management.ErrNotFound):
		statusCode = http.StatusNotFound
	case errors.Is(err, management.ErrExists), errors.Is(err, management.ErrInUse), errors.Is(err, management.ErrAlreadyRunning), errors.Is(err, management.ErrQueueFull), errors.Is(err, management.ErrNotRunning):
		statusCode = http.StatusConflict
	}
	h.writeError(w, statusCode, err.Error())
}

func (h *managementHandler) writeError(w http.ResponseWriter, statusCode int, message string) {
	h.writeJSON(w, statusCode, map[string]interface{}{"code": statusCode, "message": message})
}

// validateMediaLite re-checks the media invariants after a partial update
// (sort order and path presence). management.validateMedia runs on Add; the
// update path re-applies the sort-order whitelist here.
func validateMediaLite(m *management.Media) error {
	if m == nil {
		return fmt.Errorf("media: nil media")
	}
	switch m.SortBy {
	case "", management.SortByName, management.SortByTime, management.SortByRandom:
		return nil
	}
	return fmt.Errorf("media: %w: invalid sortBy %q", management.ErrInvalid, m.SortBy)
}

func (h *managementHandler) writeJSON(w http.ResponseWriter, statusCode int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(value)
}

func resourcePath(raw, root string) (string, string) {
	trimmed := strings.TrimPrefix(raw, root)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return "", ""
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// playerAdapter turns management scheduler requests into existing KPlayer
// provider operations. Playlist playback is expanded into every resource in
// the programme and then seeks to the first entry to start; normal queue
// progression remains owned by the resource provider.
//
// With an ffmpeg engine injected (engine non-nil) playback is handed to the
// real engine instead: Play starts an ffmpeg subprocess pushing the source
// to the configured outputs, Stop terminates it, Pause suspends the stream
// (the push ends, the position is remembered), Continue resumes it at that
// position and Skip advances to the next item of the playback queue (or
// restarts the current source when the queue holds one item).
type playerAdapter struct {
	play     playprovider.ProviderI
	resource resourceprovider.ProviderI
	store    *management.Store
	engine   engine.Engine

	// queue is the engine-mode playback queue: the media paths of the
	// resolved target in programme order. queueIdx points at the current
	// item; Skip advances it. Guarded by queueMu.
	queueMu  sync.Mutex
	queue    []string
	queueIdx int
}

func (p *playerAdapter) Play(ctx context.Context, req management.PlayRequest) error {
	// req.SceneTemplateID is deliberately not consumed: applying a scene to
	// playback is core rendering work owned by a later batch. The field is
	// accepted and carried so the request model and REST contract stay
	// complete.
	switch {
	case req.MediaID != "":
		media, err := management.NewMediaService(p.store).Get(req.MediaID)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.engine != nil {
			items, err := management.NewMediaService(p.store).Expand(media)
			if err != nil {
				return err
			}
			queue := playItemsOf(items)
			p.setQueue(pathsOf(queue), 0)
			// StartQueue 单元素与 StartAt 等价，但会携带媒体的外挂音频/字幕。
			return p.engine.StartQueue(ctx, queue, req.SeekSeconds, false)
		}
		_, err = p.resource.ResourceAdd(ctx, &svrproto.ResourceAddArgs{Path: media.Path, Unique: media.ID})
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "existed") {
			return err
		}
		_, err = p.resource.ResourceSeek(ctx, &svrproto.ResourceSeekArgs{Unique: media.ID, Seek: 0})
		return err
	case req.PlaylistID != "":
		// ResolveWithFallback transparently follows the configured fallback
		// chain when the requested playlist cannot satisfy the request (it
		// is empty or references missing media), so the playback path uses
		// the backup programme itself. The usedFallback flag is ignored:
		// it only reports whether a hop was taken, which this layer does
		// not surface.
		items, _, err := management.NewPlaylistService(p.store).ResolveWithFallback(req.PlaylistID)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return fmt.Errorf("playlist %q is empty", req.PlaylistID)
		}
		if p.engine != nil {
			// Every item (including directory media) expands into its
			// concrete files; the queue plays one after another with the
			// playlist's playback mode (order / loop / random).
			pl, err := management.NewPlaylistService(p.store).Get(req.PlaylistID)
			if err != nil {
				return err
			}
			queue := []engine.PlayItem{}
			for _, it := range items {
				expanded, err := management.NewMediaService(p.store).Expand(it)
				if err != nil {
					return err
				}
				queue = append(queue, playItemsOf(expanded)...)
			}
			if len(queue) == 0 {
				return fmt.Errorf("playlist %q is empty", req.PlaylistID)
			}
			mode := pl.EffectiveMode()
			loop := mode == management.PlayModeLoop || mode == management.PlayModeRandomLoop
			if mode == management.PlayModeRandom || mode == management.PlayModeRandomLoop {
				rand.Shuffle(len(queue), func(i, j int) { queue[i], queue[j] = queue[j], queue[i] })
			}
			p.setQueue(pathsOf(queue), 0)
			return p.engine.StartQueue(ctx, queue, req.SeekSeconds, loop)
		}
		// Expand the whole programme: add every resolved item as a
		// resource (tolerating entries that are already present), then
		// seek to the first item to begin playback.
		for _, item := range items {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_, err = p.resource.ResourceAdd(ctx, &svrproto.ResourceAddArgs{Path: item.Path, Unique: item.ID})
			if err != nil && !strings.Contains(strings.ToLower(err.Error()), "existed") {
				return err
			}
		}
		_, err = p.resource.ResourceSeek(ctx, &svrproto.ResourceSeekArgs{Unique: items[0].ID, Seek: 0})
		return err
	default:
		return fmt.Errorf("play request target is empty")
	}
}

func (p *playerAdapter) Pause(ctx context.Context) error {
	if p.engine != nil {
		return p.engine.Pause(ctx)
	}
	_, err := p.play.PlayPause(ctx, &svrproto.PlayPauseArgs{})
	return err
}
func (p *playerAdapter) Continue(ctx context.Context) error {
	if p.engine != nil {
		return p.engine.Continue(ctx)
	}
	_, err := p.play.PlayContinue(ctx, &svrproto.PlayContinueArgs{})
	return err
}
func (p *playerAdapter) Skip(ctx context.Context) error {
	if p.engine != nil {
		p.queueMu.Lock()
		defer p.queueMu.Unlock()
		if len(p.queue) == 0 {
			return p.engine.Skip(ctx)
		}
		p.queueIdx = (p.queueIdx + 1) % len(p.queue)
		return p.engine.StartAt(ctx, p.queue[p.queueIdx], 0)
	}
	_, err := p.play.PlaySkip(ctx, &svrproto.PlaySkipArgs{})
	return err
}

// setQueue records the engine-mode playback queue and the current item.
func (p *playerAdapter) setQueue(paths []string, idx int) {
	p.queueMu.Lock()
	p.queue = append([]string(nil), paths...)
	p.queueIdx = idx
	p.queueMu.Unlock()
}

// playItemsOf converts expanded media entries into engine play items,
// carrying the optional auxiliary inputs (external audio / burned-in
// subtitle) of the directory or file.
func playItemsOf(media []*management.Media) []engine.PlayItem {
	out := make([]engine.PlayItem, 0, len(media))
	for _, m := range media {
		if m == nil {
			continue
		}
		out = append(out, engine.PlayItem{
			Path:         m.Path,
			AudioPath:    m.AudioPath,
			SubtitlePath: m.SubtitlePath,
		})
	}
	return out
}

// pathsOf extracts the paths of play items (used for the skip queue).
func pathsOf(items []engine.PlayItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Path
	}
	return out
}

// Seek jumps playback to an absolute media position in seconds. With an
// engine the current source is restarted from that offset (the engine
// seeks its input before pushing); the legacy stub path is not seekable
// through this endpoint and reports an error.
func (p *playerAdapter) Seek(ctx context.Context, seconds float64) error {
	if seconds < 0 {
		seconds = 0
	}
	if p.engine != nil {
		st := p.engine.Status()
		if st.SourcePath == "" {
			return fmt.Errorf("seek: %w: no playback to jump", management.ErrNotRunning)
		}
		return p.engine.StartAt(ctx, st.SourcePath, seconds)
	}
	return fmt.Errorf("seek requires the ffmpeg engine")
}
func (p *playerAdapter) Stop(ctx context.Context) error {
	if p.engine != nil {
		return p.engine.Stop(ctx)
	}
	_, err := p.play.PlayStop(ctx, &svrproto.PlayStopArgs{})
	return err
}

type providerStatus struct {
	play     playprovider.ProviderI
	resource resourceprovider.ProviderI
	output   outputprovider.ProviderI
}

func (s *providerStatus) Status(ctx context.Context) statusSnapshot {
	result := statusSnapshot{}
	result.Current = callWithTimeout(ctx, func(callCtx context.Context) interface{} {
		value, err := s.resource.ResourceCurrent(callCtx, &svrproto.ResourceCurrentArgs{})
		if err != nil {
			return nil
		}
		return value
	})
	result.Duration = callWithTimeout(ctx, func(callCtx context.Context) interface{} {
		value, err := s.play.PlayDuration(callCtx, &svrproto.PlayDurationArgs{})
		if err != nil {
			return nil
		}
		return value
	})
	result.Information = callWithTimeout(ctx, func(callCtx context.Context) interface{} {
		value, err := s.play.PlayInformation(callCtx, &svrproto.PlayInformationArgs{})
		if err != nil {
			return nil
		}
		return value
	})
	result.Resources = callWithTimeout(ctx, func(callCtx context.Context) interface{} {
		value, err := s.resource.ResourceListAll(callCtx, &svrproto.ResourceListAllArgs{})
		if err != nil {
			return nil
		}
		return value
	})
	result.Outputs = callWithTimeout(ctx, func(callCtx context.Context) interface{} {
		value, err := s.output.OutputList(callCtx, &svrproto.OutputListArgs{})
		if err != nil {
			return nil
		}
		return value
	})
	return result
}

func callWithTimeout(parent context.Context, fn func(context.Context) interface{}) interface{} {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	result := make(chan interface{}, 1)
	go func() { result <- fn(ctx) }()
	select {
	case value := <-result:
		return value
	case <-ctx.Done():
		return nil
	}
}
