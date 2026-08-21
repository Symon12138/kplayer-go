package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TimeSlot is a daily time window during which a smart rule applies. Both
// hours are inclusive and must lie in [0, 23] with StartHour <= EndHour.
// Windows crossing midnight are not supported: a rule that should span
// midnight must be expressed as two slots (for example 22-23 and 0-2).
type TimeSlot struct {
	StartHour int `json:"startHour"`
	EndHour   int `json:"endHour"`
}

// SmartRule is an intelligent scheduling rule: a named selection policy the
// smart generation engine consults when it builds a program schedule. The
// management side only stores the rule; media matching and schedule
// generation are the engine's concern, and a generated result takes effect
// only when the user applies it explicitly — a rule never overwrites an
// existing schedule on its own.
//
// TimeSlots restricts the rule to daily windows; an empty list means the
// rule applies all day. Tags filters the media library: an item qualifies
// when any of its Media.Tags matches one of the listed tags exactly; an
// empty list means no tag filter. MaxDurationSec, when positive, caps the
// duration of each selected item against Media.Duration (in seconds); a
// non-positive value means no cap. AvoidRepeat asks the engine to skip
// media played recently, considering the most recent RepeatLookback entries
// of playback history (a non-positive lookback defaults to 10). MaxItems
// bounds the number of items generated for the rule; a non-positive value
// defaults to 20. Only enabled rules take part in generation.
type SmartRule struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Description    string     `json:"description,omitempty"`
	TimeSlots      []TimeSlot `json:"timeSlots,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
	MaxDurationSec int        `json:"maxDurationSec,omitempty"`
	AvoidRepeat    bool       `json:"avoidRepeat"`
	RepeatLookback int        `json:"repeatLookback,omitempty"`
	MaxItems       int        `json:"maxItems,omitempty"`
	Enabled        bool       `json:"enabled"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// SmartRuleSpec is the validated input used to create or replace a smart
// rule.
type SmartRuleSpec struct {
	Name           string
	Description    string
	TimeSlots      []TimeSlot
	Tags           []string
	MaxDurationSec int
	AvoidRepeat    bool
	RepeatLookback int
	MaxItems       int
	Enabled        bool
}

// SmartRuleService provides CRUD over the smart rules of a Store.
type SmartRuleService struct {
	store *Store
}

// NewSmartRuleService returns a SmartRuleService backed by store.
func NewSmartRuleService(store *Store) *SmartRuleService {
	return &SmartRuleService{store: store}
}

// List returns all smart rules sorted by name.
func (rs *SmartRuleService) List() []*SmartRule {
	out := make([]*SmartRule, 0)
	rs.store.View(func(d *Data) {
		out = append(out, d.SmartRules...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the smart rule with the given id.
func (rs *SmartRuleService) Get(id string) (*SmartRule, error) {
	var found *SmartRule
	rs.store.View(func(d *Data) {
		for _, r := range d.SmartRules {
			if r.ID == id {
				found = r
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("smart rule %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new smart rule from spec. The name must be non-empty
// (ErrInvalid) and unique among rules (ErrExists); every time slot must
// satisfy validateTimeSlot (ErrInvalid); MaxDurationSec, RepeatLookback and
// MaxItems must not be negative (ErrInvalid). The spec's slices are copied,
// so later mutation of the spec never leaks into the store.
func (rs *SmartRuleService) Create(spec SmartRuleSpec) (*SmartRule, error) {
	if err := validateSmartRuleSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	r := &SmartRule{
		ID:             newID(),
		Name:           spec.Name,
		Description:    spec.Description,
		TimeSlots:      append([]TimeSlot(nil), spec.TimeSlots...),
		Tags:           append([]string(nil), spec.Tags...),
		MaxDurationSec: spec.MaxDurationSec,
		AvoidRepeat:    spec.AvoidRepeat,
		RepeatLookback: spec.RepeatLookback,
		MaxItems:       spec.MaxItems,
		Enabled:        spec.Enabled,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	err := rs.store.Update(func(d *Data) error {
		for _, exist := range d.SmartRules {
			if exist.Name == r.Name {
				return fmt.Errorf("smart rule %q: %w", r.Name, ErrExists)
			}
		}
		d.SmartRules = append(d.SmartRules, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Update replaces the configuration of the rule with the given id from
// spec: name, description, time slots, tags, duration cap, repeat avoidance
// and the enabled flag are all replaced (empty slices clear the respective
// constraints, zero caps fall back to their defaults at generation time).
// The new name must be non-empty (ErrInvalid) and must not collide with
// another rule (ErrExists); renaming to its own current name is allowed. It
// returns the updated rule.
func (rs *SmartRuleService) Update(id string, spec SmartRuleSpec) (*SmartRule, error) {
	if err := validateSmartRuleSpec(spec); err != nil {
		return nil, err
	}
	var out *SmartRule
	err := rs.store.Update(func(d *Data) error {
		var r *SmartRule
		for _, cand := range d.SmartRules {
			if cand.ID == id {
				r = cand
				break
			}
		}
		if r == nil {
			return fmt.Errorf("smart rule %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.SmartRules {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("smart rule %q: %w", spec.Name, ErrExists)
			}
		}
		r.Name = spec.Name
		r.Description = spec.Description
		r.TimeSlots = append([]TimeSlot(nil), spec.TimeSlots...)
		r.Tags = append([]string(nil), spec.Tags...)
		r.MaxDurationSec = spec.MaxDurationSec
		r.AvoidRepeat = spec.AvoidRepeat
		r.RepeatLookback = spec.RepeatLookback
		r.MaxItems = spec.MaxItems
		r.Enabled = spec.Enabled
		r.UpdatedAt = time.Now()
		out = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the rule with the given id.
func (rs *SmartRuleService) SetEnabled(id string, enabled bool) error {
	return rs.update(id, func(r *SmartRule) error {
		r.Enabled = enabled
		return nil
	})
}

// Delete removes the smart rule with the given id.
func (rs *SmartRuleService) Delete(id string) error {
	return rs.store.Update(func(d *Data) error {
		idx := -1
		for i, r := range d.SmartRules {
			if r.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("smart rule %q: %w", id, ErrNotFound)
		}
		d.SmartRules = append(d.SmartRules[:idx], d.SmartRules[idx+1:]...)
		return nil
	})
}

// update applies fn to the rule with the given id under the store write
// lock; fn may mutate the rule in place. Returning an error rolls back.
func (rs *SmartRuleService) update(id string, fn func(r *SmartRule) error) error {
	return rs.store.Update(func(d *Data) error {
		for _, r := range d.SmartRules {
			if r.ID != id {
				continue
			}
			if err := fn(r); err != nil {
				return err
			}
			r.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("smart rule %q: %w", id, ErrNotFound)
	})
}

// validateSmartRuleSpec performs field-level validation independent of the
// store: the name must be non-empty after trimming, every time slot must
// satisfy validateTimeSlot, and MaxDurationSec, RepeatLookback and MaxItems
// must not be negative (zero is allowed and means "no cap" / "default",
// resolved at generation time).
func validateSmartRuleSpec(spec SmartRuleSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("smart rule: %w: empty name", ErrInvalid)
	}
	for _, slot := range spec.TimeSlots {
		if err := validateTimeSlot(slot); err != nil {
			return fmt.Errorf("smart rule %q: %w", spec.Name, err)
		}
	}
	if spec.MaxDurationSec < 0 {
		return fmt.Errorf("smart rule %q: %w: max duration must not be negative", spec.Name, ErrInvalid)
	}
	if spec.RepeatLookback < 0 {
		return fmt.Errorf("smart rule %q: %w: repeat lookback must not be negative", spec.Name, ErrInvalid)
	}
	if spec.MaxItems < 0 {
		return fmt.Errorf("smart rule %q: %w: max items must not be negative", spec.Name, ErrInvalid)
	}
	return nil
}

// validateTimeSlot reports whether the slot is a valid daily window: both
// hours within [0, 23] and StartHour <= EndHour. Slots crossing midnight
// are rejected; express them as two slots instead.
func validateTimeSlot(slot TimeSlot) error {
	if slot.StartHour < 0 || slot.StartHour > 23 || slot.EndHour < 0 || slot.EndHour > 23 {
		return fmt.Errorf("smart rule: %w: time slot hours must be within 0-23 (got %d-%d)", ErrInvalid, slot.StartHour, slot.EndHour)
	}
	if slot.StartHour > slot.EndHour {
		return fmt.Errorf("smart rule: %w: time slot start %d must not exceed end %d", ErrInvalid, slot.StartHour, slot.EndHour)
	}
	return nil
}
