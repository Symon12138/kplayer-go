package management

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Scheduler fires persisted ScheduleTasks on their interval or cron schedule.
//
// The runtime loop wakes on a fixed tick, snapshots the enabled tasks from the
// Store and maintains an in-memory "next fire time" for each. When a task's
// next time arrives, playback is triggered exclusively through the abstract
// Player interface (never via core or provider). Tasks whose configuration
// (detected through UpdatedAt) changes while running are rescheduled
// automatically.
//
// Beyond plain firing, the scheduler implements interrupt playback (a task
// flagged Interrupt preempts the current play and, when InterruptDuration is
// positive, restores it afterwards) and fire priorities (critical preempts,
// important queues, normal plays only when idle); see fire for the exact
// rules. At most one target plays at a time: preemption and interrupt expiry
// cancel the in-flight play's context, and a task never has overlapping Play
// calls (in-flight dedup). A failed Play is resolved through the health
// policy: auto-skip (SkippingPlayer) when enabled, alarm otherwise; see
// handlePlayError.
//
// Play attempts are reported through an optional event handler
// (WithPlayEventHandler): every play dispatched to the Player records one
// PlaySuccess event, and every play whose Play returns an error records one
// PlayFailure event carrying the error, cancelled plays (preemption,
// interrupt expiry, Stop) included. Without a handler no events are
// produced.
//
// The scheduler is safe for concurrent use: Start/Stop and SetPlayer are
// guarded by locks, and the persisted LastRun update goes through Store.Update
// with copy-on-write semantics.
type Scheduler struct {
	store *Store
	done  chan struct{}
	wg    sync.WaitGroup

	playerMu sync.RWMutex
	player   Player

	// tickInterval bounds how often the runtime loop re-evaluates the
	// schedule. Its effective resolution is 1s; sub-second values are
	// mainly useful in tests.
	tickInterval time.Duration
	onError      func(error)
	// onPlayEvent is the optional callback receiving play events; nil means
	// none are recorded. Set once at construction and read from Play
	// goroutines (never under s.mu).
	onPlayEvent func(PlayEvent)
	// healthPolicy returns the policy consulted when a Play fails, or nil
	// when none applies. The default reads the first enabled policy from
	// the store on every failure, so policy changes apply without a
	// restart; WithHealthPolicy overrides it. Set once at construction and
	// read from Play goroutines (never under s.mu).
	healthPolicy func() *HealthPolicy

	mu      sync.Mutex
	running bool
	// ctx/cancel govern the lifecycle of Play goroutines for the current run.
	// Start installs a fresh cancelable context; Stop cancels it so an
	// in-flight Play can observe cancellation. Guarded by mu.
	ctx    context.Context
	cancel context.CancelFunc
	// sched maps task id -> its current schedule entry. Guarded by mu.
	sched map[string]scheduleEntry
	// inflight holds ids of tasks whose Play call has been dispatched and not
	// yet returned, so a short interval or slow player never queues
	// overlapping Play calls for the same task. Guarded by mu.
	inflight map[string]bool
	// playing is the playback currently in progress (regular, resumed or
	// interrupt), or nil when the scheduler is idle. Guarded by mu.
	playing *pendingPlay
	// queue holds plays that could not start because something else was
	// playing; only important fires ever queue (FIFO, capped by
	// maxPlayQueue). Invariant: queue is non-empty only while playing is
	// non-nil. Guarded by mu.
	queue []*pendingPlay
	// resume is the target to restore when the active interrupt ends; nil
	// when no interrupt is active or nothing was playing when it started.
	// Guarded by mu.
	resume *pendingPlay
	// interruptEnd is the wall-clock deadline of the active interrupt (zero
	// for one-shot interrupts and when none is active). Guarded by mu.
	interruptEnd time.Time
}

// maxPlayQueue caps the in-memory FIFO of plays waiting behind the current
// playback. A fire that would exceed the cap is dropped and reported through
// the error handler.
const maxPlayQueue = 8

// ErrQueueFull is reported when a play fire is dropped because the scheduler
// play queue is full.
var ErrQueueFull = errors.New("management: play queue full")

// ErrNotRunning is reported when an operation requires the scheduler runtime
// loop to be active. Interrupt returns it when the scheduler is stopped: the
// interrupt lifecycle (expiry and restore) is driven by the run loop's ticks,
// so a dispatch without a running loop could never be expired or restored.
var ErrNotRunning = errors.New("management: scheduler not running")

// StoppingPlayer is the optional capability a Player may implement for
// stop-action tasks (定时关播): fire type-asserts the attached Player to
// StoppingPlayer and calls Stop when the task's Action is TaskActionStop.
type StoppingPlayer interface {
	Stop(ctx context.Context) error
}

// SkippingPlayer is the optional capability a Player may implement to let
// the scheduler skip the failing playback instead of raising an alarm: when
// a Play fails and the health policy enables AutoSkipOnFailure, the
// scheduler type-asserts its Player to SkippingPlayer and calls Skip. Skip
// shares the context of the failed Play (abandoned on Stop or preemption)
// and should be idempotent: the scheduler never debounces repeated failures.
type SkippingPlayer interface {
	Player
	// Skip abandons the current playback and moves on to the next resource.
	Skip(ctx context.Context) error
}

// pendingPlay is a playback the scheduler has dispatched or queued: its
// request, the owning task and the cancel function of the context handed to
// the Player.
type pendingPlay struct {
	// id is the owning task id; it is empty for plays the scheduler restores
	// on its own (interrupt resumption), which never take an in-flight slot.
	id string
	// name is the owning task name, used in error reports.
	name        string
	req         PlayRequest
	priority    Priority
	cancel      context.CancelFunc
	isInterrupt bool
}

// desc returns a human-readable owner for error messages.
func (pe *pendingPlay) desc() string {
	if pe.name != "" {
		return fmt.Sprintf("task %q", pe.name)
	}
	return "play"
}

// scheduleEntry tracks the next fire time of one task and the task revision
// (UpdatedAt) it was computed from.
type scheduleEntry struct {
	updatedAt time.Time
	next      time.Time
}

// SchedulerOption configures a Scheduler at construction time.
type SchedulerOption func(*Scheduler)

// WithTickInterval overrides the runtime loop tick; it must be positive.
func WithTickInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.tickInterval = d
		}
	}
}

// WithErrorHandler sets a callback invoked with non-fatal scheduler errors
// (for example ErrNoPlayer when a task fires without an attached Player).
func WithErrorHandler(fn func(error)) SchedulerOption {
	return func(s *Scheduler) {
		if fn != nil {
			s.onError = fn
		}
	}
}

// WithPlayEventHandler sets a callback invoked for every play event the
// scheduler records: one PlaySuccess event per play dispatched to the
// Player (see startPlayLocked), and one PlayFailure event per play whose
// Play returned an error, cancelled plays included (see handlePlayError).
// The callback runs on Play goroutines, so it must be fast and safe for
// concurrent use; the server typically bridges it to PlayEventService.Record.
// Passing nil keeps the default (no events).
func WithPlayEventHandler(fn func(PlayEvent)) SchedulerOption {
	return func(s *Scheduler) {
		if fn != nil {
			s.onPlayEvent = fn
		}
	}
}

// WithHealthPolicy overrides how the scheduler obtains the health policy it
// consults when a Play fails. The default provider reads the first enabled
// policy from the store on every failure, so policy changes apply without a
// restart; an override is mainly useful to inject a deterministic policy in
// tests or to source the policy from elsewhere. The provider must be safe
// for concurrent use: it is called from Play goroutines. Passing nil keeps
// the default.
func WithHealthPolicy(provider func() *HealthPolicy) SchedulerOption {
	return func(s *Scheduler) {
		if provider != nil {
			s.healthPolicy = provider
		}
	}
}

// NewScheduler returns a Scheduler backed by store. player may be nil and
// attached later with SetPlayer; firing a task with no player reports
// ErrNoPlayer.
func NewScheduler(store *Store, player Player, opts ...SchedulerOption) *Scheduler {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	s := &Scheduler{
		store:        store,
		player:       player,
		tickInterval: time.Second,
		done:         make(chan struct{}),
		sched:        make(map[string]scheduleEntry),
		inflight:     make(map[string]bool),
		ctx:          baseCtx,
		cancel:       baseCancel,
		healthPolicy: func() *HealthPolicy { return firstEnabledPolicy(store) },
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start launches the runtime loop. It returns ErrAlreadyRunning if the
// scheduler is already running. The scheduler can be restarted after a Stop.
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return ErrAlreadyRunning
	}
	// Cancel the context of any previous run so its in-flight Play calls are
	// abandoned, then install a fresh live context for this run. The new
	// context is what fire dispatches to Play goroutines going forward.
	if s.cancel != nil {
		s.cancel()
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.running = true
	// Recreate the stop channel so a Stop/Start cycle never closes a closed
	// channel.
	s.done = make(chan struct{})
	s.wg.Add(1)
	go s.run()
	return nil
}

// Stop stops the loop and blocks until it has exited. It is safe to call
// multiple times and when the scheduler is not running.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	// Cancel so in-flight Play goroutines observe cancellation. We do not
	// wait for those goroutines here: Stop must return promptly even if a
	// Player ignores the context and blocks forever.
	s.cancel()
	close(s.done)
	// Drop the in-memory playback state: the run is over, so a restart must
	// neither resume a stale target nor drain a stale queue. In-flight Play
	// goroutines still clear their own in-flight markers when they return.
	s.playing = nil
	s.queue = nil
	s.resume = nil
	s.interruptEnd = time.Time{}
	s.mu.Unlock()
	s.wg.Wait()
}

// Running reports whether the runtime loop is active.
func (s *Scheduler) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Interrupt starts an immediate interrupt playback of req, preempting
// whatever is currently playing. Exactly one of req.MediaID and
// req.PlaylistID must be set, otherwise ErrInvalid is returned. The
// scheduler must be running, otherwise an error wrapping ErrNotRunning is
// returned: interrupt expiry and restore are driven by the run loop (see
// expireInterruptLocked, evaluated on every tick), so an interrupt started
// on a stopped scheduler could never end or restore its target.
//
// With duration > 0 the interrupt is timed: the pre-interrupt play (if any)
// is remembered as the resume target and restored once the duration elapses.
// With duration <= 0 the interrupt is one-shot: it plays until the Player
// ends it and nothing is restored. The pending play carries an empty id, so
// like restored targets it never takes an in-flight slot.
func (s *Scheduler) Interrupt(req PlayRequest, duration int) error {
	if (req.MediaID == "") == (req.PlaylistID == "") {
		return fmt.Errorf("interrupt: %w: set either playlist or media, not both", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return fmt.Errorf("scheduler: %w", ErrNotRunning)
	}
	pe := &pendingPlay{
		req:      req,
		priority: normalizePriority(req.Priority),
	}
	s.startInterruptLocked(pe, duration, time.Now())
	return nil
}

// SetPlayer swaps the Player the scheduler triggers, or detaches it by
// passing nil. Safe to call while running.
func (s *Scheduler) SetPlayer(p Player) {
	s.playerMu.Lock()
	defer s.playerMu.Unlock()
	s.player = p
}

// Player returns the currently attached Player (nil if none).
func (s *Scheduler) Player() Player {
	s.playerMu.RLock()
	defer s.playerMu.RUnlock()
	return s.player
}

func (s *Scheduler) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

// tick evaluates the schedule once. Callers must not hold s.mu.
func (s *Scheduler) tick(now time.Time) {
	tasks := s.enabledTasks()

	s.mu.Lock()
	defer s.mu.Unlock()

	// End interrupts whose duration has elapsed before evaluating fires, so
	// the fire loop observes a consistent interrupt state for this tick.
	s.expireInterruptLocked(now)

	// Prune entries for tasks that were removed or disabled.
	for id := range s.sched {
		if _, ok := tasks[id]; !ok {
			delete(s.sched, id)
		}
	}

	for id, t := range tasks {
		e, ok := s.sched[id]
		if !ok || !e.updatedAt.Equal(t.UpdatedAt) {
			s.sched[id] = scheduleEntry{updatedAt: t.UpdatedAt, next: s.computeNext(t, now)}
			e = s.sched[id]
		}
		if e.next.IsZero() {
			continue
		}
		if !e.next.After(now) {
			s.fire(t, now)
			s.sched[id] = scheduleEntry{updatedAt: t.UpdatedAt, next: s.computeNext(t, now)}
		}
	}
}

// firstEnabledPolicy returns a snapshot of the first enabled health policy
// in the store, or nil when none is configured. It backs the scheduler's
// default policy provider, so the policy is re-read on every Play failure.
func firstEnabledPolicy(store *Store) *HealthPolicy {
	var out *HealthPolicy
	store.View(func(d *Data) {
		for _, p := range d.HealthPolicies {
			if !p.Enabled {
				continue
			}
			cp := *p
			out = &cp
			return
		}
	})
	return out
}

// enabledTasks snapshots the enabled tasks from the store.
func (s *Scheduler) enabledTasks() map[string]*ScheduleTask {
	out := make(map[string]*ScheduleTask)
	s.store.View(func(d *Data) {
		for _, t := range d.Tasks {
			if t.Enabled {
				out[t.ID] = t
			}
		}
	})
	return out
}

// computeNext derives the next fire time after from for the task. It returns
// the zero time for tasks that cannot schedule (invalid type or cron).
func (s *Scheduler) computeNext(t *ScheduleTask, from time.Time) time.Time {
	switch t.Type {
	case TaskTypeInterval:
		if t.Interval <= 0 {
			return time.Time{}
		}
		return from.Add(time.Duration(t.Interval) * time.Second)
	case TaskTypeCron:
		c, err := ParseCron(t.Cron)
		if err != nil {
			return time.Time{}
		}
		return c.Next(from)
	}
	return time.Time{}
}

// fire records the run and triggers playback according to the task's
// interrupt flag and priority. The persisted LastRun update goes through the
// store; the actual Play call runs on a goroutine so a slow or blocking
// player never stalls the scheduler loop.
//
// fire must be called with s.mu held. A task that already has a Play in
// flight is skipped (its schedule entry still advances), so a short interval
// or slow player can never start overlapping Play calls for the same task.
// The in-flight marker is cleared by the Play goroutine when it returns, so
// disabling, modifying or Stop never leave it permanently stuck.
//
// Playback precedence — the scheduler plays exactly one target at a time:
//
//   - While an interrupt is active, every fire (regular or another
//     interrupt) is skipped with LastRun recorded: the interrupt holds
//     playback exclusively until it ends.
//   - An interrupt fire preempts the current play (its context is
//     cancelled) and plays its target. With a positive InterruptDuration the
//     pre-interrupt target is restored when the duration elapses; a one-shot
//     interrupt (duration <= 0) plays until the Player ends it and restores
//     nothing.
//   - A critical fire preempts any strictly lower-priority play; it is
//     skipped when something at critical priority is already playing.
//   - An important fire plays when idle, queues (FIFO, capped by
//     maxPlayQueue) behind a normal play, and is skipped when something at
//     important or critical priority is playing.
//   - A normal fire plays only when idle; it never preempts and never
//     queues.
//
// The Play goroutine runs with the current run's context (the one installed
// by Start): Stop, preemption and interrupt expiry cancel it so a running
// Play can observe cancellation. Preemption and restoration only take effect
// when the Player honours context cancellation; a Player that ignores it
// keeps playing alongside the new target, exactly as with Stop.
func (s *Scheduler) fire(t *ScheduleTask, now time.Time) {
	if s.inflight[t.ID] {
		return
	}

	lr := now
	_ = s.store.Update(func(d *Data) error {
		for _, task := range d.Tasks {
			if task.ID == t.ID {
				task.LastRun = &lr
				return nil
			}
		}
		return nil
	})

	if s.interruptActiveLocked() {
		return
	}

	// Stop-action task (定时关播): stop the current push and report
	// failures through the error handler. No play slot is involved.
	if t.Action == TaskActionStop {
		s.playerMu.RLock()
		p := s.player
		s.playerMu.RUnlock()
		if p == nil {
			s.reportError(fmt.Errorf("task %q: %w", t.Name, ErrNoPlayer))
			return
		}
		sp, ok := p.(StoppingPlayer)
		if !ok {
			s.reportError(fmt.Errorf("task %q: %w", t.Name, errors.New("player cannot stop")))
			return
		}
		ctx, cancel := context.WithCancel(s.ctx)
		defer cancel()
		if err := sp.Stop(ctx); err != nil {
			s.reportError(fmt.Errorf("task %q: stop: %w", t.Name, err))
		}
		return
	}

	req := PlayRequest{
		PlaylistID:      t.PlaylistID,
		MediaID:         t.MediaID,
		SceneTemplateID: t.SceneTemplateID,
		Loop:            t.Loop,
		Priority:        normalizePriority(t.Priority),
	}
	pe := &pendingPlay{id: t.ID, name: t.Name, req: req, priority: req.Priority}

	if t.Interrupt {
		s.startInterruptLocked(pe, t.InterruptDuration, now)
		return
	}

	switch {
	case s.playing == nil:
		// Idle: any priority plays immediately.
		s.startPlayLocked(pe)
	case req.Priority.rank() == PriorityCritical.rank():
		// Critical preempts strictly lower priorities only.
		if s.playing.priority.rank() >= PriorityCritical.rank() {
			return
		}
		s.playing.cancel()
		s.startPlayLocked(pe)
	case req.Priority.rank() == PriorityImportant.rank():
		// Important queues behind a normal play; it never preempts and is
		// skipped while something at important or critical priority is
		// playing.
		if s.playing.priority.rank() >= PriorityImportant.rank() {
			return
		}
		if len(s.queue) >= maxPlayQueue {
			s.reportError(fmt.Errorf("task %q: %w", t.Name, ErrQueueFull))
			return
		}
		s.queue = append(s.queue, pe)
	default:
		// Normal never preempts and never queues.
	}
}

// startPlayLocked dispatches pe to the attached Player on a goroutine and
// records it as the current playback. It must be called with s.mu held. When
// no Player is attached the dispatch is dropped and ErrNoPlayer is reported;
// when the run context is already cancelled nothing is dispatched. A
// dispatched play records a PlaySuccess event (see WithPlayEventHandler);
// plays dropped here record nothing.
func (s *Scheduler) startPlayLocked(pe *pendingPlay) {
	s.playerMu.RLock()
	p := s.player
	s.playerMu.RUnlock()
	if p == nil {
		s.reportError(fmt.Errorf("%s: %w", pe.desc(), ErrNoPlayer))
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	pe.cancel = cancel
	if pe.id != "" {
		s.inflight[pe.id] = true
	}
	s.playing = pe
	go func() {
		// Clear the in-flight marker and end the playback slot when Play
		// returns (even on error or panic) so the task can fire again and
		// the queue can drain. Registered first so it runs after the
		// recovery defer below.
		defer func() {
			s.mu.Lock()
			s.playEndedLocked(pe)
			s.mu.Unlock()
		}()
		// Recover a panicking Player so a misbehaving backend cannot take
		// the whole process (and this scheduler) down. The error is surfaced
		// through the configured handler instead of crashing the runtime
		// loop.
		defer func() {
			if r := recover(); r != nil {
				s.reportError(fmt.Errorf("%s: play panicked: %v", pe.desc(), r))
			}
		}()
		// Report the attempt before handing it to the Player: a play that
		// reaches the backend records exactly one PlaySuccess event, and a
		// later failure (Play returning an error, cancellation included) is
		// reported separately by handlePlayError.
		s.recordPlayEvent(s.playEvent(pe, PlaySuccess, ""))
		if err := p.Play(ctx, pe.req); err != nil {
			s.handlePlayError(pe, ctx, p, err)
		}
	}()
}

// handlePlayError applies the configured health policy to a failed Play. A
// failure is skipped when the policy enables auto-skip and the attached
// Player implements SkippingPlayer, and reported through the error handler
// otherwise. The skip shares the play's context, so it is abandoned if the
// scheduler stops or the play is preempted while skipping; a failed Skip is
// itself reported, wrapping the skip error. There is deliberately no
// debounce: every failed Play is decided once, and the Player is expected
// to make Skip idempotent.
//
// A failure whose context is already cancelled is never skipped: the play
// was superseded by preemption, interrupt expiry or Stop, and skipping
// could act on the playback that replaced it.
//
// Every failed Play records a PlayFailure event carrying the error before
// the health policy is consulted, so the event is independent of the skip
// decision. Cancelled plays (preemption, interrupt expiry, Stop) are
// reported too: the events reflect the real outcome of every attempt, and
// handlers that want to ignore cancellations can filter on Detail.
func (s *Scheduler) handlePlayError(pe *pendingPlay, ctx context.Context, p Player, err error) {
	// Report the failed attempt first: the event must reflect the play
	// outcome, not the remediation (skip or alarm) that follows.
	s.recordPlayEvent(s.playEvent(pe, PlayFailure, err.Error()))
	if ShouldSkipOnFailure(s.healthPolicy(), ctx.Err() != nil) {
		if sp, ok := p.(SkippingPlayer); ok {
			if skipErr := sp.Skip(ctx); skipErr != nil {
				s.reportError(fmt.Errorf("%s: play: %v; skip: %w", pe.desc(), err, skipErr))
				return
			}
			return // skipped silently: the backend has moved on
		}
	}
	s.reportError(fmt.Errorf("%s: play: %w", pe.desc(), err))
}

// startInterruptLocked begins an interrupt playback: the current play (if
// any) is cancelled and, for a timed interrupt, remembered as the resume
// target; pe plays instead. With duration > 0 the interrupt auto-ends after
// that many seconds and the resume target is replayed; with duration <= 0
// the interrupt is one-shot — it plays until the Player ends it and nothing
// is restored. Must be called with s.mu held.
func (s *Scheduler) startInterruptLocked(pe *pendingPlay, duration int, now time.Time) {
	pe.isInterrupt = true
	s.resume = nil
	if duration > 0 {
		s.resume = s.playing
		s.interruptEnd = now.Add(time.Duration(duration) * time.Second)
	} else {
		s.interruptEnd = time.Time{}
	}
	if s.playing != nil {
		s.playing.cancel()
	}
	s.startPlayLocked(pe)
}

// playEndedLocked is the cleanup of a finished Play goroutine. It clears the
// task's in-flight marker and, when the goroutine is still the current
// playback, ends that playback: an interrupt restores its resume target (if
// any) and the play queue drains into the freed slot. A goroutine whose play
// was superseded (preempted or interrupted) only clears its marker. Must be
// called with s.mu held.
func (s *Scheduler) playEndedLocked(pe *pendingPlay) {
	if pe.id != "" {
		delete(s.inflight, pe.id)
	}
	if s.playing != pe {
		return
	}
	s.playing = nil
	if pe.isInterrupt {
		s.interruptEnd = time.Time{}
		if s.resume != nil {
			r := s.resume
			s.resume = nil
			s.startPlayLocked(r)
		}
	}
	s.drainLocked()
}

// drainLocked starts the next queued play once the playback slot is free. It
// must be called with s.mu held. Queued plays that cannot be dispatched (no
// Player attached) are dropped with an error reported.
func (s *Scheduler) drainLocked() {
	for s.playing == nil && len(s.queue) > 0 {
		pe := s.queue[0]
		s.queue = s.queue[1:]
		s.startPlayLocked(pe)
	}
}

// interruptActiveLocked reports whether an interrupt playback is in progress.
// Must be called with s.mu held.
func (s *Scheduler) interruptActiveLocked() bool {
	return s.playing != nil && s.playing.isInterrupt
}

// expireInterruptLocked ends an interrupt whose duration has elapsed by
// cancelling its Play context; the interrupt goroutine performs the restore
// when it returns. Must be called with s.mu held.
func (s *Scheduler) expireInterruptLocked(now time.Time) {
	if s.playing == nil || !s.playing.isInterrupt || s.interruptEnd.IsZero() {
		return
	}
	if !now.Before(s.interruptEnd) {
		s.playing.cancel()
		s.interruptEnd = time.Time{}
	}
}

// playEvent builds the play event describing pe's attempt at the given
// result. Time is stamped when the event is built, so it reflects when the
// attempt started (success) or ended (failure). Plays without task
// attribution (for example a public Interrupt) carry an empty TaskID and
// TaskName.
func (s *Scheduler) playEvent(pe *pendingPlay, result PlayResult, detail string) PlayEvent {
	return PlayEvent{
		Time:       time.Now(),
		TaskID:     pe.id,
		TaskName:   pe.name,
		MediaID:    pe.req.MediaID,
		PlaylistID: pe.req.PlaylistID,
		Result:     result,
		Detail:     detail,
	}
}

// recordPlayEvent delivers ev to the configured event handler; without a
// handler recording is a no-op. It is only ever called from Play goroutines,
// never under s.mu, so a slow handler cannot stall the scheduler loop. The
// handler must be safe for concurrent use: preemption can make a failure
// event and the next play's success event overlap.
func (s *Scheduler) recordPlayEvent(ev PlayEvent) {
	if s.onPlayEvent != nil {
		s.onPlayEvent(ev)
	}
}

func (s *Scheduler) reportError(err error) {
	if s.onError != nil {
		s.onError(err)
	}
}
