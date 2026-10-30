package holder

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

// WireConfig is the agent-wire session of a wire holder.
type WireConfig struct {
	Harness    string            `json:"harness"`
	Binary     string            `json:"binary,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Home       string            `json:"home,omitempty"`
	// Resume continues the vendor session VendorID.
	Resume       bool                       `json:"resume,omitempty"`
	VendorID     string                     `json:"vendor_id,omitempty"`
	Model        string                     `json:"model,omitempty"`
	Effort       string                     `json:"effort,omitempty"`
	Permissions  agentwire.PermissionPolicy `json:"permissions"`
	MCPServers   []agentwire.MCPServer      `json:"mcp_servers,omitempty"`
	Skills       []agentwire.Skill          `json:"skills,omitempty"`
	Instructions string                     `json:"instructions,omitempty"`
	ExtraArgs    []string                   `json:"extra_args,omitempty"`
	Raw          map[string]any             `json:"raw,omitempty"`
	// FrameLog gets each frame in and out, mode 0600.
	FrameLog string `json:"frame_log,omitempty"`
	// Ledger is the agent-wire process ledger of the session.
	Ledger        string `json:"ledger,omitempty"`
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	// Drivers replace agent-wire drivers when Run runs in the caller process.
	Drivers map[string]agentwire.Driver `json:"-"`
}

// Spool is the content of Config.Spool: the events of run that no client
// stored.
type Spool struct {
	Run    string      `json:"run"`
	Events []WireEvent `json:"events"`
}

// wireServer is one running wire holder. It keeps each event until a client
// acks it, so a new client gets the events that the last one did not store.
type wireServer struct {
	cfg   Config
	log   *slog.Logger
	ln    net.Listener
	rt    *agentwire.Runtime
	sess  agentwire.Session
	run   string
	start time.Time
	wg    sync.WaitGroup
	// acked signals a new ack.
	acked chan struct{}

	mu      sync.Mutex
	peer    *peer
	seq     uint64
	outbox  []WireEvent
	vendor  string
	status  agentwire.SessionStatus
	pending []agentwire.Permission
	// sending counts the sends in progress; held means that a held prompt
	// starts the next turn.
	sending int
	held    bool
	turned  bool
	// ending refuses new calls; idle means that the end is the close at
	// idle.
	ending   bool
	idle     bool
	closing  bool
	exitCode *int
	exitErr  string
	exit     *Exit
}

const (
	wirePeerQueue = 8192
	callTimeout   = time.Minute
	closeTimeout  = 30 * time.Second
	ackWait       = 5 * time.Second
	peerGrace     = time.Second
)

var errEnding = errors.New("holder: the session ends at idle")

// ReadSpool reads the spool of a wire holder.
func ReadSpool(path string) (Spool, error) {
	var sp Spool
	data, err := os.ReadFile(path)
	if err != nil {
		return sp, err
	}
	if err := json.Unmarshal(data, &sp); err != nil {
		return sp, fmt.Errorf("read holder spool: %w", err)
	}
	return sp, nil
}

// runWire starts the agent-wire session and serves the socket until the
// session ends.
func runWire(ctx context.Context, c Config) error {
	s := &wireServer{
		cfg: c, log: cmp.Or(c.Logger, slog.Default()).With("source", "holder", "session_id", c.ID, "mode", ModeWire),
		run: newRun(), start: time.Now().UTC(), acked: make(chan struct{}, 1),
	}
	w := c.Wire
	s.rt = agentwire.New(agentwire.Options{
		ClientName: w.ClientName, ClientVersion: w.ClientVersion, LedgerPath: w.Ledger, Logger: s.log,
	})
	for h, d := range w.Drivers {
		s.rt.SetDriver(agentwire.Harness(h), d)
	}
	if err := s.rt.Reconcile(); err != nil {
		s.log.Warn("reconcile the agent-wire ledger", "err", err)
	}
	defer func() { _ = s.rt.Close(context.WithoutCancel(ctx)) }() // the session is closed already
	sess, err := s.open(ctx)
	if err != nil {
		return err
	}
	s.sess, s.vendor = sess, cmp.Or(sess.ID(), w.VendorID)
	if s.ln, err = listen(ctx, c.Socket); err != nil {
		_ = sess.Close(context.WithoutCancel(ctx)) // the socket error is the one to report
		return err
	}
	defer s.removeSocket()
	s.wg.Add(1)
	go s.acceptLoop()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.closeSession(false)
		case <-stop:
		}
	}()
	s.pump()
	close(stop)
	s.finish()
	return nil
}

func (s *wireServer) open(ctx context.Context) (agentwire.Session, error) {
	w := s.cfg.Wire
	req := agentwire.StartRequest{
		Harness: agentwire.Harness(w.Harness), Binary: w.Binary, WorkingDir: w.WorkingDir, Env: w.Env, Home: w.Home,
		Model: w.Model, Effort: w.Effort, Permissions: w.Permissions, MCPServers: w.MCPServers, Skills: w.Skills,
		Instructions: w.Instructions, ExtraArgs: w.ExtraArgs, Raw: w.Raw, LogPath: w.FrameLog,
	}
	if w.Resume {
		req.SessionID = w.VendorID
		return s.rt.Resume(ctx, req)
	}
	return s.rt.Start(ctx, req)
}

// pump reads the session events until the session ends.
func (s *wireServer) pump() {
	for e := range s.sess.Events() {
		e.Raw = nil // the frame log has the vendor frames
		if s.record(e) {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.closeSession(true)
			}()
		}
	}
}

// record keeps one event for the clients and tracks the session. It
// reports when the session became idle and the holder must end.
func (s *wireServer) record(e agentwire.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Type {
	case agentwire.EventInit:
		s.vendor = cmp.Or(e.SessionID, s.vendor)
	case agentwire.EventPermission:
		if p := e.Permission; p != nil && !slices.ContainsFunc(s.pending, func(q agentwire.Permission) bool { return q.ID == p.ID }) {
			s.pending = append(s.pending, *p)
		}
		s.status = agentwire.StatusWaiting
	case agentwire.EventResult:
		s.pending, s.turned, s.status = nil, true, agentwire.StatusIdle
		if r := e.Result; r != nil {
			s.vendor = cmp.Or(r.SessionID, s.vendor)
		}
		if s.held {
			s.held, s.status = false, agentwire.StatusRunning
		}
	case agentwire.EventExit:
		if s.idle {
			return false // the close at idle ends the process; the session does not exit
		}
		s.exitCode, s.exitErr = e.ExitCode, e.Error
	case agentwire.EventStatus:
		s.status = cmp.Or(e.Status, s.status)
	case agentwire.EventAssistant, agentwire.EventThought, agentwire.EventTool, agentwire.EventUser:
		if len(s.pending) == 0 {
			s.status = agentwire.StatusRunning
		}
	}
	s.seq++
	we := WireEvent{Seq: s.seq, Event: e}
	s.outbox = append(s.outbox, we)
	if s.peer != nil {
		s.sendLocked(s.peer, MsgEvent, we)
	}
	return s.idleLocked()
}

// idleLocked marks the end at idle: a turn ended, no send or held prompt
// waits, and the vendor session id is known, so the next holder can resume.
// The caller holds s.mu.
func (s *wireServer) idleLocked() bool {
	if !s.turned || s.ending || s.sending > 0 || s.held || s.status != agentwire.StatusIdle || s.vendor == "" {
		return false
	}
	s.ending, s.idle = true, true
	return true
}

// closeSession closes the session once. Its events end, and so does pump.
func (s *wireServer) closeSession(idle bool) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing, s.ending = true, true
	s.idle = s.idle || idle
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := s.sess.Close(ctx); err != nil {
		s.log.Debug("close the session", "err", err)
	}
}

func (s *wireServer) acceptLoop() {
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

// serve exchanges the hellos, sends the events after the ack of the client,
// and reads its acks and calls.
func (s *wireServer) serve(conn net.Conn) {
	defer s.wg.Done()
	p, err := s.join(conn)
	if err != nil {
		s.log.Debug("refuse a peer", "err", err)
		_ = conn.Close() // the peer is gone or speaks another protocol
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		p.write(s.drop)
	}()
	for {
		t, data, err := readMsg(conn)
		if err != nil {
			s.drop(p)
			return
		}
		switch t {
		case MsgAck:
			var seq uint64
			if json.Unmarshal(data, &seq) == nil {
				s.ack(seq)
			}
		case MsgCall:
			var c Call
			if json.Unmarshal(data, &c) == nil {
				s.wg.Add(1)
				go s.call(p, c)
			}
		}
	}
}

func (s *wireServer) join(conn net.Conn) (*peer, error) {
	if err := conn.SetDeadline(time.Now().Add(helloTimeout)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	h := Hello{
		Protocol: Protocol, Mode: ModeWire, ID: s.cfg.ID, PID: os.Getpid(), ChildPID: s.sess.PID(), StartedAt: s.start,
		Run: s.run, VendorID: s.vendor, Status: s.status, Pending: slices.Clone(s.pending),
	}
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
	if t != MsgHello || json.Unmarshal(data, &their) != nil || their.Protocol != Protocol {
		return nil, ErrProtocol
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != nil {
		s.dropLocked(s.peer)
	}
	p := &peer{conn: conn, out: make(chan []byte, wirePeerQueue+len(s.outbox))}
	s.peer = p
	for _, we := range s.outbox {
		if we.Seq > their.After {
			s.sendLocked(p, MsgEvent, we)
		}
	}
	if s.exit != nil {
		s.sendLocked(p, MsgExit, s.exit)
	}
	return p, nil
}

// call runs one call and sends the reply to the peer that asked.
func (s *wireServer) call(p *peer, c Call) {
	defer s.wg.Done()
	r := Reply{ID: c.ID}
	if err := s.do(c, &r); err != nil {
		r.Error = err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer == p {
		s.sendLocked(p, MsgReply, r)
	}
}

func (s *wireServer) do(c Call, r *Reply) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	s.mu.Lock()
	if s.ending && c.Op != OpClose {
		s.mu.Unlock()
		r.Code = CodeEnding
		return errEnding
	}
	if c.Op == OpSend {
		s.sending++
	}
	s.mu.Unlock()
	switch c.Op {
	case OpSend:
		return s.send(ctx, c, r)
	case OpInterrupt:
		return s.sess.Interrupt(ctx)
	case OpAnswer:
		if c.Decision == nil {
			return errors.New("holder: an answer needs a decision")
		}
		if err := s.sess.AnswerPermission(ctx, c.RequestID, *c.Decision); err != nil {
			return err
		}
		s.mu.Lock()
		s.pending = slices.DeleteFunc(s.pending, func(p agentwire.Permission) bool { return p.ID == c.RequestID })
		if len(s.pending) == 0 && s.status == agentwire.StatusWaiting {
			s.status = agentwire.StatusRunning
		}
		s.mu.Unlock()
		return nil
	case OpClose:
		s.closeSession(false)
		return nil
	}
	return fmt.Errorf("holder: unknown op %q", c.Op)
}

// send delivers a prompt. A turn that ended while the send ran ends the
// holder after the send.
func (s *wireServer) send(ctx context.Context, c Call, r *Reply) error {
	var (
		d   agentwire.Delivery
		err = errors.New("holder: a send needs a prompt")
	)
	if c.Prompt != nil {
		d, err = agentwire.Send(ctx, s.sess, *c.Prompt)
	}
	s.mu.Lock()
	s.sending--
	switch {
	case err != nil:
	case d == agentwire.DeliveryHeld:
		s.held = true
	case d == agentwire.DeliveryPrompted:
		s.status = agentwire.StatusRunning
	}
	idle := s.idleLocked()
	s.mu.Unlock()
	if idle {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.closeSession(true)
		}()
	}
	r.Delivery = d
	return err
}

// ack drops the events that the client stored.
func (s *wireServer) ack(seq uint64) {
	s.mu.Lock()
	i := 0
	for i < len(s.outbox) && s.outbox[i].Seq <= seq {
		i++
	}
	s.outbox = s.outbox[i:]
	s.mu.Unlock()
	select {
	case s.acked <- struct{}{}:
	default:
	}
}

// finish sends the exit, waits a short time for the acks, and keeps the
// rest in the spool. The exit file comes last, so a client that finds no
// socket finds the spool.
func (s *wireServer) finish() {
	s.mu.Lock()
	exit := Exit{Idle: s.idle, Last: s.seq, Error: s.exitErr}
	if s.exitCode != nil {
		exit.Code = *s.exitCode
	}
	s.exit = &exit
	if s.peer != nil {
		s.sendLocked(s.peer, MsgExit, exit)
	}
	s.mu.Unlock()
	s.waitAcks()
	_ = s.ln.Close() // the accept loop ends on this close
	s.mu.Lock()
	rest := slices.Clone(s.outbox)
	s.mu.Unlock()
	if len(rest) > 0 && s.cfg.Spool != "" {
		if err := writeJSON(s.cfg.Spool, Spool{Run: s.run, Events: rest}); err != nil {
			s.log.Warn("write the spool", "err", err)
		}
	}
	if s.cfg.ExitFile != "" {
		if err := writeJSON(s.cfg.ExitFile, exit); err != nil {
			s.log.Warn("write the exit file", "err", err)
		}
	}
	s.removeSocket()
	s.mu.Lock()
	if s.peer != nil {
		s.dropLocked(s.peer)
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
	s.log.Info("session ended", "idle", exit.Idle, "code", exit.Code)
}

// waitAcks waits until a client stored each event. With no client, it
// waits peerGrace for a new one.
func (s *wireServer) waitAcks() {
	start := time.Now()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		s.mu.Lock()
		empty, gone := len(s.outbox) == 0, s.peer == nil
		s.mu.Unlock()
		wait := time.Since(start)
		if empty || wait > ackWait || (gone && wait > peerGrace) {
			return
		}
		select {
		case <-s.acked:
		case <-t.C:
		}
	}
}

// sendLocked queues a message for p. A peer that cannot keep up is dropped;
// it dials again and gets the events after its last one. The caller holds
// s.mu.
func (s *wireServer) sendLocked(p *peer, t MsgType, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.log.Warn("encode a message", "type", t, "err", err)
		return
	}
	select {
	case p.out <- encodeMsg(t, data):
	default:
		s.dropLocked(p)
	}
}

func (s *wireServer) drop(p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked(p)
}

// dropLocked removes the peer. Its write loop sends what is queued, then
// closes the connection.
func (s *wireServer) dropLocked(p *peer) {
	if s.peer != p {
		return
	}
	s.peer = nil
	close(p.out)
	_ = p.conn.SetReadDeadline(time.Now()) // stops the read loop of the peer
}

func (s *wireServer) removeSocket() {
	if err := os.Remove(s.cfg.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("remove the socket", "err", err)
	}
}

// writeJSON writes v to path with mode 0600.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func newRun() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}
