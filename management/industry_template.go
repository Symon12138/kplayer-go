package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// IndustryTaskSpec is the optional scheduled task definition carried by an
// industry template: the subset of a schedule task (name, type and the
// type-specific interval or cron fields, plus the enabled flag) that Deploy
// turns into a real ScheduleTask when it provisions the template. It reuses
// the TaskType constants of the scheduler.
type IndustryTaskSpec struct {
	Name     string   `json:"name"`
	Type     TaskType `json:"type"`
	Interval int      `json:"interval,omitempty"` // seconds; interval type only
	Cron     string   `json:"cron,omitempty"`     // five-field cron; cron type only
	Enabled  bool     `json:"enabled"`
}

// IndustryTemplate is a deployable bundle for one industry: a whole program
// schedule, scene templates and an optional scheduled task combined into a
// single configuration that Deploy provisions in one step. PlaylistName is
// the name of the playlist created at deploy time; MediaPlaceholders are
// the playlist entries, which may reference "${key}" placeholders that
// Deploy expands to concrete media ids; SceneKinds lists the scene template
// kinds created at deploy time; Task optionally defines the scheduled task
// to deploy. The management side only stores the configuration: expansion
// and provisioning are Deploy's concern.
type IndustryTemplate struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	PlaylistName      string            `json:"playlistName"`
	MediaPlaceholders []string          `json:"mediaPlaceholders,omitempty"`
	SceneKinds        []SceneKind       `json:"sceneKinds,omitempty"`
	Task              *IndustryTaskSpec `json:"task,omitempty"`
	Enabled           bool              `json:"enabled"`
	CreatedAt         time.Time         `json:"createdAt"`
	UpdatedAt         time.Time         `json:"updatedAt"`
}

// IndustryTemplateSpec is the validated input used to create or replace an
// industry template.
type IndustryTemplateSpec struct {
	Name              string
	Description       string
	PlaylistName      string
	MediaPlaceholders []string
	SceneKinds        []SceneKind
	Task              *IndustryTaskSpec
	Enabled           bool
}

// IndustryTemplateService provides CRUD over the industry templates of a
// Store.
type IndustryTemplateService struct {
	store *Store
}

// NewIndustryTemplateService returns an IndustryTemplateService backed by
// store.
func NewIndustryTemplateService(store *Store) *IndustryTemplateService {
	return &IndustryTemplateService{store: store}
}

// List returns all industry templates sorted by name.
func (is *IndustryTemplateService) List() []*IndustryTemplate {
	out := make([]*IndustryTemplate, 0)
	is.store.View(func(d *Data) {
		out = append(out, d.IndustryTemplates...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the industry template with the given id.
func (is *IndustryTemplateService) Get(id string) (*IndustryTemplate, error) {
	var found *IndustryTemplate
	is.store.View(func(d *Data) {
		for _, t := range d.IndustryTemplates {
			if t.ID == id {
				found = t
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("industry template %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new industry template from spec. The name must be non-empty
// (ErrInvalid) and unique among templates (ErrExists); the playlist name
// must be non-empty (ErrInvalid); every scene kind must be a known kind
// (ErrInvalid); a task, when present, must satisfy
// validateIndustryTaskSpec (ErrInvalid). The spec's slices and task are
// copied, so later mutation of the spec never leaks into the store.
func (is *IndustryTemplateService) Create(spec IndustryTemplateSpec) (*IndustryTemplate, error) {
	if err := validateIndustryTemplateSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	t := &IndustryTemplate{
		ID:                newID(),
		Name:              spec.Name,
		Description:       spec.Description,
		PlaylistName:      spec.PlaylistName,
		MediaPlaceholders: append([]string(nil), spec.MediaPlaceholders...),
		SceneKinds:        append([]SceneKind(nil), spec.SceneKinds...),
		Task:              cloneIndustryTaskSpec(spec.Task),
		Enabled:           spec.Enabled,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	err := is.store.Update(func(d *Data) error {
		for _, exist := range d.IndustryTemplates {
			if exist.Name == t.Name {
				return fmt.Errorf("industry template %q: %w", t.Name, ErrExists)
			}
		}
		d.IndustryTemplates = append(d.IndustryTemplates, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Update replaces the configuration of the template with the given id from
// spec: name, description, playlist name, media placeholders, scene kinds,
// task and the enabled flag are all replaced (a nil task clears the task
// definition). The new name must be non-empty (ErrInvalid) and must not
// collide with another template (ErrExists); renaming to its own current
// name is allowed. It returns the updated template.
func (is *IndustryTemplateService) Update(id string, spec IndustryTemplateSpec) (*IndustryTemplate, error) {
	if err := validateIndustryTemplateSpec(spec); err != nil {
		return nil, err
	}
	var out *IndustryTemplate
	err := is.store.Update(func(d *Data) error {
		var t *IndustryTemplate
		for _, cand := range d.IndustryTemplates {
			if cand.ID == id {
				t = cand
				break
			}
		}
		if t == nil {
			return fmt.Errorf("industry template %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.IndustryTemplates {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("industry template %q: %w", spec.Name, ErrExists)
			}
		}
		t.Name = spec.Name
		t.Description = spec.Description
		t.PlaylistName = spec.PlaylistName
		t.MediaPlaceholders = append([]string(nil), spec.MediaPlaceholders...)
		t.SceneKinds = append([]SceneKind(nil), spec.SceneKinds...)
		t.Task = cloneIndustryTaskSpec(spec.Task)
		t.Enabled = spec.Enabled
		t.UpdatedAt = time.Now()
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the template with the given id.
func (is *IndustryTemplateService) SetEnabled(id string, enabled bool) error {
	return is.update(id, func(t *IndustryTemplate) error {
		t.Enabled = enabled
		return nil
	})
}

// Delete removes the industry template with the given id.
func (is *IndustryTemplateService) Delete(id string) error {
	return is.store.Update(func(d *Data) error {
		idx := -1
		for i, t := range d.IndustryTemplates {
			if t.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("industry template %q: %w", id, ErrNotFound)
		}
		d.IndustryTemplates = append(d.IndustryTemplates[:idx], d.IndustryTemplates[idx+1:]...)
		return nil
	})
}

// update applies fn to the template with the given id under the store write
// lock; fn may mutate the template in place. Returning an error rolls back.
func (is *IndustryTemplateService) update(id string, fn func(t *IndustryTemplate) error) error {
	return is.store.Update(func(d *Data) error {
		for _, t := range d.IndustryTemplates {
			if t.ID != id {
				continue
			}
			if err := fn(t); err != nil {
				return err
			}
			t.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("industry template %q: %w", id, ErrNotFound)
	})
}

// validateIndustryTemplateSpec performs field-level validation independent
// of the store: the name must be non-empty after trimming, the playlist
// name must be non-empty after trimming, every scene kind must be a known
// kind, and a task, when present, must pass validateIndustryTaskSpec.
func validateIndustryTemplateSpec(spec IndustryTemplateSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("industry template: %w: empty name", ErrInvalid)
	}
	if strings.TrimSpace(spec.PlaylistName) == "" {
		return fmt.Errorf("industry template %q: %w: empty playlist name", spec.Name, ErrInvalid)
	}
	for _, k := range spec.SceneKinds {
		if err := validateSceneKind(k); err != nil {
			return fmt.Errorf("industry template %q: %w", spec.Name, err)
		}
	}
	if spec.Task != nil {
		if err := validateIndustryTaskSpec(*spec.Task); err != nil {
			return err
		}
	}
	return nil
}

// validateIndustryTaskSpec performs field-level validation of an industry
// template's task definition, mirroring the checks of validateTaskSpec that
// apply to the subset of fields an industry task carries: a non-empty name,
// a known type, and a positive interval (interval type) or a parseable
// five-field cron expression (cron type).
func validateIndustryTaskSpec(spec IndustryTaskSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("industry task: %w: empty name", ErrInvalid)
	}
	switch spec.Type {
	case TaskTypeInterval:
		if spec.Interval <= 0 {
			return fmt.Errorf("industry task %q: %w: interval must be positive", spec.Name, ErrInvalid)
		}
	case TaskTypeCron:
		if _, err := ParseCron(spec.Cron); err != nil {
			return err
		}
	default:
		return fmt.Errorf("industry task %q: %w: unknown type %q", spec.Name, ErrInvalid, spec.Type)
	}
	return nil
}

// cloneIndustryTaskSpec returns a copy of t, so the stored template never
// aliases a caller's spec. A nil task stays nil.
func cloneIndustryTaskSpec(t *IndustryTaskSpec) *IndustryTaskSpec {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
