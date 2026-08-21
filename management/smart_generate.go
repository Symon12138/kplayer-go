package management

import (
	"fmt"
	"time"
)

// Defaults for the optional SmartRule limits. A rule that does not set its
// own value (zero or negative) falls back to these, so rules stay concise
// while the generated program stays bounded.
const (
	// defaultRepeatLookback is how many of the most recently played media ids
	// are checked when a rule requests no repeats without setting its own
	// lookback window.
	defaultRepeatLookback = 10
	// defaultMaxItems caps the generated playlist length when the rule does
	// not set its own limit.
	defaultMaxItems = 20
)

// GeneratePlaylist is the pure generation step of the smart-program feature:
// it filters the media library down to what the rule allows at the given
// moment and returns the ordered media ids. It never touches the store, so
// the result can be previewed freely; persisting it is the caller's explicit
// choice (see ApplyGenerated).
//
// The filters are conjunctive and applied in a fixed order — time slot, tags,
// duration, recent-repeat — and the result keeps the input library order
// (library order is the programming order), which makes generation
// deterministic: the same rule, library, play history and time always yield
// the same playlist. The result is truncated to MaxItems (default 20).
//
// A nil rule is rejected with ErrInvalid. A nil or empty library yields an
// empty result, not an error.
func GeneratePlaylist(rule *SmartRule, media []*Media, recent []string, now time.Time) ([]string, error) {
	if rule == nil {
		return nil, fmt.Errorf("smart generate: %w: nil rule", ErrInvalid)
	}

	// The rule is not scheduled for the current hour (no matching time
	// slot): 当前时段无排播, so the result is an empty list, not an error.
	if !inTimeSlots(rule.TimeSlots, now.Hour()) {
		return []string{}, nil
	}

	lookback := rule.RepeatLookback
	if lookback <= 0 {
		lookback = defaultRepeatLookback
	}
	maxItems := rule.MaxItems
	if maxItems <= 0 {
		maxItems = defaultMaxItems
	}

	out := make([]string, 0, len(media))
	for _, m := range media {
		if m == nil {
			continue
		}
		if !matchesTags(rule.Tags, m.Tags) {
			continue
		}
		if rule.MaxDurationSec > 0 && knownDuration(m) && m.Duration > float64(rule.MaxDurationSec) {
			continue
		}
		if rule.AvoidRepeat && inRecent(recent, m.ID, lookback) {
			continue
		}
		out = append(out, m.ID)
		if len(out) >= maxItems {
			break
		}
	}
	return out, nil
}

// ApplyGenerated is the explicit application step of the smart-program
// feature: it persists the generated media ids as a new playlist under the
// given name, going through PlaylistService.Create so all of its validations
// apply (name required, every referenced media id must exist). Generation
// itself never writes to the store; only an explicit ApplyGenerated call
// does, so the operator stays in control of what enters the schedule.
//
// An empty media list is rejected with ErrInvalid; a reference to a missing
// media is rejected by Create with an error wrapping ErrNotFound and nothing
// is persisted. The created playlist is returned to the caller.
func ApplyGenerated(store *Store, playlistName string, mediaIDs []string) (*Playlist, error) {
	if len(mediaIDs) == 0 {
		return nil, fmt.Errorf("smart generate: %w: empty media list", ErrInvalid)
	}
	return NewPlaylistService(store).Create(playlistName, "", mediaIDs, false)
}

// inTimeSlots reports whether hour falls inside any of slots. Slots are
// inclusive on both ends ([StartHour, EndHour]); an empty slot list means
// all-day scheduling. A slot whose start is later than its end matches
// nothing: ranges that cross midnight must be expressed as two adjacent
// slots (for example 22-23 and 0-6).
func inTimeSlots(slots []TimeSlot, hour int) bool {
	if len(slots) == 0 {
		return true
	}
	for _, s := range slots {
		if hour >= s.StartHour && hour <= s.EndHour {
			return true
		}
	}
	return false
}

// matchesTags reports whether any rule tag appears in the media tags. An
// empty rule tag list matches everything (all media qualify); otherwise a
// single shared tag qualifies the media. Comparison is exact and
// case-sensitive, mirroring how tags are stored.
func matchesTags(ruleTags, mediaTags []string) bool {
	if len(ruleTags) == 0 {
		return true
	}
	for _, rt := range ruleTags {
		for _, mt := range mediaTags {
			if rt == mt {
				return true
			}
		}
	}
	return false
}

// knownDuration reports whether m carries a usable duration. Only probed
// media have trustworthy durations (Media.Probed); a media without a probe
// result — or with a non-positive duration — has an unknown duration and is
// allowed through the duration filter, per the "unknown means allowed" rule.
func knownDuration(m *Media) bool {
	return m.Probed && m.Duration > 0
}

// inRecent reports whether id appears within the first lookback entries of
// recent, where recent[0] is the most recently played id. A shorter history
// is checked in full.
func inRecent(recent []string, id string, lookback int) bool {
	if lookback > len(recent) {
		lookback = len(recent)
	}
	for i := 0; i < lookback; i++ {
		if recent[i] == id {
			return true
		}
	}
	return false
}
