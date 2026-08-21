package management

import (
	"fmt"
	"strings"
	"time"
)

// AlarmLevel classifies the severity of an alarm.
type AlarmLevel string

const (
	// AlarmLevelInfo is a purely informational alarm.
	AlarmLevelInfo AlarmLevel = "info"
	// AlarmLevelWarning flags something worth attention.
	AlarmLevelWarning AlarmLevel = "warning"
	// AlarmLevelCritical flags something that needs immediate action.
	AlarmLevelCritical AlarmLevel = "critical"
)

// AlarmStatus is the lifecycle state of an alarm.
type AlarmStatus string

const (
	// AlarmStatusActive marks an open alarm.
	AlarmStatusActive AlarmStatus = "active"
	// AlarmStatusResolved marks a closed alarm.
	AlarmStatusResolved AlarmStatus = "resolved"
)

// Alarm is a single condition raised by the console or by a service.
type Alarm struct {
	ID         string      `json:"id"`
	Level      AlarmLevel  `json:"level"`
	Title      string      `json:"title"`
	Message    string      `json:"message,omitempty"`
	Status     AlarmStatus `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	UpdatedAt  time.Time   `json:"updatedAt"`
	ResolvedAt *time.Time  `json:"resolvedAt,omitempty"`
}

// IsActive reports whether the alarm is still open.
func (a *Alarm) IsActive() bool { return a.Status == AlarmStatusActive }

// AlarmService provides CRUD over the alarms of a Store. Identical active
// alarms are deduplicated: raising the same (level, title, message) twice
// returns the existing open alarm instead of creating a duplicate.
type AlarmService struct {
	store *Store
}

// NewAlarmService returns an AlarmService backed by store.
func NewAlarmService(store *Store) *AlarmService {
	return &AlarmService{store: store}
}

// List returns all alarms, newest first.
func (as *AlarmService) List() []*Alarm {
	out := make([]*Alarm, 0)
	as.store.View(func(d *Data) {
		out = append(out, d.Alarms...)
	})
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ListActive returns only active alarms, newest first.
func (as *AlarmService) ListActive() []*Alarm {
	all := as.List()
	out := make([]*Alarm, 0, len(all))
	for _, a := range all {
		if a.IsActive() {
			out = append(out, a)
		}
	}
	return out
}

// Get returns the alarm with the given id.
func (as *AlarmService) Get(id string) (*Alarm, error) {
	var found *Alarm
	as.store.View(func(d *Data) {
		for _, a := range d.Alarms {
			if a.ID == id {
				found = a
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("alarm %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Raise opens an alarm. If an identical (same level, title, message) alarm is
// already active it is returned unchanged and no duplicate is created; the
// store is not rewritten and UpdatedAt is not bumped (a no-op update). An
// empty level defaults to AlarmLevelInfo.
func (as *AlarmService) Raise(level AlarmLevel, title, message string) (*Alarm, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("alarm: %w: empty title", ErrInvalid)
	}
	if level == "" {
		level = AlarmLevelInfo
	}
	if err := validateAlarmLevel(level); err != nil {
		return nil, err
	}
	now := time.Now()
	var created *Alarm
	err := as.store.Update(func(d *Data) error {
		for _, a := range d.Alarms {
			if a.Status == AlarmStatusActive && a.Level == level && a.Title == title && a.Message == message {
				created = a
				return errNoop
			}
		}
		a := &Alarm{
			ID:        newID(),
			Level:     level,
			Title:     title,
			Message:   message,
			Status:    AlarmStatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
		d.Alarms = append(d.Alarms, a)
		created = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Resolve marks the alarm with the given id as resolved, setting ResolvedAt.
// Resolving an already resolved alarm is a no-op: it returns the alarm
// unchanged without rewriting the store or bumping root UpdatedAt.
func (as *AlarmService) Resolve(id string) (*Alarm, error) {
	now := time.Now()
	var out *Alarm
	err := as.store.Update(func(d *Data) error {
		for _, a := range d.Alarms {
			if a.ID != id {
				continue
			}
			if a.Status == AlarmStatusActive {
				rt := now
				a.ResolvedAt = &rt
				a.Status = AlarmStatusResolved
				a.UpdatedAt = now
				out = a
				return nil
			}
			// already resolved: no net change, so signal a no-op update.
			out = a
			return errNoop
		}
		return fmt.Errorf("alarm %q: %w", id, ErrNotFound)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveAll marks every active alarm as resolved and returns the number
// affected. When there are no active alarms it is a no-op: the store is not
// rewritten and root UpdatedAt is not bumped.
func (as *AlarmService) ResolveAll() (int, error) {
	now := time.Now()
	count := 0
	err := as.store.Update(func(d *Data) error {
		for _, a := range d.Alarms {
			if a.Status == AlarmStatusActive {
				rt := now
				a.ResolvedAt = &rt
				a.Status = AlarmStatusResolved
				a.UpdatedAt = now
				count++
			}
		}
		if count == 0 {
			return errNoop
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// Delete removes the alarm with the given id.
func (as *AlarmService) Delete(id string) error {
	return as.store.Update(func(d *Data) error {
		idx := -1
		for i, a := range d.Alarms {
			if a.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("alarm %q: %w", id, ErrNotFound)
		}
		d.Alarms = append(d.Alarms[:idx], d.Alarms[idx+1:]...)
		return nil
	})
}

// Prune removes resolved alarms whose ResolvedAt is older than olderThan
// (relative to now) and returns the number removed. Active alarms are never
// removed. Passing a zero duration keeps every resolved alarm. When nothing
// is removed it is a no-op: the store is not rewritten and root UpdatedAt is
// not bumped.
func (as *AlarmService) Prune(olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	err := as.store.Update(func(d *Data) error {
		kept := d.Alarms[:0]
		for _, a := range d.Alarms {
			if a.Status == AlarmStatusResolved && a.ResolvedAt != nil && a.ResolvedAt.Before(cutoff) {
				removed++
				continue
			}
			kept = append(kept, a)
		}
		if removed == 0 {
			return errNoop
		}
		d.Alarms = kept
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func validateAlarmLevel(l AlarmLevel) error {
	switch l {
	case AlarmLevelInfo, AlarmLevelWarning, AlarmLevelCritical:
		return nil
	}
	return fmt.Errorf("alarm: %w: unknown level %q", ErrInvalid, l)
}
