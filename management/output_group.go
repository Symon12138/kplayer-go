package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// OutputGroup is a named, taggable collection of output references used to
// organize broadcast targets by platform, region and business type.
//
// Outputs holds the Unique identifiers of the outputs managed by the core
// side (module/output provider); the management side only stores the
// references and has no output registry, so whether a reference resolves to
// a real output is validated by the core side when the group is applied.
type OutputGroup struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Platform, Region and Business are optional classification tags.
	Platform  string    `json:"platform,omitempty"`
	Region    string    `json:"region,omitempty"`
	Business  string    `json:"business,omitempty"`
	Outputs   []string  `json:"outputs,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// OutputGroupSpec is the validated input used to create or replace an output
// group. Outputs holds output unique references; empty and duplicate entries
// are dropped when the spec is applied.
type OutputGroupSpec struct {
	Name        string
	Description string
	Platform    string
	Region      string
	Business    string
	Outputs     []string
	Enabled     bool
}

// OutputGroupService provides CRUD over the output groups of a Store plus
// output membership manipulation.
type OutputGroupService struct {
	store *Store
}

// NewOutputGroupService returns an OutputGroupService backed by store.
func NewOutputGroupService(store *Store) *OutputGroupService {
	return &OutputGroupService{store: store}
}

// List returns all output groups sorted by name.
func (gs *OutputGroupService) List() []*OutputGroup {
	out := make([]*OutputGroup, 0)
	gs.store.View(func(d *Data) {
		out = append(out, d.OutputGroups...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the output group with the given id.
func (gs *OutputGroupService) Get(id string) (*OutputGroup, error) {
	var found *OutputGroup
	gs.store.View(func(d *Data) {
		for _, g := range d.OutputGroups {
			if g.ID == id {
				found = g
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("output group %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new output group from spec. The name must be non-empty
// (ErrInvalid) and unique among groups (ErrExists).
func (gs *OutputGroupService) Create(spec OutputGroupSpec) (*OutputGroup, error) {
	if err := validateOutputGroupSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	g := &OutputGroup{
		ID:          newID(),
		Name:        spec.Name,
		Description: spec.Description,
		Platform:    spec.Platform,
		Region:      spec.Region,
		Business:    spec.Business,
		Outputs:     normalizeOutputRefs(spec.Outputs),
		Enabled:     spec.Enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	err := gs.store.Update(func(d *Data) error {
		for _, exist := range d.OutputGroups {
			if exist.Name == g.Name {
				return fmt.Errorf("output group %q: %w", g.Name, ErrExists)
			}
		}
		d.OutputGroups = append(d.OutputGroups, g)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// Update replaces the configuration of the group with the given id from
// spec: name, description, classification tags, output references and the
// enabled flag are all replaced. The new name must be non-empty (ErrInvalid)
// and must not collide with another group (ErrExists). It returns the
// updated group.
func (gs *OutputGroupService) Update(id string, spec OutputGroupSpec) (*OutputGroup, error) {
	if err := validateOutputGroupSpec(spec); err != nil {
		return nil, err
	}
	var out *OutputGroup
	err := gs.store.Update(func(d *Data) error {
		var g *OutputGroup
		for _, cand := range d.OutputGroups {
			if cand.ID == id {
				g = cand
				break
			}
		}
		if g == nil {
			return fmt.Errorf("output group %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.OutputGroups {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("output group %q: %w", spec.Name, ErrExists)
			}
		}
		g.Name = spec.Name
		g.Description = spec.Description
		g.Platform = spec.Platform
		g.Region = spec.Region
		g.Business = spec.Business
		g.Outputs = normalizeOutputRefs(spec.Outputs)
		g.Enabled = spec.Enabled
		g.UpdatedAt = time.Now()
		out = g
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the group.
func (gs *OutputGroupService) SetEnabled(id string, enabled bool) error {
	return gs.update(id, func(g *OutputGroup) error {
		g.Enabled = enabled
		return nil
	})
}

// AddOutput appends the output with the given unique reference to the group.
// The reference is trimmed before validation and matching, consistent with
// the normalization Create and Update apply. Adding an output that is
// already present is a no-op: the store is not rewritten and UpdatedAt is
// not bumped. The reference must be a non-empty string; the management side
// has no output registry, so whether the reference resolves to a real
// output is validated by the core side.
func (gs *OutputGroupService) AddOutput(id, unique string) error {
	return gs.update(id, func(g *OutputGroup) error {
		unique = strings.TrimSpace(unique)
		if unique == "" {
			return fmt.Errorf("group output: %w: empty unique", ErrInvalid)
		}
		for _, u := range g.Outputs {
			if u == unique {
				return errNoop // already present; idempotent
			}
		}
		g.Outputs = append(g.Outputs, unique)
		return nil
	})
}

// RemoveOutput removes every reference to unique from the group. The
// reference is trimmed before matching, consistent with AddOutput and the
// normalization Create and Update apply. A reference that is empty after
// trimming is rejected with ErrInvalid (symmetric with AddOutput): it can
// never match a stored reference, so treating it as a silent no-op would
// hide a caller bug. Removing an absent output is a no-op (no store write,
// no UpdatedAt bump).
func (gs *OutputGroupService) RemoveOutput(id, unique string) error {
	return gs.update(id, func(g *OutputGroup) error {
		unique = strings.TrimSpace(unique)
		if unique == "" {
			return fmt.Errorf("group output: %w: empty unique", ErrInvalid)
		}
		kept := g.Outputs[:0]
		removed := false
		for _, u := range g.Outputs {
			if u != unique {
				kept = append(kept, u)
			} else {
				removed = true
			}
		}
		if !removed {
			return errNoop
		}
		g.Outputs = kept
		return nil
	})
}

// Delete removes the group with the given id. A group that is still
// referenced by another entity cannot be deleted (ErrInUse). Groups never
// reference each other and no entity of the current document holds a group
// id, so the guard cannot fire yet; it mirrors the other Delete
// implementations and covers the group references later phases add (for
// example a scheduled task bound to a group).
func (gs *OutputGroupService) Delete(id string) error {
	return gs.store.Update(func(d *Data) error {
		idx := -1
		for i, g := range d.OutputGroups {
			if g.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("output group %q: %w", id, ErrNotFound)
		}
		g := d.OutputGroups[idx]
		if outputGroupInUse(d, g) {
			return fmt.Errorf("output group %q is referenced: %w", g.Name, ErrInUse)
		}
		d.OutputGroups = append(d.OutputGroups[:idx], d.OutputGroups[idx+1:]...)
		return nil
	})
}

// update applies fn to the group with the given id under the store write
// lock; fn may mutate the group in place. Returning an error rolls back.
func (gs *OutputGroupService) update(id string, fn func(g *OutputGroup) error) error {
	return gs.store.Update(func(d *Data) error {
		for _, g := range d.OutputGroups {
			if g.ID != id {
				continue
			}
			if err := fn(g); err != nil {
				return err
			}
			g.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("output group %q: %w", id, ErrNotFound)
	})
}

// outputGroupInUse reports whether any entity in d references the group. No
// entity of the current model references output groups (groups never
// reference each other), so it always returns false today; the guard is kept
// for consistency with the other Delete implementations and covers the
// group references later phases introduce.
func outputGroupInUse(d *Data, g *OutputGroup) bool {
	return false
}

// validateOutputGroupSpec performs field-level validation independent of the
// store: the name must be non-empty after trimming.
func validateOutputGroupSpec(spec OutputGroupSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("output group: %w: empty name", ErrInvalid)
	}
	return nil
}

// normalizeOutputRefs trims output unique references, dropping empty and
// duplicate entries while preserving order. The management side has no
// output registry: whether a reference resolves to a real output is
// validated by the core side (module/output provider).
func normalizeOutputRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
