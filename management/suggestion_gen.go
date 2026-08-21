package management

import (
	"fmt"
	"sort"
)

// Defaults for the heuristic recommendation step. A caller that does not
// set its own value (zero or negative) falls back to these, mirroring the
// SmartRule limit defaults.
const (
	// defaultRecommendLimit caps how many media ids RecommendMedia returns
	// when the caller does not set its own limit.
	defaultRecommendLimit = 5
	// defaultRecentExclude is how many of the most recently played media ids
	// are excluded from recommendations, so media that just played are not
	// suggested again.
	defaultRecentExclude = 10
)

// RecommendMedia is the heuristic recommendation step of the
// AI-suggestion skeleton: it ranks the playback log by play frequency — the
// same semantics as PlayEventService.TopMedia (play count descending, media
// id ascending breaking ties, playlist-only plays never count) — excludes
// the ids of the most recently played media (the first defaultRecentExclude
// entries of recent, where recent[0] is the most recently played id), and
// returns the top limit ids (default defaultRecommendLimit).
//
// Real AI inference is a non-goal for this batch: RecommendMedia implements
// the frequency heuristic that stands in for it, so the suggestion pipeline
// (generate → review → apply via SuggestionService) has a working skeleton
// end to end and an AI backend can be plugged in behind the same signature.
//
// It only reads the store, so the result can be previewed freely; persisting
// it as a suggestion is the caller's explicit choice. An empty log, or one
// whose candidates are all excluded, yields an empty list, not an error. A
// nil store is rejected with ErrInvalid.
func RecommendMedia(store *Store, recent []string, limit int) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("suggestion generate: %w: nil store", ErrInvalid)
	}
	if limit <= 0 {
		limit = defaultRecommendLimit
	}

	// Exclusion happens before ranking so the result stays full: a media
	// pushed out of the top by a recently played id is not silently dropped.
	excluded := make(map[string]bool, len(recent))
	lookback := defaultRecentExclude
	if len(recent) < lookback {
		lookback = len(recent)
	}
	for i := 0; i < lookback; i++ {
		excluded[recent[i]] = true
	}

	counts := make(map[string]int)
	store.View(func(d *Data) {
		for _, e := range d.PlayEvents {
			if e.MediaID == "" || excluded[e.MediaID] {
				continue
			}
			counts[e.MediaID]++
		}
	})
	if len(counts) == 0 {
		return []string{}, nil
	}

	ranked := make([]MediaCount, 0, len(counts))
	for id, c := range counts {
		ranked = append(ranked, MediaCount{MediaID: id, Count: c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Count == ranked[j].Count {
			return ranked[i].MediaID < ranked[j].MediaID
		}
		return ranked[i].Count > ranked[j].Count
	})
	out := make([]string, 0, limit)
	for _, r := range ranked {
		if len(out) >= limit {
			break
		}
		out = append(out, r.MediaID)
	}
	return out, nil
}
