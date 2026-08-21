package management

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// recordingPlayer is a thread-safe Player that records every Play request.
type recordingPlayer struct {
	mu    sync.Mutex
	calls []PlayRequest
}

func (p *recordingPlayer) Play(_ context.Context, req PlayRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	return nil
}

func (p *recordingPlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *recordingPlayer) snapshot() []PlayRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PlayRequest, len(p.calls))
	copy(out, p.calls)
	return out
}

func TestSchedulerIntervalFires(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")

	task, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sched.Stop()

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	reqs := player.snapshot()
	if len(reqs) == 0 {
		t.Fatal("expected the interval task to fire")
	}
	if reqs[0].MediaID != m.ID {
		t.Fatalf("want media %s, got %+v", m.ID, reqs[0])
	}
	if reqs[0].PlaylistID != "" {
		t.Fatalf("unexpected playlist target: %+v", reqs[0])
	}

	// the task's LastRun must have been persisted
	got, err := ts.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRun == nil {
		t.Fatal("expected LastRun to be recorded")
	}
}

func TestSchedulerDisabledTaskDoesNotFire(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: false}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	time.Sleep(400 * time.Millisecond)
	if n := player.count(); n != 0 {
		t.Fatalf("disabled task fired %d times", n)
	}
}

func TestSchedulerNoPlayerReportsErrNoPlayer(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	var gotErr error
	var mu sync.Mutex
	sched := NewScheduler(s, nil,
		WithTickInterval(20*time.Millisecond),
		WithErrorHandler(func(e error) { mu.Lock(); gotErr = e; mu.Unlock() }),
	)
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	waitFor(t, time.Second*2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotErr != nil
	})

	mu.Lock()
	defer mu.Unlock()
	if gotErr == nil {
		t.Fatal("expected an error to be reported")
	}
	if !errors.Is(gotErr, ErrNoPlayer) {
		t.Fatalf("expected ErrNoPlayer, got %v", gotErr)
	}
}

func TestSchedulerComputeNext(t *testing.T) {
	s := newTestStore(t)
	sched := NewScheduler(s, nil)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	iv := &ScheduleTask{Type: TaskTypeInterval, Interval: 60}
	if got := sched.computeNext(iv, from); !got.Equal(from.Add(time.Minute)) {
		t.Fatalf("interval next = %v, want %v", got, from.Add(time.Minute))
	}

	ivBad := &ScheduleTask{Type: TaskTypeInterval, Interval: 0}
	if got := sched.computeNext(ivBad, from); !got.IsZero() {
		t.Fatalf("expected zero for invalid interval, got %v", got)
	}

	cr := &ScheduleTask{Type: TaskTypeCron, Cron: "0 9 * * *"}
	got := sched.computeNext(cr, from)
	if want := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("cron next = %v, want %v", got, want)
	}

	crBad := &ScheduleTask{Type: TaskTypeCron, Cron: "not a cron"}
	if got := sched.computeNext(crBad, from); !got.IsZero() {
		t.Fatalf("expected zero for bad cron, got %v", got)
	}

	unknown := &ScheduleTask{Type: "bogus"}
	if got := sched.computeNext(unknown, from); !got.IsZero() {
		t.Fatalf("expected zero for unknown type, got %v", got)
	}
}

func TestSchedulerTickFiresCron(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "c", Type: TaskTypeCron, Cron: "* * * * *", MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)

	now := time.Now()
	sched.tick(now) // populate the schedule: next fire = next whole minute

	fireAt := now.Truncate(time.Minute).Add(time.Minute).Add(time.Second)
	sched.tick(fireAt) // fire is now due

	// Play is dispatched asynchronously, so wait for it to be recorded
	// rather than asserting immediately.
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	reqs := player.snapshot()
	if len(reqs) == 0 {
		t.Fatal("expected the cron task to fire")
	}
	if reqs[0].MediaID != m.ID {
		t.Fatalf("unexpected request: %+v", reqs[0])
	}
}

// TestSchedulerFireCarriesSceneTemplate verifies the scene template reference
// of a task reaches the Player: when the task fires, the dispatched
// PlayRequest carries the task's SceneTemplateID.
func TestSchedulerFireCarriesSceneTemplate(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	sts := NewSceneTemplateService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	tpl, err := sts.Create(SceneTemplateSpec{Name: "watermark", Kind: SceneWatermark})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "tpl", Type: TaskTypeInterval, Interval: 60, MediaID: m.ID, SceneTemplateID: tpl.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)                       // populate the schedule
	sched.tick(now.Add(60 * time.Second)) // fire is now due

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })
	reqs := player.snapshot()
	if len(reqs) == 0 {
		t.Fatal("expected the task to fire")
	}
	if reqs[0].SceneTemplateID != tpl.ID {
		t.Fatalf("fire dropped the scene template: got %q, want %q", reqs[0].SceneTemplateID, tpl.ID)
	}
	if reqs[0].MediaID != m.ID {
		t.Fatalf("unexpected request: %+v", reqs[0])
	}
}

// TestSchedulerFireWithoutSceneTemplate verifies a task that references no
// scene template fires with an empty SceneTemplateID.
func TestSchedulerFireWithoutSceneTemplate(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "plain", Type: TaskTypeInterval, Interval: 60, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)                       // populate the schedule
	sched.tick(now.Add(60 * time.Second)) // fire is now due

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })
	reqs := player.snapshot()
	if len(reqs) == 0 {
		t.Fatal("expected the task to fire")
	}
	if reqs[0].SceneTemplateID != "" {
		t.Fatalf("task without a template must fire with an empty reference, got %q", reqs[0].SceneTemplateID)
	}
}

// blockingPlayer records every call and blocks the first one until release is
// closed, exposing whether the scheduler starts overlapping Play calls.
type blockingPlayer struct {
	mu      sync.Mutex
	calls   []PlayRequest
	release chan struct{}
	once    sync.Once
}

func (p *blockingPlayer) Play(_ context.Context, req PlayRequest) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	n := len(p.calls)
	p.mu.Unlock()
	if n == 1 {
		p.once.Do(func() { <-p.release })
	}
	return nil
}

func (p *blockingPlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// TestSchedulerDedupsSlowPlayer verifies that when a task fires faster than
// its Player can finish, no overlapping Play calls are queued: the task is
// skipped while a Play is in flight and fires again once it returns.
func TestSchedulerDedupsSlowPlayer(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &blockingPlayer{release: make(chan struct{})}
	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// Wait for the first Play to start; it then blocks until release.
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })
	if n := player.count(); n != 1 {
		t.Fatalf("expected the first Play to start, got %d calls", n)
	}

	// Let the loop run well past the 1s interval while the player is blocked.
	time.Sleep(1200 * time.Millisecond)
	if n := player.count(); n != 1 {
		t.Fatalf("short interval started overlapping Play calls: got %d", n)
	}

	// Releasing the blocked Play clears the in-flight marker, so the task
	// must be able to fire again.
	close(player.release)
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })
}

// TestSchedulerInflightClearedAfterDisable verifies that disabling a task
// while its Play is in flight does not leave stale in-flight state: after
// re-enabling, the task fires again.
func TestSchedulerInflightClearedAfterDisable(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	task, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	player := &blockingPlayer{release: make(chan struct{})}
	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// Disable the task while its Play is still blocked.
	if err := ts.SetEnabled(task.ID, false); err != nil {
		t.Fatal(err)
	}
	close(player.release) // let the in-flight Play finish and clear its marker

	// Re-enable the task; it must fire again, proving no stale in-flight
	// state was left behind by the disabled run.
	if err := ts.SetEnabled(task.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })
}

func TestSchedulerStartStop(t *testing.T) {
	s := newTestStore(t)
	sched := NewScheduler(s, nil)

	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	if !sched.Running() {
		t.Fatal("expected running")
	}
	if err := sched.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}

	sched.Stop()
	if sched.Running() {
		t.Fatal("expected stopped")
	}

	// restart works after a stop
	if err := sched.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	sched.Stop()

	// Stop on a non-running scheduler is a no-op
	sched.Stop()
}

func TestSchedulerSetPlayer(t *testing.T) {
	s := newTestStore(t)
	sched := NewScheduler(s, nil)
	if sched.Player() != nil {
		t.Fatal("expected nil player initially")
	}
	p := &recordingPlayer{}
	sched.SetPlayer(p)
	if sched.Player() != p {
		t.Fatal("expected player to be set")
	}
	sched.SetPlayer(nil)
	if sched.Player() != nil {
		t.Fatal("expected player detached")
	}
}

// TestSchedulerStopEndsLoop verifies that Stop actually terminates the
// scheduler's own runtime goroutine (no further ticks fire) and that the
// scheduler can be restarted afterwards.
func TestSchedulerStopEndsLoop(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// Stop returns only once the runtime goroutine has fully exited.
	sched.Stop()
	stopped := player.count()
	// A dead loop must not keep firing, even past the next interval.
	time.Sleep(1200 * time.Millisecond)
	if n := player.count(); n != stopped {
		t.Fatalf("scheduler kept firing after Stop: got %d, want %d", n, stopped)
	}

	// The scheduler can be restarted and resumes firing.
	if err := sched.Start(); err != nil {
		t.Fatalf("restart after Stop: %v", err)
	}
	waitFor(t, time.Second*2, func() bool { return player.count() > stopped })
	sched.Stop()
}

// TestSchedulerInflightClearedAfterDelete verifies that deleting a task while
// its Play is still in flight does not leave a stale in-flight marker behind:
// the marker is cleared when Play returns.
func TestSchedulerInflightClearedAfterDelete(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	task, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	player := &blockingPlayer{release: make(chan struct{})}
	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// Delete the task while its Play is still blocked.
	if err := ts.Delete(task.ID); err != nil {
		t.Fatal(err)
	}

	sched.mu.Lock()
	inflight := sched.inflight[task.ID]
	sched.mu.Unlock()
	if !inflight {
		t.Fatal("expected the in-flight marker to remain until Play returns")
	}

	// Releasing the blocked Play must clear the marker even though the task
	// was deleted, leaving no stale state behind.
	close(player.release)
	waitFor(t, time.Second*2, func() bool {
		sched.mu.Lock()
		defer sched.mu.Unlock()
		return !sched.inflight[task.ID]
	})
}

// TestSchedulerModifyAppliesNewSchedule verifies that modifying a task while
// the scheduler is running reschedules it against the new configuration
// instead of leaving a stale schedule behind.
func TestSchedulerModifyAppliesNewSchedule(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	task, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m1.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// Retarget the task to a different media; the next fire must use it.
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	if _, err := ts.Replace(task.ID, TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m2.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, time.Second*2, func() bool {
		reqs := player.snapshot()
		return len(reqs) >= 1 && reqs[len(reqs)-1].MediaID == m2.ID
	})
}

// TestSchedulerPanicRecovered verifies that a panicking Player does not bring
// the scheduler (or the process) down: the panic is recovered and reported,
// and the in-flight marker is cleared so the task keeps firing.
func TestSchedulerPanicRecovered(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	panics := 0
	player := PlayerFunc(func(_ context.Context, _ PlayRequest) error {
		panic("player exploded")
	})
	sched := NewScheduler(s, player,
		WithTickInterval(20*time.Millisecond),
		WithErrorHandler(func(e error) {
			mu.Lock()
			panics++
			mu.Unlock()
		}),
	)
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// The task must fire repeatedly despite the panic, which proves both that
	// the panic is recovered and that the in-flight marker was cleared
	// between runs.
	waitFor(t, time.Second*3, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return panics >= 2
	})
	if !sched.Running() {
		t.Fatal("scheduler stopped running after a player panic")
	}
}

// cancelAwarePlayer records the context it is given, then blocks until that
// context is cancelled and records the cancellation error, simulating a
// Player that honours context cancellation.
type cancelAwarePlayer struct {
	mu    sync.Mutex
	ctx   context.Context
	got   error
	done  chan struct{}
	start chan struct{}
	once  sync.Once
}

func (p *cancelAwarePlayer) Play(ctx context.Context, _ PlayRequest) error {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	p.once.Do(func() { close(p.start) })
	<-ctx.Done()
	err := ctx.Err()
	p.mu.Lock()
	p.got = err
	p.mu.Unlock()
	close(p.done)
	return err
}

// TestSchedulerStopCancelsInflightPlay verifies that a Play in flight when
// Stop is called observes the context cancellation, and that Stop returns
// promptly.
func TestSchedulerStopCancelsInflightPlay(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &cancelAwarePlayer{done: make(chan struct{}), start: make(chan struct{})}
	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait until a Play is in flight (blocked on its context).
	select {
	case <-player.start:
	case <-time.After(time.Second * 2):
		t.Fatal("Play never started")
	}

	start := time.Now()
	sched.Stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stop took too long while a Play was in flight: %v", elapsed)
	}

	// The in-flight Play must have observed cancellation.
	select {
	case <-player.done:
		player.mu.Lock()
		defer player.mu.Unlock()
		if !errors.Is(player.got, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", player.got)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight Play never observed cancellation")
	}
}

// stuckPlayer records that a Play started and then blocks forever, completely
// ignoring context cancellation.
type stuckPlayer struct {
	mu      sync.Mutex
	calls   []PlayRequest
	started chan struct{}
	once    sync.Once
}

func (p *stuckPlayer) Play(_ context.Context, req PlayRequest) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	p.once.Do(func() { close(p.started) })
	select {} // ignore ctx and never return
}

func (p *stuckPlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// TestSchedulerStopDoesNotWaitForStuckPlayer verifies that Stop returns even
// when a Player ignores context cancellation and blocks forever: the
// scheduler must not join its Play goroutines.
func TestSchedulerStopDoesNotWaitForStuckPlayer(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &stuckPlayer{started: make(chan struct{})}
	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// Ensure a Play is genuinely stuck before calling Stop.
	select {
	case <-player.started:
	case <-time.After(time.Second * 2):
		t.Fatal("Play never started")
	}
	if n := player.count(); n != 1 {
		t.Fatalf("expected exactly one stuck Play, got %d", n)
	}

	done := make(chan struct{})
	go func() {
		sched.Stop()
		close(done)
	}()
	select {
	case <-done:
		// Stop returned even though a Play goroutine is blocked forever.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked waiting for a Player that ignores cancellation")
	}
}

// waitFor polls cond until it returns true or the deadline elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("condition not met within %s", timeout))
}

// ---------------------------------------------------------------------------
// Interrupt playback (batch 2A) and priority scheduling tests. These drive
// sched.tick with synthetic times so the state machine is fully
// deterministic; Play goroutines run against real wall-clock time.

// ctxPlayer records every Play request and blocks until the context it was
// given is cancelled, then records the completion and returns ctx.Err(). It
// keeps a play "in progress" until the scheduler (or a preempting fire)
// cancels it.
type ctxPlayer struct {
	mu    sync.Mutex
	calls []PlayRequest
	done  int
}

func (p *ctxPlayer) Play(ctx context.Context, req PlayRequest) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	<-ctx.Done()
	p.mu.Lock()
	p.done++
	p.mu.Unlock()
	return ctx.Err()
}

func (p *ctxPlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *ctxPlayer) completed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

func (p *ctxPlayer) snapshot() []PlayRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PlayRequest, len(p.calls))
	copy(out, p.calls)
	return out
}

// timedPlayer records every Play request, plays it for playFor and returns.
type timedPlayer struct {
	mu      sync.Mutex
	calls   []PlayRequest
	playFor time.Duration
}

func (p *timedPlayer) Play(_ context.Context, req PlayRequest) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	time.Sleep(p.playFor)
	return nil
}

func (p *timedPlayer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *timedPlayer) snapshot() []PlayRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PlayRequest, len(p.calls))
	copy(out, p.calls)
	return out
}

// TestSchedulerInterruptRestoresAfterDuration verifies the interrupt
// lifecycle: the interrupt preempts the current play, plays its own target
// for InterruptDuration seconds, and then automatically restores the
// pre-interrupt target.
func TestSchedulerInterruptRestoresAfterDuration(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &ctxPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	if _, err := ts.Create(TaskSpec{Name: "normal", Type: TaskTypeInterval, Interval: 60, MediaID: m1.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "interrupt", Type: TaskTypeInterval, Interval: 61, MediaID: m2.ID, Interrupt: true, InterruptDuration: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now) // populate the schedule

	sched.tick(now.Add(60 * time.Second)) // regular fire starts playing m1
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// The interrupt fires one second later: it preempts m1 and plays m2.
	sched.tick(now.Add(61 * time.Second))
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })
	// The preempted play must observe the cancellation.
	waitFor(t, time.Second*2, func() bool { return player.completed() >= 1 })

	// Once the 1s interrupt duration elapses, m1 is restored automatically.
	sched.tick(now.Add(62 * time.Second))
	waitFor(t, time.Second*2, func() bool { return player.count() >= 3 })

	reqs := player.snapshot()
	if len(reqs) != 3 || reqs[0].MediaID != m1.ID || reqs[1].MediaID != m2.ID || reqs[2].MediaID != m1.ID {
		t.Fatalf("unexpected play sequence: %+v", reqs)
	}
}

// TestSchedulerInterruptBlocksRegularFires verifies that while an interrupt
// is active no regular fire (not even a critical one) may start playback:
// fires are recorded (LastRun) but skipped, and the pre-interrupt target is
// restored when the interrupt duration elapses.
func TestSchedulerInterruptBlocksRegularFires(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &ctxPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	m3 := mustAddMedia(t, ms, "/v/3.mp4")
	if _, err := ts.Create(TaskSpec{Name: "normal", Type: TaskTypeInterval, Interval: 60, MediaID: m1.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "interrupt", Type: TaskTypeInterval, Interval: 61, MediaID: m2.ID, Interrupt: true, InterruptDuration: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	crit, err := ts.Create(TaskSpec{Name: "critical", Type: TaskTypeInterval, Interval: 62, MediaID: m3.ID, Priority: PriorityCritical, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)

	sched.tick(now.Add(60 * time.Second)) // m1 starts playing
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	sched.tick(now.Add(61 * time.Second)) // interrupt preempts and plays m2
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })

	// A critical regular fire during the interrupt must be skipped...
	sched.tick(now.Add(62 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if n := player.count(); n != 2 {
		t.Fatalf("regular fire interrupted the interrupt playback: got %d calls", n)
	}
	// ...but its LastRun is still recorded.
	got, err := ts.Get(crit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRun == nil {
		t.Fatal("expected LastRun to be recorded for a fire skipped during an interrupt")
	}

	// When the 10s interrupt duration elapses, m1 is restored.
	sched.tick(now.Add(71 * time.Second))
	waitFor(t, time.Second*2, func() bool { return player.count() >= 3 })

	reqs := player.snapshot()
	if len(reqs) != 3 || reqs[0].MediaID != m1.ID || reqs[1].MediaID != m2.ID || reqs[2].MediaID != m1.ID {
		t.Fatalf("unexpected play sequence: %+v", reqs)
	}
}

// TestSchedulerInterruptOneShotDoesNotRestore verifies that an interrupt with
// no duration (InterruptDuration <= 0) plays once and restores nothing when
// the playback ends.
func TestSchedulerInterruptOneShotDoesNotRestore(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &timedPlayer{playFor: 300 * time.Millisecond}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	if _, err := ts.Create(TaskSpec{Name: "normal", Type: TaskTypeInterval, Interval: 60, MediaID: m1.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "interrupt", Type: TaskTypeInterval, Interval: 61, MediaID: m2.ID, Interrupt: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)

	sched.tick(now.Add(60 * time.Second)) // m1 starts playing (300ms)
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	sched.tick(now.Add(61 * time.Second)) // one-shot interrupt plays m2
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })

	// m2 finishes on its own; nothing is restored (one-shot semantics).
	time.Sleep(600 * time.Millisecond)
	if n := player.count(); n != 2 {
		t.Fatalf("one-shot interrupt restored the previous target: got %d calls", n)
	}
}

// TestSchedulerInterruptWhileIdle verifies that an interrupt fired while the
// scheduler is idle plays its target and restores nothing when the duration
// elapses.
func TestSchedulerInterruptWhileIdle(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &ctxPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "interrupt", Type: TaskTypeInterval, Interval: 60, MediaID: m.ID, Interrupt: true, InterruptDuration: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)

	sched.tick(now.Add(60 * time.Second)) // interrupt starts while idle
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// Duration elapses: the interrupt ends and nothing is restored.
	sched.tick(now.Add(61 * time.Second))
	waitFor(t, time.Second*2, func() bool { return player.completed() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := player.count(); n != 1 {
		t.Fatalf("expected only the interrupt play, got %d calls", n)
	}
}

// TestSchedulerInterruptPublic verifies the public Interrupt entry point:
// invalid targets are rejected with ErrInvalid, a stopped scheduler reports
// an error wrapping ErrNotRunning (the interrupt lifecycle is driven by the
// run loop, so a stopped scheduler must not dispatch a play that could never
// be expired or restored), and a running scheduler starts an immediate
// one-shot interrupt.
func TestSchedulerInterruptPublic(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	player := &recordingPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")

	sched := NewScheduler(s, player)

	// Not running: Interrupt must fail before dispatching anything.
	if err := sched.Interrupt(PlayRequest{MediaID: m1.ID}, 0); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Interrupt while stopped: got %v, want ErrNotRunning", err)
	}

	// Exactly one target is required.
	if err := sched.Interrupt(PlayRequest{}, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Interrupt without target: got %v, want ErrInvalid", err)
	}
	if err := sched.Interrupt(PlayRequest{MediaID: m1.ID, PlaylistID: m2.ID}, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Interrupt with both targets: got %v, want ErrInvalid", err)
	}

	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// An immediate one-shot interrupt while idle plays its target.
	if err := sched.Interrupt(PlayRequest{MediaID: m2.ID}, 0); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })
	reqs := player.snapshot()
	if len(reqs) != 1 || reqs[0].MediaID != m2.ID {
		t.Fatalf("unexpected play sequence: %+v", reqs)
	}
}

// TestSchedulerInterruptPublicTimed verifies that a timed public interrupt
// preempts the current play and that the run loop restores the pre-interrupt
// target once the duration elapses (expireInterruptLocked is tick driven,
// which is why Interrupt requires a running scheduler).
func TestSchedulerInterruptPublicTimed(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	player := &ctxPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")

	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// A one-shot interrupt starts m1 (idle, so there is nothing to restore).
	if err := sched.Interrupt(PlayRequest{MediaID: m1.ID}, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// A timed interrupt preempts m1 and plays m2 for one second...
	if err := sched.Interrupt(PlayRequest{MediaID: m2.ID}, 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })

	// ...after which the run loop expires it and restores m1.
	waitFor(t, time.Second*3, func() bool { return player.count() >= 3 })

	reqs := player.snapshot()
	if len(reqs) != 3 || reqs[0].MediaID != m1.ID || reqs[1].MediaID != m2.ID || reqs[2].MediaID != m1.ID {
		t.Fatalf("unexpected play sequence: %+v", reqs)
	}
}

// TestSchedulerCriticalPreempts verifies that a critical fire cancels the
// current lower-priority play and takes over, and that another critical fire
// does not preempt a critical play (same priority never overrides).
func TestSchedulerCriticalPreempts(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &ctxPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	m3 := mustAddMedia(t, ms, "/v/3.mp4")
	if _, err := ts.Create(TaskSpec{Name: "normal", Type: TaskTypeInterval, Interval: 60, MediaID: m1.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "crit1", Type: TaskTypeInterval, Interval: 61, MediaID: m2.ID, Priority: PriorityCritical, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "crit2", Type: TaskTypeInterval, Interval: 62, MediaID: m3.ID, Priority: PriorityCritical, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)

	sched.tick(now.Add(60 * time.Second)) // normal m1 starts playing
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// A critical fire preempts the normal play (its context is cancelled).
	sched.tick(now.Add(61 * time.Second))
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })
	waitFor(t, time.Second*2, func() bool { return player.completed() >= 1 })

	// A second critical fire must not preempt the critical play.
	sched.tick(now.Add(62 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if n := player.count(); n != 2 {
		t.Fatalf("critical fire preempted a critical play: got %d calls", n)
	}

	reqs := player.snapshot()
	if len(reqs) != 2 || reqs[0].MediaID != m1.ID || reqs[1].MediaID != m2.ID {
		t.Fatalf("unexpected play sequence: %+v", reqs)
	}
	if reqs[0].Priority != PriorityNormal {
		t.Fatalf("expected default priority on the regular play, got %q", reqs[0].Priority)
	}
	if reqs[1].Priority != PriorityCritical {
		t.Fatalf("expected critical priority on the preempting play, got %q", reqs[1].Priority)
	}
}

// TestSchedulerImportantQueuesFIFO verifies that important fires queue behind
// a normal play (never preempting) and drain in FIFO order, and that an
// important fire is skipped while an important play is in progress (same
// priority never overrides).
func TestSchedulerImportantQueuesFIFO(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &timedPlayer{playFor: 300 * time.Millisecond}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	m3 := mustAddMedia(t, ms, "/v/3.mp4")
	if _, err := ts.Create(TaskSpec{Name: "normal", Type: TaskTypeInterval, Interval: 60, MediaID: m1.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "imp1", Type: TaskTypeInterval, Interval: 61, MediaID: m2.ID, Priority: PriorityImportant, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "imp2", Type: TaskTypeInterval, Interval: 62, MediaID: m3.ID, Priority: PriorityImportant, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)

	sched.tick(now.Add(60 * time.Second)) // normal m1 starts playing (300ms)
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// Two important fires while m1 plays: both queue, neither preempts.
	sched.tick(now.Add(61 * time.Second))
	sched.tick(now.Add(62 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if n := player.count(); n != 1 {
		t.Fatalf("important fire preempted the normal play: got %d calls", n)
	}

	// The queue drains in FIFO order as plays finish: m1, then m2, then m3.
	waitFor(t, time.Second*2, func() bool { return player.count() >= 2 })
	waitFor(t, time.Second*2, func() bool { return player.count() >= 3 })
	reqs := player.snapshot()
	if len(reqs) != 3 || reqs[0].MediaID != m1.ID || reqs[1].MediaID != m2.ID || reqs[2].MediaID != m3.ID {
		t.Fatalf("unexpected play sequence: %+v", reqs)
	}

	// An important fire while m3 (important) is still playing is skipped.
	sched.tick(now.Add(122 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if n := player.count(); n != 3 {
		t.Fatalf("important fire overrode an important play: got %d calls", n)
	}
}

// TestSchedulerNormalSkipsWhilePlaying verifies that a normal fire never
// overrides a play in progress (same priority never overrides).
func TestSchedulerNormalSkipsWhilePlaying(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &ctxPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	if _, err := ts.Create(TaskSpec{Name: "a", Type: TaskTypeInterval, Interval: 60, MediaID: m1.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "b", Type: TaskTypeInterval, Interval: 61, MediaID: m2.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player)
	now := time.Now()
	sched.tick(now)

	sched.tick(now.Add(60 * time.Second)) // a starts playing
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	sched.tick(now.Add(61 * time.Second)) // b fires while a plays: skipped
	time.Sleep(50 * time.Millisecond)
	if n := player.count(); n != 1 {
		t.Fatalf("normal fire overrode a playing normal: got %d calls", n)
	}
}

// TestSchedulerQueueFullDropsFires verifies that when the play queue is full,
// further important fires are dropped and reported through the error handler.
func TestSchedulerQueueFullDropsFires(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &ctxPlayer{}
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	if _, err := ts.Create(TaskSpec{Name: "normal", Type: TaskTypeInterval, Interval: 60, MediaID: m1.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPlayQueue+1; i++ {
		if _, err := ts.Create(TaskSpec{
			Name: fmt.Sprintf("imp%d", i), Type: TaskTypeInterval, Interval: 61 + i,
			MediaID: m2.ID, Priority: PriorityImportant, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var fullErr error
	sched := NewScheduler(s, player, WithErrorHandler(func(e error) {
		mu.Lock()
		if errors.Is(e, ErrQueueFull) {
			fullErr = e
		}
		mu.Unlock()
	}))
	now := time.Now()
	sched.tick(now)

	sched.tick(now.Add(60 * time.Second)) // normal plays, blocking
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	// Fire all important tasks: the first maxPlayQueue queue up, the rest are
	// dropped with ErrQueueFull.
	for i := 0; i < maxPlayQueue+1; i++ {
		sched.tick(now.Add(time.Duration(61+i) * time.Second))
	}
	waitFor(t, time.Second*2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fullErr != nil
	})
	mu.Lock()
	defer mu.Unlock()
	if fullErr == nil {
		t.Fatal("expected ErrQueueFull to be reported")
	}
}

// ---------------------------------------------------------------------------
// Health-policy integration: a failed Play is resolved through the
// configured HealthPolicy — auto-skip (SkippingPlayer) when enabled, alarm
// otherwise.

// skipPlayer is a Player that fails every Play with playErr and records Skip
// calls, implementing SkippingPlayer. skipErr, when non-nil, is returned by
// Skip.
type skipPlayer struct {
	mu        sync.Mutex
	playErr   error
	skipErr   error
	playCalls int
	skipCalls int
}

// compile-time assertion that skipPlayer satisfies SkippingPlayer.
var _ SkippingPlayer = (*skipPlayer)(nil)

func (p *skipPlayer) Play(_ context.Context, _ PlayRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playCalls++
	return p.playErr
}

func (p *skipPlayer) Skip(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skipCalls++
	return p.skipErr
}

func (p *skipPlayer) skips() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skipCalls
}

func (p *skipPlayer) plays() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playCalls
}

// TestSchedulerSkipOnPlayError verifies that when an enabled health policy
// with AutoSkipOnFailure is present in the store (the default provider) and
// the Player implements SkippingPlayer, a failed Play issues exactly one
// Skip and no alarm.
func TestSchedulerSkipOnPlayError(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	hs := NewHealthPolicyService(s)
	if _, err := hs.Create(HealthPolicySpec{Name: "auto", AutoSkipOnFailure: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &skipPlayer{playErr: errors.New("play failed")}
	var mu sync.Mutex
	var alarms []error
	sched := NewScheduler(s, player, WithErrorHandler(func(e error) {
		mu.Lock()
		alarms = append(alarms, e)
		mu.Unlock()
	}))
	now := time.Now()
	sched.tick(now)                  // populate the schedule
	sched.tick(now.Add(time.Second)) // fire: Play fails, policy says skip

	waitFor(t, time.Second*2, func() bool { return player.skips() >= 1 })
	if n := player.skips(); n != 1 {
		t.Fatalf("expected exactly one Skip, got %d", n)
	}
	// The skip path must not surface the failure as an alarm.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(alarms) != 0 {
		t.Fatalf("expected no alarms on a skipped failure, got %v", alarms)
	}
}

// TestSchedulerNoSkipWhenPolicyDisabled verifies that a store policy that is
// not enabled never triggers a Skip: the failure is reported as an alarm.
func TestSchedulerNoSkipWhenPolicyDisabled(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	hs := NewHealthPolicyService(s)
	// AutoSkipOnFailure set but the policy itself is not enabled.
	if _, err := hs.Create(HealthPolicySpec{Name: "auto", AutoSkipOnFailure: true}); err != nil {
		t.Fatal(err)
	}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &skipPlayer{playErr: errors.New("play failed")}
	var mu sync.Mutex
	var alarms []error
	sched := NewScheduler(s, player, WithErrorHandler(func(e error) {
		mu.Lock()
		alarms = append(alarms, e)
		mu.Unlock()
	}))
	now := time.Now()
	sched.tick(now)
	sched.tick(now.Add(time.Second)) // fire: Play fails, policy disabled

	waitFor(t, time.Second*2, func() bool { return player.plays() >= 1 })
	if n := player.skips(); n != 0 {
		t.Fatalf("expected no Skip with a disabled policy, got %d", n)
	}
	waitFor(t, time.Second*2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(alarms) >= 1
	})
}

// TestSchedulerAlarmWhenPlayerCannotSkip verifies that a policy allowing
// auto-skip does not skip when the Player only implements Player (no
// SkippingPlayer): the failure is reported, the error wraps the play error
// (so nothing panicked on the decision path), and the scheduler keeps
// running.
func TestSchedulerAlarmWhenPlayerCannotSkip(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	playErr := errors.New("play failed")
	var mu sync.Mutex
	var alarms []error
	policy := &HealthPolicy{Enabled: true, AutoSkipOnFailure: true}
	sched := NewScheduler(s, PlayerFunc(func(_ context.Context, _ PlayRequest) error {
		return playErr
	}),
		WithTickInterval(20*time.Millisecond),
		WithHealthPolicy(func() *HealthPolicy { return policy }),
		WithErrorHandler(func(e error) {
			mu.Lock()
			alarms = append(alarms, e)
			mu.Unlock()
		}),
	)
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	waitFor(t, time.Second*2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(alarms) >= 1
	})
	if !sched.Running() {
		t.Fatal("scheduler stopped after a failed play with a non-skipping player")
	}
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(alarms[0], playErr) {
		t.Fatalf("expected the play error to be reported, got %v", alarms[0])
	}
}

// TestSchedulerSkipErrorReported verifies that a failing Skip is surfaced as
// an alarm wrapping the skip error.
func TestSchedulerSkipErrorReported(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	skipErr := errors.New("skip failed")
	player := &skipPlayer{playErr: errors.New("play failed"), skipErr: skipErr}
	var mu sync.Mutex
	var alarms []error
	policy := &HealthPolicy{Enabled: true, AutoSkipOnFailure: true}
	sched := NewScheduler(s, player,
		WithHealthPolicy(func() *HealthPolicy { return policy }),
		WithErrorHandler(func(e error) {
			mu.Lock()
			alarms = append(alarms, e)
			mu.Unlock()
		}),
	)
	now := time.Now()
	sched.tick(now)
	sched.tick(now.Add(time.Second)) // fire: Play fails, Skip is attempted

	waitFor(t, time.Second*2, func() bool { return player.skips() >= 1 })
	waitFor(t, time.Second*2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(alarms) >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(alarms[0], skipErr) {
		t.Fatalf("expected the skip error to be reported, got %v", alarms[0])
	}
}

// TestSchedulerAlarmWithoutPolicy verifies that a store without any health
// policy never triggers a Skip: the failure is reported as an alarm.
func TestSchedulerAlarmWithoutPolicy(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &skipPlayer{playErr: errors.New("play failed")}
	var mu sync.Mutex
	var alarms []error
	sched := NewScheduler(s, player, WithErrorHandler(func(e error) {
		mu.Lock()
		alarms = append(alarms, e)
		mu.Unlock()
	}))
	now := time.Now()
	sched.tick(now)
	sched.tick(now.Add(time.Second)) // fire: Play fails, no policy at all

	waitFor(t, time.Second*2, func() bool { return player.plays() >= 1 })
	if n := player.skips(); n != 0 {
		t.Fatalf("expected no Skip without a policy, got %d", n)
	}
	waitFor(t, time.Second*2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(alarms) >= 1
	})
}

// cancelSkipPlayer is a Player that implements SkippingPlayer: Play blocks
// until its context is cancelled and then fails with ctx.Err(), while Skip
// is recorded (and must never fire for a scheduler-initiated cancellation).
type cancelSkipPlayer struct {
	mu        sync.Mutex
	skipCalls int
	done      int
	started   chan struct{}
	once      sync.Once
}

// compile-time assertion that cancelSkipPlayer satisfies SkippingPlayer.
var _ SkippingPlayer = (*cancelSkipPlayer)(nil)

func (p *cancelSkipPlayer) Play(ctx context.Context, _ PlayRequest) error {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	err := ctx.Err()
	p.mu.Lock()
	p.done++
	p.mu.Unlock()
	return err
}

func (p *cancelSkipPlayer) Skip(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skipCalls++
	return nil
}

func (p *cancelSkipPlayer) skips() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skipCalls
}

func (p *cancelSkipPlayer) finished() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// TestSchedulerNoSkipOnCancelledPlay verifies that a play failure caused by
// a scheduler-initiated context cancellation (here: Stop) never triggers a
// Skip, even with an enabled AutoSkipOnFailure policy and a Player that
// implements SkippingPlayer: the cancelled play was superseded deliberately,
// so skipping could act on the playback that replaced it. The failure is
// instead reported as an alarm wrapping context.Canceled.
func TestSchedulerNoSkipOnCancelledPlay(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	hs := NewHealthPolicyService(s)
	if _, err := hs.Create(HealthPolicySpec{Name: "auto", AutoSkipOnFailure: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &cancelSkipPlayer{started: make(chan struct{})}
	var mu sync.Mutex
	var alarms []error
	sched := NewScheduler(s, player,
		WithTickInterval(20*time.Millisecond),
		WithErrorHandler(func(e error) {
			mu.Lock()
			alarms = append(alarms, e)
			mu.Unlock()
		}),
	)
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for a Play to be in flight (blocked on its context).
	select {
	case <-player.started:
	case <-time.After(time.Second * 2):
		t.Fatal("Play never started")
	}

	// Stop cancels the play's context; the Play returns context.Canceled.
	sched.Stop()
	waitFor(t, time.Second*2, func() bool { return player.finished() >= 1 })

	// A cancelled play must never be skipped, policy and capability
	// notwithstanding...
	if n := player.skips(); n != 0 {
		t.Fatalf("expected no Skip for a cancelled play, got %d", n)
	}
	// ...it is surfaced as an alarm wrapping context.Canceled.
	waitFor(t, time.Second*2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(alarms) >= 1
	})
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(alarms[0], context.Canceled) {
		t.Fatalf("expected context.Canceled to be reported, got %v", alarms[0])
	}
}

// ---------------------------------------------------------------------------
// Play event reporting (batch 3E): the scheduler records one PlaySuccess
// event per play dispatched to the Player and one PlayFailure event per
// play whose Play returns an error, delivered through
// WithPlayEventHandler.

// playEventCollector records every PlayEvent delivered to it; thread-safe.
type playEventCollector struct {
	mu     sync.Mutex
	events []PlayEvent
}

func (c *playEventCollector) handler() func(PlayEvent) {
	return func(ev PlayEvent) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.events = append(c.events, ev)
	}
}

func (c *playEventCollector) snapshot() []PlayEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PlayEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *playEventCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// TestSchedulerPlayEventOnSuccess verifies that a fired task records exactly
// one PlaySuccess event carrying the task's attribution. The event is
// recorded on the dispatch path, so it is delivered before the Player sees
// the request.
func TestSchedulerPlayEventOnSuccess(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	task, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	collector := &playEventCollector{}
	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond), WithPlayEventHandler(collector.handler()))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })

	evs := collector.snapshot()
	if len(evs) != 1 {
		t.Fatalf("expected exactly one event, got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Result != PlaySuccess {
		t.Fatalf("expected PlaySuccess, got %v", ev.Result)
	}
	if ev.TaskID != task.ID {
		t.Fatalf("expected task id %q, got %q", task.ID, ev.TaskID)
	}
	if ev.TaskName != task.Name {
		t.Fatalf("expected task name %q, got %q", task.Name, ev.TaskName)
	}
	if ev.MediaID != m.ID {
		t.Fatalf("expected media %q, got %q", m.ID, ev.MediaID)
	}
	if ev.PlaylistID != "" {
		t.Fatalf("unexpected playlist on a media task: %q", ev.PlaylistID)
	}
	if ev.Detail != "" {
		t.Fatalf("unexpected detail on a success event: %q", ev.Detail)
	}
	if ev.Time.IsZero() {
		t.Fatal("expected the event time to be stamped")
	}
}

// TestSchedulerPlayEventOnFailure verifies that a failed Play records a
// PlayFailure event carrying the error, regardless of how the health policy
// resolves the failure (here: auto-skip): the attempt is reported before the
// skip decision, and the dispatch event precedes it.
func TestSchedulerPlayEventOnFailure(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	hs := NewHealthPolicyService(s)
	if _, err := hs.Create(HealthPolicySpec{Name: "auto", AutoSkipOnFailure: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	task, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	playErr := errors.New("play failed")
	player := &skipPlayer{playErr: playErr}
	collector := &playEventCollector{}
	sched := NewScheduler(s, player, WithPlayEventHandler(collector.handler()))
	now := time.Now()
	sched.tick(now)                  // populate the schedule
	sched.tick(now.Add(time.Second)) // fire: Play fails, policy says skip

	// The Skip confirms the failure was decided; the failure event is
	// recorded on that decision path before the Skip is attempted.
	waitFor(t, time.Second*2, func() bool { return player.skips() >= 1 })

	evs := collector.snapshot()
	if len(evs) != 2 {
		t.Fatalf("expected success + failure events, got %d: %+v", len(evs), evs)
	}
	if evs[0].Result != PlaySuccess {
		t.Fatalf("expected the dispatch event to be PlaySuccess, got %v", evs[0].Result)
	}
	ev := evs[1]
	if ev.Result != PlayFailure {
		t.Fatalf("expected PlayFailure, got %v", ev.Result)
	}
	if ev.Detail != playErr.Error() {
		t.Fatalf("expected the detail to carry %q, got %q", playErr.Error(), ev.Detail)
	}
	if ev.TaskID != task.ID || ev.TaskName != task.Name || ev.MediaID != m.ID {
		t.Fatalf("failure event lost task attribution: %+v", ev)
	}
}

// TestSchedulerWithoutPlayEventHandler verifies that a scheduler without an
// event handler records nothing: WithPlayEventHandler(nil) installs no
// handler, and firing proceeds exactly as before (the recording path is a
// zero-cost no-op).
func TestSchedulerWithoutPlayEventHandler(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond), WithPlayEventHandler(nil))
	if sched.onPlayEvent != nil {
		t.Fatal("WithPlayEventHandler(nil) must not install a handler")
	}
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// The task must fire and play normally with no handler installed.
	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })
}

// TestSchedulerPlayEventOnInterruptFire verifies that an interrupt fire
// records its play event like any other fire, carrying the interrupt task's
// attribution.
func TestSchedulerPlayEventOnInterruptFire(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	player := &recordingPlayer{}
	m := mustAddMedia(t, ms, "/v/1.mp4")
	task, err := ts.Create(TaskSpec{Name: "urgent", Type: TaskTypeInterval, Interval: 60, MediaID: m.ID, Interrupt: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	collector := &playEventCollector{}
	sched := NewScheduler(s, player, WithPlayEventHandler(collector.handler()))
	now := time.Now()
	sched.tick(now)
	sched.tick(now.Add(60 * time.Second)) // interrupt fire while idle

	waitFor(t, time.Second*2, func() bool { return player.count() >= 1 })
	evs := collector.snapshot()
	if len(evs) != 1 {
		t.Fatalf("expected exactly one event, got %d: %+v", len(evs), evs)
	}
	if evs[0].Result != PlaySuccess || evs[0].TaskID != task.ID || evs[0].TaskName != task.Name || evs[0].MediaID != m.ID {
		t.Fatalf("interrupt fire lost event attribution: %+v", evs[0])
	}
}

// TestSchedulerPlayEventOnCancelledPlay pins the documented semantics for
// cancelled plays: a play dispatched and then cancelled by Stop records its
// PlaySuccess event at dispatch and a PlayFailure event carrying
// context.Canceled once Play returns. The scheduler reports every attempt;
// handlers that want to ignore cancellations filter on Detail.
func TestSchedulerPlayEventOnCancelledPlay(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	if _, err := ts.Create(TaskSpec{Name: "iv", Type: TaskTypeInterval, Interval: 1, MediaID: m.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	player := &cancelAwarePlayer{done: make(chan struct{}), start: make(chan struct{})}
	collector := &playEventCollector{}
	sched := NewScheduler(s, player, WithTickInterval(20*time.Millisecond), WithPlayEventHandler(collector.handler()))
	if err := sched.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait until a Play is in flight (blocked on its context).
	select {
	case <-player.start:
	case <-time.After(time.Second * 2):
		t.Fatal("Play never started")
	}
	// The dispatch event is recorded before Play is invoked.
	if n := collector.count(); n != 1 {
		t.Fatalf("expected the dispatch event before the play started, got %d", n)
	}

	sched.Stop()
	select {
	case <-player.done:
	case <-time.After(time.Second * 2):
		t.Fatal("in-flight Play never observed cancellation")
	}
	waitFor(t, time.Second*2, func() bool { return collector.count() >= 2 })

	evs := collector.snapshot()
	if len(evs) != 2 {
		t.Fatalf("expected success + failure events, got %d: %+v", len(evs), evs)
	}
	if evs[0].Result != PlaySuccess {
		t.Fatalf("expected the dispatch event to be PlaySuccess, got %v", evs[0].Result)
	}
	ev := evs[1]
	if ev.Result != PlayFailure {
		t.Fatalf("expected PlayFailure for the cancelled play, got %v", ev.Result)
	}
	if ev.Detail != context.Canceled.Error() {
		t.Fatalf("expected the detail to carry %q, got %q", context.Canceled.Error(), ev.Detail)
	}
}
