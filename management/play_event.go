package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PlayResult classifies the outcome of a playback attempt.
type PlayResult string

const (
	// PlaySuccess marks a playback that started successfully.
	PlaySuccess PlayResult = "success"
	// PlayFailure marks a playback that failed to start.
	PlayFailure PlayResult = "failure"
)

// PlayEvent is one record of a playback attempt: which task started it,
// against which media item or playlist, when, and with what outcome. The
// target is either a media item (MediaID) or a playlist (PlaylistID); the
// other is empty. Events are append-only: Record assigns the ID and the
// (defaulted) timestamp, and no service mutates or removes individual
// events — only Prune drops whole ranges.
type PlayEvent struct {
	ID         string     `json:"id"`
	Time       time.Time  `json:"time"`
	TaskID     string     `json:"taskId,omitempty"`
	TaskName   string     `json:"taskName,omitempty"`
	MediaID    string     `json:"mediaId,omitempty"`
	PlaylistID string     `json:"playlistId,omitempty"`
	Result     PlayResult `json:"result"`
	Detail     string     `json:"detail,omitempty"`
}

// MediaCount is the play count of one media item, used by TopMedia.
type MediaCount struct {
	MediaID string
	Count   int
}

// HourCount is the play count of one hour of day (0-23), used by
// PlaysByHour.
type HourCount struct {
	Hour  int
	Count int
}

// PlayEventService provides append-only playback recording and playback
// statistics over a Store.
type PlayEventService struct {
	store *Store
}

// NewPlayEventService returns a PlayEventService backed by store.
func NewPlayEventService(store *Store) *PlayEventService {
	return &PlayEventService{store: store}
}

// Record appends an event to the playback log. At least one of MediaID and
// PlaylistID must be non-empty after trimming (ErrInvalid) and a non-empty
// result must be a known result (ErrInvalid); an empty result defaults to
// PlaySuccess. A zero Time is replaced with the current time. It returns
// the recorded event with its generated ID.
func (ps *PlayEventService) Record(ev PlayEvent) (*PlayEvent, error) {
	if strings.TrimSpace(ev.MediaID) == "" && strings.TrimSpace(ev.PlaylistID) == "" {
		return nil, fmt.Errorf("play event: %w: no media or playlist target", ErrInvalid)
	}
	if ev.Result == "" {
		ev.Result = PlaySuccess
	}
	if err := validatePlayResult(ev.Result); err != nil {
		return nil, err
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	e := &PlayEvent{
		ID:         newID(),
		Time:       ev.Time,
		TaskID:     ev.TaskID,
		TaskName:   ev.TaskName,
		MediaID:    ev.MediaID,
		PlaylistID: ev.PlaylistID,
		Result:     ev.Result,
		Detail:     ev.Detail,
	}
	err := ps.store.Update(func(d *Data) error {
		d.PlayEvents = append(d.PlayEvents, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// List returns the most recent limit playback events, newest first
// (ordered by Time descending; the ID breaks ties for a deterministic
// order). A limit <= 0 returns every event.
func (ps *PlayEventService) List(limit int) []*PlayEvent {
	out := make([]*PlayEvent, 0)
	ps.store.View(func(d *Data) {
		out = append(out, d.PlayEvents...)
	})
	sortPlayEvents(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// CountByResult counts playback events per result. Both the success and
// failure keys are always present, even when their count is zero; results
// other than success and failure, possible only in a hand-edited store,
// are ignored.
func (ps *PlayEventService) CountByResult() map[PlayResult]int {
	counts := map[PlayResult]int{PlaySuccess: 0, PlayFailure: 0}
	ps.store.View(func(d *Data) {
		for _, e := range d.PlayEvents {
			switch e.Result {
			case PlaySuccess, PlayFailure:
				counts[e.Result]++
			}
		}
	})
	return counts
}

// TopMedia returns the n most-played media items, ranked by play count
// descending; equal counts are ordered by MediaID ascending for a
// deterministic order. Events without a MediaID (playlist-only plays) never
// count. A n <= 0 returns an empty list.
func (ps *PlayEventService) TopMedia(n int) []MediaCount {
	if n <= 0 {
		return nil
	}
	counts := make(map[string]int)
	ps.store.View(func(d *Data) {
		for _, e := range d.PlayEvents {
			if e.MediaID != "" {
				counts[e.MediaID]++
			}
		}
	})
	out := make([]MediaCount, 0, len(counts))
	for id, c := range counts {
		out = append(out, MediaCount{MediaID: id, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].MediaID < out[j].MediaID
		}
		return out[i].Count > out[j].Count
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// PlaysByHour counts playback events per hour of day (0-23), using the
// hour of each event's own Time. All 24 hours are always present in the
// result, even when their count is zero. Events with a zero Time, possible
// only in a hand-edited store, are ignored.
func (ps *PlayEventService) PlaysByHour() []HourCount {
	counts := make([]int, 24)
	ps.store.View(func(d *Data) {
		for _, e := range d.PlayEvents {
			if e.Time.IsZero() {
				continue
			}
			counts[e.Time.Hour()]++
		}
	})
	out := make([]HourCount, 24)
	for h := 0; h < 24; h++ {
		out[h] = HourCount{Hour: h, Count: counts[h]}
	}
	return out
}

// Prune keeps only the newest maxEntries playback events (ordered by Time
// descending, with the ID breaking ties) and removes the rest, returning
// the number removed. The surviving events keep their original document
// order. A maxEntries <= 0 clears the entire playback log. When nothing is
// removed it is a no-op: the store is not rewritten and root UpdatedAt is
// not bumped.
func (ps *PlayEventService) Prune(maxEntries int) (int, error) {
	removed := 0
	err := ps.store.Update(func(d *Data) error {
		if maxEntries <= 0 {
			if len(d.PlayEvents) == 0 {
				return errNoop
			}
			removed = len(d.PlayEvents)
			d.PlayEvents = d.PlayEvents[:0]
			return nil
		}
		if len(d.PlayEvents) <= maxEntries {
			return errNoop
		}
		sorted := make([]*PlayEvent, len(d.PlayEvents))
		copy(sorted, d.PlayEvents)
		sortPlayEvents(sorted)
		keep := make(map[*PlayEvent]bool, maxEntries)
		for _, e := range sorted[:maxEntries] {
			keep[e] = true
		}
		kept := d.PlayEvents[:0]
		for _, e := range d.PlayEvents {
			if keep[e] {
				kept = append(kept, e)
			}
		}
		d.PlayEvents = kept
		removed = len(sorted) - maxEntries
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// sortPlayEvents orders events newest first: Time descending, with the ID
// breaking ties (ascending) for a deterministic order.
func sortPlayEvents(events []*PlayEvent) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Time.Equal(events[j].Time) {
			return events[i].ID < events[j].ID
		}
		return events[i].Time.After(events[j].Time)
	})
}

// validatePlayResult reports whether r is a known play result.
func validatePlayResult(r PlayResult) error {
	switch r {
	case PlaySuccess, PlayFailure:
		return nil
	}
	return fmt.Errorf("play event: %w: unknown result %q", ErrInvalid, r)
}
