package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultHealthPolicyMaxRetries and DefaultHealthPolicyRetryWindowSeconds
// are the retry limits applied when a spec leaves the corresponding field at
// its zero value.
const (
	DefaultHealthPolicyMaxRetries         = 3
	DefaultHealthPolicyRetryWindowSeconds = 60
)

// HealthPolicy is a named retry/auto-skip policy applied when a play
// output fails. It is pure configuration: the decision functions in this
// file (ShouldAutoSkip, ShouldSkipOnFailure, RetriesExceeded) turn the
// policy into decisions, and the Scheduler applies the auto-skip decision
// when a Play fails (see scheduler.go).
type HealthPolicy struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// MaxRetries is the maximum number of retry attempts permitted for a
	// failing play before it is given up.
	MaxRetries int `json:"maxRetries"`
	// RetryWindowSeconds is the time window, in seconds, within which the
	// retries count: at most MaxRetries attempts per window.
	RetryWindowSeconds int `json:"retryWindowSeconds"`
	// AutoSkipOnFailure, when enabled, lets the playback skip the failing
	// item and continue with the next one instead of stopping.
	AutoSkipOnFailure bool      `json:"autoSkipOnFailure"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// HealthPolicySpec is the validated input used to create or replace a health
// policy. A zero MaxRetries defaults to DefaultHealthPolicyMaxRetries and a
// zero RetryWindowSeconds to DefaultHealthPolicyRetryWindowSeconds; negative
// values are rejected.
type HealthPolicySpec struct {
	Name               string
	MaxRetries         int
	RetryWindowSeconds int
	AutoSkipOnFailure  bool
	Enabled            bool
}

// HealthPolicyService provides CRUD over the health policies of a Store.
type HealthPolicyService struct {
	store *Store
}

// NewHealthPolicyService returns a HealthPolicyService backed by store.
func NewHealthPolicyService(store *Store) *HealthPolicyService {
	return &HealthPolicyService{store: store}
}

// List returns all health policies sorted by name.
func (hs *HealthPolicyService) List() []*HealthPolicy {
	out := make([]*HealthPolicy, 0)
	hs.store.View(func(d *Data) {
		out = append(out, d.HealthPolicies...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the health policy with the given id.
func (hs *HealthPolicyService) Get(id string) (*HealthPolicy, error) {
	var found *HealthPolicy
	hs.store.View(func(d *Data) {
		for _, p := range d.HealthPolicies {
			if p.ID == id {
				found = p
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("health policy %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new health policy from spec. The name must be non-empty and
// unique (ErrExists); the retry limits must be positive (ErrInvalid), with
// zero fields defaulting to the package defaults.
func (hs *HealthPolicyService) Create(spec HealthPolicySpec) (*HealthPolicy, error) {
	spec, err := normalizeHealthPolicySpec(spec)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	p := &HealthPolicy{
		ID:                 newID(),
		Name:               spec.Name,
		MaxRetries:         spec.MaxRetries,
		RetryWindowSeconds: spec.RetryWindowSeconds,
		AutoSkipOnFailure:  spec.AutoSkipOnFailure,
		Enabled:            spec.Enabled,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	err = hs.store.Update(func(d *Data) error {
		for _, exist := range d.HealthPolicies {
			if exist.Name == p.Name {
				return fmt.Errorf("health policy %q: %w", p.Name, ErrExists)
			}
		}
		d.HealthPolicies = append(d.HealthPolicies, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Update replaces the configuration of the health policy with the given id
// from spec: name, retry limits, auto-skip flag and the enabled flag are all
// replaced, with the same defaulting as Create. The new name must be
// non-empty (ErrInvalid) and must not collide with another policy
// (ErrExists); renaming to its own current name is allowed. It returns the
// updated policy.
func (hs *HealthPolicyService) Update(id string, spec HealthPolicySpec) (*HealthPolicy, error) {
	spec, err := normalizeHealthPolicySpec(spec)
	if err != nil {
		return nil, err
	}
	var out *HealthPolicy
	err = hs.store.Update(func(d *Data) error {
		var p *HealthPolicy
		for _, cand := range d.HealthPolicies {
			if cand.ID == id {
				p = cand
				break
			}
		}
		if p == nil {
			return fmt.Errorf("health policy %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.HealthPolicies {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("health policy %q: %w", spec.Name, ErrExists)
			}
		}
		p.Name = spec.Name
		p.MaxRetries = spec.MaxRetries
		p.RetryWindowSeconds = spec.RetryWindowSeconds
		p.AutoSkipOnFailure = spec.AutoSkipOnFailure
		p.Enabled = spec.Enabled
		p.UpdatedAt = time.Now()
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the health policy.
func (hs *HealthPolicyService) SetEnabled(id string, enabled bool) error {
	return hs.update(id, func(p *HealthPolicy) error {
		p.Enabled = enabled
		return nil
	})
}

// Delete removes the health policy with the given id. No entity of the
// current document references health policies, so no ErrInUse guard is
// needed.
func (hs *HealthPolicyService) Delete(id string) error {
	return hs.store.Update(func(d *Data) error {
		idx := -1
		for i, p := range d.HealthPolicies {
			if p.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("health policy %q: %w", id, ErrNotFound)
		}
		d.HealthPolicies = append(d.HealthPolicies[:idx], d.HealthPolicies[idx+1:]...)
		return nil
	})
}

// update applies fn to the health policy with the given id under the store
// write lock; fn may mutate the policy in place. Returning an error rolls
// back.
func (hs *HealthPolicyService) update(id string, fn func(p *HealthPolicy) error) error {
	return hs.store.Update(func(d *Data) error {
		for _, p := range d.HealthPolicies {
			if p.ID != id {
				continue
			}
			if err := fn(p); err != nil {
				return err
			}
			p.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("health policy %q: %w", id, ErrNotFound)
	})
}

// normalizeHealthPolicySpec performs field-level validation independent of
// the store and returns the normalized spec: the name is trimmed and zero
// retry limits are replaced with the package defaults.
func normalizeHealthPolicySpec(spec HealthPolicySpec) (HealthPolicySpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return spec, fmt.Errorf("health policy: %w: empty name", ErrInvalid)
	}
	if spec.MaxRetries == 0 {
		spec.MaxRetries = DefaultHealthPolicyMaxRetries
	}
	if spec.MaxRetries < 0 {
		return spec, fmt.Errorf("health policy: %w: max retries must be positive", ErrInvalid)
	}
	if spec.RetryWindowSeconds == 0 {
		spec.RetryWindowSeconds = DefaultHealthPolicyRetryWindowSeconds
	}
	if spec.RetryWindowSeconds < 0 {
		return spec, fmt.Errorf("health policy: %w: retry window seconds must be positive", ErrInvalid)
	}
	return spec, nil
}

// ShouldAutoSkip reports whether a play failure should be silently skipped
// (the next item starts) instead of surfacing as an error, per the policy.
// A nil or disabled policy never auto-skips.
func ShouldAutoSkip(p *HealthPolicy) bool {
	return p != nil && p.Enabled && p.AutoSkipOnFailure
}

// ShouldSkipOnFailure reports whether a failed play should be skipped
// instead of surfaced as an error: the policy must enable auto-skip and the
// failure must not be a scheduler-initiated cancellation (preemption,
// interrupt expiry or Stop). A cancelled play was superseded deliberately,
// so skipping it could act on the playback that replaced it.
func ShouldSkipOnFailure(p *HealthPolicy, cancelled bool) bool {
	return !cancelled && ShouldAutoSkip(p)
}

// RetriesExceeded reports whether the number of retry attempts already made
// for a failing play has reached the policy limit: with MaxRetries=3 the
// third retry is the last permitted one (attempts >= MaxRetries). A nil or
// disabled policy reports false: without an active policy no retry limit
// applies.
func RetriesExceeded(p *HealthPolicy, attempts int) bool {
	return p != nil && p.Enabled && attempts >= p.MaxRetries
}
