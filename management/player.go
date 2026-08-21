package management

import "context"

// Player is the playback backend abstraction consumed by the Scheduler and
// related services.
//
// The management package intentionally never imports core or provider
// packages. Any playback engine can be attached to the scheduler through this
// interface, including a future console adapter that drives libkplayer over
// its existing prompt/message channels, without coupling the management layer
// to a concrete backend.
type Player interface {
	// Play starts playback of the referenced resource. A valid request sets
	// exactly one of PlaylistID or MediaID. Implementations should treat
	// context cancellation as a request to abandon the operation.
	Play(ctx context.Context, req PlayRequest) error
}

// Priority classifies the scheduling precedence of a play request. The zero
// value is treated as PriorityNormal, so phase-1 requests and tasks keep
// their behaviour without any configuration.
type Priority string

const (
	// PriorityNormal is the default priority: a fire plays only when the
	// scheduler is idle and never preempts or queues.
	PriorityNormal Priority = "normal"
	// PriorityImportant queues behind a normal play instead of preempting
	// it; it is skipped while something at important or critical priority is
	// playing.
	PriorityImportant Priority = "important"
	// PriorityCritical preempts any strictly lower-priority play in progress
	// by cancelling its context.
	PriorityCritical Priority = "critical"
)

// rank maps a priority to its precedence. Unknown or empty values rank as
// normal; task validation rejects unknown priorities before they reach the
// scheduler, so this only guards direct Play calls and legacy data.
func (p Priority) rank() int {
	switch p {
	case PriorityImportant:
		return 1
	case PriorityCritical:
		return 2
	default:
		return 0
	}
}

// normalizePriority maps an empty priority to PriorityNormal.
func normalizePriority(p Priority) Priority {
	if p == "" {
		return PriorityNormal
	}
	return p
}

// PlayRequest describes what to play. PlaylistID and MediaID are mutually
// exclusive; a valid request sets exactly one of them.
type PlayRequest struct {
	// PlaylistID references a playlist managed by PlaylistService.
	PlaylistID string `json:"playlistId,omitempty"`
	// MediaID references a media item managed by MediaService.
	MediaID string `json:"mediaId,omitempty"`
	// SceneTemplateID references a scene template to apply when the target
	// plays. It is carried through to the playback backend as advisory
	// metadata: rendering the scene is owned by the core renderer, so this
	// layer does not interpret it. Empty means no template is applied.
	SceneTemplateID string `json:"sceneTemplateId,omitempty"`
	// Loop requests the backend to repeat the resource.
	Loop bool `json:"loop,omitempty"`
	// SeekSeconds starts playback from the given offset in seconds (0 =
	// from the beginning).
	SeekSeconds float64 `json:"seekSeconds,omitempty"`
	// Random requests a random item of the playlist when PlaylistID is set
	// (single-engine mode plays one random item; the playlist order loop
	// otherwise plays in order).
	Random bool `json:"random,omitempty"`
	// Priority is the scheduling precedence of the request; the zero value
	// means PriorityNormal.
	Priority Priority `json:"priority,omitempty"`
}

// compile-time assertion that *PlayerFunc satisfies Player, enabling simple
// adapters in tests and callers.
var _ Player = (PlayerFunc)(nil)

// PlayerFunc is an adapter that lets a plain function be used as a Player.
type PlayerFunc func(ctx context.Context, req PlayRequest) error

// Play implements Player.
func (f PlayerFunc) Play(ctx context.Context, req PlayRequest) error { return f(ctx, req) }
