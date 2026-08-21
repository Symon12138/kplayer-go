package management

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOutputGroupCRUD(t *testing.T) {
	s := newTestStore(t)
	gs := NewOutputGroupService(s)

	g, err := gs.Create(OutputGroupSpec{
		Name:        "main",
		Description: "primary targets",
		Platform:    "bilibili",
		Region:      "cn",
		Business:    "live",
		Outputs:     []string{"out-a", "out-b"},
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if g.ID == "" {
		t.Fatal("expected generated id")
	}
	if g.Name != "main" || g.Description != "primary targets" ||
		g.Platform != "bilibili" || g.Region != "cn" || g.Business != "live" ||
		!g.Enabled {
		t.Fatalf("unexpected group: %+v", g)
	}
	if want := []string{"out-a", "out-b"}; !sameStrings(g.Outputs, want) {
		t.Fatalf("unexpected outputs: %v", g.Outputs)
	}

	got, err := gs.Get(g.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "main" || len(got.Outputs) != 2 {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(gs.List()) != 1 {
		t.Fatalf("expected 1 group in list")
	}

	// Update replaces name, tags and outputs; the spec's zero Enabled
	// (false) is applied, showing full-replacement semantics.
	upd, err := gs.Update(g.ID, OutputGroupSpec{
		Name:        "main-renamed",
		Description: "backup targets",
		Platform:    "douyin",
		Region:      "cn",
		Business:    "vod",
		Outputs:     []string{"out-c"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "main-renamed" || upd.Description != "backup targets" ||
		upd.Platform != "douyin" || upd.Region != "cn" || upd.Business != "vod" ||
		upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if want := []string{"out-c"}; !sameStrings(upd.Outputs, want) {
		t.Fatalf("unexpected outputs after update: %v", upd.Outputs)
	}

	// SetEnabled toggles both ways.
	if err := gs.SetEnabled(g.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := gs.SetEnabled(g.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = gs.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected group disabled after SetEnabled(false)")
	}

	if err := gs.Delete(g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(gs.List()) != 0 {
		t.Fatal("expected empty group list")
	}
	if _, err := gs.Get(g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := gs.Delete(g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestOutputGroupNameValidation(t *testing.T) {
	s := newTestStore(t)
	gs := NewOutputGroupService(s)

	// empty-name create is rejected
	if _, err := gs.Create(OutputGroupSpec{Name: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty name, got %v", err)
	}

	g1, err := gs.Create(OutputGroupSpec{Name: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := gs.Create(OutputGroupSpec{Name: "backup"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// duplicate name create is rejected
	if _, err := gs.Create(OutputGroupSpec{Name: "main"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// empty-name update is rejected
	if _, err := gs.Update(g1.ID, OutputGroupSpec{Name: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}

	// rename onto an existing name is rejected
	if _, err := gs.Update(g1.ID, OutputGroupSpec{Name: "backup"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}

	// renaming to its own current name is fine
	if _, err := gs.Update(g1.ID, OutputGroupSpec{Name: "main"}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
}

func TestOutputGroupAddRemoveOutput(t *testing.T) {
	s := newTestStore(t)
	gs := NewOutputGroupService(s)

	g, err := gs.Create(OutputGroupSpec{Name: "main", Outputs: []string{"out-a"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// AddOutput appends new references; duplicate add is a no-op.
	if err := gs.AddOutput(g.ID, "out-b"); err != nil {
		t.Fatalf("add output: %v", err)
	}
	if err := gs.AddOutput(g.ID, "out-a"); err != nil {
		t.Fatalf("duplicate add: %v", err)
	}
	got, err := gs.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"out-a", "out-b"}; !sameStrings(got.Outputs, want) {
		t.Fatalf("unexpected outputs: %v", got.Outputs)
	}

	// empty reference is rejected
	if err := gs.AddOutput(g.ID, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty reference, got %v", err)
	}

	// RemoveOutput removes every reference; removing an absent reference
	// is a no-op.
	if err := gs.RemoveOutput(g.ID, "out-a"); err != nil {
		t.Fatalf("remove output: %v", err)
	}
	if err := gs.RemoveOutput(g.ID, "nope"); err != nil {
		t.Fatalf("remove absent: %v", err)
	}
	got, err = gs.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"out-b"}; !sameStrings(got.Outputs, want) {
		t.Fatalf("unexpected outputs: %v", got.Outputs)
	}

	// References are trimmed before validation and matching: a spaced add
	// is stored in trimmed form, and a spaced duplicate of an existing
	// reference is a no-op instead of a second entry.
	if err := gs.AddOutput(g.ID, " out-c "); err != nil {
		t.Fatalf("add spaced output: %v", err)
	}
	if err := gs.AddOutput(g.ID, " out-b "); err != nil {
		t.Fatalf("add spaced duplicate: %v", err)
	}
	got, err = gs.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"out-b", "out-c"}; !sameStrings(got.Outputs, want) {
		t.Fatalf("unexpected outputs after spaced adds: %v", got.Outputs)
	}

	// RemoveOutput trims the reference before matching; an all-whitespace
	// reference is rejected with ErrInvalid, symmetric with AddOutput.
	if err := gs.RemoveOutput(g.ID, " out-b "); err != nil {
		t.Fatalf("remove spaced output: %v", err)
	}
	if err := gs.RemoveOutput(g.ID, "   "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for whitespace remove reference, got %v", err)
	}
	got, err = gs.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"out-c"}; !sameStrings(got.Outputs, want) {
		t.Fatalf("unexpected outputs after spaced remove: %v", got.Outputs)
	}

	// missing group id fails with ErrNotFound on every mutator
	if err := gs.AddOutput("missing", "out-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := gs.RemoveOutput("missing", "out-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := gs.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestOutputGroupNoOpOperationsDoNotWrite verifies that operations which do
// not change a group (duplicate AddOutput, RemoveOutput of an absent
// reference) are true no-ops: they neither bump UpdatedAt nor rewrite the
// store file.
func TestOutputGroupNoOpOperationsDoNotWrite(t *testing.T) {
	s := newTestStore(t)
	gs := NewOutputGroupService(s)
	g, err := gs.Create(OutputGroupSpec{Name: "main", Outputs: []string{"out-a"}})
	if err != nil {
		t.Fatal(err)
	}

	before, err := readStore(t, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	orig := g.UpdatedAt
	// Let time advance so a real write would produce a different UpdatedAt.
	time.Sleep(5 * time.Millisecond)

	// Duplicate AddOutput is a no-op.
	if err := gs.AddOutput(g.ID, "out-a"); err != nil {
		t.Fatal(err)
	}
	// A spaced duplicate of an existing reference is also a no-op: the
	// reference is trimmed before the duplicate check, so it takes the
	// errNoop path just like an exact duplicate.
	if err := gs.AddOutput(g.ID, " out-a "); err != nil {
		t.Fatal(err)
	}
	// RemoveOutput of an absent reference is a no-op.
	if err := gs.RemoveOutput(g.ID, "missing"); err != nil {
		t.Fatal(err)
	}

	got, err := gs.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(orig) {
		t.Fatal("expected UpdatedAt unchanged by no-op operations")
	}

	after, err := readStore(t, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("expected no-op operations to leave the store file unchanged")
	}
}

func TestOutputGroupListSorted(t *testing.T) {
	s := newTestStore(t)
	gs := NewOutputGroupService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := gs.Create(OutputGroupSpec{Name: name}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := gs.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d groups, got %d", len(want), len(list))
	}
	for i, g := range list {
		if g.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, groupNames(list))
		}
	}
}

// TestOutputGroupNormalizesReferences verifies Create and Update trim output
// references and drop empty and duplicate entries while preserving order.
func TestOutputGroupNormalizesReferences(t *testing.T) {
	s := newTestStore(t)
	gs := NewOutputGroupService(s)

	g, err := gs.Create(OutputGroupSpec{
		Name:    "main",
		Outputs: []string{" out-a ", "", "out-b", "out-a", "  "},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if want := []string{"out-a", "out-b"}; !sameStrings(g.Outputs, want) {
		t.Fatalf("unexpected normalized outputs: %v", g.Outputs)
	}

	upd, err := gs.Update(g.ID, OutputGroupSpec{Name: "main", Outputs: []string{"", "out-c", "out-c"}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if want := []string{"out-c"}; !sameStrings(upd.Outputs, want) {
		t.Fatalf("unexpected normalized outputs after update: %v", upd.Outputs)
	}
}

// TestOutputGroupMissingFieldCompat verifies a store file written before
// output groups existed (no outputGroups key) opens cleanly with an empty
// group collection and stays usable.
func TestOutputGroupMissingFieldCompat(t *testing.T) {
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
	gs := NewOutputGroupService(s)
	if len(gs.List()) != 0 {
		t.Fatal("expected no groups in legacy store")
	}
	g, err := gs.Create(OutputGroupSpec{Name: "main"})
	if err != nil {
		t.Fatalf("create in legacy store: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := NewOutputGroupService(reopened).Get(g.ID)
	if err != nil {
		t.Fatalf("group lost after reopen: %v", err)
	}
	if got.Name != "main" {
		t.Fatalf("unexpected group: %+v", got)
	}
}

// sameStrings reports whether a and b hold the same strings in the same
// order.
func sameStrings(a, b []string) bool {
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

// groupNames returns the names of the groups in order.
func groupNames(groups []*OutputGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Name)
	}
	return out
}
