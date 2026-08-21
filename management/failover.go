package management

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// FailoverPolicy selects how an OutputFailover behaves once the primary
// output comes back after a switch.
type FailoverPolicy string

const (
	// FailoverPolicyAutomatic switches back to the primary output as soon as
	// it reconnects. It is the default when a spec leaves Policy empty.
	FailoverPolicyAutomatic FailoverPolicy = "automatic"
	// FailoverPolicyManual switches to the backup when the primary fails but
	// never switches back on its own; an operator restores the primary.
	FailoverPolicyManual FailoverPolicy = "manual"
)

// OutputFailover is a named primary/backup output pair. While the primary
// output stays connected it is used; once it has been disconnected for at
// least ThresholdSeconds the failover switches to the backup output.
type OutputFailover struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// PrimaryUnique and BackupUnique are the unique references of the
	// primary and backup outputs; they must differ. The management side has
	// no output registry, so whether a reference resolves to a real output
	// is validated by the core side.
	PrimaryUnique string `json:"primaryUnique"`
	BackupUnique  string `json:"backupUnique"`
	// Policy selects the switch-back behavior once the primary recovers.
	Policy FailoverPolicy `json:"policy"`
	// ThresholdSeconds is how long the primary must stay disconnected
	// before the failover switches to the backup.
	ThresholdSeconds int       `json:"thresholdSeconds"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// OutputFailoverSpec is the validated input used to create or replace an
// output failover. An empty Policy defaults to FailoverPolicyAutomatic;
// PrimaryUnique and BackupUnique are trimmed before matching.
type OutputFailoverSpec struct {
	Name             string
	PrimaryUnique    string
	BackupUnique     string
	Policy           FailoverPolicy
	ThresholdSeconds int
	Enabled          bool
}

// OutputFailoverService provides CRUD over the output failovers of a Store.
type OutputFailoverService struct {
	store *Store
}

// NewOutputFailoverService returns an OutputFailoverService backed by store.
func NewOutputFailoverService(store *Store) *OutputFailoverService {
	return &OutputFailoverService{store: store}
}

// List returns all failovers sorted by name.
func (fs *OutputFailoverService) List() []*OutputFailover {
	out := make([]*OutputFailover, 0)
	fs.store.View(func(d *Data) {
		out = append(out, d.Failovers...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the failover with the given id.
func (fs *OutputFailoverService) Get(id string) (*OutputFailover, error) {
	var found *OutputFailover
	fs.store.View(func(d *Data) {
		for _, f := range d.Failovers {
			if f.ID == id {
				found = f
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("failover %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new failover from spec. The name must be non-empty and
// unique (ErrExists); the primary and backup references must be non-empty
// and differ from each other; the policy must be known and the threshold
// must be positive (ErrInvalid).
func (fs *OutputFailoverService) Create(spec OutputFailoverSpec) (*OutputFailover, error) {
	spec, err := validateOutputFailoverSpec(spec)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	f := &OutputFailover{
		ID:               newID(),
		Name:             spec.Name,
		PrimaryUnique:    spec.PrimaryUnique,
		BackupUnique:     spec.BackupUnique,
		Policy:           spec.Policy,
		ThresholdSeconds: spec.ThresholdSeconds,
		Enabled:          spec.Enabled,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	err = fs.store.Update(func(d *Data) error {
		for _, exist := range d.Failovers {
			if exist.Name == f.Name {
				return fmt.Errorf("failover %q: %w", f.Name, ErrExists)
			}
		}
		d.Failovers = append(d.Failovers, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Update replaces the configuration of the failover with the given id from
// spec: name, output references, policy, threshold and the enabled flag are
// all replaced. The new name must be non-empty (ErrInvalid) and must not
// collide with another failover (ErrExists); renaming to its own current
// name is allowed. It returns the updated failover.
func (fs *OutputFailoverService) Update(id string, spec OutputFailoverSpec) (*OutputFailover, error) {
	spec, err := validateOutputFailoverSpec(spec)
	if err != nil {
		return nil, err
	}
	var out *OutputFailover
	err = fs.store.Update(func(d *Data) error {
		var f *OutputFailover
		for _, cand := range d.Failovers {
			if cand.ID == id {
				f = cand
				break
			}
		}
		if f == nil {
			return fmt.Errorf("failover %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.Failovers {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("failover %q: %w", spec.Name, ErrExists)
			}
		}
		f.Name = spec.Name
		f.PrimaryUnique = spec.PrimaryUnique
		f.BackupUnique = spec.BackupUnique
		f.Policy = spec.Policy
		f.ThresholdSeconds = spec.ThresholdSeconds
		f.Enabled = spec.Enabled
		f.UpdatedAt = time.Now()
		out = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the failover.
func (fs *OutputFailoverService) SetEnabled(id string, enabled bool) error {
	return fs.update(id, func(f *OutputFailover) error {
		f.Enabled = enabled
		return nil
	})
}

// Delete removes the failover with the given id. No entity of the current
// document references failovers (outputs are referenced by unique, not by
// failover), so no ErrInUse guard is needed.
func (fs *OutputFailoverService) Delete(id string) error {
	return fs.store.Update(func(d *Data) error {
		idx := -1
		for i, f := range d.Failovers {
			if f.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("failover %q: %w", id, ErrNotFound)
		}
		d.Failovers = append(d.Failovers[:idx], d.Failovers[idx+1:]...)
		return nil
	})
}

// update applies fn to the failover with the given id under the store write
// lock; fn may mutate the failover in place. Returning an error rolls back.
func (fs *OutputFailoverService) update(id string, fn func(f *OutputFailover) error) error {
	return fs.store.Update(func(d *Data) error {
		for _, f := range d.Failovers {
			if f.ID != id {
				continue
			}
			if err := fn(f); err != nil {
				return err
			}
			f.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("failover %q: %w", id, ErrNotFound)
	})
}

// validateOutputFailoverSpec performs field-level validation independent of
// the store and returns the normalized spec: the name and both output
// references are trimmed, and an empty Policy defaults to
// FailoverPolicyAutomatic.
func validateOutputFailoverSpec(spec OutputFailoverSpec) (OutputFailoverSpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.PrimaryUnique = strings.TrimSpace(spec.PrimaryUnique)
	spec.BackupUnique = strings.TrimSpace(spec.BackupUnique)
	if spec.Name == "" {
		return spec, fmt.Errorf("failover: %w: empty name", ErrInvalid)
	}
	if spec.PrimaryUnique == "" {
		return spec, fmt.Errorf("failover: %w: empty primary unique", ErrInvalid)
	}
	if spec.BackupUnique == "" {
		return spec, fmt.Errorf("failover: %w: empty backup unique", ErrInvalid)
	}
	if spec.PrimaryUnique == spec.BackupUnique {
		return spec, fmt.Errorf("failover: %w: primary and backup must differ", ErrInvalid)
	}
	if spec.Policy == "" {
		spec.Policy = FailoverPolicyAutomatic
	}
	switch spec.Policy {
	case FailoverPolicyAutomatic, FailoverPolicyManual:
	default:
		return spec, fmt.Errorf("failover: %w: unknown policy %q", ErrInvalid, spec.Policy)
	}
	if spec.ThresholdSeconds <= 0 {
		return spec, fmt.Errorf("failover: %w: threshold seconds must be positive", ErrInvalid)
	}
	return spec, nil
}

// ErrNoSwitcher is reported when a failover switch is due but no
// FailoverSwitcher has been attached to the monitor, mirroring ErrNoPlayer
// for the scheduler.
var ErrNoSwitcher = errors.New("management: no failover switcher attached")

// OutputState is the connectivity snapshot of a single output as reported
// by an OutputStateReader.
type OutputState struct {
	// Unique identifies the output.
	Unique string
	// Connected reports whether the output is currently reachable.
	Connected bool
	// Error carries an optional diagnostic when the output is unhealthy.
	// The monitor only uses Connected for its decisions.
	Error string
}

// OutputStateReader supplies the current connectivity of the outputs. The
// core side (module/output provider) is expected to implement it; the
// management side stays behind the interface.
type OutputStateReader interface {
	// OutputStates returns the connectivity of every known output. An
	// output that is missing from the report is treated as disconnected.
	OutputStates() ([]OutputState, error)
}

// FailoverSwitcher performs the actual switch action. The core side is
// expected to implement it (for example removing the failing output from
// the play pipeline and adding the backup); the management side stays
// behind the interface.
type FailoverSwitcher interface {
	// ActivateOutput makes unique the active output of the pipeline.
	// Activating an output that is already active must be harmless, because
	// the monitor may re-issue a switch after a restart or a failed call.
	ActivateOutput(unique string) error
}

// FailoverMonitor watches output connectivity and switches enabled
// OutputFailovers from primary to backup. It is a standalone component with
// a Scheduler-style lifecycle: Start launches a tick loop, Stop ends it and
// the tick interval and error reporting are configurable. The monitor never
// imports core or provider packages; it only drives the abstract
// OutputStateReader and FailoverSwitcher interfaces.
//
// Decision model, evaluated once per tick per enabled failover:
//
//   - The primary is assumed to be the initially active output; no switch
//     action is issued for that assumption.
//   - While the primary is connected nothing happens.
//   - When the primary disconnects while it is the active output, a
//     disconnect timer starts. Once the disconnect has lasted at least
//     ThresholdSeconds the monitor calls switcher.ActivateOutput(backupUnique)
//     and records the backup as the active output. This applies under both
//     policies and even when the backup is itself disconnected: issuing the
//     switch is the recovery action and its outcome is the switcher's
//     business.
//   - While the backup is the active output and the primary is still
//     disconnected nothing happens.
//   - When the primary reconnects while the backup is active, an automatic
//     failover switches back (ActivateOutput(primaryUnique)); a manual one
//     stays on the backup until an operator acts.
//   - A reader error aborts the whole tick: no decision is made on partial
//     state and the error is reported through the error handler.
//   - A failed ActivateOutput is reported and not recorded, so the next
//     tick retries the switch.
//   - Only enabled failovers are evaluated; a failover that is disabled or
//     deleted drops its in-memory state.
//   - Start resets the in-memory state: after a restart the primary is
//     assumed active again and the next ticks re-derive the active output
//     from the reader, so a switch may be re-issued (harmless by the
//     ActivateOutput contract).
//
// A failover whose Policy is empty or unknown (for example edited into the
// store directly) is treated as automatic.
type FailoverMonitor struct {
	store    *Store
	reader   OutputStateReader
	switcher FailoverSwitcher

	done chan struct{}
	wg   sync.WaitGroup

	// tickInterval bounds how often the runtime loop re-evaluates the
	// failovers; sub-second values are mainly useful in tests.
	tickInterval time.Duration
	onError      func(error)

	mu      sync.Mutex
	running bool
	// runtime holds the in-memory state of each evaluated failover, keyed by
	// failover id. Guarded by mu.
	runtime map[string]*failoverRuntime
}

// failoverRuntime is the in-memory state of one failover.
type failoverRuntime struct {
	// active is the output the monitor believes is active: the primary
	// until a switch happens, then whatever ActivateOutput last succeeded
	// on.
	active string
	// switchedAt is when the last successful switch was executed; zero when
	// the failover never switched.
	switchedAt time.Time
	// primaryDownSince is when the primary was first observed disconnected
	// while the primary was still the active output; zero while the primary
	// is connected or the backup is active.
	primaryDownSince time.Time
}

// FailoverMonitorOption configures a FailoverMonitor at construction time.
type FailoverMonitorOption func(*FailoverMonitor)

// WithMonitorTickInterval overrides the runtime loop tick; it must be
// positive. It is the FailoverMonitor counterpart of the scheduler option
// WithTickInterval, which already owns the shorter name.
func WithMonitorTickInterval(d time.Duration) FailoverMonitorOption {
	return func(m *FailoverMonitor) {
		if d > 0 {
			m.tickInterval = d
		}
	}
}

// WithMonitorErrorHandler sets a callback invoked with non-fatal monitor
// errors (reader failures, failed or missing switches). It is the
// FailoverMonitor counterpart of the scheduler option WithErrorHandler,
// which already owns the shorter name.
func WithMonitorErrorHandler(fn func(error)) FailoverMonitorOption {
	return func(m *FailoverMonitor) {
		if fn != nil {
			m.onError = fn
		}
	}
}

// NewFailoverMonitor returns a FailoverMonitor backed by store. reader must
// be non-nil; switcher may be nil and attached later by the wiring layer —
// a switch that becomes due without a switcher is reported as ErrNoSwitcher.
func NewFailoverMonitor(store *Store, reader OutputStateReader, switcher FailoverSwitcher, opts ...FailoverMonitorOption) *FailoverMonitor {
	m := &FailoverMonitor{
		store:        store,
		reader:       reader,
		switcher:     switcher,
		tickInterval: time.Second,
		done:         make(chan struct{}),
		runtime:      make(map[string]*failoverRuntime),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Start launches the tick loop. It returns ErrAlreadyRunning if the monitor
// is already running. The monitor can be restarted after a Stop; each Start
// resets the in-memory state (see the FailoverMonitor documentation).
func (m *FailoverMonitor) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return ErrAlreadyRunning
	}
	m.running = true
	// Recreate the stop channel so a Stop/Start cycle never closes a closed
	// channel.
	m.done = make(chan struct{})
	// Drop the in-memory switch state: a fresh run assumes the primary is
	// active again and re-derives the active output from the reader.
	m.runtime = make(map[string]*failoverRuntime)
	m.wg.Add(1)
	go m.run()
	return nil
}

// Stop stops the loop and blocks until it has exited. It is safe to call
// multiple times and when the monitor is not running.
func (m *FailoverMonitor) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	close(m.done)
	m.mu.Unlock()
	m.wg.Wait()
}

// Running reports whether the tick loop is active.
func (m *FailoverMonitor) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *FailoverMonitor) run() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case now := <-ticker.C:
			m.tickSafely(now)
		}
	}
}

// tickSafely runs one evaluation tick, recovering a panicking reader or
// switcher so a misbehaving backend cannot take the monitor down; the panic
// is surfaced through the error handler.
func (m *FailoverMonitor) tickSafely(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			m.reportError(fmt.Errorf("failover: tick panicked: %v", r))
		}
	}()
	m.tick(now)
}

// tick evaluates the failover rules once. Callers must not hold m.mu.
func (m *FailoverMonitor) tick(now time.Time) {
	states, err := m.reader.OutputStates()
	if err != nil {
		m.reportError(fmt.Errorf("failover: read output states: %w", err))
		return
	}
	connected := make(map[string]bool, len(states))
	for _, st := range states {
		connected[st.Unique] = st.Connected
	}

	failovers := m.enabledFailovers()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range failovers {
		rt := m.runtime[f.ID]
		if rt == nil {
			rt = &failoverRuntime{active: f.PrimaryUnique}
			m.runtime[f.ID] = rt
		}
		m.evaluateLocked(f, rt, connected, now)
	}

	// Prune entries for failovers that were removed or disabled.
	for id := range m.runtime {
		if _, ok := failovers[id]; !ok {
			delete(m.runtime, id)
		}
	}
}

// enabledFailovers snapshots the enabled failovers from the store.
func (m *FailoverMonitor) enabledFailovers() map[string]*OutputFailover {
	out := make(map[string]*OutputFailover)
	m.store.View(func(d *Data) {
		for _, f := range d.Failovers {
			if f.Enabled {
				out[f.ID] = f
			}
		}
	})
	return out
}

// evaluateLocked runs the failover decision for one failover at time now.
// Must be called with m.mu held.
func (m *FailoverMonitor) evaluateLocked(f *OutputFailover, rt *failoverRuntime, connected map[string]bool, now time.Time) {
	if connected[f.PrimaryUnique] {
		// The primary is up: it is the desired active output. An automatic
		// failover that is currently on the backup switches back; a manual
		// one stays until an operator acts.
		rt.primaryDownSince = time.Time{}
		if rt.active == f.BackupUnique && f.Policy != FailoverPolicyManual {
			m.switchLocked(f, f.PrimaryUnique, rt, now)
		}
		return
	}

	// The primary is down. While it is still the active output the
	// disconnect timer runs; once the disconnect has lasted at least
	// ThresholdSeconds the failover switches to the backup.
	if rt.active == f.PrimaryUnique {
		if rt.primaryDownSince.IsZero() {
			rt.primaryDownSince = now
		}
		if now.Sub(rt.primaryDownSince) >= time.Duration(f.ThresholdSeconds)*time.Second {
			m.switchLocked(f, f.BackupUnique, rt, now)
		}
		return
	}

	// The backup is active and the primary is still down: nothing to do.
}

// switchLocked executes the switch action: it asks the FailoverSwitcher to
// activate target and, on success, records the new active output. On error
// the switch is not recorded so the next tick retries it; the error is
// reported through the error handler. Must be called with m.mu held.
func (m *FailoverMonitor) switchLocked(f *OutputFailover, target string, rt *failoverRuntime, now time.Time) {
	if m.switcher == nil {
		m.reportError(fmt.Errorf("failover %q: %w", f.Name, ErrNoSwitcher))
		return
	}
	if err := m.switcher.ActivateOutput(target); err != nil {
		m.reportError(fmt.Errorf("failover %q: activate %q: %w", f.Name, target, err))
		return
	}
	rt.active = target
	rt.switchedAt = now
	rt.primaryDownSince = time.Time{}
}

func (m *FailoverMonitor) reportError(err error) {
	if m.onError != nil {
		m.onError(err)
	}
}
