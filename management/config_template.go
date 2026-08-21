package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ConfigTemplate is a parameterized configuration fragment: a named, typed
// JSON object body that may contain "${key}" placeholders, substituted with
// concrete values by Expand. The type is a free-form string (for example
// "media", "playlist" or "task") interpreted by the consumer; the
// management side only stores it.
type ConfigTemplate struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Body      json.RawMessage `json:"body"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// ConfigTemplateSpec is the validated input used to create or replace a
// config template. The name and type must be non-empty and the body must
// be a valid JSON object; the type is not restricted to a fixed set.
type ConfigTemplateSpec struct {
	Name    string
	Type    string
	Body    json.RawMessage
	Enabled bool
}

// ConfigTemplateService provides CRUD over the config templates of a
// Store.
type ConfigTemplateService struct {
	store *Store
}

// NewConfigTemplateService returns a ConfigTemplateService backed by store.
func NewConfigTemplateService(store *Store) *ConfigTemplateService {
	return &ConfigTemplateService{store: store}
}

// List returns all config templates sorted by name.
func (cs *ConfigTemplateService) List() []*ConfigTemplate {
	out := make([]*ConfigTemplate, 0)
	cs.store.View(func(d *Data) {
		out = append(out, d.ConfigTemplates...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the config template with the given id.
func (cs *ConfigTemplateService) Get(id string) (*ConfigTemplate, error) {
	var found *ConfigTemplate
	cs.store.View(func(d *Data) {
		for _, t := range d.ConfigTemplates {
			if t.ID == id {
				found = t
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("config template %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new config template from spec. The name and type must be
// non-empty (ErrInvalid), the body must be a valid JSON object
// (ErrInvalid), and the name must be unique among templates (ErrExists).
func (cs *ConfigTemplateService) Create(spec ConfigTemplateSpec) (*ConfigTemplate, error) {
	if err := validateConfigTemplateSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	t := &ConfigTemplate{
		ID:        newID(),
		Name:      spec.Name,
		Type:      spec.Type,
		Body:      append(json.RawMessage(nil), spec.Body...),
		Enabled:   spec.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := cs.store.Update(func(d *Data) error {
		for _, exist := range d.ConfigTemplates {
			if exist.Name == t.Name {
				return fmt.Errorf("config template %q: %w", t.Name, ErrExists)
			}
		}
		d.ConfigTemplates = append(d.ConfigTemplates, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Update replaces the configuration of the template with the given id from
// spec: name, type, body and the enabled flag are all replaced. The new
// name must be non-empty (ErrInvalid) and must not collide with another
// template (ErrExists); renaming to its own current name is allowed. It
// returns the updated template.
func (cs *ConfigTemplateService) Update(id string, spec ConfigTemplateSpec) (*ConfigTemplate, error) {
	if err := validateConfigTemplateSpec(spec); err != nil {
		return nil, err
	}
	var out *ConfigTemplate
	err := cs.store.Update(func(d *Data) error {
		var t *ConfigTemplate
		for _, cand := range d.ConfigTemplates {
			if cand.ID == id {
				t = cand
				break
			}
		}
		if t == nil {
			return fmt.Errorf("config template %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.ConfigTemplates {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("config template %q: %w", spec.Name, ErrExists)
			}
		}
		t.Name = spec.Name
		t.Type = spec.Type
		t.Body = append(json.RawMessage(nil), spec.Body...)
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
// Setting a flag to its current value is not special-cased: like the other
// SetEnabled implementations of the package it rewrites the store and
// bumps UpdatedAt.
func (cs *ConfigTemplateService) SetEnabled(id string, enabled bool) error {
	return cs.update(id, func(t *ConfigTemplate) error {
		t.Enabled = enabled
		return nil
	})
}

// Delete removes the config template with the given id.
func (cs *ConfigTemplateService) Delete(id string) error {
	return cs.store.Update(func(d *Data) error {
		idx := -1
		for i, t := range d.ConfigTemplates {
			if t.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("config template %q: %w", id, ErrNotFound)
		}
		d.ConfigTemplates = append(d.ConfigTemplates[:idx], d.ConfigTemplates[idx+1:]...)
		return nil
	})
}

// update applies fn to the template with the given id under the store
// write lock; fn may mutate the template in place. Returning an error
// rolls back.
func (cs *ConfigTemplateService) update(id string, fn func(t *ConfigTemplate) error) error {
	return cs.store.Update(func(d *Data) error {
		for _, t := range d.ConfigTemplates {
			if t.ID != id {
				continue
			}
			if err := fn(t); err != nil {
				return err
			}
			t.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("config template %q: %w", id, ErrNotFound)
	})
}

// validateConfigTemplateSpec performs field-level validation independent
// of the store: the name must be non-empty after trimming, the type must
// be non-empty after trimming, and the body must be a valid JSON object.
func validateConfigTemplateSpec(spec ConfigTemplateSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("config template: %w: empty name", ErrInvalid)
	}
	if strings.TrimSpace(spec.Type) == "" {
		return fmt.Errorf("config template: %w: empty type", ErrInvalid)
	}
	return validateConfigTemplateBody(spec.Body)
}

// validateConfigTemplateBody reports whether body is a valid JSON object:
// it must pass json.Valid and start with '{' at the top level, which
// rules out arrays, scalars and null.
func validateConfigTemplateBody(body json.RawMessage) error {
	if !json.Valid(body) {
		return fmt.Errorf("config template: %w: body must be valid JSON", ErrInvalid)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("config template: %w: body must be a JSON object", ErrInvalid)
	}
	return nil
}

// configTemplatePlaceholderRe matches a "${key}" placeholder whose key
// consists of letters, digits and underscores.
var configTemplatePlaceholderRe = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// Expand substitutes every "${key}" placeholder of the template body with
// the corresponding value from params and returns the expanded body as
// JSON. Keys consist of letters, digits and underscores. Values are
// injected literally, in a single pass with no recursive expansion, and
// the consumer is responsible for providing fragments that keep the
// result valid JSON: a placeholder inside a JSON string takes the bare
// (already escaped) string content, a placeholder in value position takes
// a complete JSON value. A placeholder without a matching parameter
// returns an error wrapping ErrInvalid ("missing parameter"); an expanded
// body that is not valid JSON is rejected with ErrInvalid. A body without
// placeholders is returned as-is, as a copy that does not share the
// underlying array with the template, so mutating the result never
// mutates the stored template.
func Expand(template *ConfigTemplate, params map[string]string) (json.RawMessage, error) {
	src := string(template.Body)
	if !configTemplatePlaceholderRe.MatchString(src) {
		// Copy so the returned slice never aliases the template body.
		return append(json.RawMessage(nil), template.Body...), nil
	}
	var missing string
	out := configTemplatePlaceholderRe.ReplaceAllStringFunc(src, func(m string) string {
		key := m[2 : len(m)-1]
		v, ok := params[key]
		if !ok {
			if missing == "" {
				missing = key
			}
			return m
		}
		return v
	})
	if missing != "" {
		return nil, fmt.Errorf("template placeholder %q: missing parameter: %w", missing, ErrInvalid)
	}
	if !json.Valid([]byte(out)) {
		return nil, fmt.Errorf("config template: %w: expanded body is not valid JSON", ErrInvalid)
	}
	return json.RawMessage(out), nil
}
