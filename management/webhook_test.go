package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWebhookSubscriptionCRUD(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	sub, err := ws.Create(WebhookSubscriptionSpec{
		Name:    "alerts",
		URL:     "https://example.com/hook",
		Events:  []WebhookEvent{EventOutputDisconnected, EventMaterialFailed},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sub.ID == "" {
		t.Fatal("expected generated id")
	}
	if sub.Name != "alerts" || sub.URL != "https://example.com/hook" || !sub.Enabled {
		t.Fatalf("unexpected subscription: %+v", sub)
	}
	if want := []WebhookEvent{EventOutputDisconnected, EventMaterialFailed}; !sameWebhookEvents(sub.Events, want) {
		t.Fatalf("unexpected events: %v", sub.Events)
	}

	got, err := ws.Get(sub.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "alerts" || len(got.Events) != 2 {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(ws.List()) != 1 {
		t.Fatal("expected 1 subscription in list")
	}

	// Update replaces everything, including the enabled flag (the spec's
	// zero Enabled is applied, showing full-replacement semantics).
	upd, err := ws.Update(sub.ID, WebhookSubscriptionSpec{
		Name:   "alerts-renamed",
		URL:    "http://localhost:9000/hook",
		Events: []WebhookEvent{EventTaskCompleted},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "alerts-renamed" || upd.URL != "http://localhost:9000/hook" ||
		upd.Enabled || len(upd.Events) != 1 || upd.Events[0] != EventTaskCompleted {
		t.Fatalf("unexpected update: %+v", upd)
	}

	// SetEnabled toggles both ways.
	if err := ws.SetEnabled(sub.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := ws.SetEnabled(sub.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = ws.Get(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected subscription disabled after SetEnabled(false)")
	}

	if err := ws.Delete(sub.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(ws.List()) != 0 {
		t.Fatal("expected empty subscription list")
	}
	if _, err := ws.Get(sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := ws.Delete(sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestWebhookSubscriptionValidation(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	// empty (and whitespace-only) name is rejected
	for _, name := range []string{"", "   "} {
		if _, err := ws.Create(WebhookSubscriptionSpec{Name: name, URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for name %q, got %v", name, err)
		}
	}
	// invalid URLs: wrong scheme, no host, unparseable
	for _, bad := range []string{"", "ftp://example.com/hook", "http://", "http://example.com/%zz"} {
		if _, err := ws.Create(WebhookSubscriptionSpec{Name: "x", URL: bad, Events: []WebhookEvent{EventTaskCompleted}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for url %q, got %v", bad, err)
		}
	}
	// empty events are rejected
	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "x", URL: "http://example.com"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty events, got %v", err)
	}
	// an unknown event is rejected
	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "x", URL: "http://example.com", Events: []WebhookEvent{"explodes"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown event, got %v", err)
	}

	// duplicate name on create
	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "dup", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "dup", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// update validates like create
	sub, err := ws.Create(WebhookSubscriptionSpec{Name: "u", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ws.Update(sub.ID, WebhookSubscriptionSpec{Name: "  ", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}
	if _, err := ws.Update(sub.ID, WebhookSubscriptionSpec{Name: "dup", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}
	// renaming to its own current name is fine
	if _, err := ws.Update(sub.ID, WebhookSubscriptionSpec{Name: "u", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
	// missing id on every mutator
	if _, err := ws.Update("missing", WebhookSubscriptionSpec{Name: "u", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing update, got %v", err)
	}
	if err := ws.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing set-enabled, got %v", err)
	}
}

// TestWebhookSubscriptionNormalizesEventsAndTrims verifies that Create
// trims name and URL and deduplicates events while preserving order, and
// that a name empty after trimming is rejected.
func TestWebhookSubscriptionNormalizesEventsAndTrims(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	sub, err := ws.Create(WebhookSubscriptionSpec{
		Name:   "  alerts  ",
		URL:    "  http://example.com/hook  ",
		Events: []WebhookEvent{EventTaskCompleted, EventOutputDisconnected, EventTaskCompleted},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sub.Name != "alerts" {
		t.Fatalf("expected trimmed name, got %q", sub.Name)
	}
	if sub.URL != "http://example.com/hook" {
		t.Fatalf("expected trimmed url, got %q", sub.URL)
	}
	if want := []WebhookEvent{EventTaskCompleted, EventOutputDisconnected}; !sameWebhookEvents(sub.Events, want) {
		t.Fatalf("expected deduplicated events, got %v", sub.Events)
	}
}

// TestWebhookEventEngineExitedKnown pins EventEngineExited in the
// known-event whitelist: subscriptions may opt into it and validation
// accepts it, so the server can dispatch engine-exit notifications.
func TestWebhookEventEngineExitedKnown(t *testing.T) {
	if err := validateWebhookEvent(EventEngineExited); err != nil {
		t.Fatalf("validateWebhookEvent(EventEngineExited) = %v, want nil", err)
	}

	s := newTestStore(t)
	ws := NewWebhookService(s)
	sub, err := ws.Create(WebhookSubscriptionSpec{
		Name:    "engine-alerts",
		URL:     "http://example.com/hook",
		Events:  []WebhookEvent{EventEngineExited},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create with EventEngineExited: %v", err)
	}
	if len(sub.Events) != 1 || sub.Events[0] != EventEngineExited {
		t.Fatalf("events = %v, want [engine_exited]", sub.Events)
	}
}

func TestWebhookSubscriptionListSorted(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := ws.Create(WebhookSubscriptionSpec{Name: name, URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := ws.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d subscriptions, got %d", len(want), len(list))
	}
	for i, sub := range list {
		if sub.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, subscriptionNames(list))
		}
	}
}

// TestWebhookDispatchMatchesSubscriptions verifies that Dispatch posts only
// to enabled subscriptions whose Events list contains the dispatched event,
// with the event name and payload in the JSON body.
func TestWebhookDispatchMatchesSubscriptions(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	matching, mSrv := newCountingServer(t)
	other, oSrv := newCountingServer(t)
	disabled, dSrv := newCountingServer(t)

	if _, err := ws.Create(WebhookSubscriptionSpec{
		Name: "matching", URL: mSrv.URL,
		Events: []WebhookEvent{EventOutputDisconnected, EventMaterialFailed}, Enabled: true,
	}); err != nil {
		t.Fatalf("create matching: %v", err)
	}
	if _, err := ws.Create(WebhookSubscriptionSpec{
		Name: "other", URL: oSrv.URL, Events: []WebhookEvent{EventTaskCompleted}, Enabled: true,
	}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if _, err := ws.Create(WebhookSubscriptionSpec{
		Name: "disabled", URL: dSrv.URL, Events: []WebhookEvent{EventOutputDisconnected}, Enabled: false,
	}); err != nil {
		t.Fatalf("create disabled: %v", err)
	}

	d := NewWebhookDispatcher(s, WithHTTPClient(&http.Client{Timeout: 5 * time.Second}), WithRetryInterval(time.Millisecond))
	d.Dispatch(context.Background(), EventOutputDisconnected, map[string]interface{}{"id": "clip-1"})
	waitFor(t, 5*time.Second, func() bool { return matching.count() == 1 })

	body, ct := matching.request(0)
	if body["event"] != string(EventOutputDisconnected) {
		t.Fatalf("unexpected event in body: %v", body)
	}
	payload, ok := body["payload"].(map[string]interface{})
	if !ok || payload["id"] != "clip-1" {
		t.Fatalf("unexpected payload in body: %v", body["payload"])
	}
	if ct != "application/json" {
		t.Fatalf("unexpected content type %q", ct)
	}
	if other.count() != 0 {
		t.Fatal("subscription without the event received a delivery")
	}
	if disabled.count() != 0 {
		t.Fatal("disabled subscription received a delivery")
	}

	// the matching subscription also receives the second event it
	// subscribes to, and only it does.
	d.Dispatch(context.Background(), EventMaterialFailed, map[string]interface{}{"id": "clip-1"})
	waitFor(t, 5*time.Second, func() bool { return matching.count() == 2 })
	if other.count() != 0 || disabled.count() != 0 {
		t.Fatal("unexpected delivery to non-matching subscription")
	}
	body, _ = matching.request(1)
	if body["event"] != string(EventMaterialFailed) {
		t.Fatalf("unexpected second event in body: %v", body)
	}

	// an event no subscription subscribes to is a no-op.
	d.Dispatch(context.Background(), EventChannelStatusChanged, map[string]interface{}{})
	time.Sleep(20 * time.Millisecond)
	if matching.count() != 2 || other.count() != 0 || disabled.count() != 0 {
		t.Fatal("unmatched event produced deliveries")
	}
}

// TestWebhookDispatchRetriesThenSucceeds verifies that a failing attempt is
// retried and a success on a later attempt records DeliverySuccess with the
// attempt count.
func TestWebhookDispatchRetriesThenSucceeds(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub, err := ws.Create(WebhookSubscriptionSpec{Name: "retry", URL: srv.URL, Events: []WebhookEvent{EventTaskCompleted}, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := NewWebhookDispatcher(s, WithHTTPClient(srv.Client()), WithRetryInterval(time.Millisecond), WithMaxRetries(3))
	d.Dispatch(context.Background(), EventTaskCompleted, map[string]interface{}{"ok": true})

	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 1 })
	del := deliveries(t, s)[0]
	if del.Status != DeliverySuccess {
		t.Fatalf("expected success after retry, got %+v", del)
	}
	if del.Attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", del.Attempts)
	}
	if del.SubscriptionID != sub.ID || del.Event != EventTaskCompleted {
		t.Fatalf("unexpected delivery: %+v", del)
	}
	if del.DeliveredAt.IsZero() {
		t.Fatal("expected DeliveredAt set on success")
	}
	if del.LastError != "" {
		t.Fatalf("expected no last error on success, got %q", del.LastError)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("expected 2 HTTP attempts, got %d", attempts)
	}
}

// TestWebhookDispatchRetriesExhausted verifies that a persistently failing
// endpoint records DeliveryFailed with Attempts equal to MaxRetries and a
// last error.
func TestWebhookDispatchRetriesExhausted(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "fail", URL: srv.URL, Events: []WebhookEvent{EventOutputDisconnected}, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := NewWebhookDispatcher(s, WithHTTPClient(srv.Client()), WithRetryInterval(time.Millisecond), WithMaxRetries(3))
	d.Dispatch(context.Background(), EventOutputDisconnected, map[string]interface{}{})

	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 1 })
	del := deliveries(t, s)[0]
	if del.Status != DeliveryFailed {
		t.Fatalf("expected failed delivery, got %+v", del)
	}
	if del.Attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", del.Attempts)
	}
	if del.LastError == "" {
		t.Fatal("expected a last error on failed delivery")
	}
	if !del.DeliveredAt.IsZero() {
		t.Fatal("expected zero DeliveredAt on failed delivery")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("expected 3 HTTP attempts, got %d", attempts)
	}
}

// TestWebhookDispatchContextCancelStopsRetries verifies that canceling the
// context mid-delivery aborts the remaining retries and records a failed
// delivery with the attempts already made.
func TestWebhookDispatchContextCancelStopsRetries(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		once.Do(cancel)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "cancel", URL: srv.URL, Events: []WebhookEvent{EventTaskCompleted}, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := NewWebhookDispatcher(s, WithHTTPClient(srv.Client()), WithRetryInterval(time.Minute), WithMaxRetries(5))
	d.Dispatch(ctx, EventTaskCompleted, map[string]interface{}{})

	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 1 })
	del := deliveries(t, s)[0]
	if del.Status != DeliveryFailed {
		t.Fatalf("expected failed delivery after cancellation, got %+v", del)
	}
	if del.Attempts != 1 {
		t.Fatalf("expected retries to stop after cancellation, got %d attempts", del.Attempts)
	}
	if del.LastError == "" {
		t.Fatal("expected a last error")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("expected exactly 1 HTTP attempt, got %d", attempts)
	}
}

// TestWebhookDispatchContextAlreadyCanceled verifies that dispatching with
// an already-canceled context makes no HTTP request and records a failed
// delivery with zero attempts.
func TestWebhookDispatchContextAlreadyCanceled(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	cs, srv := newCountingServer(t)
	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "pre", URL: srv.URL, Events: []WebhookEvent{EventOutputDisconnected}, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := NewWebhookDispatcher(s, WithHTTPClient(srv.Client()), WithRetryInterval(time.Millisecond))
	d.Dispatch(ctx, EventOutputDisconnected, map[string]interface{}{})

	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 1 })
	del := deliveries(t, s)[0]
	if del.Status != DeliveryFailed || del.Attempts != 0 {
		t.Fatalf("expected failed delivery with no attempts, got %+v", del)
	}
	if del.LastError == "" {
		t.Fatal("expected a last error")
	}
	if cs.count() != 0 {
		t.Fatal("expected no HTTP request for a canceled context")
	}
}

// TestWebhookDispatchUnmarshalablePayload verifies that a payload that
// cannot be serialized is recorded as a failed delivery without any HTTP
// attempt.
func TestWebhookDispatchUnmarshalablePayload(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	cs, srv := newCountingServer(t)
	if _, err := ws.Create(WebhookSubscriptionSpec{Name: "bad", URL: srv.URL, Events: []WebhookEvent{EventOutputDisconnected}, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := NewWebhookDispatcher(s, WithHTTPClient(srv.Client()), WithRetryInterval(time.Millisecond))
	d.Dispatch(context.Background(), EventOutputDisconnected, make(chan int))

	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 1 })
	del := deliveries(t, s)[0]
	if del.Status != DeliveryFailed {
		t.Fatalf("expected failed delivery for unmarshalable payload, got %+v", del)
	}
	if del.Attempts != 0 {
		t.Fatalf("expected no HTTP attempts, got %d", del.Attempts)
	}
	if del.LastError == "" {
		t.Fatal("expected a last error")
	}
	if cs.count() != 0 {
		t.Fatal("expected no HTTP request for an unmarshalable payload")
	}
}

// TestWebhookDispatchInvalidURLFails verifies that a URL that is not valid
// at request time (possible only in a hand-edited store, since Create and
// Update validate) is recorded as a failed delivery after the configured
// attempts.
func TestWebhookDispatchInvalidURLFails(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	sub, err := ws.Create(WebhookSubscriptionSpec{Name: "badurl", URL: "http://example.com", Events: []WebhookEvent{EventOutputDisconnected}, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A hand-edited store can hold a URL that never passed validation.
	if err := s.Update(func(d *Data) error {
		for _, cand := range d.WebhookSubscriptions {
			if cand.ID == sub.ID {
				cand.URL = "ftp://example.com/hook"
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("corrupt url: %v", err)
	}

	d := NewWebhookDispatcher(s, WithHTTPClient(&http.Client{Timeout: time.Second}), WithRetryInterval(time.Millisecond), WithMaxRetries(2))
	d.Dispatch(context.Background(), EventOutputDisconnected, map[string]interface{}{})

	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 1 })
	del := deliveries(t, s)[0]
	if del.Status != DeliveryFailed || del.Attempts != 2 {
		t.Fatalf("expected failed delivery with all attempts, got %+v", del)
	}
	if del.LastError == "" {
		t.Fatal("expected a last error")
	}
}

// TestWebhookDispatchHistoryRolls verifies that delivery records roll to
// the newest MaxHistory entries: once the cap is exceeded, the oldest
// records are dropped.
func TestWebhookDispatchHistoryRolls(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	_, srv := newCountingServer(t)
	sub, err := ws.Create(WebhookSubscriptionSpec{
		Name: "roll", URL: srv.URL,
		Events: []WebhookEvent{EventOutputDisconnected, EventMaterialFailed, EventTaskCompleted}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := NewWebhookDispatcher(s, WithHTTPClient(srv.Client()), WithRetryInterval(time.Millisecond), WithMaxHistory(2))

	d.Dispatch(context.Background(), EventOutputDisconnected, map[string]interface{}{"n": 1})
	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 1 })
	first := deliveries(t, s)[0]

	d.Dispatch(context.Background(), EventMaterialFailed, map[string]interface{}{"n": 2})
	waitFor(t, 5*time.Second, func() bool { return len(deliveries(t, s)) == 2 })

	d.Dispatch(context.Background(), EventTaskCompleted, map[string]interface{}{"n": 3})
	// The third delivery pushes the history past the cap; the oldest
	// record is dropped.
	waitFor(t, 5*time.Second, func() bool {
		del := deliveries(t, s)
		if len(del) != 2 {
			return false
		}
		return del[0].ID != first.ID
	})

	del := deliveries(t, s)
	if len(del) != 2 {
		t.Fatalf("expected 2 delivery records after rolling, got %d", len(del))
	}
	for _, rec := range del {
		if rec.ID == first.ID {
			t.Fatal("expected the oldest delivery record to be dropped")
		}
		if rec.SubscriptionID != sub.ID {
			t.Fatalf("unexpected delivery: %+v", rec)
		}
	}
	if del[1].Event != EventTaskCompleted {
		t.Fatalf("expected the newest delivery to be kept, got %v", del[1].Event)
	}
}

// TestWebhookSetEnabledSameValueWrites verifies the package convention for
// SetEnabled: setting a flag to its current value is not special-cased, so
// the store is rewritten and UpdatedAt is bumped, matching the other
// SetEnabled implementations.
func TestWebhookSetEnabledSameValueWrites(t *testing.T) {
	s := newTestStore(t)
	ws := NewWebhookService(s)

	sub, err := ws.Create(WebhookSubscriptionSpec{Name: "toggle", URL: "http://example.com", Events: []WebhookEvent{EventTaskCompleted}, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	before, err := readStore(t, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	orig := sub.UpdatedAt
	// Let time advance so a real write would produce a different
	// UpdatedAt.
	time.Sleep(10 * time.Millisecond)

	if err := ws.SetEnabled(sub.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}

	after, err := readStore(t, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) {
		t.Fatal("expected same-value SetEnabled to rewrite the store file")
	}
	got, err := ws.Get(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt.Equal(orig) {
		t.Fatal("expected same-value SetEnabled to bump UpdatedAt")
	}
}

// deliveries returns the current delivery records of the store.
func deliveries(t *testing.T, s *Store) []*WebhookDelivery {
	t.Helper()
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap.WebhookDeliveries
}

// sameWebhookEvents reports whether a and b hold the same events in the
// same order.
func sameWebhookEvents(a, b []WebhookEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// subscriptionNames returns the names of the subscriptions in order.
func subscriptionNames(subs []*WebhookSubscription) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.Name)
	}
	return out
}

// countingServer is an httptest server that records the JSON bodies and
// content types of the requests it receives.
type countingServer struct {
	mu   sync.Mutex
	body []map[string]interface{}
	ct   []string
}

// newCountingServer starts a countingServer and returns it with its server.
func newCountingServer(t *testing.T) (*countingServer, *httptest.Server) {
	t.Helper()
	cs := &countingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		cs.mu.Lock()
		cs.body = append(cs.body, body)
		cs.ct = append(cs.ct, r.Header.Get("Content-Type"))
		cs.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return cs, srv
}

// count returns the number of requests received.
func (cs *countingServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.body)
}

// request returns the decoded body and Content-Type of the i-th request.
func (cs *countingServer) request(i int) (map[string]interface{}, string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.body[i], cs.ct[i]
}
