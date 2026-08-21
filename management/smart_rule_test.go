package management

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSmartRuleCRUD(t *testing.T) {
	s := newTestStore(t)
	rs := NewSmartRuleService(s)

	spec := SmartRuleSpec{
		Name:           "prime-time",
		Description:    "evening highlights",
		TimeSlots:      []TimeSlot{{StartHour: 18, EndHour: 22}},
		Tags:           []string{"promo", "teaser"},
		MaxDurationSec: 120,
		AvoidRepeat:    true,
		RepeatLookback: 5,
		MaxItems:       10,
		Enabled:        true,
	}
	rule, err := rs.Create(spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rule.ID == "" {
		t.Fatal("expected generated id")
	}
	if rule.Name != "prime-time" || rule.Description != "evening highlights" ||
		rule.MaxDurationSec != 120 || !rule.AvoidRepeat || rule.RepeatLookback != 5 ||
		rule.MaxItems != 10 || !rule.Enabled {
		t.Fatalf("unexpected rule: %+v", rule)
	}
	if !sameTimeSlots(rule.TimeSlots, []TimeSlot{{StartHour: 18, EndHour: 22}}) {
		t.Fatalf("unexpected time slots: %v", rule.TimeSlots)
	}
	if !sameStrings(rule.Tags, []string{"promo", "teaser"}) {
		t.Fatalf("unexpected tags: %v", rule.Tags)
	}
	if rule.CreatedAt.IsZero() || rule.UpdatedAt.IsZero() {
		t.Fatal("expected created/updated timestamps")
	}

	got, err := rs.Get(rule.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "prime-time" || !got.AvoidRepeat {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(rs.List()) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rs.List()))
	}

	// Update replaces everything: the zero values of the replacement spec
	// show full-replacement semantics (slots/tags cleared, caps reset).
	upd, err := rs.Update(rule.ID, SmartRuleSpec{Name: "prime-time-v2"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "prime-time-v2" || upd.Description != "" || len(upd.TimeSlots) != 0 ||
		len(upd.Tags) != 0 || upd.MaxDurationSec != 0 || upd.AvoidRepeat ||
		upd.RepeatLookback != 0 || upd.MaxItems != 0 || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}

	if err := rs.Delete(rule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(rs.List()) != 0 {
		t.Fatal("expected empty rule list")
	}
	if _, err := rs.Get(rule.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := rs.Delete(rule.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestSmartRuleValidation(t *testing.T) {
	s := newTestStore(t)
	rs := NewSmartRuleService(s)

	// empty-name create is rejected
	if _, err := rs.Create(SmartRuleSpec{Name: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty name, got %v", err)
	}

	slotCases := []struct {
		name string
		slot TimeSlot
	}{
		{"negative start", TimeSlot{StartHour: -1, EndHour: 10}},
		{"start above 23", TimeSlot{StartHour: 24, EndHour: 23}},
		{"negative end", TimeSlot{StartHour: 0, EndHour: -1}},
		{"end above 23", TimeSlot{StartHour: 0, EndHour: 24}},
		{"start after end", TimeSlot{StartHour: 20, EndHour: 8}},
	}
	for _, c := range slotCases {
		if _, err := rs.Create(SmartRuleSpec{Name: "x", TimeSlots: []TimeSlot{c.slot}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", c.name, err)
		}
	}

	// boundary slots and zero-valued numeric fields (defaults) are accepted
	rule, err := rs.Create(SmartRuleSpec{
		Name:      "bounds",
		TimeSlots: []TimeSlot{{StartHour: 0, EndHour: 0}, {StartHour: 23, EndHour: 23}, {StartHour: 0, EndHour: 23}},
	})
	if err != nil {
		t.Fatalf("boundary slots rejected: %v", err)
	}
	if !sameTimeSlots(rule.TimeSlots, []TimeSlot{{StartHour: 0, EndHour: 0}, {StartHour: 23, EndHour: 23}, {StartHour: 0, EndHour: 23}}) {
		t.Fatalf("unexpected slots: %v", rule.TimeSlots)
	}

	negCases := []struct {
		name string
		spec SmartRuleSpec
	}{
		{"negative max duration", SmartRuleSpec{Name: "x", MaxDurationSec: -1}},
		{"negative repeat lookback", SmartRuleSpec{Name: "x", RepeatLookback: -1}},
		{"negative max items", SmartRuleSpec{Name: "x", MaxItems: -1}},
	}
	for _, c := range negCases {
		if _, err := rs.Create(c.spec); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", c.name, err)
		}
	}

	t1, err := rs.Create(SmartRuleSpec{Name: "retail"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// duplicate-name create is rejected
	if _, err := rs.Create(SmartRuleSpec{Name: "retail"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// update validation mirrors create
	if _, err := rs.Update(t1.ID, SmartRuleSpec{Name: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}
	// a slot crossing midnight (start > end) is rejected on update too
	if _, err := rs.Update(t1.ID, SmartRuleSpec{Name: "retail", TimeSlots: []TimeSlot{{StartHour: 23, EndHour: 0}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for bad slot on update, got %v", err)
	}
	if _, err := rs.Update(t1.ID, SmartRuleSpec{Name: "retail", MaxItems: -2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for negative max items on update, got %v", err)
	}
	// rename onto an existing name is rejected ("bounds" was created above)
	if _, err := rs.Update(t1.ID, SmartRuleSpec{Name: "bounds"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}
	// renaming to its own current name is fine
	if _, err := rs.Update(t1.ID, SmartRuleSpec{Name: "retail"}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
	// update of a missing rule is ErrNotFound
	if _, err := rs.Update("missing", SmartRuleSpec{Name: "m"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing update, got %v", err)
	}
}

func TestSmartRuleSetEnabled(t *testing.T) {
	s := newTestStore(t)
	rs := NewSmartRuleService(s)

	rule, err := rs.Create(SmartRuleSpec{Name: "edu", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := rs.SetEnabled(rule.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err := rs.Get(rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected rule disabled after SetEnabled(false)")
	}
	if err := rs.SetEnabled(rule.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = rs.Get(rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Fatal("expected rule enabled after SetEnabled(true)")
	}
	if err := rs.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing rule, got %v", err)
	}
}

func TestSmartRuleListSorted(t *testing.T) {
	s := newTestStore(t)
	rs := NewSmartRuleService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := rs.Create(SmartRuleSpec{Name: name}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := rs.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d rules, got %d", len(want), len(list))
	}
	for i, rule := range list {
		if rule.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, smartRuleNames(list))
		}
	}
}

// TestSmartRulePersistsReopen verifies rules written through the service
// survive a close/reopen cycle with every field intact (camelCase JSON
// round trip), and that the document carries the agreed "smartRules" key.
func TestSmartRulePersistsReopen(t *testing.T) {
	s := newTestStore(t)
	rs := NewSmartRuleService(s)

	rule, err := rs.Create(SmartRuleSpec{
		Name:           "prime-time",
		Description:    "evening highlights",
		TimeSlots:      []TimeSlot{{StartHour: 18, EndHour: 22}, {StartHour: 9, EndHour: 11}},
		Tags:           []string{"promo", "teaser"},
		MaxDurationSec: 120,
		AvoidRepeat:    true,
		RepeatLookback: 5,
		MaxItems:       10,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"smartRules"`)) {
		t.Fatalf("store file lacks the smartRules key: %s", raw)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := NewSmartRuleService(reopened).Get(rule.ID)
	if err != nil {
		t.Fatalf("rule lost after reopen: %v", err)
	}
	if got.Name != "prime-time" || got.Description != "evening highlights" ||
		got.MaxDurationSec != 120 || !got.AvoidRepeat || got.RepeatLookback != 5 ||
		got.MaxItems != 10 || !got.Enabled {
		t.Fatalf("unexpected rule after reopen: %+v", got)
	}
	if !sameTimeSlots(got.TimeSlots, []TimeSlot{{StartHour: 18, EndHour: 22}, {StartHour: 9, EndHour: 11}}) {
		t.Fatalf("time slots lost after reopen: %v", got.TimeSlots)
	}
	if !sameStrings(got.Tags, []string{"promo", "teaser"}) {
		t.Fatalf("tags lost after reopen: %v", got.Tags)
	}
}

// TestSmartRuleLegacyStoreCompatible verifies that a store file written
// before the smart rule collection existed (no smartRules key) still opens
// and serves the service: the absent key leaves the field nil instead of
// failing, and rules created after the upgrade persist and reopen normally.
func TestSmartRuleLegacyStoreCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	legacy := "{\n  \"media\": [],\n  \"users\": [],\n  \"updated_at\": \"2026-01-01T00:00:00Z\"\n}\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.SmartRules != nil {
		t.Fatalf("expected SmartRules nil after opening a legacy file, got %v", snap.SmartRules)
	}
	rs := NewSmartRuleService(s)
	if len(rs.List()) != 0 {
		t.Fatal("expected no smart rules in a legacy store")
	}

	// rules created after the upgrade persist and reopen fine
	rule, err := rs.Create(SmartRuleSpec{
		Name:      "retail",
		TimeSlots: []TimeSlot{{StartHour: 8, EndHour: 20}},
		Tags:      []string{"promo"},
	})
	if err != nil {
		t.Fatalf("create after upgrade: %v", err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewSmartRuleService(reopened).Get(rule.ID)
	if err != nil {
		t.Fatalf("rule lost after upgrade reopen: %v", err)
	}
	if got.Name != "retail" ||
		!sameTimeSlots(got.TimeSlots, []TimeSlot{{StartHour: 8, EndHour: 20}}) ||
		!sameStrings(got.Tags, []string{"promo"}) {
		t.Fatalf("unexpected rule after upgrade reopen: %+v", got)
	}
}

// TestSmartRuleSpecSlicesCopied verifies the service never aliases the
// caller's spec: mutating the spec's slices after Create or Update must not
// leak into the store.
func TestSmartRuleSpecSlicesCopied(t *testing.T) {
	s := newTestStore(t)
	rs := NewSmartRuleService(s)

	spec := SmartRuleSpec{
		Name:      "copy",
		TimeSlots: []TimeSlot{{StartHour: 8, EndHour: 12}},
		Tags:      []string{"a", "b"},
	}
	rule, err := rs.Create(spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	spec.TimeSlots[0].EndHour = 23
	spec.Tags[0] = "mutated"
	got, err := rs.Get(rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTimeSlots(got.TimeSlots, []TimeSlot{{StartHour: 8, EndHour: 12}}) {
		t.Fatalf("stored slots changed by spec mutation: %v", got.TimeSlots)
	}
	if !sameStrings(got.Tags, []string{"a", "b"}) {
		t.Fatalf("stored tags changed by spec mutation: %v", got.Tags)
	}

	updSpec := SmartRuleSpec{
		Name:      "copy-v2",
		TimeSlots: []TimeSlot{{StartHour: 20, EndHour: 23}},
		Tags:      []string{"c"},
	}
	if _, err := rs.Update(rule.ID, updSpec); err != nil {
		t.Fatalf("update: %v", err)
	}
	updSpec.TimeSlots[0].StartHour = 0
	updSpec.Tags[0] = "mutated"
	got, err = rs.Get(rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTimeSlots(got.TimeSlots, []TimeSlot{{StartHour: 20, EndHour: 23}}) {
		t.Fatalf("stored slots changed by update spec mutation: %v", got.TimeSlots)
	}
	if !sameStrings(got.Tags, []string{"c"}) {
		t.Fatalf("stored tags changed by update spec mutation: %v", got.Tags)
	}
}

// sameTimeSlots reports whether a and b hold the same slots in order.
func sameTimeSlots(a, b []TimeSlot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// smartRuleNames returns the names of the rules in order.
func smartRuleNames(rules []*SmartRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Name)
	}
	return out
}
