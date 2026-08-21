package management

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIndustryTemplateCRUD(t *testing.T) {
	s := newTestStore(t)
	is := NewIndustryTemplateService(s)

	spec := IndustryTemplateSpec{
		Name:              "hospital",
		Description:       "hospital lobby feed",
		PlaylistName:      "hospital-feed",
		MediaPlaceholders: []string{"/v/welcome.mp4", "${logo}"},
		SceneKinds:        []SceneKind{SceneLogo, SceneClock},
		Task:              &IndustryTaskSpec{Name: "daily", Type: TaskTypeCron, Cron: "0 9 * * *", Enabled: true},
		Enabled:           true,
	}
	tpl, err := is.Create(spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tpl.ID == "" {
		t.Fatal("expected generated id")
	}
	if tpl.Name != "hospital" || tpl.Description != "hospital lobby feed" ||
		tpl.PlaylistName != "hospital-feed" || !tpl.Enabled {
		t.Fatalf("unexpected template: %+v", tpl)
	}
	if !sameStrings(tpl.MediaPlaceholders, []string{"/v/welcome.mp4", "${logo}"}) {
		t.Fatalf("unexpected media placeholders: %v", tpl.MediaPlaceholders)
	}
	if !sameSceneKinds(tpl.SceneKinds, []SceneKind{SceneLogo, SceneClock}) {
		t.Fatalf("unexpected scene kinds: %v", tpl.SceneKinds)
	}
	if tpl.Task == nil || tpl.Task.Name != "daily" || tpl.Task.Type != TaskTypeCron ||
		tpl.Task.Cron != "0 9 * * *" || !tpl.Task.Enabled {
		t.Fatalf("unexpected task: %+v", tpl.Task)
	}

	got, err := is.Get(tpl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "hospital" || got.PlaylistName != "hospital-feed" {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(is.List()) != 1 {
		t.Fatalf("expected 1 template, got %d", len(is.List()))
	}

	// The stored template is independent of the caller's spec: mutating the
	// spec's slices and task after create must not leak into the store.
	spec.MediaPlaceholders[0] = "mutated"
	spec.SceneKinds[0] = SceneTitle
	spec.Task.Interval = 999
	got, err = is.Get(tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(got.MediaPlaceholders, []string{"/v/welcome.mp4", "${logo}"}) {
		t.Fatalf("stored placeholders changed by spec mutation: %v", got.MediaPlaceholders)
	}
	if !sameSceneKinds(got.SceneKinds, []SceneKind{SceneLogo, SceneClock}) {
		t.Fatalf("stored kinds changed by spec mutation: %v", got.SceneKinds)
	}
	if got.Task.Interval != 0 || got.Task.Cron != "0 9 * * *" {
		t.Fatalf("stored task changed by spec mutation: %+v", got.Task)
	}

	// Update replaces everything: description and the enabled flag (the
	// zero values show full-replacement semantics) and the task (a nil task
	// clears it).
	upd, err := is.Update(tpl.ID, IndustryTemplateSpec{
		Name:              "hospital-v2",
		PlaylistName:      "hospital-feed-2",
		MediaPlaceholders: []string{"${intro}"},
		SceneKinds:        []SceneKind{SceneWatermark},
		Task:              &IndustryTaskSpec{Name: "shift", Type: TaskTypeInterval, Interval: 900},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "hospital-v2" || upd.Description != "" || upd.PlaylistName != "hospital-feed-2" || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if !sameStrings(upd.MediaPlaceholders, []string{"${intro}"}) {
		t.Fatalf("unexpected placeholders after update: %v", upd.MediaPlaceholders)
	}
	if !sameSceneKinds(upd.SceneKinds, []SceneKind{SceneWatermark}) {
		t.Fatalf("unexpected kinds after update: %v", upd.SceneKinds)
	}
	if upd.Task == nil || upd.Task.Name != "shift" || upd.Task.Type != TaskTypeInterval || upd.Task.Interval != 900 {
		t.Fatalf("unexpected task after update: %+v", upd.Task)
	}

	// A spec without a task clears the task definition (full replacement).
	upd, err = is.Update(tpl.ID, IndustryTemplateSpec{Name: "hospital-v3", PlaylistName: "feed"})
	if err != nil {
		t.Fatalf("update clearing task: %v", err)
	}
	if upd.Task != nil || upd.Enabled || len(upd.SceneKinds) != 0 {
		t.Fatalf("expected cleared task, kinds and disabled flag, got %+v", upd)
	}

	if err := is.Delete(tpl.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(is.List()) != 0 {
		t.Fatal("expected empty template list")
	}
	if _, err := is.Get(tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := is.Delete(tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestIndustryTemplateValidation(t *testing.T) {
	s := newTestStore(t)
	is := NewIndustryTemplateService(s)

	// empty-name create is rejected
	if _, err := is.Create(IndustryTemplateSpec{Name: "  ", PlaylistName: "feed"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty name, got %v", err)
	}
	// empty playlist name is rejected
	if _, err := is.Create(IndustryTemplateSpec{Name: "x", PlaylistName: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty playlist name, got %v", err)
	}
	// unknown scene kind is rejected
	if _, err := is.Create(IndustryTemplateSpec{Name: "x", PlaylistName: "feed", SceneKinds: []SceneKind{"fly"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown scene kind, got %v", err)
	}

	taskCases := []struct {
		name string
		task *IndustryTaskSpec
	}{
		{"unknown type", &IndustryTaskSpec{Name: "t", Type: "bogus"}},
		{"empty task name", &IndustryTaskSpec{Name: " ", Type: TaskTypeInterval, Interval: 10}},
		{"non-positive interval", &IndustryTaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 0}},
		{"bad cron", &IndustryTaskSpec{Name: "t", Type: TaskTypeCron, Cron: "not a cron"}},
	}
	for _, c := range taskCases {
		spec := IndustryTemplateSpec{Name: "x", PlaylistName: "feed"}
		spec.Task = c.task
		if _, err := is.Create(spec); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", c.name, err)
		}
	}

	// a valid interval task is accepted
	if _, err := is.Create(IndustryTemplateSpec{
		Name:         "x2",
		PlaylistName: "feed",
		Task:         &IndustryTaskSpec{Name: "t", Type: TaskTypeInterval, Interval: 30},
	}); err != nil {
		t.Fatalf("valid interval task rejected: %v", err)
	}

	t1, err := is.Create(IndustryTemplateSpec{Name: "retail", PlaylistName: "retail-feed"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// duplicate-name create is rejected
	if _, err := is.Create(IndustryTemplateSpec{Name: "retail", PlaylistName: "other"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// update validation mirrors create
	if _, err := is.Update(t1.ID, IndustryTemplateSpec{Name: " ", PlaylistName: "retail-feed"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}
	if _, err := is.Update(t1.ID, IndustryTemplateSpec{Name: "retail", PlaylistName: ""}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update playlist name, got %v", err)
	}
	if _, err := is.Update(t1.ID, IndustryTemplateSpec{Name: "retail", PlaylistName: "retail-feed", SceneKinds: []SceneKind{"fly"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown kind on update, got %v", err)
	}
	badTask := IndustryTemplateSpec{Name: "retail", PlaylistName: "retail-feed", Task: &IndustryTaskSpec{Name: "t", Type: "bogus"}}
	if _, err := is.Update(t1.ID, badTask); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for bad task on update, got %v", err)
	}
	// rename onto an existing name is rejected ("x2" was created by the
	// valid interval task case above)
	if _, err := is.Update(t1.ID, IndustryTemplateSpec{Name: "x2", PlaylistName: "retail-feed"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}
	// renaming to its own current name is fine
	if _, err := is.Update(t1.ID, IndustryTemplateSpec{Name: "retail", PlaylistName: "retail-feed"}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
	// update of a missing template is ErrNotFound
	if _, err := is.Update("missing", IndustryTemplateSpec{Name: "m", PlaylistName: "feed"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing update, got %v", err)
	}
}

func TestIndustryTemplateSetEnabled(t *testing.T) {
	s := newTestStore(t)
	is := NewIndustryTemplateService(s)

	tpl, err := is.Create(IndustryTemplateSpec{Name: "edu", PlaylistName: "edu-feed", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := is.SetEnabled(tpl.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err := is.Get(tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected template disabled after SetEnabled(false)")
	}
	if err := is.SetEnabled(tpl.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = is.Get(tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Fatal("expected template enabled after SetEnabled(true)")
	}
	if err := is.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing template, got %v", err)
	}
}

func TestIndustryTemplateListSorted(t *testing.T) {
	s := newTestStore(t)
	is := NewIndustryTemplateService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := is.Create(IndustryTemplateSpec{Name: name, PlaylistName: name + "-feed"}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := is.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d templates, got %d", len(want), len(list))
	}
	for i, tpl := range list {
		if tpl.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, industryTemplateNames(list))
		}
	}
}

// TestIndustryTemplatePersistsReopen verifies templates written through the
// service survive a close/reopen cycle with every field intact (camelCase
// JSON round trip), and that the document carries the agreed
// "industryTemplates" key.
func TestIndustryTemplatePersistsReopen(t *testing.T) {
	s := newTestStore(t)
	is := NewIndustryTemplateService(s)

	tpl, err := is.Create(IndustryTemplateSpec{
		Name:              "hospital",
		Description:       "lobby feed",
		PlaylistName:      "hospital-feed",
		MediaPlaceholders: []string{"/v/welcome.mp4", "${logo}"},
		SceneKinds:        []SceneKind{SceneLogo, SceneClock},
		Task:              &IndustryTaskSpec{Name: "daily", Type: TaskTypeCron, Cron: "0 9 * * *", Enabled: true},
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"industryTemplates"`)) {
		t.Fatalf("store file lacks the industryTemplates key: %s", raw)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := NewIndustryTemplateService(reopened).Get(tpl.ID)
	if err != nil {
		t.Fatalf("template lost after reopen: %v", err)
	}
	if got.Name != "hospital" || got.Description != "lobby feed" ||
		got.PlaylistName != "hospital-feed" || !got.Enabled {
		t.Fatalf("unexpected template after reopen: %+v", got)
	}
	if !sameStrings(got.MediaPlaceholders, []string{"/v/welcome.mp4", "${logo}"}) {
		t.Fatalf("placeholders lost after reopen: %v", got.MediaPlaceholders)
	}
	if !sameSceneKinds(got.SceneKinds, []SceneKind{SceneLogo, SceneClock}) {
		t.Fatalf("scene kinds lost after reopen: %v", got.SceneKinds)
	}
	if got.Task == nil || got.Task.Name != "daily" || got.Task.Type != TaskTypeCron ||
		got.Task.Cron != "0 9 * * *" || !got.Task.Enabled {
		t.Fatalf("task lost after reopen: %+v", got.Task)
	}
}

// TestIndustryTemplateLegacyStoreCompatible verifies that a store file
// written before the industry template collection existed (no
// industryTemplates key) still opens and serves the service: the absent key
// leaves the field nil instead of failing, and templates created after the
// upgrade persist and reopen normally.
func TestIndustryTemplateLegacyStoreCompatible(t *testing.T) {
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
	if snap.IndustryTemplates != nil {
		t.Fatalf("expected IndustryTemplates nil after opening a legacy file, got %v", snap.IndustryTemplates)
	}
	is := NewIndustryTemplateService(s)
	if len(is.List()) != 0 {
		t.Fatal("expected no industry templates in a legacy store")
	}

	// templates created after the upgrade persist and reopen fine
	tpl, err := is.Create(IndustryTemplateSpec{
		Name:         "retail",
		PlaylistName: "retail-feed",
		SceneKinds:   []SceneKind{SceneBackground},
	})
	if err != nil {
		t.Fatalf("create after upgrade: %v", err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewIndustryTemplateService(reopened).Get(tpl.ID)
	if err != nil {
		t.Fatalf("template lost after upgrade reopen: %v", err)
	}
	if got.Name != "retail" || !sameSceneKinds(got.SceneKinds, []SceneKind{SceneBackground}) {
		t.Fatalf("unexpected template after upgrade reopen: %+v", got)
	}
}

// sameSceneKinds reports whether a and b hold the same kinds in order.
func sameSceneKinds(a, b []SceneKind) bool {
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

// industryTemplateNames returns the names of the templates in order.
func industryTemplateNames(templates []*IndustryTemplate) []string {
	out := make([]string, 0, len(templates))
	for _, t := range templates {
		out = append(out, t.Name)
	}
	return out
}
