package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SceneKind distinguishes the visual scene kinds a template can describe.
type SceneKind string

const (
	// SceneLogo is a persistent logo overlay.
	SceneLogo SceneKind = "logo"
	// SceneClock is a clock overlay.
	SceneClock SceneKind = "clock"
	// SceneTitle is a title text overlay.
	SceneTitle SceneKind = "title"
	// SceneScroll is a scrolling text overlay.
	SceneScroll SceneKind = "scroll"
	// SceneWatermark is a watermark overlay.
	SceneWatermark SceneKind = "watermark"
	// SceneProgress is a progress indicator.
	SceneProgress SceneKind = "progress"
	// SceneIntro is an intro clip scene.
	SceneIntro SceneKind = "intro"
	// SceneOutro is an outro clip scene.
	SceneOutro SceneKind = "outro"
	// SceneBackground is a background scene.
	SceneBackground SceneKind = "background"
)

// SceneTemplate is a reusable scene configuration: a named, typed parameter
// set (for example text, image paths, position, font size as key/value
// pairs) that the renderer instantiates into a real scene. Params keys and
// values are free-form strings interpreted by the scene kind; the management
// side only stores them.
type SceneTemplate struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Kind      SceneKind         `json:"kind"`
	Params    map[string]string `json:"params,omitempty"`
	Enabled   bool              `json:"enabled"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// SceneTemplateSpec is the validated input used to create or replace a scene
// template.
type SceneTemplateSpec struct {
	Name    string
	Kind    SceneKind
	Params  map[string]string
	Enabled bool
}

// SceneTemplateService provides CRUD over the scene templates of a Store
// plus template duplication.
type SceneTemplateService struct {
	store *Store
}

// NewSceneTemplateService returns a SceneTemplateService backed by store.
func NewSceneTemplateService(store *Store) *SceneTemplateService {
	return &SceneTemplateService{store: store}
}

// List returns all scene templates sorted by name.
func (ts *SceneTemplateService) List() []*SceneTemplate {
	out := make([]*SceneTemplate, 0)
	ts.store.View(func(d *Data) {
		out = append(out, d.SceneTemplates...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the scene template with the given id.
func (ts *SceneTemplateService) Get(id string) (*SceneTemplate, error) {
	var found *SceneTemplate
	ts.store.View(func(d *Data) {
		for _, t := range d.SceneTemplates {
			if t.ID == id {
				found = t
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("scene template %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new scene template from spec. The name must be non-empty
// (ErrInvalid) and unique among templates (ErrExists); the kind must be one
// of the known kinds (ErrInvalid).
func (ts *SceneTemplateService) Create(spec SceneTemplateSpec) (*SceneTemplate, error) {
	if err := validateSceneTemplateSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	t := &SceneTemplate{
		ID:        newID(),
		Name:      spec.Name,
		Kind:      spec.Kind,
		Params:    spec.Params,
		Enabled:   spec.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := ts.store.Update(func(d *Data) error {
		for _, exist := range d.SceneTemplates {
			if exist.Name == t.Name {
				return fmt.Errorf("scene template %q: %w", t.Name, ErrExists)
			}
		}
		d.SceneTemplates = append(d.SceneTemplates, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Update replaces the configuration of the template with the given id from
// spec: name, kind, params and the enabled flag are all replaced. The new
// name must be non-empty (ErrInvalid) and must not collide with another
// template (ErrExists); renaming to its own current name is allowed. It
// returns the updated template.
func (ts *SceneTemplateService) Update(id string, spec SceneTemplateSpec) (*SceneTemplate, error) {
	if err := validateSceneTemplateSpec(spec); err != nil {
		return nil, err
	}
	var out *SceneTemplate
	err := ts.store.Update(func(d *Data) error {
		var t *SceneTemplate
		for _, cand := range d.SceneTemplates {
			if cand.ID == id {
				t = cand
				break
			}
		}
		if t == nil {
			return fmt.Errorf("scene template %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.SceneTemplates {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("scene template %q: %w", spec.Name, ErrExists)
			}
		}
		t.Name = spec.Name
		t.Kind = spec.Kind
		t.Params = spec.Params
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

// SetEnabled toggles the enabled flag of the template.
func (ts *SceneTemplateService) SetEnabled(id string, enabled bool) error {
	return ts.update(id, func(t *SceneTemplate) error {
		t.Enabled = enabled
		return nil
	})
}

// Duplicate creates a copy of the template with the given id: the copy gets
// a fresh id and the source name suffixed with " (copy)"; every other field
// (kind, params, enabled) is carried over, with the params map deep-copied
// so the copy is fully independent of the source. The suffixed name must not
// collide with an existing template (ErrExists). It returns the new
// template.
func (ts *SceneTemplateService) Duplicate(id string) (*SceneTemplate, error) {
	now := time.Now()
	var out *SceneTemplate
	err := ts.store.Update(func(d *Data) error {
		var src *SceneTemplate
		for _, cand := range d.SceneTemplates {
			if cand.ID == id {
				src = cand
				break
			}
		}
		if src == nil {
			return fmt.Errorf("scene template %q: %w", id, ErrNotFound)
		}
		name := src.Name + " (copy)"
		for _, exist := range d.SceneTemplates {
			if exist.Name == name {
				return fmt.Errorf("scene template %q: %w", name, ErrExists)
			}
		}
		params := make(map[string]string, len(src.Params))
		for k, v := range src.Params {
			params[k] = v
		}
		t := &SceneTemplate{
			ID:        newID(),
			Name:      name,
			Kind:      src.Kind,
			Params:    params,
			Enabled:   src.Enabled,
			CreatedAt: now,
			UpdatedAt: now,
		}
		d.SceneTemplates = append(d.SceneTemplates, t)
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes the scene template with the given id. Tasks may reference a
// template, but deletion is not guarded: the reference is re-validated the
// next time the referencing task is created or replaced. Play requests carry
// the id as advisory metadata and are unaffected.
func (ts *SceneTemplateService) Delete(id string) error {
	return ts.store.Update(func(d *Data) error {
		idx := -1
		for i, t := range d.SceneTemplates {
			if t.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("scene template %q: %w", id, ErrNotFound)
		}
		d.SceneTemplates = append(d.SceneTemplates[:idx], d.SceneTemplates[idx+1:]...)
		return nil
	})
}

// update applies fn to the template with the given id under the store write
// lock; fn may mutate the template in place. Returning an error rolls back.
func (ts *SceneTemplateService) update(id string, fn func(t *SceneTemplate) error) error {
	return ts.store.Update(func(d *Data) error {
		for _, t := range d.SceneTemplates {
			if t.ID != id {
				continue
			}
			if err := fn(t); err != nil {
				return err
			}
			t.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("scene template %q: %w", id, ErrNotFound)
	})
}

// validateSceneTemplateSpec performs field-level validation independent of
// the store: the name must be non-empty after trimming and the kind must be
// one of the known kinds.
func validateSceneTemplateSpec(spec SceneTemplateSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("scene template: %w: empty name", ErrInvalid)
	}
	return validateSceneKind(spec.Kind)
}

// validateSceneKind reports whether the kind is one of the known scene
// kinds.
func validateSceneKind(k SceneKind) error {
	switch k {
	case SceneLogo, SceneClock, SceneTitle, SceneScroll, SceneWatermark,
		SceneProgress, SceneIntro, SceneOutro, SceneBackground:
		return nil
	}
	return fmt.Errorf("scene template: %w: unknown kind %q", ErrInvalid, k)
}

// ResolveSceneTemplate returns the scene template for id, or an error
// wrapping ErrNotFound.
func ResolveSceneTemplate(d *Data, id string) (*SceneTemplate, error) {
	for _, t := range d.SceneTemplates {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("scene template %q: %w", id, ErrNotFound)
}

// validateSceneTemplateRef reports whether a scene template with the given id
// exists in d. An empty id is allowed: it means no template is applied
// (clearing).
func validateSceneTemplateRef(d *Data, id string) error {
	if id == "" {
		return nil
	}
	if _, err := ResolveSceneTemplate(d, id); err != nil {
		return err
	}
	return nil
}
