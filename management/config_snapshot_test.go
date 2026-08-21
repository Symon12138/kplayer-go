package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// TestConfigSnapshotCreateListGetHash covers the basic lifecycle: Create
// stamps the id, timestamp and hash, an unchanged document produces the
// identical DataHash, List is newest first and Get returns the recorded
// fields.
func TestConfigSnapshotCreateListGetHash(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewConfigSnapshotService(s)
	mustAddMedia(t, ms, "/v/a.mp4")

	before := time.Now()
	snap1, err := cs.Create("alice", "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if snap1.ID == "" {
		t.Fatal("expected generated id")
	}
	if snap1.CreatedAt.IsZero() || snap1.CreatedAt.Before(before) || snap1.CreatedAt.After(time.Now()) {
		t.Fatalf("unexpected created time: %v", snap1.CreatedAt)
	}
	if snap1.Operator != "alice" || snap1.Description != "baseline" {
		t.Fatalf("operator/description not stored: %+v", snap1)
	}
	if snap1.DataHash == "" {
		t.Fatal("expected a non-empty data hash")
	}

	// an unchanged document produces the identical hash
	snap2 := mustSnapshot(t, cs, "alice", "again")
	if snap2.DataHash != snap1.DataHash {
		t.Fatalf("expected identical content to hash identically: %q vs %q", snap1.DataHash, snap2.DataHash)
	}
	if snap2.ID == snap1.ID {
		t.Fatal("expected distinct ids")
	}

	// List is newest first, but the two snapshots may share the same
	// millisecond (clock granularity), in which case snapshotNewer breaks
	// the tie by id ascending. With generated ids either order is valid,
	// so only the membership and the timestamp order are asserted here;
	// the tie-break itself is covered as a pure function by
	// TestRollSnapshotsEvictsOldest.
	got := cs.List()
	if len(got) != 2 {
		t.Fatalf("expected 2 snapshots, got %d: %v", len(got), snapshotIDs(got))
	}
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids[snap1.ID] || !ids[snap2.ID] {
		t.Fatalf("expected snapshots %q and %q in the list, got %v", snap1.ID, snap2.ID, snapshotIDs(got))
	}
	if got[0].CreatedAt.Before(got[1].CreatedAt) {
		t.Fatalf("expected the newest snapshot first, got %v at %v then %v at %v",
			got[0].ID, got[0].CreatedAt, got[1].ID, got[1].CreatedAt)
	}

	// Get returns the recorded fields
	one, err := cs.Get(snap1.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if one.ID != snap1.ID || !one.CreatedAt.Equal(snap1.CreatedAt) ||
		one.Operator != "alice" || one.Description != "baseline" ||
		one.DataHash != snap1.DataHash {
		t.Fatalf("unexpected snapshot: %+v", one)
	}
	if _, err := cs.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing snapshot, got %v", err)
	}
}

// TestConfigSnapshotRestoreRollsBackBusinessState verifies the whole-point
// rollback: media added after the snapshot is gone after Restore, the
// snapshot collection itself survives the rollback, and a second restore
// after further changes still lands on the snapshot state.
func TestConfigSnapshotRestoreRollsBackBusinessState(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewConfigSnapshotService(s)

	m1 := mustAddMedia(t, ms, "/v/a.mp4")
	snap := mustSnapshot(t, cs, "alice", "baseline")
	mustAddMedia(t, ms, "/v/b.mp4")
	mustAddMedia(t, ms, "/v/c.mp4")

	before, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Media) != 3 {
		t.Fatalf("expected 3 media before restore, got %d", len(before.Media))
	}

	if err := cs.Restore(snap.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// the business state is back to the snapshot's: only m1, unchanged
	after, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Media) != 1 {
		t.Fatalf("expected 1 media after restore, got %d", len(after.Media))
	}
	if after.Media[0].ID != m1.ID || after.Media[0].Path != m1.Path {
		t.Fatalf("expected media rolled back to the snapshot state (id %q path %q), got id %q path %q",
			m1.ID, m1.Path, after.Media[0].ID, after.Media[0].Path)
	}

	// the rollback never touches the snapshot collection itself
	if got := cs.List(); len(got) != 1 || got[0].ID != snap.ID {
		t.Fatalf("expected the snapshot to survive the rollback, got %v", snapshotIDs(got))
	}
	if got, err := cs.Get(snap.ID); err != nil || got.DataHash != snap.DataHash {
		t.Fatalf("snapshot damaged by restore: %+v, %v", got, err)
	}

	// restoring again after further changes still lands on the snapshot
	mustAddMedia(t, ms, "/v/d.mp4")
	if err := cs.Restore(snap.ID); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	again, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Media) != 1 || again.Media[0].ID != m1.ID {
		t.Fatalf("expected the second restore to land on the snapshot state, got %+v", again.Media)
	}
}

// TestConfigSnapshotDataExcludesMetaCollections verifies the payload design:
// a snapshot's Data holds only business collections — no nested
// ConfigSnapshots or ConfigTemplates, neither in memory nor in its
// serialized JSON — while the document still keeps the snapshots
// themselves.
func TestConfigSnapshotDataExcludesMetaCollections(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewConfigSnapshotService(s)
	m1 := mustAddMedia(t, ms, "/v/a.mp4")

	s1 := mustSnapshot(t, cs, "alice", "one")
	s2 := mustSnapshot(t, cs, "alice", "two")

	for i, snap := range []*ConfigSnapshot{s1, s2} {
		if len(snap.Data.ConfigSnapshots) != 0 {
			t.Fatalf("snapshot %d embeds snapshots: %+v", i, snap.Data.ConfigSnapshots)
		}
		if len(snap.Data.ConfigTemplates) != 0 {
			t.Fatalf("snapshot %d embeds templates: %+v", i, snap.Data.ConfigTemplates)
		}
		raw, err := json.Marshal(snap.Data)
		if err != nil {
			t.Fatal(err)
		}
		// The collection fields have no omitempty, so an excluded
		// collection serializes as null — never as a nested array.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"configSnapshots", "configTemplates"} {
			if v, ok := fields[key]; ok && string(v) != "null" {
				t.Fatalf("snapshot %d Data embeds %s: %s", i, key, v)
			}
		}
	}

	// the business content is still captured
	if len(s1.Data.Media) != 1 || s1.Data.Media[0].ID != m1.ID || s1.Data.Media[0].Path != m1.Path {
		t.Fatalf("snapshot lost business content: %+v", s1.Data.Media)
	}

	doc, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.ConfigSnapshots) != 2 {
		t.Fatalf("expected 2 snapshots in the document, got %d", len(doc.ConfigSnapshots))
	}
}

// TestConfigSnapshotDeleteAndCap covers deletion and the rolling cap: a
// failed delete neither removes anything nor rewrites the store file, and
// creating past WithMaxSnapshots evicts the oldest snapshots while a
// non-positive cap keeps the history unbounded.
func TestConfigSnapshotDeleteAndCap(t *testing.T) {
	s := newTestStore(t)
	cs := NewConfigSnapshotService(s)

	s1 := mustSnapshot(t, cs, "alice", "one")
	s2 := mustSnapshot(t, cs, "bob", "two")
	if err := cs.Delete(s1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Get(s1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for deleted snapshot, got %v", err)
	}
	if got := cs.List(); !sameStrings(snapshotIDs(got), []string{s2.ID}) {
		t.Fatalf("unexpected list after delete: %v", snapshotIDs(got))
	}

	// a failed delete leaves the store file untouched
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
	after, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed delete rewrote the store file")
	}

	// past the cap the oldest snapshots are evicted
	capped := NewConfigSnapshotService(newTestStore(t), WithMaxSnapshots(3))
	created := make([]*ConfigSnapshot, 0, 4)
	for i := 0; i < 4; i++ {
		created = append(created, mustSnapshot(t, capped, "alice", fmt.Sprintf("v%d", i)))
	}
	list := capped.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 snapshots under a cap of 3, got %d", len(list))
	}
	sorted := make([]*ConfigSnapshot, len(created))
	copy(sorted, created)
	sort.Slice(sorted, func(i, j int) bool { return snapshotNewer(sorted[i], sorted[j]) })
	if list[0].ID != sorted[0].ID || list[1].ID != sorted[1].ID || list[2].ID != sorted[2].ID {
		t.Fatalf("expected the newest 3 kept in order, got %v", snapshotIDs(list))
	}
	for _, c := range created {
		if c.ID == sorted[3].ID {
			continue
		}
		if _, err := capped.Get(c.ID); err != nil {
			t.Fatalf("snapshot %q evicted unexpectedly: %v", c.ID, err)
		}
	}
	if _, err := capped.Get(sorted[3].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected the oldest snapshot evicted, got %v", err)
	}

	// a non-positive cap keeps the history unbounded
	unbounded := NewConfigSnapshotService(newTestStore(t), WithMaxSnapshots(0))
	for i := 0; i < 3; i++ {
		mustSnapshot(t, unbounded, "alice", "")
	}
	if got := unbounded.List(); len(got) != 3 {
		t.Fatalf("expected unlimited snapshots to keep all 3, got %d", len(got))
	}
}

// TestConfigSnapshotPersistence verifies that snapshots survive a
// close/reopen cycle: list, get and restore all keep working, the fields
// round-trip unchanged and the collection is stored under the
// configSnapshots key of the document.
func TestConfigSnapshotPersistence(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewConfigSnapshotService(s)

	mBase := mustAddMedia(t, ms, "/v/a.mp4")
	snap := mustSnapshot(t, cs, "alice", "baseline")
	mustAddMedia(t, ms, "/v/b.mp4")

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	rcs := NewConfigSnapshotService(reopened)

	got := rcs.List()
	if len(got) != 1 || got[0].ID != snap.ID {
		t.Fatalf("expected the snapshot after reopen, got %v", snapshotIDs(got))
	}
	if !got[0].CreatedAt.Equal(snap.CreatedAt) || got[0].Operator != "alice" ||
		got[0].Description != "baseline" || got[0].DataHash != snap.DataHash {
		t.Fatalf("snapshot fields changed after reopen: %+v", got[0])
	}

	// the payload round-trips too: restore still rolls the business state back
	if err := rcs.Restore(snap.ID); err != nil {
		t.Fatalf("restore after reopen: %v", err)
	}
	after, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Media) != 1 {
		t.Fatalf("expected 1 media after reopen+restore, got %d", len(after.Media))
	}
	if after.Media[0].ID != mBase.ID || after.Media[0].Path != mBase.Path {
		t.Fatalf("expected restore after reopen to roll media back to (id %q path %q), got id %q path %q",
			mBase.ID, mBase.Path, after.Media[0].ID, after.Media[0].Path)
	}
	if len(after.ConfigSnapshots) != 1 {
		t.Fatalf("expected the snapshot to survive the reopen+restore, got %d", len(after.ConfigSnapshots))
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"configSnapshots"`)) {
		t.Fatal("expected the snapshots to be stored under the configSnapshots key")
	}
}

// TestConfigSnapshotRestoreMissingIsNoop verifies that restoring a missing
// snapshot returns ErrNotFound and leaves both the document and the store
// file untouched.
func TestConfigSnapshotRestoreMissingIsNoop(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	cs := NewConfigSnapshotService(s)
	mustAddMedia(t, ms, "/v/a.mp4")
	mustSnapshot(t, cs, "alice", "baseline")

	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}

	if err := cs.Restore("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing restore, got %v", err)
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Media) != 1 || len(snap.ConfigSnapshots) != 1 {
		t.Fatalf("failed restore changed the document: %+v", snap)
	}
	after, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed restore rewrote the store file")
	}
}

// TestRollSnapshotsEvictsOldest covers the pure ordering and eviction
// logic: newest-first ordering with the id breaking timestamp ties, and
// rolling eviction that keeps only the newest max snapshots (or everything
// for a non-positive cap) while preserving the order of the survivors.
func TestRollSnapshotsEvictsOldest(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, at time.Time) *ConfigSnapshot {
		return &ConfigSnapshot{ID: id, CreatedAt: at}
	}

	// snapshotNewer: newest first, id tie-break ascending
	if !snapshotNewer(mk("b", base.Add(time.Minute)), mk("a", base)) {
		t.Fatal("expected the newer snapshot to sort first")
	}
	if !snapshotNewer(mk("a1", base), mk("b2", base)) {
		t.Fatal("expected the smaller id to break the timestamp tie")
	}

	// eviction keeps only the newest max, preserving the order of survivors
	snaps := []*ConfigSnapshot{
		mk("s1", base),
		mk("s2", base.Add(time.Minute)),
		mk("s3", base.Add(2*time.Minute)),
		mk("s4", base.Add(3*time.Minute)),
		mk("s5", base.Add(4*time.Minute)),
	}
	got := rollSnapshots(snaps, 2)
	if len(got) != 2 || got[0].ID != "s4" || got[1].ID != "s5" {
		t.Fatalf("expected the two newest kept in order, got %v", snapshotIDs(got))
	}

	// within the cap nothing is dropped
	if got := rollSnapshots(snaps, 5); len(got) != 5 {
		t.Fatalf("expected nothing dropped within the cap, got %d", len(got))
	}

	// non-positive caps keep the history unbounded
	if got := rollSnapshots(snaps, 0); len(got) != 5 {
		t.Fatalf("expected zero cap to keep everything, got %d", len(got))
	}
	if got := rollSnapshots(snaps, -1); len(got) != 5 {
		t.Fatalf("expected negative cap to keep everything, got %d", len(got))
	}

	// a timestamp tie is broken by id: the largest id is the oldest
	tied := []*ConfigSnapshot{mk("t3", base), mk("t1", base), mk("t2", base)}
	got = rollSnapshots(tied, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 kept from tied snapshots, got %d", len(got))
	}
	kept := map[string]bool{got[0].ID: true, got[1].ID: true}
	if kept["t3"] || !kept["t1"] || !kept["t2"] {
		t.Fatalf("expected the largest id evicted from a tie, got %v", snapshotIDs(got))
	}
}

// mustSnapshot creates a snapshot and fails the test on error.
func mustSnapshot(t *testing.T, cs *ConfigSnapshotService, operator, description string) *ConfigSnapshot {
	t.Helper()
	snap, err := cs.Create(operator, description)
	if err != nil {
		t.Fatalf("create snapshot (%q, %q): %v", operator, description, err)
	}
	return snap
}

// snapshotIDs returns the ids of the snapshots in order.
func snapshotIDs(snaps []*ConfigSnapshot) []string {
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.ID)
	}
	return out
}
