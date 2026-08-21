package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// NodeStatus is the connectivity state of a managed node.
type NodeStatus string

const (
	// NodeOnline marks a node that is currently reachable.
	NodeOnline NodeStatus = "online"
	// NodeOffline marks a node that is known but currently unreachable.
	NodeOffline NodeStatus = "offline"
	// NodeUnknown is the default state of a node that has never been
	// observed.
	NodeUnknown NodeStatus = "unknown"
)

// Node is one managed playback host of the platform. Address is a
// "host:port" pair or a URL; its format is interpreted loosely by the
// consumer and the management side only stores it. LastSeen is the time of
// the last heartbeat; its zero value means the node has never reported
// one.
type Node struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Address   string     `json:"address"`
	Status    NodeStatus `json:"status"`
	LastSeen  time.Time  `json:"lastSeen,omitempty"`
	Enabled   bool       `json:"enabled"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// NodeSpec is the validated input used to create or replace a node. The
// name and address must be non-empty; an empty status defaults to
// NodeUnknown.
type NodeSpec struct {
	Name    string
	Address string
	Status  NodeStatus
	Enabled bool
}

// NodeService provides CRUD and heartbeat tracking over the nodes of a
// Store.
type NodeService struct {
	store *Store
}

// NewNodeService returns a NodeService backed by store.
func NewNodeService(store *Store) *NodeService {
	return &NodeService{store: store}
}

// List returns all nodes sorted by name.
func (ns *NodeService) List() []*Node {
	out := make([]*Node, 0)
	ns.store.View(func(d *Data) {
		out = append(out, d.Nodes...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the node with the given id.
func (ns *NodeService) Get(id string) (*Node, error) {
	var found *Node
	ns.store.View(func(d *Data) {
		for _, n := range d.Nodes {
			if n.ID == id {
				found = n
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("node %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new node from spec. The name and address must be non-empty
// (ErrInvalid) and the name must be unique among nodes (ErrExists). A node
// is created without a heartbeat: Status defaults to NodeUnknown and
// LastSeen stays zero until Heartbeat stamps it.
func (ns *NodeService) Create(spec NodeSpec) (*Node, error) {
	spec, err := normalizeNodeSpec(spec)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	n := &Node{
		ID:        newID(),
		Name:      spec.Name,
		Address:   spec.Address,
		Status:    spec.Status,
		Enabled:   spec.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err = ns.store.Update(func(d *Data) error {
		for _, exist := range d.Nodes {
			if exist.Name == n.Name {
				return fmt.Errorf("node %q: %w", n.Name, ErrExists)
			}
		}
		d.Nodes = append(d.Nodes, n)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return n, nil
}

// Update replaces the configuration of the node with the given id from
// spec: name, address, status and the enabled flag are all replaced (the
// spec's zero Status becomes NodeUnknown, so a full replacement must pass
// the desired status explicitly). The new name must be non-empty
// (ErrInvalid) and must not collide with another node (ErrExists);
// renaming to its own current name is allowed. LastSeen is left untouched:
// only Heartbeat stamps it. It returns the updated node.
func (ns *NodeService) Update(id string, spec NodeSpec) (*Node, error) {
	spec, err := normalizeNodeSpec(spec)
	if err != nil {
		return nil, err
	}
	var out *Node
	err = ns.store.Update(func(d *Data) error {
		var n *Node
		for _, cand := range d.Nodes {
			if cand.ID == id {
				n = cand
				break
			}
		}
		if n == nil {
			return fmt.Errorf("node %q: %w", id, ErrNotFound)
		}
		for _, exist := range d.Nodes {
			if exist.ID != id && exist.Name == spec.Name {
				return fmt.Errorf("node %q: %w", spec.Name, ErrExists)
			}
		}
		n.Name = spec.Name
		n.Address = spec.Address
		n.Status = spec.Status
		n.Enabled = spec.Enabled
		n.UpdatedAt = time.Now()
		out = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetEnabled toggles the enabled flag of the node with the given id.
// Setting a flag to its current value is not special-cased: like the other
// SetEnabled implementations of the package it rewrites the store and
// bumps UpdatedAt.
func (ns *NodeService) SetEnabled(id string, enabled bool) error {
	return ns.update(id, func(n *Node) error {
		n.Enabled = enabled
		return nil
	})
}

// Delete removes the node with the given id. A node that still has
// instances attached cannot be deleted (ErrInUse).
func (ns *NodeService) Delete(id string) error {
	return ns.store.Update(func(d *Data) error {
		idx := -1
		for i, n := range d.Nodes {
			if n.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("node %q: %w", id, ErrNotFound)
		}
		for _, inst := range d.Instances {
			if inst.NodeID == id {
				return fmt.Errorf("node %q is referenced by instance %q: %w", id, inst.Name, ErrInUse)
			}
		}
		d.Nodes = append(d.Nodes[:idx], d.Nodes[idx+1:]...)
		return nil
	})
}

// Heartbeat records a heartbeat from the node with the given id: the node
// is marked online and LastSeen and UpdatedAt are stamped with the current
// time. Everything else (including the enabled flag) is left untouched. A
// missing node returns ErrNotFound.
func (ns *NodeService) Heartbeat(id string) (*Node, error) {
	now := time.Now()
	var out *Node
	err := ns.store.Update(func(d *Data) error {
		for _, n := range d.Nodes {
			if n.ID != id {
				continue
			}
			n.Status = NodeOnline
			n.LastSeen = now
			n.UpdatedAt = now
			out = n
			return nil
		}
		return fmt.Errorf("node %q: %w", id, ErrNotFound)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// NodeStale reports whether a node should be considered offline for lack
// of heartbeats. It is a pure function: a nil node, or a node whose
// LastSeen is zero (never heartbeated), is always stale; otherwise the
// node is stale when now.Sub(LastSeen) > timeout, so a heartbeat exactly
// timeout old is still fresh.
func NodeStale(node *Node, now time.Time, timeout time.Duration) bool {
	if node == nil || node.LastSeen.IsZero() {
		return true
	}
	return now.Sub(node.LastSeen) > timeout
}

// update applies fn to the node with the given id under the store write
// lock; fn may mutate the node in place. Returning an error rolls back.
func (ns *NodeService) update(id string, fn func(n *Node) error) error {
	return ns.store.Update(func(d *Data) error {
		for _, n := range d.Nodes {
			if n.ID != id {
				continue
			}
			if err := fn(n); err != nil {
				return err
			}
			n.UpdatedAt = time.Now()
			return nil
		}
		return fmt.Errorf("node %q: %w", id, ErrNotFound)
	})
}

// normalizeNodeSpec performs field-level validation independent of the
// store and returns the normalized spec: the name and address are trimmed
// and an empty status defaults to NodeUnknown.
func normalizeNodeSpec(spec NodeSpec) (NodeSpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return spec, fmt.Errorf("node: %w: empty name", ErrInvalid)
	}
	spec.Address = strings.TrimSpace(spec.Address)
	if spec.Address == "" {
		return spec, fmt.Errorf("node: %w: empty address", ErrInvalid)
	}
	if spec.Status == "" {
		spec.Status = NodeUnknown
	}
	if err := validateNodeStatus(spec.Status); err != nil {
		return spec, err
	}
	return spec, nil
}

// validateNodeStatus rejects statuses outside the NodeStatus set.
func validateNodeStatus(s NodeStatus) error {
	switch s {
	case NodeOnline, NodeOffline, NodeUnknown:
		return nil
	}
	return fmt.Errorf("node: %w: unknown status %q", ErrInvalid, s)
}

// InstanceStatus is the runtime state of an instance on a node.
type InstanceStatus string

const (
	// InstanceRunning marks an instance whose playback process is up.
	InstanceRunning InstanceStatus = "running"
	// InstanceStopped marks an instance whose playback process is down.
	InstanceStopped InstanceStatus = "stopped"
	// InstanceUnknown is the default state of an instance whose runtime
	// state has not been observed.
	InstanceUnknown InstanceStatus = "unknown"
)

// Instance is one playback instance running on a node. NodeID references
// the hosting node; ChannelID is a free-form channel binding string
// interpreted by the consumer.
type Instance struct {
	ID        string         `json:"id"`
	NodeID    string         `json:"nodeId"`
	Name      string         `json:"name"`
	Status    InstanceStatus `json:"status"`
	ChannelID string         `json:"channelId,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

// InstanceSpec is the validated input used to create or replace an
// instance. The name must be non-empty and the NodeID must reference an
// existing node; an empty status defaults to InstanceUnknown.
type InstanceSpec struct {
	NodeID    string
	Name      string
	Status    InstanceStatus
	ChannelID string
}

// InstanceService provides CRUD over the instances of a Store.
type InstanceService struct {
	store *Store
}

// NewInstanceService returns an InstanceService backed by store.
func NewInstanceService(store *Store) *InstanceService {
	return &InstanceService{store: store}
}

// List returns all instances sorted by name.
func (is *InstanceService) List() []*Instance {
	out := make([]*Instance, 0)
	is.store.View(func(d *Data) {
		out = append(out, d.Instances...)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListByNode returns the instances of the node with the given id, sorted
// by name. An unknown node yields an empty list.
func (is *InstanceService) ListByNode(nodeID string) []*Instance {
	out := make([]*Instance, 0)
	is.store.View(func(d *Data) {
		for _, inst := range d.Instances {
			if inst.NodeID == nodeID {
				out = append(out, inst)
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the instance with the given id.
func (is *InstanceService) Get(id string) (*Instance, error) {
	var found *Instance
	is.store.View(func(d *Data) {
		for _, inst := range d.Instances {
			if inst.ID == id {
				found = inst
				return
			}
		}
	})
	if found == nil {
		return nil, fmt.Errorf("instance %q: %w", id, ErrNotFound)
	}
	return found, nil
}

// Create adds a new instance from spec. The name must be non-empty
// (ErrInvalid) and the NodeID must reference an existing node
// (ErrNotFound). Instance names are not required to be unique: the same
// node may run several instances sharing a name, and names may repeat
// across nodes.
func (is *InstanceService) Create(spec InstanceSpec) (*Instance, error) {
	spec, err := normalizeInstanceSpec(spec)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	inst := &Instance{
		ID:        newID(),
		NodeID:    spec.NodeID,
		Name:      spec.Name,
		Status:    spec.Status,
		ChannelID: spec.ChannelID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err = is.store.Update(func(d *Data) error {
		if findNode(d, spec.NodeID) == nil {
			return fmt.Errorf("instance %q: node %q: %w", spec.Name, spec.NodeID, ErrNotFound)
		}
		d.Instances = append(d.Instances, inst)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inst, nil
}

// Update replaces the configuration of the instance with the given id from
// spec: the hosting node, name, status and the channel binding are all
// replaced. The new NodeID must reference an existing node (ErrNotFound)
// and the name must be non-empty (ErrInvalid). It returns the updated
// instance.
func (is *InstanceService) Update(id string, spec InstanceSpec) (*Instance, error) {
	spec, err := normalizeInstanceSpec(spec)
	if err != nil {
		return nil, err
	}
	var out *Instance
	err = is.store.Update(func(d *Data) error {
		var inst *Instance
		for _, cand := range d.Instances {
			if cand.ID == id {
				inst = cand
				break
			}
		}
		if inst == nil {
			return fmt.Errorf("instance %q: %w", id, ErrNotFound)
		}
		if findNode(d, spec.NodeID) == nil {
			return fmt.Errorf("instance %q: node %q: %w", id, spec.NodeID, ErrNotFound)
		}
		inst.NodeID = spec.NodeID
		inst.Name = spec.Name
		inst.Status = spec.Status
		inst.ChannelID = spec.ChannelID
		inst.UpdatedAt = time.Now()
		out = inst
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes the instance with the given id.
func (is *InstanceService) Delete(id string) error {
	return is.store.Update(func(d *Data) error {
		idx := -1
		for i, inst := range d.Instances {
			if inst.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("instance %q: %w", id, ErrNotFound)
		}
		d.Instances = append(d.Instances[:idx], d.Instances[idx+1:]...)
		return nil
	})
}

// SetStatus sets the runtime status of the instance with the given id. An
// empty status defaults to InstanceUnknown. Setting the status the
// instance already has is a no-op: the instance is returned unchanged and
// the store is not rewritten (root UpdatedAt is not bumped), mirroring the
// alarm Resolve no-op.
func (is *InstanceService) SetStatus(id string, status InstanceStatus) (*Instance, error) {
	if status == "" {
		status = InstanceUnknown
	}
	if err := validateInstanceStatus(status); err != nil {
		return nil, err
	}
	now := time.Now()
	var out *Instance
	err := is.store.Update(func(d *Data) error {
		for _, inst := range d.Instances {
			if inst.ID != id {
				continue
			}
			if inst.Status == status {
				out = inst
				return errNoop
			}
			inst.Status = status
			inst.UpdatedAt = now
			out = inst
			return nil
		}
		return fmt.Errorf("instance %q: %w", id, ErrNotFound)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeInstanceSpec performs field-level validation independent of the
// store and returns the normalized spec: the name is trimmed and an empty
// status defaults to InstanceUnknown. The NodeID is checked against the
// store by Create and Update, since it must reference an existing node.
func normalizeInstanceSpec(spec InstanceSpec) (InstanceSpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return spec, fmt.Errorf("instance: %w: empty name", ErrInvalid)
	}
	if spec.Status == "" {
		spec.Status = InstanceUnknown
	}
	if err := validateInstanceStatus(spec.Status); err != nil {
		return spec, err
	}
	return spec, nil
}

// validateInstanceStatus rejects statuses outside the InstanceStatus set.
func validateInstanceStatus(s InstanceStatus) error {
	switch s {
	case InstanceRunning, InstanceStopped, InstanceUnknown:
		return nil
	}
	return fmt.Errorf("instance: %w: unknown status %q", ErrInvalid, s)
}

// findNode returns the node with the given id in d, or nil.
func findNode(d *Data, id string) *Node {
	for _, n := range d.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}
