// Package agentvisor keeps agent and terminal sessions alive on one machine
// and lets a product attach to them. It does not sandbox them.
package agentvisor

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/internal/store"
	"github.com/trebi-ai/agent-visor/workdir"
)

// Options configures a visor.
type Options struct {
	// Home owns every path of the visor.
	Home string
	// Client is the product name that the harnesses see.
	Client        string
	ClientVersion string
	// Build is the build of this visor. Empty reads the module build info.
	Build string
	// Socket is the socket path that sessions get in AGENT_VISOR_SOCKET.
	Socket string
	// Env is the default env of each session.
	Env map[string]string
	// Backends are extra backends, for example an optional host of sessions.
	Backends []Backend
	// Drivers replace agent-wire drivers, for tests (for example the fake
	// harness with a script).
	Drivers map[string]agentwire.Driver
	Logger  *slog.Logger
	// KeepExited is how long the visor keeps an exited session and its
	// events. Zero keeps them for 7 days.
	KeepExited time.Duration
}

// Info describes a running visor.
type Info struct {
	Build    string `json:"build"`
	Protocol string `json:"protocol"`
	Client   string `json:"client,omitempty"`
	Home     string `json:"home"`
	Seq      uint64 `json:"seq"`
	Upgrade  string `json:"upgrade,omitempty"`
	PID      int    `json:"pid"`
}

// Visor holds the sessions of one product on one machine.
type Visor struct {
	opts     Options
	log      *slog.Logger
	st       *store.Store
	events   *eventLog
	workdirs *workdir.Manager
	wire     *agentwire.Runtime
	backends map[Kind]Backend

	ctx    context.Context //nolint:containedctx // the process-lifetime context that Close cancels
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*record
	hello    Hello
	closed   bool
}

// record is the visor state of one session.
type record struct {
	s Session
	h Handle
	// ending is the state that the session gets when its handle ends; empty
	// means exited.
	ending State
	// done closes when the pump of the current handle ends.
	done chan struct{}
}

// Protocol is the socket protocol version.
const Protocol = "agent-visor/1"

const defaultKeepExited = 7 * 24 * time.Hour

// Errors. The client maps a socket error code back to the same error.
var (
	ErrNotFound    = errors.New("agentvisor: not found")
	ErrInvalid     = errors.New("agentvisor: invalid request")
	ErrState       = errors.New("agentvisor: wrong session state")
	ErrBusy        = errors.New("agentvisor: busy")
	ErrClosed      = errors.New("agentvisor: closed")
	ErrUnsupported = errors.New("agentvisor: not supported")
	// ErrProtocol means that the two sides speak socket protocols with a
	// different major version.
	ErrProtocol = errors.New("agentvisor: unknown protocol")
)

// New opens the visor in home, recovers the sessions of the last run, and
// applies the restart policies.
func New(ctx context.Context, o Options) (*Visor, error) {
	if o.Home == "" {
		return nil, fmt.Errorf("%w: Options.Home is empty", ErrInvalid)
	}
	if err := os.MkdirAll(o.Home, 0o700); err != nil {
		return nil, fmt.Errorf("open visor: %w", err)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Build == "" {
		o.Build = moduleBuild()
	}
	if o.KeepExited == 0 {
		o.KeepExited = defaultKeepExited
	}
	st, err := store.Open(ctx, filepath.Join(o.Home, "visor.db"))
	if err != nil {
		return nil, err
	}
	v := &Visor{opts: o, log: o.Logger.With("source", "visor"), st: st, sessions: map[string]*record{}, backends: map[Kind]Backend{}}
	v.ctx, v.cancel = context.WithCancel(context.WithoutCancel(ctx))
	if v.events, err = newEventLog(ctx, st, o.Logger.With("source", "events")); err != nil {
		_ = st.Close() // the event log error is the one to report
		return nil, err
	}
	v.workdirs = workdir.New(st, filepath.Join(o.Home, "worktrees"), o.Logger, v.onScript)
	v.wire = agentwire.New(agentwire.Options{
		ClientName: o.Client, ClientVersion: o.ClientVersion, LedgerPath: filepath.Join(o.Home, "ledger.json"),
		Logger: o.Logger.With("source", "wire"),
	})
	for h, d := range o.Drivers {
		v.wire.SetDriver(agentwire.Harness(h), d)
	}
	if err := v.wire.Reconcile(); err != nil {
		v.log.Warn("reconcile the agent-wire ledger", "err", err)
	}
	v.backends[KindWire] = newWireBackend(v.wire)
	for _, b := range o.Backends {
		v.backends[b.Kind()] = b
	}
	if err := v.boot(ctx); err != nil {
		_ = v.Close(ctx) // the boot error is the one to report
		return nil, err
	}
	return v, nil
}

// Info describes the visor.
func (v *Visor) Info(context.Context) (Info, error) {
	return Info{Build: v.opts.Build, Protocol: Protocol, Client: v.opts.Client, Home: v.opts.Home, Seq: v.events.head(), PID: os.Getpid()}, nil
}

// Hello records the product that connects and describes the visor.
func (v *Visor) Hello(ctx context.Context, h Hello) (Info, error) {
	if h.Protocol != "" && protocolMajor(h.Protocol) != protocolMajor(Protocol) {
		v.log.Warn("a client speaks another protocol", "client", h.Client, "protocol", h.Protocol)
	}
	v.mu.Lock()
	v.hello = h
	v.mu.Unlock()
	return v.Info(ctx)
}

// Start starts a session.
func (v *Visor) Start(ctx context.Context, spec StartSpec) (Session, error) {
	if err := spec.Validate(); err != nil {
		return Session{}, err
	}
	spec = spec.withDefaults()
	b, ok := v.backends[spec.Kind]
	if !ok {
		return Session{}, fmt.Errorf("%w: no backend for kind %s", ErrUnsupported, spec.Kind)
	}
	w, dir, err := v.placeFor(ctx, spec)
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	s := Session{
		ID: newID("ses"), Project: w.Project, Workdir: w.ID, Kind: spec.Kind, Role: spec.Role, Title: spec.Title,
		Harness: spec.Harness, Model: spec.Model, Effort: spec.Effort, WorkingDir: dir, State: StateStarting,
		VendorID: spec.ResumeID, Labels: spec.Labels, Restart: spec.Restart, EnvKeys: envKeys(spec.Env),
		CreatedAt: now, UpdatedAt: now,
	}
	spec.WorkingDir = dir
	if err := newSpecFile(v.opts.Home, s.ID).write(spec); err != nil {
		return Session{}, err
	}
	rec := &record{s: s}
	if err := v.add(ctx, rec); err != nil {
		return Session{}, err
	}
	var prompt *Input
	if spec.Prompt != "" {
		prompt = &Input{Text: spec.Prompt}
	}
	if err := v.launch(ctx, rec, b, spec, w, spec.ResumeID != "", prompt); err != nil {
		return Session{}, err
	}
	return v.Session(ctx, s.ID)
}

// Session reads one session.
func (v *Visor) Session(_ context.Context, id string) (Session, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	rec, ok := v.sessions[id]
	if !ok {
		return Session{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	return rec.s.clone(), nil
}

// Sessions lists the sessions that match f, oldest first.
func (v *Visor) Sessions(_ context.Context, f Filter) ([]Session, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := []Session{}
	for _, rec := range v.sessions {
		if f.matchSession(rec.s) {
			out = append(out, rec.s.clone())
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// Send writes a user turn. It resumes a suspended or lost session first.
func (v *Visor) Send(ctx context.Context, id string, in Input) error {
	rec, h, err := v.lookup(id)
	if err != nil {
		return err
	}
	if h == nil {
		return v.resume(ctx, rec, &in)
	}
	v.emit(ctx, rec, Event{Type: EventUser, Text: in.Text})
	// Move first: a fast result can come before Send returns.
	if err := v.setState(ctx, rec, StateRunning); err != nil {
		return err
	}
	if err := h.Send(ctx, in); err != nil {
		v.moveIf(ctx, rec, StateRunning, StateIdle)
		return err
	}
	return nil
}

// Interrupt stops the current turn.
func (v *Visor) Interrupt(ctx context.Context, id string) error {
	_, h, err := v.lookup(id)
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("%w: session %s has no process", ErrState, id)
	}
	return h.Interrupt(ctx)
}

// Answer decides an open request.
func (v *Visor) Answer(ctx context.Context, id, requestID string, a Answer) error {
	rec, h, err := v.lookup(id)
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("%w: session %s has no process", ErrState, id)
	}
	if err := h.Answer(ctx, requestID, a); err != nil {
		return err
	}
	v.closeRequest(ctx, rec, requestID)
	return nil
}

// Stop ends a session. Its process stops, and it cannot resume.
func (v *Visor) Stop(ctx context.Context, id string) error {
	rec, h, err := v.lookup(id)
	if err != nil {
		return err
	}
	if h == nil {
		return v.setState(ctx, rec, StateExited)
	}
	v.mu.Lock()
	rec.ending = StateExited
	done := rec.done
	v.mu.Unlock()
	if err := h.Close(ctx); err != nil {
		v.log.Warn("close session", "session_id", id, "err", err)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Events gives the events after a sequence number that match f: first from
// the log, then live. Events of the visor itself (no session) match each
// filter without a session.
func (v *Visor) Events(ctx context.Context, f Filter, after uint64) (*EventStream, error) {
	if v.isClosed() {
		return nil, ErrClosed
	}
	match := func(e Event) bool { return v.eventMatch(f, e) }
	sessions := func() []string { return v.eventSessions(f) }
	return v.events.stream(ctx, after, sessions, match), nil
}

// Projects lists the projects.
func (v *Visor) Projects(ctx context.Context, _ Filter) ([]Project, error) {
	return v.workdirs.Projects(ctx)
}

// ForgetProject removes a project with no workdirs.
func (v *Visor) ForgetProject(ctx context.Context, id string) error {
	return mapWorkdirErr(v.workdirs.ForgetProject(ctx, id))
}

// Workdirs lists the workdirs that match f.
func (v *Visor) Workdirs(ctx context.Context, f Filter) ([]Workdir, error) {
	return v.workdirs.Workdirs(ctx, f.workdirFilter())
}

// EnsureWorkdir gives the workdir for w. With a key, it makes the workdir
// once and then gives the same one.
func (v *Visor) EnsureWorkdir(ctx context.Context, w WorkdirSpec) (Workdir, error) {
	if w.Path == "" {
		return Workdir{}, fmt.Errorf("%w: a workdir needs a path", ErrInvalid)
	}
	out, err := v.workdirs.Ensure(ctx, w)
	return out, mapWorkdirErr(err)
}

// ArchiveWorkdir stops the sessions of a workdir and archives it. The folder
// and the branch stay.
func (v *Visor) ArchiveWorkdir(ctx context.Context, id string) error {
	if err := v.stopWorkdir(ctx, id); err != nil {
		return err
	}
	_, err := v.workdirs.Archive(ctx, id)
	return mapWorkdirErr(err)
}

// RestoreWorkdir makes an archived workdir active again.
func (v *Visor) RestoreWorkdir(ctx context.Context, id string) (Workdir, error) {
	w, err := v.workdirs.Restore(ctx, id)
	return w, mapWorkdirErr(err)
}

// RemoveWorkdir removes a workdir. A workdir with live sessions, or a
// worktree with changes, needs force.
func (v *Visor) RemoveWorkdir(ctx context.Context, id string, force bool) error {
	live, err := v.Sessions(ctx, Filter{Workdir: id, States: []State{StateStarting, StateRunning, StateWaiting, StateIdle}})
	if err != nil {
		return err
	}
	if len(live) > 0 && !force {
		return fmt.Errorf("%w: workdir %s has %d live sessions", ErrBusy, id, len(live))
	}
	if err := v.stopWorkdir(ctx, id); err != nil {
		return err
	}
	return mapWorkdirErr(v.workdirs.Remove(ctx, id, force))
}

// Close stops the visor. A session in a turn becomes lost, and an idle wire
// session becomes suspended, so the next visor resumes it.
func (v *Visor) Close(ctx context.Context) error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	var handles []Handle
	for _, rec := range v.sessions {
		if rec.h == nil {
			continue
		}
		rec.ending = StateLost
		if rec.s.State == StateIdle && v.caps(rec.s).Resume {
			rec.ending = StateSuspended
		}
		handles = append(handles, rec.h)
	}
	v.mu.Unlock()
	for _, h := range handles {
		if err := h.Close(ctx); err != nil {
			v.log.Warn("close session", "session_id", h.Session(), "err", err)
		}
	}
	v.wg.Wait()
	v.cancel()
	for _, b := range v.backends {
		if err := b.Close(ctx); err != nil {
			v.log.Warn("close backend", "kind", b.Kind(), "err", err)
		}
	}
	v.events.close()
	return v.st.Close()
}

// boot loads the sessions, joins the sessions that lived through the restart,
// and marks the others lost or suspended.
func (v *Visor) boot(ctx context.Context) error {
	if err := v.load(ctx); err != nil {
		return err
	}
	recovered := map[string]Handle{}
	for _, b := range v.backends {
		hs, err := b.Recover(ctx)
		if err != nil {
			v.log.Warn("recover sessions", "kind", b.Kind(), "err", err)
		}
		for _, h := range hs {
			recovered[h.Session()] = h
		}
	}
	var lost []*record
	for id, rec := range v.sessions {
		if h, ok := recovered[id]; ok {
			v.attach(rec, h)
			continue
		}
		if rec.s.State.Live() {
			to := StateLost
			if rec.s.State == StateIdle && rec.s.resumable() {
				to = StateSuspended
			}
			if err := v.setState(ctx, rec, to); err != nil {
				return err
			}
		}
		if rec.s.State == StateLost && rec.s.Restart.Mode == RestartResume && rec.s.resumable() {
			lost = append(lost, rec)
		}
	}
	for _, rec := range lost {
		v.restart(rec)
	}
	return nil
}

// load reads the sessions from the store and forgets old exited sessions.
func (v *Visor) load(ctx context.Context) error {
	raw, err := v.st.List(ctx, store.KindSession)
	if err != nil {
		return err
	}
	for _, data := range raw {
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("decode session: %w", err)
		}
		if s.State == StateExited && time.Since(s.UpdatedAt) > v.opts.KeepExited {
			v.forget(ctx, s.ID)
			continue
		}
		v.sessions[s.ID] = &record{s: s}
	}
	return nil
}

// restart resumes a lost session with its restart prompt, in the background.
func (v *Visor) restart(rec *record) {
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		in := &Input{Text: rec.s.Restart.Prompt}
		if in.Text == "" {
			in = nil
		}
		if err := v.resume(v.ctx, rec, in); err != nil {
			v.log.Warn("resume a lost session", "session_id", rec.s.ID, "err", err)
		}
	}()
}

// resume starts the process of a suspended or lost session again.
func (v *Visor) resume(ctx context.Context, rec *record, in *Input) error {
	v.mu.Lock()
	s := rec.s
	v.mu.Unlock()
	if !s.resumable() {
		return fmt.Errorf("%w: session %s cannot resume", ErrState, s.ID)
	}
	spec, err := newSpecFile(v.opts.Home, s.ID).read()
	if err != nil {
		return err
	}
	b, ok := v.backends[s.Kind]
	if !ok {
		return fmt.Errorf("%w: no backend for kind %s", ErrUnsupported, s.Kind)
	}
	w, err := v.workdirs.Workdir(ctx, s.Workdir)
	if err != nil {
		return mapWorkdirErr(err)
	}
	if err := v.setState(ctx, rec, StateStarting); err != nil {
		return err
	}
	return v.launch(ctx, rec, b, spec, w, true, in)
}

// launch starts the process of a session in its backend.
func (v *Visor) launch(ctx context.Context, rec *record, b Backend, spec StartSpec, w Workdir, resume bool, prompt *Input) error {
	v.mu.Lock()
	s := rec.s
	v.mu.Unlock()
	l := Launch{
		Session: s, Spec: spec, Dir: s.WorkingDir, Resume: resume, Prompt: prompt,
		Env: mergeEnv(v.opts.Env, spec.Env, visorEnv(s, w, v.opts.Socket)),
	}
	if prompt != nil {
		v.emit(ctx, rec, Event{Type: EventUser, Text: prompt.Text})
	}
	h, err := b.Start(ctx, l)
	if err != nil {
		v.mu.Lock()
		rec.s.Error = err.Error()
		v.mu.Unlock()
		if serr := v.setState(ctx, rec, StateExited); serr != nil {
			v.log.Warn("mark a failed session exited", "session_id", s.ID, "err", serr)
		}
		return err
	}
	v.attach(rec, h)
	// The pump can move the state first, when a fast turn ends.
	to := StateIdle
	if prompt != nil {
		to = StateRunning
	}
	v.moveIf(ctx, rec, StateStarting, to)
	return nil
}

// moveIf moves the state only when the session is still in from.
func (v *Visor) moveIf(ctx context.Context, rec *record, from, to State) {
	v.mu.Lock()
	ok := rec.s.State == from
	v.mu.Unlock()
	if ok {
		if err := v.setState(ctx, rec, to); err != nil {
			v.log.Debug("ignore a state move", "session_id", rec.s.ID, "err", err)
		}
	}
}

// attach binds a live handle to a session and starts its pump.
func (v *Visor) attach(rec *record, h Handle) {
	v.mu.Lock()
	rec.h, rec.ending, rec.done = h, "", make(chan struct{})
	rec.s.PID = h.PID()
	if id := h.VendorID(); id != "" {
		rec.s.VendorID = id
	}
	done := rec.done
	v.mu.Unlock()
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		defer close(done)
		for e := range h.Events() {
			v.observe(rec, e)
		}
		v.ended(rec, h)
	}()
}

// observe records one backend event and moves the session state.
func (v *Visor) observe(rec *record, e Event) {
	ctx := v.ctx
	switch e.Type {
	case EventInit:
		v.update(ctx, rec, func(s *Session) { s.VendorID = cmp.Or(e.VendorID, s.VendorID) })
	case EventRequest:
		if e.Request != nil {
			v.update(ctx, rec, func(s *Session) { s.Pending = append(s.Pending, *e.Request) })
		}
	case EventResult:
		v.update(ctx, rec, func(s *Session) {
			s.Pending, s.Turns = nil, s.Turns+1
			if e.Result != nil {
				s.VendorID = cmp.Or(e.Result.VendorID, s.VendorID)
			}
		})
	case EventExit:
		v.update(ctx, rec, func(s *Session) { s.ExitCode, s.Error = e.ExitCode, cmp.Or(e.Error, s.Error) })
	}
	v.emit(ctx, rec, e)
	if to, ok := v.stateAfter(rec, e); ok {
		if err := v.setState(ctx, rec, to); err != nil {
			v.log.Debug("ignore a state move", "session_id", rec.s.ID, "err", err)
		}
	}
}

// stateAfter gives the state that an event moves the session to.
func (v *Visor) stateAfter(rec *record, e Event) (State, bool) {
	v.mu.Lock()
	pending := len(rec.s.Pending)
	v.mu.Unlock()
	switch e.Type {
	case EventRequest:
		return StateWaiting, true
	case EventResult:
		return StateIdle, true
	case EventAssistant, EventThought, EventTool:
		if pending == 0 {
			return StateRunning, true
		}
	case EventStatus:
		switch agentwire.SessionStatus(e.Status) {
		case agentwire.StatusRunning:
			return StateRunning, pending == 0
		case agentwire.StatusWaiting:
			return StateWaiting, true
		case agentwire.StatusIdle:
			return StateIdle, true
		}
	}
	return "", false
}

// ended moves a session whose handle closed.
func (v *Visor) ended(rec *record, h Handle) {
	v.mu.Lock()
	if rec.h != h {
		v.mu.Unlock()
		return
	}
	to := rec.ending
	if to == "" {
		to = StateExited
	}
	rec.h, rec.s.PID = nil, 0
	if to == StateExited {
		rec.s.Pending = nil
	}
	v.mu.Unlock()
	if err := v.setState(v.ctx, rec, to); err != nil {
		v.log.Warn("mark an ended session", "session_id", rec.s.ID, "err", err)
	}
}

func (v *Visor) closeRequest(ctx context.Context, rec *record, requestID string) {
	v.update(ctx, rec, func(s *Session) {
		s.Pending = slices.DeleteFunc(s.Pending, func(r Request) bool { return r.ID == requestID })
	})
	v.emit(ctx, rec, Event{Type: EventRequestClosed, RequestID: requestID})
	v.mu.Lock()
	pending := len(rec.s.Pending)
	v.mu.Unlock()
	if pending == 0 {
		if err := v.setState(ctx, rec, StateRunning); err != nil {
			v.log.Debug("ignore a state move", "session_id", rec.s.ID, "err", err)
		}
	}
}

// setState moves the state, saves the session, and emits session.state.
func (v *Visor) setState(ctx context.Context, rec *record, to State) error {
	v.mu.Lock()
	if rec.s.State == to {
		v.mu.Unlock()
		return nil
	}
	if err := rec.s.move(to); err != nil {
		v.mu.Unlock()
		return err
	}
	v.mu.Unlock()
	if err := v.save(ctx, rec); err != nil {
		return err
	}
	v.emit(ctx, rec, Event{Type: EventSessionState, State: to})
	return nil
}

// update changes the session under the lock and saves it.
func (v *Visor) update(ctx context.Context, rec *record, f func(*Session)) {
	v.mu.Lock()
	f(&rec.s)
	rec.s.UpdatedAt = time.Now().UTC()
	v.mu.Unlock()
	if err := v.save(ctx, rec); err != nil {
		v.log.Warn("save session", "session_id", rec.s.ID, "err", err)
	}
}

func (v *Visor) save(ctx context.Context, rec *record) error {
	v.mu.Lock()
	data, err := json.Marshal(rec.s)
	id := rec.s.ID
	v.mu.Unlock()
	if err != nil {
		return fmt.Errorf("encode session %s: %w", id, err)
	}
	return v.st.Put(context.WithoutCancel(ctx), store.KindSession, id, data)
}

func (v *Visor) emit(ctx context.Context, rec *record, e Event) {
	v.mu.Lock()
	e.Session = rec.s.ID
	v.mu.Unlock()
	out, err := v.events.append(ctx, e)
	if err != nil {
		v.log.Warn("append event", "session_id", e.Session, "type", e.Type, "err", err)
		return
	}
	v.mu.Lock()
	rec.s.Seq = out.Seq
	v.mu.Unlock()
}

func (v *Visor) add(ctx context.Context, rec *record) error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return ErrClosed
	}
	v.sessions[rec.s.ID] = rec
	v.mu.Unlock()
	if err := v.save(ctx, rec); err != nil {
		return err
	}
	v.emit(ctx, rec, Event{Type: EventSessionState, State: StateStarting})
	return nil
}

func (v *Visor) forget(ctx context.Context, id string) {
	if err := v.st.Delete(ctx, store.KindSession, id); err != nil {
		v.log.Warn("forget session", "session_id", id, "err", err)
	}
	if err := v.st.DeleteSessionEvents(ctx, id); err != nil {
		v.log.Warn("forget session events", "session_id", id, "err", err)
	}
	if err := newSpecFile(v.opts.Home, id).remove(); err != nil {
		v.log.Warn("forget session files", "session_id", id, "err", err)
	}
}

func (v *Visor) lookup(id string) (*record, Handle, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, nil, ErrClosed
	}
	rec, ok := v.sessions[id]
	if !ok {
		return nil, nil, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	return rec, rec.h, nil
}

// placeFor gives the workdir and the working folder of a new session.
func (v *Visor) placeFor(ctx context.Context, spec StartSpec) (Workdir, string, error) {
	if spec.Workdir != "" {
		w, err := v.workdirs.Workdir(ctx, spec.Workdir)
		if err != nil {
			return Workdir{}, "", mapWorkdirErr(err)
		}
		if w.State != workdir.Active {
			return Workdir{}, "", fmt.Errorf("%w: workdir %s is %s", ErrState, w.ID, w.State)
		}
		return w, cmp.Or(spec.WorkingDir, w.Path), nil
	}
	dir := spec.WorkingDir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return Workdir{}, "", fmt.Errorf("start session: %w", err)
		}
		dir = wd
	}
	w, err := v.workdirs.ForPath(ctx, dir)
	if err != nil {
		return Workdir{}, "", mapWorkdirErr(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Workdir{}, "", fmt.Errorf("start session: %w", err)
	}
	return w, abs, nil
}

func (v *Visor) stopWorkdir(ctx context.Context, id string) error {
	live, err := v.Sessions(ctx, Filter{Workdir: id})
	if err != nil {
		return err
	}
	for _, s := range live {
		if s.State == StateExited {
			continue
		}
		if err := v.Stop(ctx, s.ID); err != nil {
			return err
		}
	}
	return nil
}

func (v *Visor) onScript(s workdir.Script) {
	data, err := json.Marshal(map[string]any{"workdir": s.Workdir, "name": s.Name, "command": s.Command, "output": string(s.Output), "ok": s.Err == nil})
	if err != nil {
		return
	}
	if _, err := v.events.append(v.ctx, Event{Type: EventScript, Data: data}); err != nil {
		v.log.Warn("append script event", "workdir_id", s.Workdir, "err", err)
	}
}

func (v *Visor) eventMatch(f Filter, e Event) bool {
	if e.Session == "" {
		return f.Session == ""
	}
	v.mu.Lock()
	rec, ok := v.sessions[e.Session]
	var s Session
	if ok {
		s = rec.s
	}
	v.mu.Unlock()
	if !ok {
		return false
	}
	f.States = nil
	return f.matchSession(s)
}

// eventSessions narrows the replay query. nil reads all sessions.
func (v *Visor) eventSessions(f Filter) []string {
	if f.Session == "" && f.Workdir == "" && f.Project == "" && f.Kind == "" && f.Role == "" && len(f.Labels) == 0 {
		return nil
	}
	if f.Session != "" {
		return []string{f.Session}
	}
	f.States = nil
	v.mu.Lock()
	defer v.mu.Unlock()
	ids := []string{""}
	for id, rec := range v.sessions {
		if f.matchSession(rec.s) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (v *Visor) caps(s Session) Caps {
	if b, ok := v.backends[s.Kind]; ok {
		return b.Caps(s.Harness)
	}
	return Caps{}
}

func (v *Visor) isClosed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closed
}

func mapWorkdirErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workdir.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, workdir.ErrDirty), errors.Is(err, workdir.ErrHasWork):
		return fmt.Errorf("%w: %w", ErrBusy, err)
	default:
		return err
	}
}

func moduleBuild() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return s.Value[:12]
		}
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// protocolMajor gives the major version of a protocol name, for example 1
// for agent-visor/1.
func protocolMajor(p string) string {
	_, v, _ := strings.Cut(p, "/")
	major, _, _ := strings.Cut(v, ".")
	return major
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return prefix + "_" + hex.EncodeToString(b)
}
