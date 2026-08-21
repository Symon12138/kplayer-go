package management

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

func TestNodeCRUD(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)

	before := time.Now()
	n, err := ns.Create(NodeSpec{Name: "edge-1", Address: "192.168.1.10:4150", Status: NodeOnline, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	after := time.Now()
	if n.ID == "" {
		t.Fatal("expected generated id")
	}
	if n.Name != "edge-1" || n.Address != "192.168.1.10:4150" || n.Status != NodeOnline || !n.Enabled {
		t.Fatalf("unexpected node: %+v", n)
	}
	if n.CreatedAt.Before(before) || n.CreatedAt.After(after) {
		t.Fatalf("unexpected createdAt: %v", n.CreatedAt)
	}
	if !n.CreatedAt.Equal(n.UpdatedAt) {
		t.Fatalf("expected createdAt == updatedAt on create, got %v vs %v", n.CreatedAt, n.UpdatedAt)
	}

	got, err := ns.Get(n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "edge-1" || got.Status != NodeOnline {
		t.Fatalf("unexpected get: %+v", got)
	}
	if _, err := ns.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing get, got %v", err)
	}

	// Update replaces name, address, status and the enabled flag (the
	// spec's zero Status shows the default-to-unknown and full-replacement
	// semantics).
	upd, err := ns.Update(n.ID, NodeSpec{Name: "edge-1b", Address: "10.0.0.7:4150", Enabled: false})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "edge-1b" || upd.Address != "10.0.0.7:4150" || upd.Status != NodeUnknown || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if upd.UpdatedAt.Before(upd.CreatedAt) {
		t.Fatalf("updatedAt moved backwards: %v < %v", upd.UpdatedAt, upd.CreatedAt)
	}

	// SetEnabled toggles both ways.
	if err := ns.SetEnabled(n.ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if err := ns.SetEnabled(n.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err = ns.Get(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected node disabled after SetEnabled(false)")
	}

	if err := ns.Delete(n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(ns.List()) != 0 {
		t.Fatal("expected empty node list")
	}
	if _, err := ns.Get(n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := ns.Delete(n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestNodeListSorted(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)

	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := ns.Create(NodeSpec{Name: name, Address: "h:1"}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	list := ns.List()
	want := []string{"alpha", "mike", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("expected %d nodes, got %d", len(want), len(list))
	}
	for i, n := range list {
		if n.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, nodeNames(list))
		}
	}
}

func TestNodeValidation(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)

	// empty (and whitespace-only) names and addresses are rejected
	for _, name := range []string{"", "   "} {
		if _, err := ns.Create(NodeSpec{Name: name, Address: "h:1"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for name %q, got %v", name, err)
		}
	}
	for _, addr := range []string{"", "   "} {
		if _, err := ns.Create(NodeSpec{Name: "x", Address: addr}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for address %q, got %v", addr, err)
		}
	}
	// unknown statuses are rejected; an empty status defaults to unknown
	if _, err := ns.Create(NodeSpec{Name: "x", Address: "h:1", Status: NodeStatus("booting")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown status, got %v", err)
	}
	n, err := ns.Create(NodeSpec{Name: "x", Address: "h:1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n.Status != NodeUnknown {
		t.Fatalf("expected default status unknown, got %q", n.Status)
	}

	// duplicate name on create
	if _, err := ns.Create(NodeSpec{Name: "dup", Address: "h:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ns.Create(NodeSpec{Name: "dup", Address: "h:2"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for duplicate name, got %v", err)
	}

	// update validates like create
	if _, err := ns.Update(n.ID, NodeSpec{Name: "  ", Address: "h:1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}
	if _, err := ns.Update(n.ID, NodeSpec{Name: "y", Address: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update address, got %v", err)
	}
	if _, err := ns.Update(n.ID, NodeSpec{Name: "y", Address: "h:1", Status: NodeStatus("weird")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown update status, got %v", err)
	}
	if _, err := ns.Update(n.ID, NodeSpec{Name: "dup", Address: "h:1"}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists for colliding rename, got %v", err)
	}
	// renaming to its own current name is fine
	if _, err := ns.Update(n.ID, NodeSpec{Name: "x", Address: "h:9"}); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
	// missing id on every mutator
	if _, err := ns.Update("missing", NodeSpec{Name: "x", Address: "h:1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing update, got %v", err)
	}
	if err := ns.SetEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing set-enabled, got %v", err)
	}
	if err := ns.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
	if _, err := ns.Heartbeat("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing heartbeat, got %v", err)
	}
}

func TestNodeHeartbeat(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)

	n, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1", Status: NodeOffline, Enabled: false})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !n.LastSeen.IsZero() {
		t.Fatalf("expected zero LastSeen before first heartbeat, got %v", n.LastSeen)
	}

	before := time.Now()
	hb, err := ns.Heartbeat(n.ID)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	after := time.Now()
	if hb.Status != NodeOnline {
		t.Fatalf("expected online after heartbeat, got %q", hb.Status)
	}
	if hb.LastSeen.Before(before) || hb.LastSeen.After(after) {
		t.Fatalf("unexpected LastSeen: %v (outside %v..%v)", hb.LastSeen, before, after)
	}
	if !hb.UpdatedAt.Equal(hb.LastSeen) {
		t.Fatalf("expected UpdatedAt stamped with the heartbeat time, got %v vs LastSeen %v", hb.UpdatedAt, hb.LastSeen)
	}
	if hb.Enabled {
		t.Fatal("heartbeat must not change the enabled flag")
	}
	if NodeStale(hb, hb.LastSeen, time.Minute) {
		t.Fatal("expected a fresh heartbeat to be not stale")
	}

	// heartbeating again refreshes LastSeen
	time.Sleep(5 * time.Millisecond)
	again, err := ns.Heartbeat(n.ID)
	if err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	if !again.LastSeen.After(hb.LastSeen) {
		t.Fatalf("expected LastSeen to advance, got %v then %v", hb.LastSeen, again.LastSeen)
	}
}

func TestNodeStale(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const timeout = 5 * time.Second

	tests := []struct {
		name    string
		node    *Node
		now     time.Time
		timeout time.Duration
		want    bool
	}{
		{
			name:    "never heartbeated is stale",
			node:    &Node{},
			now:     now,
			timeout: timeout,
			want:    true,
		},
		{
			name:    "zero last seen with zero timeout is stale",
			node:    &Node{},
			now:     now,
			timeout: 0,
			want:    true,
		},
		{
			name:    "just heartbeated is fresh",
			node:    &Node{LastSeen: now},
			now:     now,
			timeout: timeout,
			want:    false,
		},
		{
			name:    "exactly at the timeout boundary is fresh",
			node:    &Node{LastSeen: now.Add(-timeout)},
			now:     now,
			timeout: timeout,
			want:    false,
		},
		{
			name:    "just past the timeout is stale",
			node:    &Node{LastSeen: now.Add(-timeout - time.Nanosecond)},
			now:     now,
			timeout: timeout,
			want:    true,
		},
		{
			name:    "within the timeout is fresh",
			node:    &Node{LastSeen: now.Add(-timeout + time.Second)},
			now:     now,
			timeout: timeout,
			want:    false,
		},
		{
			name:    "long silence is stale",
			node:    &Node{LastSeen: now.Add(-time.Hour)},
			now:     now,
			timeout: timeout,
			want:    true,
		},
		{
			name:    "nil node is stale",
			node:    nil,
			now:     now,
			timeout: timeout,
			want:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NodeStale(tt.node, tt.now, tt.timeout); got != tt.want {
				t.Fatalf("NodeStale(%+v, %v, %v) = %v, want %v", tt.node, tt.now, tt.timeout, got, tt.want)
			}
		})
	}
}

func TestNodeDeleteInUse(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)
	is := NewInstanceService(s)

	n, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := is.Create(InstanceSpec{NodeID: n.ID, Name: "inst-1"}); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	if err := ns.Delete(n.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse deleting a node with instances, got %v", err)
	}
	// the failed delete leaves the node in place
	if _, err := ns.Get(n.ID); err != nil {
		t.Fatalf("node lost after failed delete: %v", err)
	}

	// removing the instances releases the node
	for _, inst := range is.ListByNode(n.ID) {
		if err := is.Delete(inst.ID); err != nil {
			t.Fatalf("delete instance: %v", err)
		}
	}
	if err := ns.Delete(n.ID); err != nil {
		t.Fatalf("delete after detaching instances: %v", err)
	}
}

func TestInstanceCRUD(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)
	is := NewInstanceService(s)

	n1, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	n2, err := ns.Create(NodeSpec{Name: "edge-2", Address: "h:2"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	before := time.Now()
	inst, err := is.Create(InstanceSpec{NodeID: n1.ID, Name: "main", Status: InstanceRunning, ChannelID: "ch-1"})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	after := time.Now()
	if inst.ID == "" {
		t.Fatal("expected generated id")
	}
	if inst.NodeID != n1.ID || inst.Name != "main" || inst.Status != InstanceRunning || inst.ChannelID != "ch-1" {
		t.Fatalf("unexpected instance: %+v", inst)
	}
	if inst.CreatedAt.Before(before) || inst.CreatedAt.After(after) {
		t.Fatalf("unexpected createdAt: %v", inst.CreatedAt)
	}
	if !inst.CreatedAt.Equal(inst.UpdatedAt) {
		t.Fatalf("expected createdAt == updatedAt on create, got %v vs %v", inst.CreatedAt, inst.UpdatedAt)
	}

	got, err := is.Get(inst.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "main" || got.NodeID != n1.ID {
		t.Fatalf("unexpected get: %+v", got)
	}

	// Update replaces every field, including the hosting node.
	upd, err := is.Update(inst.ID, InstanceSpec{NodeID: n2.ID, Name: "backup", Status: InstanceStopped, ChannelID: ""})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.NodeID != n2.ID || upd.Name != "backup" || upd.Status != InstanceStopped || upd.ChannelID != "" {
		t.Fatalf("unexpected update: %+v", upd)
	}
	if upd.UpdatedAt.Before(upd.CreatedAt) {
		t.Fatalf("updatedAt moved backwards: %v < %v", upd.UpdatedAt, upd.CreatedAt)
	}

	if err := is.Delete(inst.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(is.List()) != 0 {
		t.Fatal("expected empty instance list")
	}
	if _, err := is.Get(inst.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := is.Delete(inst.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
}

func TestInstanceListByNode(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)
	is := NewInstanceService(s)

	n1, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	n2, err := ns.Create(NodeSpec{Name: "edge-2", Address: "h:2"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := is.Create(InstanceSpec{NodeID: n1.ID, Name: name}); err != nil {
			t.Fatalf("create instance %q: %v", name, err)
		}
	}
	if _, err := is.Create(InstanceSpec{NodeID: n2.ID, Name: "other"}); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	got := is.ListByNode(n1.ID)
	want := []string{"alpha", "mike", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("expected %d instances, got %d", len(want), len(got))
	}
	for i, inst := range got {
		if inst.Name != want[i] {
			t.Fatalf("expected sorted order %v, got %v", want, instanceNames(got))
		}
	}
	// instances of other nodes are not included
	if got := is.ListByNode(n2.ID); len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("unexpected instances of node 2: %+v", got)
	}
	// an unknown node has no instances
	if got := is.ListByNode("missing"); len(got) != 0 {
		t.Fatalf("expected empty list for unknown node, got %+v", got)
	}
	// List returns every instance
	if got := is.List(); len(got) != 4 {
		t.Fatalf("expected 4 instances in List, got %d", len(got))
	}
}

func TestInstanceNodeValidation(t *testing.T) {
	s := newTestStore(t)
	is := NewInstanceService(s)

	// empty names are rejected
	for _, name := range []string{"", "   "} {
		if _, err := is.Create(InstanceSpec{NodeID: "n1", Name: name}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected ErrInvalid for name %q, got %v", name, err)
		}
	}
	// unknown statuses are rejected
	if _, err := is.Create(InstanceSpec{NodeID: "n1", Name: "x", Status: InstanceStatus("booting")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown status, got %v", err)
	}
	// a missing (or empty) NodeID is ErrNotFound
	if _, err := is.Create(InstanceSpec{NodeID: "missing", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing node, got %v", err)
	}
	if _, err := is.Create(InstanceSpec{NodeID: "", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for empty node, got %v", err)
	}

	ns := NewNodeService(s)
	n, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	inst, err := is.Create(InstanceSpec{NodeID: n.ID, Name: "main"})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if inst.Status != InstanceUnknown {
		t.Fatalf("expected default status unknown, got %q", inst.Status)
	}

	// update validates like create
	if _, err := is.Update(inst.ID, InstanceSpec{NodeID: n.ID, Name: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for empty update name, got %v", err)
	}
	if _, err := is.Update(inst.ID, InstanceSpec{NodeID: "missing", Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing update node, got %v", err)
	}
	if _, err := is.Update(inst.ID, InstanceSpec{NodeID: n.ID, Name: "x", Status: InstanceStatus("booting")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown update status, got %v", err)
	}
	// missing id on every mutator
	if _, err := is.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing get, got %v", err)
	}
	if _, err := is.Update("missing", InstanceSpec{NodeID: n.ID, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing update, got %v", err)
	}
	if err := is.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing delete, got %v", err)
	}
	if _, err := is.SetStatus("missing", InstanceRunning); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing set-status, got %v", err)
	}
}

func TestInstanceSetStatus(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)
	is := NewInstanceService(s)

	n, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	inst, err := is.Create(InstanceSpec{NodeID: n.ID, Name: "main"})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}

	got, err := is.SetStatus(inst.ID, InstanceRunning)
	if err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got.Status != InstanceRunning {
		t.Fatalf("expected running, got %q", got.Status)
	}
	if _, err := is.SetStatus(inst.ID, InstanceStopped); err != nil {
		t.Fatalf("set status: %v", err)
	}
	// an empty status defaults to unknown
	got, err = is.SetStatus(inst.ID, "")
	if err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got.Status != InstanceUnknown {
		t.Fatalf("expected unknown, got %q", got.Status)
	}
	if _, err := is.SetStatus(inst.ID, InstanceStatus("booting")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for unknown status, got %v", err)
	}
}

// TestInstanceSetStatusNoop verifies the no-op semantics of SetStatus:
// setting the status an instance already has succeeds but does not rewrite
// the store file (and so does not bump root UpdatedAt), mirroring the
// alarm Resolve no-op.
func TestInstanceSetStatusNoop(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)
	is := NewInstanceService(s)

	n, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	inst, err := is.Create(InstanceSpec{NodeID: n.ID, Name: "main", Status: InstanceStopped})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := is.SetStatus(inst.ID, InstanceRunning); err != nil {
		t.Fatalf("set status: %v", err)
	}
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}

	// the same status again is a no-op update
	got, err := is.SetStatus(inst.ID, InstanceRunning)
	if err != nil {
		t.Fatalf("re-set status: %v", err)
	}
	if got.Status != InstanceRunning {
		t.Fatalf("unexpected status: %q", got.Status)
	}
	after, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("no-op SetStatus rewrote the store file")
	}
}

func TestNodeInstanceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ns := NewNodeService(s)
	is := NewInstanceService(s)

	n, err := ns.Create(NodeSpec{Name: "edge-1", Address: "h:1", Status: NodeOffline, Enabled: true})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	inst, err := is.Create(InstanceSpec{NodeID: n.ID, Name: "main", Status: InstanceRunning, ChannelID: "ch-1"})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := ns.Heartbeat(n.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	reopened, err := OpenStore(s.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	gotNode, err := NewNodeService(reopened).Get(n.ID)
	if err != nil {
		t.Fatalf("node lost after reopen: %v", err)
	}
	if gotNode.Name != "edge-1" || gotNode.Address != "h:1" || gotNode.Status != NodeOnline || !gotNode.Enabled {
		t.Fatalf("unexpected node after reopen: %+v", gotNode)
	}
	if gotNode.LastSeen.IsZero() || !gotNode.LastSeen.Equal(gotNode.UpdatedAt) {
		t.Fatalf("heartbeat did not survive reopen: %+v", gotNode)
	}
	gotInst, err := NewInstanceService(reopened).Get(inst.ID)
	if err != nil {
		t.Fatalf("instance lost after reopen: %v", err)
	}
	if gotInst.NodeID != n.ID || gotInst.Name != "main" || gotInst.Status != InstanceRunning || gotInst.ChannelID != "ch-1" {
		t.Fatalf("unexpected instance after reopen: %+v", gotInst)
	}

	// the collections are persisted under their camelCase keys
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"nodes"`, `"instances"`, `"remoteCommands"`, `"nodeId"`, `"channelId"`, `"lastSeen"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("store file lacks %s: %s", key, raw)
		}
	}
}

// nodeNames returns the names of the nodes in order.
func nodeNames(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

// instanceNames returns the names of the instances in order.
func instanceNames(instances []*Instance) []string {
	out := make([]string, 0, len(instances))
	for _, inst := range instances {
		out = append(out, inst.Name)
	}
	return out
}
