package management

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

func TestPlayEventRecordBasics(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)

	// an event with neither a media nor a playlist target is rejected
	if _, err := ps.Record(PlayEvent{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for no target, got %v", err)
	}
	if _, err := ps.Record(PlayEvent{MediaID: "   "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for blank media target, got %v", err)
	}
	if _, err := ps.Record(PlayEvent{PlaylistID: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for blank playlist target, got %v", err)
	}

	// an unknown result is rejected
	if _, err := ps.Record(PlayEvent{MediaID: "m1", Result: "maybe"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown result, got %v", err)
	}
	if len(ps.List(0)) != 0 {
		t.Fatal("expected no events recorded by rejected inputs")
	}

	// Record assigns an ID and a timestamp and keeps every field; an empty
	// result defaults to success
	before := time.Now()
	e, err := ps.Record(PlayEvent{
		TaskID:   "t1",
		TaskName: "nightly",
		MediaID:  "m1",
		Detail:   "started",
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
	if e.Result != PlaySuccess {
		t.Fatalf("expected default success result, got %q", e.Result)
	}
	if e.TaskID != "t1" || e.TaskName != "nightly" ||
		e.MediaID != "m1" || e.PlaylistID != "" || e.Detail != "started" {
		t.Fatalf("unexpected event: %+v", e)
	}

	// an explicit time, a playlist target and a failure result are preserved
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f, err := ps.Record(PlayEvent{
		Time:       when,
		TaskID:     "t2",
		PlaylistID: "p9",
		Result:     PlayFailure,
		Detail:     "media not found",
	})
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if !f.Time.Equal(when) || f.Result != PlayFailure {
		t.Fatalf("explicit time/result not preserved: %+v", f)
	}
	if f.MediaID != "" || f.PlaylistID != "p9" || f.TaskID != "t2" || f.Detail != "media not found" {
		t.Fatalf("unexpected failure event: %+v", f)
	}

	// reopening the store yields the same fields
	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	list := NewPlayEventService(reopened).List(0)
	if len(list) != 2 {
		t.Fatalf("expected 2 events after reopen, got %d", len(list))
	}
	var got *PlayEvent
	for _, x := range list {
		if x.ID == e.ID {
			got = x
		}
	}
	if got == nil {
		t.Fatal("recorded event lost after reopen")
	}
	if !got.Time.Equal(e.Time) || got.TaskID != e.TaskID || got.TaskName != e.TaskName ||
		got.MediaID != e.MediaID || got.PlaylistID != e.PlaylistID ||
		got.Result != e.Result || got.Detail != e.Detail {
		t.Fatalf("event fields changed after reopen: %+v", got)
	}
}

// TestPlayEventCountByResult verifies CountByResult counts per result, that
// both keys are always present (even with zero counts) and that unknown
// results, possible only in a hand-edited store, are ignored.
func TestPlayEventCountByResult(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)

	// an empty log still yields both keys
	counts := ps.CountByResult()
	if counts[PlaySuccess] != 0 || counts[PlayFailure] != 0 {
		t.Fatalf("expected zero counts on empty log, got %v", counts)
	}

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), MediaID: "m1"})
	}
	for i := 0; i < 2; i++ {
		mustRecordPlay(t, ps, PlayEvent{
			Time:       base.Add(time.Duration(i) * time.Minute),
			PlaylistID: "p1",
			Result:     PlayFailure,
		})
	}
	// an unknown result is hand-seeded, as Record would reject it
	if err := s.Update(func(d *Data) error {
		d.PlayEvents = append(d.PlayEvents, &PlayEvent{ID: "x1", MediaID: "m1", Result: "maybe"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	counts = ps.CountByResult()
	if counts[PlaySuccess] != 3 || counts[PlayFailure] != 2 {
		t.Fatalf("unexpected counts: %v", counts)
	}
	if len(counts) != 2 {
		t.Fatalf("expected exactly the success and failure keys, got %v", counts)
	}
}

// TestPlayEventTopMedia verifies the ranking: play count descending, media
// id ascending breaking ties, playlist-only events never counted and the n
// cap applied.
func TestPlayEventTopMedia(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seed := func(media string, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			ev := PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), MediaID: media}
			if media == "" {
				ev = PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), PlaylistID: "p1"}
			}
			mustRecordPlay(t, ps, ev)
		}
	}
	// "b" and "a" tie at 2, "c" and "d" tie at 1; playlist-only plays count
	// for nothing
	seed("b", 2)
	seed("a", 2)
	seed("", 3)
	seed("c", 1)
	seed("d", 1)

	got := ps.TopMedia(10)
	want := []MediaCount{
		{MediaID: "a", Count: 2},
		{MediaID: "b", Count: 2},
		{MediaID: "c", Count: 1},
		{MediaID: "d", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d entries, got %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	// the n cap keeps the top n only
	if got := ps.TopMedia(2); len(got) != 2 || got[0].MediaID != "a" || got[1].MediaID != "b" {
		t.Fatalf("unexpected top 2: %v", got)
	}
	// n <= 0 yields an empty list
	if got := ps.TopMedia(0); len(got) != 0 {
		t.Fatalf("expected empty list for n=0, got %v", got)
	}
	if got := ps.TopMedia(-1); len(got) != 0 {
		t.Fatalf("expected empty list for negative n, got %v", got)
	}
}

// TestPlayEventPlaysByHour verifies the per-hour distribution: the hour of
// each event's own Time is counted, all 24 hours are always present, and
// zero-Time events, possible only in a hand-edited store, are ignored.
func TestPlayEventPlaysByHour(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, h := range []int{3, 3, 9, 22} {
		mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Duration(h) * time.Hour), MediaID: "m1"})
	}
	// a zero-Time event is hand-seeded, as Record would stamp the time
	if err := s.Update(func(d *Data) error {
		d.PlayEvents = append(d.PlayEvents, &PlayEvent{ID: "z1", MediaID: "m1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got := ps.PlaysByHour()
	if len(got) != 24 {
		t.Fatalf("expected 24 hour entries, got %d", len(got))
	}
	for h := 0; h < 24; h++ {
		if got[h].Hour != h {
			t.Fatalf("expected hour %d at index %d, got %+v", h, h, got[h])
		}
	}
	want := map[int]int{3: 2, 9: 1, 22: 1}
	for h := 0; h < 24; h++ {
		if got[h].Count != want[h] {
			t.Fatalf("unexpected count for hour %d: got %d, want %d", h, got[h].Count, want[h])
		}
	}
}

func TestPlayEventListOrderAndLimit(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	eOld := mustRecordPlay(t, ps, PlayEvent{Time: base, MediaID: "m1"})
	eMid := mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Minute), MediaID: "m2"})
	eNew := mustRecordPlay(t, ps, PlayEvent{Time: base.Add(2 * time.Minute), MediaID: "m3"})
	// two more events sharing eMid's timestamp exercise the id tie-break
	eTie1 := mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Minute), MediaID: "m4"})
	eTie2 := mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Minute), MediaID: "m5"})

	list := ps.List(0)
	if len(list) != 5 {
		t.Fatalf("expected 5 events, got %d", len(list))
	}
	if list[0].ID != eNew.ID || list[4].ID != eOld.ID {
		t.Fatalf("unexpected order: %v", playIDs(list))
	}
	// the three events sharing a timestamp are ordered by id, ascending
	for i := 1; i < 3; i++ {
		if list[i].ID >= list[i+1].ID {
			t.Fatalf("expected id-ascending order among tied timestamps, got %v", playIDs(list))
		}
	}
	tied := map[string]bool{eMid.ID: true, eTie1.ID: true, eTie2.ID: true}
	for i := 1; i <= 3; i++ {
		if !tied[list[i].ID] {
			t.Fatalf("unexpected event in tied group: %v", playIDs(list))
		}
	}

	// a positive limit keeps only the newest events
	if got := ps.List(2); !sameStrings(playIDs(got), []string{eNew.ID, list[1].ID}) {
		t.Fatalf("unexpected limited list: %v", playIDs(got))
	}
	// a limit >= the log size keeps everything
	if got := ps.List(5); len(got) != 5 {
		t.Fatalf("expected all events for limit 5, got %d", len(got))
	}
	// a negative limit returns everything too
	if got := ps.List(-1); len(got) != 5 {
		t.Fatalf("expected all events for negative limit, got %d", len(got))
	}
}

func TestPlayEventPrune(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		e := mustRecordPlay(t, ps, PlayEvent{
			Time:    base.Add(time.Duration(i) * time.Minute),
			MediaID: "m1",
		})
		ids[i] = e.ID
	}

	// keeping the newest 3 removes the two oldest
	n, err := ps.Prune(3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 pruned, got %d", n)
	}
	if got := ps.List(0); !sameStrings(playIDs(got), []string{ids[4], ids[3], ids[2]}) {
		t.Fatalf("unexpected list after prune: %v", playIDs(got))
	}

	// maxEntries <= 0 clears the whole log
	n, err = ps.Prune(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("expected 3 cleared, got %d", n)
	}
	if len(ps.List(0)) != 0 {
		t.Fatal("expected empty log after clearing")
	}
	if _, err := ps.Record(PlayEvent{MediaID: "m1"}); err != nil {
		t.Fatal(err)
	}
	n, err = ps.Prune(-1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected negative maxEntries to clear 1, got %d", n)
	}
	if len(ps.List(0)) != 0 {
		t.Fatal("expected empty log after negative maxEntries")
	}

	// clearing an already empty log removes nothing
	n, err = ps.Prune(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 removed from empty log, got %d", n)
	}
}

// TestPlayEventPruneNoop verifies that Prune within the retention limit
// neither rewrites the store file nor bumps root UpdatedAt, and still
// returns zero removed while keeping every event.
func TestPlayEventPruneNoop(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)
	for i := 0; i < 3; i++ {
		if _, err := ps.Record(PlayEvent{MediaID: "m1"}); err != nil {
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

	n, err := ps.Prune(3)
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
	if len(ps.List(0)) != 3 {
		t.Fatalf("expected all events kept, got %d", len(ps.List(0)))
	}
}

// TestPlayEventPersistence verifies that recorded events survive a
// close/reopen cycle and are stored under the playEvents key of the
// document with camelCase JSON keys.
func TestPlayEventPersistence(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)

	want := make([]*PlayEvent, 0, 3)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seeds := []PlayEvent{
		{Time: base, TaskID: "t1", TaskName: "nightly", MediaID: "m1", Result: PlaySuccess, Detail: "d1"},
		{Time: base.Add(time.Minute), TaskID: "t2", PlaylistID: "p2", Result: PlayFailure, Detail: "d2"},
		{Time: base.Add(2 * time.Minute), TaskID: "t3", TaskName: "loop", MediaID: "m3", PlaylistID: "", Result: PlaySuccess},
	}
	for _, seed := range seeds {
		e, err := ps.Record(seed)
		if err != nil {
			t.Fatalf("record %+v: %v", seed, err)
		}
		want = append(want, e)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got := NewPlayEventService(reopened).List(0)
	if len(got) != len(want) {
		t.Fatalf("expected %d events after reopen, got %d", len(want), len(got))
	}
	byID := make(map[string]*PlayEvent, len(got))
	for _, e := range got {
		byID[e.ID] = e
	}
	for _, w := range want {
		e, ok := byID[w.ID]
		if !ok {
			t.Fatalf("event %q lost after reopen", w.ID)
		}
		if !e.Time.Equal(w.Time) || e.TaskID != w.TaskID || e.TaskName != w.TaskName ||
			e.MediaID != w.MediaID || e.PlaylistID != w.PlaylistID ||
			e.Result != w.Result || e.Detail != w.Detail {
			t.Fatalf("event %q changed after reopen: %+v", w.ID, e)
		}
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"playEvents"`)) {
		t.Fatal("expected events to be stored under the playEvents key")
	}
	for _, key := range []string{`"taskId"`, `"taskName"`, `"mediaId"`, `"playlistId"`, `"detail"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("expected camelCase key %s in stored document", key)
		}
	}
}

// mustRecordPlay records an event and fails the test on error.
func mustRecordPlay(t *testing.T, ps *PlayEventService, ev PlayEvent) *PlayEvent {
	t.Helper()
	e, err := ps.Record(ev)
	if err != nil {
		t.Fatalf("record %+v: %v", ev, err)
	}
	return e
}

// playIDs returns the ids of the events in order.
func playIDs(events []*PlayEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}
