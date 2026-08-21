package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bytelang/kplayer/management"
	outputprovider "github.com/bytelang/kplayer/module/output/provider"
	svrproto "github.com/bytelang/kplayer/types/server"
)

// outputFailoverAdapter adapts the output provider to the FailoverMonitor
// interfaces from the management package: it is both the OutputStateReader
// (connectivity snapshot from OutputList) and the FailoverSwitcher (output
// remove + re-add as the activation action).
//
// Every provider call is bounded by a short timeout: the monitor tick loop
// runs on a single goroutine and Stop waits for the loop, so an unbounded
// provider call would freeze both the failover decisions and the server
// shutdown. 2s matches the per-read timeout used by the /status endpoint
// (callWithTimeout).
type outputFailoverAdapter struct {
	output outputprovider.ProviderI
}

// OutputStates reports the connectivity snapshot of every configured output.
// Connected maps directly from the provider and Unique is the output unique;
// the monitor treats an output missing from the report as disconnected.
func (a *outputFailoverAdapter) OutputStates() ([]management.OutputState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := a.output.OutputList(ctx, &svrproto.OutputListArgs{})
	if err != nil {
		return nil, err
	}
	if reply == nil {
		return nil, fmt.Errorf("output list returned no reply")
	}
	states := make([]management.OutputState, 0, len(reply.Outputs))
	for _, out := range reply.Outputs {
		if out == nil {
			continue
		}
		states = append(states, management.OutputState{Unique: out.Unique, Connected: out.Connected})
	}
	return states, nil
}

// ActivateOutput makes unique the active output of the pipeline by cycling
// it: the output is removed and re-added with its path, which restarts the
// stream. Removing an output that is not in the list is harmless (it was
// never active) and ignored; re-adding an output that already exists is also
// treated as success, so re-issued switches (the monitor retries a failed
// call and Start resets its in-memory state) stay idempotent.
//
// The path is resolved from the current output list because OutputAdd
// requires one and the management side has no other registry of output
// paths. A target missing from the list (for example a backup output that
// was never added to the pipeline) has no path, so the activation fails; the
// monitor reports the error and retries on the next tick.
func (a *outputFailoverAdapter) ActivateOutput(unique string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reply, err := a.output.OutputList(ctx, &svrproto.OutputListArgs{})
	if err != nil {
		return fmt.Errorf("list outputs: %w", err)
	}
	path := ""
	if reply != nil {
		for _, out := range reply.Outputs {
			if out != nil && out.Unique == unique {
				path = out.Path
				break
			}
		}
	}
	if path == "" {
		return fmt.Errorf("output %q is not in the output list; cannot activate it", unique)
	}

	if _, err := a.output.OutputRemove(ctx, &svrproto.OutputRemoveArgs{Unique: unique}); err != nil && !errors.Is(err, outputprovider.OutputUniqueNotFound) {
		return fmt.Errorf("remove output %q: %w", unique, err)
	}
	if _, err := a.output.OutputAdd(ctx, &svrproto.OutputAddArgs{Path: path, Unique: unique}); err != nil && !errors.Is(err, outputprovider.OutputUniqueHasExisted) {
		return fmt.Errorf("add output %q: %w", unique, err)
	}
	return nil
}
