package module

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bytelang/kplayer/types"
	kpproto "github.com/bytelang/kplayer/types/core/proto"
	"github.com/spf13/cobra"
	"sync"
)

type ModuleOption int

const (
	ModuleOptionGenerateCache ModuleOption = iota
)

type KeeperContext struct {
	id        string
	action    kpproto.EventMessageAction
	ch        chan string
	validator func(msg string) bool
	dirty     bool
	// closeState is shared by every copy of a KeeperContext (the value
	// returned by NewKeeperContext is copied both into the caller's local
	// variable and into ModuleKeeper.keeper), so Close is idempotent no
	// matter which copy it is invoked on.
	closeState *keeperCloseState
}

// keeperCloseState guards channel close. It lives behind a pointer so that
// value copies of KeeperContext share the same close state; the mutex also
// serializes Close against Trigger's channel send, which prevents
// "send on closed channel" panics.
type keeperCloseState struct {
	mu     sync.Mutex
	closed bool
}

func NewKeeperContext(id string, action kpproto.EventMessageAction, validator func(msg string) bool) KeeperContext {
	return KeeperContext{
		id:         id,
		action:     action,
		ch:         make(chan string),
		validator:  validator,
		dirty:      false,
		closeState: &keeperCloseState{},
	}
}

// Close closes the keeper channel. It is idempotent: repeated or concurrent
// calls — on the original value or on any of its copies — close the channel
// exactly once and never panic.
func (kc *KeeperContext) Close() {
	if kc.closeState == nil {
		close(kc.ch)
		return
	}
	kc.closeState.mu.Lock()
	defer kc.closeState.mu.Unlock()
	if !kc.closeState.closed {
		kc.closeState.closed = true
		close(kc.ch)
	}
}

// WaitContext blocks until a keeper message arrives, the keeper channel is
// closed, or ctx is canceled. It returns nil when a message arrives or the
// channel is closed (the same semantics as Wait), and ctx.Err() when ctx is
// canceled.
func (kc *KeeperContext) WaitContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-kc.ch:
		return nil
	}
}

// Wait blocks until a keeper message arrives or the keeper channel is closed.
func (kc KeeperContext) Wait() {
	_ = kc.WaitContext(context.Background())
}

// isClosed reports whether Close has been called on this keeper context.
func (kc *KeeperContext) isClosed() bool {
	if kc.closeState == nil {
		return false
	}
	kc.closeState.mu.Lock()
	defer kc.closeState.mu.Unlock()
	return kc.closeState.closed
}

// trySend delivers body to the keeper channel unless the context has been
// closed. It returns true when the message was delivered. The close state
// mutex is held across the send so that Close cannot close the channel while
// a send is still in flight (which would panic with "send on closed
// channel").
func (kc *KeeperContext) trySend(body string) bool {
	if kc.closeState == nil {
		kc.ch <- body
		return true
	}
	kc.closeState.mu.Lock()
	defer kc.closeState.mu.Unlock()
	if kc.closeState.closed {
		return false
	}
	kc.ch <- body
	return true
}

func (kc KeeperContext) GetId() string {
	return kc.id
}

type ModuleKeeper struct {
	keeper       []KeeperContext
	triggerMutex sync.Mutex
}

func (m *ModuleKeeper) GetKeeperContext(id string) *KeeperContext {
	for i := range m.keeper {
		if m.keeper[i].id == id {
			// return a pointer to the element stored in the slice, not to the
			// range loop variable copy (which would be detached from
			// m.keeper and make mutations through it invisible)
			return &m.keeper[i]
		}
	}

	return nil
}

func (m *ModuleKeeper) RegisterKeeperChannel(ctx KeeperContext) error {
	m.triggerMutex.Lock()
	defer m.triggerMutex.Unlock()

	if m.GetKeeperContext(ctx.id) != nil {
		return fmt.Errorf("id has existed: %s", ctx.id)
	}
	m.keeper = append(m.keeper, ctx)

	return nil
}

func (m *ModuleKeeper) Trigger(message *kpproto.KPMessage) {
	m.triggerMutex.Lock()
	defer m.triggerMutex.Unlock()

	// delete dirty or closed object
	washingKeeper := []KeeperContext{}
	for _, item := range m.keeper {
		if !item.dirty && !item.isClosed() {
			washingKeeper = append(washingKeeper, item)
		}
	}
	m.keeper = washingKeeper

	for key, item := range m.keeper {
		if item.action == message.Action {
			if item.validator(message.Body) {
				if item.trySend(message.Body) {
					m.keeper[key].dirty = true
				}
			}
		}
	}
}

type BasicAppModule interface {
	RegisterKeeperChannel(ctx KeeperContext) error
	GetKeeperContext(id string) *KeeperContext
	ParseMessage(message *kpproto.KPMessage)
	TriggerMessage(message *kpproto.KPMessage)
}

type AppModule interface {
	BasicAppModule
	GetModuleName() string
	GetCommand() *cobra.Command
	InitConfig(ctx *types.ClientContext, cfg json.RawMessage) (interface{}, error)
	ValidateConfig() error
	BeginRunning(...ModuleOption)
	EndRunning(...ModuleOption)
}

type ModuleManager struct {
	Modules         map[string]AppModule
	OrderInitConfig []string
}

func NewModuleManager(modules ...AppModule) ModuleManager {
	moduleMap := ModuleManager{
		Modules: make(map[string]AppModule, 0),
	}

	for _, module := range modules {
		moduleMap.Modules[module.GetModuleName()] = module
	}

	return moduleMap
}

func (mm *ModuleManager) GetModule(name string) AppModule {
	m := mm.Modules[name]
	return m
}

func (mm *ModuleManager) SetOrderInitConfig(order ...string) {
	mm.OrderInitConfig = order
}
