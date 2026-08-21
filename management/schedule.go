package management

import (
	"fmt"
	"strings"
	"time"
)

// TaskType distinguishes the two scheduling kinds supported by the Scheduler.
type TaskType string

const (
	// TaskTypeInterval fires the task on a fixed second interval.
	TaskTypeInterval TaskType = "interval"
	// TaskTypeCron fires the task according to a five-field cron expression.
	TaskTypeCron TaskType = "cron"
)

// TaskAction is the action a scheduled task performs when it fires.
type TaskAction string

const (
	// TaskActionPlay plays the task's playlist (or media) immediately: the
	// classic "定时开播 / 定时更新节目单并播放最新内容" behaviour.
	TaskActionPlay TaskAction = "play"
	// TaskActionStop stops the current push: "定时关播".
	TaskActionStop TaskAction = "stop"
)

// ScheduleTask is a persisted scheduling configuration. It references either
// a playlist or a single media to play when it fires (TaskActionPlay), or
// stops the current push (TaskActionStop), and carries runtime bookkeeping
// (last run) that the Scheduler updates.
type ScheduleTask struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Type       TaskType `json:"type"`
	Interval   int      `json:"interval,omitempty"` // seconds; interval type only
	Cron       string   `json:"cron,omitempty"`     // five-field cron; cron type only
	// Action is what the task does when it fires; empty means TaskActionPlay.
	Action     TaskAction `json:"action,omitempty"`
	PlaylistID string   `json:"playlistId,omitempty"`
	MediaID    string   `json:"mediaId,omitempty"`
	// SceneTemplateID references a scene template applied when the task
	// fires; empty means no template is applied.
	SceneTemplateID string `json:"sceneTemplateId,omitempty"`
	Loop            bool   `json:"loop,omitempty"`
	// Priority is the scheduling precedence of fires; the zero value is
	// PriorityNormal.
	Priority Priority `json:"priority,omitempty"`
	// Interrupt marks the task as an interrupt task (插播): when it fires,
	// its target plays immediately, preempting whatever is currently
	// playing, and the pre-interrupt target is remembered for restoration.
	Interrupt bool `json:"interrupt,omitempty"`
	// InterruptDuration is the interrupt length in seconds; only meaningful
	// when Interrupt is set. A positive value auto-restores the
	// pre-interrupt target once the duration elapses. A zero value (the
	// default) makes the interrupt one-shot: it plays until the Player ends
	// it and nothing is restored afterwards. Negative values are rejected by
	// validation.
	InterruptDuration int        `json:"interruptDuration,omitempty"`
	Enabled           bool       `json:"enabled"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	LastRun           *time.Time `json:"lastRun,omitempty"`
}

// TaskSpec is the validated input used to create or replace a task.
type TaskSpec struct {
	Name       string
	Type       TaskType
	Interval   int
	Cron       string
	// Action is the fire action; empty means TaskActionPlay.
	Action     TaskAction
	PlaylistID string
	MediaID    string
	// SceneTemplateID references a scene template applied when the task
	// fires; empty clears the application.
	SceneTemplateID string
	Loop            bool
	// Priority is the scheduling precedence of fires; the zero value is
	// PriorityNormal.
	Priority Priority
	// Interrupt marks the task as an interrupt task; see ScheduleTask.
	Interrupt bool
	// InterruptDuration is the interrupt length in seconds; see
	// ScheduleTask.
	InterruptDuration int
	Enabled           bool
}

// TaskService provides CRUD over the scheduled tasks of a Store.
type TaskService struct {
	store *Store
}

// NewTaskService returns a TaskService backed by store.
func NewTaskService(store *Store) *TaskService {
	return &TaskService{store: store}
}

// List returns all tasks in insertion order.
func (ts *TaskService) List() []*ScheduleTask {
	out := make([]*ScheduleTask, 0)
	ts.store.View(func(d *Data) {
		out = append(out, d.Tasks...)
	})
	return out
}

// Get returns the task with the given id.
func (ts *TaskService) Get(id string) (*ScheduleTask, error) {
	var found *ScheduleTask
	ts.store.View(func(d *Data) {
		for _, t := range d.Tasks {
			if t.ID == id {
				found = t
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("task %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new task from spec and returns it.
func (ts *TaskService) Create(spec TaskSpec) (*ScheduleTask, error) {
	if err := validateTaskSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	t := &ScheduleTask{
		ID:                newID(),
		Name:              spec.Name,
		Type:              spec.Type,
		Interval:          spec.Interval,
		Cron:              spec.Cron,
		Action:            spec.Action,
		PlaylistID:        spec.PlaylistID,
		MediaID:           spec.MediaID,
		SceneTemplateID:   spec.SceneTemplateID,
		Loop:              spec.Loop,
		Priority:          normalizePriority(spec.Priority),
		Interrupt:         spec.Interrupt,
		InterruptDuration: spec.InterruptDuration,
		Enabled:           spec.Enabled,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	err := ts.store.Update(func(d *Data) error {
		if err := validateTaskRefs(d, t); err != nil {
			return err
		}
		d.Tasks = append(d.Tasks, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Update applies fn to the task with the given id under the store write lock;
// fn may mutate the task in place. Returning an error rolls back.
func (ts *TaskService) Update(id string, fn func(t *ScheduleTask) error) error {
	return ts.store.Update(func(d *Data) error {
		for _, t := range d.Tasks {
			if t.ID != id {
				continue
			}
			if err := fn(t); err != nil {
				return err
			}
			t.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("task %q: %w", id, ErrNotFound)
	})
}

// Replace overwrites the scheduling configuration of the task with the given
// id from spec (references and type-specific fields are re-validated). It
// returns the updated task.
func (ts *TaskService) Replace(id string, spec TaskSpec) (*ScheduleTask, error) {
	if err := validateTaskSpec(spec); err != nil {
		return nil, err
	}
	var out *ScheduleTask
	err := ts.store.Update(func(d *Data) error {
		for _, t := range d.Tasks {
			if t.ID != id {
				continue
			}
			t.Name = spec.Name
			t.Type = spec.Type
			t.Interval = spec.Interval
			t.Cron = spec.Cron
			t.PlaylistID = spec.PlaylistID
			t.MediaID = spec.MediaID
			t.SceneTemplateID = spec.SceneTemplateID
			t.Loop = spec.Loop
			t.Priority = normalizePriority(spec.Priority)
			t.Interrupt = spec.Interrupt
			t.InterruptDuration = spec.InterruptDuration
			t.Enabled = spec.Enabled
			if err := validateTaskRefs(d, t); err != nil {
				return err
			}
			t.UpdatedAt = time.Now()
			out = t
			return nil
		}
		return fmt.Errorf("task %q: %w", id, ErrNotFound)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the task.
func (ts *TaskService) SetEnabled(id string, enabled bool) error {
	return ts.Update(id, func(t *ScheduleTask) error {
		t.Enabled = enabled
		return nil
	})
}

// Delete removes the task with the given id.
func (ts *TaskService) Delete(id string) error {
	return ts.store.Update(func(d *Data) error {
		idx := -1
		for i, t := range d.Tasks {
			if t.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("task %q: %w", id, ErrNotFound)
		}
		d.Tasks = append(d.Tasks[:idx], d.Tasks[idx+1:]...)
		return nil
	})
}

// validateTaskSpec performs field-level validation independent of the store.
func validateTaskSpec(spec TaskSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("task: %w: empty name", ErrInvalid)
	}
	switch spec.Type {
	case TaskTypeInterval:
		if spec.Interval <= 0 {
			return fmt.Errorf("task %q: %w: interval must be positive", spec.Name, ErrInvalid)
		}
	case TaskTypeCron:
		if _, err := ParseCron(spec.Cron); err != nil {
			return err
		}
	default:
		return fmt.Errorf("task %q: %w: unknown type %q", spec.Name, ErrInvalid, spec.Type)
	}
	switch spec.Priority {
	case "", PriorityNormal, PriorityImportant, PriorityCritical:
	default:
		return fmt.Errorf("task %q: %w: unknown priority %q", spec.Name, ErrInvalid, spec.Priority)
	}
	if spec.InterruptDuration < 0 {
		return fmt.Errorf("task %q: %w: interrupt duration must not be negative", spec.Name, ErrInvalid)
	}
	switch spec.Action {
	case "", TaskActionPlay, TaskActionStop:
	default:
		return fmt.Errorf("task %q: %w: unknown action %q", spec.Name, ErrInvalid, spec.Action)
	}
	// A play action needs exactly one target; a stop action needs none.
	switch {
	case spec.Action == TaskActionStop:
		if spec.PlaylistID != "" || spec.MediaID != "" {
			return fmt.Errorf("task %q: %w: stop action must not carry a target", spec.Name, ErrInvalid)
		}
	case spec.PlaylistID != "" && spec.MediaID != "":
		return fmt.Errorf("task %q: %w: set either playlist or media, not both", spec.Name, ErrInvalid)
	case spec.PlaylistID == "" && spec.MediaID == "":
		return fmt.Errorf("task %q: %w: a playlist or media target is required", spec.Name, ErrInvalid)
	}
	return nil
}

// validateTaskRefs checks that the task's referenced playlist/media exist.
func validateTaskRefs(d *Data, t *ScheduleTask) error {
	if t.PlaylistID != "" {
		if _, err := ResolvePlaylist(d, t.PlaylistID); err != nil {
			return fmt.Errorf("task %q: %w", t.Name, err)
		}
	}
	if t.MediaID != "" {
		if _, err := ResolveMedia(d, t.MediaID); err != nil {
			return fmt.Errorf("task %q: %w", t.Name, err)
		}
	}
	// An empty scene template reference is allowed: it means no template is
	// applied (clearing).
	if err := validateSceneTemplateRef(d, t.SceneTemplateID); err != nil {
		return fmt.Errorf("task %q: %w", t.Name, err)
	}
	return nil
}

// ResolvePlaylist returns the playlist for id, or an error wrapping
// ErrNotFound.
func ResolvePlaylist(d *Data, id string) (*Playlist, error) {
	for _, p := range d.Playlists {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("playlist %q: %w", id, ErrNotFound)
}
