package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestConfigTemplateCRUD(t *testing.T) {
	s := newTestStore(t)
	cs := NewConfigTemplateService(s)

	body := json.RawMessage(`{"src": "/img/logo.png", "dur": "${dur}"}`)
	tpl, err := cs.Create(ConfigTemplateSpec{
		Name:    "watermark",
		Type:    "media",
		Body:    body,
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tpl.ID == "" {
		t.Fatal("expected generated id")
	}
	if tpl.Name != "watermark" || tpl.Type != "media" || !tpl.Enabled {
		t.Fatalf("unexpected template: %+v", tpl)
	}
	if !bytes.Equal(tpl.Body, body) {
		t.Fatalf("unexpected body: %s", tpl.Body)
	}

	got, err := cs.Get(tpl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "watermark" || got.Type != "media" {
		t.Fatalf("unexpected get: %+v", got)
	}

	// Update replaces name, type, body and the enabled flag (the spec's
	// zero Enabled shows full-replacement semantics).
	updBody := json.RawMessage(`{"kind": "title"}`)
	upd, err := cs.Update(tpl.ID, ConfigTemplateSpec{
		Name: "title-v2",
		Type: "task",
		Body: updBody,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "title-v2" || upd.Type != "task" || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if !bytes.Equal(upd.Body, updBody) {
		t.Fatalf("unexpected body after update: %s", upd.Body)
	}

	// SetEnabled toggles both ways.
	if err := cs.SetEnabled(tpl.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := cs.SetEnabled(tpl.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = cs.Get(tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected template disabled after SetEnabled(false)")
	}

	if err := cs.Delete(tpl.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(cs.List()) != 0 {
		t.Fatal("expected empty template list")
	}
	if _, err := cs.Get(tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := cs.Delete(tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestConfigTemplateValidation(t *testing.T) {
	s := newTestStore(t)
	cs := NewConfigTemplateService(s)

	// empty (and whitespace-only) names are rejected
	for _, name := range []string{"", "   "} {
		if _, err := cs.Create(ConfigTemplateSpec{Name: name, Type: "media", Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for name %q, got %v", name, err)
		}
	}
	// empty (and whitespace-only) types are rejected
	for _, typ := range []string{"", "   "} {
		if _, err := cs.Create(ConfigTemplateSpec{Name: "x", Type: typ, Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for type %q, got %v", typ, err)
		}
	}
	// invalid JSON bodies and top-level non-objects are rejected
	for _, body := range []string{"", "{", `{"a":}`, "[1, 2]", `"str"`, "42", "null", "true"} {
		if _, err := cs.Create(ConfigTemplateSpec{Name: "x", Type: "media", Body: json.RawMessage(body)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for body %q, got %v", body, err)
		}
	}
	// an empty JSON object is a valid body
	if _, err := cs.Create(ConfigTemplateSpec{Name: "empty", Type: "media", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("create with empty object body: %v", err)
	}

	// duplicate name on create
	if _, err := cs.Create(ConfigTemplateSpec{Name: "dup", Type: "media", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := cs.Create(ConfigTemplateSpec{Name: "dup", Type: "task", Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// update validates like create
	tpl, err := cs.Create(ConfigTemplateSpec{Name: "u", Type: "media", Body: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := cs.Update(tpl.ID, ConfigTemplateSpec{Name: "  ", Type: "media", Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}
	if _, err := cs.Update(tpl.ID, ConfigTemplateSpec{Name: "u", Type: " ", Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update type, got %v", err)
	}
	if _, err := cs.Update(tpl.ID, ConfigTemplateSpec{Name: "u", Type: "media", Body: json.RawMessage("[1]")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for non-object update body, got %v", err)
	}
	if _, err := cs.Update(tpl.ID, ConfigTemplateSpec{Name: "dup", Type: "media", Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}
	// renaming to its own current name is fine
	if _, err := cs.Update(tpl.ID, ConfigTemplateSpec{Name: "u", Type: "media", Body: json.RawMessage(`{"a": 1}`)}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
	// missing id on every mutator
	if _, err := cs.Update("missing", ConfigTemplateSpec{Name: "u", Type: "media", Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing update, got %v", err)
	}
	if err := cs.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing set-enabled, got %v", err)
	}
	if err := cs.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestConfigTemplateListSorted(t *testing.T) {
	s := newTestStore(t)
	cs := NewConfigTemplateService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := cs.Create(ConfigTemplateSpec{Name: name, Type: "media", Body: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := cs.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d templates, got %d", len(want), len(list))
	}
	for i, tpl := range list {
		if tpl.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, configTemplateNames(list))
		}
	}
}

// TestConfigTemplateExpand verifies placeholder substitution: single,
// multiple and repeated keys are all replaced, a body without placeholders
// comes back unchanged, and values are injected literally as JSON
// fragments. A placeholder inside a JSON string takes the bare string
// content; a placeholder in value position (the embedded object fragment
// row) takes a complete JSON value. Note that a value-position placeholder
// makes the source body invalid JSON, so Create rejects such bodies: that
// pattern is only reachable through direct construction or a hand-edited
// store, and Expand validates the expanded result, not the source.
func TestConfigTemplateExpand(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		params map[string]string
		want   string
	}{
		{
			name:   "single placeholder",
			body:   `{"title": "${title}"}`,
			params: map[string]string{"title": "KPlayer"},
			want:   `{"title": "KPlayer"}`,
		},
		{
			name:   "multiple and repeated placeholders",
			body:   `{"label": "${x}", "alt": "${x}", "dur": "${dur}"}`,
			params: map[string]string{"x": "same", "dur": "30"},
			want:   `{"label": "same", "alt": "same", "dur": "30"}`,
		},
		{
			name:   "no placeholders",
			body:   `{"a": 1, "b": [2, 3]}`,
			params: map[string]string{"a": "99"},
			want:   `{"a": 1, "b": [2, 3]}`,
		},
		{
			name:   "embedded object fragment",
			body:   `{"scene": ${scene}}`,
			params: map[string]string{"scene": `{"kind": "logo", "opts": {"x": 1}}`},
			want:   `{"scene": {"kind": "logo", "opts": {"x": 1}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Expand(&ConfigTemplate{Body: json.RawMessage(tt.body)}, tt.params)
			if err != nil {
				t.Fatalf("expand: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("expanded %q, want %q", got, tt.want)
			}
			if !json.Valid(got) {
				t.Fatalf("expanded body is not valid JSON: %q", got)
			}
		})
	}
}

// TestConfigTemplateExpandSpecialCharacters verifies that a value holding
// JSON special characters (quotes, backslashes) is injected literally and
// still yields a valid document: the placeholder sits inside a JSON
// string, so the value is the string content with the characters escaped
// as JSON requires, and it decodes back to the intended text. The
// boundary is documented in TestConfigTemplateExpandUnsafeValueRejected: a
// raw unescaped value would break the JSON instead.
func TestConfigTemplateExpandSpecialCharacters(t *testing.T) {
	got, err := Expand(&ConfigTemplate{Body: json.RawMessage(`{"text": "${text}", "path": "${path}"}`)}, map[string]string{
		"text": `quoted \"inner\" text`,
		"path": `C:\\v\\a.mp4`,
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	var out struct {
		Text string `json:"text"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("expanded body does not decode: %v (%s)", err, got)
	}
	if out.Text != `quoted "inner" text` {
		t.Fatalf("unexpected text %q", out.Text)
	}
	if out.Path != `C:\v\a.mp4` {
		t.Fatalf("unexpected path %q", out.Path)
	}
}

// TestConfigTemplateExpandMissingParameter verifies that a placeholder
// without a matching parameter fails with ErrInvalid and names the key.
func TestConfigTemplateExpandMissingParameter(t *testing.T) {
	body := json.RawMessage(`{"a": "${x}", "b": "${y}"}`)
	for _, tt := range []struct {
		params  map[string]string
		missing string
	}{
		{map[string]string{}, "x"},
		{map[string]string{"y": `"1"`}, "x"},
		{map[string]string{"x": `"1"`, "z": `"2"`}, "y"},
	} {
		_, err := Expand(&ConfigTemplate{Body: body}, tt.params)
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for missing parameter, got %v", err)
		}
		if !strings.Contains(err.Error(), "missing parameter") || !strings.Contains(err.Error(), `"`+tt.missing+`"`) {
			t.Fatalf("expected missing-parameter error naming %q, got %v", tt.missing, err)
		}
	}
}

// TestConfigTemplateExpandUnsafeValueRejected documents the injection
// boundary: values are spliced into the body literally, so a value that is
// not a valid JSON fragment (here an unescaped quote inside a string
// position) produces an invalid document, which is rejected with
// ErrInvalid. Consumers must provide fragments that keep the expanded body
// valid JSON, as in TestConfigTemplateExpandSpecialCharacters.
func TestConfigTemplateExpandUnsafeValueRejected(t *testing.T) {
	_, err := Expand(&ConfigTemplate{Body: json.RawMessage(`{"text": "${text}"}`)}, map[string]string{
		"text": `quoted "inner" text`,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unsafe value, got %v", err)
	}
}

// TestConfigTemplateExpandDeepCopy verifies that the returned body never
// aliases the template's stored body, so mutating an expansion cannot
// corrupt the template: both the placeholder-free path (which returns a
// copy by design) and the substitution path are covered.
func TestConfigTemplateExpandDeepCopy(t *testing.T) {
	tpl := &ConfigTemplate{Body: json.RawMessage(`{"a": 1}`)}
	got, err := Expand(tpl, map[string]string{"a": "9"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	got[0] = 'x'
	if !bytes.Equal(tpl.Body, json.RawMessage(`{"a": 1}`)) {
		t.Fatalf("mutating the expansion mutated the template body: %s", tpl.Body)
	}
	if string(got) != `x"a": 1}` {
		t.Fatalf("unexpected expansion after mutation: %q", got)
	}

	tpl2 := &ConfigTemplate{Body: json.RawMessage(`{"a": "${x}"}`)}
	got2, err := Expand(tpl2, map[string]string{"x": "1"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	got2[0] = 'x'
	if !bytes.Equal(tpl2.Body, json.RawMessage(`{"a": "${x}"}`)) {
		t.Fatalf("mutating the expansion mutated the template body: %s", tpl2.Body)
	}
	if string(got2) != `x"a": "1"}` {
		t.Fatalf("unexpected expansion after mutation: %q", got2)
	}
}

// TestConfigTemplateRoundTrip verifies templates written through the
// service survive a close/reopen cycle with name, type, enabled and the
// body intact (camelCase JSON round trip).
func TestConfigTemplateRoundTrip(t *testing.T) {
	s := newTestStore(t)
	cs := NewConfigTemplateService(s)

	body := json.RawMessage(`{"logo": {"src": "/img/logo.png", "opacity": "${opacity}"}}`)
	tpl, err := cs.Create(ConfigTemplateSpec{
		Name:    "watermark",
		Type:    "media",
		Body:    body,
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := NewConfigTemplateService(reopened).Get(tpl.ID)
	if err != nil {
		t.Fatalf("template lost after reopen: %v", err)
	}
	if got.Name != "watermark" || got.Type != "media" || !got.Enabled {
		t.Fatalf("unexpected template after reopen: %+v", got)
	}
	// The store writes with json.MarshalIndent, which re-indents
	// RawMessage content, so compare the compacted forms.
	if compactJSON(t, got.Body) != compactJSON(t, body) {
		t.Fatalf("body lost after reopen: %s", got.Body)
	}
}

// compactJSON returns the compact form of raw for byte comparison.
func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact body %s: %v", raw, err)
	}
	return buf.String()
}

// configTemplateNames returns the names of the templates in order.
func configTemplateNames(templates []*ConfigTemplate) []string {
	out := make([]string, 0, len(templates))
	for _, t := range templates {
		out = append(out, t.Name)
	}
	return out
}
