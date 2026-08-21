package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// CacheStatus is the lifecycle state of a cache task.
type CacheStatus string

const (
	// CachePending marks a task that has been created but not started.
	CachePending CacheStatus = "pending"
	// CacheRunning marks a task whose cache job is in progress.
	CacheRunning CacheStatus = "running"
	// CacheDone marks a task whose cache job completed successfully.
	CacheDone CacheStatus = "done"
	// CacheFailed marks a task whose cache job failed.
	CacheFailed CacheStatus = "failed"
)

// IsTerminal reports whether the status is a terminal one (done or failed).
func (s CacheStatus) IsTerminal() bool { return s == CacheDone || s == CacheFailed }

// CacheTask is one entry of the cache center: a request to pre-cache a media
// item so the player can start it without buffering. It references a media
// item of the library and tracks the job lifecycle.
type CacheTask struct {
	ID      string      `json:"id"`
	MediaID string      `json:"mediaId"`
	Status  CacheStatus `json:"status"`
	Note    string      `json:"note,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// CompletedAt records when the task reached a terminal state (done or
	// failed); it is nil while the task is pending or running.
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// CacheTaskSpec is the validated input used to create or replace a cache
// task. Status is optional; an empty status defaults to CachePending.
type CacheTaskSpec struct {
	MediaID string
	Note    string
	Status  CacheStatus
}

// CacheTaskService provides CRUD and status transitions over the cache tasks
// of a Store.
type CacheTaskService struct {
	store *Store
}

// NewCacheTaskService returns a CacheTaskService backed by store.
func NewCacheTaskService(store *Store) *CacheTaskService {
	return &CacheTaskService{store: store}
}

// List returns all cache tasks, newest first (sorted by CreatedAt
// descending; id breaks ties for a deterministic order).
func (cs *CacheTaskService) List() []*CacheTask {
	out := make([]*CacheTask, 0)
	cs.store.View(func(d *Data) {
		out = append(out, d.CacheTasks...)
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Get returns the cache task with the given id.
func (cs *CacheTaskService) Get(id string) (*CacheTask, error) {
	var found *CacheTask
	cs.store.View(func(d *Data) {
		for _, t := range d.CacheTasks {
			if t.ID == id {
				found = t
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("cache task %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new cache task from spec. The media reference must be
// non-empty (ErrInvalid) and must exist in the media library (an error
// wrapping ErrNotFound is returned otherwise). An empty status defaults to
// CachePending; any other value must be a known status (ErrInvalid).
func (cs *CacheTaskService) Create(spec CacheTaskSpec) (*CacheTask, error) {
	spec.MediaID = strings.TrimSpace(spec.MediaID)
	if spec.Status == "" {
		spec.Status = CachePending
	}
	if err := validateCacheTaskSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	t := &CacheTask{
		ID:        newID(),
		MediaID:   spec.MediaID,
		Status:    spec.Status,
		Note:      spec.Note,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := cs.store.Update(func(d *Data) error {
		if _, err := ResolveMedia(d, t.MediaID); err != nil {
			return fmt.Errorf("cache task: %w", err)
		}
		syncCacheCompletedAt(t)
		d.CacheTasks = append(d.CacheTasks, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Update replaces the configuration of the task with the given id from spec:
// the media reference, note and status are all replaced. The media reference
// must be non-empty (ErrInvalid) and exist (ErrNotFound wrapped); an empty
// status defaults to CachePending and any other value must be a known status
// (ErrInvalid). To keep CompletedAt consistent with the status, a terminal
// status (done/failed) without a completion time records one and a
// pending/running status clears any previous completion. It returns the
// updated task.
func (cs *CacheTaskService) Update(id string, spec CacheTaskSpec) (*CacheTask, error) {
	spec.MediaID = strings.TrimSpace(spec.MediaID)
	if spec.Status == "" {
		spec.Status = CachePending
	}
	if err := validateCacheTaskSpec(spec); err != nil {
		return nil, err
	}
	var out *CacheTask
	err := cs.store.Update(func(d *Data) error {
		var t *CacheTask
		for _, cand := range d.CacheTasks {
			if cand.ID == id {
				t = cand
				break
			}
		}
		if t == nil {
			return fmt.Errorf("cache task %q: %w", id, ErrNotFound)
		}
		if _, err := ResolveMedia(d, spec.MediaID); err != nil {
			return fmt.Errorf("cache task %q: %w", id, err)
		}
		t.MediaID = spec.MediaID
		t.Note = spec.Note
		t.Status = spec.Status
		syncCacheCompletedAt(t)
		t.UpdatedAt = time.Now()
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes the cache task with the given id.
func (cs *CacheTaskService) Delete(id string) error {
	return cs.store.Update(func(d *Data) error {
		idx := -1
		for i, t := range d.CacheTasks {
			if t.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("cache task %q: %w", id, ErrNotFound)
		}
		d.CacheTasks = append(d.CacheTasks[:idx], d.CacheTasks[idx+1:]...)
		return nil
	})
}

// MarkRunning moves the task with the given id to running and clears any
// previous completion time: a task that starts running again is no longer
// completed. It returns the updated task.
func (cs *CacheTaskService) MarkRunning(id string) (*CacheTask, error) {
	return cs.transition(id, func(t *CacheTask) {
		t.Status = CacheRunning
		t.CompletedAt = nil
	})
}

// MarkDone marks the task with the given id as completed successfully,
// recording CompletedAt. It returns the updated task.
func (cs *CacheTaskService) MarkDone(id string) (*CacheTask, error) {
	return cs.transition(id, func(t *CacheTask) {
		t.Status = CacheDone
		now := time.Now()
		t.CompletedAt = &now
	})
}

// MarkFailed marks the task with the given id as failed, recording
// CompletedAt and, when note is non-empty, replacing the task note with the
// failure reason (an empty note leaves the existing note untouched). It
// returns the updated task.
func (cs *CacheTaskService) MarkFailed(id, note string) (*CacheTask, error) {
	return cs.transition(id, func(t *CacheTask) {
		t.Status = CacheFailed
		now := time.Now()
		t.CompletedAt = &now
		if note != "" {
			t.Note = note
		}
	})
}

// transition applies fn to the task with the given id and bumps UpdatedAt.
// Transitions are unconditional: re-marking a task that already holds the
// target status is allowed and still refreshes the timestamps, so a caller
// re-running a completed job can move it back to running and complete it
// again.
func (cs *CacheTaskService) transition(id string, fn func(t *CacheTask)) (*CacheTask, error) {
	var out *CacheTask
	err := cs.store.Update(func(d *Data) error {
		for _, t := range d.CacheTasks {
			if t.ID != id {
				continue
			}
			fn(t)
			t.UpdatedAt = time.Now()
			out = t
			return nil
		}
		return fmt.Errorf("cache task %q: %w", id, ErrNotFound)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountByStatus returns the number of cache tasks per status. Every known
// status appears in the map with its count (zero when no task has it);
// unrecognized statuses, possible only in a hand-edited store, are ignored.
func (cs *CacheTaskService) CountByStatus() map[CacheStatus]int {
	counts := map[CacheStatus]int{
		CachePending: 0,
		CacheRunning: 0,
		CacheDone:    0,
		CacheFailed:  0,
	}
	cs.store.View(func(d *Data) {
		for _, t := range d.CacheTasks {
			switch t.Status {
			case CachePending, CacheRunning, CacheDone, CacheFailed:
				counts[t.Status]++
			}
		}
	})
	return counts
}

// validateCacheTaskSpec performs field-level validation independent of the
// store: the media reference must be non-empty and the status must be one of
// the known statuses.
func validateCacheTaskSpec(spec CacheTaskSpec) error {
	if strings.TrimSpace(spec.MediaID) == "" {
		return fmt.Errorf("cache task: %w: empty media id", ErrInvalid)
	}
	return validateCacheStatus(spec.Status)
}

func validateCacheStatus(s CacheStatus) error {
	switch s {
	case CachePending, CacheRunning, CacheDone, CacheFailed:
		return nil
	}
	return fmt.Errorf("cache task: %w: unknown status %q", ErrInvalid, s)
}

// syncCacheCompletedAt keeps CompletedAt consistent with the status: a
// terminal status (done/failed) without a completion time records the
// current time, a pending/running status clears any previous completion.
func syncCacheCompletedAt(t *CacheTask) {
	switch t.Status {
	case CacheDone, CacheFailed:
		if t.CompletedAt == nil {
			now := time.Now()
			t.CompletedAt = &now
		}
	default:
		t.CompletedAt = nil
	}
}
