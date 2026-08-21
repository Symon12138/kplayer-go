package management

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

func TestAlarmRaiseDedupe(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)

	a1, err := as.Raise(AlarmLevelWarning, "disk", "low space")
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if a1.Level != AlarmLevelWarning || !a1.IsActive() {
		t.Fatalf("unexpected alarm: %+v", a1)
	}

	// identical active alarm is deduplicated
	a2, err := as.Raise(AlarmLevelWarning, "disk", "low space")
	if err != nil {
		t.Fatalf("raise dup: %v", err)
	}
	if a2.ID != a1.ID {
		t.Fatalf("expected dedupe to return existing alarm, got %s vs %s", a2.ID, a1.ID)
	}

	// different message creates a new alarm
	a3, err := as.Raise(AlarmLevelWarning, "disk", "full")
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if a3.ID == a1.ID {
		t.Fatal("expected a distinct alarm for a different message")
	}

	if len(as.ListActive()) != 2 {
		t.Fatalf("expected 2 active alarms, got %d", len(as.ListActive()))
	}
	if len(as.List()) != 2 {
		t.Fatalf("expected 2 alarms total, got %d", len(as.List()))
	}
}

func TestAlarmRaiseDedupeDoesNotRewrite(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)

	a1, err := as.Raise(AlarmLevelWarning, "disk", "low space")
	if err != nil {
		t.Fatalf("raise: %v", err)
	}

	path := s.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}

	// duplicate raise must not rewrite the store file nor bump UpdatedAt
	a2, err := as.Raise(AlarmLevelWarning, "disk", "low space")
	if err != nil {
		t.Fatalf("raise dup: %v", err)
	}
	if a2.ID != a1.ID {
		t.Fatalf("expected dedupe to return existing alarm, got %s vs %s", a2.ID, a1.ID)
	}
	if !a2.UpdatedAt.Equal(a1.UpdatedAt) {
		t.Fatalf("expected UpdatedAt unchanged on dedupe, got %v vs %v", a2.UpdatedAt, a1.UpdatedAt)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected store file bytes unchanged on duplicate raise")
	}

	// a distinct raise must still persist a change
	if _, err := as.Raise(AlarmLevelWarning, "disk", "full"); err != nil {
		t.Fatalf("raise distinct: %v", err)
	}
	after2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if bytes.Equal(after, after2) {
		t.Fatal("expected a distinct raise to rewrite the store file")
	}
}

func TestAlarmDefaultsLevel(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)
	a, err := as.Raise("", "x", "y")
	if err != nil {
		t.Fatal(err)
	}
	if a.Level != AlarmLevelInfo {
		t.Fatalf("expected info level, got %s", a.Level)
	}
}

func TestAlarmRejectsEmptyTitleAndBadLevel(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)
	if _, err := as.Raise(AlarmLevelInfo, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty title, got %v", err)
	}
	if _, err := as.Raise("loud", "x", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for bad level, got %v", err)
	}
}

func TestAlarmResolveAndResolveAll(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)
	a1, _ := as.Raise(AlarmLevelInfo, "t1", "")
	a2, _ := as.Raise(AlarmLevelInfo, "t2", "")

	r, err := as.Resolve(a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != AlarmStatusResolved || r.ResolvedAt == nil {
		t.Fatalf("unexpected resolved alarm: %+v", r)
	}
	if !a2.IsActive() {
		t.Fatal("expected t2 to remain active after resolving t1")
	}

	// resolving again is a no-op
	r2, err := as.Resolve(a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r2.ResolvedAt.Unix() != r.ResolvedAt.Unix() {
		t.Fatal("resolving twice changed ResolvedAt")
	}

	if _, err := as.Resolve("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if len(as.ListActive()) != 1 {
		t.Fatalf("expected 1 active alarm, got %d", len(as.ListActive()))
	}

	n, err := as.ResolveAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected ResolveAll to resolve 1, got %d", n)
	}
	if len(as.ListActive()) != 0 {
		t.Fatalf("expected no active alarms, got %d", len(as.ListActive()))
	}
}

func TestAlarmPrune(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)
	a1, _ := as.Raise(AlarmLevelInfo, "t1", "")
	a2, _ := as.Raise(AlarmLevelInfo, "t2", "")
	if _, err := as.Resolve(a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := as.Resolve(a2.ID); err != nil {
		t.Fatal(err)
	}

	// age a1's ResolvedAt so only it is eligible for pruning
	old := time.Now().Add(-2 * time.Hour)
	if err := s.Update(func(d *Data) error {
		for _, a := range d.Alarms {
			if a.ID == a1.ID {
				a.ResolvedAt = &old
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	n, err := as.Prune(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pruned, got %d", n)
	}
	if _, err := as.Get(a1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a1 pruned, got %v", err)
	}
	if _, err := as.Get(a2.ID); err != nil {
		t.Fatalf("expected a2 kept, got %v", err)
	}
}

func TestAlarmDelete(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)
	a, _ := as.Raise(AlarmLevelInfo, "t", "")
	if err := as.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if len(as.List()) != 0 {
		t.Fatal("expected empty alarm list")
	}
	if err := as.Delete(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestAlarmResolveAlreadyResolvedIsNoop verifies that resolving an already
// resolved alarm neither rewrites the store file nor bumps root UpdatedAt.
func TestAlarmResolveAlreadyResolvedIsNoop(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)
	a, _ := as.Raise(AlarmLevelInfo, "t", "")
	if _, err := as.Resolve(a.ID); err != nil {
		t.Fatal(err)
	}

	path := s.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := snap.UpdatedAt

	// Let time pass so a spurious rewrite would be observable both in the
	// file bytes and in root UpdatedAt.
	time.Sleep(10 * time.Millisecond)

	r, err := as.Resolve(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != AlarmStatusResolved {
		t.Fatalf("expected resolved status, got %s", r.Status)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected resolving an already-resolved alarm not to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UpdatedAt.Equal(rootBefore) {
		t.Fatalf("expected root UpdatedAt unchanged, got %v vs %v", snap.UpdatedAt, rootBefore)
	}
}

// TestAlarmResolveAllNoActiveIsNoop verifies that ResolveAll with no active
// alarms neither rewrites the store file nor bumps root UpdatedAt, and still
// returns zero affected.
func TestAlarmResolveAllNoActiveIsNoop(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)

	path := s.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := snap.UpdatedAt

	time.Sleep(10 * time.Millisecond)

	n, err := as.ResolveAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected ResolveAll with no active alarms to affect 0, got %d", n)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected ResolveAll with no active alarms not to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UpdatedAt.Equal(rootBefore) {
		t.Fatalf("expected root UpdatedAt unchanged, got %v vs %v", snap.UpdatedAt, rootBefore)
	}
}

// TestAlarmPruneNothingToDeleteIsNoop verifies that Prune with nothing
// eligible neither rewrites the store file nor bumps root UpdatedAt, and
// still returns zero removed while keeping every alarm.
func TestAlarmPruneNothingToDeleteIsNoop(t *testing.T) {
	s := newTestStore(t)
	as := NewAlarmService(s)
	if _, err := as.Raise(AlarmLevelInfo, "active", ""); err != nil {
		t.Fatal(err)
	}
	recent, _ := as.Raise(AlarmLevelInfo, "recent", "")
	if _, err := as.Resolve(recent.ID); err != nil {
		t.Fatal(err)
	}

	path := s.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := snap.UpdatedAt

	time.Sleep(10 * time.Millisecond)

	n, err := as.Prune(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected Prune with nothing to delete to remove 0, got %d", n)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected Prune with nothing to delete not to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UpdatedAt.Equal(rootBefore) {
		t.Fatalf("expected root UpdatedAt unchanged, got %v vs %v", snap.UpdatedAt, rootBefore)
	}
	if len(as.List()) != 2 {
		t.Fatalf("expected both alarms to be kept, got %d", len(as.List()))
	}
}
