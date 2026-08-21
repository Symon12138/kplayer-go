package management

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPriorityRank(t *testing.T) {
	cases := []struct {
		p    Priority
		rank int
	}{
		{"", 0},
		{PriorityNormal, 0},
		{PriorityImportant, 1},
		{PriorityCritical, 2},
		{"bogus", 0}, // unknown values rank as normal defensively
	}
	for _, c := range cases {
		if got := c.p.rank(); got != c.rank {
			t.Errorf("rank(%q) = %d, want %d", c.p, got, c.rank)
		}
	}
}

func TestNormalizePriority(t *testing.T) {
	if got := normalizePriority(""); got != PriorityNormal {
		t.Fatalf("normalizePriority(\"\") = %q, want normal", got)
	}
	if got := normalizePriority(PriorityCritical); got != PriorityCritical {
		t.Fatalf("normalizePriority(critical) = %q, want critical", got)
	}
}

func TestPlayRequestJSON(t *testing.T) {
	raw, err := json.Marshal(PlayRequest{MediaID: "m1", Priority: PriorityImportant})
	if err != nil {
		t.Fatal(err)
	}
	var back PlayRequest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Priority != PriorityImportant || back.MediaID != "m1" {
		t.Fatalf("unexpected round-trip: %+v", back)
	}

	// The zero priority is omitted and unmarshals to the normal-ranking "".
	raw, err = json.Marshal(PlayRequest{MediaID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("priority")) {
		t.Fatalf("expected zero priority omitted, got %s", raw)
	}
	var zero PlayRequest
	if err := json.Unmarshal(raw, &zero); err != nil {
		t.Fatal(err)
	}
	if zero.Priority != "" {
		t.Fatalf("expected zero priority after round-trip, got %q", zero.Priority)
	}
}

func TestPlayRequestSceneTemplateJSON(t *testing.T) {
	raw, err := json.Marshal(PlayRequest{MediaID: "m1", SceneTemplateID: "tpl1"})
	if err != nil {
		t.Fatal(err)
	}
	var back PlayRequest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.SceneTemplateID != "tpl1" || back.MediaID != "m1" {
		t.Fatalf("unexpected round-trip: %+v", back)
	}

	// The empty template id is omitted from the wire and unmarshals back to
	// "" (no template).
	raw, err = json.Marshal(PlayRequest{MediaID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("sceneTemplateId")) {
		t.Fatalf("expected empty template id omitted, got %s", raw)
	}
	var zero PlayRequest
	if err := json.Unmarshal(raw, &zero); err != nil {
		t.Fatal(err)
	}
	if zero.SceneTemplateID != "" {
		t.Fatalf("expected empty template id after round-trip, got %q", zero.SceneTemplateID)
	}
}
