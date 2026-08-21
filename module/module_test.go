package module

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kpproto "github.com/bytelang/kplayer/types/core/proto"
)

const (
	testAction = kpproto.EventMessageAction_EVENT_MESSAGE_ACTION_PLAYER_STOP
)

// testKeeperContext returns a keeper context with an always-accepting
// validator.
func testKeeperContext(id string) KeeperContext {
	return NewKeeperContext(id, testAction, func(msg string) bool { return true })
}

func TestKeeperContextWaitContextCanceled(t *testing.T) {
	kc := NewKeeperContext("cancel", testAction, nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- kc.WaitContext(ctx)
	}()

	// give the goroutine a chance to block inside WaitContext; the outcome is
	// the same even if cancel() runs first (ctx already canceled).
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitContext returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitContext did not return after context cancellation")
	}
}

func TestKeeperContextWaitContextPreCanceled(t *testing.T) {
	kc := NewKeeperContext("pre-cancel", testAction, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := kc.WaitContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitContext returned %v, want context.Canceled", err)
	}
}

func TestKeeperContextWaitContextMessage(t *testing.T) {
	kc := NewKeeperContext("message", testAction, nil)
	go func() {
		kc.ch <- "hello"
	}()

	if err := kc.WaitContext(context.Background()); err != nil {
		t.Fatalf("WaitContext returned %v, want nil on message arrival", err)
	}
}

func TestKeeperContextWaitContextClosed(t *testing.T) {
	kc := NewKeeperContext("closed", testAction, nil)
	kc.Close()

	if err := kc.WaitContext(context.Background()); err != nil {
		t.Fatalf("WaitContext returned %v, want nil on closed channel", err)
	}
}

// TestKeeperContextWait verifies the legacy Wait() keeps its contract: it
// returns when a message arrives.
func TestKeeperContextWait(t *testing.T) {
	kc := NewKeeperContext("wait", testAction, nil)
	go func() {
		kc.ch <- "hello"
	}()

	done := make(chan struct{})
	go func() {
		kc.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after message arrival")
	}
}

func TestKeeperContextCloseIdempotent(t *testing.T) {
	kc := NewKeeperContext("close", testAction, nil)
	kc.Close()
	kc.Close() // repeated Close must not panic

	// Close via a copy (mirrors the provider holding its own value while the
	// keeper slice holds another copy sharing the same channel).
	kcCopy := kc
	kcCopy.Close()
	kc.Close()

	// the channel must be closed exactly once: a receive yields zero value
	// with ok == false.
	if v, ok := <-kc.ch; ok || v != "" {
		t.Fatalf("expected closed channel, got %q ok=%v", v, ok)
	}
}

func TestKeeperContextCloseConcurrent(t *testing.T) {
	kc := NewKeeperContext("close-concurrent", testAction, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kc.Close()
		}()
	}
	wg.Wait()

	if v, ok := <-kc.ch; ok || v != "" {
		t.Fatalf("expected closed channel, got %q ok=%v", v, ok)
	}
}

// TestModuleKeeperGetKeeperContext is a regression test for the range
// variable address bug: the returned pointer must reference the element
// stored in the keeper slice, so mutations through it are visible.
func TestModuleKeeperGetKeeperContext(t *testing.T) {
	m := &ModuleKeeper{}
	kc := testKeeperContext("id-1")
	if err := m.RegisterKeeperChannel(kc); err != nil {
		t.Fatalf("RegisterKeeperChannel failed: %v", err)
	}

	ptr := m.GetKeeperContext("id-1")
	if ptr == nil {
		t.Fatal("GetKeeperContext returned nil for registered id")
	}
	if ptr.id != "id-1" {
		t.Fatalf("GetKeeperContext returned id %q, want id-1", ptr.id)
	}

	ptr.dirty = true
	if !m.keeper[0].dirty {
		t.Fatal("mutation through GetKeeperContext result not visible in keeper (range variable address bug)")
	}

	if m.GetKeeperContext("unknown") != nil {
		t.Fatal("GetKeeperContext returned non-nil for unknown id")
	}
}

func TestModuleKeeperRegisterDuplicate(t *testing.T) {
	m := &ModuleKeeper{}
	kc := testKeeperContext("dup")
	if err := m.RegisterKeeperChannel(kc); err != nil {
		t.Fatalf("first RegisterKeeperChannel failed: %v", err)
	}
	if err := m.RegisterKeeperChannel(kc); err == nil {
		t.Fatal("RegisterKeeperChannel should reject duplicate id")
	}
}

func TestModuleKeeperTriggerDeliversAndWashes(t *testing.T) {
	m := &ModuleKeeper{}
	var got string
	kc := NewKeeperContext("t1", testAction, func(msg string) bool {
		got = msg
		return true
	})
	if err := m.RegisterKeeperChannel(kc); err != nil {
		t.Fatalf("RegisterKeeperChannel failed: %v", err)
	}

	// Join the trigger goroutine before asserting on shared state. The
	// WaitContext receive only synchronizes with the channel send in
	// trySend; Trigger writes keeper[i].dirty after trySend returns, so the
	// receive alone does not cover that write. close(done) provides the
	// happens-before edge for every write made inside Trigger.
	done := make(chan struct{})
	go func() {
		m.Trigger(&kpproto.KPMessage{Action: testAction, Body: "payload"})
		close(done)
	}()
	if err := kc.WaitContext(context.Background()); err != nil {
		t.Fatalf("WaitContext returned %v, want nil", err)
	}
	<-done
	if got != "payload" {
		t.Fatalf("validator received %q, want payload", got)
	}
	if !m.keeper[0].dirty {
		t.Fatal("delivered item should be marked dirty")
	}

	// the next Trigger washes the dirty item away; use a non-matching action
	// so no further send happens.
	m.Trigger(&kpproto.KPMessage{Action: kpproto.EventMessageAction_EVENT_MESSAGE_ACTION_PLAYER_PAUSE, Body: "x"})
	if len(m.keeper) != 0 {
		t.Fatalf("dirty item should have been washed, keeper size = %d", len(m.keeper))
	}
}

// TestModuleKeeperTriggerAfterClose verifies a Trigger matching a closed
// keeper context neither panics (send on closed channel) nor delivers.
func TestModuleKeeperTriggerAfterClose(t *testing.T) {
	m := &ModuleKeeper{}
	kc := testKeeperContext("ta")
	if err := m.RegisterKeeperChannel(kc); err != nil {
		t.Fatalf("RegisterKeeperChannel failed: %v", err)
	}

	kc.Close()
	m.Trigger(&kpproto.KPMessage{Action: testAction, Body: "x"})

	if len(m.keeper) != 0 {
		t.Fatalf("closed item should have been washed, keeper size = %d", len(m.keeper))
	}
}

// TestModuleKeeperTriggerConcurrentClose exercises Trigger and Close from
// many goroutines at once: it must not panic and must not deadlock.
func TestModuleKeeperTriggerConcurrentClose(t *testing.T) {
	m := &ModuleKeeper{}
	kc := testKeeperContext("cc")
	if err := m.RegisterKeeperChannel(kc); err != nil {
		t.Fatalf("RegisterKeeperChannel failed: %v", err)
	}

	// drain receiver, playing the role of the provider waiting on the
	// channel so in-flight sends can complete.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-kc.ch:
			}
		}
	}()

	msg := &kpproto.KPMessage{Action: testAction, Body: "x"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.Trigger(msg)
		}()
		go func() {
			defer wg.Done()
			kc.Close()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent Trigger/Close deadlocked")
	}

	// the keeper context must be closed exactly once.
	if v, ok := <-kc.ch; ok || v != "" {
		t.Fatalf("expected closed channel, got %q ok=%v", v, ok)
	}
}
