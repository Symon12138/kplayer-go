package management

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOutputFailoverCRUD(t *testing.T) {
	s := newTestStore(t)
	fs := NewOutputFailoverService(s)

	f, err := fs.Create(OutputFailoverSpec{
		Name:             "main",
		PrimaryUnique:    "out-a",
		BackupUnique:     "out-b",
		Policy:           FailoverPolicyAutomatic,
		ThresholdSeconds: 10,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if f.ID == "" {
		t.Fatal("expected generated id")
	}
	if f.Name != "main" || f.PrimaryUnique != "out-a" || f.BackupUnique != "out-b" ||
		f.Policy != FailoverPolicyAutomatic || f.ThresholdSeconds != 10 || !f.Enabled {
		t.Fatalf("unexpected failover: %+v", f)
	}

	got, err := fs.Get(f.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "main" || got.PrimaryUnique != "out-a" {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(fs.List()) != 1 {
		t.Fatalf("expected 1 failover in list")
	}

	// Update replaces everything, including the enabled flag (full
	// replacement semantics: the spec's zero Enabled is applied).
	upd, err := fs.Update(f.ID, OutputFailoverSpec{
		Name:             "main-renamed",
		PrimaryUnique:    "out-c",
		BackupUnique:     "out-d",
		Policy:           FailoverPolicyManual,
		ThresholdSeconds: 30,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "main-renamed" || upd.PrimaryUnique != "out-c" || upd.BackupUnique != "out-d" ||
		upd.Policy != FailoverPolicyManual || upd.ThresholdSeconds != 30 || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}

	// SetEnabled toggles both ways.
	if err := fs.SetEnabled(f.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := fs.SetEnabled(f.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = fs.Get(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected failover disabled after SetEnabled(false)")
	}

	if err := fs.Delete(f.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(fs.List()) != 0 {
		t.Fatal("expected empty failover list")
	}
	if _, err := fs.Get(f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := fs.Delete(f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestOutputFailoverValidation(t *testing.T) {
	s := newTestStore(t)
	fs := NewOutputFailoverService(s)

	base := OutputFailoverSpec{
		Name:             "main",
		PrimaryUnique:    "out-a",
		BackupUnique:     "out-b",
		Policy:           FailoverPolicyAutomatic,
		ThresholdSeconds: 10,
	}

	// field-level rejections
	cases := []struct {
		name string
		spec OutputFailoverSpec
	}{
		{"empty name", func() OutputFailoverSpec { sp := base; sp.Name = "  "; return sp }()},
		{"empty primary", func() OutputFailoverSpec { sp := base; sp.PrimaryUnique = " "; return sp }()},
		{"empty backup", func() OutputFailoverSpec { sp := base; sp.BackupUnique = ""; return sp }()},
		{"same primary and backup", func() OutputFailoverSpec { sp := base; sp.BackupUnique = "out-a"; return sp }()},
		{"unknown policy", func() OutputFailoverSpec { sp := base; sp.Policy = "wat"; return sp }()},
		{"zero threshold", func() OutputFailoverSpec { sp := base; sp.ThresholdSeconds = 0; return sp }()},
		{"negative threshold", func() OutputFailoverSpec { sp := base; sp.ThresholdSeconds = -5; return sp }()},
	}
	for _, tc := range cases {
		if _, err := fs.Create(tc.spec); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for %s, got %v", tc.name, err)
		}
	}

	f1, err := fs.Create(base)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := fs.Create(OutputFailoverSpec{
		Name: "backup", PrimaryUnique: "out-x", BackupUnique: "out-y",
		Policy: FailoverPolicyAutomatic, ThresholdSeconds: 5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// duplicate name create is rejected
	if _, err := fs.Create(base); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// empty policy defaults to automatic
	def, err := fs.Create(OutputFailoverSpec{
		Name: "defaults", PrimaryUnique: "out-e", BackupUnique: "out-f", ThresholdSeconds: 5,
	})
	if err != nil {
		t.Fatalf("create with empty policy: %v", err)
	}
	if def.Policy != FailoverPolicyAutomatic {
		t.Fatalf("expected empty policy to default to automatic, got %q", def.Policy)
	}

	// references are trimmed on create
	trimmed, err := fs.Create(OutputFailoverSpec{
		Name: "trimmed", PrimaryUnique: " out-g ", BackupUnique: " out-h ",
		Policy: FailoverPolicyManual, ThresholdSeconds: 5,
	})
	if err != nil {
		t.Fatalf("create with spaced references: %v", err)
	}
	if trimmed.PrimaryUnique != "out-g" || trimmed.BackupUnique != "out-h" {
		t.Fatalf("references not trimmed: %+v", trimmed)
	}

	// invalid update is rejected and leaves the failover untouched
	if _, err := fs.Update(f1.ID, OutputFailoverSpec{Name: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}

	// rename onto an existing name is rejected
	if _, err := fs.Update(f1.ID, OutputFailoverSpec{
		Name: "backup", PrimaryUnique: "out-a", BackupUnique: "out-b", ThresholdSeconds: 10,
	}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}

	// renaming to its own current name is fine
	if _, err := fs.Update(f1.ID, OutputFailoverSpec{
		Name: "main", PrimaryUnique: "out-a", BackupUnique: "out-b", ThresholdSeconds: 10,
	}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}

	// missing id fails with ErrNotFound on every mutator
	if _, err := fs.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := fs.Update("missing", base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := fs.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestOutputFailoverListSorted(t *testing.T) {
	s := newTestStore(t)
	fs := NewOutputFailoverService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := fs.Create(OutputFailoverSpec{
			Name: name, PrimaryUnique: "p-" + name, BackupUnique: "b-" + name, ThresholdSeconds: 5,
		}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := fs.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d failovers, got %d", len(want), len(list))
	}
	for i, f := range list {
		if f.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, failoverNames(list))
		}
	}
}

// TestOutputFailoverMissingFieldCompat verifies a store file written before
// failovers existed (no outputFailovers key) opens cleanly with an empty
// failover collection and stays usable.
func TestOutputFailoverMissingFieldCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	legacy := `{
  "media": [],
  "playlists": [],
  "alarms": [],
  "tasks": [],
  "updated_at": "2026-01-01T00:00:00Z"
}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	fs := NewOutputFailoverService(s)
	if len(fs.List()) != 0 {
		t.Fatal("expected no failovers in legacy store")
	}
	f, err := fs.Create(OutputFailoverSpec{
		Name: "main", PrimaryUnique: "out-a", BackupUnique: "out-b", ThresholdSeconds: 5,
	})
	if err != nil {
		t.Fatalf("create in legacy store: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := NewOutputFailoverService(reopened).Get(f.ID)
	if err != nil {
		t.Fatalf("failover lost after reopen: %v", err)
	}
	if got.Name != "main" {
		t.Fatalf("unexpected failover: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// FailoverMonitor state machine tests. The monitor is driven tick by tick
// with synthetic times so the threshold and switch-back logic is fully
// deterministic; the run loop itself is exercised by the lifecycle test.

// fakeStateReader is an OutputStateReader whose report can be swapped by
// tests between ticks.
type fakeStateReader struct {
	mu     sync.Mutex
	states []OutputState
	err    error
}

func (r *fakeStateReader) set(states []OutputState, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = states
	r.err = err
}

func (r *fakeStateReader) OutputStates() ([]OutputState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states, r.err
}

// fakeSwitcher records ActivateOutput calls; an optional per-call error can
// be injected.
type fakeSwitcher struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (sw *fakeSwitcher) setErr(err error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.err = err
}

func (sw *fakeSwitcher) ActivateOutput(unique string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.calls = append(sw.calls, unique)
	return sw.err
}

func (sw *fakeSwitcher) snapshot() []string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	out := make([]string, len(sw.calls))
	copy(out, sw.calls)
	return out
}

// mustCreateFailover registers an enabled failover and fails the test on
// error.
func mustCreateFailover(t *testing.T, s *Store, name, primary, backup string, policy FailoverPolicy, threshold int) *OutputFailover {
	t.Helper()
	f, err := NewOutputFailoverService(s).Create(OutputFailoverSpec{
		Name:             name,
		PrimaryUnique:    primary,
		BackupUnique:     backup,
		Policy:           policy,
		ThresholdSeconds: threshold,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("create failover %q: %v", name, err)
	}
	return f
}

// monitorRuntime returns the in-memory runtime state of one failover.
func monitorRuntime(t *testing.T, m *FailoverMonitor, id string) *failoverRuntime {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime[id]
}

func TestFailoverMonitorSwitchesAfterThreshold(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 3)

	reader := &fakeStateReader{}
	reader.set([]OutputState{
		{Unique: "out-a", Connected: false},
		{Unique: "out-b", Connected: true},
	}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw)

	t0 := time.Now()
	m.tick(t0)
	// inside the threshold: the disconnect timer is running, no switch yet
	m.tick(t0.Add(2 * time.Second))
	if n := len(sw.snapshot()); n != 0 {
		t.Fatalf("expected no switch within threshold, got %v", sw.snapshot())
	}

	// at exactly ThresholdSeconds the failover switches to the backup
	m.tick(t0.Add(3 * time.Second))
	calls := sw.snapshot()
	if len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected one switch to out-b, got %v", calls)
	}
	rt := monitorRuntime(t, m, mustFailoverID(t, s, "main"))
	if rt == nil || rt.active != "out-b" {
		t.Fatalf("expected backup recorded as active, got %+v", rt)
	}
	if !rt.switchedAt.Equal(t0.Add(3 * time.Second)) {
		t.Fatalf("expected switchedAt %v, got %v", t0.Add(3*time.Second), rt.switchedAt)
	}

	// a still-disconnected primary does not re-trigger the switch
	m.tick(t0.Add(10 * time.Second))
	if n := len(sw.snapshot()); n != 1 {
		t.Fatalf("expected no repeated switch, got %v", sw.snapshot())
	}
}

func TestFailoverMonitorNoSwitchWithinThreshold(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 5)

	reader := &fakeStateReader{}
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw)

	t0 := time.Now()
	for _, at := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 4999 * time.Millisecond} {
		m.tick(t0.Add(at))
	}
	if n := len(sw.snapshot()); n != 0 {
		t.Fatalf("expected no switch before threshold, got %v", sw.snapshot())
	}

	// a reconnect inside the threshold resets the timer
	reader.set([]OutputState{{Unique: "out-a", Connected: true}}, nil)
	m.tick(t0.Add(5 * time.Second))
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)
	m.tick(t0.Add(6 * time.Second))
	if n := len(sw.snapshot()); n != 0 {
		t.Fatalf("expected reconnect to reset the timer, got %v", sw.snapshot())
	}

	// the timer restarts from the new disconnect
	m.tick(t0.Add(6 * time.Second).Add(5 * time.Second))
	calls := sw.snapshot()
	if len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected one switch after the restarted timer, got %v", calls)
	}
}

func TestFailoverMonitorAutomaticSwitchBack(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 3)

	reader := &fakeStateReader{}
	reader.set([]OutputState{
		{Unique: "out-a", Connected: false},
		{Unique: "out-b", Connected: true},
	}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw)

	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(3 * time.Second))
	if calls := sw.snapshot(); len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected switch to backup first, got %v", calls)
	}

	// the primary recovers: an automatic failover switches back immediately
	reader.set([]OutputState{
		{Unique: "out-a", Connected: true},
		{Unique: "out-b", Connected: true},
	}, nil)
	m.tick(t0.Add(4 * time.Second))
	calls := sw.snapshot()
	if len(calls) != 2 || calls[1] != "out-a" {
		t.Fatalf("expected automatic switch back to out-a, got %v", calls)
	}
	rt := monitorRuntime(t, m, mustFailoverID(t, s, "main"))
	if rt == nil || rt.active != "out-a" {
		t.Fatalf("expected primary recorded as active, got %+v", rt)
	}

	// once back on the primary, another primary outage switches again
	reader.set([]OutputState{
		{Unique: "out-a", Connected: false},
		{Unique: "out-b", Connected: true},
	}, nil)
	m.tick(t0.Add(5 * time.Second))
	m.tick(t0.Add(8 * time.Second))
	if calls := sw.snapshot(); len(calls) != 3 || calls[2] != "out-b" {
		t.Fatalf("expected a second switch to backup, got %v", calls)
	}
}

func TestFailoverMonitorManualKeepsBackup(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyManual, 3)

	reader := &fakeStateReader{}
	reader.set([]OutputState{
		{Unique: "out-a", Connected: false},
		{Unique: "out-b", Connected: true},
	}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw)

	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(3 * time.Second))
	if calls := sw.snapshot(); len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected switch to backup, got %v", calls)
	}

	// the primary recovers: a manual failover stays on the backup
	reader.set([]OutputState{
		{Unique: "out-a", Connected: true},
		{Unique: "out-b", Connected: true},
	}, nil)
	m.tick(t0.Add(4 * time.Second))
	m.tick(t0.Add(10 * time.Second))
	if calls := sw.snapshot(); len(calls) != 1 {
		t.Fatalf("expected no switch back under manual policy, got %v", calls)
	}
	rt := monitorRuntime(t, m, mustFailoverID(t, s, "main"))
	if rt == nil || rt.active != "out-b" {
		t.Fatalf("expected backup still active, got %+v", rt)
	}
}

func TestFailoverMonitorSwitchesWhenBothDown(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 1)

	reader := &fakeStateReader{}
	reader.set([]OutputState{
		{Unique: "out-a", Connected: false},
		{Unique: "out-b", Connected: false},
	}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw)

	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(time.Second))
	calls := sw.snapshot()
	if len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected switch to backup even with backup down, got %v", calls)
	}
}

func TestFailoverMonitorMissingOutputTreatedDisconnected(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 1)

	// the reader does not report out-a at all: it counts as disconnected
	reader := &fakeStateReader{}
	reader.set([]OutputState{{Unique: "out-b", Connected: true}}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw)

	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(time.Second))
	if calls := sw.snapshot(); len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected unlisted primary to be treated as down, got %v", calls)
	}
}

func TestFailoverMonitorDisabledFailoverIgnored(t *testing.T) {
	s := newTestStore(t)
	if _, err := NewOutputFailoverService(s).Create(OutputFailoverSpec{
		Name: "main", PrimaryUnique: "out-a", BackupUnique: "out-b",
		Policy: FailoverPolicyAutomatic, ThresholdSeconds: 1, Enabled: false,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	reader := &fakeStateReader{}
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw)

	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(10 * time.Second))
	if n := len(sw.snapshot()); n != 0 {
		t.Fatalf("disabled failover must not switch, got %v", sw.snapshot())
	}
	// disabled failovers never enter the in-memory state
	if rt := monitorRuntime(t, m, "missing-id"); rt != nil {
		t.Fatalf("disabled failover must not have runtime state, got %+v", rt)
	}
}

func TestFailoverMonitorReaderErrorReported(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 1)

	reader := &fakeStateReader{}
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)
	sw := &fakeSwitcher{}

	var gotErr error
	var mu sync.Mutex
	m := NewFailoverMonitor(s, reader, sw, WithMonitorErrorHandler(func(e error) {
		mu.Lock()
		gotErr = e
		mu.Unlock()
	}))

	boom := errors.New("reader boom")
	reader.set(nil, boom)
	m.tick(time.Now())
	mu.Lock()
	err := gotErr
	mu.Unlock()
	if !errors.Is(err, boom) {
		t.Fatalf("expected reader error reported, got %v", err)
	}
	if n := len(sw.snapshot()); n != 0 {
		t.Fatalf("no decision may be made on a failed read, got %v", sw.snapshot())
	}

	// a later successful read resumes normal evaluation
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)
	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(time.Second))
	if calls := sw.snapshot(); len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected switch after the reader recovered, got %v", calls)
	}
}

func TestFailoverMonitorSwitcherErrorRetries(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 1)

	reader := &fakeStateReader{}
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)
	sw := &fakeSwitcher{}

	var errs []error
	var mu sync.Mutex
	m := NewFailoverMonitor(s, reader, sw, WithMonitorErrorHandler(func(e error) {
		mu.Lock()
		errs = append(errs, e)
		mu.Unlock()
	}))

	t0 := time.Now()
	m.tick(t0)
	sw.setErr(errors.New("activate boom"))
	m.tick(t0.Add(time.Second))
	m.tick(t0.Add(2 * time.Second))
	mu.Lock()
	reported := len(errs)
	mu.Unlock()
	if reported != 2 {
		t.Fatalf("expected 2 failed switches reported, got %d", reported)
	}
	// a failed switch is not recorded, so the backup is not active yet
	if rt := monitorRuntime(t, m, mustFailoverID(t, s, "main")); rt == nil || rt.active != "out-a" {
		t.Fatalf("failed switch must not change the active output, got %+v", rt)
	}

	// the next tick retries and succeeds; afterwards no more attempts
	sw.setErr(nil)
	m.tick(t0.Add(3 * time.Second))
	if calls := sw.snapshot(); len(calls) != 3 || calls[2] != "out-b" {
		t.Fatalf("expected the retried switch to succeed, got %v", calls)
	}
	m.tick(t0.Add(4 * time.Second))
	if calls := sw.snapshot(); len(calls) != 3 {
		t.Fatalf("expected no switch after success, got %v", calls)
	}
	if rt := monitorRuntime(t, m, mustFailoverID(t, s, "main")); rt == nil || rt.active != "out-b" {
		t.Fatalf("expected backup recorded after the successful switch, got %+v", rt)
	}
}

func TestFailoverMonitorNoSwitcherReportsErrNoSwitcher(t *testing.T) {
	s := newTestStore(t)
	mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 1)

	reader := &fakeStateReader{}
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)

	var gotErr error
	var mu sync.Mutex
	m := NewFailoverMonitor(s, reader, nil, WithMonitorErrorHandler(func(e error) {
		mu.Lock()
		gotErr = e
		mu.Unlock()
	}))

	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(time.Second))
	mu.Lock()
	err := gotErr
	mu.Unlock()
	if !errors.Is(err, ErrNoSwitcher) {
		t.Fatalf("expected ErrNoSwitcher, got %v", err)
	}
}

func TestFailoverMonitorStartStopLifecycle(t *testing.T) {
	s := newTestStore(t)
	reader := &fakeStateReader{}
	reader.set(nil, nil)
	sw := &fakeSwitcher{}

	m := NewFailoverMonitor(s, reader, sw, WithMonitorTickInterval(time.Hour))
	if m.Running() {
		t.Fatal("expected a fresh monitor to be stopped")
	}
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !m.Running() {
		t.Fatal("expected monitor running after Start")
	}
	if err := m.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning on double start, got %v", err)
	}
	m.Stop()
	if m.Running() {
		t.Fatal("expected monitor stopped after Stop")
	}
	m.Stop() // safe when already stopped

	// the monitor can be restarted after a Stop
	if err := m.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !m.Running() {
		t.Fatal("expected monitor running after restart")
	}
	m.Stop()
}

// TestFailoverMonitorRestartResetsState verifies that a Stop/Start cycle
// drops the in-memory switch state: the primary is assumed active again and
// a still-disconnected primary re-triggers the switch.
func TestFailoverMonitorRestartResetsState(t *testing.T) {
	s := newTestStore(t)
	f := mustCreateFailover(t, s, "main", "out-a", "out-b", FailoverPolicyAutomatic, 1)

	reader := &fakeStateReader{}
	reader.set([]OutputState{{Unique: "out-a", Connected: false}}, nil)
	sw := &fakeSwitcher{}
	m := NewFailoverMonitor(s, reader, sw, WithMonitorTickInterval(time.Hour))

	t0 := time.Now()
	m.tick(t0)
	m.tick(t0.Add(time.Second))
	if calls := sw.snapshot(); len(calls) != 1 || calls[0] != "out-b" {
		t.Fatalf("expected switch to backup, got %v", calls)
	}
	if rt := monitorRuntime(t, m, f.ID); rt == nil || rt.active != "out-b" {
		t.Fatalf("expected backup active before restart, got %+v", rt)
	}

	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	m.Stop()
	if rt := monitorRuntime(t, m, f.ID); rt != nil {
		t.Fatalf("expected runtime state dropped by restart, got %+v", rt)
	}

	// with the primary still down, the restarted monitor counts the
	// threshold afresh and switches again
	t1 := time.Now()
	m.tick(t1)
	m.tick(t1.Add(time.Second))
	calls := sw.snapshot()
	if len(calls) != 2 || calls[1] != "out-b" {
		t.Fatalf("expected the restart to re-issue the switch, got %v", calls)
	}
}

// mustFailoverID returns the id of the failover with the given name.
func mustFailoverID(t *testing.T, s *Store, name string) string {
	t.Helper()
	for _, f := range NewOutputFailoverService(s).List() {
		if f.Name == name {
			return f.ID
		}
	}
	t.Fatalf("failover %q not found", name)
	return ""
}

// failoverNames returns the names of the failovers in order.
func failoverNames(failovers []*OutputFailover) []string {
	out := make([]string, 0, len(failovers))
	for _, f := range failovers {
		out = append(out, f.Name)
	}
	return out
}
