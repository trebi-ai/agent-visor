// Package holder owns the PTY or the agent-wire session of one session, so
// that the session lives through a restart of the visor. It imports nothing
// from this repository: an old holder keeps its protocol until its session
// ends.
package holder

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Config tells a holder what to run. The visor sends it as JSON on stdin,
// because Env can hold secrets.
type Config struct {
	ID   string `json:"id"`
	Mode Mode   `json:"mode,omitempty"`
	// Socket is the path of the holder socket, mode 0600.
	Socket  string   `json:"socket"`
	Command []string `json:"command"`
	Dir     string   `json:"dir,omitempty"`
	// Env is the full env of the process, as KEY=value.
	Env  []string `json:"env,omitempty"`
	Cols int      `json:"cols,omitempty"`
	Rows int      `json:"rows,omitempty"`
	// Log gets all output, mode 0600. Empty writes no log.
	Log string `json:"log,omitempty"`
	// ExitFile gets the JSON Exit when the process ends, mode 0600.
	ExitFile string `json:"exit_file,omitempty"`
	// Ring is the size of the screen buffer in bytes. Zero is 256 KiB.
	Ring int `json:"ring,omitempty"`
	// Wire is the session of a wire holder.
	Wire *WireConfig `json:"wire,omitempty"`
	// Spool gets the events that no client stored when a wire holder ends,
	// mode 0600.
	Spool  string       `json:"spool,omitempty"`
	Logger *slog.Logger `json:"-"`
}

// server is one running holder.
type server struct {
	cfg   Config
	log   *slog.Logger
	ln    net.Listener
	ptmx  *os.File
	cmd   *exec.Cmd
	start time.Time
	wg    sync.WaitGroup
	// exited closes when the process ends.
	exited chan struct{}
	wmu    sync.Mutex // writes to the PTY

	mu    sync.Mutex
	ring  []byte
	logf  *os.File
	size  Size
	peers map[*peer]struct{}
	exit  *Exit
}

// peer is one connected client.
type peer struct {
	conn net.Conn
	out  chan []byte
}

// Defaults and limits.
const (
	DefaultRing  = 256 << 10
	defaultCols  = 80
	defaultRows  = 24
	peerQueue    = 1024
	killGrace    = 3 * time.Second
	drainTimeout = time.Second
)

// MaxSocketPath is the macOS limit of a Unix socket path, less one.
const MaxSocketPath = 103

// ReadConfig reads the config of the holder of session id from r.
func ReadConfig(r io.Reader, id string) (Config, error) {
	var c Config
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return c, fmt.Errorf("read holder config: %w", err)
	}
	if c.ID != id {
		return c, fmt.Errorf("holder config is for session %q, not %q", c.ID, id)
	}
	return c, nil
}

// Run starts the process in a PTY, or the agent-wire session of a wire
// holder, and serves the socket until the process ends. When ctx ends, Run
// ends the process first.
func Run(ctx context.Context, c Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	if c.Mode == ModeWire {
		return runWire(ctx, c)
	}
	s := &server{
		cfg: c, log: cmp.Or(c.Logger, slog.Default()).With("source", "holder", "session_id", c.ID),
		exited: make(chan struct{}), peers: map[*peer]struct{}{},
		size: Size{Cols: cmp.Or(c.Cols, defaultCols), Rows: cmp.Or(c.Rows, defaultRows)},
	}
	ln, err := listen(ctx, c.Socket)
	if err != nil {
		return err
	}
	s.ln = ln
	defer s.removeSocket()
	if err := s.startProcess(); err != nil {
		_ = s.ln.Close() // the start error is the one to report
		return err
	}
	readDone := make(chan struct{})
	go s.readLoop(readDone)
	s.wg.Add(1)
	go s.acceptLoop()
	go func() {
		select {
		case <-ctx.Done():
			s.kill()
		case <-s.exited:
		}
	}()
	exit := waitExit(s.cmd.Wait())
	close(s.exited)
	s.drain(readDone)
	s.finish(exit)
	s.log.Info("process ended", "code", exit.Code)
	return nil
}

func (c Config) validate() error {
	switch {
	case c.ID == "":
		return errors.New("holder: no session id")
	case c.Mode != ModePTY && c.Mode != ModeWire:
		return fmt.Errorf("holder: unknown mode %q", c.Mode)
	case c.Mode == ModeWire && c.Wire == nil:
		return errors.New("holder: no wire session")
	case c.Mode == ModePTY && (len(c.Command) == 0 || c.Command[0] == ""):
		return errors.New("holder: no command")
	case c.Socket == "":
		return errors.New("holder: no socket path")
	case len(c.Socket) > MaxSocketPath:
		return fmt.Errorf("holder: the socket path has %d bytes; the limit is %d", len(c.Socket), MaxSocketPath)
	}
	return nil
}

func (s *server) startProcess() error {
	if s.cfg.Log != "" {
		if err := os.MkdirAll(filepath.Dir(s.cfg.Log), 0o700); err != nil {
			return fmt.Errorf("holder log: %w", err)
		}
		f, err := os.OpenFile(s.cfg.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("holder log: %w", err)
		}
		s.logf = f
	}
	// The holder ends the process itself, so the command has no context.
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...) //nolint:noctx // the process outlives each request
	cmd.Dir, cmd.Env = s.cfg.Dir, s.cfg.Env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(s.size.Cols), Rows: uint16(s.size.Rows)})
	if err != nil {
		if s.logf != nil {
			_ = s.logf.Close() // the start error is the one to report
		}
		return fmt.Errorf("start %s: %w", s.cfg.Command[0], err)
	}
	s.cmd, s.ptmx, s.start = cmd, ptmx, time.Now().UTC()
	return nil
}

func (s *server) readLoop(done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.output(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// output keeps new bytes in the ring and the log, and sends them to each
// peer.
func (s *server) output(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := cmp.Or(s.cfg.Ring, DefaultRing)
	s.ring = append(s.ring, p...)
	if over := len(s.ring) - limit; over > 0 {
		s.ring = append(s.ring[:0:0], s.ring[over:]...)
	}
	if s.logf != nil {
		if _, err := s.logf.Write(p); err != nil {
			s.log.Warn("write the output log", "err", err)
			_ = s.logf.Close() // stop the log after a write error
			s.logf = nil
		}
	}
	s.broadcast(encodeMsg(MsgOutput, p))
}

// broadcast queues a message for each peer. A peer that cannot keep up is
// dropped; it dials again and gets a new snapshot. The caller holds s.mu.
func (s *server) broadcast(msg []byte) {
	for p := range s.peers {
		select {
		case p.out <- msg:
		default:
			s.dropLocked(p)
		}
	}
}

func (s *server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serve(conn)
	}
}

// serve exchanges the hellos, sends the snapshot, and reads the messages of
// one peer.
func (s *server) serve(conn net.Conn) {
	defer s.wg.Done()
	p, err := s.join(conn)
	if err != nil {
		s.log.Debug("refuse a peer", "err", err)
		_ = conn.Close() // the peer is gone or speaks another protocol
		return
	}
	s.wg.Add(1)
	go s.writeLoop(p)
	for {
		t, data, err := readMsg(conn)
		if err != nil {
			s.drop(p)
			return
		}
		s.handle(t, data)
	}
}

func (s *server) join(conn net.Conn) (*peer, error) {
	if err := conn.SetDeadline(time.Now().Add(helloTimeout)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	h := Hello{Protocol: Protocol, Mode: ModePTY, ID: s.cfg.ID, PID: os.Getpid(), ChildPID: s.cmd.Process.Pid, Cols: s.size.Cols, Rows: s.size.Rows, StartedAt: s.start}
	s.mu.Unlock()
	data, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if err := writeMsg(conn, MsgHello, data); err != nil {
		return nil, err
	}
	t, data, err := readMsg(conn)
	if err != nil {
		return nil, err
	}
	var their Hello
	if t != MsgHello || json.Unmarshal(data, &their) != nil || their.Protocol < MinProtocol || their.Protocol > Protocol {
		return nil, ErrProtocol
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	p := &peer{conn: conn, out: make(chan []byte, peerQueue)}
	s.mu.Lock()
	defer s.mu.Unlock()
	p.out <- encodeMsg(MsgSnapshot, s.ring)
	if s.exit != nil {
		exit, _ := json.Marshal(s.exit) //nolint:errchkjson // a plain struct
		p.out <- encodeMsg(MsgExit, exit)
		close(p.out)
		return p, nil
	}
	s.peers[p] = struct{}{}
	return p, nil
}

func (s *server) writeLoop(p *peer) {
	defer s.wg.Done()
	p.write(s.drop)
}

// write sends the queued messages until the queue closes, then closes the
// connection. A failed write calls drop once.
func (p *peer) write(drop func(*peer)) {
	defer p.conn.Close()
	failed := false
	for msg := range p.out {
		if failed {
			continue
		}
		if err := p.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err == nil {
			_, err = p.conn.Write(msg)
			failed = err != nil
		}
		if failed {
			drop(p)
		}
	}
}

func (s *server) handle(t MsgType, data []byte) {
	switch t {
	case MsgInput:
		s.wmu.Lock()
		_, err := s.ptmx.Write(data)
		s.wmu.Unlock()
		if err != nil {
			s.log.Debug("write to the PTY", "err", err)
		}
	case MsgResize:
		var size Size
		if json.Unmarshal(data, &size) != nil || size.Cols <= 0 || size.Rows <= 0 {
			return
		}
		s.resize(size)
	case MsgKill:
		s.kill()
	}
}

func (s *server) resize(size Size) {
	ws := &pty.Winsize{Cols: uint16(size.Cols), Rows: uint16(size.Rows)}
	if err := pty.Setsize(s.ptmx, ws); err != nil {
		s.log.Debug("resize the PTY", "err", err)
		return
	}
	data, err := json.Marshal(size)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.size = size
	s.broadcast(encodeMsg(MsgResized, data))
}

// kill ends the process group: a hangup and a terminate first, then a kill
// after a grace time.
func (s *server) kill() {
	s.signal(syscall.SIGHUP)
	s.signal(syscall.SIGTERM)
	go func() {
		t := time.NewTimer(killGrace)
		defer t.Stop()
		select {
		case <-s.exited:
		case <-t.C:
			s.signal(syscall.SIGKILL)
		}
	}()
}

// drain reads the last output after the process ended.
func (s *server) drain(readDone <-chan struct{}) {
	t := time.NewTimer(drainTimeout)
	defer t.Stop()
	select {
	case <-readDone:
	case <-t.C:
	}
	_ = s.ptmx.Close() // the read loop ends on this close
	select {
	case <-readDone:
	case <-time.After(drainTimeout):
		s.log.Warn("the PTY read did not end")
	}
}

// finish writes the exit file, stops the socket, and sends the exit to each
// peer.
func (s *server) finish(exit Exit) {
	data, _ := json.Marshal(exit) //nolint:errchkjson // a plain struct
	if s.cfg.ExitFile != "" {
		if err := os.WriteFile(s.cfg.ExitFile, data, 0o600); err != nil {
			s.log.Warn("write the exit file", "err", err)
		}
	}
	_ = s.ln.Close() // the accept loop ends on this close
	s.removeSocket()
	s.mu.Lock()
	s.exit = &exit
	for p := range s.peers {
		select {
		case p.out <- encodeMsg(MsgExit, data):
		default:
		}
		s.dropLocked(p)
	}
	if s.logf != nil {
		_ = s.logf.Close() // the output is complete
		s.logf = nil
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(writeTimeout):
		s.log.Warn("a peer did not take the exit")
	}
}

func (s *server) drop(p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked(p)
}

// dropLocked removes a peer. Its write loop sends what is queued, then
// closes the connection.
func (s *server) dropLocked(p *peer) {
	if _, ok := s.peers[p]; !ok {
		return
	}
	delete(s.peers, p)
	close(p.out)
	_ = p.conn.SetReadDeadline(time.Now()) // stops the read loop of the peer
}

func (s *server) removeSocket() {
	if err := os.Remove(s.cfg.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("remove the socket", "err", err)
	}
}

// listen serves a socket with mode 0600 at path.
func listen(ctx context.Context, path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("holder socket: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("holder socket: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("holder socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close() // the chmod error is the one to report
		return nil, fmt.Errorf("holder socket: %w", err)
	}
	return ln, nil
}

// waitExit converts the result of Wait.
func waitExit(err error) Exit {
	if err == nil {
		return Exit{}
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return Exit{Code: -1}
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return Exit{Code: 128 + int(ws.Signal())}
	}
	return Exit{Code: ee.ExitCode()}
}
