// Package channel runs the agent channel: a Connect bidirectional stream
// AgentService.Channel over HTTP/2, opened with a fresh osd_ device session
// credential. It sends a heartbeat every 15 seconds, treats the channel as
// lost when no heartbeat acknowledgement of the current channel arrives for
// 45 seconds, and reconnects with back-off.
package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/controlplane"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

const (
	HeartbeatInterval = 15 * time.Second
	LossTimeout       = 45 * time.Second
	MinBackoff        = time.Second
	MaxBackoff        = time.Minute
	// stableSession is how long a channel must stay open before the
	// back-off resets.
	stableSession = time.Minute
	outboxSize    = 4096
)

// ErrStop ends Run without a retry (for example, after device removal).
var ErrStop = errors.New("agent channel stopped")

// errLost reports a channel without heartbeat acknowledgements.
var errLost = errors.New("no heartbeat acknowledgement for 45 seconds")

// Sender queues an envelope on the current channel. It never blocks; it
// reports false when the channel is gone or its queue is full. Updates that
// need delivery are resent until acknowledged.
type Sender func(*ownsandv1.AgentEnvelope) bool

// Handler receives the channel events. Generation identifies one channel.
type Handler interface {
	Connected(generation uint64, send Sender)
	Receive(generation uint64, env *ownsandv1.AgentEnvelope)
	Disconnected(generation uint64, err error)
}

// Config configures Run.
type Config struct {
	Client ownsandv1connect.AgentServiceClient
	// Authenticate returns a new osd_ credential for each channel. It returns
	// an error wrapping ErrStop to end Run.
	Authenticate func(ctx context.Context) (string, error)
	Handler      Handler
	Logger       *slog.Logger
	// OnConnecting is called before each attempt; optional.
	OnConnecting func()

	HeartbeatInterval time.Duration
	LossTimeout       time.Duration
	MinBackoff        time.Duration
	MaxBackoff        time.Duration
}

// Run keeps one agent channel open until ctx ends or Authenticate returns
// ErrStop.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = HeartbeatInterval
	}
	if cfg.LossTimeout <= 0 {
		cfg.LossTimeout = LossTimeout
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = MinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = MaxBackoff
	}
	backoff := cfg.MinBackoff
	var gen uint64
	for ctx.Err() == nil {
		if cfg.OnConnecting != nil {
			cfg.OnConnecting()
		}
		cred, err := cfg.Authenticate(ctx)
		if err != nil {
			if errors.Is(err, ErrStop) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			cfg.Logger.Warn("agent authentication failed", "error", err, "retry_in", backoff.String())
		} else {
			gen++
			start := time.Now()
			err = session(ctx, cfg, gen, cred)
			cfg.Logger.Info("agent channel disconnected", "generation", gen, "error", errString(err), "duration", time.Since(start).String())
			cfg.Handler.Disconnected(gen, err)
			if time.Since(start) >= stableSession {
				backoff = cfg.MinBackoff
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		// Full jitter keeps a fleet of agents from reconnecting at once after
		// a control plane restart.
		wait := cfg.MinBackoff + time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(2*backoff, cfg.MaxBackoff)
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// session runs one channel until it closes or is lost.
func session(parent context.Context, cfg Config, gen uint64, cred string) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stream := cfg.Client.Channel(ctx)
	stream.RequestHeader().Set("Authorization", controlplane.BearerHeader(cred))
	// Send the request headers at once, so the control plane registers the
	// channel before the agent's first message.
	if err := stream.Send(nil); err != nil {
		return fmt.Errorf("open agent channel: %w", err)
	}
	cfg.Logger.Info("agent channel connected", "generation", gen)

	out := make(chan *ownsandv1.AgentEnvelope, outboxSize)
	var closedMu sync.Mutex
	closed := false
	send := func(env *ownsandv1.AgentEnvelope) bool {
		closedMu.Lock()
		defer closedMu.Unlock()
		if closed {
			return false
		}
		select {
		case out <- env:
			return true
		default:
			cfg.Logger.Warn("agent channel queue is full; dropping a message", "generation", gen)
			return false
		}
	}
	defer func() {
		closedMu.Lock()
		closed = true
		closedMu.Unlock()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				_ = stream.CloseRequest()
				return
			case env := <-out:
				if err := stream.Send(env); err != nil {
					cancel(fmt.Errorf("send on agent channel: %w", err))
					return
				}
			}
		}
	}()

	// Heartbeats and the loss timer.
	var hbMu sync.Mutex
	var seq uint64
	lastAck := time.Now()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(cfg.HeartbeatInterval)
		defer t.Stop()
		check := time.NewTicker(time.Second)
		defer check.Stop()
		send(heartbeat(&hbMu, &seq))
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				send(heartbeat(&hbMu, &seq))
			case <-check.C:
				hbMu.Lock()
				since := time.Since(lastAck)
				hbMu.Unlock()
				if since >= cfg.LossTimeout {
					cancel(errLost)
					return
				}
			}
		}
	}()

	cfg.Handler.Connected(gen, send)

	recvErr := make(chan error, 1)
	go func() {
		for {
			env, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			if ack := env.GetHeartbeatAck(); ack != nil {
				hbMu.Lock()
				// Only an acknowledgement of a heartbeat of this channel resets
				// the loss timer.
				if ack.GetSequence() >= 1 && ack.GetSequence() <= seq {
					lastAck = time.Now()
				}
				hbMu.Unlock()
				continue
			}
			cfg.Handler.Receive(gen, env)
		}
	}()
	var err error
	select {
	case err = <-recvErr:
		cancel(err)
	case <-ctx.Done():
		err = context.Cause(ctx)
	}
	_ = stream.CloseResponse()
	return err
}

func heartbeat(mu *sync.Mutex, seq *uint64) *ownsandv1.AgentEnvelope {
	mu.Lock()
	defer mu.Unlock()
	*seq++
	return &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{Sequence: *seq}}}
}
