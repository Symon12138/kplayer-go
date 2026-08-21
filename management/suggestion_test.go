package management

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSuggestionCRUD(t *testing.T) {
	s := newTestStore(t)
	ss := NewSuggestionService(s)

	// a fresh store has no suggestions
	if len(ss.List()) != 0 {
		t.Fatal("expected an empty suggestion list")
	}
	if _, err := ss.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing get, got %v", err)
	}

	// Create assigns an id and timestamps and defaults the status to pending
	sug, err := ss.Create(SuggestionMediaRecommend, "play this", map[string]string{"media_id": "m1", "playlist_name": "morning"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sug.ID == "" {
		t.Fatal("expected generated id")
	}
	if sug.Kind != SuggestionMediaRecommend || sug.Title != "play this" || sug.Status != SuggestionPending {
		t.Fatalf("unexpected suggestion: %+v", sug)
	}
	if sug.Payload["media_id"] != "m1" || sug.Payload["playlist_name"] != "morning" {
		t.Fatalf("payload not preserved: %+v", sug.Payload)
	}
	if sug.CreatedAt.IsZero() || sug.UpdatedAt.IsZero() {
		t.Fatal("expected created/updated timestamps")
	}
	if sug.AppliedAt != nil || sug.Reason != "" {
		t.Fatalf("unexpected applied timestamp or reason on a fresh suggestion: %+v", sug)
	}

	// the title is optional
	plain, err := ss.Create(SuggestionTitleGenerate, "", map[string]string{"playlist_name": "night"})
	if err != nil {
		t.Fatalf("create without title: %v", err)
	}
	if plain.Title != "" || plain.Kind != SuggestionTitleGenerate {
		t.Fatalf("unexpected suggestion: %+v", plain)
	}

	// the payload map is copied: mutating the caller's map afterwards does
	// not leak into the store
	payload := map[string]string{"media_id": "m2"}
	leak, err := ss.Create(SuggestionMediaRecommend, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	payload["media_id"] = "mutated"
	got, err := ss.Get(leak.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Payload["media_id"] != "m2" {
		t.Fatalf("payload mutation leaked into the store: %+v", got.Payload)
	}

	// Get returns the stored suggestion
	got, err = ss.Get(sug.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "play this" || got.Status != SuggestionPending {
		t.Fatalf("unexpected get: %+v", got)
	}
	if len(ss.List()) != 3 {
		t.Fatalf("expected 3 suggestions, got %d", len(ss.List()))
	}
}

func TestSuggestionValidation(t *testing.T) {
	s := newTestStore(t)
	ss := NewSuggestionService(s)

	// an unknown kind, including the empty string, is rejected
	for _, kind := range []SuggestionKind{"", "bogus", "ai_magic"} {
		if _, err := ss.Create(kind, "t", map[string]string{"media_id": "m1"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("kind %q: expected ErrInvalid, got %v", kind, err)
		}
	}
	// a payload with no key-value pair is rejected
	if _, err := ss.Create(SuggestionMediaRecommend, "t", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for nil payload, got %v", err)
	}
	if _, err := ss.Create(SuggestionMediaRecommend, "t", map[string]string{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty payload, got %v", err)
	}

	// the enum validators accept the constants and reject anything else
	for _, kind := range []SuggestionKind{SuggestionMediaRecommend, SuggestionTitleGenerate} {
		if err := validateSuggestionKind(kind); err != nil {
			t.Fatalf("kind %q rejected: %v", kind, err)
		}
	}
	for _, st := range []SuggestionStatus{SuggestionPending, SuggestionApplied, SuggestionRejected} {
		if err := validateSuggestionStatus(st); err != nil {
			t.Fatalf("status %q rejected: %v", st, err)
		}
	}
	if err := validateSuggestionKind("nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown kind, got %v", err)
	}
	if err := validateSuggestionStatus(SuggestionStatus("nope")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown status, got %v", err)
	}

	// nothing was persisted by rejected inputs
	if len(ss.List()) != 0 {
		t.Fatal("expected no suggestions from rejected inputs")
	}
}

// TestSuggestionListOrder verifies the List ordering: CreatedAt descending,
// with the ID breaking ties. The entries are hand-seeded with explicit
// timestamps because Create stamps the current time.
func TestSuggestionListOrder(t *testing.T) {
	s := newTestStore(t)
	ss := NewSuggestionService(s)

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seed := []*Suggestion{
		{ID: "s-old", Kind: SuggestionMediaRecommend, Status: SuggestionPending, CreatedAt: base, UpdatedAt: base},
		{ID: "s-new", Kind: SuggestionMediaRecommend, Status: SuggestionPending, CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)},
		{ID: "s-tie-b", Kind: SuggestionMediaRecommend, Status: SuggestionPending, CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)},
		{ID: "s-tie-a", Kind: SuggestionMediaRecommend, Status: SuggestionPending, CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)},
	}
	if err := s.Update(func(d *Data) error {
		d.Suggestions = append(d.Suggestions, seed...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got := ss.List()
	if len(got) != 4 {
		t.Fatalf("expected 4 suggestions, got %d", len(got))
	}
	// newest first; the two sharing a timestamp are ordered by id ascending
	want := []string{"s-new", "s-tie-a", "s-tie-b", "s-old"}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("unexpected order: got %v, want %v", suggestionIDs(got), want)
		}
	}
}

// TestSuggestionApprove covers the happy path of the review state machine:
// approving a media recommendation creates a playlist through
// PlaylistService semantics, writes the new playlist id back into the
// payload and flips the status to applied with an applied timestamp.
func TestSuggestionApprove(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	m2 := mustAddMedia(t, ms, "/v/2.mp4")
	ss := NewSuggestionService(s)

	sug, err := ss.Create(SuggestionMediaRecommend, "play this", map[string]string{"media_id": m1.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sug.Status != SuggestionPending {
		t.Fatalf("expected pending before approve, got %q", sug.Status)
	}

	before := time.Now()
	approved, err := ss.Approve(sug.ID, "recommended morning")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.ID != sug.ID {
		t.Fatalf("approve returned the wrong suggestion: %+v", approved)
	}
	if approved.Status != SuggestionApplied {
		t.Fatalf("expected applied status, got %q", approved.Status)
	}
	if approved.AppliedAt == nil || approved.AppliedAt.IsZero() {
		t.Fatal("expected AppliedAt to be recorded")
	}
	if approved.AppliedAt.Before(before) || approved.AppliedAt.After(time.Now()) {
		t.Fatalf("unexpected AppliedAt: %v", approved.AppliedAt)
	}
	if approved.Payload["playlist_id"] == "" {
		t.Fatal("expected the new playlist id to be written back into the payload")
	}
	if approved.Payload["media_id"] != m1.ID {
		t.Fatalf("original payload not preserved: %+v", approved.Payload)
	}

	// the recommended media was persisted as a playlist with the given name
	ps := NewPlaylistService(s)
	pl, err := ps.Get(approved.Payload["playlist_id"])
	if err != nil {
		t.Fatalf("get created playlist: %v", err)
	}
	if pl.Name != "recommended morning" || pl.Desc != "" || pl.Loop {
		t.Fatalf("unexpected playlist: %+v", pl)
	}
	if !sameStrings(pl.MediaIDs(), []string{m1.ID}) {
		t.Fatalf("unexpected playlist items: %v", pl.MediaIDs())
	}

	// the store reflects the applied state, stable across reads
	got, err := ss.Get(sug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SuggestionApplied || got.AppliedAt == nil || !got.AppliedAt.Equal(*approved.AppliedAt) {
		t.Fatalf("unexpected stored suggestion: %+v", got)
	}
	if got.Payload["playlist_id"] != pl.ID {
		t.Fatalf("playlist id not persisted: %+v", got.Payload)
	}

	// a second media recommendation approves independently
	sug2, err := ss.Create(SuggestionMediaRecommend, "", map[string]string{"media_id": m2.ID})
	if err != nil {
		t.Fatal(err)
	}
	approved2, err := ss.Approve(sug2.ID, "second")
	if err != nil {
		t.Fatalf("approve second: %v", err)
	}
	if approved2.Payload["playlist_id"] == approved.Payload["playlist_id"] {
		t.Fatal("each approval must create its own playlist")
	}
	if len(ps.List()) != 2 {
		t.Fatalf("expected 2 playlists, got %d", len(ps.List()))
	}
}

// TestSuggestionApproveErrors verifies that every failed approval leaves the
// suggestion pending: an empty playlist name, a payload without media_id, a
// missing media and a non-pending status are all rejected and nothing is
// persisted.
func TestSuggestionApproveErrors(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	ss := NewSuggestionService(s)

	// an empty playlist name is rejected
	sug, err := ss.Create(SuggestionMediaRecommend, "t", map[string]string{"media_id": m1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Approve(sug.ID, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty playlist name, got %v", err)
	}
	assertSuggestionPending(t, ss, sug.ID)

	// a payload without media_id is rejected
	sug, err = ss.Create(SuggestionMediaRecommend, "t", map[string]string{"playlist_name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Approve(sug.ID, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for missing media_id, got %v", err)
	}
	assertSuggestionPending(t, ss, sug.ID)

	// a blank media_id is rejected the same way
	sug, err = ss.Create(SuggestionMediaRecommend, "t", map[string]string{"media_id": "  "})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Approve(sug.ID, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for blank media_id, got %v", err)
	}
	assertSuggestionPending(t, ss, sug.ID)

	// a media id that does not exist in the library is rejected with
	// ErrNotFound, through PlaylistService.Create semantics
	sug, err = ss.Create(SuggestionMediaRecommend, "t", map[string]string{"media_id": "missing-media"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Approve(sug.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing media, got %v", err)
	}
	assertSuggestionPending(t, ss, sug.ID)

	// none of the failed approvals created a playlist
	if len(NewPlaylistService(s).List()) != 0 {
		t.Fatal("failed approvals must not persist playlists")
	}

	// a suggestion that is no longer pending cannot be approved
	ok, err := ss.Create(SuggestionMediaRecommend, "t", map[string]string{"media_id": m1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Approve(ok.ID, "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := ss.Approve(ok.ID, "again"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for re-approve, got %v", err)
	} else if !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("expected a 'not pending' error, got %v", err)
	}
	// the failed re-approve created no second playlist
	if len(NewPlaylistService(s).List()) != 1 {
		t.Fatalf("re-approve must not create another playlist, got %d", len(NewPlaylistService(s).List()))
	}
	rej, err := ss.Create(SuggestionMediaRecommend, "t", map[string]string{"media_id": m1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Reject(rej.ID, "no"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := ss.Approve(rej.ID, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for approving a rejected suggestion, got %v", err)
	}

	// an unknown id is reported
	if _, err := ss.Approve("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing approve, got %v", err)
	}
}

// TestSuggestionReject verifies the reject decision: the reason is recorded
// (defaulting when blank), the status flips to rejected, and a suggestion
// that is no longer pending cannot be rejected.
func TestSuggestionReject(t *testing.T) {
	s := newTestStore(t)
	ss := NewSuggestionService(s)

	// an explicit reason is recorded
	sug, err := ss.Create(SuggestionTitleGenerate, "title", map[string]string{"playlist_name": "night"})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := ss.Reject(sug.ID, "not our style")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.Status != SuggestionRejected {
		t.Fatalf("expected rejected status, got %q", rejected.Status)
	}
	if rejected.Reason != "not our style" {
		t.Fatalf("unexpected reason %q", rejected.Reason)
	}
	if rejected.AppliedAt != nil {
		t.Fatal("a rejected suggestion must not carry an applied timestamp")
	}
	got, err := ss.Get(sug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SuggestionRejected || got.Reason != "not our style" {
		t.Fatalf("rejection not persisted: %+v", got)
	}

	// an empty or blank reason defaults
	plain, err := ss.Create(SuggestionTitleGenerate, "", map[string]string{"playlist_name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err = ss.Reject(plain.ID, "")
	if err != nil {
		t.Fatalf("reject with empty reason: %v", err)
	}
	if rejected.Reason != "rejected by operator" {
		t.Fatalf("expected the default reason, got %q", rejected.Reason)
	}
	blank, err := ss.Create(SuggestionTitleGenerate, "", map[string]string{"playlist_name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err = ss.Reject(blank.ID, "   ")
	if err != nil {
		t.Fatalf("reject with blank reason: %v", err)
	}
	if rejected.Reason != "rejected by operator" {
		t.Fatalf("expected the default reason for a blank reason, got %q", rejected.Reason)
	}

	// a suggestion that is no longer pending cannot be rejected: neither a
	// re-reject nor a reject of an applied suggestion
	if _, err := ss.Reject(sug.ID, "again"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for re-reject, got %v", err)
	}
	ms := NewMediaService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	applied, err := ss.Create(SuggestionMediaRecommend, "", map[string]string{"media_id": m1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Approve(applied.ID, "x"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := ss.Reject(applied.ID, "too late"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for rejecting an applied suggestion, got %v", err)
	}

	// an unknown id is reported
	if _, err := ss.Reject("nope", "why"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing reject, got %v", err)
	}
}

// TestSuggestionPersistence verifies that suggestions — including the review
// state, the applied timestamp and the payload written back on approval —
// survive a close/reopen cycle, together with the playlist the approval
// created, and are stored under the suggestions key with camelCase JSON keys.
func TestSuggestionPersistence(t *testing.T) {
	s := newTestStore(t)
	ms := NewMediaService(s)
	m1 := mustAddMedia(t, ms, "/v/1.mp4")
	ss := NewSuggestionService(s)

	sug, err := ss.Create(SuggestionMediaRecommend, "play this", map[string]string{"media_id": m1.ID})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := ss.Approve(sug.ID, "recommended")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	rejected, err := ss.Create(SuggestionTitleGenerate, "title", map[string]string{"playlist_name": "night"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Reject(rejected.ID, "no"); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, err := NewSuggestionService(reopened).Get(sug.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.ID != approved.ID || got.Kind != approved.Kind || got.Title != approved.Title ||
		got.Status != approved.Status || got.Reason != approved.Reason ||
		!got.CreatedAt.Equal(approved.CreatedAt) || !got.UpdatedAt.Equal(approved.UpdatedAt) ||
		got.AppliedAt == nil || !got.AppliedAt.Equal(*approved.AppliedAt) {
		t.Fatalf("suggestion changed after reopen: %+v", got)
	}
	if got.Payload["media_id"] != m1.ID || got.Payload["playlist_id"] != approved.Payload["playlist_id"] {
		t.Fatalf("payload changed after reopen: %+v", got.Payload)
	}
	gotRejected, err := NewSuggestionService(reopened).Get(rejected.ID)
	if err != nil {
		t.Fatalf("get rejected after reopen: %v", err)
	}
	if gotRejected.Status != SuggestionRejected || gotRejected.Reason != "no" || gotRejected.AppliedAt != nil {
		t.Fatalf("rejected suggestion changed after reopen: %+v", gotRejected)
	}

	// the playlist created by the approval survives too
	pl, err := NewPlaylistService(reopened).Get(approved.Payload["playlist_id"])
	if err != nil {
		t.Fatalf("playlist lost after reopen: %v", err)
	}
	if !sameStrings(pl.MediaIDs(), []string{m1.ID}) {
		t.Fatalf("unexpected playlist items after reopen: %v", pl.MediaIDs())
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"suggestions"`)) {
		t.Fatal("expected suggestions to be stored under the suggestions key")
	}
	for _, key := range []string{`"createdAt"`, `"appliedAt"`, `"updatedAt"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("expected camelCase key %s in stored document", key)
		}
	}
}

// TestSuggestionLegacyStoreCompatibility verifies that a store file written
// before the suggestions collection existed (no suggestions key) still opens
// and deserializes: the absent key leaves the new field nil instead of
// failing, and the legacy document keeps accepting new suggestions.
func TestSuggestionLegacyStoreCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	legacy := "{\n  \"media\": [],\n  \"playlists\": [],\n  \"updated_at\": \"2026-01-01T00:00:00Z\"\n}\n"
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
	if snap.Suggestions != nil {
		t.Fatalf("expected Suggestions to be nil after opening a legacy file, got %v", snap.Suggestions)
	}

	// the legacy document keeps working: new suggestions are accepted and
	// persisted
	ss := NewSuggestionService(s)
	sug, err := ss.Create(SuggestionMediaRecommend, "t", map[string]string{"media_id": "m1"})
	if err != nil {
		t.Fatalf("create on legacy store: %v", err)
	}
	if len(ss.List()) != 1 {
		t.Fatalf("expected 1 suggestion after create on legacy store, got %d", len(ss.List()))
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSuggestionService(reopened).Get(sug.ID); err != nil {
		t.Fatalf("suggestion on legacy store lost after reopen: %v", err)
	}
}

// assertSuggestionPending fails the test unless the stored suggestion with
// the given id is still pending and carries no applied timestamp.
func assertSuggestionPending(t *testing.T, ss *SuggestionService, id string) {
	t.Helper()
	got, err := ss.Get(id)
	if err != nil {
		t.Fatalf("get %q: %v", id, err)
	}
	if got.Status != SuggestionPending {
		t.Fatalf("suggestion %q expected pending, got %q", id, got.Status)
	}
	if got.AppliedAt != nil {
		t.Fatalf("suggestion %q must not carry an applied timestamp after a failed approve", id)
	}
}

// suggestionIDs returns the ids of the suggestions in order.
func suggestionIDs(sugs []*Suggestion) []string {
	out := make([]string, 0, len(sugs))
	for _, s := range sugs {
		out = append(out, s.ID)
	}
	return out
}
