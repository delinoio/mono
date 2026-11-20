package ownsand

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
)

// streamIdleTimeout is how long a relay server stream may receive no byte
// of its response body before the SDK treats it as a transport interruption.
const streamIdleTimeout = 90 * time.Second

// errStreamIdle is the cancel cause of a stream that received no byte for
// streamIdleTimeout.
var errStreamIdle = errors.New("relay stream received no byte for 90 seconds")

// activity records when a response body last delivered a byte. Counting
// bytes, not complete messages, keeps a large chunk on a slow link alive.
type activity struct {
	clk  clock.Clock
	mu   sync.Mutex
	last time.Time
}

type activityKey struct{}

func withActivity(ctx context.Context, a *activity) context.Context {
	return context.WithValue(ctx, activityKey{}, a)
}

func activityFrom(ctx context.Context) *activity {
	a, _ := ctx.Value(activityKey{}).(*activity)
	return a
}

func (a *activity) touch() {
	a.mu.Lock()
	a.last = a.clk.Now()
	a.mu.Unlock()
}

func (a *activity) idleFor() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clk.Now().Sub(a.last)
}

type activityBody struct {
	io.ReadCloser
	a *activity
}

func (b *activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.a.touch()
	}
	return n, err
}

// watchIdle returns a context that ends with errStreamIdle when the stream
// receives no byte for streamIdleTimeout. The returned stop function ends
// the watchdog.
func watchIdle(ctx context.Context, clk clock.Clock) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	a := &activity{clk: clk, last: clk.Now()}
	ctx = withActivity(ctx, a)
	go func() {
		wait := streamIdleTimeout
		for {
			t := clk.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C():
			}
			idle := a.idleFor()
			if idle >= streamIdleTimeout {
				cancel(errStreamIdle)
				return
			}
			wait = streamIdleTimeout - idle
		}
	}()
	return ctx, cancel
}
