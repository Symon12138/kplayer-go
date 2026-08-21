package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PlayMode names the playback mode of a playlist.
type PlayMode string

const (
	// PlayModeOrder plays the items in order, once (default).
	PlayModeOrder PlayMode = "order"
	// PlayModeLoop plays the items in order, looping forever.
	PlayModeLoop PlayMode = "loop"
	// PlayModeRandom plays the items in random order, once.
	PlayModeRandom PlayMode = "random"
	// PlayModeRandomLoop plays the items in random order, looping forever.
	PlayModeRandomLoop PlayMode = "random-loop"
)

// PlaylistItem is one entry in a playlist: a reference to a media item. A
// media item whose path is a directory expands to its video files (in the
// directory's sort order) when the playlist is resolved for playback.
type PlaylistItem struct {
	// MediaID references a media item managed by MediaService.
	MediaID string `json:"mediaId"`
}

// Playlist is an ordered list of media references used as a program schedule.
type Playlist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`
	// FallbackPlaylistID optionally references another playlist that is
	// resolved when this playlist cannot satisfy a request (it is empty or a
	// referenced media is missing). It must reference an existing playlist
	// other than this one; deeper chains are capped and cycle-checked at
	// resolution time (see ResolveWithFallback).
	FallbackPlaylistID string          `json:"fallbackPlaylistId,omitempty"`
	Items              []*PlaylistItem `json:"items,omitempty"`
	// Mode is the playback mode; empty means PlayModeOrder. Loop (bool) is
	// kept as a compatibility alias for PlayModeLoop.
	Mode      PlayMode        `json:"mode,omitempty"`
	Loop      bool            `json:"loop,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// EffectiveMode returns the playback mode, honouring the legacy Loop flag.
func (p *Playlist) EffectiveMode() PlayMode {
	switch p.Mode {
	case PlayModeLoop, PlayModeRandom, PlayModeRandomLoop:
		return p.Mode
	}
	if p.Loop {
		return PlayModeLoop
	}
	return PlayModeOrder
}

// MediaIDs returns the ordered media identifiers of the playlist.
func (p *Playlist) MediaIDs() []string {
	ids := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		ids = append(ids, it.MediaID)
	}
	return ids
}

// PlaylistService provides CRUD over the playlists of a Store plus ordered
// item manipulation.
type PlaylistService struct {
	store *Store
}

// NewPlaylistService returns a PlaylistService backed by store.
func NewPlaylistService(store *Store) *PlaylistService {
	return &PlaylistService{store: store}
}

// List returns all playlists in insertion order.
func (ps *PlaylistService) List() []*Playlist {
	out := make([]*Playlist, 0)
	ps.store.View(func(d *Data) {
		out = append(out, d.Playlists...)
	})
	return out
}

// ListSorted returns all playlists sorted by name.
func (ps *PlaylistService) ListSorted() []*Playlist {
	out := ps.List()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the playlist with the given id.
func (ps *PlaylistService) Get(id string) (*Playlist, error) {
	var found *Playlist
	ps.store.View(func(d *Data) {
		for _, p := range d.Playlists {
			if p.ID == id {
				found = p
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("playlist %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new playlist with the given name, description and ordered
// media references. mediaIDs may be empty (an empty playlist is allowed).
// Every referenced media id must exist; otherwise ErrInvalid is returned.
func (ps *PlaylistService) Create(name, desc string, mediaIDs []string, loop bool) (*Playlist, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("playlist: %w: empty name", ErrInvalid)
	}
	now := time.Now()
	p := &Playlist{ID: newID(), Name: name, Desc: desc, Loop: loop, CreatedAt: now, UpdatedAt: now}
	err := ps.store.Update(func(d *Data) error {
		if err := setPlaylistItems(d, p, mediaIDs); err != nil {
			return err
		}
		if err := validateFallbackRef(d, p); err != nil {
			return err
		}
		d.Playlists = append(d.Playlists, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Update applies fn to the playlist with the given id under the store write
// lock; fn may mutate the playlist in place. Returning an error rolls back.
// A fallback reference set by fn is validated (target must exist and must
// not be the playlist itself).
func (ps *PlaylistService) Update(id string, fn func(p *Playlist) error) error {
	return ps.store.Update(func(d *Data) error {
		for _, p := range d.Playlists {
			if p.ID != id {
				continue
			}
			if err := fn(p); err != nil {
				return err
			}
			if err := validateFallbackRef(d, p); err != nil {
				return err
			}
			p.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("playlist %q: %w", id, ErrNotFound)
	})
}

// SetFallback attaches the fallback playlist with the given id to the
// playlist with the given id, or clears the reference by passing "". The
// fallback must reference an existing playlist other than the playlist
// itself.
func (ps *PlaylistService) SetFallback(id, fallbackID string) error {
	return ps.Update(id, func(p *Playlist) error {
		p.FallbackPlaylistID = fallbackID
		return nil
	})
}

// Delete removes a playlist. A playlist still referenced by a scheduled task
// cannot be deleted (ErrInUse).
func (ps *PlaylistService) Delete(id string) error {
	return ps.store.Update(func(d *Data) error {
		idx := -1
		for i, p := range d.Playlists {
			if p.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("playlist %q: %w", id, ErrNotFound)
		}
		p := d.Playlists[idx]
		for _, t := range d.Tasks {
			if t.PlaylistID == id {
				return fmt.Errorf("playlist %q is referenced by task %q: %w", p.Name, t.Name, ErrInUse)
			}
		}
		for _, other := range d.Playlists {
			if other.FallbackPlaylistID == id {
				return fmt.Errorf("playlist %q is referenced as fallback by playlist %q: %w", p.Name, other.Name, ErrInUse)
			}
		}
		d.Playlists = append(d.Playlists[:idx], d.Playlists[idx+1:]...)
		return nil
	})
}

// SetItems replaces the playlist items with the given ordered media ids.
// Every referenced media id must exist, otherwise an error wrapping
// ErrNotFound is returned and the playlist is left unchanged. The playlist's
// fallback reference is re-validated as well.
func (ps *PlaylistService) SetItems(id string, mediaIDs []string) error {
	return ps.store.Update(func(d *Data) error {
		for _, p := range d.Playlists {
			if p.ID != id {
				continue
			}
			if err := setPlaylistItems(d, p, mediaIDs); err != nil {
				return err
			}
			if err := validateFallbackRef(d, p); err != nil {
				return err
			}
			p.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("playlist %q: %w", id, ErrNotFound)
	})
}

// AddItem appends a media item to the playlist. The media id must reference
// an existing media item, otherwise an error wrapping ErrNotFound is returned
// and the playlist is left unchanged. Adding an item that is already present
// is a no-op and does not rewrite the store or bump UpdatedAt.
func (ps *PlaylistService) AddItem(id, mediaID string) error {
	return ps.store.Update(func(d *Data) error {
		for _, p := range d.Playlists {
			if p.ID != id {
				continue
			}
			if mediaID == "" {
				return fmt.Errorf("playlist item: %w: empty media id", ErrInvalid)
			}
			if _, err := ResolveMedia(d, mediaID); err != nil {
				return fmt.Errorf("playlist item: %w", err)
			}
			for _, it := range p.Items {
				if it.MediaID == mediaID {
					return errNoop // already present; idempotent
				}
			}
			p.Items = append(p.Items, &PlaylistItem{MediaID: mediaID})
			return nil
		}
		return fmt.Errorf("playlist %q: %w", id, ErrNotFound)
	})
}

// RemoveItem removes every reference to mediaID from the playlist. It is a
// no-op (no store write, no UpdatedAt bump) when the media is not present.
func (ps *PlaylistService) RemoveItem(id, mediaID string) error {
	return ps.Update(id, func(p *Playlist) error {
		kept := p.Items[:0]
		removed := false
		for _, it := range p.Items {
			if it.MediaID != mediaID {
				kept = append(kept, it)
			} else {
				removed = true
			}
		}
		if !removed {
			return errNoop
		}
		p.Items = kept
		return nil
	})
}

// MoveItem moves the item at index from to index to (both 0-based). Out of
// range indices are clamped to the slice bounds. A move that would not change
// the order is a no-op (no store write, no UpdatedAt bump).
func (ps *PlaylistService) MoveItem(id string, from, to int) error {
	return ps.Update(id, func(p *Playlist) error {
		n := len(p.Items)
		if n < 2 {
			return errNoop
		}
		if from < 0 || from >= n {
			return fmt.Errorf("playlist %q: %w: from index %d out of range", id, ErrInvalid, from)
		}
		if to < 0 {
			to = 0
		}
		if to >= n {
			to = n - 1
		}
		if from == to {
			return errNoop
		}
		item := p.Items[from]
		p.Items = append(p.Items[:from], p.Items[from+1:]...)
		p.Items = append(p.Items, nil)
		copy(p.Items[to+1:], p.Items[to:])
		p.Items[to] = item
		return nil
	})
}

// ClearItems removes all items from the playlist.
func (ps *PlaylistService) ClearItems(id string) error {
	return ps.Update(id, func(p *Playlist) error {
		p.Items = nil
		return nil
	})
}

// Resolve returns the media items referenced by the playlist in order. An
// error is returned if the playlist does not exist or a referenced media
// cannot be resolved.
func (ps *PlaylistService) Resolve(id string) ([]*Media, error) {
	var out []*Media
	var err error
	ps.store.View(func(d *Data) {
		out, err = resolvePlaylistItems(d, id)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// maxFallbackDepth caps how many fallback hops ResolveWithFallback follows
// before giving up, guarding against pathological chains.
const maxFallbackDepth = 5

// ResolveWithFallback resolves the playlist with the given id, transparently
// following its fallback chain when the requested playlist cannot satisfy
// the request: it is empty or references a missing media. (A missing
// playlist is reported as-is: it carries no fallback reference to follow.)
// Each failed playlist is replaced by its FallbackPlaylistID; the first
// playlist in the chain that resolves to at least one media wins.
//
// The returned usedFallback reports whether any fallback hop was taken. When
// no fallback is configured the original error is returned (an empty main
// playlist without fallback yields an empty result and no error, matching
// Resolve); when the whole chain fails, the deepest resolution error is
// returned. A chain that revisits a playlist or exceeds maxFallbackDepth
// hops is reported as ErrInvalid.
func (ps *PlaylistService) ResolveWithFallback(id string) ([]*Media, bool, error) {
	var out []*Media
	var used bool
	var err error
	ps.store.View(func(d *Data) {
		seen := make(map[string]bool)
		cur := id
		hops := 0
		for {
			if seen[cur] {
				err = fmt.Errorf("playlist %q: %w: fallback chain cycle at %q", id, ErrInvalid, cur)
				return
			}
			seen[cur] = true
			items, resolveErr := resolvePlaylistItems(d, cur)
			if resolveErr == nil && len(items) > 0 {
				out = items
				return
			}
			p, findErr := ResolvePlaylist(d, cur)
			if findErr != nil {
				// The playlist itself is gone (the head id is unknown, or a
				// fallback target was deleted): nothing further can be tried.
				err = resolveErr
				return
			}
			if p.FallbackPlaylistID == "" {
				// No fallback configured: report the failure that started
				// the fallback (an empty playlist reports no error).
				err = resolveErr
				if err == nil {
					out = items
				}
				return
			}
			if hops >= maxFallbackDepth {
				err = fmt.Errorf("playlist %q: %w: fallback chain exceeds depth %d", id, ErrInvalid, maxFallbackDepth)
				return
			}
			cur = p.FallbackPlaylistID
			hops++
			used = true
		}
	})
	if err != nil {
		return nil, used, err
	}
	return out, used, nil
}

// resolvePlaylistItems returns the media items of the playlist with the given
// id, or an error when the playlist does not exist or a referenced media is
// missing.
func resolvePlaylistItems(d *Data, id string) ([]*Media, error) {
	var p *Playlist
	for _, pl := range d.Playlists {
		if pl.ID == id {
			p = pl
			break
		}
	}
	if p == nil {
		return nil, fmt.Errorf("playlist %q: %w", id, ErrNotFound)
	}
	out := make([]*Media, 0, len(p.Items))
	for _, it := range p.Items {
		m, _ := ResolveMedia(d, it.MediaID)
		if m == nil {
			return nil, fmt.Errorf("playlist %q references missing media %q", p.Name, it.MediaID)
		}
		out = append(out, m)
	}
	return out, nil
}

// validateFallbackRef checks that p's fallback reference targets an existing
// playlist other than p itself. Deeper chains (cycles, length) are checked at
// resolution time by ResolveWithFallback.
func validateFallbackRef(d *Data, p *Playlist) error {
	if p.FallbackPlaylistID == "" {
		return nil
	}
	if p.FallbackPlaylistID == p.ID {
		return fmt.Errorf("playlist %q: %w: fallback cannot reference itself", p.Name, ErrInvalid)
	}
	if _, err := ResolvePlaylist(d, p.FallbackPlaylistID); err != nil {
		return fmt.Errorf("playlist %q: %w", p.Name, err)
	}
	return nil
}

// setPlaylistItems validates and installs mediaIDs onto p. When d is non-nil
// it is used to validate that every referenced media id exists; a reference
// to a missing media is rejected with an error wrapping ErrNotFound and p is
// left unchanged. References are stored in the given order.
func setPlaylistItems(d *Data, p *Playlist, mediaIDs []string) error {
	items := make([]*PlaylistItem, 0, len(mediaIDs))
	for _, id := range mediaIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if d != nil {
			if _, err := ResolveMedia(d, id); err != nil {
				return fmt.Errorf("playlist item: %w", err)
			}
		}
		items = append(items, &PlaylistItem{MediaID: id})
	}
	p.Items = items
	return nil
}
