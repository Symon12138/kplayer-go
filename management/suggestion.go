package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SuggestionKind classifies what a suggestion proposes. Real AI inference is
// a non-goal for this batch; the kinds describe the slots the suggestion
// skeleton serves (media selection and title generation), so an AI backend
// can be plugged in later without changing the review flow.
type SuggestionKind string

const (
	// SuggestionMediaRecommend proposes media worth playing next (see
	// RecommendMedia); its Payload carries the candidate media_id.
	SuggestionMediaRecommend SuggestionKind = "media_recommend"
	// SuggestionTitleGenerate proposes a title for a program slot; its
	// Payload carries the generation context (for example playlist_name).
	SuggestionTitleGenerate SuggestionKind = "title_generate"
)

// SuggestionStatus is the review lifecycle of a suggestion: it is created
// pending, then either applied by an operator or rejected with a reason.
type SuggestionStatus string

const (
	// SuggestionPending means the suggestion awaits operator review.
	SuggestionPending SuggestionStatus = "pending"
	// SuggestionApplied means the operator approved the suggestion and its
	// effect has been applied (for a media recommendation, the media was
	// persisted as a playlist).
	SuggestionApplied SuggestionStatus = "applied"
	// SuggestionRejected means the operator declined the suggestion; Reason
	// records why.
	SuggestionRejected SuggestionStatus = "rejected"
)

// Suggestion is one proposal awaiting operator review: a heuristic (see
// RecommendMedia) or future AI inference that suggests media or a title.
// Payload holds the suggestion content as key-value pairs (for example
// media_id for a media recommendation, playlist_name for a title
// generation); its exact keys depend on the kind. The review state machine
// is linear — pending is the initial state set at creation, and Approve or
// Reject moves a pending suggestion to applied or rejected, a final
// decision that cannot be undone.
type Suggestion struct {
	ID        string            `json:"id"`
	Kind      SuggestionKind    `json:"kind"`
	Title     string            `json:"title,omitempty"`
	Payload   map[string]string `json:"payload,omitempty"`
	Status    SuggestionStatus  `json:"status"`
	Reason    string            `json:"reason,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	AppliedAt *time.Time        `json:"appliedAt,omitempty"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// SuggestionService provides the suggestion review flow over a Store:
// listing, retrieval, creation of pending suggestions, and the two final
// review decisions — Approve (apply the effect) and Reject (decline with a
// reason).
type SuggestionService struct {
	store *Store
}

// NewSuggestionService returns a SuggestionService backed by store.
func NewSuggestionService(store *Store) *SuggestionService {
	return &SuggestionService{store: store}
}

// List returns all suggestions, newest first (ordered by CreatedAt
// descending, with the ID breaking ties for a deterministic order).
func (ss *SuggestionService) List() []*Suggestion {
	out := make([]*Suggestion, 0)
	ss.store.View(func(d *Data) {
		out = append(out, d.Suggestions...)
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Get returns the suggestion with the given id.
func (ss *SuggestionService) Get(id string) (*Suggestion, error) {
	var found *Suggestion
	ss.store.View(func(d *Data) {
		found = findSuggestion(d, id)
	})
	if found == nil {
		return nil, fmt.Errorf("suggestion %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a pending suggestion with the given kind, optional title and
// payload. The kind must be a known SuggestionKind (ErrInvalid) and the
// payload must carry at least one key-value pair (ErrInvalid); the title is
// optional. The payload map is copied, so later mutation of the caller's map
// never leaks into the store. It returns the suggestion with its generated
// ID and timestamps.
func (ss *SuggestionService) Create(kind SuggestionKind, title string, payload map[string]string) (*Suggestion, error) {
	if err := validateSuggestionKind(kind); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("suggestion: %w: empty payload", ErrInvalid)
	}
	payloadCopy := make(map[string]string, len(payload))
	for k, v := range payload {
		payloadCopy[k] = v
	}
	now := time.Now()
	s := &Suggestion{
		ID:        newID(),
		Kind:      kind,
		Title:     title,
		Payload:   payloadCopy,
		Status:    SuggestionPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := ss.store.Update(func(d *Data) error {
		d.Suggestions = append(d.Suggestions, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Approve applies the pending suggestion with the given id. A media
// recommendation — a suggestion whose Payload carries media_id — is
// persisted as a new playlist named playlistName through the same validation
// as PlaylistService.Create (the name must be non-empty, ErrInvalid, and the
// referenced media must exist, ErrNotFound), and the new playlist id is
// written back into the payload as Payload["playlist_id"]. Only a pending
// suggestion can be approved: any other status is rejected with ErrInvalid
// ("suggestion not pending"). On any error the suggestion keeps its pending
// status and nothing is persisted, so the review decision and its effect
// commit or roll back together. It returns the updated suggestion.
func (ss *SuggestionService) Approve(id, playlistName string) (*Suggestion, error) {
	var out *Suggestion
	err := ss.store.Update(func(d *Data) error {
		s := findSuggestion(d, id)
		if s == nil {
			return fmt.Errorf("suggestion %q: %w", id, ErrNotFound)
		}
		if s.Status != SuggestionPending {
			return fmt.Errorf("suggestion %q: %w: suggestion not pending", id, ErrInvalid)
		}
		if s.Payload == nil {
			// Possible only in a hand-edited store; Create guarantees a
			// non-empty payload.
			return fmt.Errorf("suggestion %q: %w: payload carries no media_id", id, ErrInvalid)
		}
		mediaID := s.Payload["media_id"]
		if strings.TrimSpace(mediaID) == "" {
			return fmt.Errorf("suggestion %q: %w: payload carries no media_id", id, ErrInvalid)
		}
		// Persist the recommended media as a playlist under the write lock,
		// mirroring PlaylistService.Create so the status change and the
		// playlist creation are one atomic update.
		if strings.TrimSpace(playlistName) == "" {
			return fmt.Errorf("playlist: %w: empty name", ErrInvalid)
		}
		now := time.Now()
		pl := &Playlist{ID: newID(), Name: playlistName, CreatedAt: now, UpdatedAt: now}
		if err := setPlaylistItems(d, pl, []string{mediaID}); err != nil {
			return err
		}
		if err := validateFallbackRef(d, pl); err != nil {
			return err
		}
		d.Playlists = append(d.Playlists, pl)

		s.Payload["playlist_id"] = pl.ID
		s.Status = SuggestionApplied
		s.AppliedAt = &now
		s.UpdatedAt = now
		out = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Reject declines the pending suggestion with the given id, recording the
// reason; an empty or blank reason defaults to "rejected by operator". Only
// a pending suggestion can be rejected (ErrInvalid otherwise), so a review
// decision is final. It returns the updated suggestion.
func (ss *SuggestionService) Reject(id, reason string) (*Suggestion, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "rejected by operator"
	}
	var out *Suggestion
	err := ss.store.Update(func(d *Data) error {
		s := findSuggestion(d, id)
		if s == nil {
			return fmt.Errorf("suggestion %q: %w", id, ErrNotFound)
		}
		if s.Status != SuggestionPending {
			return fmt.Errorf("suggestion %q: %w: suggestion not pending", id, ErrInvalid)
		}
		s.Status = SuggestionRejected
		s.Reason = reason
		s.UpdatedAt = time.Now()
		out = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// findSuggestion returns the suggestion with the given id, or nil.
func findSuggestion(d *Data, id string) *Suggestion {
	for _, s := range d.Suggestions {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// validateSuggestionKind reports whether k is a known suggestion kind.
func validateSuggestionKind(k SuggestionKind) error {
	switch k {
	case SuggestionMediaRecommend, SuggestionTitleGenerate:
		return nil
	}
	return fmt.Errorf("suggestion: %w: unknown kind %q", ErrInvalid, k)
}

// validateSuggestionStatus reports whether st is a known suggestion status.
// The services only ever write the three constants — transitions are guarded
// by equality checks — so an unknown status is possible only in a
// hand-edited store.
func validateSuggestionStatus(st SuggestionStatus) error {
	switch st {
	case SuggestionPending, SuggestionApplied, SuggestionRejected:
		return nil
	}
	return fmt.Errorf("suggestion: %w: unknown status %q", ErrInvalid, st)
}
