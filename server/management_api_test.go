package server

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytelang/kplayer/management"
	outputprovider "github.com/bytelang/kplayer/module/output/provider"
	playprovider "github.com/bytelang/kplayer/module/play/provider"
	resourceprovider "github.com/bytelang/kplayer/module/resource/provider"
	"github.com/bytelang/kplayer/types/config"
	svrproto "github.com/bytelang/kplayer/types/server"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// Local fake providers. These stand in for the gRPC provider interfaces so the
// management REST API can be exercised without a running core / native player.
// ---------------------------------------------------------------------------

type fakePlayProvider struct {
	block time.Duration // when non-zero, status reads sleep longer than the 2s handler timeout
}

func (f *fakePlayProvider) GetStartPoint() uint32           { return 1 }
func (f *fakePlayProvider) GetPlayModel() config.PLAY_MODEL { return config.PLAY_MODEL(0) }
func (f *fakePlayProvider) GetRPCParams() config.Server     { return config.Server{} }
func (f *fakePlayProvider) GetCacheOn() bool                { return false }

func (f *fakePlayProvider) PlayStop(_ context.Context, _ *svrproto.PlayStopArgs) (*svrproto.PlayStopReply, error) {
	return &svrproto.PlayStopReply{}, nil
}
func (f *fakePlayProvider) PlayPause(_ context.Context, _ *svrproto.PlayPauseArgs) (*svrproto.PlayPauseReply, error) {
	return &svrproto.PlayPauseReply{}, nil
}
func (f *fakePlayProvider) PlaySkip(_ context.Context, _ *svrproto.PlaySkipArgs) (*svrproto.PlaySkipReply, error) {
	return &svrproto.PlaySkipReply{}, nil
}
func (f *fakePlayProvider) PlayContinue(_ context.Context, _ *svrproto.PlayContinueArgs) (*svrproto.PlayContinueReply, error) {
	return &svrproto.PlayContinueReply{}, nil
}
func (f *fakePlayProvider) PlayDuration(_ context.Context, _ *svrproto.PlayDurationArgs) (*svrproto.PlayDurationReply, error) {
	if f.block > 0 {
		time.Sleep(f.block)
	}
	return &svrproto.PlayDurationReply{}, nil
}
func (f *fakePlayProvider) PlayInformation(_ context.Context, _ *svrproto.PlayInformationArgs) (*svrproto.PlayInformationReply, error) {
	if f.block > 0 {
		time.Sleep(f.block)
	}
	return &svrproto.PlayInformationReply{}, nil
}

type fakeResourceProvider struct {
	block time.Duration
	// Recorded provider calls so tests can assert on what Play expanded.
	mu        sync.Mutex
	addCalls  []svrproto.ResourceAddArgs
	seekCalls []svrproto.ResourceSeekArgs
	// seekBlock, when non-nil, makes ResourceSeek wait until the channel is
	// closed before returning, so a test can hold a scheduler-dispatched
	// play in flight (mirroring blockingPlayer in the management tests).
	seekBlock chan struct{}
}

func (f *fakeResourceProvider) ResourceAdd(_ context.Context, args *svrproto.ResourceAddArgs) (*svrproto.ResourceAddReply, error) {
	if args != nil {
		f.mu.Lock()
		f.addCalls = append(f.addCalls, *args)
		f.mu.Unlock()
	}
	return &svrproto.ResourceAddReply{}, nil
}
func (f *fakeResourceProvider) ResourceRemove(_ context.Context, _ *svrproto.ResourceRemoveArgs) (*svrproto.ResourceRemoveReply, error) {
	return &svrproto.ResourceRemoveReply{}, nil
}
func (f *fakeResourceProvider) ResourceList(_ context.Context, _ *svrproto.ResourceListArgs) (*svrproto.ResourceListReply, error) {
	return &svrproto.ResourceListReply{}, nil
}
func (f *fakeResourceProvider) ResourceListAll(_ context.Context, _ *svrproto.ResourceListAllArgs) (*svrproto.ResourceListAllReply, error) {
	if f.block > 0 {
		time.Sleep(f.block)
	}
	return &svrproto.ResourceListAllReply{}, nil
}
func (f *fakeResourceProvider) ResourceCurrent(_ context.Context, _ *svrproto.ResourceCurrentArgs) (*svrproto.ResourceCurrentReply, error) {
	if f.block > 0 {
		time.Sleep(f.block)
	}
	return &svrproto.ResourceCurrentReply{}, nil
}
func (f *fakeResourceProvider) ResourceSeek(_ context.Context, args *svrproto.ResourceSeekArgs) (*svrproto.ResourceSeekReply, error) {
	if args != nil {
		f.mu.Lock()
		f.seekCalls = append(f.seekCalls, *args)
		f.mu.Unlock()
	}
	if f.seekBlock != nil {
		<-f.seekBlock
	}
	return &svrproto.ResourceSeekReply{}, nil
}

// seekSnapshot returns a copy of the recorded seek calls, safe to assert on
// while a scheduler-dispatched play may still be writing to the provider.
func (f *fakeResourceProvider) seekSnapshot() []svrproto.ResourceSeekArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]svrproto.ResourceSeekArgs, len(f.seekCalls))
	copy(out, f.seekCalls)
	return out
}

type fakeOutputProvider struct {
	// ProviderI carries the unexported mustEmbedUnimplementedOutputGreeterServer,
	// whose identity is bound to the package that declares the interface
	// (module/output/provider): neither a locally defined method nor an embed
	// of svrproto.UnimplementedOutputGreeterServer can satisfy it. Embedding
	// the real Provider provides the method; the fake is used only for its
	// method set, its own OutputAdd/OutputRemove/OutputList shadow the
	// promoted stubs, and the zero value is safe because no Provider method
	// is ever called on the fake.
	outputprovider.Provider
	block time.Duration
	// mu guards outputs and the recorded provider calls: the failover
	// monitor reads the output list from its own goroutine while tests
	// mutate it, so every access goes through the lock.
	mu          sync.Mutex
	outputs     []svrproto.OutputModule
	addCalls    []svrproto.OutputAddArgs
	removeCalls []svrproto.OutputRemoveArgs
}

func (f *fakeOutputProvider) OutputAdd(_ context.Context, args *svrproto.OutputAddArgs) (*svrproto.OutputAddReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if args != nil {
		f.addCalls = append(f.addCalls, *args)
	}
	// Mirror the real provider: adding an output that is already present is
	// an error the adapter tolerates.
	for _, out := range f.outputs {
		if out.Unique == args.Unique {
			return nil, outputprovider.OutputUniqueHasExisted
		}
	}
	f.outputs = append(f.outputs, svrproto.OutputModule{Path: args.Path, Unique: args.Unique})
	return &svrproto.OutputAddReply{}, nil
}

func (f *fakeOutputProvider) OutputRemove(_ context.Context, args *svrproto.OutputRemoveArgs) (*svrproto.OutputRemoveReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if args != nil {
		f.removeCalls = append(f.removeCalls, *args)
	}
	// Mirror the real provider: removing an unknown unique is an error the
	// adapter ignores.
	for i, out := range f.outputs {
		if out.Unique == args.Unique {
			f.outputs = append(f.outputs[:i], f.outputs[i+1:]...)
			return &svrproto.OutputRemoveReply{}, nil
		}
	}
	return nil, outputprovider.OutputUniqueNotFound
}

func (f *fakeOutputProvider) OutputList(_ context.Context, _ *svrproto.OutputListArgs) (*svrproto.OutputListReply, error) {
	if f.block > 0 {
		time.Sleep(f.block)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*svrproto.OutputModule, 0, len(f.outputs))
	for i := range f.outputs {
		out = append(out, &f.outputs[i])
	}
	return &svrproto.OutputListReply{Outputs: out}, nil
}

// setOutputs replaces the configured output list, i.e. the connectivity
// state the failover monitor's reader sees.
func (f *fakeOutputProvider) setOutputs(outputs []svrproto.OutputModule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs = outputs
}

// outputActions returns copies of the recorded OutputAdd/OutputRemove calls,
// safe to assert on while the failover monitor may still be writing.
func (f *fakeOutputProvider) outputActions() (adds []svrproto.OutputAddArgs, removes []svrproto.OutputRemoveArgs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	adds = append(adds, f.addCalls...)
	removes = append(removes, f.removeCalls...)
	return adds, removes
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestManagementHandler mirrors newManagementHandler but backs the store
// with an isolated temp file (the production constructor pins management.json
// in the working directory, which would bleed state between tests).
func newTestManagementHandler(t *testing.T, play playprovider.ProviderI, resource resourceprovider.ProviderI, output outputprovider.ProviderI, authOn bool, authToken string) *managementHandler {
	return newTestManagementHandlerWithEngine(t, play, resource, output, authOn, authToken, nil)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// jsonBody marshals v for use as a request payload.
func jsonBody(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	return string(b)
}

// perform issues a request against h and returns the status code and body.
func perform(t *testing.T, h http.Handler, method, target, token, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// performWithin issues a request in a goroutine and fails the test if it does
// not complete before the deadline. Used to prove a handler does not hang.
func performWithin(t *testing.T, h http.Handler, method, target, body string, timeout time.Duration) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rr, req)
		close(done)
	}()
	select {
	case <-done:
		return rr.Code, rr.Body.String()
	case <-time.After(timeout):
		t.Fatalf("request %s %s did not complete within %s", method, target, timeout)
		return 0, ""
	}
}

// addMedia posts a media entry and returns its id.
func addMedia(t *testing.T, h http.Handler) string {
	t.Helper()
	code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{
		"path": "/videos/test.mp4",
		"name": "test media",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /media: status %d body %s", code, body)
	}
	id := gjson.Get(body, "id").String()
	if id == "" {
		t.Fatalf("POST /media: no id in response: %s", body)
	}
	return id
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

func TestManagementAuthRequired(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, true, "secret-token")

	// No token -> 401.
	code, body := perform(t, h, http.MethodGet, "/status", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401 (body %s)", code, body)
	}
	if !strings.Contains(body, "authentication failed") {
		t.Fatalf("no token: unexpected body %q", body)
	}

	// Wrong token -> 401.
	code, body = perform(t, h, http.MethodGet, "/status", "wrong-token", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401 (body %s)", code, body)
	}

	// Correct token -> 200.
	code, body = perform(t, h, http.MethodGet, "/status", "secret-token", "")
	if code != http.StatusOK {
		t.Fatalf("correct token: status %d, want 200 (body %s)", code, body)
	}
}

func TestManagementAuthDisabledSkipsCheck(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "secret-token")

	// With auth disabled a missing header still succeeds.
	code, body := perform(t, h, http.MethodGet, "/status", "", "")
	if code != http.StatusOK {
		t.Fatalf("auth disabled: status %d, want 200 (body %s)", code, body)
	}
}

// ---------------------------------------------------------------------------
// Session authentication (/auth/login, /auth/logout, /auth/me)
// ---------------------------------------------------------------------------

// seedUser creates a console user directly through the service, as the
// server layer's user bootstrap would (there is no open registration route).
func seedUser(t *testing.T, h *managementHandler, username, password string, role management.UserRole, enabled bool) *management.User {
	t.Helper()
	user, err := h.users.Create(management.UserSpec{Username: username, Password: password, Role: role, Enabled: enabled})
	if err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	return user
}

// login performs a login request and returns the session token.
func login(t *testing.T, h http.Handler, username, password string) string {
	t.Helper()
	code, body := perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{
		"username": username, "password": password,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /auth/login %s: status %d body %s", username, code, body)
	}
	token := gjson.Get(body, "token").String()
	if token == "" {
		t.Fatalf("POST /auth/login %s: no token in %s", username, body)
	}
	return token
}

func TestManagementAuthLogin(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	seedUser(t, h, "admin", "password123", management.RoleAdmin, true)

	code, body := perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{
		"username": "admin", "password": "password123",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /auth/login: status %d body %s", code, body)
	}
	if gjson.Get(body, "token").String() == "" {
		t.Fatalf("POST /auth/login: no token in %s", body)
	}
	if gjson.Get(body, "username").String() != "admin" ||
		gjson.Get(body, "role").String() != string(management.RoleAdmin) ||
		gjson.Get(body, "expiresAt").String() == "" {
		t.Fatalf("POST /auth/login: unexpected body %s", body)
	}

	// Wrong password -> 401 with a single generic message.
	code, body = perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{
		"username": "admin", "password": "wrong-password",
	}))
	if code != http.StatusUnauthorized {
		t.Fatalf("POST /auth/login wrong password: status %d, want 401 (body %s)", code, body)
	}
	if !strings.Contains(body, "invalid credentials") {
		t.Fatalf("POST /auth/login wrong password: unexpected body %q", body)
	}

	// Unknown user -> 401 with the same message (no username enumeration).
	code, body = perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{
		"username": "nobody", "password": "password123",
	}))
	if code != http.StatusUnauthorized || !strings.Contains(body, "invalid credentials") {
		t.Fatalf("POST /auth/login unknown user: status %d body %s, want 401 invalid credentials", code, body)
	}

	// Disabled user -> 401 with the same message.
	seedUser(t, h, "fired", "password123", management.RoleOperator, false)
	code, body = perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{
		"username": "fired", "password": "password123",
	}))
	if code != http.StatusUnauthorized || !strings.Contains(body, "invalid credentials") {
		t.Fatalf("POST /auth/login disabled user: status %d body %s, want 401 invalid credentials", code, body)
	}
}

func TestManagementAuthMeLogout(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	seedUser(t, h, "admin", "password123", management.RoleAdmin, true)
	token := login(t, h, "admin", "password123")

	// /auth/me round-trips the session identity.
	code, body := perform(t, h, http.MethodGet, "/auth/me", "Bearer "+token, "")
	if code != http.StatusOK {
		t.Fatalf("GET /auth/me: status %d body %s", code, body)
	}
	if gjson.Get(body, "username").String() != "admin" ||
		gjson.Get(body, "role").String() != string(management.RoleAdmin) ||
		gjson.Get(body, "expiresAt").String() == "" {
		t.Fatalf("GET /auth/me: unexpected body %s", body)
	}

	// An invalid bearer token is a 401.
	code, body = perform(t, h, http.MethodGet, "/auth/me", "Bearer not-a-session", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("GET /auth/me invalid token: status %d, want 401 (body %s)", code, body)
	}

	// Logout via the body token; the session is then gone.
	code, body = perform(t, h, http.MethodPost, "/auth/logout", "", jsonBody(t, map[string]interface{}{"token": token}))
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /auth/logout: status %d body %s", code, body)
	}
	code, _ = perform(t, h, http.MethodGet, "/auth/me", "Bearer "+token, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("GET /auth/me after logout: status %d, want 401", code)
	}

	// Logout via the Authorization header also works (fresh login).
	token = login(t, h, "admin", "password123")
	code, body = perform(t, h, http.MethodPost, "/auth/logout", "Bearer "+token, "")
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /auth/logout via header: status %d body %s", code, body)
	}
	code, _ = perform(t, h, http.MethodGet, "/auth/me", "Bearer "+token, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("GET /auth/me after header logout: status %d, want 401", code)
	}

	// Logout without any token is a 400.
	code, _ = perform(t, h, http.MethodPost, "/auth/logout", "", "")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /auth/logout without token: status %d, want 400", code)
	}
}

// Auth-disabled mode (the default for a private single-machine console):
// /auth/me without a token answers 200 with an anonymous admin principal so
// the frontend can enter the console directly. With auth enabled the same
// probe is a 401 and the frontend shows the login screen instead.
func TestManagementAuthMeAnonymousWhenAuthDisabled(t *testing.T) {
	t.Run("auth disabled -> anonymous admin", func(t *testing.T) {
		h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
		code, body := perform(t, h, http.MethodGet, "/auth/me", "", "")
		if code != http.StatusOK {
			t.Fatalf("GET /auth/me (auth off, no token): status %d, want 200 (body %s)", code, body)
		}
		if gjson.Get(body, "username").String() != "admin" ||
			gjson.Get(body, "role").String() != string(management.RoleAdmin) ||
			gjson.Get(body, "authRequired").Bool() {
			t.Fatalf("GET /auth/me (auth off, no token): unexpected body %s", body)
		}
		// A stored session still round-trips its own identity in auth-off mode.
		seedUser(t, h, "ops", "password123", management.RoleOperator, true)
		token := login(t, h, "ops", "password123")
		code, body = perform(t, h, http.MethodGet, "/auth/me", "Bearer "+token, "")
		if code != http.StatusOK || gjson.Get(body, "username").String() != "ops" {
			t.Fatalf("GET /auth/me (auth off, with session): status %d body %s", code, body)
		}
	})

	t.Run("auth enabled -> login required", func(t *testing.T) {
		h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, true, "fixed-token")
		code, body := perform(t, h, http.MethodGet, "/auth/me", "", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("GET /auth/me (auth on, no token): status %d, want 401 (body %s)", code, body)
		}
	})
}

// ---------------------------------------------------------------------------
// User management CRUD (/user)
// ---------------------------------------------------------------------------

func TestManagementUserCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create a user through the API.
	code, body := perform(t, h, http.MethodPost, "/user", "", jsonBody(t, map[string]interface{}{
		"username": "alice", "password": "password123", "role": "operator", "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /user: status %d body %s", code, body)
	}
	if gjson.Get(body, "username").String() != "alice" ||
		gjson.Get(body, "role").String() != "operator" ||
		!gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /user: unexpected user %s", body)
	}
	// The password hash must never appear in responses.
	if gjson.Get(body, "passwordHash").Exists() {
		t.Fatalf("POST /user: passwordHash leaked in %s", body)
	}
	uid := gjson.Get(body, "id").String()
	if uid == "" {
		t.Fatalf("POST /user: no id in %s", body)
	}

	// List contains the entry, without hashes.
	code, body = perform(t, h, http.MethodGet, "/user", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /user: status %d body %s", code, body)
	}
	if gjson.Get(body, "users.#").Int() != 1 || gjson.Get(body, "users.0.id").String() != uid {
		t.Fatalf("GET /user: expected the created user, got %s", body)
	}
	if gjson.Get(body, "users.0.passwordHash").Exists() {
		t.Fatalf("GET /user: passwordHash leaked in %s", body)
	}

	// Get by id, without a hash.
	code, body = perform(t, h, http.MethodGet, "/user/"+uid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /user/%s: status %d body %s", uid, code, body)
	}
	if gjson.Get(body, "id").String() != uid {
		t.Fatalf("GET /user/%s: wrong id in %s", uid, body)
	}
	if gjson.Get(body, "passwordHash").Exists() {
		t.Fatalf("GET /user/%s: passwordHash leaked in %s", uid, body)
	}

	// Update via POST /user/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/user/update", "", jsonBody(t, map[string]interface{}{
		"id": uid, "username": "alice2", "role": "admin", "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /user/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "username").String() != "alice2" ||
		gjson.Get(body, "role").String() != "admin" ||
		gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /user/update: unexpected user %s", body)
	}

	// Update via URL id fallback (no id in the body).
	code, body = perform(t, h, http.MethodPost, "/user/"+uid+"/update", "", jsonBody(t, map[string]interface{}{
		"username": "alice3", "role": "operator", "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /user/%s/update: status %d body %s", uid, code, body)
	}
	if gjson.Get(body, "username").String() != "alice3" {
		t.Fatalf("POST /user/%s/update: unexpected user %s", uid, body)
	}

	// Password change: the new password logs in, the old one does not.
	code, body = perform(t, h, http.MethodPost, "/user/"+uid+"/password", "", jsonBody(t, map[string]interface{}{"password": "newpassword456"}))
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /user/%s/password: status %d body %s", uid, code, body)
	}
	code, _ = perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{"username": "alice3", "password": "newpassword456"}))
	if code != http.StatusOK {
		t.Fatalf("login with new password: status %d, want 200", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{"username": "alice3", "password": "password123"}))
	if code != http.StatusUnauthorized {
		t.Fatalf("login with old password: status %d, want 401", code)
	}

	// Enabled toggle locks the account.
	code, body = perform(t, h, http.MethodPost, "/user/"+uid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": false}))
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /user/%s/enabled: status %d body %s", uid, code, body)
	}
	code, _ = perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{"username": "alice3", "password": "newpassword456"}))
	if code != http.StatusUnauthorized {
		t.Fatalf("login while disabled: status %d, want 401", code)
	}

	// Delete, then gone.
	code, body = perform(t, h, http.MethodDelete, "/user/"+uid, "", "")
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("DELETE /user/%s: status %d body %s", uid, code, body)
	}
	code, _ = perform(t, h, http.MethodGet, "/user/"+uid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /user/%s after delete: status %d, want 404", uid, code)
	}

	// Validation errors: short password -> 400, duplicate username -> 409,
	// unknown role -> 400.
	code, _ = perform(t, h, http.MethodPost, "/user", "", jsonBody(t, map[string]interface{}{"username": "bob", "password": "short", "role": "operator"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /user short password: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/user", "", jsonBody(t, map[string]interface{}{"username": "bob", "password": "password123", "role": "operator", "enabled": true}))
	if code != http.StatusOK {
		t.Fatalf("seed user bob: status %d, want 200", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/user", "", jsonBody(t, map[string]interface{}{"username": "bob", "password": "password123", "role": "operator", "enabled": true}))
	if code != http.StatusConflict {
		t.Fatalf("POST /user duplicate username: status %d, want 409", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/user", "", jsonBody(t, map[string]interface{}{"username": "carol", "password": "password123", "role": "superuser", "enabled": true}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /user unknown role: status %d, want 400", code)
	}
}

// ---------------------------------------------------------------------------
// Role-based permissions
// ---------------------------------------------------------------------------

// TestManagementRolePermissions drives the permission matrix through the
// unified ServeHTTP check. Auth is disabled so the legacy pass-through is
// exercised at the same time: without a token the caller is admin, and a
// valid bearer token narrows the role.
func TestManagementRolePermissions(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	seedUser(t, h, "admin", "password123", management.RoleAdmin, true)
	seedUser(t, h, "ops", "password123", management.RoleOperator, true)
	seedUser(t, h, "watch", "password123", management.RoleAuditor, true)
	auditor := login(t, h, "watch", "password123")
	operator := login(t, h, "ops", "password123")
	admin := login(t, h, "admin", "password123")

	// The auditor can read media but not write it.
	code, body := perform(t, h, http.MethodGet, "/media", "Bearer "+auditor, "")
	if code != http.StatusOK {
		t.Fatalf("auditor GET /media: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/media", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"path": "/videos/x.mp4"}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /media: status %d, want 403 (body %s)", code, body)
	}
	if !strings.Contains(body, "permission denied") {
		t.Fatalf("auditor POST /media: unexpected body %q", body)
	}

	// The operator may write media.
	code, body = perform(t, h, http.MethodPost, "/media", "Bearer "+operator, jsonBody(t, map[string]interface{}{"path": "/videos/x.mp4", "name": "x"}))
	if code != http.StatusOK {
		t.Fatalf("operator POST /media: status %d body %s", code, body)
	}
	mediaID := gjson.Get(body, "id").String()

	// The auditor cannot delete media either.
	code, _ = perform(t, h, http.MethodDelete, "/media/"+mediaID, "Bearer "+auditor, "")
	if code != http.StatusForbidden {
		t.Fatalf("auditor DELETE /media: status %d, want 403", code)
	}

	// The auditor may read the audit log but not prune it.
	code, _ = perform(t, h, http.MethodGet, "/audit", "Bearer "+auditor, "")
	if code != http.StatusOK {
		t.Fatalf("auditor GET /audit: status %d, want 200", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/audit/prune", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"maxEntries": 1}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /audit/prune: status %d, want 403", code)
	}

	// The auditor cannot control playback or the scheduler; reads of
	// scheduler state are allowed.
	code, _ = perform(t, h, http.MethodPost, "/player/play", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /player/play: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/scheduler/start", "Bearer "+auditor, "")
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /scheduler/start: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodGet, "/scheduler", "Bearer "+auditor, "")
	if code != http.StatusOK {
		t.Fatalf("auditor GET /scheduler: status %d, want 200", code)
	}

	// User management writes are admin-only; reads follow the matrix
	// (operators are denied, the read-only auditor role is allowed).
	code, _ = perform(t, h, http.MethodGet, "/user", "Bearer "+operator, "")
	if code != http.StatusForbidden {
		t.Fatalf("operator GET /user: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/user", "Bearer "+operator, jsonBody(t, map[string]interface{}{"username": "intruder", "password": "password123", "role": "admin", "enabled": true}))
	if code != http.StatusForbidden {
		t.Fatalf("operator POST /user: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/user", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"username": "intruder", "password": "password123", "role": "admin", "enabled": true}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /user: status %d, want 403", code)
	}
	code, body = perform(t, h, http.MethodGet, "/user", "Bearer "+admin, "")
	if code != http.StatusOK || gjson.Get(body, "users.#").Int() != 3 {
		t.Fatalf("admin GET /user: status %d body %s", code, body)
	}

	// Without a token (auth disabled) the caller is admin: writes pass.
	code, _ = perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": "/videos/z.mp4", "name": "z"}))
	if code != http.StatusOK {
		t.Fatalf("no token with auth disabled: status %d, want 200 admin pass-through", code)
	}

	// An invalid bearer with auth disabled also falls back to the legacy
	// pass-through (admin), never to a lockout.
	code, _ = perform(t, h, http.MethodPost, "/media", "Bearer not-a-session", jsonBody(t, map[string]interface{}{"path": "/videos/y.mp4", "name": "y"}))
	if code != http.StatusOK {
		t.Fatalf("invalid bearer with auth disabled: status %d, want 200 pass-through", code)
	}
}

// TestManagementAuthSessionWithAuthOn covers the auth-enabled gate: the
// legacy fixed token passes as admin, session bearer tokens are accepted
// with their role, and everything else is rejected.
func TestManagementAuthSessionWithAuthOn(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, true, "fixed-token")
	seedUser(t, h, "admin", "password123", management.RoleAdmin, true)
	seedUser(t, h, "watch", "password123", management.RoleAuditor, true)

	// No token with auth enabled -> 401 (fail closed).
	code, body := perform(t, h, http.MethodGet, "/status", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no token with auth on: status %d, want 401 (body %s)", code, body)
	}

	// The legacy fixed token still passes, as admin.
	code, _ = perform(t, h, http.MethodPost, "/media", "fixed-token", jsonBody(t, map[string]interface{}{"path": "/videos/fixed.mp4", "name": "fixed"}))
	if code != http.StatusOK {
		t.Fatalf("fixed token POST /media: status %d, want 200", code)
	}

	// Login works without any token: the credential endpoint is exempt.
	code, body = perform(t, h, http.MethodPost, "/auth/login", "", jsonBody(t, map[string]interface{}{
		"username": "watch", "password": "password123",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /auth/login with auth on: status %d body %s", code, body)
	}
	token := gjson.Get(body, "token").String()
	if token == "" {
		t.Fatalf("POST /auth/login with auth on: no token in %s", body)
	}

	// A session bearer is accepted and its role enforced.
	code, _ = perform(t, h, http.MethodGet, "/media", "Bearer "+token, "")
	if code != http.StatusOK {
		t.Fatalf("auditor bearer GET /media with auth on: status %d, want 200", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/media", "Bearer "+token, jsonBody(t, map[string]interface{}{"path": "/videos/a.mp4"}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor bearer POST /media with auth on: status %d, want 403", code)
	}

	// A stale bearer with auth on is rejected (fail closed).
	code, _ = perform(t, h, http.MethodGet, "/media", "Bearer not-a-session", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer with auth on: status %d, want 401", code)
	}

	// /auth/me works through the session even with auth on.
	code, body = perform(t, h, http.MethodGet, "/auth/me", "Bearer "+token, "")
	if code != http.StatusOK || gjson.Get(body, "role").String() != string(management.RoleAuditor) {
		t.Fatalf("GET /auth/me with auth on: status %d body %s", code, body)
	}
}

// ---------------------------------------------------------------------------
// Media add / list / remove
// ---------------------------------------------------------------------------

func TestManagementMediaAddListRemove(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	id := addMedia(t, h)

	// List contains the entry.
	code, body := perform(t, h, http.MethodGet, "/media", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /media: status %d body %s", code, body)
	}
	if gjson.Get(body, "media.#").Int() != 1 {
		t.Fatalf("GET /media: expected 1 media entry, got %s", body)
	}
	if gjson.Get(body, "media.0.id").String() != id {
		t.Fatalf("GET /media: missing added media %s in %s", id, body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/media/"+id, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /media/%s: status %d body %s", id, code, body)
	}
	if gjson.Get(body, "id").String() != id {
		t.Fatalf("GET /media/%s: wrong id in %s", id, body)
	}

	// Remove.
	code, body = perform(t, h, http.MethodDelete, "/media/"+id, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /media/%s: status %d body %s", id, code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("DELETE /media/%s: missing ok in %s", id, body)
	}

	// Now gone.
	code, _ = perform(t, h, http.MethodGet, "/media/"+id, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /media/%s after delete: status %d, want 404", id, code)
	}
}

func TestManagementMediaAddRejectsEmptyPath(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"name": "no path"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /media empty path: status %d, want 400 (body %s)", code, body)
	}
}

func TestManagementMediaAddDuplicateRejected(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	_ = addMedia(t, h)

	code, _ := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": "/videos/test.mp4"}))
	if code != http.StatusConflict {
		t.Fatalf("POST /media duplicate path: status %d, want 409", code)
	}
}

// ---------------------------------------------------------------------------
// Playlist CRUD
// ---------------------------------------------------------------------------

func TestManagementPlaylistCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// Create.
	code, body := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "main", "desc": "primary", "items": []string{mediaID}, "loop": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /playlist: status %d body %s", code, body)
	}
	plID := gjson.Get(body, "id").String()
	if plID == "" {
		t.Fatalf("POST /playlist: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "main" {
		t.Fatalf("POST /playlist: unexpected name in %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/playlist", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /playlist: status %d body %s", code, body)
	}
	if gjson.Get(body, "playlists.#").Int() != 1 {
		t.Fatalf("GET /playlist: expected 1 playlist, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/playlist/"+plID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /playlist/%s: status %d body %s", plID, code, body)
	}
	if gjson.Get(body, "id").String() != plID {
		t.Fatalf("GET /playlist/%s: wrong id in %s", plID, body)
	}

	// Add item (idempotent no-op here since it already has one).
	code, body = perform(t, h, http.MethodPost, "/playlist/"+plID+"/items", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusOK {
		t.Fatalf("POST /playlist/%s/items: status %d body %s", plID, code, body)
	}

	// Remove item.
	code, body = perform(t, h, http.MethodDelete, "/playlist/"+plID+"/items/"+mediaID, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /playlist/%s/items/%s: status %d body %s", plID, mediaID, code, body)
	}
	if gjson.Get(body, "items.#").Int() != 0 {
		t.Fatalf("DELETE item: expected empty items, got %s", body)
	}

	// Update (PUT).
	code, body = perform(t, h, http.MethodPut, "/playlist/"+plID, "", jsonBody(t, map[string]interface{}{
		"name": "renamed", "desc": "changed", "items": []string{mediaID}, "loop": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("PUT /playlist/%s: status %d body %s", plID, code, body)
	}
	if gjson.Get(body, "name").String() != "renamed" {
		t.Fatalf("PUT /playlist/%s: name not updated in %s", plID, body)
	}

	// Delete.
	code, body = perform(t, h, http.MethodDelete, "/playlist/"+plID, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /playlist/%s: status %d body %s", plID, code, body)
	}

	// Gone.
	code, _ = perform(t, h, http.MethodGet, "/playlist/"+plID, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /playlist/%s after delete: status %d, want 404", plID, code)
	}
}

func TestManagementPlaylistDeleteBlockedByTask(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	_, plBody := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{"name": "used", "items": []string{mediaID}}))
	plID := gjson.Get(plBody, "id").String()

	_, taskBody := perform(t, h, http.MethodPost, "/task", "", jsonBody(t, map[string]interface{}{
		"name": "daily", "type": "interval", "interval": 60, "playlistId": plID,
	}))
	taskID := gjson.Get(taskBody, "id").String()
	if taskID == "" {
		t.Fatalf("failed to create referencing task: %s", taskBody)
	}

	code, _ := perform(t, h, http.MethodDelete, "/playlist/"+plID, "", "")
	if code != http.StatusConflict {
		t.Fatalf("DELETE playlist in use by task: status %d, want 409", code)
	}
}

// ---------------------------------------------------------------------------
// Task CRUD
// ---------------------------------------------------------------------------

func TestManagementTaskCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// Create.
	code, body := perform(t, h, http.MethodPost, "/task", "", jsonBody(t, map[string]interface{}{
		"name": "t1", "type": "interval", "interval": 60, "mediaId": mediaID, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task: status %d body %s", code, body)
	}
	taskID := gjson.Get(body, "id").String()
	if taskID == "" {
		t.Fatalf("POST /task: no id in %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/task", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /task: status %d body %s", code, body)
	}
	if gjson.Get(body, "tasks.#").Int() != 1 {
		t.Fatalf("GET /task: expected 1 task, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /task/%s: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "id").String() != taskID {
		t.Fatalf("GET /task/%s: wrong id in %s", taskID, body)
	}

	// Disable.
	code, body = perform(t, h, http.MethodPost, "/task/"+taskID+"/enabled", "", jsonBody(t, map[string]interface{}{"id": taskID, "enabled": false}))
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/enabled: status %d body %s", taskID, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if gjson.Get(body, "enabled").Bool() {
		t.Fatalf("expected task to be disabled, got %s", body)
	}

	// Replace.
	code, body = perform(t, h, http.MethodPost, "/task/"+taskID+"/replace", "", jsonBody(t, map[string]interface{}{
		"id": taskID, "name": "t2", "type": "interval", "interval": 120, "mediaId": mediaID, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/replace: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "name").String() != "t2" || gjson.Get(body, "interval").Int() != 120 {
		t.Fatalf("POST replace: unexpected task in %s", body)
	}

	// Remove via POST route; an empty body falls back to the URL id.
	code, _ = perform(t, h, http.MethodPost, "/task/"+taskID+"/remove", "", "{}")
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/remove: status %d, want 200", taskID, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /task/%s after remove: status %d, want 404", taskID, code)
	}
}

func TestManagementTaskRejectsNoTarget(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	code, _ := perform(t, h, http.MethodPost, "/task", "", jsonBody(t, map[string]interface{}{"name": "x", "type": "interval", "interval": 10}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /task without target: status %d, want 400", code)
	}
}

// TestManagementTaskSceneTemplateRoundTrip proves sceneTemplateId survives
// task create, replace and GET round-trips, that a missing template reference
// is a 404, and that an empty reference clears the application.
func TestManagementTaskSceneTemplateRoundTrip(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// A scene template to reference.
	_, stBody := perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{
		"name": "watermark", "kind": "watermark",
	}))
	stid := gjson.Get(stBody, "id").String()
	if stid == "" {
		t.Fatalf("POST /scene-template: no id in %s", stBody)
	}

	// Create with a template reference.
	code, body := perform(t, h, http.MethodPost, "/task", "", jsonBody(t, map[string]interface{}{
		"name": "t1", "type": "interval", "interval": 60, "mediaId": mediaID, "sceneTemplateId": stid, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task with sceneTemplateId: status %d body %s", code, body)
	}
	taskID := gjson.Get(body, "id").String()
	if taskID == "" {
		t.Fatalf("POST /task: no id in %s", body)
	}
	if gjson.Get(body, "sceneTemplateId").String() != stid {
		t.Fatalf("POST /task: sceneTemplateId not echoed in %s", body)
	}

	// GET round-trip.
	code, body = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /task/%s: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "sceneTemplateId").String() != stid {
		t.Fatalf("GET /task/%s: sceneTemplateId lost in %s", taskID, body)
	}

	// A missing template reference is a 404.
	code, _ = perform(t, h, http.MethodPost, "/task", "", jsonBody(t, map[string]interface{}{
		"name": "bad", "type": "interval", "interval": 60, "mediaId": mediaID, "sceneTemplateId": "missing-template",
	}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /task with missing sceneTemplateId: status %d, want 404", code)
	}

	// Replace moves the reference to another template.
	_, stBody = perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{
		"name": "clock", "kind": "clock",
	}))
	stid2 := gjson.Get(stBody, "id").String()
	if stid2 == "" {
		t.Fatalf("POST /scene-template: no id in %s", stBody)
	}
	code, body = perform(t, h, http.MethodPost, "/task/"+taskID+"/replace", "", jsonBody(t, map[string]interface{}{
		"id": taskID, "name": "t2", "type": "interval", "interval": 120, "mediaId": mediaID, "sceneTemplateId": stid2, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/replace: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "sceneTemplateId").String() != stid2 {
		t.Fatalf("POST replace: sceneTemplateId not applied in %s", body)
	}

	// A replace referencing a missing template is a 404.
	code, _ = perform(t, h, http.MethodPost, "/task/"+taskID+"/replace", "", jsonBody(t, map[string]interface{}{
		"id": taskID, "name": "t2", "type": "interval", "interval": 120, "mediaId": mediaID, "sceneTemplateId": "missing-template",
	}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /task/%s/replace with missing template: status %d, want 404", taskID, code)
	}

	// Replace with an empty reference clears it.
	code, body = perform(t, h, http.MethodPost, "/task/"+taskID+"/replace", "", jsonBody(t, map[string]interface{}{
		"id": taskID, "name": "t3", "type": "interval", "interval": 120, "mediaId": mediaID, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/replace clearing: status %d body %s", taskID, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /task/%s after clear: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "sceneTemplateId").String() != "" {
		t.Fatalf("GET /task/%s: sceneTemplateId not cleared in %s", taskID, body)
	}
}

// ---------------------------------------------------------------------------
// Alarm raise / resolve
// ---------------------------------------------------------------------------

func TestManagementAlarmRaiseResolve(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Raise two distinct alarms.
	code, body := perform(t, h, http.MethodPost, "/alarm/raise", "", jsonBody(t, map[string]interface{}{
		"level": "warning", "title": "disk", "message": "disk nearly full",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /alarm/raise: status %d body %s", code, body)
	}
	alarm1 := gjson.Get(body, "id").String()
	if alarm1 == "" || gjson.Get(body, "status").String() != "active" {
		t.Fatalf("POST /alarm/raise: unexpected alarm %s", body)
	}

	_, body = perform(t, h, http.MethodPost, "/alarm/raise", "", jsonBody(t, map[string]interface{}{
		"level": "critical", "title": "core", "message": "core crashed",
	}))
	alarm2 := gjson.Get(body, "id").String()
	if alarm2 == "" {
		t.Fatalf("POST /alarm/raise: no second alarm id in %s", body)
	}

	// List shows newest first (2 entries).
	code, body = perform(t, h, http.MethodGet, "/alarm", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /alarm: status %d body %s", code, body)
	}
	if gjson.Get(body, "alarms.#").Int() != 2 {
		t.Fatalf("GET /alarm: expected 2 alarms, got %s", body)
	}

	// Resolve one.
	code, body = perform(t, h, http.MethodPost, "/alarm/resolve", "", jsonBody(t, map[string]interface{}{"id": alarm1}))
	if code != http.StatusOK {
		t.Fatalf("POST /alarm/resolve: status %d body %s", code, body)
	}
	if gjson.Get(body, "status").String() != "resolved" {
		t.Fatalf("POST /alarm/resolve: expected resolved, got %s", body)
	}

	// Resolve-all clears the remaining active one.
	code, body = perform(t, h, http.MethodPost, "/alarm/resolve-all", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /alarm/resolve-all: status %d body %s", code, body)
	}
	if gjson.Get(body, "resolved").Int() != 1 {
		t.Fatalf("POST /alarm/resolve-all: expected 1 resolved, got %s", body)
	}

	// Delete the first alarm.
	code, body = perform(t, h, http.MethodDelete, "/alarm/"+alarm1, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /alarm/%s: status %d body %s", alarm1, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/alarm", "", "")
	if gjson.Get(body, "alarms.#").Int() != 1 {
		t.Fatalf("DELETE /alarm/%s: expected 1 remaining, got %s", alarm1, body)
	}
}

func TestManagementAlarmRaiseRejectsEmptyTitle(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	code, _ := perform(t, h, http.MethodPost, "/alarm/raise", "", jsonBody(t, map[string]interface{}{"level": "warning", "title": "", "message": "x"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /alarm/raise empty title: status %d, want 400", code)
	}
}

// ---------------------------------------------------------------------------
// Scheduler start / stop
// ---------------------------------------------------------------------------

func TestManagementSchedulerStartStop(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Initially stopped.
	code, body := perform(t, h, http.MethodGet, "/scheduler", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /scheduler: status %d body %s", code, body)
	}
	if gjson.Get(body, "running").Bool() {
		t.Fatalf("GET /scheduler: expected not running, got %s", body)
	}

	// Start.
	code, body = perform(t, h, http.MethodPost, "/scheduler/start", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /scheduler/start: status %d body %s", code, body)
	}
	if !gjson.Get(body, "running").Bool() {
		t.Fatalf("POST /scheduler/start: expected running, got %s", body)
	}

	// Double start -> 409.
	code, _ = perform(t, h, http.MethodPost, "/scheduler/start", "", "")
	if code != http.StatusConflict {
		t.Fatalf("POST /scheduler/start again: status %d, want 409", code)
	}

	// Stop.
	code, body = perform(t, h, http.MethodPost, "/scheduler/stop", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /scheduler/stop: status %d body %s", code, body)
	}
	if gjson.Get(body, "running").Bool() {
		t.Fatalf("POST /scheduler/stop: expected stopped, got %s", body)
	}

	// Restart works after stop.
	code, _ = perform(t, h, http.MethodPost, "/scheduler/start", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /scheduler/start after stop: status %d, want 200", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/scheduler/stop", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /scheduler/stop after restart: status %d, want 200", code)
	}
}

// ---------------------------------------------------------------------------
// Status must not hang when a provider blocks
// ---------------------------------------------------------------------------

func TestManagementStatusDoesNotHangOnBlockingProvider(t *testing.T) {
	// The play provider sleeps far longer than the 2s per-read timeout used
	// by providerStatus/callWithTimeout. The endpoint must still return.
	play := &fakePlayProvider{block: 60 * time.Second}
	resource := &fakeResourceProvider{}
	output := &fakeOutputProvider{}
	h := newTestManagementHandler(t, play, resource, output, false, "")

	start := time.Now()
	code, body := performWithin(t, h, http.MethodGet, "/status", "", 12*time.Second)
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("GET /status with blocking provider: status %d body %s", code, body)
	}
	if !gjson.Get(body, "scheduler").IsObject() {
		t.Fatalf("GET /status: missing scheduler key in %s", body)
	}
	if elapsed >= 12*time.Second {
		t.Fatalf("GET /status took too long (%s); expected to be bounded by the per-call timeout", elapsed)
	}
	t.Logf("status returned in %s despite a blocking provider", elapsed)
}

func TestManagementStatusFastProviders(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	code, body := perform(t, h, http.MethodGet, "/status", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /status: status %d body %s", code, body)
	}
	if !gjson.Get(body, "scheduler").IsObject() {
		t.Fatalf("GET /status: missing scheduler key in %s", body)
	}
}

// ---------------------------------------------------------------------------
// apiRouter /console route dispatch
// ---------------------------------------------------------------------------

// markerHandler is a tiny http.Handler that writes a fixed status and body.
type markerHandler struct {
	status int
	body   string
}

func (m markerHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(m.status)
	_, _ = io.WriteString(w, m.body)
}

func TestAPIRouterDispatch(t *testing.T) {
	gateway := markerHandler{status: http.StatusOK, body: "gateway"}
	console := markerHandler{status: http.StatusOK, body: "console"}
	mgmt := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	router := &apiRouter{gateway: gateway, management: mgmt, console: console}

	cases := []struct {
		path string
		// wantBody matches the raw body for console/gateway stubs.
		wantBody string
		// wantSubstring matches the real management handler's JSON.
		wantSubstring string
	}{
		{path: "/console/", wantBody: "console"},
		{path: "/console/app.js", wantBody: "console"},
		{path: "/console/api/play/duration", wantBody: "console"},
		{path: "/play", wantBody: "gateway"},
		{path: "/resource/current", wantBody: "gateway"},
		{path: "/v1/operations/name", wantBody: "gateway"},
		{path: "/status", wantSubstring: "scheduler"},
		{path: "/user", wantSubstring: "users"},
		{path: "/media", wantSubstring: "media"},
		{path: "/playlist", wantSubstring: "playlists"},
		{path: "/task", wantSubstring: "tasks"},
		{path: "/alarm", wantSubstring: "alarms"},
		{path: "/failover", wantSubstring: "failovers"},
		{path: "/health-policy", wantSubstring: "healthPolicies"},
		{path: "/cache-task", wantSubstring: "cacheTasks"},
		{path: "/scene-template", wantSubstring: "sceneTemplates"},
		{path: "/node", wantSubstring: "nodes"},
		{path: "/instance", wantSubstring: "instances"},
		{path: "/remote-command", wantSubstring: "commands"},
		{path: "/config-snapshot", wantSubstring: "snapshots"},
		{path: "/config-template", wantSubstring: "templates"},
		{path: "/industry-template", wantSubstring: "templates"},
		{path: "/smart-rule", wantSubstring: "rules"},
		{path: "/suggestion", wantSubstring: "suggestions"},
		{path: "/metrics/summary", wantSubstring: "summary"},
	}

	for _, tc := range cases {
		code, body := perform(t, router, http.MethodGet, tc.path, "", "")
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d body %s", tc.path, code, body)
		}
		if tc.wantBody != "" {
			if body != tc.wantBody {
				t.Fatalf("GET %s: body %q, want %q", tc.path, body, tc.wantBody)
			}
		}
		if tc.wantSubstring != "" && !strings.Contains(body, tc.wantSubstring) {
			t.Fatalf("GET %s: body %q missing %q", tc.path, body, tc.wantSubstring)
		}
	}
}

func TestIsManagementPath(t *testing.T) {
	cases := map[string]bool{
		"/status":                 true,
		"/auth":                   true,
		"/auth/login":             true,
		"/auth/logout":            true,
		"/auth/me":                true,
		"/user":                   true,
		"/user/abc":               true,
		"/media":                  true,
		"/media/abc":              true,
		"/playlist":               true,
		"/playlist/abc/items":     true,
		"/task":                   true,
		"/alarm":                  true,
		"/scheduler":              true,
		"/player":                 true,
		"/output-group":           true,
		"/output-group/abc":       true,
		"/failover":               true,
		"/failover/abc":           true,
		"/health-policy":          true,
		"/health-policy/abc":      true,
		"/cache-task":             true,
		"/cache-task/abc":         true,
		"/scene-template":         true,
		"/scene-template/abc":     true,
		"/webhook":                true,
		"/webhook/abc":            true,
		"/webhook/abc/deliveries": true,
		"/audit":                  true,
		"/audit/prune":            true,
		"/node":                   true,
		"/node/abc":               true,
		"/instance":               true,
		"/instance/abc":           true,
		"/remote-command":         true,
		"/remote-command/abc":     true,
		"/config-snapshot":        true,
		"/config-snapshot/abc":    true,
		"/config-template":        true,
		"/config-template/abc":    true,
		"/industry-template":      true,
		"/industry-template/abc":  true,
		"/smart-rule":             true,
		"/smart-rule/abc":         true,
		"/suggestion":             true,
		"/suggestion/abc":         true,
		"/metrics":                true,
		"/metrics/failure-rate":   true,
		"/play":                   false,
		"/play/duration":          false,
		"/resource/current":       false,
		"/console":                false,
		"/console/app.js":         false,
		"/v1/operations/name":     false,
		"/websocket":              false,
	}
	for path, want := range cases {
		if got := isManagementPath(path); got != want {
			t.Fatalf("isManagementPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Webhook subscription CRUD and delivery history
// ---------------------------------------------------------------------------

func TestManagementWebhookCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create.
	code, body := perform(t, h, http.MethodPost, "/webhook", "", jsonBody(t, map[string]interface{}{
		"name": "ops", "url": "https://example.com/hook", "events": []string{"task_completed", "material_failed"}, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /webhook: status %d body %s", code, body)
	}
	id := gjson.Get(body, "id").String()
	if id == "" {
		t.Fatalf("POST /webhook: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "ops" {
		t.Fatalf("POST /webhook: unexpected name in %s", body)
	}
	if gjson.Get(body, "enabled").Bool() != true {
		t.Fatalf("POST /webhook: unexpected enabled in %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/webhook/"+id, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /webhook/%s: status %d body %s", id, code, body)
	}
	if gjson.Get(body, "url").String() != "https://example.com/hook" {
		t.Fatalf("GET /webhook/%s: wrong url in %s", id, body)
	}

	// Update via /webhook/{id}/update.
	code, body = perform(t, h, http.MethodPost, "/webhook/"+id+"/update", "", jsonBody(t, map[string]interface{}{
		"name": "ops2", "url": "https://example.com/hook2", "events": []string{"output_disconnected"}, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /webhook/%s/update: status %d body %s", id, code, body)
	}
	if gjson.Get(body, "name").String() != "ops2" {
		t.Fatalf("POST /webhook/%s/update: unexpected name in %s", id, body)
	}

	// Update via /webhook/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/webhook/update", "", jsonBody(t, map[string]interface{}{
		"id": id, "name": "ops3", "url": "https://example.com/hook3", "events": []string{"task_completed"}, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /webhook/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "ops3" {
		t.Fatalf("POST /webhook/update: unexpected name in %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/webhook", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /webhook: status %d body %s", code, body)
	}
	if gjson.Get(body, "webhooks.#").Int() != 1 {
		t.Fatalf("GET /webhook: expected 1 subscription, got %s", body)
	}

	// Toggle enabled off.
	code, body = perform(t, h, http.MethodPost, "/webhook/"+id+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": false}))
	if code != http.StatusOK {
		t.Fatalf("POST /webhook/%s/enabled: status %d body %s", id, code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /webhook/%s/enabled: missing ok in %s", id, body)
	}
	code, body = perform(t, h, http.MethodGet, "/webhook/"+id, "", "")
	if gjson.Get(body, "enabled").Bool() {
		t.Fatalf("GET /webhook/%s after disable: still enabled in %s", id, body)
	}

	// Delivery history is empty for a fresh subscription.
	code, body = perform(t, h, http.MethodGet, "/webhook/"+id+"/deliveries", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /webhook/%s/deliveries: status %d body %s", id, code, body)
	}
	if gjson.Get(body, "deliveries.#").Int() != 0 {
		t.Fatalf("GET /webhook/%s/deliveries: expected 0 records, got %s", id, body)
	}

	// Delete.
	code, body = perform(t, h, http.MethodDelete, "/webhook/"+id, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /webhook/%s: status %d body %s", id, code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("DELETE /webhook/%s: missing ok in %s", id, body)
	}

	// Now gone.
	code, _ = perform(t, h, http.MethodGet, "/webhook/"+id, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /webhook/%s after delete: status %d, want 404", id, code)
	}
}

func TestManagementWebhookErrors(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Empty name -> 400.
	code, body := perform(t, h, http.MethodPost, "/webhook", "", jsonBody(t, map[string]interface{}{
		"name": " ", "url": "https://example.com/hook", "events": []string{"task_completed"},
	}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /webhook empty name: status %d, want 400 (body %s)", code, body)
	}

	// Non-http(s) URL -> 400.
	code, body = perform(t, h, http.MethodPost, "/webhook", "", jsonBody(t, map[string]interface{}{
		"name": "bad url", "url": "ftp://example.com/hook", "events": []string{"task_completed"},
	}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /webhook bad url: status %d, want 400 (body %s)", code, body)
	}

	// Duplicate name -> 409.
	code, body = perform(t, h, http.MethodPost, "/webhook", "", jsonBody(t, map[string]interface{}{
		"name": "dup", "url": "https://example.com/a", "events": []string{"task_completed"},
	}))
	if code != http.StatusOK {
		t.Fatalf("seed webhook: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/webhook", "", jsonBody(t, map[string]interface{}{
		"name": "dup", "url": "https://example.com/b", "events": []string{"task_completed"},
	}))
	if code != http.StatusConflict {
		t.Fatalf("POST /webhook duplicate: status %d, want 409 (body %s)", code, body)
	}

	// Unknown id -> 404.
	code, body = perform(t, h, http.MethodGet, "/webhook/no-such-id", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /webhook/no-such-id: status %d, want 404 (body %s)", code, body)
	}
}

// ---------------------------------------------------------------------------
// Audit log list / filter / prune
// ---------------------------------------------------------------------------

func TestManagementAuditListFilterPrune(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Seed the audit log directly through the service, as the operations
	// console would.
	seed := func(operator, action string, result management.AuditResult) {
		t.Helper()
		if _, err := h.audit.Record(management.AuditEntry{Operator: operator, Action: action, Result: result}); err != nil {
			t.Fatalf("record audit entry: %v", err)
		}
	}
	seed("alice", "media.add", management.AuditSuccess)
	seed("alice", "webhook.update", management.AuditFailure)
	seed("bob", "media.add", management.AuditSuccess)

	// List round trip.
	code, body := perform(t, h, http.MethodGet, "/audit", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /audit: status %d body %s", code, body)
	}
	if gjson.Get(body, "audit.#").Int() != 3 {
		t.Fatalf("GET /audit: expected 3 entries, got %s", body)
	}

	// Filter by operator.
	code, body = perform(t, h, http.MethodGet, "/audit?operator=alice", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /audit?operator=alice: status %d body %s", code, body)
	}
	if gjson.Get(body, "audit.#").Int() != 2 {
		t.Fatalf("GET /audit?operator=alice: expected 2 entries, got %s", body)
	}

	// Filter by action and result combined.
	code, body = perform(t, h, http.MethodGet, "/audit?action=media.add&result=success", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /audit filtered: status %d body %s", code, body)
	}
	if gjson.Get(body, "audit.#").Int() != 2 {
		t.Fatalf("GET /audit filtered: expected 2 entries, got %s", body)
	}

	// No match -> empty list.
	code, body = perform(t, h, http.MethodGet, "/audit?operator=nobody", "", "")
	if gjson.Get(body, "audit.#").Int() != 0 {
		t.Fatalf("GET /audit?operator=nobody: expected 0 entries, got %s", body)
	}

	// Prune to the newest 2 entries.
	code, body = perform(t, h, http.MethodPost, "/audit/prune", "", jsonBody(t, map[string]interface{}{"maxEntries": 2}))
	if code != http.StatusOK {
		t.Fatalf("POST /audit/prune: status %d body %s", code, body)
	}
	if gjson.Get(body, "removed").Int() != 1 {
		t.Fatalf("POST /audit/prune: expected 1 removed, got %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/audit", "", "")
	if gjson.Get(body, "audit.#").Int() != 2 {
		t.Fatalf("GET /audit after prune: expected 2 entries, got %s", body)
	}
}

// ---------------------------------------------------------------------------
// Event-source wiring (static check)
// ---------------------------------------------------------------------------

// The webhook dispatcher itself (retries, delivery recording, history
// rollover) is unit-tested in the management package. The server layer only
// wires it to the scheduler and failover error paths inside
// newManagementHandler (EventMaterialFailed / EventOutputDisconnected) and
// raises "Webhook delivery failed" alarms through WithWebhookErrorHandler;
// that constructor cannot run under test (it pins management.json), so its
// wiring is verified by reading the code. This test pins the handler fields
// so the wiring cannot silently disappear from the test constructor either.
func TestManagementEventSourceWiring(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	if h.webhooks == nil {
		t.Fatal("webhooks service not wired")
	}
	if h.audit == nil {
		t.Fatal("audit service not wired")
	}
	if h.users == nil {
		t.Fatal("users service not wired")
	}
	if h.sessions == nil {
		t.Fatal("sessions service not wired")
	}
	if h.auth == nil {
		t.Fatal("auth service not wired")
	}
	if h.webhookDispatcher == nil {
		t.Fatal("webhook dispatcher not wired")
	}
	if h.nodes == nil {
		t.Fatal("nodes service not wired")
	}
	if h.instances == nil {
		t.Fatal("instances service not wired")
	}
	if h.remoteCommands == nil {
		t.Fatal("remote commands service not wired")
	}
	if h.configSnapshots == nil {
		t.Fatal("config snapshots service not wired")
	}
	if h.configTemplates == nil {
		t.Fatal("config templates service not wired")
	}
	if h.industryTemplates == nil {
		t.Fatal("industry templates service not wired")
	}
	if h.smartRules == nil {
		t.Fatal("smart rules service not wired")
	}
	if h.metrics == nil {
		t.Fatal("metrics service not wired")
	}
	if h.suggestions == nil {
		t.Fatal("suggestions service not wired")
	}
	if h.playEvents == nil {
		t.Fatal("play events service not wired")
	}
}

// TestManagementPlayEventBridge proves the scheduler -> playback log bridge:
// a fired interval task must land in h.playEvents, which backs the metrics
// and recommendation endpoints. The test constructor carries the same
// WithPlayEventHandler wiring as production, so this exercises the bridge
// end to end: fire -> event -> Record -> List.
func TestManagementPlayEventBridge(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	task, err := h.tasks.Create(management.TaskSpec{
		Name: "bridge-interval", Type: management.TaskTypeInterval,
		Interval: 1, MediaID: mediaID, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create interval task: %v", err)
	}
	if task == nil || task.ID == "" {
		t.Fatal("create interval task: no task id")
	}

	if err := h.scheduler.Start(); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	defer h.scheduler.Stop()

	// The fire is recorded on a scheduler goroutine, so poll for the event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events := h.playEvents.List(0)
		if len(events) == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if events[0].Result != management.PlaySuccess {
			t.Fatalf("play event result = %q, want %q (event %+v)", events[0].Result, management.PlaySuccess, events[0])
		}
		if events[0].MediaID != mediaID {
			t.Fatalf("play event mediaId = %q, want %q", events[0].MediaID, mediaID)
		}
		return
	}
	t.Fatal("no play event recorded within 2s")
}

// ---------------------------------------------------------------------------
// Auth fail-closed / task route id fallback / playlist full playback
// ---------------------------------------------------------------------------

// TestManagementAuthFailsClosedOnEmptyToken proves that enabling auth with an
// empty configured token still rejects requests, so the door can never be
// silently left open (matching authAllowed's fail-closed semantics).
func TestManagementAuthFailsClosedOnEmptyToken(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, true, "")

	code, body := perform(t, h, http.MethodGet, "/status", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("auth on with empty token: status %d, want 401 (body %s)", code, body)
	}
	if !strings.Contains(body, "authentication failed") {
		t.Fatalf("auth on with empty token: unexpected body %q", body)
	}
}

// TestManagementTaskRouteIDFallback proves that POST /task/{id}/replace and
// POST /task/{id}/enabled fall back to the URL id when the request body omits
// the id, mirroring the /task/{id}/remove and /alarm/resolve behaviour.
func TestManagementTaskRouteIDFallback(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	_, body := perform(t, h, http.MethodPost, "/task", "", jsonBody(t, map[string]interface{}{
		"name": "t1", "type": "interval", "interval": 60, "mediaId": mediaID, "enabled": true,
	}))
	taskID := gjson.Get(body, "id").String()
	if taskID == "" {
		t.Fatalf("POST /task: no id in %s", body)
	}

	// Replace with no id in the body: the URL id must win.
	code, body := perform(t, h, http.MethodPost, "/task/"+taskID+"/replace", "", jsonBody(t, map[string]interface{}{
		"name": "renamed", "type": "interval", "interval": 120, "mediaId": mediaID, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/replace without body id: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "id").String() != taskID {
		t.Fatalf("POST replace without body id: expected id %s, got %s", taskID, body)
	}
	if gjson.Get(body, "name").String() != "renamed" {
		t.Fatalf("POST replace without body id: name not updated in %s", body)
	}

	// Disable with no id in the body: the URL id must win.
	code, body = perform(t, h, http.MethodPost, "/task/"+taskID+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": false}))
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/enabled without body id: status %d body %s", taskID, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /task/%s: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "enabled").Bool() {
		t.Fatalf("expected task %s to be disabled, got %s", taskID, body)
	}
}

// TestManagementPlayerPlayPlaylistFullExpansion proves that playing a playlist
// adds every resolved item as a resource and then seeks to the first item to
// start playback, rather than only playing the first entry.
func TestManagementPlayerPlayPlaylistFullExpansion(t *testing.T) {
	resource := &fakeResourceProvider{}
	h := newTestManagementHandler(t, &fakePlayProvider{}, resource, &fakeOutputProvider{}, false, "")

	addPath := func(path, name string) string {
		code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": path, "name": name}))
		if code != http.StatusOK {
			t.Fatalf("POST /media %s: status %d body %s", path, code, body)
		}
		id := gjson.Get(body, "id").String()
		if id == "" {
			t.Fatalf("POST /media %s: no id in %s", path, body)
		}
		return id
	}
	mediaA := addPath("/videos/a.mp4", "a")
	mediaB := addPath("/videos/b.mp4", "b")
	mediaC := addPath("/videos/c.mp4", "c")

	_, plBody := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "program", "items": []string{mediaA, mediaB, mediaC}, "loop": true,
	}))
	plID := gjson.Get(plBody, "id").String()
	if plID == "" {
		t.Fatalf("POST /playlist: no id in %s", plBody)
	}

	code, body := perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"playlistId": plID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play playlist: status %d body %s", code, body)
	}

	// Every resolved playlist item must be added, in order.
	if len(resource.addCalls) != 3 {
		t.Fatalf("ResourceAdd called %d times, want 3 (all playlist items)", len(resource.addCalls))
	}
	wantIDs := []string{mediaA, mediaB, mediaC}
	for i, call := range resource.addCalls {
		if call.Unique != wantIDs[i] {
			t.Fatalf("ResourceAdd[%d].Unique = %q, want %q", i, call.Unique, wantIDs[i])
		}
		if call.Path == "" {
			t.Fatalf("ResourceAdd[%d] missing Path", i)
		}
	}

	// A single seek to the first item starts playback.
	if len(resource.seekCalls) != 1 {
		t.Fatalf("ResourceSeek called %d times, want 1", len(resource.seekCalls))
	}
	if resource.seekCalls[0].Unique != mediaA {
		t.Fatalf("ResourceSeek.Unique = %q, want first item %q", resource.seekCalls[0].Unique, mediaA)
	}
	if resource.seekCalls[0].Seek != 0 {
		t.Fatalf("ResourceSeek.Seek = %d, want 0", resource.seekCalls[0].Seek)
	}
}

// TestManagementPlayerPlayFallsBackToBackupPlaylist proves that playing an
// empty main playlist automatically switches to its configured backup
// programme: the backup's media is what gets added to the resource queue and
// seeked, so the playback path itself uses the fallback chain.
func TestManagementPlayerPlayFallsBackToBackupPlaylist(t *testing.T) {
	resource := &fakeResourceProvider{}
	h := newTestManagementHandler(t, &fakePlayProvider{}, resource, &fakeOutputProvider{}, false, "")

	addPath := func(path, name string) string {
		code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": path, "name": name}))
		if code != http.StatusOK {
			t.Fatalf("POST /media %s: status %d body %s", path, code, body)
		}
		id := gjson.Get(body, "id").String()
		if id == "" {
			t.Fatalf("POST /media %s: no id in %s", path, body)
		}
		return id
	}
	backupMedia := addPath("/videos/backup.mp4", "backup")

	_, fbBody := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "backup", "items": []string{backupMedia},
	}))
	fbID := gjson.Get(fbBody, "id").String()
	if fbID == "" {
		t.Fatalf("POST /playlist backup: no id in %s", fbBody)
	}

	// The main programme is created empty with the backup referenced.
	_, mainBody := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "main", "items": []string{}, "fallbackPlaylistId": fbID,
	}))
	mainID := gjson.Get(mainBody, "id").String()
	if mainID == "" {
		t.Fatalf("POST /playlist main: no id in %s", mainBody)
	}

	code, body := perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"playlistId": mainID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play with fallback: status %d body %s", code, body)
	}

	// The backup programme's media is expanded, not the empty main one.
	if len(resource.addCalls) != 1 {
		t.Fatalf("ResourceAdd called %d times, want 1 (backup media)", len(resource.addCalls))
	}
	if resource.addCalls[0].Unique != backupMedia || resource.addCalls[0].Path == "" {
		t.Fatalf("ResourceAdd = %+v, want backup media %q", resource.addCalls[0], backupMedia)
	}
	if len(resource.seekCalls) != 1 || resource.seekCalls[0].Unique != backupMedia || resource.seekCalls[0].Seek != 0 {
		t.Fatalf("ResourceSeek = %+v, want a single seek to backup media %q", resource.seekCalls, backupMedia)
	}
}

// TestManagementPlayerPlayFallsBackOnMissingMediaReference covers the other
// fallback trigger: a main programme whose media references are broken. The
// API rejects creating such a playlist (a missing media reference is a 404),
// so the corruption can only arrive through direct data manipulation; the
// broken reference is injected below, mirroring the management package's own
// tests, and the play path must still switch to the backup programme.
func TestManagementPlayerPlayFallsBackOnMissingMediaReference(t *testing.T) {
	resource := &fakeResourceProvider{}
	h := newTestManagementHandler(t, &fakePlayProvider{}, resource, &fakeOutputProvider{}, false, "")

	// A missing media reference cannot be created through the API.
	code, body := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "broken", "items": []string{"missing-media"},
	}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /playlist with missing media: status %d, want 404 (body %s)", code, body)
	}

	backupMedia := addMedia(t, h)
	_, fbBody := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "backup", "items": []string{backupMedia},
	}))
	fbID := gjson.Get(fbBody, "id").String()
	if fbID == "" {
		t.Fatalf("POST /playlist backup: no id in %s", fbBody)
	}

	_, mainBody := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "main", "fallbackPlaylistId": fbID,
	}))
	mainID := gjson.Get(mainBody, "id").String()
	if mainID == "" {
		t.Fatalf("POST /playlist main: no id in %s", mainBody)
	}

	// Inject the dangling reference directly into the store: the services
	// never create one, so this is the only way the corruption can exist.
	if err := h.store.Update(func(d *management.Data) error {
		for _, p := range d.Playlists {
			if p.ID == mainID {
				p.Items = append(p.Items, &management.PlaylistItem{MediaID: "missing-media"})
				return nil
			}
		}
		t.Fatal("main playlist not found")
		return nil
	}); err != nil {
		t.Fatalf("inject broken reference: %v", err)
	}

	code, body = perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{"playlistId": mainID}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play broken main: status %d body %s", code, body)
	}
	if len(resource.addCalls) != 1 || resource.addCalls[0].Unique != backupMedia {
		t.Fatalf("ResourceAdd = %+v, want backup media %q", resource.addCalls, backupMedia)
	}
	if len(resource.seekCalls) != 1 || resource.seekCalls[0].Unique != backupMedia {
		t.Fatalf("ResourceSeek = %+v, want a single seek to backup media %q", resource.seekCalls, backupMedia)
	}
}

// TestManagementPlayerPlayWithSceneTemplate proves POST /player/play accepts
// a sceneTemplateId: the request is accepted (200) and the play itself is
// unaffected, because the field is advisory metadata the adapter does not
// consume (scene rendering is core work outside this layer).
func TestManagementPlayerPlayWithSceneTemplate(t *testing.T) {
	resource := &fakeResourceProvider{}
	h := newTestManagementHandler(t, &fakePlayProvider{}, resource, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	_, stBody := perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{
		"name": "watermark", "kind": "watermark",
	}))
	stid := gjson.Get(stBody, "id").String()
	if stid == "" {
		t.Fatalf("POST /scene-template: no id in %s", stBody)
	}

	code, body := perform(t, h, http.MethodPost, "/player/play", "", jsonBody(t, map[string]interface{}{
		"mediaId": mediaID, "sceneTemplateId": stid,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/play with sceneTemplateId: status %d body %s", code, body)
	}

	// The play itself is unchanged: the media is added and seeked as usual.
	if len(resource.addCalls) != 1 || resource.addCalls[0].Unique != mediaID {
		t.Fatalf("ResourceAdd = %+v, want media %q", resource.addCalls, mediaID)
	}
	if len(resource.seekCalls) != 1 || resource.seekCalls[0].Unique != mediaID || resource.seekCalls[0].Seek != 0 {
		t.Fatalf("ResourceSeek = %+v, want a single seek to media %q", resource.seekCalls, mediaID)
	}
}

// waitSeekCount polls the fake resource provider until it has recorded at
// least n seek calls, failing the test after the deadline. Needed because the
// scheduler dispatches interrupt playback on a goroutine, so the provider
// calls land after the HTTP response.
func waitSeekCount(t *testing.T, resource *fakeResourceProvider, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(resource.seekSnapshot()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected at least %d seek calls, got %v", n, resource.seekSnapshot())
}

// TestManagementPlayerInterrupt covers POST /player/interrupt: a stopped
// scheduler is rejected (the interrupt lifecycle is tick driven), an empty
// target is a 400, and a running scheduler dispatches the interrupt through
// the player adapter with the duration plumbed to the scheduler. The seek
// sequence [media, playlist, media] proves the timed interrupt preempted the
// first play and restored it after the interrupt ended — which only happens
// when the duration was passed through as a timed interrupt.
func TestManagementPlayerInterrupt(t *testing.T) {
	resource := &fakeResourceProvider{seekBlock: make(chan struct{})}
	h := newTestManagementHandler(t, &fakePlayProvider{}, resource, &fakeOutputProvider{}, false, "")

	addPath := func(path, name string) string {
		code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": path, "name": name}))
		if code != http.StatusOK {
			t.Fatalf("POST /media %s: status %d body %s", path, code, body)
		}
		id := gjson.Get(body, "id").String()
		if id == "" {
			t.Fatalf("POST /media %s: no id in %s", path, body)
		}
		return id
	}
	mediaA := addPath("/videos/a.mp4", "a")
	mediaB := addPath("/videos/b.mp4", "b")

	_, plBody := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "interrupt program", "items": []string{mediaB},
	}))
	plID := gjson.Get(plBody, "id").String()
	if plID == "" {
		t.Fatalf("POST /playlist: no id in %s", plBody)
	}

	// The test handler's scheduler is never started: interrupt expiry is
	// driven by its runtime loop, so a stopped scheduler must reject the
	// request rather than dispatch a play that can never end. The
	// precondition failure is reported as 409 Conflict (not 500), so the
	// console can surface it as a recoverable state instead of a server
	// error.
	code, body := perform(t, h, http.MethodPost, "/player/interrupt", "", jsonBody(t, map[string]interface{}{"mediaId": mediaA}))
	if code != http.StatusConflict {
		t.Fatalf("POST /player/interrupt while stopped: status %d, want 409 (body %s)", code, body)
	}
	if !strings.Contains(body, "not running") {
		t.Fatalf("POST /player/interrupt while stopped: unexpected body %q", body)
	}

	// An interrupt without a target is a client error.
	code, _ = perform(t, h, http.MethodPost, "/player/interrupt", "", jsonBody(t, map[string]interface{}{}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /player/interrupt without target: status %d, want 400", code)
	}

	code, _ = perform(t, h, http.MethodPost, "/scheduler/start", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /scheduler/start: status %d, want 200", code)
	}
	defer func() { _, _ = perform(t, h, http.MethodPost, "/scheduler/stop", "", "") }()

	// One-shot interrupt by media id: the target starts playing and stays in
	// flight (ResourceSeek blocks until released below).
	code, body = perform(t, h, http.MethodPost, "/player/interrupt", "", jsonBody(t, map[string]interface{}{"mediaId": mediaA}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/interrupt media: status %d body %s", code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /player/interrupt: missing ok in %s", body)
	}
	waitSeekCount(t, resource, 1, 2*time.Second)

	// A timed interrupt by playlist id (duration 60) preempts the media play
	// and remembers it as the resume target.
	code, body = perform(t, h, http.MethodPost, "/player/interrupt", "", jsonBody(t, map[string]interface{}{
		"playlistId": plID, "duration": 60,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /player/interrupt playlist: status %d body %s", code, body)
	}
	waitSeekCount(t, resource, 2, 2*time.Second)

	// Releasing the blocked seeks lets both plays finish; the timed
	// interrupt then restores the pre-interrupt target (a third play),
	// proving the duration reached the scheduler.
	close(resource.seekBlock)
	waitSeekCount(t, resource, 3, 2*time.Second)
	seeks := resource.seekSnapshot()
	if len(seeks) != 3 || seeks[0].Unique != mediaA || seeks[1].Unique != mediaB || seeks[2].Unique != mediaA {
		t.Fatalf("unexpected seek sequence: %+v", seeks)
	}
}

// TestManagementTaskPriorityInterruptRoundTrip proves that priority,
// interrupt and interruptDuration survive task create, replace and GET
// round-trips.
func TestManagementTaskPriorityInterruptRoundTrip(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// Create with all interrupt fields set.
	code, body := perform(t, h, http.MethodPost, "/task", "", jsonBody(t, map[string]interface{}{
		"name": "int", "type": "interval", "interval": 60, "mediaId": mediaID,
		"priority": "critical", "interrupt": true, "interruptDuration": 30, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task: status %d body %s", code, body)
	}
	taskID := gjson.Get(body, "id").String()
	if taskID == "" {
		t.Fatalf("POST /task: no id in %s", body)
	}
	if gjson.Get(body, "priority").String() != "critical" ||
		!gjson.Get(body, "interrupt").Bool() ||
		gjson.Get(body, "interruptDuration").Int() != 30 {
		t.Fatalf("POST /task: interrupt fields not echoed in %s", body)
	}

	// GET round-trip.
	code, body = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /task/%s: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "priority").String() != "critical" ||
		!gjson.Get(body, "interrupt").Bool() ||
		gjson.Get(body, "interruptDuration").Int() != 30 {
		t.Fatalf("GET /task/%s: interrupt fields lost in %s", taskID, body)
	}

	// Replace with a different priority and a one-shot interrupt.
	code, body = perform(t, h, http.MethodPost, "/task/"+taskID+"/replace", "", jsonBody(t, map[string]interface{}{
		"id": taskID, "name": "int2", "type": "interval", "interval": 90, "mediaId": mediaID,
		"priority": "important", "interrupt": true, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /task/%s/replace: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "priority").String() != "important" || !gjson.Get(body, "interrupt").Bool() {
		t.Fatalf("POST replace: interrupt fields not applied in %s", body)
	}

	// GET round-trip after replace.
	code, body = perform(t, h, http.MethodGet, "/task/"+taskID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /task/%s after replace: status %d body %s", taskID, code, body)
	}
	if gjson.Get(body, "priority").String() != "important" ||
		!gjson.Get(body, "interrupt").Bool() ||
		gjson.Get(body, "interruptDuration").Int() != 0 {
		t.Fatalf("GET /task/%s after replace: unexpected task in %s", taskID, body)
	}
}

// TestManagementPlaylistFallback covers fallbackPlaylistId across create,
// update, clear and the failure path (a missing reference is rejected after
// the update itself has already been applied).
func TestManagementPlaylistFallback(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// The playlist to reference as a fallback.
	_, body := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{"name": "fallback", "items": []string{mediaID}}))
	fallbackID := gjson.Get(body, "id").String()
	if fallbackID == "" {
		t.Fatalf("POST /playlist fallback: no id in %s", body)
	}

	// Create with a fallback reference.
	code, body := perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "main", "items": []string{mediaID}, "fallbackPlaylistId": fallbackID,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /playlist with fallback: status %d body %s", code, body)
	}
	mainID := gjson.Get(body, "id").String()
	if mainID == "" {
		t.Fatalf("POST /playlist with fallback: no id in %s", body)
	}
	// The create response is produced before the fallback reference is
	// attached in a second write, so the applied reference is read back.
	code, body = perform(t, h, http.MethodGet, "/playlist/"+mainID, "", "")
	if code != http.StatusOK || gjson.Get(body, "fallbackPlaylistId").String() != fallbackID {
		t.Fatalf("GET /playlist/%s: fallback not applied in %s", mainID, body)
	}

	// A broken fallback reference on create is rejected and the playlist is
	// not created.
	code, _ = perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{
		"name": "broken", "items": []string{mediaID}, "fallbackPlaylistId": "missing",
	}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /playlist with missing fallback: status %d, want 404", code)
	}

	// Update moves the fallback to another playlist.
	_, body = perform(t, h, http.MethodPost, "/playlist", "", jsonBody(t, map[string]interface{}{"name": "backup", "items": []string{mediaID}}))
	backupID := gjson.Get(body, "id").String()
	if backupID == "" {
		t.Fatalf("POST /playlist backup: no id in %s", body)
	}
	code, body = perform(t, h, http.MethodPut, "/playlist/"+mainID, "", jsonBody(t, map[string]interface{}{
		"name": "main", "items": []string{mediaID}, "fallbackPlaylistId": backupID,
	}))
	if code != http.StatusOK {
		t.Fatalf("PUT /playlist/%s fallback: status %d body %s", mainID, code, body)
	}
	if gjson.Get(body, "fallbackPlaylistId").String() != backupID {
		t.Fatalf("PUT /playlist/%s: fallback not updated in %s", mainID, body)
	}

	// An update that clears the fallback (empty reference) succeeds.
	code, body = perform(t, h, http.MethodPut, "/playlist/"+mainID, "", jsonBody(t, map[string]interface{}{
		"name": "main", "items": []string{mediaID}, "fallbackPlaylistId": "",
	}))
	if code != http.StatusOK {
		t.Fatalf("PUT /playlist/%s clear fallback: status %d body %s", mainID, code, body)
	}
	if gjson.Get(body, "fallbackPlaylistId").String() != "" {
		t.Fatalf("PUT /playlist/%s: fallback not cleared in %s", mainID, body)
	}

	// An update referencing a missing fallback fails; the fallback is a
	// separate write, so the update itself already took effect.
	code, body = perform(t, h, http.MethodPut, "/playlist/"+mainID, "", jsonBody(t, map[string]interface{}{
		"name": "main-renamed", "fallbackPlaylistId": "missing",
	}))
	if code != http.StatusNotFound {
		t.Fatalf("PUT /playlist/%s missing fallback: status %d, want 404 (body %s)", mainID, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/playlist/"+mainID, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /playlist/%s after failed fallback: status %d body %s", mainID, code, body)
	}
	if gjson.Get(body, "name").String() != "main-renamed" {
		t.Fatalf("GET /playlist/%s after failed fallback: name not updated, got %s", mainID, body)
	}
}

// TestManagementOutputGroupCRUD covers the full /output-group route family:
// create/list/get/update (body id and URL id fallback), enabled toggle, add
// and remove outputs, delete, and the 400/404 error paths.
func TestManagementOutputGroupCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create.
	code, body := perform(t, h, http.MethodPost, "/output-group", "", jsonBody(t, map[string]interface{}{
		"name": "sdi", "description": "main sdi", "platform": "sdi", "region": "cn", "business": "ad",
		"outputs": []string{"out-1", "out-2"}, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /output-group: status %d body %s", code, body)
	}
	gid := gjson.Get(body, "id").String()
	if gid == "" {
		t.Fatalf("POST /output-group: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "sdi" ||
		gjson.Get(body, "platform").String() != "sdi" ||
		gjson.Get(body, "outputs.#").Int() != 2 ||
		!gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /output-group: unexpected group %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/output-group", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /output-group: status %d body %s", code, body)
	}
	if gjson.Get(body, "groups.#").Int() != 1 {
		t.Fatalf("GET /output-group: expected 1 group, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/output-group/"+gid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /output-group/%s: status %d body %s", gid, code, body)
	}
	if gjson.Get(body, "id").String() != gid {
		t.Fatalf("GET /output-group/%s: wrong id in %s", gid, body)
	}

	// Update via POST /output-group/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/output-group/update", "", jsonBody(t, map[string]interface{}{
		"id": gid, "name": "sdi2", "platform": "ip", "outputs": []string{"out-9"}, "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /output-group/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "sdi2" ||
		gjson.Get(body, "platform").String() != "ip" ||
		gjson.Get(body, "outputs.0").String() != "out-9" ||
		gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /output-group/update: unexpected group %s", body)
	}

	// Update via URL id fallback (no id in the body).
	code, body = perform(t, h, http.MethodPost, "/output-group/"+gid+"/update", "", jsonBody(t, map[string]interface{}{
		"name": "sdi3", "outputs": []string{"out-1", "out-2"},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /output-group/%s/update: status %d body %s", gid, code, body)
	}
	if gjson.Get(body, "name").String() != "sdi3" || gjson.Get(body, "outputs.#").Int() != 2 {
		t.Fatalf("POST /output-group/%s/update: unexpected group %s", gid, body)
	}

	// Enabled toggle via URL id fallback.
	code, body = perform(t, h, http.MethodPost, "/output-group/"+gid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusOK {
		t.Fatalf("POST /output-group/%s/enabled: status %d body %s", gid, code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /output-group/%s/enabled: missing ok in %s", gid, body)
	}
	code, body = perform(t, h, http.MethodGet, "/output-group/"+gid, "", "")
	if !gjson.Get(body, "enabled").Bool() {
		t.Fatalf("GET /output-group/%s: expected enabled, got %s", gid, body)
	}

	// Add an output; the response is the updated group.
	code, body = perform(t, h, http.MethodPost, "/output-group/"+gid+"/outputs", "", jsonBody(t, map[string]interface{}{"unique": "out-7"}))
	if code != http.StatusOK {
		t.Fatalf("POST /output-group/%s/outputs: status %d body %s", gid, code, body)
	}
	if gjson.Get(body, "outputs.#").Int() != 3 || gjson.Get(body, "outputs.2").String() != "out-7" {
		t.Fatalf("POST /output-group/%s/outputs: output not added in %s", gid, body)
	}

	// Remove an output via the URL.
	code, body = perform(t, h, http.MethodDelete, "/output-group/"+gid+"/outputs/out-7", "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /output-group/%s/outputs/out-7: status %d body %s", gid, code, body)
	}
	if gjson.Get(body, "outputs.#").Int() != 2 {
		t.Fatalf("DELETE output: expected 2 outputs left, got %s", body)
	}

	// Add with an empty reference is a 400.
	code, _ = perform(t, h, http.MethodPost, "/output-group/"+gid+"/outputs", "", jsonBody(t, map[string]interface{}{"unique": "  "}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /output-group/%s/outputs empty unique: status %d, want 400", gid, code)
	}

	// Create with an empty name is a 400; a duplicate name is a 409.
	code, _ = perform(t, h, http.MethodPost, "/output-group", "", jsonBody(t, map[string]interface{}{"name": "  "}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /output-group empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/output-group", "", jsonBody(t, map[string]interface{}{"name": "sdi3"}))
	if code != http.StatusConflict {
		t.Fatalf("POST /output-group duplicate name: status %d, want 409", code)
	}

	// Operations on a missing group are 404s.
	code, _ = perform(t, h, http.MethodGet, "/output-group/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /output-group/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/output-group/missing/update", "", jsonBody(t, map[string]interface{}{"name": "x"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /output-group/missing/update: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/output-group/missing/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /output-group/missing/enabled: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/output-group/missing/outputs", "", jsonBody(t, map[string]interface{}{"unique": "x"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /output-group/missing/outputs: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/output-group/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /output-group/missing: status %d, want 404", code)
	}

	// Delete the group, then it is gone.
	code, _ = perform(t, h, http.MethodDelete, "/output-group/"+gid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /output-group/%s: status %d, want 200", gid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/output-group/"+gid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /output-group/%s after delete: status %d, want 404", gid, code)
	}
}

// ---------------------------------------------------------------------------
// Output failover CRUD
// ---------------------------------------------------------------------------

func TestManagementFailoverCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create.
	code, body := perform(t, h, http.MethodPost, "/failover", "", jsonBody(t, map[string]interface{}{
		"name": "pair", "primaryUnique": "primary", "backupUnique": "backup",
		"policy": "manual", "thresholdSeconds": 30, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /failover: status %d body %s", code, body)
	}
	fid := gjson.Get(body, "id").String()
	if fid == "" {
		t.Fatalf("POST /failover: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "pair" ||
		gjson.Get(body, "primaryUnique").String() != "primary" ||
		gjson.Get(body, "backupUnique").String() != "backup" ||
		gjson.Get(body, "policy").String() != "manual" ||
		gjson.Get(body, "thresholdSeconds").Int() != 30 ||
		!gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /failover: unexpected failover %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/failover", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /failover: status %d body %s", code, body)
	}
	if gjson.Get(body, "failovers.#").Int() != 1 {
		t.Fatalf("GET /failover: expected 1 failover, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/failover/"+fid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /failover/%s: status %d body %s", fid, code, body)
	}
	if gjson.Get(body, "id").String() != fid {
		t.Fatalf("GET /failover/%s: wrong id in %s", fid, body)
	}

	// Update via POST /failover/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/failover/update", "", jsonBody(t, map[string]interface{}{
		"id": fid, "name": "pair2", "primaryUnique": "primary", "backupUnique": "backup2",
		"policy": "automatic", "thresholdSeconds": 60, "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /failover/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "pair2" ||
		gjson.Get(body, "backupUnique").String() != "backup2" ||
		gjson.Get(body, "policy").String() != "automatic" ||
		gjson.Get(body, "thresholdSeconds").Int() != 60 ||
		gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /failover/update: unexpected failover %s", body)
	}

	// Update via URL id fallback (no id in the body); an omitted policy
	// defaults to automatic.
	code, body = perform(t, h, http.MethodPost, "/failover/"+fid+"/update", "", jsonBody(t, map[string]interface{}{
		"name": "pair3", "primaryUnique": "primary", "backupUnique": "backup2", "thresholdSeconds": 10,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /failover/%s/update: status %d body %s", fid, code, body)
	}
	if gjson.Get(body, "name").String() != "pair3" || gjson.Get(body, "thresholdSeconds").Int() != 10 {
		t.Fatalf("POST /failover/%s/update: unexpected failover %s", fid, body)
	}

	// Enabled toggle via URL id fallback.
	code, body = perform(t, h, http.MethodPost, "/failover/"+fid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusOK {
		t.Fatalf("POST /failover/%s/enabled: status %d body %s", fid, code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /failover/%s/enabled: missing ok in %s", fid, body)
	}
	code, body = perform(t, h, http.MethodGet, "/failover/"+fid, "", "")
	if !gjson.Get(body, "enabled").Bool() {
		t.Fatalf("GET /failover/%s: expected enabled, got %s", fid, body)
	}

	// Empty name is a 400, a duplicate name a 409, an unknown policy a 400.
	code, _ = perform(t, h, http.MethodPost, "/failover", "", jsonBody(t, map[string]interface{}{"name": "  ", "primaryUnique": "a", "backupUnique": "b", "thresholdSeconds": 1}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /failover empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/failover", "", jsonBody(t, map[string]interface{}{"name": "pair3", "primaryUnique": "a", "backupUnique": "b", "thresholdSeconds": 1}))
	if code != http.StatusConflict {
		t.Fatalf("POST /failover duplicate name: status %d, want 409", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/failover", "", jsonBody(t, map[string]interface{}{"name": "bad", "primaryUnique": "a", "backupUnique": "b", "policy": "nope", "thresholdSeconds": 1}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /failover unknown policy: status %d, want 400", code)
	}

	// Operations on a missing failover are 404s.
	code, _ = perform(t, h, http.MethodGet, "/failover/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /failover/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/failover/missing/update", "", jsonBody(t, map[string]interface{}{"name": "x", "primaryUnique": "a", "backupUnique": "b", "thresholdSeconds": 1}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /failover/missing/update: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/failover/missing/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /failover/missing/enabled: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/failover/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /failover/missing: status %d, want 404", code)
	}

	// Delete, then it is gone.
	code, _ = perform(t, h, http.MethodDelete, "/failover/"+fid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /failover/%s: status %d, want 200", fid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/failover/"+fid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /failover/%s after delete: status %d, want 404", fid, code)
	}
}

// ---------------------------------------------------------------------------
// Health policy CRUD
// ---------------------------------------------------------------------------

func TestManagementHealthPolicyCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create; zero retry limits exercise the package defaults (3 retries,
	// 60s window).
	code, body := perform(t, h, http.MethodPost, "/health-policy", "", jsonBody(t, map[string]interface{}{
		"name": "retry", "maxRetries": 0, "retryWindowSeconds": 0, "autoSkipOnFailure": true, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /health-policy: status %d body %s", code, body)
	}
	pid := gjson.Get(body, "id").String()
	if pid == "" {
		t.Fatalf("POST /health-policy: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "retry" ||
		gjson.Get(body, "maxRetries").Int() != 3 ||
		gjson.Get(body, "retryWindowSeconds").Int() != 60 ||
		!gjson.Get(body, "autoSkipOnFailure").Bool() ||
		!gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /health-policy: unexpected policy %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/health-policy", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /health-policy: status %d body %s", code, body)
	}
	if gjson.Get(body, "healthPolicies.#").Int() != 1 {
		t.Fatalf("GET /health-policy: expected 1 policy, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/health-policy/"+pid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /health-policy/%s: status %d body %s", pid, code, body)
	}
	if gjson.Get(body, "id").String() != pid {
		t.Fatalf("GET /health-policy/%s: wrong id in %s", pid, body)
	}

	// Update via POST /health-policy/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/health-policy/update", "", jsonBody(t, map[string]interface{}{
		"id": pid, "name": "retry2", "maxRetries": 5, "retryWindowSeconds": 120, "autoSkipOnFailure": false, "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /health-policy/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "retry2" ||
		gjson.Get(body, "maxRetries").Int() != 5 ||
		gjson.Get(body, "retryWindowSeconds").Int() != 120 ||
		gjson.Get(body, "autoSkipOnFailure").Bool() ||
		gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /health-policy/update: unexpected policy %s", body)
	}

	// Update via URL id fallback (no id in the body).
	code, body = perform(t, h, http.MethodPost, "/health-policy/"+pid+"/update", "", jsonBody(t, map[string]interface{}{
		"name": "retry3", "maxRetries": 1, "retryWindowSeconds": 30,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /health-policy/%s/update: status %d body %s", pid, code, body)
	}
	if gjson.Get(body, "name").String() != "retry3" || gjson.Get(body, "maxRetries").Int() != 1 {
		t.Fatalf("POST /health-policy/%s/update: unexpected policy %s", pid, body)
	}

	// Enabled toggle via URL id fallback.
	code, body = perform(t, h, http.MethodPost, "/health-policy/"+pid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusOK {
		t.Fatalf("POST /health-policy/%s/enabled: status %d body %s", pid, code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /health-policy/%s/enabled: missing ok in %s", pid, body)
	}
	code, body = perform(t, h, http.MethodGet, "/health-policy/"+pid, "", "")
	if !gjson.Get(body, "enabled").Bool() {
		t.Fatalf("GET /health-policy/%s: expected enabled, got %s", pid, body)
	}

	// Empty name is a 400, a negative retry limit a 400, a duplicate name a
	// 409.
	code, _ = perform(t, h, http.MethodPost, "/health-policy", "", jsonBody(t, map[string]interface{}{"name": "  "}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /health-policy empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/health-policy", "", jsonBody(t, map[string]interface{}{"name": "neg", "maxRetries": -1}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /health-policy negative retries: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/health-policy", "", jsonBody(t, map[string]interface{}{"name": "retry3"}))
	if code != http.StatusConflict {
		t.Fatalf("POST /health-policy duplicate name: status %d, want 409", code)
	}

	// Operations on a missing policy are 404s.
	code, _ = perform(t, h, http.MethodGet, "/health-policy/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /health-policy/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/health-policy/missing/update", "", jsonBody(t, map[string]interface{}{"name": "x"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /health-policy/missing/update: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/health-policy/missing/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /health-policy/missing/enabled: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/health-policy/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /health-policy/missing: status %d, want 404", code)
	}

	// Delete, then it is gone.
	code, _ = perform(t, h, http.MethodDelete, "/health-policy/"+pid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /health-policy/%s: status %d, want 200", pid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/health-policy/"+pid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /health-policy/%s after delete: status %d, want 404", pid, code)
	}
}

// ---------------------------------------------------------------------------
// Cache task CRUD and status transitions
// ---------------------------------------------------------------------------

func TestManagementCacheTaskCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// Create with an explicit pending status.
	code, body := perform(t, h, http.MethodPost, "/cache-task", "", jsonBody(t, map[string]interface{}{
		"mediaId": mediaID, "note": "prime", "status": "pending",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /cache-task: status %d body %s", code, body)
	}
	ctid := gjson.Get(body, "id").String()
	if ctid == "" {
		t.Fatalf("POST /cache-task: no id in %s", body)
	}
	if gjson.Get(body, "mediaId").String() != mediaID ||
		gjson.Get(body, "note").String() != "prime" ||
		gjson.Get(body, "status").String() != "pending" {
		t.Fatalf("POST /cache-task: unexpected task %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/cache-task", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /cache-task: status %d body %s", code, body)
	}
	if gjson.Get(body, "cacheTasks.#").Int() != 1 {
		t.Fatalf("GET /cache-task: expected 1 task, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/cache-task/"+ctid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /cache-task/%s: status %d body %s", ctid, code, body)
	}
	if gjson.Get(body, "id").String() != ctid {
		t.Fatalf("GET /cache-task/%s: wrong id in %s", ctid, body)
	}

	// Update via POST /cache-task/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/cache-task/update", "", jsonBody(t, map[string]interface{}{
		"id": ctid, "mediaId": mediaID, "note": "re-prime", "status": "running",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /cache-task/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "note").String() != "re-prime" || gjson.Get(body, "status").String() != "running" {
		t.Fatalf("POST /cache-task/update: unexpected task %s", body)
	}

	// Update via URL id fallback (no id in the body); an omitted status
	// defaults to pending.
	code, body = perform(t, h, http.MethodPost, "/cache-task/"+ctid+"/update", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusOK {
		t.Fatalf("POST /cache-task/%s/update: status %d body %s", ctid, code, body)
	}
	if gjson.Get(body, "status").String() != "pending" {
		t.Fatalf("POST /cache-task/%s/update: unexpected task %s", ctid, body)
	}

	// An empty media reference is a 400, an unknown status a 400 and a
	// missing media a 404.
	code, _ = perform(t, h, http.MethodPost, "/cache-task", "", jsonBody(t, map[string]interface{}{"mediaId": "  "}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /cache-task empty media: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/cache-task", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID, "status": "bogus"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /cache-task unknown status: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/cache-task", "", jsonBody(t, map[string]interface{}{"mediaId": "missing-media"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /cache-task missing media: status %d, want 404", code)
	}

	// Operations on a missing task are 404s.
	code, _ = perform(t, h, http.MethodGet, "/cache-task/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /cache-task/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/cache-task/missing/update", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /cache-task/missing/update: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/cache-task/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /cache-task/missing: status %d, want 404", code)
	}

	// Delete, then it is gone.
	code, _ = perform(t, h, http.MethodDelete, "/cache-task/"+ctid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /cache-task/%s: status %d, want 200", ctid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/cache-task/"+ctid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /cache-task/%s after delete: status %d, want 404", ctid, code)
	}
}

func TestManagementCacheTaskStatusTransitions(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	_, body := perform(t, h, http.MethodPost, "/cache-task", "", jsonBody(t, map[string]interface{}{"mediaId": mediaID, "note": "prime"}))
	ctid := gjson.Get(body, "id").String()
	if ctid == "" {
		t.Fatalf("POST /cache-task: no id in %s", body)
	}

	// running.
	code, body := perform(t, h, http.MethodPost, "/cache-task/"+ctid+"/running", "", "{}")
	if code != http.StatusOK {
		t.Fatalf("POST /cache-task/%s/running: status %d body %s", ctid, code, body)
	}
	if gjson.Get(body, "status").String() != "running" {
		t.Fatalf("POST /cache-task/%s/running: unexpected task %s", ctid, body)
	}

	// done records the completion time.
	code, body = perform(t, h, http.MethodPost, "/cache-task/"+ctid+"/done", "", "{}")
	if code != http.StatusOK {
		t.Fatalf("POST /cache-task/%s/done: status %d body %s", ctid, code, body)
	}
	if gjson.Get(body, "status").String() != "done" || gjson.Get(body, "completedAt").String() == "" {
		t.Fatalf("POST /cache-task/%s/done: unexpected task %s", ctid, body)
	}

	// failed with a note replaces the task note.
	code, body = perform(t, h, http.MethodPost, "/cache-task/"+ctid+"/failed", "", jsonBody(t, map[string]interface{}{"note": "timeout"}))
	if code != http.StatusOK {
		t.Fatalf("POST /cache-task/%s/failed: status %d body %s", ctid, code, body)
	}
	if gjson.Get(body, "status").String() != "failed" || gjson.Get(body, "note").String() != "timeout" {
		t.Fatalf("POST /cache-task/%s/failed: unexpected task %s", ctid, body)
	}

	// Transitions are unconditional: re-marking a completed task as done is
	// allowed.
	code, body = perform(t, h, http.MethodPost, "/cache-task/"+ctid+"/done", "", "{}")
	if code != http.StatusOK || gjson.Get(body, "status").String() != "done" {
		t.Fatalf("POST /cache-task/%s/done again: status %d body %s", ctid, code, body)
	}

	// Transitions on a missing task are 404s.
	code, _ = perform(t, h, http.MethodPost, "/cache-task/missing/running", "", "{}")
	if code != http.StatusNotFound {
		t.Fatalf("POST /cache-task/missing/running: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/cache-task/missing/failed", "", jsonBody(t, map[string]interface{}{"note": "x"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /cache-task/missing/failed: status %d, want 404", code)
	}
}

// ---------------------------------------------------------------------------
// Scene template CRUD and duplicate
// ---------------------------------------------------------------------------

func TestManagementSceneTemplateCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create.
	code, body := perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{
		"name": "intro", "kind": "intro",
		"params":  map[string]string{"text": "hello", "font": "16px"},
		"enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /scene-template: status %d body %s", code, body)
	}
	stid := gjson.Get(body, "id").String()
	if stid == "" {
		t.Fatalf("POST /scene-template: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "intro" ||
		gjson.Get(body, "kind").String() != "intro" ||
		gjson.Get(body, "params.text").String() != "hello" ||
		gjson.Get(body, "params.font").String() != "16px" ||
		!gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /scene-template: unexpected template %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/scene-template", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /scene-template: status %d body %s", code, body)
	}
	if gjson.Get(body, "sceneTemplates.#").Int() != 1 {
		t.Fatalf("GET /scene-template: expected 1 template, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/scene-template/"+stid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /scene-template/%s: status %d body %s", stid, code, body)
	}
	if gjson.Get(body, "id").String() != stid {
		t.Fatalf("GET /scene-template/%s: wrong id in %s", stid, body)
	}

	// Update via POST /scene-template/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/scene-template/update", "", jsonBody(t, map[string]interface{}{
		"id": stid, "name": "intro2", "kind": "title",
		"params": map[string]string{"text": "hi"}, "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /scene-template/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "intro2" ||
		gjson.Get(body, "kind").String() != "title" ||
		gjson.Get(body, "params.text").String() != "hi" ||
		gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /scene-template/update: unexpected template %s", body)
	}

	// Update via URL id fallback (no id in the body).
	code, body = perform(t, h, http.MethodPost, "/scene-template/"+stid+"/update", "", jsonBody(t, map[string]interface{}{
		"name": "intro3", "kind": "clock",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /scene-template/%s/update: status %d body %s", stid, code, body)
	}
	if gjson.Get(body, "name").String() != "intro3" || gjson.Get(body, "kind").String() != "clock" {
		t.Fatalf("POST /scene-template/%s/update: unexpected template %s", stid, body)
	}

	// Enabled toggle via URL id fallback.
	code, body = perform(t, h, http.MethodPost, "/scene-template/"+stid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusOK {
		t.Fatalf("POST /scene-template/%s/enabled: status %d body %s", stid, code, body)
	}
	if !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /scene-template/%s/enabled: missing ok in %s", stid, body)
	}
	code, body = perform(t, h, http.MethodGet, "/scene-template/"+stid, "", "")
	if !gjson.Get(body, "enabled").Bool() {
		t.Fatalf("GET /scene-template/%s: expected enabled, got %s", stid, body)
	}

	// Empty name is a 400, an unknown kind a 400, a duplicate name a 409.
	code, _ = perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{"name": "  ", "kind": "logo"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /scene-template empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{"name": "bad", "kind": "nope"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /scene-template unknown kind: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{"name": "intro3", "kind": "logo"}))
	if code != http.StatusConflict {
		t.Fatalf("POST /scene-template duplicate name: status %d, want 409", code)
	}

	// Operations on a missing template are 404s.
	code, _ = perform(t, h, http.MethodGet, "/scene-template/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /scene-template/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/scene-template/missing/update", "", jsonBody(t, map[string]interface{}{"name": "x", "kind": "logo"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /scene-template/missing/update: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/scene-template/missing/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /scene-template/missing/enabled: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/scene-template/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /scene-template/missing: status %d, want 404", code)
	}

	// Delete, then it is gone.
	code, _ = perform(t, h, http.MethodDelete, "/scene-template/"+stid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /scene-template/%s: status %d, want 200", stid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/scene-template/"+stid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /scene-template/%s after delete: status %d, want 404", stid, code)
	}
}

func TestManagementSceneTemplateDuplicate(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	_, body := perform(t, h, http.MethodPost, "/scene-template", "", jsonBody(t, map[string]interface{}{
		"name": "watermark", "kind": "watermark",
		"params": map[string]string{"image": "/logo.png"},
	}))
	stid := gjson.Get(body, "id").String()
	if stid == "" {
		t.Fatalf("POST /scene-template: no id in %s", body)
	}

	// Duplicate via the URL id: fresh id, " (copy)" suffix, fields carried
	// over.
	code, body := perform(t, h, http.MethodPost, "/scene-template/"+stid+"/duplicate", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /scene-template/%s/duplicate: status %d body %s", stid, code, body)
	}
	dupID := gjson.Get(body, "id").String()
	if dupID == "" || dupID == stid {
		t.Fatalf("POST /scene-template/%s/duplicate: no fresh id in %s", stid, body)
	}
	if gjson.Get(body, "name").String() != "watermark (copy)" ||
		gjson.Get(body, "kind").String() != "watermark" ||
		gjson.Get(body, "params.image").String() != "/logo.png" {
		t.Fatalf("POST /scene-template/%s/duplicate: unexpected copy %s", stid, body)
	}

	// List has both templates.
	code, body = perform(t, h, http.MethodGet, "/scene-template", "", "")
	if code != http.StatusOK || gjson.Get(body, "sceneTemplates.#").Int() != 2 {
		t.Fatalf("GET /scene-template after duplicate: status %d body %s", code, body)
	}

	// Duplicating again collides with the " (copy)" name: 409.
	code, _ = perform(t, h, http.MethodPost, "/scene-template/"+stid+"/duplicate", "", "")
	if code != http.StatusConflict {
		t.Fatalf("POST /scene-template/%s/duplicate again: status %d, want 409", stid, code)
	}

	// Duplicating a missing template is a 404.
	code, _ = perform(t, h, http.MethodPost, "/scene-template/missing/duplicate", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("POST /scene-template/missing/duplicate: status %d, want 404", code)
	}
}

// ---------------------------------------------------------------------------
// Output failover adapter (OutputStateReader / FailoverSwitcher wiring)
// ---------------------------------------------------------------------------

func TestOutputFailoverAdapterOutputStates(t *testing.T) {
	output := &fakeOutputProvider{}
	output.setOutputs([]svrproto.OutputModule{
		{Path: "/dev/a", Unique: "a", Connected: true},
		{Path: "/dev/b", Unique: "b", Connected: false},
	})
	adapter := &outputFailoverAdapter{output: output}

	states, err := adapter.OutputStates()
	if err != nil {
		t.Fatalf("OutputStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("OutputStates: got %d states, want 2", len(states))
	}
	// Unique and Connected map directly from the provider; Error stays
	// empty because the provider reports no diagnostics.
	if states[0].Unique != "a" || !states[0].Connected || states[0].Error != "" {
		t.Fatalf("OutputStates[0] = %+v, want unique a connected", states[0])
	}
	if states[1].Unique != "b" || states[1].Connected {
		t.Fatalf("OutputStates[1] = %+v, want unique b disconnected", states[1])
	}
}

func TestOutputFailoverAdapterActivateOutput(t *testing.T) {
	output := &fakeOutputProvider{}
	output.setOutputs([]svrproto.OutputModule{
		{Path: "/dev/primary", Unique: "primary", Connected: true},
		{Path: "/dev/backup", Unique: "backup", Connected: false},
	})
	adapter := &outputFailoverAdapter{output: output}

	// Activating a known output cycles it: one remove, then one re-add with
	// the path resolved from the output list.
	if err := adapter.ActivateOutput("backup"); err != nil {
		t.Fatalf("ActivateOutput(backup): %v", err)
	}
	adds, removes := output.outputActions()
	if len(removes) != 1 || removes[0].Unique != "backup" {
		t.Fatalf("removes = %+v, want a single remove of backup", removes)
	}
	if len(adds) != 1 || adds[0].Unique != "backup" || adds[0].Path != "/dev/backup" {
		t.Fatalf("adds = %+v, want a single re-add of backup with its path", adds)
	}

	// An output that was never added to the pipeline has no path: the
	// activation fails before any provider mutation.
	if err := adapter.ActivateOutput("ghost"); err == nil {
		t.Fatal("ActivateOutput(ghost): expected an error")
	}
	adds, removes = output.outputActions()
	if len(adds) != 1 || len(removes) != 1 {
		t.Fatalf("failed activation must not touch the provider; adds %+v removes %+v", adds, removes)
	}
}

// waitOutputAdd polls the fake output provider until at least n OutputAdd
// calls for unique have been recorded, failing the test after the deadline.
// The failover monitor switches on its own tick loop, so the provider calls
// land asynchronously after the reader state changes.
func waitOutputAdd(t *testing.T, output *fakeOutputProvider, unique string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		adds, _ := output.outputActions()
		count := 0
		for _, add := range adds {
			if add.Unique == unique {
				count++
			}
		}
		if count >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least %d OutputAdd calls for %q within %s, got %d", n, unique, timeout, count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestManagementFailoverMonitorWiring drives the handler's own failover
// monitor (store + outputFailoverAdapter + fake output provider) through a
// full failover cycle: a primary disconnect past the threshold activates the
// backup, and an automatic policy switches back once the primary recovers.
// The monitor tick interval is the production default (1s), so the test
// waits a few seconds.
func TestManagementFailoverMonitorWiring(t *testing.T) {
	output := &fakeOutputProvider{}
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, output, false, "")

	// The primary is connected and the backup is present with a known path
	// but disconnected: the monitor stays on the primary.
	output.setOutputs([]svrproto.OutputModule{
		{Path: "/dev/primary", Unique: "primary", Connected: true},
		{Path: "/dev/backup", Unique: "backup", Connected: false},
	})

	// An enabled failover with a 1s disconnect threshold.
	code, body := perform(t, h, http.MethodPost, "/failover", "", jsonBody(t, map[string]interface{}{
		"name": "pair", "primaryUnique": "primary", "backupUnique": "backup",
		"policy": "automatic", "thresholdSeconds": 1, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /failover: status %d body %s", code, body)
	}

	if err := h.failoverMonitor.Start(); err != nil {
		t.Fatalf("start failover monitor: %v", err)
	}
	defer h.failoverMonitor.Stop()

	// Drop the primary: once the disconnect outlasts the threshold the
	// monitor must activate the backup through the adapter.
	output.setOutputs([]svrproto.OutputModule{
		{Path: "/dev/primary", Unique: "primary", Connected: false},
		{Path: "/dev/backup", Unique: "backup", Connected: false},
	})
	waitOutputAdd(t, output, "backup", 1, 8*time.Second)

	// The switch removes the backup and re-adds it with the path resolved
	// from the output list.
	adds, removes := output.outputActions()
	removeCount := 0
	for _, remove := range removes {
		if remove.Unique == "backup" {
			removeCount++
		}
	}
	if removeCount != 1 {
		t.Fatalf("expected exactly one OutputRemove of backup, got %+v", removes)
	}
	var backupAdd *svrproto.OutputAddArgs
	for i := range adds {
		if adds[i].Unique == "backup" {
			backupAdd = &adds[i]
		}
	}
	if backupAdd == nil || backupAdd.Path != "/dev/backup" {
		t.Fatalf("backup add = %+v, want path /dev/backup", backupAdd)
	}

	// The primary recovers: with an automatic policy the monitor switches
	// back and reactivates the primary the same way.
	output.setOutputs([]svrproto.OutputModule{
		{Path: "/dev/primary", Unique: "primary", Connected: true},
		{Path: "/dev/backup", Unique: "backup", Connected: false},
	})
	waitOutputAdd(t, output, "primary", 1, 8*time.Second)

	_, removes = output.outputActions()
	removeCount = 0
	for _, remove := range removes {
		if remove.Unique == "primary" {
			removeCount++
		}
	}
	if removeCount != 1 {
		t.Fatalf("expected exactly one OutputRemove of primary, got %+v", removes)
	}
}

// ---------------------------------------------------------------------------
// Phase five: nodes, instances, remote commands, config snapshots and
// templates, industry templates, smart rules, metrics and suggestions.
// ---------------------------------------------------------------------------

// addNode posts a node and returns its id.
func addNode(t *testing.T, h http.Handler, name, address string) string {
	t.Helper()
	code, body := perform(t, h, http.MethodPost, "/node", "", jsonBody(t, map[string]interface{}{
		"name": name, "address": address, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /node %s: status %d body %s", name, code, body)
	}
	id := gjson.Get(body, "id").String()
	if id == "" {
		t.Fatalf("POST /node %s: no id in %s", name, body)
	}
	return id
}

func TestManagementNodeCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create.
	code, body := perform(t, h, http.MethodPost, "/node", "", jsonBody(t, map[string]interface{}{
		"name": "node-1", "address": "10.0.0.1:4156", "status": "unknown", "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /node: status %d body %s", code, body)
	}
	nid := gjson.Get(body, "id").String()
	if nid == "" {
		t.Fatalf("POST /node: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "node-1" ||
		gjson.Get(body, "address").String() != "10.0.0.1:4156" ||
		gjson.Get(body, "status").String() != "unknown" ||
		!gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /node: unexpected node %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/node", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /node: status %d body %s", code, body)
	}
	if gjson.Get(body, "nodes.#").Int() != 1 || gjson.Get(body, "nodes.0.id").String() != nid {
		t.Fatalf("GET /node: expected the created node, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/node/"+nid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /node/%s: status %d body %s", nid, code, body)
	}
	if gjson.Get(body, "id").String() != nid {
		t.Fatalf("GET /node/%s: wrong id in %s", nid, body)
	}

	// Update via POST /node/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/node/update", "", jsonBody(t, map[string]interface{}{
		"id": nid, "name": "node-1b", "address": "10.0.0.2:4156", "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /node/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "node-1b" || gjson.Get(body, "address").String() != "10.0.0.2:4156" || gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /node/update: unexpected node %s", body)
	}

	// Update via URL id fallback (no id in the body).
	code, body = perform(t, h, http.MethodPost, "/node/"+nid+"/update", "", jsonBody(t, map[string]interface{}{
		"name": "node-1c", "address": "10.0.0.3:4156", "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /node/%s/update: status %d body %s", nid, code, body)
	}
	if gjson.Get(body, "name").String() != "node-1c" {
		t.Fatalf("POST /node/%s/update: unexpected node %s", nid, body)
	}

	// Heartbeat marks the node online and stamps lastSeen.
	code, body = perform(t, h, http.MethodPost, "/node/"+nid+"/heartbeat", "", "")
	if code != http.StatusOK {
		t.Fatalf("POST /node/%s/heartbeat: status %d body %s", nid, code, body)
	}
	if gjson.Get(body, "status").String() != "online" || gjson.Get(body, "lastSeen").String() == "" {
		t.Fatalf("POST /node/%s/heartbeat: unexpected node %s", nid, body)
	}

	// Enabled toggle.
	code, body = perform(t, h, http.MethodPost, "/node/"+nid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": false}))
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /node/%s/enabled: status %d body %s", nid, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/node/"+nid, "", "")
	if gjson.Get(body, "enabled").Bool() {
		t.Fatalf("GET /node/%s: expected disabled, got %s", nid, body)
	}

	// Error paths: empty name -> 400, duplicate name -> 409, unknown
	// status -> 400.
	code, _ = perform(t, h, http.MethodPost, "/node", "", jsonBody(t, map[string]interface{}{"name": "  ", "address": "a"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /node empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/node", "", jsonBody(t, map[string]interface{}{"name": "node-1c", "address": "b"}))
	if code != http.StatusConflict {
		t.Fatalf("POST /node duplicate name: status %d, want 409", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/node", "", jsonBody(t, map[string]interface{}{"name": "bad", "address": "a", "status": "ghost"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /node unknown status: status %d, want 400", code)
	}

	// Operations on a missing node are 404s.
	code, _ = perform(t, h, http.MethodGet, "/node/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /node/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/node/missing/heartbeat", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("POST /node/missing/heartbeat: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/node/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /node/missing: status %d, want 404", code)
	}

	// A node with an attached instance cannot be deleted (409).
	other := addNode(t, h, "node-2", "10.0.0.4:4156")
	code, body = perform(t, h, http.MethodPost, "/instance", "", jsonBody(t, map[string]interface{}{"nodeId": other, "name": "inst"}))
	if code != http.StatusOK {
		t.Fatalf("POST /instance: status %d body %s", code, body)
	}
	code, _ = perform(t, h, http.MethodDelete, "/node/"+other, "", "")
	if code != http.StatusConflict {
		t.Fatalf("DELETE /node/%s with instance: status %d, want 409", other, code)
	}

	// Delete, then gone.
	code, _ = perform(t, h, http.MethodDelete, "/node/"+nid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /node/%s: status %d, want 200", nid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/node/"+nid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /node/%s after delete: status %d, want 404", nid, code)
	}
}

func TestManagementInstanceCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	nodeA := addNode(t, h, "node-a", "10.0.0.1:4156")
	nodeB := addNode(t, h, "node-b", "10.0.0.2:4156")

	// Create.
	code, body := perform(t, h, http.MethodPost, "/instance", "", jsonBody(t, map[string]interface{}{
		"nodeId": nodeA, "name": "inst-a", "status": "running", "channelId": "ch-1",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /instance: status %d body %s", code, body)
	}
	iid := gjson.Get(body, "id").String()
	if iid == "" {
		t.Fatalf("POST /instance: no id in %s", body)
	}
	if gjson.Get(body, "nodeId").String() != nodeA ||
		gjson.Get(body, "name").String() != "inst-a" ||
		gjson.Get(body, "status").String() != "running" ||
		gjson.Get(body, "channelId").String() != "ch-1" {
		t.Fatalf("POST /instance: unexpected instance %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/instance", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /instance: status %d body %s", code, body)
	}
	if gjson.Get(body, "instances.#").Int() != 1 {
		t.Fatalf("GET /instance: expected 1 instance, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/instance/"+iid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /instance/%s: status %d body %s", iid, code, body)
	}
	if gjson.Get(body, "id").String() != iid {
		t.Fatalf("GET /instance/%s: wrong id in %s", iid, body)
	}

	// nodeId filter lists only that node's instances.
	_, body = perform(t, h, http.MethodPost, "/instance", "", jsonBody(t, map[string]interface{}{"nodeId": nodeB, "name": "inst-b"}))
	iidB := gjson.Get(body, "id").String()
	if iidB == "" {
		t.Fatalf("POST /instance inst-b: no id in %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/instance?nodeId="+nodeB, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /instance?nodeId=: status %d body %s", code, body)
	}
	if gjson.Get(body, "instances.#").Int() != 1 || gjson.Get(body, "instances.0.id").String() != iidB {
		t.Fatalf("GET /instance?nodeId=%s: expected only inst-b, got %s", nodeB, body)
	}

	// Update via POST /instance/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/instance/update", "", jsonBody(t, map[string]interface{}{
		"id": iid, "nodeId": nodeA, "name": "inst-a2", "status": "stopped", "channelId": "ch-2",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /instance/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "inst-a2" || gjson.Get(body, "status").String() != "stopped" || gjson.Get(body, "channelId").String() != "ch-2" {
		t.Fatalf("POST /instance/update: unexpected instance %s", body)
	}

	// Update via URL id fallback (no id in the body).
	code, body = perform(t, h, http.MethodPost, "/instance/"+iid+"/update", "", jsonBody(t, map[string]interface{}{"nodeId": nodeA, "name": "inst-a3"}))
	if code != http.StatusOK {
		t.Fatalf("POST /instance/%s/update: status %d body %s", iid, code, body)
	}
	if gjson.Get(body, "name").String() != "inst-a3" {
		t.Fatalf("POST /instance/%s/update: unexpected instance %s", iid, body)
	}

	// Status transition.
	code, body = perform(t, h, http.MethodPost, "/instance/"+iid+"/status", "", jsonBody(t, map[string]interface{}{"status": "running"}))
	if code != http.StatusOK {
		t.Fatalf("POST /instance/%s/status: status %d body %s", iid, code, body)
	}
	if gjson.Get(body, "status").String() != "running" {
		t.Fatalf("POST /instance/%s/status: unexpected instance %s", iid, body)
	}

	// Error paths: unknown node -> 404, empty name -> 400, unknown status
	// -> 400.
	code, _ = perform(t, h, http.MethodPost, "/instance", "", jsonBody(t, map[string]interface{}{"nodeId": "missing-node", "name": "x"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /instance unknown node: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/instance", "", jsonBody(t, map[string]interface{}{"nodeId": nodeA, "name": "  "}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /instance empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/instance/"+iid+"/status", "", jsonBody(t, map[string]interface{}{"status": "ghost"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /instance/%s/status unknown status: status %d, want 400", iid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/instance/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /instance/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/instance/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /instance/missing: status %d, want 404", code)
	}

	// Delete, then gone.
	code, _ = perform(t, h, http.MethodDelete, "/instance/"+iid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /instance/%s: status %d, want 200", iid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/instance/"+iid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /instance/%s after delete: status %d, want 404", iid, code)
	}
}

func TestManagementRemoteCommandCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	nodeA := addNode(t, h, "node-a", "10.0.0.1:4156")
	nodeB := addNode(t, h, "node-b", "10.0.0.2:4156")

	// Enqueue.
	code, body := perform(t, h, http.MethodPost, "/remote-command", "", jsonBody(t, map[string]interface{}{
		"nodeId": nodeA, "instanceId": "inst-1", "action": "start",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /remote-command: status %d body %s", code, body)
	}
	cid := gjson.Get(body, "id").String()
	if cid == "" {
		t.Fatalf("POST /remote-command: no id in %s", body)
	}
	if gjson.Get(body, "nodeId").String() != nodeA ||
		gjson.Get(body, "action").String() != "start" ||
		gjson.Get(body, "status").String() != "pending" {
		t.Fatalf("POST /remote-command: unexpected command %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/remote-command", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /remote-command: status %d body %s", code, body)
	}
	if gjson.Get(body, "commands.#").Int() != 1 {
		t.Fatalf("GET /remote-command: expected 1 command, got %s", body)
	}

	// nodeId filter.
	_, body = perform(t, h, http.MethodPost, "/remote-command", "", jsonBody(t, map[string]interface{}{"nodeId": nodeB, "action": "stop"}))
	cidB := gjson.Get(body, "id").String()
	if cidB == "" {
		t.Fatalf("POST /remote-command cidB: no id in %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/remote-command?nodeId="+nodeB, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /remote-command?nodeId=: status %d body %s", code, body)
	}
	if gjson.Get(body, "commands.#").Int() != 1 || gjson.Get(body, "commands.0.id").String() != cidB {
		t.Fatalf("GET /remote-command?nodeId=%s: expected only the node-b command, got %s", nodeB, body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/remote-command/"+cid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /remote-command/%s: status %d body %s", cid, code, body)
	}
	if gjson.Get(body, "id").String() != cid {
		t.Fatalf("GET /remote-command/%s: wrong id in %s", cid, body)
	}

	// Lifecycle: sent then success; re-marking a terminal command is a 400.
	code, body = perform(t, h, http.MethodPost, "/remote-command/"+cid+"/sent", "", "")
	if code != http.StatusOK || gjson.Get(body, "status").String() != "sent" {
		t.Fatalf("POST /remote-command/%s/sent: status %d body %s", cid, code, body)
	}
	if gjson.Get(body, "executedAt").String() == "" {
		t.Fatalf("POST /remote-command/%s/sent: executedAt not stamped in %s", cid, body)
	}
	code, body = perform(t, h, http.MethodPost, "/remote-command/"+cid+"/success", "", "")
	if code != http.StatusOK || gjson.Get(body, "status").String() != "success" {
		t.Fatalf("POST /remote-command/%s/success: status %d body %s", cid, code, body)
	}
	code, _ = perform(t, h, http.MethodPost, "/remote-command/"+cid+"/success", "", "")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /remote-command/%s/success again: status %d, want 400", cid, code)
	}

	// Failed with an error message.
	code, body = perform(t, h, http.MethodPost, "/remote-command/"+cidB+"/failed", "", jsonBody(t, map[string]interface{}{"error": "connection refused"}))
	if code != http.StatusOK {
		t.Fatalf("POST /remote-command/%s/failed: status %d body %s", cidB, code, body)
	}
	if gjson.Get(body, "status").String() != "failed" || gjson.Get(body, "error").String() != "connection refused" {
		t.Fatalf("POST /remote-command/%s/failed: unexpected command %s", cidB, body)
	}

	// Purge keeps the maxKeep newest terminal commands.
	code, body = perform(t, h, http.MethodPost, "/remote-command/purge", "", jsonBody(t, map[string]interface{}{"maxKeep": 1}))
	if code != http.StatusOK {
		t.Fatalf("POST /remote-command/purge: status %d body %s", code, body)
	}
	if gjson.Get(body, "removed").Int() != 1 {
		t.Fatalf("POST /remote-command/purge: expected 1 removed, got %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/remote-command", "", "")
	if gjson.Get(body, "commands.#").Int() != 1 || gjson.Get(body, "commands.0.id").String() != cidB {
		t.Fatalf("GET /remote-command after purge: expected only the newest terminal command, got %s", body)
	}

	// Error paths: empty nodeId -> 400, unknown action -> 400, negative
	// maxKeep -> 400, missing command -> 404.
	code, _ = perform(t, h, http.MethodPost, "/remote-command", "", jsonBody(t, map[string]interface{}{"nodeId": " ", "action": "start"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /remote-command empty nodeId: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/remote-command", "", jsonBody(t, map[string]interface{}{"nodeId": nodeA, "action": "explode"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /remote-command unknown action: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/remote-command/purge", "", jsonBody(t, map[string]interface{}{"maxKeep": -1}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /remote-command/purge negative: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodGet, "/remote-command/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /remote-command/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/remote-command/missing/sent", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("POST /remote-command/missing/sent: status %d, want 404", code)
	}
}

func TestManagementConfigSnapshotCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create a snapshot, then mutate the document.
	code, body := perform(t, h, http.MethodPost, "/config-snapshot", "", jsonBody(t, map[string]interface{}{
		"operator": "alice", "description": "before changes",
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /config-snapshot: status %d body %s", code, body)
	}
	sid := gjson.Get(body, "id").String()
	if sid == "" {
		t.Fatalf("POST /config-snapshot: no id in %s", body)
	}
	if gjson.Get(body, "operator").String() != "alice" || gjson.Get(body, "dataHash").String() == "" {
		t.Fatalf("POST /config-snapshot: unexpected snapshot %s", body)
	}

	code, body = perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": "/videos/after.mp4", "name": "after"}))
	if code != http.StatusOK {
		t.Fatalf("POST /media: status %d body %s", code, body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/config-snapshot", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /config-snapshot: status %d body %s", code, body)
	}
	if gjson.Get(body, "snapshots.#").Int() != 1 {
		t.Fatalf("GET /config-snapshot: expected 1 snapshot, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/config-snapshot/"+sid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /config-snapshot/%s: status %d body %s", sid, code, body)
	}
	if gjson.Get(body, "id").String() != sid {
		t.Fatalf("GET /config-snapshot/%s: wrong id in %s", sid, body)
	}

	// Restore rolls the document back: the media added after the snapshot
	// is gone, while the snapshot history itself survives.
	code, body = perform(t, h, http.MethodPost, "/config-snapshot/"+sid+"/restore", "", "")
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /config-snapshot/%s/restore: status %d body %s", sid, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/media", "", "")
	if gjson.Get(body, "media.#").Int() != 0 {
		t.Fatalf("GET /media after restore: expected 0 entries, got %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/config-snapshot", "", "")
	if gjson.Get(body, "snapshots.#").Int() != 1 {
		t.Fatalf("GET /config-snapshot after restore: history lost, got %s", body)
	}

	// Error paths.
	code, _ = perform(t, h, http.MethodGet, "/config-snapshot/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /config-snapshot/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/config-snapshot/missing/restore", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("POST /config-snapshot/missing/restore: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/config-snapshot/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /config-snapshot/missing: status %d, want 404", code)
	}

	// Delete, then gone.
	code, _ = perform(t, h, http.MethodDelete, "/config-snapshot/"+sid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /config-snapshot/%s: status %d, want 200", sid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/config-snapshot/"+sid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /config-snapshot/%s after delete: status %d, want 404", sid, code)
	}
}

func TestManagementConfigTemplateCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// Create; the body is a raw JSON object with a placeholder.
	code, body := perform(t, h, http.MethodPost, "/config-template", "", jsonBody(t, map[string]interface{}{
		"name": "edge", "type": "media",
		"body":    map[string]interface{}{"url": "${host}/stream", "port": 8080},
		"enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /config-template: status %d body %s", code, body)
	}
	tid := gjson.Get(body, "id").String()
	if tid == "" {
		t.Fatalf("POST /config-template: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "edge" ||
		gjson.Get(body, "type").String() != "media" ||
		gjson.Get(body, "body.url").String() != "${host}/stream" ||
		!gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /config-template: unexpected template %s", body)
	}

	// List.
	code, body = perform(t, h, http.MethodGet, "/config-template", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /config-template: status %d body %s", code, body)
	}
	if gjson.Get(body, "templates.#").Int() != 1 {
		t.Fatalf("GET /config-template: expected 1 template, got %s", body)
	}

	// Get by id.
	code, body = perform(t, h, http.MethodGet, "/config-template/"+tid, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /config-template/%s: status %d body %s", tid, code, body)
	}
	if gjson.Get(body, "id").String() != tid {
		t.Fatalf("GET /config-template/%s: wrong id in %s", tid, body)
	}

	// Expand substitutes placeholders and returns the body as raw JSON.
	code, body = perform(t, h, http.MethodPost, "/config-template/"+tid+"/expand", "", jsonBody(t, map[string]interface{}{
		"params": map[string]string{"host": "http://cdn.example"},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /config-template/%s/expand: status %d body %s", tid, code, body)
	}
	if gjson.Get(body, "body.url").String() != "http://cdn.example/stream" || gjson.Get(body, "body.port").Int() != 8080 {
		t.Fatalf("POST /config-template/%s/expand: unexpected body %s", tid, body)
	}

	// Update via POST /config-template/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/config-template/update", "", jsonBody(t, map[string]interface{}{
		"id": tid, "name": "edge2", "type": "playlist",
		"body": map[string]interface{}{"items": []string{"a", "b"}}, "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /config-template/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "edge2" || gjson.Get(body, "type").String() != "playlist" || gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /config-template/update: unexpected template %s", body)
	}

	// Update via URL id fallback (no id in the body).
	code, body = perform(t, h, http.MethodPost, "/config-template/"+tid+"/update", "", jsonBody(t, map[string]interface{}{
		"name": "edge3", "type": "media", "body": map[string]interface{}{"url": "${host}/stream"},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /config-template/%s/update: status %d body %s", tid, code, body)
	}
	if gjson.Get(body, "name").String() != "edge3" {
		t.Fatalf("POST /config-template/%s/update: unexpected template %s", tid, body)
	}

	// Enabled toggle via URL id fallback.
	code, body = perform(t, h, http.MethodPost, "/config-template/"+tid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /config-template/%s/enabled: status %d body %s", tid, code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/config-template/"+tid, "", "")
	if !gjson.Get(body, "enabled").Bool() {
		t.Fatalf("GET /config-template/%s: expected enabled, got %s", tid, body)
	}

	// Error paths: non-object body -> 400, missing expand parameter -> 400,
	// duplicate name -> 409.
	code, _ = perform(t, h, http.MethodPost, "/config-template", "", jsonBody(t, map[string]interface{}{
		"name": "arr", "type": "media", "body": []interface{}{1, 2},
	}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /config-template non-object body: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/config-template/"+tid+"/expand", "", jsonBody(t, map[string]interface{}{"params": map[string]string{}}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /config-template/%s/expand missing param: status %d, want 400", tid, code)
	}
	code, _ = perform(t, h, http.MethodPost, "/config-template", "", jsonBody(t, map[string]interface{}{
		"name": "edge3", "type": "media", "body": map[string]interface{}{"a": 1},
	}))
	if code != http.StatusConflict {
		t.Fatalf("POST /config-template duplicate name: status %d, want 409", code)
	}

	// Operations on a missing template are 404s.
	code, _ = perform(t, h, http.MethodGet, "/config-template/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /config-template/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/config-template/missing/expand", "", jsonBody(t, map[string]interface{}{"params": map[string]string{}}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /config-template/missing/expand: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/config-template/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("DELETE /config-template/missing: status %d, want 404", code)
	}

	// Delete, then gone.
	code, _ = perform(t, h, http.MethodDelete, "/config-template/"+tid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /config-template/%s: status %d, want 200", tid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/config-template/"+tid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /config-template/%s after delete: status %d, want 404", tid, code)
	}
}

func TestManagementIndustryTemplateCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// Create.
	code, body := perform(t, h, http.MethodPost, "/industry-template", "", jsonBody(t, map[string]interface{}{
		"name":              "news",
		"description":       "news industry",
		"playlistName":      "news program",
		"mediaPlaceholders": []string{"${m}"},
		"sceneKinds":        []string{"logo", "clock"},
		"task": map[string]interface{}{
			"name": "news task", "type": "interval", "interval": 60, "enabled": true,
		},
		"enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /industry-template: status %d body %s", code, body)
	}
	iid := gjson.Get(body, "id").String()
	if iid == "" {
		t.Fatalf("POST /industry-template: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "news" ||
		gjson.Get(body, "playlistName").String() != "news program" ||
		gjson.Get(body, "mediaPlaceholders.0").String() != "${m}" ||
		gjson.Get(body, "sceneKinds.#").Int() != 2 ||
		gjson.Get(body, "task.name").String() != "news task" {
		t.Fatalf("POST /industry-template: unexpected template %s", body)
	}

	// List and get.
	code, body = perform(t, h, http.MethodGet, "/industry-template", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /industry-template: status %d body %s", code, body)
	}
	if gjson.Get(body, "templates.#").Int() != 1 {
		t.Fatalf("GET /industry-template: expected 1 template, got %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/industry-template/"+iid, "", "")
	if code != http.StatusOK || gjson.Get(body, "id").String() != iid {
		t.Fatalf("GET /industry-template/%s: status %d body %s", iid, code, body)
	}

	// Deploy with a missing parameter fails before creating anything.
	code, _ = perform(t, h, http.MethodPost, "/industry-template/"+iid+"/deploy", "", jsonBody(t, map[string]interface{}{"params": map[string]string{}}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /industry-template/%s/deploy missing param: status %d, want 400", iid, code)
	}

	// Deploy with a media id that does not exist is a 404.
	code, _ = perform(t, h, http.MethodPost, "/industry-template/"+iid+"/deploy", "", jsonBody(t, map[string]interface{}{"params": map[string]string{"m": "missing-media"}}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /industry-template/%s/deploy missing media: status %d, want 404", iid, code)
	}

	// Deploy provisions the playlist, the scene templates and the task.
	code, body = perform(t, h, http.MethodPost, "/industry-template/"+iid+"/deploy", "", jsonBody(t, map[string]interface{}{"params": map[string]string{"m": mediaID}}))
	if code != http.StatusOK {
		t.Fatalf("POST /industry-template/%s/deploy: status %d body %s", iid, code, body)
	}
	plID := gjson.Get(body, "result.playlistId").String()
	if plID == "" || gjson.Get(body, "result.sceneTemplateIds.#").Int() != 2 || gjson.Get(body, "result.taskId").String() == "" {
		t.Fatalf("POST /industry-template/%s/deploy: unexpected result %s", iid, body)
	}
	code, body = perform(t, h, http.MethodGet, "/playlist/"+plID, "", "")
	if code != http.StatusOK || gjson.Get(body, "name").String() != "news program" {
		t.Fatalf("GET /playlist/%s after deploy: status %d body %s", plID, code, body)
	}

	// Update via POST /industry-template/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/industry-template/update", "", jsonBody(t, map[string]interface{}{
		"id": iid, "name": "news2", "playlistName": "news program 2", "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /industry-template/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "news2" || gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /industry-template/update: unexpected template %s", body)
	}

	// Enabled toggle via URL id fallback.
	code, body = perform(t, h, http.MethodPost, "/industry-template/"+iid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /industry-template/%s/enabled: status %d body %s", iid, code, body)
	}

	// Error paths: empty name -> 400, duplicate name -> 409.
	code, _ = perform(t, h, http.MethodPost, "/industry-template", "", jsonBody(t, map[string]interface{}{"name": "  ", "playlistName": "p"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /industry-template empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/industry-template", "", jsonBody(t, map[string]interface{}{"name": "news2", "playlistName": "p"}))
	if code != http.StatusConflict {
		t.Fatalf("POST /industry-template duplicate name: status %d, want 409", code)
	}
	code, _ = perform(t, h, http.MethodGet, "/industry-template/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /industry-template/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/industry-template/missing/deploy", "", jsonBody(t, map[string]interface{}{"params": map[string]string{}}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /industry-template/missing/deploy: status %d, want 404", code)
	}

	// Delete, then gone.
	code, _ = perform(t, h, http.MethodDelete, "/industry-template/"+iid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /industry-template/%s: status %d, want 200", iid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/industry-template/"+iid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /industry-template/%s after delete: status %d, want 404", iid, code)
	}
}

func TestManagementSmartRuleCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")

	// A media item matching the rule's tag filter.
	code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{
		"path": "/videos/hot.mp4", "name": "hot", "tags": []string{"hot"},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /media: status %d body %s", code, body)
	}
	hotID := gjson.Get(body, "id").String()

	// Create.
	code, body = perform(t, h, http.MethodPost, "/smart-rule", "", jsonBody(t, map[string]interface{}{
		"name": "hot picks", "tags": []string{"hot"}, "maxItems": 10, "enabled": true,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /smart-rule: status %d body %s", code, body)
	}
	rid := gjson.Get(body, "id").String()
	if rid == "" {
		t.Fatalf("POST /smart-rule: no id in %s", body)
	}
	if gjson.Get(body, "name").String() != "hot picks" || gjson.Get(body, "tags.0").String() != "hot" || !gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /smart-rule: unexpected rule %s", body)
	}

	// List and get.
	code, body = perform(t, h, http.MethodGet, "/smart-rule", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /smart-rule: status %d body %s", code, body)
	}
	if gjson.Get(body, "rules.#").Int() != 1 {
		t.Fatalf("GET /smart-rule: expected 1 rule, got %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/smart-rule/"+rid, "", "")
	if code != http.StatusOK || gjson.Get(body, "id").String() != rid {
		t.Fatalf("GET /smart-rule/%s: status %d body %s", rid, code, body)
	}

	// Generate previews the matching media ids without persisting.
	code, body = perform(t, h, http.MethodPost, "/smart-rule/"+rid+"/generate", "", jsonBody(t, map[string]interface{}{"recent": []string{}, "limit": 5}))
	if code != http.StatusOK {
		t.Fatalf("POST /smart-rule/%s/generate: status %d body %s", rid, code, body)
	}
	if gjson.Get(body, "mediaIds.#").Int() != 1 || gjson.Get(body, "mediaIds.0").String() != hotID {
		t.Fatalf("POST /smart-rule/%s/generate: expected [%s], got %s", rid, hotID, body)
	}
	code, body = perform(t, h, http.MethodGet, "/playlist", "", "")
	if gjson.Get(body, "playlists.#").Int() != 0 {
		t.Fatalf("GET /playlist after generate: preview must not persist, got %s", body)
	}

	// Generate-and-apply persists the generated ids as a playlist.
	code, body = perform(t, h, http.MethodPost, "/smart-rule/"+rid+"/generate-and-apply", "", jsonBody(t, map[string]interface{}{"playlistName": "auto program"}))
	if code != http.StatusOK {
		t.Fatalf("POST /smart-rule/%s/generate-and-apply: status %d body %s", rid, code, body)
	}
	plID := gjson.Get(body, "playlist.id").String()
	if plID == "" || gjson.Get(body, "playlist.name").String() != "auto program" {
		t.Fatalf("POST /smart-rule/%s/generate-and-apply: unexpected playlist %s", rid, body)
	}

	// Update via POST /smart-rule/update with the id in the body.
	code, body = perform(t, h, http.MethodPost, "/smart-rule/update", "", jsonBody(t, map[string]interface{}{
		"id": rid, "name": "hot picks 2", "tags": []string{"hot"}, "enabled": false,
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /smart-rule/update: status %d body %s", code, body)
	}
	if gjson.Get(body, "name").String() != "hot picks 2" || gjson.Get(body, "enabled").Bool() {
		t.Fatalf("POST /smart-rule/update: unexpected rule %s", body)
	}

	// Enabled toggle via URL id fallback.
	code, body = perform(t, h, http.MethodPost, "/smart-rule/"+rid+"/enabled", "", jsonBody(t, map[string]interface{}{"enabled": true}))
	if code != http.StatusOK || !gjson.Get(body, "ok").Bool() {
		t.Fatalf("POST /smart-rule/%s/enabled: status %d body %s", rid, code, body)
	}

	// A rule matching nothing generates an empty list: applying it is a 400.
	_, body = perform(t, h, http.MethodPost, "/smart-rule", "", jsonBody(t, map[string]interface{}{"name": "empty", "tags": []string{"nothing"}, "enabled": true}))
	emptyID := gjson.Get(body, "id").String()
	if emptyID == "" {
		t.Fatalf("POST /smart-rule empty: no id in %s", body)
	}
	code, body = perform(t, h, http.MethodPost, "/smart-rule/"+emptyID+"/generate-and-apply", "", jsonBody(t, map[string]interface{}{"playlistName": "x"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /smart-rule/%s/generate-and-apply empty result: status %d, want 400 (body %s)", emptyID, code, body)
	}

	// Error paths: empty name -> 400, duplicate name -> 409, unknown rule
	// -> 404.
	code, _ = perform(t, h, http.MethodPost, "/smart-rule", "", jsonBody(t, map[string]interface{}{"name": "  "}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /smart-rule empty name: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/smart-rule", "", jsonBody(t, map[string]interface{}{"name": "empty", "enabled": true}))
	if code != http.StatusConflict {
		t.Fatalf("POST /smart-rule duplicate name: status %d, want 409", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/smart-rule/missing/generate", "", jsonBody(t, map[string]interface{}{}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /smart-rule/missing/generate: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/smart-rule/missing/generate-and-apply", "", jsonBody(t, map[string]interface{}{"playlistName": "x"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /smart-rule/missing/generate-and-apply: status %d, want 404", code)
	}

	// Delete, then gone.
	code, _ = perform(t, h, http.MethodDelete, "/smart-rule/"+rid, "", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /smart-rule/%s: status %d, want 200", rid, code)
	}
	code, _ = perform(t, h, http.MethodGet, "/smart-rule/"+rid, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /smart-rule/%s after delete: status %d, want 404", rid, code)
	}
}

func TestManagementMetrics(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	addPath := func(path, name string) string {
		code, body := perform(t, h, http.MethodPost, "/media", "", jsonBody(t, map[string]interface{}{"path": path, "name": name}))
		if code != http.StatusOK {
			t.Fatalf("POST /media %s: status %d body %s", path, code, body)
		}
		return gjson.Get(body, "id").String()
	}
	mediaA := addPath("/videos/a.mp4", "a")
	mediaB := addPath("/videos/b.mp4", "b")

	// Seed the playback log directly through the service, as the scheduler
	// would: two successes and one failure for A, one success for B.
	events := management.NewPlayEventService(h.store)
	for i := 0; i < 2; i++ {
		if _, err := events.Record(management.PlayEvent{MediaID: mediaA, Result: management.PlaySuccess}); err != nil {
			t.Fatalf("record success: %v", err)
		}
	}
	if _, err := events.Record(management.PlayEvent{MediaID: mediaA, Result: management.PlayFailure}); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if _, err := events.Record(management.PlayEvent{MediaID: mediaB, Result: management.PlaySuccess}); err != nil {
		t.Fatalf("record success B: %v", err)
	}

	// Failure rate per media.
	code, body := perform(t, h, http.MethodGet, "/metrics/failure-rate?mediaId="+mediaA, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics/failure-rate: status %d body %s", code, body)
	}
	if gjson.Get(body, "plays").Int() != 3 || gjson.Get(body, "failures").Int() != 1 || math.Abs(gjson.Get(body, "rate").Float()-1.0/3.0) > 1e-9 {
		t.Fatalf("GET /metrics/failure-rate media A: unexpected body %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/metrics/failure-rate?mediaId="+mediaB, "", "")
	if gjson.Get(body, "plays").Int() != 1 || gjson.Get(body, "failures").Int() != 0 || gjson.Get(body, "rate").Float() != 0 {
		t.Fatalf("GET /metrics/failure-rate media B: unexpected body %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/metrics/failure-rate", "", "")
	if gjson.Get(body, "plays").Int() != 0 || gjson.Get(body, "failures").Int() != 0 || gjson.Get(body, "rate").Float() != 0 {
		t.Fatalf("GET /metrics/failure-rate without mediaId: unexpected body %s", body)
	}

	// Trend covers the full requested window, oldest first.
	code, body = perform(t, h, http.MethodGet, "/metrics/trend?days=3", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics/trend: status %d body %s", code, body)
	}
	if gjson.Get(body, "days.#").Int() != 3 {
		t.Fatalf("GET /metrics/trend?days=3: expected 3 entries, got %s", body)
	}
	if gjson.Get(body, "days.0.date").String() == "" || gjson.Get(body, "days.0.count").Int() != 0 {
		t.Fatalf("GET /metrics/trend?days=3: unexpected entry in %s", body)
	}

	// Summary aggregates every event.
	code, body = perform(t, h, http.MethodGet, "/metrics/summary", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics/summary: status %d body %s", code, body)
	}
	if gjson.Get(body, "summary.totalPlays").Int() != 4 ||
		gjson.Get(body, "summary.successes").Int() != 3 ||
		gjson.Get(body, "summary.failures").Int() != 1 ||
		math.Abs(gjson.Get(body, "summary.successRate").Float()-0.75) > 1e-9 {
		t.Fatalf("GET /metrics/summary: unexpected body %s", body)
	}

	// Error paths: non-positive days -> 400, malformed days -> 400,
	// unknown metrics route -> 404.
	code, _ = perform(t, h, http.MethodGet, "/metrics/trend?days=0", "", "")
	if code != http.StatusBadRequest {
		t.Fatalf("GET /metrics/trend?days=0: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodGet, "/metrics/trend?days=abc", "", "")
	if code != http.StatusBadRequest {
		t.Fatalf("GET /metrics/trend?days=abc: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodGet, "/metrics/nope", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /metrics/nope: status %d, want 404", code)
	}
}

func TestManagementSuggestionCRUD(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	mediaID := addMedia(t, h)

	// Create a media recommendation and a title generation.
	code, body := perform(t, h, http.MethodPost, "/suggestion", "", jsonBody(t, map[string]interface{}{
		"kind": "media_recommend", "title": "play next", "payload": map[string]string{"media_id": mediaID},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /suggestion: status %d body %s", code, body)
	}
	sid := gjson.Get(body, "id").String()
	if sid == "" {
		t.Fatalf("POST /suggestion: no id in %s", body)
	}
	if gjson.Get(body, "kind").String() != "media_recommend" ||
		gjson.Get(body, "title").String() != "play next" ||
		gjson.Get(body, "payload.media_id").String() != mediaID ||
		gjson.Get(body, "status").String() != "pending" {
		t.Fatalf("POST /suggestion: unexpected suggestion %s", body)
	}
	code, body = perform(t, h, http.MethodPost, "/suggestion", "", jsonBody(t, map[string]interface{}{
		"kind": "title_generate", "payload": map[string]string{"playlist_name": "morning"},
	}))
	if code != http.StatusOK {
		t.Fatalf("POST /suggestion title: status %d body %s", code, body)
	}
	sid2 := gjson.Get(body, "id").String()
	if sid2 == "" {
		t.Fatalf("POST /suggestion title: no id in %s", body)
	}

	// List and get.
	code, body = perform(t, h, http.MethodGet, "/suggestion", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET /suggestion: status %d body %s", code, body)
	}
	if gjson.Get(body, "suggestions.#").Int() != 2 {
		t.Fatalf("GET /suggestion: expected 2 suggestions, got %s", body)
	}
	code, body = perform(t, h, http.MethodGet, "/suggestion/"+sid, "", "")
	if code != http.StatusOK || gjson.Get(body, "id").String() != sid {
		t.Fatalf("GET /suggestion/%s: status %d body %s", sid, code, body)
	}

	// Approve persists the recommended media as a playlist.
	code, body = perform(t, h, http.MethodPost, "/suggestion/"+sid+"/approve", "", jsonBody(t, map[string]interface{}{"playlistName": "rec playlist"}))
	if code != http.StatusOK {
		t.Fatalf("POST /suggestion/%s/approve: status %d body %s", sid, code, body)
	}
	if gjson.Get(body, "status").String() != "applied" || gjson.Get(body, "payload.playlist_id").String() == "" {
		t.Fatalf("POST /suggestion/%s/approve: unexpected suggestion %s", sid, body)
	}
	plID := gjson.Get(body, "payload.playlist_id").String()
	code, body = perform(t, h, http.MethodGet, "/playlist/"+plID, "", "")
	if code != http.StatusOK || gjson.Get(body, "name").String() != "rec playlist" {
		t.Fatalf("GET /playlist/%s after approve: status %d body %s", plID, code, body)
	}

	// Approving the same suggestion again is a 400 (no longer pending).
	code, _ = perform(t, h, http.MethodPost, "/suggestion/"+sid+"/approve", "", jsonBody(t, map[string]interface{}{"playlistName": "again"}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /suggestion/%s/approve again: status %d, want 400", sid, code)
	}

	// Reject the other suggestion with a reason.
	code, body = perform(t, h, http.MethodPost, "/suggestion/"+sid2+"/reject", "", jsonBody(t, map[string]interface{}{"reason": "no thanks"}))
	if code != http.StatusOK {
		t.Fatalf("POST /suggestion/%s/reject: status %d body %s", sid2, code, body)
	}
	if gjson.Get(body, "status").String() != "rejected" || gjson.Get(body, "reason").String() != "no thanks" {
		t.Fatalf("POST /suggestion/%s/reject: unexpected suggestion %s", sid2, body)
	}
	code, _ = perform(t, h, http.MethodPost, "/suggestion/"+sid2+"/reject", "", jsonBody(t, map[string]interface{}{}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /suggestion/%s/reject again: status %d, want 400", sid2, code)
	}

	// Recommend ranks the playback log (frequency first).
	events := management.NewPlayEventService(h.store)
	if _, err := events.Record(management.PlayEvent{MediaID: mediaID, Result: management.PlaySuccess}); err != nil {
		t.Fatalf("record play event: %v", err)
	}
	if _, err := events.Record(management.PlayEvent{MediaID: mediaID, Result: management.PlaySuccess}); err != nil {
		t.Fatalf("record play event: %v", err)
	}
	code, body = perform(t, h, http.MethodPost, "/suggestion/recommend", "", jsonBody(t, map[string]interface{}{"limit": 5}))
	if code != http.StatusOK {
		t.Fatalf("POST /suggestion/recommend: status %d body %s", code, body)
	}
	if gjson.Get(body, "mediaIds.#").Int() != 1 || gjson.Get(body, "mediaIds.0").String() != mediaID {
		t.Fatalf("POST /suggestion/recommend: expected [%s], got %s", mediaID, body)
	}

	// Error paths: unknown kind -> 400, empty payload -> 400, missing
	// suggestion -> 404, approve without playlist name -> 400.
	code, _ = perform(t, h, http.MethodPost, "/suggestion", "", jsonBody(t, map[string]interface{}{"kind": "ghost", "payload": map[string]string{"a": "b"}}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /suggestion unknown kind: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/suggestion", "", jsonBody(t, map[string]interface{}{"kind": "media_recommend", "payload": map[string]string{}}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /suggestion empty payload: status %d, want 400", code)
	}
	code, _ = perform(t, h, http.MethodGet, "/suggestion/missing", "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /suggestion/missing: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/suggestion/missing/approve", "", jsonBody(t, map[string]interface{}{"playlistName": "x"}))
	if code != http.StatusNotFound {
		t.Fatalf("POST /suggestion/missing/approve: status %d, want 404", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/suggestion/"+sid+"/approve", "", jsonBody(t, map[string]interface{}{"playlistName": " "}))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /suggestion/%s/approve empty name: status %d, want 400", sid, code)
	}
}

// TestManagementPhaseFivePermissions drives the permission matrix for the
// phase-five resources: the auditor reads metrics and the compute-only POST
// endpoints (expand/generate/recommend are read-mapped), while every write
// — including generate-and-apply, which persists a playlist — stays
// forbidden for the auditor and allowed for the operator.
func TestManagementPhaseFivePermissions(t *testing.T) {
	h := newTestManagementHandler(t, &fakePlayProvider{}, &fakeResourceProvider{}, &fakeOutputProvider{}, false, "")
	seedUser(t, h, "admin", "password123", management.RoleAdmin, true)
	seedUser(t, h, "ops", "password123", management.RoleOperator, true)
	seedUser(t, h, "watch", "password123", management.RoleAuditor, true)
	auditor := login(t, h, "watch", "password123")
	operator := login(t, h, "ops", "password123")

	// Seed one of each resource without a token (auth disabled -> admin).
	_, body := perform(t, h, http.MethodPost, "/node", "", jsonBody(t, map[string]interface{}{"name": "node-a", "address": "10.0.0.1:4156", "enabled": true}))
	nodeID := gjson.Get(body, "id").String()
	if nodeID == "" {
		t.Fatalf("seed node: no id in %s", body)
	}
	_, body = perform(t, h, http.MethodPost, "/smart-rule", "", jsonBody(t, map[string]interface{}{"name": "rule-a", "enabled": true}))
	ruleID := gjson.Get(body, "id").String()
	if ruleID == "" {
		t.Fatalf("seed smart rule: no id in %s", body)
	}
	_, body = perform(t, h, http.MethodPost, "/config-template", "", jsonBody(t, map[string]interface{}{
		"name": "tpl-a", "type": "media", "body": map[string]interface{}{"k": "${v}"}, "enabled": true,
	}))
	tplID := gjson.Get(body, "id").String()
	if tplID == "" {
		t.Fatalf("seed config template: no id in %s", body)
	}

	// The auditor can read metrics.
	code, body := perform(t, h, http.MethodGet, "/metrics/summary", "Bearer "+auditor, "")
	if code != http.StatusOK {
		t.Fatalf("auditor GET /metrics/summary: status %d body %s", code, body)
	}

	// The compute-only POST endpoints are read-mapped and stay open.
	code, body = perform(t, h, http.MethodPost, "/smart-rule/"+ruleID+"/generate", "Bearer "+auditor, "{}")
	if code != http.StatusOK {
		t.Fatalf("auditor POST /smart-rule/%s/generate: status %d body %s, want 200 (read-mapped)", ruleID, code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/config-template/"+tplID+"/expand", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"params": map[string]string{"v": "1"}}))
	if code != http.StatusOK {
		t.Fatalf("auditor POST /config-template/%s/expand: status %d body %s, want 200 (read-mapped)", tplID, code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/suggestion/recommend", "Bearer "+auditor, "{}")
	if code != http.StatusOK {
		t.Fatalf("auditor POST /suggestion/recommend: status %d body %s, want 200 (read-mapped)", code, body)
	}

	// Auditor writes are forbidden, generate-and-apply included (it
	// persists the generated playlist).
	code, _ = perform(t, h, http.MethodPost, "/smart-rule/"+ruleID+"/generate-and-apply", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"playlistName": "x"}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /smart-rule/%s/generate-and-apply: status %d, want 403", ruleID, code)
	}
	code, _ = perform(t, h, http.MethodPost, "/node", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"name": "n", "address": "a"}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /node: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/remote-command", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"nodeId": nodeID, "action": "start"}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /remote-command: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/config-snapshot", "Bearer "+auditor, "{}")
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /config-snapshot: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodPost, "/suggestion", "Bearer "+auditor, jsonBody(t, map[string]interface{}{"kind": "media_recommend", "payload": map[string]string{"media_id": "x"}}))
	if code != http.StatusForbidden {
		t.Fatalf("auditor POST /suggestion: status %d, want 403", code)
	}
	code, _ = perform(t, h, http.MethodDelete, "/node/"+nodeID, "Bearer "+auditor, "")
	if code != http.StatusForbidden {
		t.Fatalf("auditor DELETE /node/%s: status %d, want 403", nodeID, code)
	}

	// The operator has full write access to every phase-five resource.
	code, body = perform(t, h, http.MethodPost, "/node", "Bearer "+operator, jsonBody(t, map[string]interface{}{"name": "node-b", "address": "10.0.0.2:4156", "enabled": true}))
	if code != http.StatusOK {
		t.Fatalf("operator POST /node: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/remote-command", "Bearer "+operator, jsonBody(t, map[string]interface{}{"nodeId": nodeID, "action": "start"}))
	if code != http.StatusOK {
		t.Fatalf("operator POST /remote-command: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/config-snapshot", "Bearer "+operator, "{}")
	if code != http.StatusOK {
		t.Fatalf("operator POST /config-snapshot: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/industry-template", "Bearer "+operator, jsonBody(t, map[string]interface{}{"name": "ind-a", "playlistName": "p"}))
	if code != http.StatusOK {
		t.Fatalf("operator POST /industry-template: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/smart-rule", "Bearer "+operator, jsonBody(t, map[string]interface{}{"name": "rule-b", "enabled": true}))
	if code != http.StatusOK {
		t.Fatalf("operator POST /smart-rule: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodPost, "/suggestion", "Bearer "+operator, jsonBody(t, map[string]interface{}{"kind": "media_recommend", "payload": map[string]string{"media_id": "x"}}))
	if code != http.StatusOK {
		t.Fatalf("operator POST /suggestion: status %d body %s", code, body)
	}
	code, body = perform(t, h, http.MethodGet, "/metrics/summary", "Bearer "+operator, "")
	if code != http.StatusOK {
		t.Fatalf("operator GET /metrics/summary: status %d body %s", code, body)
	}
}
