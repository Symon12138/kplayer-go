package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RemoteAction is the operation a remote command asks the target node to
// perform.
type RemoteAction string

const (
	// ActionStart asks the target node to start a service or instance.
	ActionStart RemoteAction = "start"
	// ActionStop asks the target node to stop a service or instance.
	ActionStop RemoteAction = "stop"
	// ActionRestart asks the target node to restart a service or instance.
	ActionRestart RemoteAction = "restart"
)

// CommandStatus is the lifecycle state of a remote command.
type CommandStatus string

const (
	// CommandPending marks a command queued but not yet sent to the node.
	CommandPending CommandStatus = "pending"
	// CommandSent marks a command handed off to the node, awaiting the
	// outcome.
	CommandSent CommandStatus = "sent"
	// CommandSuccess marks a command the node reported as executed
	// successfully.
	CommandSuccess CommandStatus = "success"
	// CommandFailed marks a command that did not succeed.
	CommandFailed CommandStatus = "failed"
)

// IsTerminal reports whether the status is a terminal one (success or
// failed); unrecognized statuses, possible only in a hand-edited store, are
// treated as non-terminal.
func (s CommandStatus) IsTerminal() bool { return s == CommandSuccess || s == CommandFailed }

// RemoteCommand is one entry of the remote command queue: an operation
// addressed to a target node, optionally scoped to one of its instances
// (an empty InstanceID means a node-level command). It tracks the
// delivery lifecycle and, once executed, the outcome. ExecutedAt records
// the moment the command first leaves pending (see MarkSent); it is nil
// while the command is pending.
type RemoteCommand struct {
	ID         string        `json:"id"`
	NodeID     string        `json:"nodeId"`
	InstanceID string        `json:"instanceId,omitempty"`
	Action     RemoteAction  `json:"action"`
	Status     CommandStatus `json:"status"`
	Error      string        `json:"error,omitempty"`
	CreatedAt  time.Time     `json:"createdAt"`
	ExecutedAt *time.Time    `json:"executedAt,omitempty"`
	UpdatedAt  time.Time     `json:"updatedAt"`
}

// RemoteCommandSpec is the validated input used to enqueue a remote
// command. The node id must be non-empty and the action must be one of the
// known actions; the instance id is optional (empty means node-level).
type RemoteCommandSpec struct {
	NodeID     string
	InstanceID string
	Action     RemoteAction
}

// RemoteCommandService provides queueing and status transitions over the
// remote commands of a Store.
type RemoteCommandService struct {
	store *Store
}

// NewRemoteCommandService returns a RemoteCommandService backed by store.
func NewRemoteCommandService(store *Store) *RemoteCommandService {
	return &RemoteCommandService{store: store}
}

// Enqueue adds a new remote command from spec to the queue. The node id
// must be non-empty after trimming (ErrInvalid) and the action must be a
// known action (ErrInvalid); the instance id is optional and passed through
// as-is. The command is enqueued with status CommandPending and no
// ExecutedAt. It returns the created command.
func (rs *RemoteCommandService) Enqueue(spec RemoteCommandSpec) (*RemoteCommand, error) {
	spec.NodeID = strings.TrimSpace(spec.NodeID)
	if err := validateRemoteCommandSpec(spec); err != nil {
		return nil, err
	}
	now := time.Now()
	cmd := &RemoteCommand{
		ID:         newID(),
		NodeID:     spec.NodeID,
		InstanceID: spec.InstanceID,
		Action:     spec.Action,
		Status:     CommandPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	err := rs.store.Update(func(d *Data) error {
		d.RemoteCommands = append(d.RemoteCommands, cmd)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

// List returns all remote commands, newest first (sorted by CreatedAt
// descending; id breaks ties for a deterministic order).
func (rs *RemoteCommandService) List() []*RemoteCommand {
	out := make([]*RemoteCommand, 0)
	rs.store.View(func(d *Data) {
		out = append(out, d.RemoteCommands...)
	})
	sortRemoteCommands(out)
	return out
}

// ListByNode returns the remote commands targeting nodeID (exact match),
// newest first in the same order as List. An unknown node yields an empty
// list.
func (rs *RemoteCommandService) ListByNode(nodeID string) []*RemoteCommand {
	out := make([]*RemoteCommand, 0)
	rs.store.View(func(d *Data) {
		for _, c := range d.RemoteCommands {
			if c.NodeID == nodeID {
				out = append(out, c)
			}
		}
	})
	sortRemoteCommands(out)
	return out
}

// Get returns the remote command with the given id.
func (rs *RemoteCommandService) Get(id string) (*RemoteCommand, error) {
	var found *RemoteCommand
	rs.store.View(func(d *Data) {
		for _, c := range d.RemoteCommands {
			if c.ID == id {
				found = c
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("remote command %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// MarkSent marks the command with the given id as sent to its node and
// returns it. Terminal commands reject the transition with ErrInvalid (see
// transitionRemoteCommand for the full state machine).
func (rs *RemoteCommandService) MarkSent(id string) (*RemoteCommand, error) {
	return rs.mark(id, CommandSent, "")
}

// MarkSuccess marks the command with the given id as executed successfully
// and returns it. Terminal commands reject the transition with ErrInvalid.
func (rs *RemoteCommandService) MarkSuccess(id string) (*RemoteCommand, error) {
	return rs.mark(id, CommandSuccess, "")
}

// MarkFailed marks the command with the given id as failed and returns it.
// A non-empty errMsg replaces the command Error field; an empty errMsg
// leaves the existing Error untouched. Terminal commands reject the
// transition with ErrInvalid.
func (rs *RemoteCommandService) MarkFailed(id, errMsg string) (*RemoteCommand, error) {
	return rs.mark(id, CommandFailed, errMsg)
}

// mark applies one state-machine step to the command with the given id
// under the store write lock and bumps its UpdatedAt. Terminal commands are
// locked by transitionRemoteCommand, so a rejected step leaves the command
// untouched and the store unrewritten.
func (rs *RemoteCommandService) mark(id string, next CommandStatus, errMsg string) (*RemoteCommand, error) {
	var out *RemoteCommand
	err := rs.store.Update(func(d *Data) error {
		for _, c := range d.RemoteCommands {
			if c.ID != id {
				continue
			}
			if err := transitionRemoteCommand(c, next, errMsg); err != nil {
				return err
			}
			c.UpdatedAt = time.Now()
			out = c
			return nil
		}
		return fmt.Errorf("remote command %q: %w", id, ErrNotFound)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// transitionRemoteCommand applies one state-machine step to c, mutating it
// in place. The allowed transitions are pending -> sent/success/failed and
// sent -> success/failed; every Mark on a non-terminal command applies
// (re-marking to the same status is allowed and refreshes UpdatedAt via the
// caller). ExecutedAt is recorded the first time the command leaves pending:
// when it is marked sent, or when it goes straight from pending to a
// terminal state (success/failed); it is never re-recorded afterwards. A
// non-empty errMsg replaces Error (used by MarkFailed); an empty one leaves
// it untouched. Terminal commands (success/failed) are locked: any further
// transition returns ErrInvalid wrapped with "command already terminal" and
// leaves c untouched, so the caller's update rolls back without rewriting
// the store.
func transitionRemoteCommand(c *RemoteCommand, next CommandStatus, errMsg string) error {
	if c.Status.IsTerminal() {
		return fmt.Errorf("remote command %q: %w: command already terminal (%s)", c.ID, ErrInvalid, c.Status)
	}
	if c.ExecutedAt == nil {
		now := time.Now()
		c.ExecutedAt = &now
	}
	if errMsg != "" {
		c.Error = errMsg
	}
	c.Status = next
	return nil
}

// PurgeTerminal keeps the maxKeep most recent terminal commands
// (success/failed, newest by CreatedAt with id tie-break, matching List
// order) and removes the older terminal ones; non-terminal commands are
// never removed. It returns the number removed. A negative maxKeep is
// rejected with ErrInvalid; zero keeps no terminal command (everything
// terminal is removed). When nothing is removed it is a no-op: the store is
// not rewritten and root UpdatedAt is not bumped (like alarm.Prune).
func (rs *RemoteCommandService) PurgeTerminal(maxKeep int) (int, error) {
	if maxKeep < 0 {
		return 0, fmt.Errorf("remote command: %w: negative maxKeep %d", ErrInvalid, maxKeep)
	}
	removed := 0
	err := rs.store.Update(func(d *Data) error {
		terminal := make([]*RemoteCommand, 0)
		for _, c := range d.RemoteCommands {
			if c.Status.IsTerminal() {
				terminal = append(terminal, c)
			}
		}
		sortRemoteCommands(terminal)
		keep := make(map[string]struct{}, maxKeep)
		for i := 0; i < len(terminal) && i < maxKeep; i++ {
			keep[terminal[i].ID] = struct{}{}
		}
		kept := d.RemoteCommands[:0]
		for _, c := range d.RemoteCommands {
			if c.Status.IsTerminal() {
				if _, ok := keep[c.ID]; !ok {
					removed++
					continue
				}
			}
			kept = append(kept, c)
		}
		if removed == 0 {
			return errNoop
		}
		d.RemoteCommands = kept
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// sortRemoteCommands orders commands newest first (CreatedAt descending; id
// breaks ties).
func sortRemoteCommands(cmds []*RemoteCommand) {
	sort.Slice(cmds, func(i, j int) bool {
		if cmds[i].CreatedAt.Equal(cmds[j].CreatedAt) {
			return cmds[i].ID < cmds[j].ID
		}
		return cmds[i].CreatedAt.After(cmds[j].CreatedAt)
	})
}

// validateRemoteCommandSpec performs field-level validation independent of
// the store: the node id must be non-empty and the action must be one of
// the known actions.
func validateRemoteCommandSpec(spec RemoteCommandSpec) error {
	if strings.TrimSpace(spec.NodeID) == "" {
		return fmt.Errorf("remote command: %w: empty node id", ErrInvalid)
	}
	return validateRemoteAction(spec.Action)
}

// validateRemoteAction reports whether a is a known remote action.
func validateRemoteAction(a RemoteAction) error {
	switch a {
	case ActionStart, ActionStop, ActionRestart:
		return nil
	}
	return fmt.Errorf("remote command: %w: unknown action %q", ErrInvalid, a)
}

// validateCommandStatus reports whether s is a known command status.
// Remote commands are never created with a caller-supplied status (the
// queue always starts pending), so this is a guard for store content that
// was written or repaired by hand.
func validateCommandStatus(s CommandStatus) error {
	switch s {
	case CommandPending, CommandSent, CommandSuccess, CommandFailed:
		return nil
	}
	return fmt.Errorf("remote command: %w: unknown status %q", ErrInvalid, s)
}
