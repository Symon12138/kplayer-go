package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// WebhookEvent identifies a domain event that webhook subscriptions can opt
// into. Events are delivered to the endpoints of the enabled subscriptions
// that list them.
type WebhookEvent string

const (
	// EventOutputDisconnected fires when an output goes offline.
	EventOutputDisconnected WebhookEvent = "output_disconnected"
	// EventChannelStatusChanged fires when a channel reports a status
	// change.
	EventChannelStatusChanged WebhookEvent = "channel_status_changed"
	// EventMaterialFailed fires when a material fails to process.
	EventMaterialFailed WebhookEvent = "material_failed"
	// EventTaskCompleted fires when a scheduled task finishes.
	EventTaskCompleted WebhookEvent = "task_completed"
	// EventEngineExited fires when the playback engine process exits
	// abnormally (a non-zero exit code), interrupting the stream.
	EventEngineExited WebhookEvent = "engine_exited"
)

// WebhookSubscription is a registered webhook endpoint: an HTTP URL that
// receives a POST notification for every event in Events while Enabled.
type WebhookSubscription struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	URL       string         `json:"url"`
	Events    []WebhookEvent `json:"events"`
	Enabled   bool           `json:"enabled"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

// WebhookSubscriptionSpec is the validated input used to create or replace
// a webhook subscription. The name must be non-empty, the URL must be an
// http(s) URL that parses, and Events must list at least one known event;
// duplicate events are dropped when the spec is applied.
type WebhookSubscriptionSpec struct {
	Name    string
	URL     string
	Events  []WebhookEvent
	Enabled bool
}

// DeliveryStatus is the outcome of a webhook delivery.
type DeliveryStatus string

const (
	// DeliverySuccess marks a delivery acknowledged by the endpoint (an
	// HTTP 2xx response).
	DeliverySuccess DeliveryStatus = "success"
	// DeliveryFailed marks a delivery that never succeeded, even after
	// the configured number of attempts.
	DeliveryFailed DeliveryStatus = "failed"
)

// WebhookDelivery is one recorded delivery outcome for a subscription:
// which event was sent to which subscription, how many attempts were made
// and whether the endpoint eventually acknowledged it. DeliveredAt is the
// time of the successful attempt and stays zero for failed deliveries.
type WebhookDelivery struct {
	ID             string         `json:"id"`
	SubscriptionID string         `json:"subscriptionId"`
	Event          WebhookEvent   `json:"event"`
	Status         DeliveryStatus `json:"status"`
	Attempts       int            `json:"attempts"`
	LastError      string         `json:"lastError,omitempty"`
	DeliveredAt    time.Time      `json:"deliveredAt"`
	CreatedAt      time.Time      `json:"createdAt"`
}

// WebhookService provides CRUD over the webhook subscriptions of a Store.
type WebhookService struct {
	store *Store
}

// NewWebhookService returns a WebhookService backed by store.
func NewWebhookService(store *Store) *WebhookService {
	return &WebhookService{store: store}
}

// List returns all webhook subscriptions sorted by name.
func (ws *WebhookService) List() []*WebhookSubscription {
	out := make([]*WebhookSubscription, 0)
	ws.store.View(func(d *Data) {
		out = append(out, d.WebhookSubscriptions...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the webhook subscription with the given id.
func (ws *WebhookService) Get(id string) (*WebhookSubscription, error) {
	var found *WebhookSubscription
	ws.store.View(func(d *Data) {
		for _, sub := range d.WebhookSubscriptions {
			if sub.ID == id {
				found = sub
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("webhook subscription %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new webhook subscription from spec. The name must be
// non-empty (ErrInvalid), the URL must be an http(s) URL that parses
// (ErrInvalid) and Events must list at least one known event (ErrInvalid);
// duplicate events are dropped. The name must be unique among subscriptions
// (ErrExists).
func (ws *WebhookService) Create(spec WebhookSubscriptionSpec) (*WebhookSubscription, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.URL = strings.TrimSpace(spec.URL)
	if err := validateWebhookSubscriptionSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	sub := &WebhookSubscription{
		ID:        newID(),
		Name:      spec.Name,
		URL:       spec.URL,
		Events:    normalizeWebhookEvents(spec.Events),
		Enabled:   spec.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := ws.store.Update(func(d *Data) error {
		for _, exist := range d.WebhookSubscriptions {
			if exist.Name == sub.Name {
				return fmt.Errorf("webhook subscription %q: %w", sub.Name, ErrExists)
			}
		}
		d.WebhookSubscriptions = append(d.WebhookSubscriptions, sub)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// Update replaces the configuration of the subscription with the given id
// from spec: name, URL, events and the enabled flag are all replaced. The
// new name must be non-empty (ErrInvalid) and must not collide with another
// subscription (ErrExists); renaming a subscription to its own current name
// is allowed. It returns the updated subscription.
func (ws *WebhookService) Update(id string, spec WebhookSubscriptionSpec) (*WebhookSubscription, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.URL = strings.TrimSpace(spec.URL)
	if err := validateWebhookSubscriptionSpec(spec); err != nil {
		return nil, err
	}
	var out *WebhookSubscription
	err := ws.store.Update(func(d *Data) error {
		var sub *WebhookSubscription
		for _, cand := range d.WebhookSubscriptions {
			if cand.ID == id {
				sub = cand
				break
			}
		}
		if sub == nil {
			return fmt.Errorf("webhook subscription %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.WebhookSubscriptions {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("webhook subscription %q: %w", spec.Name, ErrExists)
			}
		}
		sub.Name = spec.Name
		sub.URL = spec.URL
		sub.Events = normalizeWebhookEvents(spec.Events)
		sub.Enabled = spec.Enabled
		sub.UpdatedAt = time.Now()
		out = sub
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the subscription with the given
// id. Setting a flag to its current value is not special-cased: like the
// other SetEnabled implementations of the package it rewrites the store
// and bumps UpdatedAt.
func (ws *WebhookService) SetEnabled(id string, enabled bool) error {
	return ws.update(id, func(sub *WebhookSubscription) error {
		sub.Enabled = enabled
		return nil
	})
}

// Delete removes the subscription with the given id. Delivery records of
// past deliveries are kept: they reference the subscription by id and
// remain as history.
func (ws *WebhookService) Delete(id string) error {
	return ws.store.Update(func(d *Data) error {
		idx := -1
		for i, sub := range d.WebhookSubscriptions {
			if sub.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("webhook subscription %q: %w", id, ErrNotFound)
		}
		d.WebhookSubscriptions = append(d.WebhookSubscriptions[:idx], d.WebhookSubscriptions[idx+1:]...)
		return nil
	})
}

// update applies fn to the subscription with the given id under the store
// write lock; fn may mutate the subscription in place. Returning an error
// rolls back.
func (ws *WebhookService) update(id string, fn func(sub *WebhookSubscription) error) error {
	return ws.store.Update(func(d *Data) error {
		for _, sub := range d.WebhookSubscriptions {
			if sub.ID != id {
				continue
			}
			if err := fn(sub); err != nil {
				return err
			}
			sub.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("webhook subscription %q: %w", id, ErrNotFound)
	})
}

// validateWebhookSubscriptionSpec performs field-level validation
// independent of the store: the name must be non-empty after trimming, the
// URL must be an http(s) URL that parses, and Events must list at least one
// known event.
func validateWebhookSubscriptionSpec(spec WebhookSubscriptionSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("webhook subscription: %w: empty name", ErrInvalid)
	}
	if err := validateWebhookURL(spec.URL); err != nil {
		return err
	}
	if len(spec.Events) == 0 {
		return fmt.Errorf("webhook subscription: %w: empty events", ErrInvalid)
	}
	for _, e := range spec.Events {
		if err := validateWebhookEvent(e); err != nil {
			return err
		}
	}
	return nil
}

// validateWebhookURL checks that raw parses as a URL with an http or https
// scheme and a host, which is what the dispatcher can POST to.
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook subscription: %w: invalid url %q", ErrInvalid, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook subscription: %w: url must be http(s): %q", ErrInvalid, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("webhook subscription: %w: url must have a host: %q", ErrInvalid, raw)
	}
	return nil
}

// validateWebhookEvent reports whether e is a known webhook event.
func validateWebhookEvent(e WebhookEvent) error {
	switch e {
	case EventOutputDisconnected, EventChannelStatusChanged, EventMaterialFailed, EventTaskCompleted, EventEngineExited:
		return nil
	}
	return fmt.Errorf("webhook subscription: %w: unknown event %q", ErrInvalid, e)
}

// normalizeWebhookEvents drops duplicate events while preserving order.
func normalizeWebhookEvents(events []WebhookEvent) []WebhookEvent {
	out := make([]WebhookEvent, 0, len(events))
	seen := make(map[WebhookEvent]struct{}, len(events))
	for _, e := range events {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// webhookEnvelope is the JSON body posted to every subscription endpoint.
type webhookEnvelope struct {
	Event   WebhookEvent `json:"event"`
	Payload interface{}  `json:"payload"`
}

// WebhookDispatcher delivers events to the matching webhook subscriptions
// of a Store and records every delivery outcome in the store's
// WebhookDeliveries.
//
// Dispatch is fire-and-forget from the caller's point of view: each call
// spawns one goroutine that visits the matching subscriptions one after
// another, so a slow endpoint delays the deliveries behind it but never
// blocks the caller. The store serializes the delivery records, so
// concurrent Dispatch calls are safe.
type WebhookDispatcher struct {
	store         *Store
	client        *http.Client
	maxRetries    int
	retryInterval time.Duration
	maxHistory    int
	onError       func(error)
}

// WebhookDispatcherOption configures a WebhookDispatcher.
type WebhookDispatcherOption func(*WebhookDispatcher)

// WithHTTPClient overrides the HTTP client used for deliveries; tests use
// it to inject the httptest server's client. Defaults to an http.Client
// with a 2s timeout.
func WithHTTPClient(client *http.Client) WebhookDispatcherOption {
	return func(d *WebhookDispatcher) { d.client = client }
}

// WithMaxRetries sets the number of attempts per delivery (default 3).
// Values below 1 are clamped to a single attempt.
func WithMaxRetries(n int) WebhookDispatcherOption {
	return func(d *WebhookDispatcher) { d.maxRetries = n }
}

// WithRetryInterval sets the delay between delivery attempts (default
// 500ms). The wait is interruptible: a canceled context aborts it early.
func WithRetryInterval(interval time.Duration) WebhookDispatcherOption {
	return func(d *WebhookDispatcher) { d.retryInterval = interval }
}

// WithMaxHistory caps the number of delivery records kept in the store.
// When a dispatch pushes the collection past the cap, the oldest records
// are dropped (rolling history). Defaults to 500; a non-positive value
// keeps the history unbounded.
func WithMaxHistory(n int) WebhookDispatcherOption {
	return func(d *WebhookDispatcher) { d.maxHistory = n }
}

// WithWebhookErrorHandler registers a callback invoked when the dispatcher
// hits an internal error (for example failing to write a delivery record).
// It is never called for per-delivery HTTP failures: those are recorded as
// failed deliveries in the store. (The plain WithErrorHandler name is taken
// by the scheduler option.)
func WithWebhookErrorHandler(fn func(error)) WebhookDispatcherOption {
	return func(d *WebhookDispatcher) { d.onError = fn }
}

// NewWebhookDispatcher returns a WebhookDispatcher that delivers events to
// the webhook subscriptions of store.
func NewWebhookDispatcher(store *Store, opts ...WebhookDispatcherOption) *WebhookDispatcher {
	d := &WebhookDispatcher{
		store:         store,
		client:        &http.Client{Timeout: 2 * time.Second},
		maxRetries:    3,
		retryInterval: 500 * time.Millisecond,
		maxHistory:    500,
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.maxRetries < 1 {
		d.maxRetries = 1
	}
	return d
}

// Dispatch delivers event to every enabled subscription whose Events list
// contains it and returns immediately: the deliveries run in a background
// goroutine, one subscription at a time. Each delivery is attempted up to
// maxRetries times with retryInterval between attempts, both respecting ctx
// cancellation, and the outcome is recorded in the store's
// WebhookDeliveries, which rolls to the newest maxHistory records.
// Subscriptions that are disabled or do not subscribe to the event at
// dispatch time are skipped, as is an event no subscription subscribes to.
// Internal errors are reported through the configured error handler;
// per-delivery failures are recorded, not reported.
func (d *WebhookDispatcher) Dispatch(ctx context.Context, event WebhookEvent, payload interface{}) {
	targets := make([]*WebhookSubscription, 0)
	d.store.View(func(data *Data) {
		for _, sub := range data.WebhookSubscriptions {
			if !sub.Enabled {
				continue
			}
			for _, e := range sub.Events {
				if e == event {
					targets = append(targets, sub)
					break
				}
			}
		}
	})
	if len(targets) == 0 {
		return
	}
	go func() {
		for _, sub := range targets {
			d.deliver(ctx, sub, event, payload)
		}
	}()
}

// deliver sends payload for event to one subscription, retrying as
// configured, and records the outcome.
func (d *WebhookDispatcher) deliver(ctx context.Context, sub *WebhookSubscription, event WebhookEvent, payload interface{}) {
	del := &WebhookDelivery{
		ID:             newID(),
		SubscriptionID: sub.ID,
		Event:          event,
		Status:         DeliveryFailed,
		CreatedAt:      time.Now(),
	}

	body, err := json.Marshal(webhookEnvelope{Event: event, Payload: payload})
	if err != nil {
		// An unmarshalable payload can never be sent; record it as a
		// failed delivery without any HTTP attempt.
		del.LastError = fmt.Sprintf("marshal payload: %v", err)
		d.record(del)
		return
	}
	if err := ctx.Err(); err != nil {
		del.LastError = err.Error()
		d.record(del)
		return
	}

	var lastErr error
	for attempt := 1; attempt <= d.maxRetries; attempt++ {
		del.Attempts = attempt
		if err := d.post(ctx, sub.URL, body); err == nil {
			del.Status = DeliverySuccess
			del.DeliveredAt = time.Now()
			d.record(del)
			return
		} else {
			lastErr = err
		}
		if attempt == d.maxRetries {
			break
		}
		if !d.waitRetry(ctx) {
			lastErr = ctx.Err()
			break
		}
	}
	del.LastError = lastErr.Error()
	d.record(del)
}

// waitRetry waits retryInterval unless ctx is done; it reports whether the
// wait completed.
func (d *WebhookDispatcher) waitRetry(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d.retryInterval):
		return true
	}
}

// post sends one JSON delivery attempt and reports success only for an
// HTTP 2xx response.
func (d *WebhookDispatcher) post(ctx context.Context, rawURL string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused; the body
	// of a notification response is not meaningful.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: %s returned %s", rawURL, resp.Status)
	}
	return nil
}

// record appends one delivery outcome to the store and rolls the history
// to the newest maxHistory records. Failures to write the record are
// reported through the error handler.
func (d *WebhookDispatcher) record(del *WebhookDelivery) {
	err := d.store.Update(func(data *Data) error {
		data.WebhookDeliveries = append(data.WebhookDeliveries, del)
		if d.maxHistory > 0 {
			// Records are only ever appended, so the slice head is
			// the oldest history; drop the excess.
			if excess := len(data.WebhookDeliveries) - d.maxHistory; excess > 0 {
				data.WebhookDeliveries = data.WebhookDeliveries[excess:]
			}
		}
		return nil
	})
	if err != nil && d.onError != nil {
		d.onError(fmt.Errorf("webhook: record delivery: %w", err))
	}
}
