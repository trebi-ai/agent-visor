package agentvisor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/holder"
)

// wireBackend hosts agent-wire sessions. Each session runs in a wire
// holder, so a turn lives through a visor restart. The holder ends when its
// session is idle, and the next send starts a new one. A harness with a
// driver in Options.Drivers runs in the visor process and does not survive.
type wireBackend struct {
	rt            *agentwire.Runtime
	inproc        map[string]bool
	home          string
	hs            *holders
	client        string
	clientVersion string
	// cursor gives the last stored holder event of a session.
	cursor func(ctx context.Context, session string) (holder.Cursor, error)
	log    *slog.Logger

	mu      sync.Mutex
	handles map[*heldWire]struct{}
	wg      sync.WaitGroup
}

// wireHandle is a wire session in the visor process.
type wireHandle struct {
	id   string
	s    agentwire.Session
	ch   chan Event
	once sync.Once
}

// heldWire is the connection of the visor to one wire holder. The visor
// acks each event after it stores it; the holder keeps the events that the
// visor did not ack and sends them again after a reconnect.
type heldWire struct {
	b      *wireBackend
	id     string
	events chan Event
	// stop closes on detach; done closes when the pump ends.
	stop chan struct{}
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	c        *holder.Client
	vendor   string
	pid      int
	last     holder.Cursor
	calls    map[uint64]chan holder.Reply
	nextCall uint64
	exit     *holder.Exit
	detached bool
}

var (
	_ Backend = (*wireBackend)(nil)
	_ Handle  = (*wireHandle)(nil)
	_ Handle  = (*heldWire)(nil)
)

// errHolderEnding means that the wire holder ends at idle. The visor waits
// for the end and resumes the session in a new holder.
var errHolderEnding = errors.New("the wire holder ends at idle")

const (
	wireDialTimeout = time.Minute
	wireCallTimeout = time.Minute
)

func newWireBackend(rt *agentwire.Runtime, o Options, cursor func(context.Context, string) (holder.Cursor, error), log *slog.Logger) *wireBackend {
	b := &wireBackend{
		rt: rt, inproc: map[string]bool{}, home: o.Home, hs: newHolders(o.Home, "wire", o.HolderCommand),
		client: o.Client, clientVersion: o.ClientVersion, cursor: cursor, log: log, handles: map[*heldWire]struct{}{},
	}
	for h := range o.Drivers {
		b.inproc[h] = true
	}
	return b
}

func (b *wireBackend) Kind() Kind { return KindWire }

func (b *wireBackend) Caps(harness string) Caps {
	return Caps{Structured: true, Permissions: true, Steer: true, Resume: true, Survives: b.held(harness)}
}

func (b *wireBackend) Start(ctx context.Context, l Launch) (Handle, error) {
	var (
		h   Handle
		err error
	)
	if b.held(l.Spec.Harness) {
		h, err = b.startHeld(ctx, l)
	} else {
		h, err = b.startInProc(ctx, l)
	}
	if err != nil {
		return nil, err
	}
	if l.Prompt != nil {
		if _, err := h.Send(ctx, *l.Prompt); err != nil {
			_ = h.Close(context.WithoutCancel(ctx)) // the send error is the one to report
			return nil, err
		}
	}
	return h, nil
}

// Recover connects to each live wire holder. A holder that ended while no
// visor ran left an exit file and maybe a spool; its handle sends the
// spooled events and ends.
func (b *wireBackend) Recover(ctx context.Context) ([]Handle, error) {
	socks, exits, err := b.hs.scan()
	if err != nil {
		return nil, err
	}
	var out []Handle
	for _, id := range socks {
		cur, err := b.cursor(ctx, id)
		if err != nil {
			return out, err
		}
		c, err := b.reach(ctx, id, cur)
		switch {
		case err != nil:
			b.log.Warn("recover a wire holder", "session_id", id, "err", err)
			if !errors.Is(err, holder.ErrProtocol) {
				_ = os.Remove(b.hs.socket(id)) // a stale socket of a dead holder
			}
		default:
			out = append(out, b.track(id, "", c, cur))
		}
	}
	for _, id := range exits {
		cur, err := b.cursor(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, b.track(id, "", nil, cur))
	}
	return out, nil
}

// Close detaches from each holder, and closes the sessions in the visor
// process.
func (b *wireBackend) Close(ctx context.Context) error {
	b.mu.Lock()
	hs := make([]*heldWire, 0, len(b.handles))
	for h := range b.handles {
		hs = append(hs, h)
	}
	b.mu.Unlock()
	for _, h := range hs {
		h.detach()
	}
	b.wg.Wait()
	return b.rt.Close(ctx)
}

// held tells if the sessions of a harness run in a holder.
func (b *wireBackend) held(harness string) bool {
	return len(b.hs.cmd) > 0 && !b.inproc[harness]
}

func (b *wireBackend) request(l Launch) agentwire.StartRequest {
	return agentwire.StartRequest{
		Harness: agentwire.Harness(l.Spec.Harness), Binary: l.Binary, WorkingDir: l.Dir, Env: l.Env, Home: l.Home,
		Model: l.Spec.Model, Effort: l.Spec.Effort, Permissions: l.Spec.Permissions.wire(), MCPServers: wireMCPServers(l.Spec.MCPServers),
		Skills: wireSkills(l.Spec.Skills), Instructions: l.Spec.Instructions, ExtraArgs: l.Spec.ExtraArgs, Raw: l.Spec.Raw,
		LogPath: filepath.Join(b.home, "sessions", l.Session.ID, "frames.ndjson"),
	}
}

func (b *wireBackend) startInProc(ctx context.Context, l Launch) (Handle, error) {
	req := b.request(l)
	var (
		s   agentwire.Session
		err error
	)
	if l.Resume {
		req.SessionID = l.Session.VendorID
		s, err = b.rt.Resume(ctx, req)
	} else {
		s, err = b.rt.Start(ctx, req)
	}
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", l.Spec.Harness, err)
	}
	h := &wireHandle{id: l.Session.ID, s: s, ch: make(chan Event, 64)}
	go h.pump()
	return h, nil
}

// startHeld spawns a wire holder for the session and connects to it. The
// holder gets the env of the visor and the session env, because a driver
// can read its env.
func (b *wireBackend) startHeld(ctx context.Context, l Launch) (Handle, error) {
	id := l.Session.ID
	if err := b.hs.remove(id); err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	req := b.request(l)
	cfg := holder.Config{
		ID: id, Mode: holder.ModeWire, Socket: b.hs.socket(id), ExitFile: b.hs.exitFile(id), Spool: b.hs.spool(id),
		Wire: &holder.WireConfig{
			Harness: string(req.Harness), Binary: req.Binary, WorkingDir: req.WorkingDir, Env: req.Env, Home: req.Home,
			Resume: l.Resume, VendorID: l.Session.VendorID, Model: req.Model, Effort: req.Effort, Permissions: req.Permissions,
			MCPServers: req.MCPServers, Skills: req.Skills, Instructions: req.Instructions, ExtraArgs: req.ExtraArgs, Raw: req.Raw,
			FrameLog: req.LogPath, Ledger: filepath.Join(b.home, "sessions", id, "ledger.json"),
			ClientName: b.client, ClientVersion: b.clientVersion,
		},
	}
	env := os.Environ()
	for _, k := range envKeys(l.Env) {
		env = append(env, k+"="+l.Env[k])
	}
	exited, err := b.hs.spawn(ctx, cfg, env)
	if err != nil {
		return nil, err
	}
	c, err := b.hs.dial(ctx, id, exited, wireDialTimeout)
	if err != nil {
		return nil, err
	}
	return b.track(id, l.Session.VendorID, c, holder.Cursor{}), nil
}

// reach dials a live holder with the cursor. It gives nil and no error
// when the holder ended and left its exit file.
func (b *wireBackend) reach(ctx context.Context, id string, cur holder.Cursor) (*holder.Client, error) {
	deadline := time.Now().Add(holderRedial)
	for {
		dctx, cancel := context.WithTimeout(ctx, holderRedial)
		c, err := holder.Dial(dctx, b.hs.socket(id), cur)
		cancel()
		switch {
		case err == nil:
			return c, nil
		case errors.Is(err, holder.ErrProtocol):
			return nil, err
		}
		// A holder that ends closes its socket first, then writes the exit file.
		if _, ok := b.hs.readExit(id); ok {
			return nil, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		time.Sleep(holderPoll)
	}
}

// track starts the pump of a holder. A nil client is a holder that ended
// while no visor ran.
func (b *wireBackend) track(id, vendor string, c *holder.Client, cur holder.Cursor) *heldWire {
	h := &heldWire{
		b: b, id: id, vendor: vendor, last: cur, events: make(chan Event, 64),
		stop: make(chan struct{}), done: make(chan struct{}), calls: map[uint64]chan holder.Reply{},
	}
	h.connected(c)
	b.mu.Lock()
	b.handles[h] = struct{}{}
	b.mu.Unlock()
	b.wg.Add(1)
	go h.pump()
	return h
}

func (b *wireBackend) untrack(h *heldWire) {
	b.mu.Lock()
	delete(b.handles, h)
	b.mu.Unlock()
}

func (h *wireHandle) Session() string { return h.id }

func (h *wireHandle) VendorID() string { return h.s.ID() }

func (h *wireHandle) PID() int { return h.s.PID() }

func (h *wireHandle) Send(ctx context.Context, in Input) (Delivery, error) {
	d, err := agentwire.Send(ctx, h.s, in.wire())
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", h.id, err)
	}
	return Delivery(d), nil
}

func (h *wireHandle) Interrupt(ctx context.Context) error { return h.s.Interrupt(ctx) }

func (h *wireHandle) Answer(ctx context.Context, requestID string, a Answer) error {
	return h.s.AnswerPermission(ctx, requestID, a.wire())
}

func (h *wireHandle) Close(ctx context.Context) error {
	var err error
	h.once.Do(func() { err = h.s.Close(ctx) })
	return err
}

func (h *wireHandle) Events() <-chan Event { return h.ch }

func (h *wireHandle) pump() {
	defer close(h.ch)
	for e := range h.s.Events() {
		h.ch <- wireEvent(e)
	}
}

func (h *heldWire) Session() string { return h.id }

func (h *heldWire) VendorID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.vendor
}

func (h *heldWire) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pid
}

func (h *heldWire) Send(ctx context.Context, in Input) (Delivery, error) {
	p := in.wire()
	r, err := h.call(ctx, holder.Call{Op: holder.OpSend, Prompt: &p})
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", h.id, err)
	}
	return Delivery(r.Delivery), nil
}

func (h *heldWire) Interrupt(ctx context.Context) error {
	_, err := h.call(ctx, holder.Call{Op: holder.OpInterrupt})
	return err
}

func (h *heldWire) Answer(ctx context.Context, requestID string, a Answer) error {
	d := a.wire()
	_, err := h.call(ctx, holder.Call{Op: holder.OpAnswer, RequestID: requestID, Decision: &d})
	return err
}

// Close ends the session and waits for the holder to report the exit.
func (h *heldWire) Close(ctx context.Context) error {
	if _, err := h.call(ctx, holder.Call{Op: holder.OpClose}); err != nil {
		h.b.log.Debug("close a wire holder", "session_id", h.id, "err", err)
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *heldWire) Events() <-chan Event { return h.events }

// ack tells the holder that the visor stored the event.
func (h *heldWire) ack(e Event) {
	h.mu.Lock()
	c := h.c
	ok := c != nil && e.src.Run != "" && e.src.Run == c.Hello().Run
	h.mu.Unlock()
	if !ok {
		return
	}
	if err := c.Ack(e.src.Seq); err != nil {
		h.b.log.Debug("ack a holder event", "session_id", h.id, "err", err)
	}
}

// endState gives the state of a session whose holder ended: suspended
// after the end at idle, lost when the holder is gone with no exit. Empty
// means the usual end.
func (h *heldWire) endState() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.detached:
		return ""
	case h.exit == nil:
		return StateLost
	case h.exit.Idle:
		return StateSuspended
	}
	return ""
}

// release removes the exit file and the spool, after the visor stored the
// last events. A detached handle keeps them for the next visor.
func (h *heldWire) release() {
	if h.isDetached() {
		return
	}
	if err := h.b.hs.remove(h.id); err != nil {
		h.b.log.Warn("remove the holder files", "session_id", h.id, "err", err)
	}
}

// pump reads the holder until it ends or the visor detaches. Then it sends
// the events of the spool that the visor did not store.
func (h *heldWire) pump() {
	defer h.b.wg.Done()
	defer h.b.untrack(h)
	defer close(h.done)
	defer close(h.events)
	c := h.client()
	for c != nil {
		m, err := c.Read()
		if err != nil {
			c = h.redial(c)
			continue
		}
		switch m.Type {
		case holder.MsgEvent:
			h.forward(c.Hello().Run, m.Event)
		case holder.MsgReply:
			h.reply(m.Reply)
		case holder.MsgExit:
			h.mu.Lock()
			h.exit = &m.Exit
			h.mu.Unlock()
		}
	}
	if h.isDetached() {
		return
	}
	h.mu.Lock()
	if h.exit == nil {
		if e, ok := h.b.hs.readExit(h.id); ok {
			h.exit = &e
		}
	}
	h.mu.Unlock()
	sp, err := holder.ReadSpool(h.b.hs.spool(h.id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.b.log.Warn("read the holder spool", "session_id", h.id, "err", err)
	}
	for _, we := range sp.Events {
		h.forward(sp.Run, we)
	}
}

// forward sends one holder event to the visor once.
func (h *heldWire) forward(run string, we holder.WireEvent) {
	h.mu.Lock()
	if run == h.last.Run && we.Seq <= h.last.Seq {
		h.mu.Unlock()
		return
	}
	h.last = holder.Cursor{Run: run, Seq: we.Seq}
	h.mu.Unlock()
	e := wireEvent(we.Event)
	e.src = holder.Cursor{Run: run, Seq: we.Seq}
	select {
	case h.events <- e:
	case <-h.stop:
	}
}

// redial connects again after a lost connection, for example when the
// holder dropped a slow visor. nil means that the holder ended or is gone.
func (h *heldWire) redial(old *holder.Client) *holder.Client {
	_ = old.Close() // the connection failed already
	h.mu.Lock()
	h.c = nil
	for id, ch := range h.calls {
		close(ch)
		delete(h.calls, id)
	}
	end := h.detached || h.exit != nil
	cur := h.last
	h.mu.Unlock()
	if end {
		return nil
	}
	c, err := h.b.reach(context.Background(), h.id, cur)
	if err != nil || c == nil {
		return nil
	}
	if !h.connected(c) {
		_ = c.Close() // the visor detaches
		return nil
	}
	return c
}

// connected takes a new connection. It reports false after a detach.
func (h *heldWire) connected(c *holder.Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.detached || c == nil {
		return false
	}
	hello := c.Hello()
	h.c, h.pid = c, hello.ChildPID
	h.vendor = cmp.Or(hello.VendorID, h.vendor)
	if hello.Run != h.last.Run {
		h.last = holder.Cursor{Run: hello.Run}
	}
	return true
}

// call sends a call to the holder and waits for its reply.
func (h *heldWire) call(ctx context.Context, c holder.Call) (holder.Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, wireCallTimeout)
	defer cancel()
	ch := make(chan holder.Reply, 1)
	h.mu.Lock()
	cl := h.c
	if cl != nil {
		h.nextCall++
		c.ID = h.nextCall
		h.calls[c.ID] = ch
	}
	h.mu.Unlock()
	if cl == nil {
		return holder.Reply{}, h.gone(ctx)
	}
	defer h.dropCall(c.ID)
	if err := cl.Call(c); err != nil {
		return holder.Reply{}, h.gone(ctx)
	}
	select {
	case r, ok := <-ch:
		switch {
		case !ok:
			return holder.Reply{}, h.gone(ctx)
		case r.Code == holder.CodeEnding:
			return r, errHolderEnding
		case r.Error != "":
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		return holder.Reply{}, ctx.Err()
	}
}

// gone waits a short time for the end of a holder that the visor cannot
// reach. A holder that ended at idle gives errHolderEnding.
func (h *heldWire) gone(ctx context.Context) error {
	select {
	case <-h.done:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * holderRedial):
		return fmt.Errorf("lost the connection to the holder of %s", h.id)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exit != nil && h.exit.Idle {
		return errHolderEnding
	}
	return fmt.Errorf("%w: session %s has no process", ErrState, h.id)
}

func (h *heldWire) reply(r holder.Reply) {
	h.mu.Lock()
	ch, ok := h.calls[r.ID]
	delete(h.calls, r.ID)
	h.mu.Unlock()
	if ok {
		ch <- r
	}
}

func (h *heldWire) dropCall(id uint64) {
	h.mu.Lock()
	delete(h.calls, id)
	h.mu.Unlock()
}

// detach closes the connection. The holder keeps running.
func (h *heldWire) detach() {
	h.once.Do(func() {
		h.mu.Lock()
		h.detached = true
		c := h.c
		h.mu.Unlock()
		close(h.stop)
		if c != nil {
			_ = c.Close() // the pump ends on this close
		}
	})
}

func (h *heldWire) isDetached() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.detached
}

func (h *heldWire) client() *holder.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.c
}

func (in Input) wire() agentwire.Prompt {
	return agentwire.Prompt{Text: in.Text, Kind: in.Kind, Attachments: wireAttachments(in.Attachments)}
}

func (a Answer) wire() agentwire.Decision {
	return agentwire.Decision{Allow: a.Allow, Message: a.Message, UpdatedInput: a.UpdatedInput}
}

func (p Permissions) wire() agentwire.PermissionPolicy {
	return agentwire.PermissionPolicy{Mode: p.Mode, AllowTools: p.AllowTools}
}

func wireMCPServers(in []MCPServer) []agentwire.MCPServer {
	var out []agentwire.MCPServer
	for _, m := range in {
		out = append(out, agentwire.MCPServer{Name: m.Name, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers})
	}
	return out
}

func wireSkills(in []Skill) []agentwire.Skill {
	var out []agentwire.Skill
	for _, s := range in {
		out = append(out, agentwire.Skill{Name: s.Name, Dir: s.Dir})
	}
	return out
}

func wireAttachments(in []Attachment) []agentwire.Attachment {
	var out []agentwire.Attachment
	for _, a := range in {
		out = append(out, agentwire.Attachment{MIME: a.MIME, Path: a.Path, Data: a.Data})
	}
	return out
}
