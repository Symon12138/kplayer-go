package management

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// genMedia builds a Media entry for generation tests: tags, probed duration
// and probe flag are set directly because GeneratePlaylist is a pure function
// over library data, not over the store.
func genMedia(id string, tags []string, dur float64, probed bool) *Media {
	return &Media{ID: id, Name: id, Path: "/v/" + id + ".mp4", Tags: tags, Duration: dur, Probed: probed}
}

// wantList compares an ordered id list against want and fails the test with
// the ids shown when they differ. Empty results compare equal whether they
// are nil or an empty non-nil slice.
func wantList(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestGeneratePlaylistTimeSlots covers the time-slot filter: hits and misses
// at the inclusive boundaries, an all-day rule, and a multi-slot rule.
func TestGeneratePlaylistTimeSlots(t *testing.T) {
	media := []*Media{
		{ID: "m1", Path: "/v/1.mp4"},
		{ID: "m2", Path: "/v/2.mp4"},
	}
	at := func(hour int) time.Time { return time.Date(2026, 8, 14, hour, 30, 0, 0, time.UTC) }
	rule := &SmartRule{TimeSlots: []TimeSlot{{StartHour: 9, EndHour: 12}}}

	// Hits inside the slot, both boundaries included (start 09:00, end 12:30).
	for _, hour := range []int{9, 10, 12} {
		got, err := GeneratePlaylist(rule, media, nil, at(hour))
		if err != nil {
			t.Fatalf("hour %d: %v", hour, err)
		}
		wantList(t, got, "m1", "m2")
	}

	// Misses just outside the boundaries yield an empty list, not an error:
	// 当前时段无排播.
	for _, hour := range []int{8, 13} {
		got, err := GeneratePlaylist(rule, media, nil, at(hour))
		if err != nil {
			t.Fatalf("hour %d: %v", hour, err)
		}
		wantList(t, got)
	}

	// Empty TimeSlots means all-day scheduling, whatever the hour.
	allDay := &SmartRule{}
	for _, hour := range []int{0, 3, 23} {
		got, err := GeneratePlaylist(allDay, media, nil, at(hour))
		if err != nil {
			t.Fatalf("all-day hour %d: %v", hour, err)
		}
		wantList(t, got, "m1", "m2")
	}

	// A multi-slot rule matches any of its slots, with the same boundary
	// semantics; the gap between them is not scheduled.
	multi := &SmartRule{TimeSlots: []TimeSlot{{StartHour: 9, EndHour: 12}, {StartHour: 14, EndHour: 18}}}
	got, err := GeneratePlaylist(multi, media, nil, at(14))
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "m1", "m2")
	got, err = GeneratePlaylist(multi, media, nil, at(18))
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "m1", "m2")
	got, err = GeneratePlaylist(multi, media, nil, at(13))
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got)
}

// TestGeneratePlaylistTags covers the tag filter: any single shared tag
// qualifies, no intersection yields an empty result, and empty rule tags
// match the whole library.
func TestGeneratePlaylistTags(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	media := []*Media{
		genMedia("news1", []string{"news", "morning"}, 0, false),
		genMedia("sports1", []string{"sports"}, 0, false),
		genMedia("weather1", []string{"weather"}, 0, false),
		genMedia("untagged", nil, 0, false),
	}

	// One shared tag qualifies (news1 shares "news", sports1 shares "sports").
	rule := &SmartRule{Tags: []string{"news", "sports"}}
	got, err := GeneratePlaylist(rule, media, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "news1", "sports1")

	// No intersection: nothing qualifies, including untagged media.
	rule = &SmartRule{Tags: []string{"music"}}
	got, err = GeneratePlaylist(rule, media, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got)

	// Empty rule tags: the whole library qualifies, tagged or not.
	rule = &SmartRule{}
	got, err = GeneratePlaylist(rule, media, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "news1", "sports1", "weather1", "untagged")
}

// TestGeneratePlaylistDuration covers the duration filter: over-limit probed
// media are excluded, the limit itself is allowed, media with unknown
// duration pass through, and a non-positive limit disables the filter.
func TestGeneratePlaylistDuration(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	media := []*Media{
		genMedia("long", nil, 600, true),
		genMedia("boundary", nil, 300, true),
		genMedia("short", nil, 120, true),
		genMedia("unknown", nil, 600, false),
		genMedia("unprobed", nil, 0, false),
	}

	rule := &SmartRule{MaxDurationSec: 300}
	got, err := GeneratePlaylist(rule, media, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	// "long" exceeds the limit and is excluded; the exact limit and shorter
	// durations are kept; unknown durations are let through.
	wantList(t, got, "boundary", "short", "unknown", "unprobed")

	// Non-positive limit: no duration filtering at all.
	rule = &SmartRule{MaxDurationSec: 0}
	got, err = GeneratePlaylist(rule, media, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "long", "boundary", "short", "unknown", "unprobed")
}

// TestGeneratePlaylistAvoidRepeat covers the repeat filter: ids inside the
// lookback window of the recent history are excluded, ids beyond it are
// allowed, the lookback defaults to 10, and the filter is off unless the rule
// opts in.
func TestGeneratePlaylistAvoidRepeat(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	media := []*Media{
		genMedia("a", nil, 0, false),
		genMedia("b", nil, 0, false),
		genMedia("c", nil, 0, false),
	}

	// recent[0] is the most recently played id; only the first RepeatLookback
	// entries count, so "c" (at index 2, past the window of 2) is allowed.
	recent := []string{"a", "b", "c", "d"}
	rule := &SmartRule{AvoidRepeat: true, RepeatLookback: 2}
	got, err := GeneratePlaylist(rule, media, recent, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "c")

	// RepeatLookback <= 0 falls back to the default of 10: an id 10 plays
	// back is excluded, one 11 plays back is allowed.
	longRecent := make([]string, 12)
	longRecent[9] = "a"
	longRecent[10] = "b"
	rule = &SmartRule{AvoidRepeat: true}
	got, err = GeneratePlaylist(rule, media, longRecent, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "b", "c")

	// An explicit lookback widens the window: with 11, "b" is excluded too.
	rule = &SmartRule{AvoidRepeat: true, RepeatLookback: 11}
	got, err = GeneratePlaylist(rule, media, longRecent, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "c")

	// AvoidRepeat off: repeats are allowed even when recent is full of ids.
	rule = &SmartRule{RepeatLookback: 2}
	got, err = GeneratePlaylist(rule, media, recent, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "a", "b", "c")
}

// TestGeneratePlaylistMaxItems covers truncation: the result is cut to
// MaxItems in library order, and an unset limit defaults to 20.
func TestGeneratePlaylistMaxItems(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)

	media := make([]*Media, 5)
	for i := range media {
		media[i] = genMedia(string(rune('a'+i)), nil, 0, false)
	}
	rule := &SmartRule{MaxItems: 3}
	got, err := GeneratePlaylist(rule, media, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "a", "b", "c")

	// Truncation applies after filtering, so only the first MaxItems of the
	// surviving media are returned ("t4" is filtered out by tag, "t5" is the
	// fourth match and is cut by the cap).
	tagged := []*Media{
		genMedia("t1", []string{"news"}, 0, false),
		genMedia("t2", []string{"news"}, 0, false),
		genMedia("t3", []string{"news"}, 0, false),
		genMedia("t4", []string{"sports"}, 0, false),
		genMedia("t5", []string{"news"}, 0, false),
	}
	rule = &SmartRule{MaxItems: 3, Tags: []string{"news"}}
	got, err = GeneratePlaylist(rule, tagged, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, got, "t1", "t2", "t3")

	// MaxItems <= 0 defaults to 20.
	media = make([]*Media, 25)
	for i := range media {
		media[i] = genMedia(string(rune('a'+i)), nil, 0, false)
	}
	rule = &SmartRule{}
	got, err = GeneratePlaylist(rule, media, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("expected default cap of 20, got %d", len(got))
	}
	for i := range got {
		if want := string(rune('a' + i)); got[i] != want {
			t.Fatalf("default cap keeps library order: index %d = %q, want %q", i, got[i], want)
		}
	}
}

// TestGeneratePlaylistDeterministic verifies that identical inputs produce
// identical outputs, and that the result mirrors the input library order
// (library order is the programming order).
func TestGeneratePlaylistDeterministic(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	rule := &SmartRule{
		TimeSlots:      []TimeSlot{{StartHour: 9, EndHour: 12}},
		Tags:           []string{"news"},
		MaxDurationSec: 450,
		AvoidRepeat:    true,
		RepeatLookback: 2,
		MaxItems:       3,
	}
	media := []*Media{
		genMedia("m1", []string{"news"}, 100, true),
		genMedia("m2", []string{"news"}, 200, true),
		genMedia("m3", []string{"news"}, 300, true),
		genMedia("m4", []string{"news"}, 400, true),
		genMedia("m5", []string{"news"}, 500, true),
		genMedia("m6", []string{"sports"}, 100, true),
	}
	recent := []string{"m9", "m8"}

	first, err := GeneratePlaylist(rule, media, recent, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GeneratePlaylist(rule, media, recent, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same inputs produced different results: %v vs %v", first, second)
	}
	// Eligible in library order: m1..m4 (m5 over the 450s limit, m6 not
	// tagged), truncated to 3.
	wantList(t, first, "m1", "m2", "m3")

	// Reversing the library order reverses the selection deterministically.
	rev := make([]*Media, len(media))
	for i := range media {
		rev[len(media)-1-i] = media[i]
	}
	reversed, err := GeneratePlaylist(rule, rev, recent, now)
	if err != nil {
		t.Fatal(err)
	}
	wantList(t, reversed, "m4", "m3", "m2")
}

// TestGeneratePlaylistInvalidInputs covers the degenerate inputs: a nil rule
// is rejected with ErrInvalid, a nil library yields an empty result without
// an error.
func TestGeneratePlaylistInvalidInputs(t *testing.T) {
	if _, err := GeneratePlaylist(nil, nil, nil, time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for nil rule, got %v", err)
	}
	got, err := GeneratePlaylist(&SmartRule{}, nil, nil, time.Now())
	if err != nil {
		t.Fatalf("nil media should not error, got %v", err)
	}
	wantList(t, got)
}

// TestApplyGenerated covers the explicit application step: the generated ids
// are persisted as a new playlist in order, an empty list is rejected with
// ErrInvalid, and a missing media id is rejected by Create with ErrNotFound.
func TestApplyGenerated(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")

	// Generation never writes to the store; application persists the result
	// under the given name, keeping the generated order.
	p, err := ApplyGenerated(s, "generated morning", []string{m2.ID, m1.ID})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if p.Name != "generated morning" {
		t.Fatalf("unexpected playlist name %q", p.Name)
	}
	wantList(t, p.MediaIDs(), m2.ID, m1.ID)
	// The playlist is really in the store and resolvable by service.
	ps := NewPlaylistService(s)
	got, err := ps.Get(p.ID)
	if err != nil {
		t.Fatalf("get created playlist: %v", err)
	}
	wantList(t, got.MediaIDs(), m2.ID, m1.ID)

	// An empty media list is rejected.
	if _, err := ApplyGenerated(s, "empty", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty media list, got %v", err)
	}

	// A reference to a missing media is rejected by Create with ErrNotFound
	// and nothing is persisted.
	if _, err := ApplyGenerated(s, "broken", []string{m1.ID, "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing media, got %v", err)
	}
	for _, pl := range ps.List() {
		if pl.Name == "broken" {
			t.Fatal("playlist with missing media must not be persisted")
		}
	}
}
