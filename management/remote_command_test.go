package management

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRemoteCommandEnqueueValidation(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	// an empty node id is rejected
	if _, err := rs.Enqueue(RemoteCommandSpec{Action: ActionStart}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty node id, got %v", err)
	}
	// a blank node id is rejected
	if _, err := rs.Enqueue(RemoteCommandSpec{NodeID: "   ", Action: ActionStart}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for blank node id, got %v", err)
	}
	// an unknown action is rejected
	if _, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: "reboot"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown action, got %v", err)
	}
	// an empty action is rejected too
	if _, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty action, got %v", err)
	}
	if len(rs.List()) != 0 {
		t.Fatal("expected no commands enqueued by rejected specs")
	}
}

func TestRemoteCommandEnqueueDefaultsPending(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	cmd, err := rs.Enqueue(RemoteCommandSpec{NodeID: "  n1  ", Action: ActionRestart})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if cmd.ID == "" {
		t.Fatal("expected generated id")
	}
	if cmd.NodeID != "n1" || cmd.Action != ActionRestart || cmd.Status != CommandPending {
		t.Fatalf("unexpected command: %+v", cmd)
	}
	if cmd.ExecutedAt != nil {
		t.Fatalf("expected nil ExecutedAt for pending command, got %v", cmd.ExecutedAt)
	}
	if cmd.Error != "" {
		t.Fatalf("expected empty error, got %q", cmd.Error)
	}
	// node-level command: no instance id
	if cmd.InstanceID != "" {
		t.Fatalf("expected empty instance id for node-level command, got %q", cmd.InstanceID)
	}
	// an instance-scoped command keeps the instance id
	scoped, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", InstanceID: "i7", Action: ActionStop})
	if err != nil {
		t.Fatalf("enqueue scoped: %v", err)
	}
	if scoped.InstanceID != "i7" {
		t.Fatalf("expected instance id kept, got %q", scoped.InstanceID)
	}
}

func TestRemoteCommandListNewestFirst(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	var ids []string
	for i := 0; i < 3; i++ {
		cmd, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStart})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		ids = append(ids, cmd.ID)
		time.Sleep(2 * time.Millisecond)
	}

	list := rs.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(list))
	}
	for i, cmd := range list {
		if cmd.ID != ids[len(ids)-1-i] {
			t.Fatalf("expected newest-first order, got %v", cmd.ID)
		}
	}
}

func TestRemoteCommandListByNode(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	n1a, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStart})
	if err != nil {
		t.Fatalf("enqueue n1a: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n2", Action: ActionStop}); err != nil {
		t.Fatalf("enqueue n2: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	n1b, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionRestart})
	if err != nil {
		t.Fatalf("enqueue n1b: %v", err)
	}

	list := rs.ListByNode("n1")
	if len(list) != 2 {
		t.Fatalf("expected 2 commands for n1, got %d", len(list))
	}
	if list[0].ID != n1b.ID || list[1].ID != n1a.ID {
		t.Fatalf("expected newest-first n1 commands, got %v, %v", list[0].ID, list[1].ID)
	}
	// the other node's command is not included
	for _, cmd := range list {
		if cmd.NodeID != "n1" {
			t.Fatalf("command for another node leaked into list: %+v", cmd)
		}
	}
	if len(rs.ListByNode("missing")) != 0 {
		t.Fatal("expected no commands for an unknown node")
	}
}

// TestRemoteCommandStatusTransitions walks the full state machine through
// the service: pending -> sent -> success, pending -> failed directly,
// sent -> failed, and the terminal lock that rejects every further Mark
// with ErrInvalid while leaving the command unchanged.
func TestRemoteCommandStatusTransitions(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	cmd, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStart})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// pending -> sent
	cmd, err = rs.MarkSent(cmd.ID)
	if err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if cmd.Status != CommandSent {
		t.Fatalf("expected sent status, got %+v", cmd)
	}
	// sent -> success
	cmd, err = rs.MarkSuccess(cmd.ID)
	if err != nil {
		t.Fatalf("mark success: %v", err)
	}
	if cmd.Status != CommandSuccess {
		t.Fatalf("expected success status, got %+v", cmd)
	}

	// a terminal command rejects every Mark with ErrInvalid and keeps its
	// state
	marks := []struct {
		name string
		fn   func(id string) (*RemoteCommand, error)
	}{
		{"MarkSent", rs.MarkSent},
		{"MarkSuccess", rs.MarkSuccess},
		{"MarkFailed", func(id string) (*RemoteCommand, error) { return rs.MarkFailed(id, "x") }},
	}
	for _, m := range marks {
		got, err := m.fn(cmd.ID)
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid from %s on terminal command, got %v", m.name, err)
		}
		if got != nil {
			t.Fatalf("expected nil command from %s, got %+v", m.name, got)
		}
		if !strings.Contains(err.Error(), "command already terminal") {
			t.Fatalf("expected %s error to mention terminal state, got %q", m.name, err)
		}
		cur, err := rs.Get(cmd.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != CommandSuccess {
			t.Fatalf("status changed by rejected %s: %+v", m.name, cur)
		}
	}

	// pending -> failed directly
	direct, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStop})
	if err != nil {
		t.Fatalf("enqueue direct: %v", err)
	}
	direct, err = rs.MarkFailed(direct.ID, "node unreachable")
	if err != nil {
		t.Fatalf("mark failed from pending: %v", err)
	}
	if direct.Status != CommandFailed || direct.Error != "node unreachable" {
		t.Fatalf("unexpected directly failed command: %+v", direct)
	}

	// sent -> failed
	sent, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionRestart})
	if err != nil {
		t.Fatalf("enqueue sent: %v", err)
	}
	sent, err = rs.MarkSent(sent.ID)
	if err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	sent, err = rs.MarkFailed(sent.ID, "timeout")
	if err != nil {
		t.Fatalf("mark failed from sent: %v", err)
	}
	if sent.Status != CommandFailed || sent.Error != "timeout" {
		t.Fatalf("unexpected sent-then-failed command: %+v", sent)
	}
}

// TestRemoteCommandExecutedAt verifies ExecutedAt is recorded when the
// command first leaves pending: on MarkSent, or on a direct pending ->
// failed (and by extension pending -> success) transition, and never
// re-recorded afterwards.
func TestRemoteCommandExecutedAt(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	// recorded on sent
	cmd, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStart})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if cmd.ExecutedAt != nil {
		t.Fatalf("expected nil ExecutedAt while pending, got %v", cmd.ExecutedAt)
	}
	cmd, err = rs.MarkSent(cmd.ID)
	if err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if cmd.ExecutedAt == nil {
		t.Fatal("expected ExecutedAt recorded on sent")
	}
	sentAt := *cmd.ExecutedAt

	// unchanged by the outcome transition
	time.Sleep(2 * time.Millisecond)
	cmd, err = rs.MarkSuccess(cmd.ID)
	if err != nil {
		t.Fatalf("mark success: %v", err)
	}
	if !cmd.ExecutedAt.Equal(sentAt) {
		t.Fatalf("expected ExecutedAt unchanged after success, got %v vs %v", cmd.ExecutedAt, sentAt)
	}

	// recorded on a direct pending -> failed transition
	direct, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStop})
	if err != nil {
		t.Fatalf("enqueue direct: %v", err)
	}
	if direct.ExecutedAt != nil {
		t.Fatalf("expected nil ExecutedAt while pending, got %v", direct.ExecutedAt)
	}
	direct, err = rs.MarkFailed(direct.ID, "boom")
	if err != nil {
		t.Fatalf("mark failed from pending: %v", err)
	}
	if direct.ExecutedAt == nil {
		t.Fatal("expected ExecutedAt recorded on direct pending -> failed")
	}
}

func TestRemoteCommandMarkFailedError(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	// a non-empty errMsg replaces the Error field
	cmd, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStart})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cmd, err = rs.MarkFailed(cmd.ID, "disk full")
	if err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if cmd.Error != "disk full" {
		t.Fatalf("expected error set to %q, got %q", "disk full", cmd.Error)
	}

	// an empty errMsg leaves the existing Error untouched (seeded through
	// the store, since a failed command is terminal and locked)
	seeded, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionRestart})
	if err != nil {
		t.Fatalf("enqueue seeded: %v", err)
	}
	if err := s.Update(func(d *Data) error {
		for _, c := range d.RemoteCommands {
			if c.ID == seeded.ID {
				c.Error = "stale"
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed error: %v", err)
	}
	seeded, err = rs.MarkFailed(seeded.ID, "")
	if err != nil {
		t.Fatalf("mark failed empty msg: %v", err)
	}
	if seeded.Error != "stale" {
		t.Fatalf("expected error kept, got %q", seeded.Error)
	}
}

// TestRemoteCommandPurgeTerminal verifies PurgeTerminal keeps the newest
// maxKeep terminal commands, removes older terminal ones and never touches
// non-terminal commands.
func TestRemoteCommandPurgeTerminal(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	// four commands, one per 2ms: c1 (oldest, failed), c2 (success),
	// keep (pending), c3 (newest, success)
	enqueue := func(action RemoteAction) *RemoteCommand {
		t.Helper()
		cmd, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: action})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
		return cmd
	}
	c1 := enqueue(ActionStart)
	c2 := enqueue(ActionStop)
	keep := enqueue(ActionRestart)
	c3 := enqueue(ActionStart)

	mustMark := func(cmd *RemoteCommand, status CommandStatus) {
		t.Helper()
		var err error
		switch status {
		case CommandFailed:
			_, err = rs.MarkFailed(cmd.ID, "x")
		case CommandSuccess:
			_, err = rs.MarkSuccess(cmd.ID)
		default:
			t.Fatalf("unexpected mark target %s", status)
		}
		if err != nil {
			t.Fatalf("mark %s: %v", status, err)
		}
	}
	mustMark(c1, CommandFailed)
	mustMark(c2, CommandSuccess)
	mustMark(c3, CommandSuccess)

	// keep the 2 newest terminal commands: c1 (oldest terminal) is purged
	removed, err := rs.PurgeTerminal(2)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	list := rs.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 commands left, got %d", len(list))
	}
	for _, cmd := range list {
		if cmd.ID == c1.ID {
			t.Fatal("expected the oldest terminal command to be purged")
		}
		if cmd.ID == keep.ID && cmd.Status != CommandPending {
			t.Fatalf("pending command mutated by purge: %+v", cmd)
		}
	}

	// maxKeep 0 removes every terminal command, pending survives
	removed, err = rs.PurgeTerminal(0)
	if err != nil {
		t.Fatalf("purge all: %v", err)
	}
	if removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}
	list = rs.List()
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Fatalf("expected only the pending command to survive, got %+v", list)
	}
}

// TestRemoteCommandPurgeTerminalNoop verifies PurgeTerminal with nothing to
// delete neither rewrites the store file nor bumps root UpdatedAt, and
// still returns zero removed while keeping every command.
func TestRemoteCommandPurgeTerminalNoop(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	if _, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStart}); err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	done, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", Action: ActionStop})
	if err != nil {
		t.Fatalf("enqueue terminal: %v", err)
	}
	if _, err := rs.MarkSuccess(done.ID); err != nil {
		t.Fatalf("mark success: %v", err)
	}

	path := s.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := snap.UpdatedAt

	removed, err := rs.PurgeTerminal(10)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("expected purge with nothing to delete not to rewrite the store file")
	}
	snap, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UpdatedAt.Equal(rootBefore) {
		t.Fatalf("expected root UpdatedAt unchanged, got %v vs %v", snap.UpdatedAt, rootBefore)
	}
	if len(rs.List()) != 2 {
		t.Fatalf("expected both commands kept, got %d", len(rs.List()))
	}

	// a negative maxKeep is rejected
	if _, err := rs.PurgeTerminal(-1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for negative maxKeep, got %v", err)
	}
}

// TestRemoteCommandPersistence verifies enqueued commands and their status
// survive a close/reopen cycle and are stored under the remoteCommands key
// of the document.
func TestRemoteCommandPersistence(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	cmd, err := rs.Enqueue(RemoteCommandSpec{NodeID: "n1", InstanceID: "i2", Action: ActionRestart})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cmd, err = rs.MarkSent(cmd.ID)
	if err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	cmd, err = rs.MarkSuccess(cmd.ID)
	if err != nil {
		t.Fatalf("mark success: %v", err)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	list := NewRemoteCommandService(reopened).List()
	if len(list) != 1 {
		t.Fatalf("expected 1 command after reopen, got %d", len(list))
	}
	cur := list[0]
	if cur.ID != cmd.ID || cur.NodeID != "n1" || cur.InstanceID != "i2" ||
		cur.Action != ActionRestart || cur.Status != CommandSuccess || cur.Error != "" {
		t.Fatalf("command changed after reopen: %+v", cur)
	}
	if cur.ExecutedAt == nil || !cur.ExecutedAt.Equal(*cmd.ExecutedAt) {
		t.Fatalf("ExecutedAt changed after reopen: %v vs %v", cur.ExecutedAt, cmd.ExecutedAt)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"remoteCommands"`)) {
		t.Fatal("expected commands to be stored under the remoteCommands key")
	}
}

// TestRemoteCommandMissingID verifies every id-addressed method reports
// ErrNotFound for an unknown id.
func TestRemoteCommandMissingID(t *testing.T) {
	s := newTestStore(t)
	rs := NewRemoteCommandService(s)

	if _, err := rs.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound from Get, got %v", err)
	}
	if _, err := rs.MarkSent("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound from MarkSent, got %v", err)
	}
	if _, err := rs.MarkSuccess("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound from MarkSuccess, got %v", err)
	}
	if _, err := rs.MarkFailed("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound from MarkFailed, got %v", err)
	}
}

// TestRemoteCommandValidationHelpers exercises the pure validators: every
// known action and status is accepted, anything else is ErrInvalid.
func TestRemoteCommandValidationHelpers(t *testing.T) {
	for _, a := range []RemoteAction{ActionStart, ActionStop, ActionRestart} {
		if err := validateRemoteAction(a); err != nil {
			t.Fatalf("expected action %q valid, got %v", a, err)
		}
	}
	for _, a := range []RemoteAction{"", "reboot", "pause"} {
		if err := validateRemoteAction(a); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for action %q, got %v", a, err)
		}
	}
	for _, st := range []CommandStatus{CommandPending, CommandSent, CommandSuccess, CommandFailed} {
		if err := validateCommandStatus(st); err != nil {
			t.Fatalf("expected status %q valid, got %v", st, err)
		}
	}
	if err := validateCommandStatus("weird"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown status, got %v", err)
	}
	if err := validateRemoteCommandSpec(RemoteCommandSpec{NodeID: "n1", Action: ActionStart}); err != nil {
		t.Fatalf("expected valid spec accepted, got %v", err)
	}
	if err := validateRemoteCommandSpec(RemoteCommandSpec{Action: ActionStart}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for spec without node id, got %v", err)
	}
}

// TestRemoteCommandTransitionPure exercises the state machine directly on
// an in-memory command, without the store: transitions apply, ExecutedAt is
// recorded once on the first departure from pending, and a rejected
// transition on a terminal command mutates nothing.
func TestRemoteCommandTransitionPure(t *testing.T) {
	c := &RemoteCommand{ID: "c1", Status: CommandPending}
	if err := transitionRemoteCommand(c, CommandSent, ""); err != nil {
		t.Fatalf("pending->sent: %v", err)
	}
	if c.Status != CommandSent || c.ExecutedAt == nil {
		t.Fatalf("unexpected after pending->sent: %+v", c)
	}
	sentAt := *c.ExecutedAt

	// sent -> failed records the error and keeps ExecutedAt
	if err := transitionRemoteCommand(c, CommandFailed, "boom"); err != nil {
		t.Fatalf("sent->failed: %v", err)
	}
	if c.Status != CommandFailed || c.Error != "boom" || !c.ExecutedAt.Equal(sentAt) {
		t.Fatalf("unexpected after sent->failed: %+v", c)
	}

	// terminal rejects and leaves the command untouched
	before := *c
	if err := transitionRemoteCommand(c, CommandSuccess, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid on terminal transition, got %v", err)
	}
	if c.Status != before.Status || c.Error != before.Error ||
		c.ExecutedAt != before.ExecutedAt || !c.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("terminal command mutated by rejected transition: %+v", c)
	}
}
