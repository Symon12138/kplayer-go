package management

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPlaylistCRUD(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")

	p, err := ps.Create("main", "primary feed", []string{m1.ID, m2.ID}, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected generated id")
	}
	if p.Name != "main" || p.Desc != "primary feed" || !p.Loop {
		t.Fatalf("unexpected playlist: %+v", p)
	}

	got, err := ps.Get(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "main" || len(got.Items) != 2 {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(ps.List()) != 1 || len(ps.ListSorted()) != 1 {
		t.Fatalf("expected 1 playlist in list")
	}

	// empty-name create is rejected
	if _, err := ps.Create("", "", nil, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty name, got %v", err)
	}

	if err := ps.Delete(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(ps.List()) != 0 {
		t.Fatal("expected empty playlist list")
	}
	if _, err := ps.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestPlaylistRejectsMissingMedia(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlaylistService(s)
	if _, err := ps.Create("x", "", []string{"nope"}, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestPlaylistSetItemsRejectsMissingMedia verifies SetItems refuses a
// reference to a non-existent media and, on rollback, leaves the playlist
// (memory and store file) unchanged.
func TestPlaylistSetItemsRejectsMissingMedia(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	p, err := ps.Create("main", "", []string{m.ID}, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := ps.SetItems(p.ID, []string{m.ID, "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// The playlist still holds its original items.
	got, err := ps.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := wantIDs(got, m.ID); diff != "" {
		t.Fatalf("playlist changed after failed SetItems: %s", diff)
	}

	// Setting to all-missing is also rejected and leaves items intact.
	if err := ps.SetItems(p.ID, []string{"nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	got, err = ps.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := wantIDs(got, m.ID); diff != "" {
		t.Fatalf("playlist changed after failed SetItems: %s", diff)
	}

	// A valid SetItems still works afterwards.
	if err := ps.SetItems(p.ID, []string{m.ID}); err != nil {
		t.Fatalf("valid SetItems after rejections: %v", err)
	}
}

// TestPlaylistAddItemRejectsMissingMedia verifies AddItem refuses a reference
// to a non-existent media and, on rollback, leaves the playlist unchanged.
func TestPlaylistAddItemRejectsMissingMedia(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	p, err := ps.Create("main", "", []string{m.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := readStore(t, s.Path())
	if err != nil {
		t.Fatal(err)
	}

	if err := ps.AddItem(p.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	got, err := ps.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := wantIDs(got, m.ID); diff != "" {
		t.Fatalf("playlist changed after failed AddItem: %s", diff)
	}
	// A rejected AddItem must not rewrite the store file (atomic rollback).
	after, err := readStore(t, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("expected store file unchanged after rejected AddItem")
	}
}

func TestPlaylistDeleteInUseByTask(t *testing.T) {
	s := newTestStore(t)
	ps := NewPlaylistService(s)
	ts := NewTaskService(s)
	m := mustAddMedia(t, NewMediaService(s), "/v/1.mp4")
	p, err := ps.Create("main", "", []string{m.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create(TaskSpec{Name: "nightly", Type: TaskTypeInterval, Interval: 3600, PlaylistID: p.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ps.Delete(p.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse, got %v", err)
	}
}

func TestPlaylistItemOps(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	a := mustAddMedia(t, ms, "/v/a.mp4")
	b := mustAddMedia(t, ms, "/v/b.mp4")
	c := mustAddMedia(t, ms, "/v/c.mp4")
	d := mustAddMedia(t, ms, "/v/d.mp4")

	p, err := ps.Create("main", "", []string{a.ID, b.ID, c.ID, d.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	fresh := func(want ...string) string {
		cur, err := ps.Get(p.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return wantIDs(cur, want...)
	}

	// MoveItem: [a,b,c,d] move 0->2 => [b,c,a,d]
	if err := ps.MoveItem(p.ID, 0, 2); err != nil {
		t.Fatal(err)
	}
	if got := fresh(b.ID, c.ID, a.ID, d.ID); got != "" {
		t.Fatalf("after move: %s", got)
	}

	// RemoveItem removes every reference
	if err := ps.RemoveItem(p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := fresh(b.ID, c.ID, d.ID); got != "" {
		t.Fatalf("after remove: %s", got)
	}

	// AddItem (already present is a no-op, new appends)
	if err := ps.AddItem(p.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := ps.AddItem(p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := fresh(b.ID, c.ID, d.ID, a.ID); got != "" {
		t.Fatalf("after add: %s", got)
	}

	// SetItems replaces the whole list
	if err := ps.SetItems(p.ID, []string{c.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	if got := fresh(c.ID, a.ID); got != "" {
		t.Fatalf("after set: %s", got)
	}

	// ClearItems empties it
	if err := ps.ClearItems(p.ID); err != nil {
		t.Fatal(err)
	}
	if got := fresh(); got != "" {
		t.Fatalf("after clear: %s", got)
	}
}

func TestPlaylistResolve(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")

	p, err := ps.Create("main", "", []string{m2.ID, m1.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ps.Resolve(p.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res) != 2 || res[0].ID != m2.ID || res[1].ID != m1.ID {
		t.Fatalf("unexpected resolve order: %+v", res)
	}

	if _, err := ps.Resolve("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// wantIDs returns a description of p's current media ids when they do not
// equal want, or "" when they do.
func wantIDs(p *Playlist, want ...string) string {
	got := p.MediaIDs()
	if len(got) != len(want) {
		return "length mismatch"
	}
	for i := range want {
		if got[i] != want[i] {
			return "order mismatch"
		}
	}
	return ""
}

// TestPlaylistNoOpOperationsDoNotWrite verifies that operations which do not
// change a playlist (duplicate AddItem, RemoveItem of an absent id, no-op
// MoveItem) are true no-ops: they neither bump UpdatedAt nor rewrite the
// store file.
func TestPlaylistNoOpOperationsDoNotWrite(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")
	p, err := ps.Create("main", "", []string{m.ID}, false)
	if err != nil {
		t.Fatal(err)
	}

	before, err := readStore(t, s.Path())
	if err != nil {
		t.Fatal(err)
	}
	orig := p.UpdatedAt
	// Let time advance so a real write would produce a different UpdatedAt.
	time.Sleep(5 * time.Millisecond)

	// Duplicate AddItem is a no-op.
	if err := ps.AddItem(p.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	// RemoveItem of an absent id is a no-op.
	if err := ps.RemoveItem(p.ID, "missing"); err != nil {
		t.Fatal(err)
	}
	// MoveItem within a 1-item playlist is a no-op.
	if err := ps.MoveItem(p.ID, 0, 0); err != nil {
		t.Fatal(err)
	}

	got, err := ps.Get(p.ID)
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

func readStore(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func TestPlaylistFallbackValidation(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	m := mustAddMedia(t, ms, "/v/1.mp4")

	p, err := ps.Create("main", "", []string{m.ID}, false)
	if err != nil {
		t.Fatal(err)
	}

	// a missing fallback target is rejected
	if err := ps.SetFallback(p.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing fallback, got %v", err)
	}
	// a self reference is rejected
	if err := ps.SetFallback(p.ID, p.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for self fallback, got %v", err)
	}
	// the Update path validates too
	if err := ps.Update(p.ID, func(p *Playlist) error { p.FallbackPlaylistID = "nope"; return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound via Update, got %v", err)
	}
	if err := ps.Update(p.ID, func(p *Playlist) error { p.FallbackPlaylistID = p.ID; return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid via Update, got %v", err)
	}
	// rejected writes leave the playlist unchanged
	got, err := ps.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FallbackPlaylistID != "" {
		t.Fatalf("playlist changed after rejected fallback: %+v", got)
	}

	// a valid fallback attaches and persists
	fb, err := ps.Create("fb", "", []string{m.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SetFallback(p.ID, fb.ID); err != nil {
		t.Fatalf("set fallback: %v", err)
	}
	got, err = ps.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FallbackPlaylistID != fb.ID {
		t.Fatalf("expected fallback %s, got %q", fb.ID, got.FallbackPlaylistID)
	}

	// SetItems still works with a fallback configured
	if err := ps.SetItems(p.ID, []string{m.ID}); err != nil {
		t.Fatalf("set items with fallback: %v", err)
	}

	// clearing with "" is allowed
	if err := ps.SetFallback(p.ID, ""); err != nil {
		t.Fatalf("clear fallback: %v", err)
	}

	// a playlist referenced as fallback cannot be deleted
	if err := ps.SetFallback(p.ID, fb.ID); err != nil {
		t.Fatal(err)
	}
	if err := ps.Delete(fb.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse deleting a fallback target, got %v", err)
	}
}

func TestPlaylistResolveWithFallback(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	ps := NewPlaylistService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")

	create := func(name string, ids ...string) *Playlist {
		p, err := ps.Create(name, "", ids, false)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return p
	}

	// An empty main playlist falls back to the referenced playlist.
	main := create("main")
	fb := create("fb", m1.ID)
	if err := ps.SetFallback(main.ID, fb.ID); err != nil {
		t.Fatal(err)
	}
	items, used, err := ps.ResolveWithFallback(main.ID)
	if err != nil {
		t.Fatalf("resolve with fallback: %v", err)
	}
	if !used || len(items) != 1 || items[0].ID != m1.ID {
		t.Fatalf("expected fallback items with used=true, got used=%v items=%+v", used, items)
	}

	// A main playlist that resolves is used directly, fallback untouched.
	full := create("full", m2.ID)
	if err := ps.SetFallback(full.ID, fb.ID); err != nil {
		t.Fatal(err)
	}
	items, used, err = ps.ResolveWithFallback(full.ID)
	if err != nil {
		t.Fatalf("resolve full: %v", err)
	}
	if used || len(items) != 1 || items[0].ID != m2.ID {
		t.Fatalf("expected direct resolve with used=false, got used=%v items=%+v", used, items)
	}

	// A main playlist referencing a missing media falls back. The broken
	// reference is injected directly since the services never create one.
	broken := create("broken", m1.ID)
	if err := ps.SetFallback(broken.ID, fb.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *Data) error {
		for _, p := range d.Playlists {
			if p.ID == broken.ID {
				p.Items = append(p.Items, &PlaylistItem{MediaID: "gone"})
				return nil
			}
		}
		t.Fatal("broken playlist not found")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items, used, err = ps.ResolveWithFallback(broken.ID)
	if err != nil {
		t.Fatalf("resolve broken: %v", err)
	}
	if !used || len(items) != 1 || items[0].ID != m1.ID {
		t.Fatalf("expected fallback for broken main, got used=%v items=%+v", used, items)
	}

	// Two-level chain: main -> fb1 -> fb2.
	fb1 := create("fb1")
	fb2 := create("fb2", m1.ID, m2.ID)
	if err := ps.SetFallback(main.ID, fb1.ID); err != nil {
		t.Fatal(err)
	}
	if err := ps.SetFallback(fb1.ID, fb2.ID); err != nil {
		t.Fatal(err)
	}
	items, used, err = ps.ResolveWithFallback(main.ID)
	if err != nil {
		t.Fatalf("resolve two-level chain: %v", err)
	}
	if !used || len(items) != 2 || items[0].ID != m1.ID || items[1].ID != m2.ID {
		t.Fatalf("unexpected two-level fallback result: used=%v items=%+v", used, items)
	}

	// A fallback cycle is detected and reported as invalid. The cycle is only
	// entered when every playlist on it fails to resolve, so it uses empty
	// playlists.
	c1 := create("c1")
	c2 := create("c2")
	if err := ps.SetFallback(c1.ID, c2.ID); err != nil {
		t.Fatal(err)
	}
	if err := ps.SetFallback(c2.ID, c1.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ps.ResolveWithFallback(c1.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for fallback cycle, got %v", err)
	}

	// Without a fallback, the original error is reported.
	if err := ps.SetFallback(broken.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, _, fallbackErr := ps.ResolveWithFallback(broken.ID)
	_, directErr := ps.Resolve(broken.ID)
	if fallbackErr == nil || directErr == nil || fallbackErr.Error() != directErr.Error() {
		t.Fatalf("expected the original resolve error %v, got %v", directErr, fallbackErr)
	}

	// An empty main without fallback resolves to an empty result, no error.
	empty := create("empty")
	items, used, err = ps.ResolveWithFallback(empty.ID)
	if err != nil {
		t.Fatalf("resolve empty without fallback: %v", err)
	}
	if used || len(items) != 0 {
		t.Fatalf("expected empty direct result with used=false, got used=%v items=%+v", used, items)
	}

	// A missing playlist reports ErrNotFound (it carries no fallback ref).
	if _, _, err := ps.ResolveWithFallback("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing playlist, got %v", err)
	}

	// Depth limit: more than maxFallbackDepth hops is invalid.
	chain := make([]*Playlist, 0, maxFallbackDepth+2)
	for i := 0; i <= maxFallbackDepth+1; i++ {
		chain = append(chain, create(fmt.Sprintf("d%d", i)))
	}
	for i := 0; i+1 < len(chain); i++ {
		if err := ps.SetFallback(chain[i].ID, chain[i+1].ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ps.ResolveWithFallback(chain[0].ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for deep fallback chain, got %v", err)
	}
	// Exactly maxFallbackDepth hops is allowed when the last playlist resolves.
	if err := ps.SetItems(chain[maxFallbackDepth].ID, []string{m1.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ps.SetFallback(chain[maxFallbackDepth].ID, ""); err != nil {
		t.Fatal(err)
	}
	items, used, err = ps.ResolveWithFallback(chain[0].ID)
	if err != nil {
		t.Fatalf("resolve max-depth chain: %v", err)
	}
	if !used || len(items) != 1 || items[0].ID != m1.ID {
		t.Fatalf("unexpected max-depth result: used=%v items=%+v", used, items)
	}
}
