package management

import (
	"fmt"
	"strings"
	"time"
)

// DayCount is the alarm count of one calendar day, used by
// OutputStabilityTrend. Date is the day in "2006-01-02" form.
type DayCount struct {
	Date  string
	Count int
}

// PlaybackSummary is the aggregate playback outcome across every playback
// event of the store.
type PlaybackSummary struct {
	TotalPlays  int
	Successes   int
	Failures    int
	SuccessRate float64
}

// MetricsService provides read-only metrics over the playback events and
// alarms of a Store. Every method is a pure aggregation: the store is only
// read, never mutated or persisted.
type MetricsService struct {
	store *Store
}

// NewMetricsService returns a MetricsService backed by store.
func NewMetricsService(store *Store) *MetricsService {
	return &MetricsService{store: store}
}

// MediaFailureRate aggregates the playback events of one media item,
// matched exactly by MediaID. plays counts the events with a known result
// (success or failure), failures the failures, and rate the failure ratio
// failures/plays. Events with an unknown result, possible only in a
// hand-edited store, are ignored; playlist-only events (empty MediaID)
// match only an empty mediaID. With no matching plays the rate is 0. The
// method never fails: err is always nil.
func (ms *MetricsService) MediaFailureRate(mediaID string) (rate float64, plays int, failures int, err error) {
	ms.store.View(func(d *Data) {
		for _, e := range d.PlayEvents {
			if e.MediaID != mediaID {
				continue
			}
			switch e.Result {
			case PlaySuccess:
				plays++
			case PlayFailure:
				plays++
				failures++
			}
		}
	})
	if plays > 0 {
		rate = float64(failures) / float64(plays)
	}
	return rate, plays, failures, nil
}

// OutputStabilityTrend counts output-related alarms per calendar day:
// warning-level alarms whose title contains "Output failover failed" are
// counted by the date of their CreatedAt. It returns the most recent days
// days, oldest first, ending today; every day of the window is always
// present, with count 0 for days without matching alarms (the same
// all-keys-present pattern as PlaysByHour). Alarms with a zero CreatedAt,
// possible only in a hand-edited store, are ignored. A days <= 0 is
// rejected with ErrInvalid.
func (ms *MetricsService) OutputStabilityTrend(days int) ([]DayCount, error) {
	if days <= 0 {
		return nil, fmt.Errorf("metrics: %w: days must be positive", ErrInvalid)
	}
	now := time.Now()
	counts := make(map[string]int)
	ms.store.View(func(d *Data) {
		for _, a := range d.Alarms {
			if a.Level != AlarmLevelWarning || !strings.Contains(a.Title, "Output failover failed") {
				continue
			}
			if a.CreatedAt.IsZero() {
				continue
			}
			counts[a.CreatedAt.Format("2006-01-02")]++
		}
	})
	out := make([]DayCount, days)
	for i := 0; i < days; i++ {
		date := now.AddDate(0, 0, i-days+1).Format("2006-01-02")
		out[i] = DayCount{Date: date, Count: counts[date]}
	}
	return out, nil
}

// PlaybackSummary aggregates every playback event of the store: TotalPlays
// is the number of events with a known result, Successes and Failures the
// per-result counts, and SuccessRate the success ratio
// Successes/TotalPlays. Events with an unknown result, possible only in a
// hand-edited store, are ignored. With no events the SuccessRate is 0.
func (ms *MetricsService) PlaybackSummary() PlaybackSummary {
	var sum PlaybackSummary
	ms.store.View(func(d *Data) {
		for _, e := range d.PlayEvents {
			switch e.Result {
			case PlaySuccess:
				sum.TotalPlays++
				sum.Successes++
			case PlayFailure:
				sum.TotalPlays++
				sum.Failures++
			}
		}
	})
	if sum.TotalPlays > 0 {
		sum.SuccessRate = float64(sum.Successes) / float64(sum.TotalPlays)
	}
	return sum
}
