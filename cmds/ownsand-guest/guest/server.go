// Package guest implements the OwnSand guest helper protocol: the GuestService
// that runs inside every sandbox guest and serves exec and file requests from
// the device agent over the hypervisor socket.
//
// Wire format: each message on the connection is a GuestFrame encoded as a
// varint length followed by the message (the datapath codec). The agent sends
// GuestControl messages (readiness, set clock) and DataFrame requests (exec,
// exec attach, file read, file write chunk). Every DataFrame request opens a
// stream that the agent identifies with a stream_id that is unique among its
// open streams on the connection. The guest answers on the same stream_id.
//
// Flow control: the guest sends payload frames of a stream only within a
// credit window. Every stream starts with InitialWindow bytes, and the agent
// adds credit with relay window update frames on the same stream_id as it
// consumes the frames. A relay stream close frame from the agent ends the
// stream and releases its output subscription without stopping an exec or
// ending a read snapshot or a write.
package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	// DefaultVsockPort is the vsock port on which the guest helper listens.
	DefaultVsockPort = 1024
	// InitialWindow is the first credit window of every guest stream. It is
	// larger than one maximum frame so a maximum file chunk always fits.
	InitialWindow = 4 << 20
	// ExecOutputRetention is the number of bytes kept per output stream.
	ExecOutputRetention = 8 << 20
	// ExecResultRetention is how long an ended exec stays attachable.
	ExecResultRetention = 10 * time.Minute
	// ExecKillGrace is the time between SIGTERM and SIGKILL after a timeout.
	ExecKillGrace = 10 * time.Second
	// FileOperationRetention is how long a read snapshot or a write stays
	// after its last call, and how long a finished write keeps its result.
	FileOperationRetention = 15 * time.Minute
	// TombstoneRetention is how long the guest remembers ended operation IDs.
	TombstoneRetention = 30 * time.Minute
	// ExecKeepaliveInterval is the longest time an exec stream stays silent.
	ExecKeepaliveInterval = 30 * time.Second
	// readChunkSize is the data size of each file chunk frame.
	readChunkSize = datapath.MaxFileChunk
)

// Version is the guest helper version reported in readiness responses.
var Version = "0.0.0-dev"

// Config configures a Server.
type Config struct {
	// StateDir holds read snapshots. Default: /var/lib/ownsand-guest on Linux.
	StateDir string
	Logger   *slog.Logger
	// Now returns the current time. Tests replace it.
	Now func() time.Time
	// SetClock sets the guest clock. Default: the platform implementation.
	SetClock func(time.Time) error
	// KillGrace is the time between SIGTERM and SIGKILL after an exec
	// timeout. Default: ExecKillGrace. Tests shorten it.
	KillGrace time.Duration
	// OutputRetention is the number of bytes kept per output stream.
	// Default: ExecOutputRetention. Tests shorten it.
	OutputRetention int
}

// Server holds the guest state that survives agent reconnections: execs,
// read snapshots and writes.
type Server struct {
	cfg Config
	log *slog.Logger

	mu         sync.Mutex
	execs      map[string]*execRecord
	reads      map[string]*readRecord
	writes     map[string]*writeRecord
	tombstones map[string]time.Time

	janitorOnce sync.Once
}

// NewServer returns a Server.
func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SetClock == nil {
		cfg.SetClock = setSystemClock
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = ExecKillGrace
	}
	if cfg.OutputRetention <= 0 {
		cfg.OutputRetention = ExecOutputRetention
	}
	if cfg.StateDir == "" {
		cfg.StateDir = defaultStateDir()
	}
	return &Server{
		cfg:        cfg,
		log:        cfg.Logger,
		execs:      map[string]*execRecord{},
		reads:      map[string]*readRecord{},
		writes:     map[string]*writeRecord{},
		tombstones: map[string]time.Time{},
	}
}

func defaultStateDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("ProgramData"), "ownsand-guest")
	}
	return "/var/lib/ownsand-guest"
}

// Serve accepts connections from ln until ctx ends. Only one agent is
// expected, but every accepted connection is served.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			if err := s.ServeConn(ctx, c); err != nil {
				s.log.Warn("guest connection ended", "error", err)
			}
		}()
	}
}

// StartJanitor starts the retention clean-up loop. ServeConn also starts it.
func (s *Server) StartJanitor(ctx context.Context) {
	s.janitorOnce.Do(func() {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					s.sweep()
				}
			}
		}()
	})
}

// sweep drops expired execs, reads, writes and tombstones.
func (s *Server) sweep() {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.execs {
		if ended, at := e.endedAt(); ended && now.Sub(at) >= ExecResultRetention {
			delete(s.execs, id)
			s.tombstones[id] = now
		}
	}
	for id, r := range s.reads {
		if r.expired(now) {
			delete(s.reads, id)
			s.tombstones[id] = now
			r.remove()
		}
	}
	for id, w := range s.writes {
		if w.expired(now) {
			delete(s.writes, id)
			s.tombstones[id] = now
			w.discard()
		}
	}
	for id, at := range s.tombstones {
		if now.Sub(at) >= TombstoneRetention {
			delete(s.tombstones, id)
		}
	}
}

// conn is one agent connection.
type conn struct {
	s   *Server
	rwc io.ReadWriteCloser
	ctx context.Context

	wmu sync.Mutex
	fw  *datapath.FrameWriter

	smu     sync.Mutex
	streams map[uint64]*stream
}

// stream is one request stream with its credit window.
type stream struct {
	id        uint64
	sandboxID string
	ctx       context.Context
	cancel    context.CancelFunc

	mu      sync.Mutex
	credit  int64
	changed chan struct{}
}

// ServeConn serves one agent connection until it closes or ctx ends.
func (s *Server) ServeConn(ctx context.Context, rwc io.ReadWriteCloser) error {
	s.StartJanitor(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := &conn{s: s, rwc: rwc, ctx: ctx, fw: datapath.NewFrameWriter(rwc), streams: map[uint64]*stream{}}
	go func() {
		<-ctx.Done()
		_ = rwc.Close()
	}()
	fr := datapath.NewFrameReader(rwc)
	for {
		f := &ownsandv1.GuestFrame{}
		if err := fr.Read(f); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			return fmt.Errorf("read guest frame: %w", err)
		}
		switch fr := f.GetFrame().(type) {
		case *ownsandv1.GuestFrame_Control:
			c.handleControl(fr.Control)
		case *ownsandv1.GuestFrame_Data:
			c.handleData(fr.Data)
		default:
			s.log.Warn("ignoring guest frame without payload")
		}
	}
}

func (c *conn) send(f *ownsandv1.GuestFrame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.fw.Write(f)
}

func (c *conn) handleControl(m *ownsandv1.GuestControl) {
	switch msg := m.GetMessage().(type) {
	case *ownsandv1.GuestControl_ReadyRequest:
		_ = c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Control{Control: &ownsandv1.GuestControl{
			Message: &ownsandv1.GuestControl_ReadyResponse{ReadyResponse: &ownsandv1.GuestReadyResponse{
				GuestVersion: Version,
				GuestOs:      currentGuestOS(),
			}},
		}}})
	case *ownsandv1.GuestControl_SetClock:
		t := msg.SetClock.GetNow()
		if t == nil || !t.IsValid() {
			c.s.log.Warn("ignoring set clock without a valid time")
			return
		}
		if err := c.s.cfg.SetClock(t.AsTime()); err != nil {
			c.s.log.Error("set guest clock failed", "error", err)
			return
		}
		c.s.log.Info("guest clock set", "time", t.AsTime().UTC().Format(time.RFC3339Nano))
	default:
		c.s.log.Warn("ignoring unknown guest control message")
	}
}

func currentGuestOS() ownsandv1.GuestOs {
	switch runtime.GOOS {
	case "linux":
		return ownsandv1.GuestOs_GUEST_OS_LINUX
	case "darwin":
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	case "windows":
		return ownsandv1.GuestOs_GUEST_OS_WINDOWS
	default:
		return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED
	}
}

func (c *conn) handleData(f *ownsandv1.DataFrame) {
	switch p := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_RelayWindowUpdate:
		if st := c.lookup(f.GetStreamId()); st != nil {
			st.addCredit(p.RelayWindowUpdate.GetAddedBytes())
		}
		return
	case *ownsandv1.DataFrame_RelayStreamClose:
		c.closeStream(f.GetStreamId())
		return
	case *ownsandv1.DataFrame_ExecRequest, *ownsandv1.DataFrame_ExecAttachRequest,
		*ownsandv1.DataFrame_FileReadRequest, *ownsandv1.DataFrame_FileWriteChunk:
	default:
		c.s.log.Warn("ignoring unsupported data frame", "kind", f.GetKind().String(), "stream_id", f.GetStreamId())
		return
	}
	st, ok := c.openStream(f.GetStreamId(), f.GetSandboxId())
	if !ok {
		c.sendErrorFrame(f.GetStreamId(), f.GetSandboxId(), connect.CodeInvalidArgument, "stream_id is already open")
		return
	}
	go func() {
		defer c.closeStream(st.id)
		var err error
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_ExecRequest:
			err = c.s.serveExec(c, st, p.ExecRequest)
		case *ownsandv1.DataFrame_ExecAttachRequest:
			err = c.s.serveAttach(c, st, p.ExecAttachRequest)
		case *ownsandv1.DataFrame_FileReadRequest:
			err = c.s.serveRead(c, st, p.FileReadRequest)
		case *ownsandv1.DataFrame_FileWriteChunk:
			err = c.s.serveWrite(c, st, p.FileWriteChunk)
		}
		if err != nil {
			var ce *connect.Error
			if errors.As(err, &ce) {
				c.sendErrorFrame(st.id, st.sandboxID, ce.Code(), ce.Message())
				return
			}
			if st.ctx.Err() == nil {
				c.s.log.Warn("guest stream failed", "stream_id", st.id, "error", err)
			}
		}
	}()
}

func (c *conn) lookup(id uint64) *stream {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.streams[id]
}

func (c *conn) openStream(id uint64, sandboxID string) (*stream, bool) {
	c.smu.Lock()
	defer c.smu.Unlock()
	if _, exists := c.streams[id]; exists {
		return nil, false
	}
	ctx, cancel := context.WithCancel(c.ctx)
	st := &stream{id: id, sandboxID: sandboxID, ctx: ctx, cancel: cancel, credit: InitialWindow, changed: make(chan struct{})}
	c.streams[id] = st
	return st, true
}

func (c *conn) closeStream(id uint64) {
	c.smu.Lock()
	st := c.streams[id]
	delete(c.streams, id)
	c.smu.Unlock()
	if st != nil {
		st.cancel()
	}
}

func (st *stream) addCredit(n int64) {
	if n <= 0 {
		return
	}
	st.mu.Lock()
	st.credit += n
	close(st.changed)
	st.changed = make(chan struct{})
	st.mu.Unlock()
}

// take waits until the window holds n bytes and consumes them.
func (st *stream) take(n int64) error {
	for {
		st.mu.Lock()
		if st.credit >= n {
			st.credit -= n
			st.mu.Unlock()
			return nil
		}
		ch := st.changed
		st.mu.Unlock()
		select {
		case <-ch:
		case <-st.ctx.Done():
			return st.ctx.Err()
		}
	}
}

// sendPayload sends a payload frame on st within its credit window.
func (c *conn) sendPayload(st *stream, set func(f *ownsandv1.DataFrame)) error {
	f := datapath.NewFrame(st.id, st.sandboxID, set)
	if err := st.take(datapath.FrameSize(f)); err != nil {
		return err
	}
	return c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Data{Data: f}})
}

func (c *conn) sendErrorFrame(id uint64, sandboxID string, code connect.Code, msg string) {
	f := datapath.NewFrame(id, sandboxID, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_Error{Error: &ownsandv1.FrameError{Code: code.String(), Message: msg}}
	})
	_ = c.send(&ownsandv1.GuestFrame{Frame: &ownsandv1.GuestFrame_Data{Data: f}})
}

// tombstoned reports whether id names an ended or expired operation.
// The caller holds s.mu.
func (s *Server) tombstonedLocked(id string) bool {
	_, ok := s.tombstones[id]
	return ok
}

// errNotFound is the error for an operation that ended or expired.
func errNotFound(what string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s is unknown, expired or no longer kept", what))
}
