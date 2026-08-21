package management

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthPolicyCRUD(t *testing.T) {
	s := newTestStore(t)
	hs := NewHealthPolicyService(s)

	p, err := hs.Create(HealthPolicySpec{
		Name:               "main",
		MaxRetries:         5,
		RetryWindowSeconds: 120,
		AutoSkipOnFailure:  true,
		Enabled:            true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected generated id")
	}
	if p.Name != "main" || p.MaxRetries != 5 || p.RetryWindowSeconds != 120 ||
		!p.AutoSkipOnFailure || !p.Enabled {
		t.Fatalf("unexpected policy: %+v", p)
	}

	got, err := hs.Get(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "main" || got.MaxRetries != 5 {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(hs.List()) != 1 {
		t.Fatalf("expected 1 policy in list")
	}

	// Update replaces everything, including the enabled flag (full
	// replacement semantics: the spec's zero Enabled is applied).
	upd, err := hs.Update(p.ID, HealthPolicySpec{
		Name:               "main-renamed",
		MaxRetries:         2,
		RetryWindowSeconds: 30,
		AutoSkipOnFailure:  false,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "main-renamed" || upd.MaxRetries != 2 || upd.RetryWindowSeconds != 30 ||
		upd.AutoSkipOnFailure || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}

	// SetEnabled toggles both ways.
	if err := hs.SetEnabled(p.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := hs.SetEnabled(p.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = hs.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected policy disabled after SetEnabled(false)")
	}

	if err := hs.Delete(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(hs.List()) != 0 {
		t.Fatal("expected empty policy list")
	}
	if _, err := hs.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := hs.Delete(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestHealthPolicyDefaultsAndValidation(t *testing.T) {
	s := newTestStore(t)
	hs := NewHealthPolicyService(s)

	// zero retry limits default to the package defaults
	p, err := hs.Create(HealthPolicySpec{Name: "defaults"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.MaxRetries != DefaultHealthPolicyMaxRetries {
		t.Fatalf("expected default max retries %d, got %d", DefaultHealthPolicyMaxRetries, p.MaxRetries)
	}
	if p.RetryWindowSeconds != DefaultHealthPolicyRetryWindowSeconds {
		t.Fatalf("expected default retry window %d, got %d", DefaultHealthPolicyRetryWindowSeconds, p.RetryWindowSeconds)
	}
	if p.AutoSkipOnFailure || p.Enabled {
		t.Fatalf("expected auto-skip and enabled to default to false: %+v", p)
	}

	// negative retry limits are rejected
	if _, err := hs.Create(HealthPolicySpec{Name: "neg", MaxRetries: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for negative max retries, got %v", err)
	}
	if _, err := hs.Create(HealthPolicySpec{Name: "neg", RetryWindowSeconds: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for negative retry window, got %v", err)
	}
	if _, err := hs.Create(HealthPolicySpec{Name: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty name, got %v", err)
	}

	// duplicate name create is rejected
	if _, err := hs.Create(HealthPolicySpec{Name: "defaults"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// invalid update is rejected and leaves the policy untouched
	if _, err := hs.Update(p.ID, HealthPolicySpec{Name: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}

	// rename onto an existing name is rejected
	if _, err := hs.Create(HealthPolicySpec{Name: "other"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := hs.Update(p.ID, HealthPolicySpec{Name: "other"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}

	// renaming to its own current name is fine
	if _, err := hs.Update(p.ID, HealthPolicySpec{Name: "defaults"}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}

	// Update applies defaults to zero fields, full replacement semantics
	upd, err := hs.Update(p.ID, HealthPolicySpec{Name: "defaults"})
	if err != nil {
		t.Fatalf("update with zero limits: %v", err)
	}
	if upd.MaxRetries != DefaultHealthPolicyMaxRetries || upd.RetryWindowSeconds != DefaultHealthPolicyRetryWindowSeconds {
		t.Fatalf("expected defaults restored by update, got %+v", upd)
	}

	// missing id fails with ErrNotFound on every mutator
	if _, err := hs.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := hs.Update("missing", HealthPolicySpec{Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := hs.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestHealthPolicyListSorted(t *testing.T) {
	s := newTestStore(t)
	hs := NewHealthPolicyService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := hs.Create(HealthPolicySpec{Name: name}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := hs.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d policies, got %d", len(want), len(list))
	}
	for i, p := range list {
		if p.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, policyNames(list))
		}
	}
}

func TestShouldAutoSkip(t *testing.T) {
	cases := []struct {
		name   string
		policy *HealthPolicy
		want   bool
	}{
		{"nil policy never auto-skips", nil, false},
		{"disabled policy never auto-skips", &HealthPolicy{Enabled: false, AutoSkipOnFailure: true}, false},
		{"auto-skip off", &HealthPolicy{Enabled: true, AutoSkipOnFailure: false}, false},
		{"auto-skip on", &HealthPolicy{Enabled: true, AutoSkipOnFailure: true}, true},
	}
	for _, tc := range cases {
		if got := ShouldAutoSkip(tc.policy); got != tc.want {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestRetriesExceeded(t *testing.T) {
	p := &HealthPolicy{Enabled: true, MaxRetries: 3}
	cases := []struct {
		name     string
		policy   *HealthPolicy
		attempts int
		want     bool
	}{
		{"nil policy has no limit", nil, 100, false},
		{"disabled policy has no limit", &HealthPolicy{Enabled: false, MaxRetries: 1}, 5, false},
		{"no retries yet", p, 0, false},
		{"within the limit", p, 2, false},
		{"third retry is the last permitted", p, 3, true},
		{"beyond the limit", p, 4, true},
	}
	for _, tc := range cases {
		if got := RetriesExceeded(tc.policy, tc.attempts); got != tc.want {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestShouldSkipOnFailure(t *testing.T) {
	autoskip := &HealthPolicy{Enabled: true, AutoSkipOnFailure: true}
	cases := []struct {
		name      string
		policy    *HealthPolicy
		cancelled bool
		want      bool
	}{
		{"nil policy never skips", nil, false, false},
		{"disabled policy never skips", &HealthPolicy{Enabled: false, AutoSkipOnFailure: true}, false, false},
		{"auto-skip off never skips", &HealthPolicy{Enabled: true, AutoSkipOnFailure: false}, false, false},
		{"auto-skip on skips genuine failures", autoskip, false, true},
		{"cancelled failure is never skipped", autoskip, true, false},
	}
	for _, tc := range cases {
		if got := ShouldSkipOnFailure(tc.policy, tc.cancelled); got != tc.want {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.want, got)
		}
	}
}

// TestHealthPolicyMissingFieldCompat verifies a store file written before
// health policies existed (no healthPolicies key) opens cleanly with an
// empty policy collection and stays usable.
func TestHealthPolicyMissingFieldCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	legacy := `{
  "media": [],
  "playlists": [],
  "alarms": [],
  "tasks": [],
  "updated_at": "2026-01-01T00:00:00Z"
}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	hs := NewHealthPolicyService(s)
	if len(hs.List()) != 0 {
		t.Fatal("expected no policies in legacy store")
	}
	p, err := hs.Create(HealthPolicySpec{Name: "main"})
	if err != nil {
		t.Fatalf("create in legacy store: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := NewHealthPolicyService(reopened).Get(p.ID)
	if err != nil {
		t.Fatalf("policy lost after reopen: %v", err)
	}
	if got.Name != "main" {
		t.Fatalf("unexpected policy: %+v", got)
	}
}

// policyNames returns the names of the policies in order.
func policyNames(policies []*HealthPolicy) []string {
	out := make([]string, 0, len(policies))
	for _, p := range policies {
		out = append(out, p.Name)
	}
	return out
}
