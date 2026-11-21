// SPDX-License-Identifier: FSL-1.1-ALv2

// Package guest implements the GuestService of the guest helper: the
// readiness check, the clock setting and the DataFrame exec and file
// requests of the device agent. The owning contracts are
// docs/cmds-ownsand-agent-foundation.md (guest helper) and
// docs/protos-ownsand-api-contract.md (exec, file transfer, guest channel).
//
// Exec commands, environment values, exec output, guest paths and file
// contents are sandbox content: the helper never logs them.
package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-guest/execlog"
	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// ClockMode says whether SetClock changes the system clock.
type ClockMode int

const (
	ClockModeUnspecified ClockMode = iota
	// ClockModeSet sets the guest system clock (inside a sandbox guest).
	ClockModeSet
	// ClockModeIgnore accepts SetClock without changing any clock. The test
	// runtime of the device agent uses it, because its guest helper runs as
	// a host process and must never change the host clock.
	ClockModeIgnore
)

// DefaultKeepaliveInterval is how often a stream without other frames gets
// a keepalive. The contract requires at least one every 60 seconds.
const DefaultKeepaliveInterval = 30 * time.Second

// Config configures a Server.
type Config struct {
	// StateDir holds the read snapshots. It must be on the guest disk.
	StateDir string
	// Version is the helper version that CheckReady reports.
	Version string
	Clock   ClockMode
	Logger  *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// KeepaliveInterval defaults to DefaultKeepaliveInterval.
	KeepaliveInterval time.Duration
	// SweepInterval is how often expired operations are removed; it
	// defaults to 30 seconds.
	SweepInterval time.Duration
}

// Server serves the GuestService.
type Server struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
	execs  *execManager
	reads  *readManager
	writes *writeManager
}

var _ ownsandv1connect.GuestServiceHandler = (*Server)(nil)

// New returns a server. It clears read snapshots of an earlier process.
func New(cfg Config) (*Server, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("guest: state directory is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = DefaultKeepaliveInterval
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	if cfg.Clock == ClockModeUnspecified {
		cfg.Clock = ClockModeSet
	}
	reads, err := newReadManager(filepath.Join(cfg.StateDir, "reads"), cfg.Now, cfg.Logger)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:    cfg,
		logger: cfg.Logger,
		now:    cfg.Now,
		execs:  newExecManager(cfg.Now, cfg.Logger),
		reads:  reads,
		writes: newWriteManager(cfg.Now, cfg.Logger),
	}, nil
}

// GuestOS returns the GuestOs of the running operating system.
func GuestOS() ownsandv1.GuestOs {
	switch runtime.GOOS {
	case "linux":
		return ownsandv1.GuestOs_GUEST_OS_LINUX
	case "darwin":
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	case "windows":
		return ownsandv1.GuestOs_GUEST_OS_WINDOWS
	}
	return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED
}

// CheckReady reports the guest OS and the helper version.
func (s *Server) CheckReady(context.Context, *connect.Request[ownsandv1.CheckReadyRequest]) (*connect.Response[ownsandv1.CheckReadyResponse], error) {
	return connect.NewResponse(&ownsandv1.CheckReadyResponse{GuestOs: GuestOS(), HelperVersion: s.cfg.Version}), nil
}

// SetClock sets the guest clock from the host clock.
func (s *Server) SetClock(_ context.Context, req *connect.Request[ownsandv1.SetClockRequest]) (*connect.Response[ownsandv1.SetClockResponse], error) {
	if req.Msg.GetNow() == nil || req.Msg.GetNow().CheckValid() != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("now is required"))
	}
	t := req.Msg.GetNow().AsTime()
	if s.cfg.Clock == ClockModeIgnore {
		s.logger.Info("set clock ignored", "clock_mode", "ignore")
		return connect.NewResponse(&ownsandv1.SetClockResponse{}), nil
	}
	if err := setSystemClock(t); err != nil {
		s.logger.Warn("set clock failed", "error", err)
		if errors.Is(err, errors.ErrUnsupported) {
			return nil, connect.NewError(connect.CodeUnimplemented, err)
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("set clock failed"))
	}
	s.logger.Info("clock set", "offset_ms", time.Until(t).Milliseconds())
	return connect.NewResponse(&ownsandv1.SetClockResponse{}), nil
}

// frameSender serializes sends on one Serve stream.
type frameSender struct {
	mu     sync.Mutex
	stream *connect.BidiStream[ownsandv1.ServeRequest, ownsandv1.ServeResponse]
}

func (fs *frameSender) send(f *ownsandv1.DataFrame) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.stream.Send(&ownsandv1.ServeResponse{Frame: f})
}

// Serve handles the DataFrame requests of one agent stream. Each request
// runs on its own goroutine; execs keep running after the stream ends.
func (s *Server) Serve(ctx context.Context, stream *connect.BidiStream[ownsandv1.ServeRequest, ownsandv1.ServeResponse]) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &frameSender{stream: stream}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		req, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		f := req.GetFrame()
		if f == nil {
			continue
		}
		if err := datapath.ValidateFrame(f); err != nil {
			var unknown *datapath.UnknownKindError
			if f.GetStreamId() != 0 {
				code := ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
				if errors.As(err, &unknown) {
					code = ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED
				}
				s.sendError(out, f, &opError{code: code, msg: "malformed or unknown frame"})
			}
			continue
		}
		wg.Add(1)
		go func(f *ownsandv1.DataFrame) {
			defer wg.Done()
			s.handle(ctx, out, f)
		}(f)
	}
}

func (s *Server) sendError(out *frameSender, req *ownsandv1.DataFrame, err error) {
	_ = out.send(datapath.MustNewFrame(req.GetStreamId(), req.GetSandboxId(), detailOf(err)))
}

func (s *Server) handle(ctx context.Context, out *frameSender, f *ownsandv1.DataFrame) {
	switch p := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest:
		req := p.ExecRequest
		if req.GetExecId() == "" || len(req.GetCommand()) == 0 || req.GetCommand()[0] == "" {
			s.sendError(out, f, errorf(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "exec request needs an exec_id and a program"))
			return
		}
		l, _ := s.execs.start(req)
		s.streamExec(ctx, out, f, l.Cursor(0, 0))
	case *ownsandv1.DataFrame_ExecAttachRequest:
		req := p.ExecAttachRequest
		l, ok := s.execs.lookup(req.GetExecId())
		if !ok {
			s.sendError(out, f, errorf(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "exec is unknown or its result is no longer kept"))
			return
		}
		s.streamExec(ctx, out, f, l.Cursor(req.GetStdoutOffset(), req.GetStderrOffset()))
	case *ownsandv1.DataFrame_FileReadRequest:
		s.serveRead(ctx, out, f, p.FileReadRequest)
	case *ownsandv1.DataFrame_FileWriteChunk:
		resp, err := s.writes.apply(p.FileWriteChunk)
		if err != nil {
			s.sendError(out, f, err)
			return
		}
		_ = out.send(datapath.MustNewFrame(f.GetStreamId(), f.GetSandboxId(), resp))
	default:
		s.sendError(out, f, errorf(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "frame kind %s is not a guest request", f.GetKind()))
	}
}

func (s *Server) streamExec(ctx context.Context, out *frameSender, req *ownsandv1.DataFrame, c *execlog.Cursor) {
	for {
		ev, err := c.Next(ctx, s.cfg.KeepaliveInterval)
		if err != nil {
			return
		}
		if err := out.send(datapath.MustNewFrame(req.GetStreamId(), req.GetSandboxId(), ev)); err != nil {
			return
		}
	}
}

func readEvent(ev any) *ownsandv1.FileReadEvent {
	switch e := ev.(type) {
	case *ownsandv1.FileChunk:
		return &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: e}}
	case *ownsandv1.FileReadEnd:
		return &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_End{End: e}}
	}
	return &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}}
}

func (s *Server) serveRead(ctx context.Context, out *frameSender, f *ownsandv1.DataFrame, req *ownsandv1.ReadFileRequest) {
	if req.GetOffset() < 0 {
		s.sendError(out, f, errorf(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "offset must not be negative"))
		return
	}
	e, err := s.reads.begin(req.GetReadId(), req.GetPath())
	if err != nil {
		s.sendError(out, f, err)
		return
	}
	defer s.reads.end(req.GetReadId(), e)
	send := func(ev any) error {
		return out.send(datapath.MustNewFrame(f.GetStreamId(), f.GetSandboxId(), readEvent(ev)))
	}
	ticker := time.NewTicker(s.cfg.KeepaliveInterval)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-e.ready:
			break wait
		case <-ctx.Done():
			return
		case <-ticker.C:
			if send(nil) != nil {
				return
			}
		}
	}
	if e.err != nil {
		s.sendError(out, f, e.err)
		return
	}
	if req.GetOffset() > e.size {
		s.sendError(out, f, errorf(ownsandv1.ErrorCode_ERROR_CODE_OUT_OF_RANGE, "offset is beyond the end of the file"))
		return
	}
	// The snapshot is opened and read on a worker goroutine, so a stalled
	// disk never blocks the keepalive ticker or the cancellation of this
	// handler. The worker only reads; every frame is sent from this
	// goroutine, so nothing is sent after the end or an error frame.
	chunks := make(chan readChunk)
	stop := make(chan struct{})
	defer close(stop)
	go s.readChunks(e, req.GetOffset(), chunks, stop)
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				_ = send(&ownsandv1.FileReadEnd{SizeBytes: e.size})
				return
			}
			if c.err != nil {
				s.sendError(out, f, c.err)
				return
			}
			if send(c.chunk) != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-ticker.C:
			if send(nil) != nil {
				return
			}
		}
	}
}

// readChunk is one result of the chunk worker: a chunk or the error that
// ends the read.
type readChunk struct {
	chunk *ownsandv1.FileChunk
	err   error
}

// readChunks opens the snapshot of e and publishes its chunks from offset to
// the end of the snapshot in order, then closes out. It publishes at most
// one error, as its last result. It stops when stop is closed and always
// closes the file it opened, also when it finishes a read that the handler
// no longer waits for.
func (s *Server) readChunks(e *readEntry, offset int64, out chan<- readChunk, stop <-chan struct{}) {
	defer close(out)
	publish := func(c readChunk) bool {
		select {
		case out <- c:
			return true
		case <-stop:
			return false
		}
	}
	file, err := s.reads.open(e.snapshot)
	if err != nil {
		publish(readChunk{err: fsError("read snapshot", err)})
		return
	}
	defer file.Close()
	buf := make([]byte, datapath.MaxFileChunkSize)
	for off := offset; off < e.size; {
		select {
		case <-stop:
			return
		default:
		}
		n, err := file.ReadAt(buf[:min(int64(len(buf)), e.size-off)], off)
		if n == 0 && err != nil {
			publish(readChunk{err: fsError("read snapshot", err)})
			return
		}
		if !publish(readChunk{chunk: &ownsandv1.FileChunk{Offset: off, Data: append([]byte(nil), buf[:n]...)}}) {
			return
		}
		off += int64(n)
	}
}

// Handler returns the HTTP handler of the GuestService. Compression is off:
// file chunks and exec output gain little from gzip on a local socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	path, h := ownsandv1connect.NewGuestServiceHandler(s,
		connect.WithCompression("gzip", nil, nil),
		connect.WithReadMaxBytes(datapath.MaxFrameSize+(64<<10)),
	)
	mux.Handle(path, h)
	return mux
}

// Run serves the GuestService on l with HTTP/2 without TLS (prior
// knowledge) until ctx ends, and removes expired operations meanwhile.
func (s *Server) Run(ctx context.Context, l net.Listener) error {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:           s.Handler(),
		Protocols:         &protocols,
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(s.cfg.SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.Sweep()
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				srv.Close()
			}
		case <-done:
		}
	}()
	s.logger.Info("guest helper serving", "address", l.Addr().String(), "network", l.Addr().Network())
	err := srv.Serve(l)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("guest: serve: %w", err)
}

// Sweep removes expired execs, read snapshots and writes.
func (s *Server) Sweep() {
	now := s.now()
	s.execs.sweep(now)
	s.reads.sweep(now)
	s.writes.sweep(now)
}
