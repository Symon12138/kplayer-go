package management

import (
	"errors"
	"testing"
)

func TestSceneTemplateCRUD(t *testing.T) {
	s := newTestStore(t)
	ts := NewSceneTemplateService(s)

	tpl, err := ts.Create(SceneTemplateSpec{
		Name:    "main",
		Kind:    SceneWatermark,
		Params:  map[string]string{"text": "KPlayer", "size": "24"},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tpl.ID == "" {
		t.Fatal("expected generated id")
	}
	if tpl.Name != "main" || tpl.Kind != SceneWatermark || !tpl.Enabled {
		t.Fatalf("unexpected template: %+v", tpl)
	}
	if !sameParams(tpl.Params, map[string]string{"text": "KPlayer", "size": "24"}) {
		t.Fatalf("unexpected params: %v", tpl.Params)
	}

	got, err := ts.Get(tpl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "main" || got.Kind != SceneWatermark {
		t.Fatalf("unexpected get: %+v", got)
	}

	// Update replaces name, kind, params and the enabled flag (the spec's
	// zero Enabled shows full-replacement semantics).
	upd, err := ts.Update(tpl.ID, SceneTemplateSpec{
		Name:   "intro-v2",
		Kind:   SceneIntro,
		Params: map[string]string{"src": "/img/intro.png"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "intro-v2" || upd.Kind != SceneIntro || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if !sameParams(upd.Params, map[string]string{"src": "/img/intro.png"}) {
		t.Fatalf("unexpected params after update: %v", upd.Params)
	}

	// SetEnabled toggles both ways.
	if err := ts.SetEnabled(tpl.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := ts.SetEnabled(tpl.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = ts.Get(tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected template disabled after SetEnabled(false)")
	}

	if err := ts.Delete(tpl.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(ts.List()) != 0 {
		t.Fatal("expected empty template list")
	}
	if _, err := ts.Get(tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := ts.Delete(tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestSceneTemplateValidation(t *testing.T) {
	s := newTestStore(t)
	ts := NewSceneTemplateService(s)

	// empty-name create is rejected
	if _, err := ts.Create(SceneTemplateSpec{Name: "  ", Kind: SceneLogo}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty name, got %v", err)
	}
	// unknown-kind create is rejected
	if _, err := ts.Create(SceneTemplateSpec{Name: "x", Kind: "fly"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown kind, got %v", err)
	}

	t1, err := ts.Create(SceneTemplateSpec{Name: "logo", Kind: SceneLogo})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ts.Create(SceneTemplateSpec{Name: "clock", Kind: SceneClock}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// duplicate-name create is rejected
	if _, err := ts.Create(SceneTemplateSpec{Name: "logo", Kind: SceneLogo}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// empty-name update is rejected
	if _, err := ts.Update(t1.ID, SceneTemplateSpec{Name: " ", Kind: SceneLogo}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}
	// unknown-kind update is rejected
	if _, err := ts.Update(t1.ID, SceneTemplateSpec{Name: "logo", Kind: "fly"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown update kind, got %v", err)
	}
	// rename onto an existing name is rejected
	if _, err := ts.Update(t1.ID, SceneTemplateSpec{Name: "clock", Kind: SceneLogo}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}
	// renaming to its own current name is fine
	if _, err := ts.Update(t1.ID, SceneTemplateSpec{Name: "logo", Kind: SceneLogo}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
}

func TestSceneTemplateDuplicate(t *testing.T) {
	s := newTestStore(t)
	ts := NewSceneTemplateService(s)

	src, err := ts.Create(SceneTemplateSpec{
		Name:    "intro",
		Kind:    SceneIntro,
		Params:  map[string]string{"src": "/img/a.png", "dur": "3"},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	dup, err := ts.Duplicate(src.ID)
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if dup.ID == src.ID || dup.ID == "" {
		t.Fatalf("expected a fresh id, got %q", dup.ID)
	}
	if dup.Name != "intro (copy)" {
		t.Fatalf("expected suffixed name, got %q", dup.Name)
	}
	if dup.Kind != src.Kind || !dup.Enabled {
		t.Fatalf("expected kind and enabled carried over, got %+v", dup)
	}
	if !sameParams(dup.Params, src.Params) {
		t.Fatalf("expected params carried over, got %v", dup.Params)
	}

	// the copy is independent: mutating the copy's params in place leaves
	// the source untouched, and vice versa
	dup.Params["dur"] = "9"
	dup.Params["extra"] = "x"
	got, err := ts.Get(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameParams(got.Params, map[string]string{"src": "/img/a.png", "dur": "3"}) {
		t.Fatalf("source params changed by copy mutation: %v", got.Params)
	}
	src.Params["dur"] = "7"
	got, err = ts.Get(dup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Params["dur"] != "9" {
		t.Fatalf("copy params changed by source mutation: %v", got.Params)
	}

	// toggling the copy through the service leaves the source enabled
	if err := ts.SetEnabled(dup.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = ts.Get(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Fatal("source template disabled by copy toggle")
	}

	// duplicating a missing template is ErrNotFound
	if _, err := ts.Duplicate("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// a second duplicate collides with the " (copy)" name
	if _, err := ts.Duplicate(src.ID); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding copy name, got %v", err)
	}
}

func TestSceneTemplateListSorted(t *testing.T) {
	s := newTestStore(t)
	ts := NewSceneTemplateService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := ts.Create(SceneTemplateSpec{Name: name, Kind: SceneLogo}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := ts.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d templates, got %d", len(want), len(list))
	}
	for i, tpl := range list {
		if tpl.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, templateNames(list))
		}
	}
}

// TestSceneTemplateParamsRoundTrip verifies templates written through the
// service survive a close/reopen cycle with their params intact (camelCase
// JSON round trip).
func TestSceneTemplateParamsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ts := NewSceneTemplateService(s)

	tpl, err := ts.Create(SceneTemplateSpec{
		Name:    "ticker",
		Kind:    SceneScroll,
		Params:  map[string]string{"text": "breaking", "speed": "2"},
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := NewSceneTemplateService(reopened).Get(tpl.ID)
	if err != nil {
		t.Fatalf("template lost after reopen: %v", err)
	}
	if got.Name != "ticker" || got.Kind != SceneScroll || !got.Enabled {
		t.Fatalf("unexpected template after reopen: %+v", got)
	}
	if !sameParams(got.Params, map[string]string{"text": "breaking", "speed": "2"}) {
		t.Fatalf("params lost after reopen: %v", got.Params)
	}
}

// sameParams reports whether a and b hold the same key/value pairs.
func sameParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// templateNames returns the names of the templates in order.
func templateNames(templates []*SceneTemplate) []string {
	out := make([]string, 0, len(templates))
	for _, t := range templates {
		out = append(out, t.Name)
	}
	return out
}

func TestResolveSceneTemplate(t *testing.T) {
	s := newTestStore(t)
	ts := NewSceneTemplateService(s)
	tpl, err := ts.Create(SceneTemplateSpec{Name: "logo", Kind: SceneLogo})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSceneTemplate(snapshot, tpl.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ID != tpl.ID || got.Name != "logo" {
		t.Fatalf("resolved %+v, want template %q", got, tpl.ID)
	}

	if _, err := ResolveSceneTemplate(snapshot, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing template, got %v", err)
	}
}
