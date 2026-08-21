package management

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// seedPlays records count play events for media on ps, spaced a minute
// apart from base.
func seedPlays(t *testing.T, ps *PlayEventService, base time.Time, media string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), MediaID: media})
	}
}

// TestRecommendMediaFrequency verifies the ranking: play count descending
// with media id ascending breaking ties, failures counting like successes
// (TopMedia semantics) and playlist-only plays never counted.
func TestRecommendMediaFrequency(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	// m1 plays 3 times, m2 twice plus one failure, m3 twice, m4 once;
	// playlist-only plays count for nothing
	seedPlays(t, ps, base, "m1", 3)
	seedPlays(t, ps, base, "m2", 2)
	seedPlays(t, ps, base, "m3", 2)
	seedPlays(t, ps, base, "m4", 1)
	mustRecordPlay(t, ps, PlayEvent{Time: base.Add(30 * time.Minute), MediaID: "m2", Result: PlayFailure})
	for i := 0; i < 2; i++ {
		mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), PlaylistID: "p1"})
	}

	got, err := RecommendMedia(s, nil, 10)
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	// m1 and m2 tie at 3 (the failure counts like a success); ties break by
	// media id ascending; the playlist-only plays never count
	wantList(t, got, "m1", "m2", "m3", "m4")

	// identical inputs yield identical results
	again, err := RecommendMedia(s, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("same inputs produced different results: %v vs %v", got, again)
	}
}

// TestRecommendMediaRecent verifies the recent exclusion: the first 10
// entries of recent are excluded (recent[0] is the most recently played id),
// entries beyond the window still qualify, and excluding every candidate
// yields an empty list, not an error.
func TestRecommendMediaRecent(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	seedPlays(t, ps, base, "m1", 5)
	seedPlays(t, ps, base, "m2", 3)
	seedPlays(t, ps, base, "m3", 1)

	// the top-ranked m1 is excluded, so m2 leads
	got, err := RecommendMedia(s, []string{"m1", "m9", "m8"}, 10)
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	wantList(t, got, "m2", "m3")

	// only the first 10 recent entries are excluded: an id 10 positions back
	// still qualifies
	recent := make([]string, 11)
	for i := range recent {
		recent[i] = "skip"
	}
	recent[10] = "m2"
	got, err = RecommendMedia(s, recent, 10)
	if err != nil {
		t.Fatalf("recommend with long recent: %v", err)
	}
	wantList(t, got, "m1", "m2", "m3")

	// every candidate excluded: empty list, not an error
	got, err = RecommendMedia(s, []string{"m1", "m2", "m3"}, 10)
	if err != nil {
		t.Fatalf("recommend with all excluded: %v", err)
	}
	wantList(t, got)
}

// TestRecommendMediaLimit verifies the result cap: a positive limit keeps
// the top limit ids and a non-positive limit defaults to 5.
func TestRecommendMediaLimit(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlayEventService(s)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 7; i++ {
		seedPlays(t, ps, base, "m"+string(rune('1'+i)), 1)
	}

	got, err := RecommendMedia(s, nil, 3)
	if err != nil {
		t.Fatalf("recommend with limit: %v", err)
	}
	wantList(t, got, "m1", "m2", "m3")

	got, err = RecommendMedia(s, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "m1", "m2", "m3", "m4", "m5")

	got, err = RecommendMedia(s, nil, -2)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "m1", "m2", "m3", "m4", "m5")
}

// TestRecommendMediaEmptyStore verifies the degenerate inputs: an empty log
// yields an empty list, events without a media id never count and a nil
// store is rejected with ErrInvalid.
func TestRecommendMediaEmptyStore(t *testing.T) {
	s := newTestStore(t)

	got, err := RecommendMedia(s, nil, 5)
	if err != nil {
		t.Fatalf("recommend on empty store: %v", err)
	}
	wantList(t, got)

	ps := NewPlayEventService(s)
	for i := 0; i < 3; i++ {
		mustRecordPlay(t, ps, PlayEvent{PlaylistID: "p1"})
	}
	got, err = RecommendMedia(s, nil, 5)
	if err != nil {
		t.Fatalf("recommend with playlist-only plays: %v", err)
	}
	wantList(t, got)

	if _, err := RecommendMedia(nil, nil, 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for nil store, got %v", err)
	}
}
