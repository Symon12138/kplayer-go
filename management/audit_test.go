package management

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

func TestAuditRecordBasics(t *testing.T) {
	s := newTestStore(t)
	as := NewAuditService(s)

	// empty or blank action is rejected
	if _, err := as.Record(AuditEntry{Action: ""}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty action, got %v", err)
	}
	if _, err := as.Record(AuditEntry{Action: "   "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for blank action, got %v", err)
	}

	// unknown result is rejected
	if _, err := as.Record(AuditEntry{Action: "media.delete", Result: "maybe"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown result, got %v", err)
	}

	// Record assigns an ID and a timestamp and keeps every field; an empty
	// result defaults to success
	before := time.Now()
	e, err := as.Record(AuditEntry{
		Operator: "console",
		Action:   "media.delete",
		Target:   "m1",
		Detail:   "removed stale file",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if e.ID == "" {
		t.Fatal("expected generated id")
	}
	if e.Time.IsZero() {
		t.Fatal("expected zero time to be replaced with the current time")
	}
	if e.Time.Before(before) || e.Time.After(time.Now()) {
		t.Fatalf("unexpected recorded time: %v", e.Time)
	}
	if e.Result != AuditSuccess {
		t.Fatalf("expected default success result, got %q", e.Result)
	}
	if e.Operator != "console" || e.Action != "media.delete" ||
		e.Target != "m1" || e.Detail != "removed stale file" {
		t.Fatalf("unexpected entry: %+v", e)
	}

	// an explicit time and a failure result are preserved
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f, err := as.Record(AuditEntry{
		Time:   when,
		Action: "task.replace",
		Target: "t9",
		Result: AuditFailure,
	})
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if !f.Time.Equal(when) || f.Result != AuditFailure {
		t.Fatalf("explicit time/result not preserved: %+v", f)
	}

	// reopening the store yields the same fields
	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	list := NewAuditService(reopened).List()
	if len(list) != 2 {
		t.Fatalf("expected 2 entries after reopen, got %d", len(list))
	}
	var got *AuditEntry
	for _, x := range list {
		if x.ID == e.ID {
			got = x
		}
	}
	if got == nil {
		t.Fatal("recorded entry lost after reopen")
	}
	if !got.Time.Equal(e.Time) || got.Operator != e.Operator || got.Action != e.Action ||
		got.Target != e.Target || got.Result != e.Result || got.Detail != e.Detail {
		t.Fatalf("entry fields changed after reopen: %+v", got)
	}
}

func TestAuditListOrder(t *testing.T) {
	s := newTestStore(t)
	as := NewAuditService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	eOld := mustRecordAudit(t, as, AuditEntry{Time: base, Action: "a"})
	eMid := mustRecordAudit(t, as, AuditEntry{Time: base.Add(time.Minute), Action: "b"})
	eNew := mustRecordAudit(t, as, AuditEntry{Time: base.Add(2 * time.Minute), Action: "c"})
	// two more entries sharing eMid's timestamp exercise the id tie-break
	eTie1 := mustRecordAudit(t, as, AuditEntry{Time: base.Add(time.Minute), Action: "d"})
	eTie2 := mustRecordAudit(t, as, AuditEntry{Time: base.Add(time.Minute), Action: "e"})

	list := as.List()
	if len(list) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(list))
	}
	if list[0].ID != eNew.ID || list[4].ID != eOld.ID {
		t.Fatalf("unexpected order: %v", auditIDs(list))
	}
	// the three entries sharing a timestamp are ordered by id, ascending
	for i := 1; i < 3; i++ {
		if list[i].ID >= list[i+1].ID {
			t.Fatalf("expected id-ascending order among tied timestamps, got %v", auditIDs(list))
		}
	}
	tied := map[string]bool{eMid.ID: true, eTie1.ID: true, eTie2.ID: true}
	for i := 1; i <= 3; i++ {
		if !tied[list[i].ID] {
			t.Fatalf("unexpected entry in tied group: %v", auditIDs(list))
		}
	}
}

func TestAuditListFiltered(t *testing.T) {
	s := newTestStore(t)
	as := NewAuditService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// newest first: e4, e3, e2, e1
	e1 := mustRecordAudit(t, as, AuditEntry{Time: base, Operator: "console", Action: "media.delete", Result: AuditSuccess})
	e2 := mustRecordAudit(t, as, AuditEntry{Time: base.Add(time.Minute), Operator: "console", Action: "task.replace", Result: AuditSuccess})
	e3 := mustRecordAudit(t, as, AuditEntry{Time: base.Add(2 * time.Minute), Operator: "alice", Action: "media.delete", Result: AuditFailure})
	e4 := mustRecordAudit(t, as, AuditEntry{Time: base.Add(3 * time.Minute), Operator: "alice", Action: "media.delete", Result: AuditSuccess})

	// an empty filter matches everything, newest first
	if got := as.ListFiltered(AuditFilter{}); !sameStrings(auditIDs(got), []string{e4.ID, e3.ID, e2.ID, e1.ID}) {
		t.Fatalf("unexpected unfiltered list: %v", auditIDs(got))
	}

	// single filter dimensions
	if got := as.ListFiltered(AuditFilter{Operator: "console"}); !sameStrings(auditIDs(got), []string{e2.ID, e1.ID}) {
		t.Fatalf("unexpected operator filter: %v", auditIDs(got))
	}
	if got := as.ListFiltered(AuditFilter{Action: "media.delete"}); !sameStrings(auditIDs(got), []string{e4.ID, e3.ID, e1.ID}) {
		t.Fatalf("unexpected action filter: %v", auditIDs(got))
	}
	if got := as.ListFiltered(AuditFilter{Result: AuditFailure}); !sameStrings(auditIDs(got), []string{e3.ID}) {
		t.Fatalf("unexpected result filter: %v", auditIDs(got))
	}

	// combined dimensions
	if got := as.ListFiltered(AuditFilter{Operator: "alice", Action: "media.delete"}); !sameStrings(auditIDs(got), []string{e4.ID, e3.ID}) {
		t.Fatalf("unexpected operator+action filter: %v", auditIDs(got))
	}
	if got := as.ListFiltered(AuditFilter{Operator: "console", Action: "media.delete", Result: AuditSuccess}); !sameStrings(auditIDs(got), []string{e1.ID}) {
		t.Fatalf("unexpected full filter: %v", auditIDs(got))
	}

	// no match yields an empty list
	if got := as.ListFiltered(AuditFilter{Operator: "nobody"}); len(got) != 0 {
		t.Fatalf("expected empty result, got %v", auditIDs(got))
	}
}

func TestAuditPrune(t *testing.T) {
	s := newTestStore(t)
	as := NewAuditService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		e := mustRecordAudit(t, as, AuditEntry{
			Time:     base.Add(time.Duration(i) * time.Minute),
			Operator: "console",
			Action:   "media.delete",
			Target:   "m",
		})
		ids[i] = e.ID
	}

	// keeping the newest 3 removes the two oldest
	n, err := as.Prune(3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 pruned, got %d", n)
	}
	if got := as.List(); !sameStrings(auditIDs(got), []string{ids[4], ids[3], ids[2]}) {
		t.Fatalf("unexpected list after prune: %v", auditIDs(got))
	}

	// maxEntries <= 0 clears the whole log
	n, err = as.Prune(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("expected 3 cleared, got %d", n)
	}
	if len(as.List()) != 0 {
		t.Fatal("expected empty log after clearing")
	}
	if _, err := as.Record(AuditEntry{Action: "x"}); err != nil {
		t.Fatal(err)
	}
	n, err = as.Prune(-1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected negative maxEntries to clear 1, got %d", n)
	}
	if len(as.List()) != 0 {
		t.Fatal("expected empty log after negative maxEntries")
	}

	// clearing an already empty log removes nothing
	n, err = as.Prune(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 removed from empty log, got %d", n)
	}
}

// TestAuditPruneNothingToDeleteIsNoop verifies that Prune within the
// retention limit neither rewrites the store file nor bumps root UpdatedAt,
// and still returns zero removed while keeping every entry.
func TestAuditPruneNothingToDeleteIsNoop(t *testing.T) {
	s := newTestStore(t)
	as := NewAuditService(s)
	for i := 0; i < 3; i++ {
		if _, err := as.Record(AuditEntry{Action: "play.start"}); err != nil {
			t.Fatal(err)
		}
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

	n, err := as.Prune(3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected Prune within limit to remove 0, got %d", n)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected Prune within limit not to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UpdatedAt.Equal(rootBefore) {
		t.Fatalf("expected root UpdatedAt unchanged, got %v vs %v", snap.UpdatedAt, rootBefore)
	}
	if len(as.List()) != 3 {
		t.Fatalf("expected all entries kept, got %d", len(as.List()))
	}
}

// TestAuditPersistence verifies that recorded entries survive a
// close/reopen cycle and are stored under the auditLogs key of the
// document.
func TestAuditPersistence(t *testing.T) {
	s := newTestStore(t)
	as := NewAuditService(s)

	want := make([]*AuditEntry, 0, 3)
	for _, action := range []string{"media.delete", "task.replace", "play.start"} {
		e, err := as.Record(AuditEntry{
			Operator: "console",
			Action:   action,
			Target:   "t",
			Result:   AuditFailure,
			Detail:   "d",
		})
		if err != nil {
			t.Fatalf("record %q: %v", action, err)
		}
		want = append(want, e)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got := NewAuditService(reopened).List()
	if len(got) != len(want) {
		t.Fatalf("expected %d entries after reopen, got %d", len(want), len(got))
	}
	byID := make(map[string]*AuditEntry, len(got))
	for _, e := range got {
		byID[e.ID] = e
	}
	for _, w := range want {
		e, ok := byID[w.ID]
		if !ok {
			t.Fatalf("entry %q lost after reopen", w.ID)
		}
		if !e.Time.Equal(w.Time) || e.Operator != w.Operator || e.Action != w.Action ||
			e.Target != w.Target || e.Result != w.Result || e.Detail != w.Detail {
			t.Fatalf("entry %q changed after reopen: %+v", w.ID, e)
		}
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"auditLogs"`)) {
		t.Fatal("expected the audit log to be stored under the auditLogs key")
	}
}

// mustRecordAudit records an entry and fails the test on error.
func mustRecordAudit(t *testing.T, as *AuditService, entry AuditEntry) *AuditEntry {
	t.Helper()
	e, err := as.Record(entry)
	if err != nil {
		t.Fatalf("record %+v: %v", entry, err)
	}
	return e
}

// auditIDs returns the ids of the entries in order.
func auditIDs(entries []*AuditEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}
