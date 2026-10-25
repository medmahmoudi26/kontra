// Package slowagent is the e2e upstream: an A2A agent written with a2a-go v2.7.0 (local mode) that
// streams Chunks artifact chunks Interval apart and then completes.
//
// It keeps running its task when the gate's streaming connection drops (a2a-go runs executions detached
// from the request), serves GetTask and SubscribeToTask so a re-attaching dispatch can follow it (D42),
// and counts SendMessage and SendStreamingMessage calls so a test can prove the gate never sent the
// message twice.
package slowagent

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/unimpl"
)

// Config shapes the agent.
type Config struct {
	Chunks    int           // artifact chunks before completed
	Interval  time.Duration // between chunks
	Streaming bool          // the card says capabilities.streaming
	Binding   string        // "JSONRPC" or "HTTP+JSON"
}

// Agent is one running upstream.
type Agent struct {
	cfg   Config
	sends atomic.Int64
}

// New returns an agent; Start serves it.
func New(cfg Config) *Agent {
	return &Agent{cfg: cfg}
}

// Start serves the agent on 127.0.0.1 and returns its base URL. It stops when ctx ends.
func (a *Agent) Start(ctx context.Context) (string, error) {
	return "", unimpl.Error("slowagent.Agent.Start", "WP-09")
}

// Sends is the number of SendMessage and SendStreamingMessage calls the agent received.
func (a *Agent) Sends() int64 {
	return a.sends.Load()
}

// Executor is the agent's a2asrv.AgentExecutor.
func (a *Agent) Executor() a2asrv.AgentExecutor {
	panic(unimpl.Error("slowagent.Agent.Executor", "WP-09"))
}
