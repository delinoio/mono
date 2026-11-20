// Package guestconn is the agent side of the guest channel: it multiplexes
// request streams over one hypervisor socket connection to the guest helper,
// according to the guest channel wire details in the agent contract.
package guestconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-guest/guest"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrClosed reports a closed guest connection.
var ErrClosed = errors.New("guest connection closed")

// Client is one connection to a guest helper.
type Client struct {
	rwc io.ReadWriteCloser
	log *slog.Logger

	wmu sync.Mutex
	fw  *datapath.FrameWriter

	mu      sync.Mutex
	next    uint64
	streams map[uint64]*Stream
	ready   []chan *ownsandv1.GuestReadyResponse
	closed  bool
	done    chan struct{}
}

// New starts a Client on rwc.
func New(rwc io.ReadWriteCloser, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	c := &Client{rwc: rwc, log: log, fw: datapath.NewFrameWriter(rwc), streams: map[uint64]*Stream{}, done: make(chan struct{})}
	go c.readLoop()
	return c
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close closes the connection and ends every stream.
func (c *Client) Close() error {
	err := c.rwc.Close()
	c.shutdown()
	return err
}

func (c *Client) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	streams := c.streams
	c.streams = map[uint64]*Stream{}
	c.mu.Unlock()
	for _, s := range streams {
		s.fail(ErrClosed)
	}
	close(c.done)
}

func (c *Client) readLoop() {
	defer c.shutdown()
	fr := datapath.NewFrameReader(c.rwc)
	for {
		f := &ownsandv1.GuestFrame{}
		if err := fr.Read(f); err != nil {
			return
		}
		switch p := f.GetFrame().(type) {
		case *ownsandv1.GuestFrame_Control:
			if r := p.Control.GetReadyResponse(); r != nil {
				c.mu.Lock()
				waiters := c.ready
				c.ready = nil
				c.mu.Unlock()
				for _, w := range waiters {
					w <- r
				}
			}
		case *ownsandv1.GuestFrame_Data:
			c.mu.Lock()
			s := c.streams[p.Data.GetStreamId()]
			c.mu.Unlock()
			if s != nil {
				s.deliver(p.Data)
			}
		}
	}
}

func (c *Client) send(f *ownsandv1.GuestFrame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.fw.Write(f); err != nil {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return nil
}

// Ready sends a readiness request and waits for the response.
func (c *Client) Ready(ctx context.Context) (*ownsandv1.GuestReadyResponse, error) {
	ch := make(chan *ownsandv1.GuestReadyResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.ready = append(c.ready, ch)
	c.mu.Unlock()
	if err := c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Control{Control: &ownsandv1.GuestControl{
		Message: &ownsandv1.GuestControl_ReadyRequest{ReadyRequest: &ownsandv1.GuestReadyRequest{}},
	}}}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-c.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SetClock sets the guest clock from the host clock.
func (c *Client) SetClock(now time.Time) error {
	return c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Control{Control: &ownsandv1.GuestControl{
		Message: &ownsandv1.GuestControl_SetClock{SetClock: &ownsandv1.SetClockRequest{Now: timestamppb.New(now)}},
	}}})
}

// Open sends a request frame on a new stream. The frame's stream_id is
// replaced with the guest stream ID.
func (c *Client) Open(req *ownsandv1.DataFrame) (*Stream, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.next++
	s := &Stream{c: c, id: c.next, frames: make(chan *ownsandv1.DataFrame, 64), done: make(chan struct{})}
	c.streams[s.id] = s
	c.mu.Unlock()
	f := &ownsandv1.DataFrame{StreamId: s.id, SandboxId: req.GetSandboxId(), Kind: req.GetKind(), Payload: req.GetPayload()}
	if err := c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Data{Data: f}}); err != nil {
		c.forget(s.id)
		return nil, err
	}
	return s, nil
}

func (c *Client) forget(id uint64) {
	c.mu.Lock()
	delete(c.streams, id)
	c.mu.Unlock()
}

// Stream is one request stream to the guest helper.
type Stream struct {
	c  *Client
	id uint64

	frames chan *ownsandv1.DataFrame
	mu     sync.Mutex
	err    error
	done   chan struct{}
	once   sync.Once
	queued []*ownsandv1.DataFrame
}

func (s *Stream) deliver(f *ownsandv1.DataFrame) {
	// The guest sends at most its credit window per stream, so the queue is
	// bounded by guest.InitialWindow plus the returned credit.
	s.mu.Lock()
	s.queued = append(s.queued, f)
	s.mu.Unlock()
	select {
	case s.frames <- nil:
	default:
	}
}

func (s *Stream) fail(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

// Recv returns the next frame of the stream. It returns the credit of a
// payload frame to the guest once the caller received it. The caller must
// stop reading after a final frame.
func (s *Stream) Recv(ctx context.Context) (*ownsandv1.DataFrame, error) {
	for {
		s.mu.Lock()
		if len(s.queued) > 0 {
			f := s.queued[0]
			s.queued = s.queued[1:]
			s.mu.Unlock()
			if datapath.PayloadUsesCredit(f) {
				s.grant(datapath.FrameSize(f))
			}
			return f, nil
		}
		err := s.err
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		select {
		case <-s.frames:
		case <-s.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *Stream) grant(n int64) {
	_ = s.c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Data{Data: datapath.NewFrame(s.id, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: n}}
	})}})
}

// Close ends the stream: the guest releases its output subscription. It does
// not stop an exec or end a read snapshot or a write.
func (s *Stream) Close() {
	s.c.forget(s.id)
	s.fail(ErrClosed)
	_ = s.c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Data{Data: datapath.NewFrame(s.id, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayStreamClose{RelayStreamClose: &ownsandv1.RelayStreamClose{}}
	})}})
}

// InitialWindow is the guest's first window per stream.
const InitialWindow = guest.InitialWindow
