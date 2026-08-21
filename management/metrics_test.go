package management

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

// TestMetricsMediaFailureRate verifies the per-media aggregation: exact
// MediaID matching, success/failure counting, the failure ratio and the
// zero results for an empty or unknown media.
func TestMetricsMediaFailureRate(t *testing.T) {
	s := newTestStore(t)
	ms := NewMetricsService(s)

	// an empty event log yields zeros with no error
	rate, plays, failures, err := ms.MediaFailureRate("m1")
	if err != nil {
		t.Fatalf("unexpected error on empty log: %v", err)
	}
	if rate != 0 || plays != 0 || failures != 0 {
		t.Fatalf("expected 0/0/0 on empty log, got %v/%v/%v", rate, plays, failures)
	}

	ps := NewPlayEventService(s)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), MediaID: "m1"})
	}
	for i := 0; i < 2; i++ {
		mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), MediaID: "m1", Result: PlayFailure})
	}
	// events of other media and playlist-only plays never count for m1
	mustRecordPlay(t, ps, PlayEvent{Time: base, MediaID: "m2", Result: PlayFailure})
	mustRecordPlay(t, ps, PlayEvent{Time: base, PlaylistID: "p1", Result: PlayFailure})

	rate, plays, failures, err = ms.MediaFailureRate("m1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plays != 5 || failures != 2 {
		t.Fatalf("unexpected counts: plays=%d failures=%d", plays, failures)
	}
	if rate != 0.4 {
		t.Fatalf("expected rate 0.4, got %v", rate)
	}

	// an unknown media yields zeros
	rate, plays, failures, err = ms.MediaFailureRate("nope")
	if err != nil {
		t.Fatalf("unexpected error for unknown media: %v", err)
	}
	if rate != 0 || plays != 0 || failures != 0 {
		t.Fatalf("expected 0/0/0 for unknown media, got %v/%v/%v", rate, plays, failures)
	}
}

// TestMetricsMediaFailureRateIgnoresUnknownResult verifies that events with
// an unknown result, possible only in a hand-edited store, are ignored by
// the aggregation.
func TestMetricsMediaFailureRateIgnoresUnknownResult(t *testing.T) {
	s := newTestStore(t)
	ms := NewMetricsService(s)
	ps := NewPlayEventService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	mustRecordPlay(t, ps, PlayEvent{Time: base, MediaID: "m1"})
	mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Minute), MediaID: "m1", Result: PlayFailure})
	if err := s.Update(func(d *Data) error {
		d.PlayEvents = append(d.PlayEvents, &PlayEvent{ID: "x1", Time: base.Add(2 * time.Minute), MediaID: "m1", Result: "maybe"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rate, plays, failures, err := ms.MediaFailureRate("m1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plays != 2 || failures != 1 {
		t.Fatalf("expected unknown result ignored, got plays=%d failures=%d", plays, failures)
	}
	if rate != 0.5 {
		t.Fatalf("expected rate 0.5, got %v", rate)
	}

	// a media with only unknown results yields zeros
	if err := s.Update(func(d *Data) error {
		d.PlayEvents = append(d.PlayEvents, &PlayEvent{ID: "x2", Time: base.Add(3 * time.Minute), MediaID: "m2", Result: "maybe"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rate, plays, failures, err = ms.MediaFailureRate("m2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rate != 0 || plays != 0 || failures != 0 {
		t.Fatalf("expected 0/0/0 for only unknown results, got %v/%v/%v", rate, plays, failures)
	}
}

// TestMetricsOutputStabilityTrend verifies the per-day alarm counts: only
// warning-level alarms whose title contains "Output failover failed" count,
// days without matching alarms stay present with count 0, and the window
// holds the most recent days days, oldest first, ending today.
func TestMetricsOutputStabilityTrend(t *testing.T) {
	s := newTestStore(t)
	ms := NewMetricsService(s)

	// no alarms at all: a 7-day window is fully present with zero counts
	got, err := ms.OutputStabilityTrend(7)
	if err != nil {
		t.Fatalf("trend(7): %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("expected 7 days, got %d", len(got))
	}
	now := time.Now()
	for i, dc := range got {
		if want := now.AddDate(0, 0, i-6).Format("2006-01-02"); dc.Date != want {
			t.Fatalf("expected day %d to be %s, got %s", i, want, dc.Date)
		}
		if dc.Count != 0 {
			t.Fatalf("expected count 0 for day %d, got %d", i, dc.Count)
		}
	}

	// seed matching alarms today, two days ago and six days ago, plus
	// non-matching ones (wrong level or wrong title) that must not count
	seed := func(level AlarmLevel, title string, daysAgo int) {
		t.Helper()
		at := now.AddDate(0, 0, -daysAgo)
		if err := s.Update(func(d *Data) error {
			d.Alarms = append(d.Alarms, &Alarm{
				ID: newID(), Level: level, Title: title,
				Status: AlarmStatusActive, CreatedAt: at, UpdatedAt: at,
			})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed(AlarmLevelWarning, "Output failover failed", 0)
	seed(AlarmLevelWarning, "Output failover failed: output 2 offline", 0)
	seed(AlarmLevelWarning, "Output failover failed", 2)
	seed(AlarmLevelWarning, "Output failover failed", 6)
	seed(AlarmLevelInfo, "Output failover failed", 0)
	seed(AlarmLevelCritical, "Output failover failed", 0)
	seed(AlarmLevelWarning, "Scheduled playback failed", 0)
	seed(AlarmLevelWarning, "Output group degraded", 0)

	got, err = ms.OutputStabilityTrend(7)
	if err != nil {
		t.Fatalf("trend(7): %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("expected 7 days, got %d", len(got))
	}
	counts := map[string]int{}
	for _, dc := range got {
		counts[dc.Date] = dc.Count
	}
	want := map[string]int{
		now.Format("2006-01-02"):                   2,
		now.AddDate(0, 0, -2).Format("2006-01-02"): 1,
		now.AddDate(0, 0, -6).Format("2006-01-02"): 1,
	}
	for date, c := range want {
		if counts[date] != c {
			t.Fatalf("expected count %d for %s, got %d", c, date, counts[date])
		}
	}
	for date, c := range counts {
		if _, ok := want[date]; !ok && c != 0 {
			t.Fatalf("unexpected count %d for %s", c, date)
		}
	}
	// chronological order: oldest first, today last
	if got[0].Date != now.AddDate(0, 0, -6).Format("2006-01-02") {
		t.Fatalf("expected the window to start six days ago, got %s", got[0].Date)
	}
	if got[6].Date != now.Format("2006-01-02") {
		t.Fatalf("expected the window to end today, got %s", got[6].Date)
	}

	// a 5-day window drops the six-day-old alarm entirely
	got, err = ms.OutputStabilityTrend(5)
	if err != nil {
		t.Fatalf("trend(5): %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 days, got %d", len(got))
	}
	if got[0].Date != now.AddDate(0, 0, -4).Format("2006-01-02") {
		t.Fatalf("expected 5-day window to start four days ago, got %s", got[0].Date)
	}
	counts = map[string]int{}
	for _, dc := range got {
		counts[dc.Date] = dc.Count
	}
	if counts[now.Format("2006-01-02")] != 2 || counts[now.AddDate(0, 0, -2).Format("2006-01-02")] != 1 {
		t.Fatalf("unexpected counts in 5-day window: %v", counts)
	}
}

// TestMetricsOutputStabilityTrendDaysBoundary verifies the 1-day window
// (today only) and the ErrInvalid rejection of non-positive days.
func TestMetricsOutputStabilityTrendDaysBoundary(t *testing.T) {
	s := newTestStore(t)
	ms := NewMetricsService(s)

	now := time.Now()
	if err := s.Update(func(d *Data) error {
		d.Alarms = append(d.Alarms, &Alarm{
			ID: "a1", Level: AlarmLevelWarning, Title: "Output failover failed",
			Status: AlarmStatusActive, CreatedAt: now.AddDate(0, 0, -1), UpdatedAt: now.AddDate(0, 0, -1),
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ms.OutputStabilityTrend(1)
	if err != nil {
		t.Fatalf("trend(1): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 day, got %d", len(got))
	}
	if got[0].Date != now.Format("2006-01-02") || got[0].Count != 0 {
		t.Fatalf("expected today with count 0, got %+v", got[0])
	}

	for _, days := range []int{0, -1, -7} {
		if _, err := ms.OutputStabilityTrend(days); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for days=%d, got %v", days, err)
		}
	}
}

// TestMetricsPlaybackSummary verifies the whole-log aggregation: totals,
// per-result counts and the success ratio, with zeros for an empty log.
func TestMetricsPlaybackSummary(t *testing.T) {
	s := newTestStore(t)
	ms := NewMetricsService(s)

	// an empty log yields zeros, including a 0 success rate
	sum := ms.PlaybackSummary()
	if sum.TotalPlays != 0 || sum.Successes != 0 || sum.Failures != 0 || sum.SuccessRate != 0 {
		t.Fatalf("unexpected empty summary: %+v", sum)
	}

	ps := NewPlayEventService(s)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		mustRecordPlay(t, ps, PlayEvent{Time: base.Add(time.Duration(i) * time.Minute), MediaID: "m1"})
	}
	mustRecordPlay(t, ps, PlayEvent{Time: base, MediaID: "m2", Result: PlayFailure})
	// an unknown result, possible only in a hand-edited store, is ignored
	if err := s.Update(func(d *Data) error {
		d.PlayEvents = append(d.PlayEvents, &PlayEvent{ID: "x1", Time: base, MediaID: "m3", Result: "maybe"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sum = ms.PlaybackSummary()
	if sum.TotalPlays != 4 || sum.Successes != 3 || sum.Failures != 1 {
		t.Fatalf("unexpected summary: %+v", sum)
	}
	if sum.SuccessRate != 0.75 {
		t.Fatalf("expected success rate 0.75, got %v", sum.SuccessRate)
	}
}

// TestMetricsPureAggregation verifies that the metrics methods only read
// the store: neither the store file bytes nor root UpdatedAt change.
func TestMetricsPureAggregation(t *testing.T) {
	s := newTestStore(t)
	ms := NewMetricsService(s)
	ps := NewPlayEventService(s)
	if _, err := ps.Record(PlayEvent{MediaID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Record(PlayEvent{MediaID: "m1", Result: PlayFailure}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAlarmService(s).Raise(AlarmLevelWarning, "Output failover failed", "boom"); err != nil {
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

	// Let time pass so a spurious write would be observable both in the
	// file bytes and in root UpdatedAt.
	time.Sleep(10 * time.Millisecond)

	if _, _, _, err := ms.MediaFailureRate("m1"); err != nil {
		t.Fatalf("media failure rate: %v", err)
	}
	if _, err := ms.OutputStabilityTrend(7); err != nil {
		t.Fatalf("output stability trend: %v", err)
	}
	ms.PlaybackSummary()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected metrics aggregation not to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UpdatedAt.Equal(rootBefore) {
		t.Fatalf("expected root UpdatedAt unchanged, got %v vs %v", snap.UpdatedAt, rootBefore)
	}
}
