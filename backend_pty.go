package agentvisor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/holder"
)

// TerminalCommand builds the command line of a vendor terminal program for
// an agent session in a PTY. l.Resume asks for the command that continues
// the vendor session l.Session.VendorID.
type TerminalCommand func(l Launch) ([]string, error)

// ptyBackend hosts sessions in a PTY. A holder process owns each PTY, so the
// session lives through a visor restart.
type ptyBackend struct {
	home      string
	holderCmd []string
	termCmd   TerminalCommand
	log       *slog.Logger

	mu      sync.Mutex
	handles map[*ptyHandle]struct{}
	wg      sync.WaitGroup
}

// ptyHandle is the connection of the visor to one holder. It keeps a copy
// of the screen buffer for the attachments.
type ptyHandle struct {
	b      *ptyBackend
	id     string
	vendor string
	events chan Event
	// stop closes on detach; done closes when the pump ends.
	stop chan struct{}
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	c        *holder.Client
	pid      int
	size     holder.Size
	ring     []byte
	subs     map[*AttachStream]struct{}
	lease    *AttachStream
	detached bool
	ended    bool
}

var (
	_ Backend = (*ptyBackend)(nil)
	_ Handle  = (*ptyHandle)(nil)
)

const (
	holderDialTimeout = 10 * time.Second
	holderRedial      = 2 * time.Second
	holderPoll        = 20 * time.Millisecond
	defaultTerm       = "xterm-256color"
)

func newPTYBackend(home string, holderCmd []string, termCmd TerminalCommand, log *slog.Logger) *ptyBackend {
	return &ptyBackend{home: home, holderCmd: holderCmd, termCmd: termCmd, log: log, handles: map[*ptyHandle]struct{}{}}
}

func (b *ptyBackend) Kind() Kind { return KindPTY }

func (b *ptyBackend) Caps(string) Caps { return Caps{Terminal: true, Survives: true} }

// Start spawns a holder for the session and connects to it. A first prompt
// goes to the terminal as typed input.
func (b *ptyBackend) Start(ctx context.Context, l Launch) (Handle, error) {
	env := childEnv(l.Env)
	argv, err := b.command(l, env)
	if err != nil {
		return nil, err
	}
	id := l.Session.ID
	cfg := holder.Config{
		ID: id, Socket: b.socket(id), Command: argv, Dir: l.Dir, Env: env,
		Log: filepath.Join(b.home, "sessions", id, "pty.log"), ExitFile: b.exitFile(id),
	}
	exited, err := b.spawn(ctx, cfg)
	if err != nil {
		return nil, err
	}
	c, err := b.dial(ctx, id, exited)
	if err != nil {
		return nil, err
	}
	h := b.track(id, l.Session.VendorID, c)
	if l.Prompt != nil {
		if err := h.Send(ctx, *l.Prompt); err != nil {
			_ = h.Close(context.WithoutCancel(ctx)) // the send error is the one to report
			return nil, err
		}
	}
	return h, nil
}

// Recover connects to each live holder. A holder that ended while no visor
// ran left an exit file; its handle reports the exit and ends.
func (b *ptyBackend) Recover(ctx context.Context) ([]Handle, error) {
	entries, err := os.ReadDir(filepath.Join(b.home, "holders"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recover holders: %w", err)
	}
	var out []Handle
	live := map[string]bool{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".sock")
		if !ok {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, holderRedial)
		c, err := holder.Dial(dctx, b.socket(id))
		cancel()
		if err != nil {
			b.log.Warn("recover a holder", "session_id", id, "err", err)
			if !errors.Is(err, holder.ErrProtocol) {
				_ = os.Remove(b.socket(id)) // a stale socket of a dead holder
			}
			continue
		}
		live[id] = true
		out = append(out, b.track(id, "", c))
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".exit")
		if !ok || live[id] {
			continue
		}
		out = append(out, b.track(id, "", nil))
	}
	return out, nil
}

// Close detaches from each holder. The holders and their processes keep
// running.
func (b *ptyBackend) Close(context.Context) error {
	b.mu.Lock()
	hs := make([]*ptyHandle, 0, len(b.handles))
	for h := range b.handles {
		hs = append(hs, h)
	}
	b.mu.Unlock()
	for _, h := range hs {
		h.detach()
	}
	b.wg.Wait()
	return nil
}

// command gives the command line of the session.
func (b *ptyBackend) command(l Launch, env []string) ([]string, error) {
	switch l.Spec.Role {
	case RoleShell:
		if len(l.Spec.Command) > 0 {
			return l.Spec.Command, nil
		}
		return []string{cmp.Or(envValue(env, "SHELL"), "/bin/sh"), "-l"}, nil
	case RoleScript:
		if len(l.Spec.Command) == 0 {
			return nil, fmt.Errorf("%w: a script session needs a command", ErrInvalid)
		}
		return l.Spec.Command, nil
	}
	if b.termCmd != nil {
		return b.termCmd(l)
	}
	if l.Resume {
		return nil, fmt.Errorf("%w: no terminal resume command for harness %s", ErrUnsupported, l.Spec.Harness)
	}
	if len(l.Spec.Command) > 0 {
		return l.Spec.Command, nil
	}
	argv := []string{cmp.Or(l.Binary, l.Spec.Harness)}
	argv = append(argv, agentwire.ModelArgs(agentwire.Harness(l.Spec.Harness), l.Spec.Model, l.Spec.Effort)...)
	return append(argv, l.Spec.ExtraArgs...), nil
}

// spawn starts the holder process in its own process session, so that it
// outlives the visor. The channel gives the end of the process.
func (b *ptyBackend) spawn(ctx context.Context, cfg holder.Config) (<-chan error, error) {
	if len(b.holderCmd) == 0 {
		return nil, fmt.Errorf("%w: no holder command", ErrUnsupported)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	dir := filepath.Join(b.home, "sessions", cfg.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	logf, err := os.OpenFile(filepath.Join(dir, "holder.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	defer logf.Close()
	args := append(append([]string{}, b.holderCmd[1:]...), cfg.ID)
	cmd := exec.CommandContext(context.WithoutCancel(ctx), b.holderCmd[0], args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(data), logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	exited := make(chan error, 1)
	// The goroutine reaps the holder; it ends with the holder process.
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// dial waits for the socket of a new holder.
func (b *ptyBackend) dial(ctx context.Context, id string, exited <-chan error) (*holder.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, holderDialTimeout)
	defer cancel()
	t := time.NewTicker(holderPoll)
	defer t.Stop()
	for {
		if _, err := os.Stat(b.socket(id)); err == nil {
			if c, err := holder.Dial(ctx, b.socket(id)); err == nil {
				return c, nil
			}
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("holder of %s ended: %w: %s", id, err, b.holderLog(id))
		case <-ctx.Done():
			return nil, fmt.Errorf("dial holder of %s: %w", id, ctx.Err())
		case <-t.C:
		}
	}
}

// track starts the pump of a holder. A nil client is a holder that ended
// while no visor ran.
func (b *ptyBackend) track(id, vendor string, c *holder.Client) *ptyHandle {
	h := &ptyHandle{
		b: b, id: id, vendor: vendor, c: c, events: make(chan Event, 8),
		stop: make(chan struct{}), done: make(chan struct{}), subs: map[*AttachStream]struct{}{},
	}
	if c != nil {
		hello := c.Hello()
		h.pid, h.size = hello.ChildPID, holder.Size{Cols: hello.Cols, Rows: hello.Rows}
	}
	b.mu.Lock()
	b.handles[h] = struct{}{}
	b.mu.Unlock()
	b.wg.Add(1)
	go h.pump()
	return h
}

func (b *ptyBackend) untrack(h *ptyHandle) {
	b.mu.Lock()
	delete(b.handles, h)
	b.mu.Unlock()
}

func (b *ptyBackend) socket(id string) string {
	return filepath.Join(b.home, "holders", id+".sock")
}

func (b *ptyBackend) exitFile(id string) string {
	return filepath.Join(b.home, "holders", id+".exit")
}

// readExit reads the exit code that a holder wrote. -1 means unknown.
func (b *ptyBackend) readExit(id string) int {
	data, err := os.ReadFile(b.exitFile(id))
	if err != nil {
		return -1
	}
	var e holder.Exit
	if json.Unmarshal(data, &e) != nil {
		return -1
	}
	return e.Code
}

// holderLog gives the end of the log of a holder, for an error.
func (b *ptyBackend) holderLog(id string) string {
	data, err := os.ReadFile(filepath.Join(b.home, "sessions", id, "holder.log"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data[max(0, len(data)-512):]))
}

func (h *ptyHandle) Session() string { return h.id }

func (h *ptyHandle) VendorID() string { return h.vendor }

func (h *ptyHandle) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pid
}

// Send types the text and a return.
func (h *ptyHandle) Send(_ context.Context, in Input) error {
	return h.write([]byte(in.Text + "\r"))
}

func (h *ptyHandle) Interrupt(context.Context) error { return h.write([]byte{0x03}) }

func (h *ptyHandle) Answer(context.Context, string, Answer) error {
	return fmt.Errorf("%w: a terminal session takes answers as input", ErrUnsupported)
}

// Close ends the process and waits for the holder to report the exit.
func (h *ptyHandle) Close(ctx context.Context) error {
	if c := h.client(); c != nil {
		if err := c.Kill(); err != nil {
			h.b.log.Debug("kill the holder process", "session_id", h.id, "err", err)
		}
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *ptyHandle) Events() <-chan Event { return h.events }

// pump reads the holder until the process ends or the visor detaches.
func (h *ptyHandle) pump() {
	defer h.b.wg.Done()
	defer h.b.untrack(h)
	defer close(h.done)
	defer close(h.events)
	c := h.client()
	for c != nil {
		m, err := c.Read()
		if err != nil {
			c = h.redial()
			continue
		}
		switch m.Type {
		case holder.MsgSnapshot:
			h.snapshot(m.Data)
		case holder.MsgOutput:
			h.output(m.Data)
		case holder.MsgResized:
			h.resized(m.Size)
		case holder.MsgExit:
			h.finish(&m.Exit.Code)
			return
		}
	}
	if h.isDetached() {
		h.endAll(EndClosed)
		return
	}
	var code *int
	if c := h.b.readExit(h.id); c >= 0 {
		code = &c
	}
	h.finish(code)
}

// redial connects again after a lost connection, for example when the
// holder dropped a slow visor. nil means that the holder is gone.
func (h *ptyHandle) redial() *holder.Client {
	h.mu.Lock()
	old := h.c
	h.c = nil
	h.mu.Unlock()
	_ = old.Close() // the connection failed already
	deadline := time.Now().Add(holderRedial)
	for !h.isDetached() && time.Now().Before(deadline) {
		if _, err := os.Stat(h.b.socket(h.id)); err != nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), holderRedial)
		c, err := holder.Dial(ctx, h.b.socket(h.id))
		cancel()
		if err == nil {
			h.mu.Lock()
			if h.detached {
				h.mu.Unlock()
				_ = c.Close() // the visor detaches
				return nil
			}
			h.c = c
			h.mu.Unlock()
			return c
		}
		time.Sleep(holderPoll)
	}
	return nil
}

// finish reports the exit and ends the attachments.
func (h *ptyHandle) finish(code *int) {
	select {
	case h.events <- Event{Type: EventExit, ExitCode: code}:
	case <-h.stop:
	}
	if err := os.Remove(h.b.exitFile(h.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.b.log.Warn("remove the exit file", "session_id", h.id, "err", err)
	}
	h.mu.Lock()
	if h.c != nil {
		_ = h.c.Close() // the holder ends
		h.c = nil
	}
	h.mu.Unlock()
	h.endAll(EndExited)
}

// detach closes the connection. The holder keeps running.
func (h *ptyHandle) detach() {
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

func (h *ptyHandle) isDetached() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.detached
}

func (h *ptyHandle) client() *holder.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.c
}

func (h *ptyHandle) write(p []byte) error {
	c := h.client()
	if c == nil {
		return fmt.Errorf("%w: session %s has no terminal", ErrState, h.id)
	}
	if err := c.Input(p); err != nil {
		return fmt.Errorf("write to %s: %w", h.id, err)
	}
	return nil
}

// join adds an attachment: the attached frame, the snapshot, and the lease.
func (h *ptyHandle) join(a *AttachStream, o AttachOptions, title string) error {
	h.mu.Lock()
	if h.ended {
		h.mu.Unlock()
		return fmt.Errorf("%w: session %s ended", ErrState, h.id)
	}
	h.subs[a] = struct{}{}
	a.queue = []Frame{
		{Type: FrameAttached, View: ViewTerminal, Source: KindPTY, Cols: h.size.Cols, Rows: h.size.Rows, Writable: true, Resizable: true, Title: title},
		{Op: OpSnapshot, Data: bytes.Clone(h.ring)},
	}
	take := h.lease == nil && o.Cols > 0 && o.Rows > 0
	if !take {
		a.queue = append(a.queue, Frame{Type: FrameLease, Holder: h.holderFor(a)})
	}
	h.mu.Unlock()
	if take {
		return h.take(a, o.Cols, o.Rows)
	}
	return nil
}

// leave removes an attachment and frees its lease.
func (h *ptyHandle) leave(a *AttachStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, a)
	if h.lease == a {
		h.lease = nil
		h.notifyLease()
	}
}

// take gives the size lease to a and resizes the PTY.
func (h *ptyHandle) take(a *AttachStream, cols, rows int) error {
	h.mu.Lock()
	if h.lease != a {
		h.lease = a
		h.notifyLease()
	}
	c := h.c
	h.mu.Unlock()
	if c == nil {
		return fmt.Errorf("%w: session %s has no terminal", ErrState, h.id)
	}
	return c.Resize(cols, rows)
}

func (h *ptyHandle) release(a *AttachStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lease == a {
		h.lease = nil
		h.notifyLease()
	}
}

// notifyLease tells each attachment who holds the lease. The caller holds
// h.mu.
func (h *ptyHandle) notifyLease() {
	for a := range h.subs {
		a.push(Frame{Type: FrameLease, Holder: h.holderFor(a)})
	}
}

func (h *ptyHandle) holderFor(a *AttachStream) LeaseHolder {
	switch h.lease {
	case nil:
		return LeaseNone
	case a:
		return LeaseYou
	}
	return LeaseOther
}

// resnapshot gives a stale attachment the screen buffer.
func (h *ptyHandle) resnapshot(a *AttachStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a.resync(bytes.Clone(h.ring))
}

// snapshot replaces the screen buffer after a connect. Each attachment gets
// the new buffer.
func (h *ptyHandle) snapshot(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = p
	for a := range h.subs {
		a.markStale()
	}
}

func (h *ptyHandle) output(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = append(h.ring, p...)
	if over := len(h.ring) - holder.DefaultRing; over > 0 {
		h.ring = append(h.ring[:0:0], h.ring[over:]...)
	}
	for a := range h.subs {
		a.output(p)
	}
}

func (h *ptyHandle) resized(s holder.Size) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.size = s
	for a := range h.subs {
		a.push(Frame{Type: FrameResized, Cols: s.Cols, Rows: s.Rows})
	}
}

func (h *ptyHandle) endAll(reason EndReason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ended = true
	for a := range h.subs {
		a.end(reason)
	}
}

// childEnv gives the env of the process in the PTY: the env of the visor
// process, then the session env.
func childEnv(env map[string]string) []string {
	merged := map[string]string{"TERM": defaultTerm}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	for k, v := range env {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for _, k := range envKeys(merged) {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}
