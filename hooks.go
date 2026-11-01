package agentvisor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/hooks"
)

// HookCall is one call of a vendor hook.
type HookCall struct {
	Harness string `json:"harness"`
	// Session is the AGENT_VISOR_SESSION value of the hook process, or empty
	// for a session that started outside the visor.
	Session string `json:"session,omitempty"`
	// Labels find the session when Session is empty, for example the
	// terminal of another app. A new observed session gets them.
	Labels  map[string]string `json:"labels,omitempty"`
	Payload json.RawMessage   `json:"payload"`
	// At is when the hook ran. Zero is now.
	At time.Time `json:"at,omitzero"`
}

// HookSpec selects the vendor hook configs. The empty spec selects each
// harness with hook support in the user login and in the visor login.
type HookSpec struct {
	Harnesses []string `json:"harnesses,omitempty"`
	// Auth selects the login home: shared is the user login, separate is
	// the login of the visor. Empty selects both, and the visor login only
	// when its folder exists.
	Auth AuthMode `json:"auth,omitempty"`
	// Dir is a vendor home that replaces the login homes.
	Dir string `json:"dir,omitempty"`
}

// HookState is the hook state of one vendor config.
type HookState = hooks.State

// HookStatus is the hook state of one harness in one vendor home.
type HookStatus struct {
	Harness string `json:"harness"`
	// Auth is empty when HookSpec.Dir selects the home.
	Auth  AuthMode  `json:"auth,omitempty"`
	Dir   string    `json:"dir"`
	State HookState `json:"state"`
}

// HookSpool keeps the hook calls that did not reach the visor. The visor
// applies them at its next start.
type HookSpool struct{ dir string }

// hookTarget is one vendor home of one harness.
type hookTarget struct {
	harness string
	auth    AuthMode
	dir     string
}

// Hook states.
const (
	HookInstalled = hooks.StateInstalled
	HookMissing   = hooks.StateMissing
	HookOutdated  = hooks.StateOutdated
	// HookUntrusted means that the vendor runs the hooks only after the user
	// trusts them in the vendor CLI.
	HookUntrusted = hooks.StateUntrusted
)

// Result subtypes of a turn that a hook ends.
const (
	hookSuccess     = "success"
	hookInterrupted = "interrupted"
	hookError       = "error"
)

// NewHookSpool gives the spool of the visor in home.
func NewHookSpool(home string) *HookSpool {
	return &HookSpool{dir: filepath.Join(home, "hooks", "spool")}
}

// Write keeps one call. The file name starts with the call time, so a replay
// keeps the order.
func (s *HookSpool) Write(c HookCall) error {
	if len(c.Payload) > hooks.MaxPayload {
		return fmt.Errorf("%w: hook payload of %d bytes", ErrInvalid, len(c.Payload))
	}
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("spool hook call: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("spool hook call: %w", err)
	}
	f, err := os.CreateTemp(s.dir, ".call-*")
	if err != nil {
		return fmt.Errorf("spool hook call: %w", err)
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := cmp.Or(werr, cerr); err != nil {
		_ = os.Remove(f.Name()) // the write error is the one to report
		return fmt.Errorf("spool hook call: %w", err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	name := fmt.Sprintf("%020d-%s.json", c.At.UnixNano(), hex.EncodeToString(b))
	if err := os.Rename(f.Name(), filepath.Join(s.dir, name)); err != nil {
		_ = os.Remove(f.Name()) // the rename error is the one to report
		return fmt.Errorf("spool hook call: %w", err)
	}
	return nil
}

// replay applies the calls in order and deletes each one after it. A call
// that apply rejects is deleted too. ErrClosed stops the replay.
func (s *HookSpool) replay(ctx context.Context, apply func(context.Context, HookCall) error) error {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("replay hook calls: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("replay %s: %w", e.Name(), err))
			continue
		}
		var c HookCall
		if err := json.Unmarshal(data, &c); err != nil {
			errs = append(errs, fmt.Errorf("replay %s: %w", e.Name(), err))
		} else if err := apply(ctx, c); errors.Is(err, ErrClosed) || ctx.Err() != nil {
			return cmp.Or(ctx.Err(), err)
		} else if err != nil {
			errs = append(errs, fmt.Errorf("replay %s: %w", e.Name(), err))
		}
		if err := os.Remove(path); err != nil {
			errs = append(errs, fmt.Errorf("replay %s: %w", e.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Hook applies one hook call. With a session id it updates that session and
// never makes one. Without, it makes or updates an observed session for the
// vendor session. The session is empty when the call changes no session.
func (v *Visor) Hook(ctx context.Context, c HookCall) (Session, error) {
	e, ok, err := hooks.Parse(c.Harness, c.Payload)
	if err != nil {
		return Session{}, fmt.Errorf("%w: hook call: %w", ErrInvalid, err)
	}
	var rec *record
	if c.Session != "" {
		if rec, _, err = v.lookup(c.Session); err != nil {
			return Session{}, err
		}
	} else if rec = v.labeled(c.Labels); rec == nil {
		if rec, err = v.observed(ctx, e, ok, c.Labels); err != nil || rec == nil {
			return Session{}, err
		}
	}
	data, err := compactJSON(c.Payload)
	if err != nil {
		return Session{}, fmt.Errorf("%w: hook call: %w", ErrInvalid, err)
	}
	v.emit(ctx, rec, Event{Type: EventHook, At: c.At, VendorID: e.VendorID, Harness: c.Harness, Data: data})
	if ok {
		v.applyHook(ctx, rec, e)
	}
	return v.Session(ctx, rec.s.ID)
}

// Hooks gives the hook state of the vendor configs in spec.
func (v *Visor) Hooks(ctx context.Context, spec HookSpec) ([]HookStatus, error) {
	return v.eachHook(ctx, spec, nil)
}

// InstallHooks adds or updates the hook entries of the visor in the vendor
// configs. The other entries and the rest of each file stay.
func (v *Visor) InstallHooks(ctx context.Context, spec HookSpec) ([]HookStatus, error) {
	return v.eachHook(ctx, spec, (*hooks.Installer).Install)
}

// RemoveHooks takes the hook entries of the visor out of the vendor configs.
func (v *Visor) RemoveHooks(ctx context.Context, spec HookSpec) ([]HookStatus, error) {
	return v.eachHook(ctx, spec, (*hooks.Installer).Remove)
}

// eachHook runs change on each target of spec, then reads its state.
func (v *Visor) eachHook(ctx context.Context, spec HookSpec, change func(*hooks.Installer, string, string) error) ([]HookStatus, error) {
	targets, err := v.hookTargets(spec)
	if err != nil {
		return nil, err
	}
	inst, err := v.hookInstaller()
	if err != nil {
		return nil, err
	}
	out := make([]HookStatus, 0, len(targets))
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if change != nil {
			if err := change(inst, t.harness, t.dir); err != nil {
				return nil, fmt.Errorf("hooks of %s in %s: %w", t.harness, t.dir, err)
			}
		}
		st, err := inst.Status(t.harness, t.dir)
		if err != nil {
			return nil, err
		}
		out = append(out, HookStatus{Harness: t.harness, Auth: t.auth, Dir: t.dir, State: st})
	}
	return out, nil
}

// hookInstaller uses Options.HookCommand, or this program with the home and
// the hook command.
func (v *Visor) hookInstaller() (*hooks.Installer, error) {
	argv := v.opts.HookCommand
	if len(argv) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("%w: no hook command: %w", ErrUnsupported, err)
		}
		argv = []string{exe, "--home", v.opts.Home, "hook"}
	}
	inst, err := hooks.NewInstaller(v.opts.Client, argv)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return inst, nil
}

// hookTargets gives the vendor homes that spec selects.
func (v *Visor) hookTargets(spec HookSpec) ([]hookTarget, error) {
	names := spec.Harnesses
	if len(names) == 0 {
		names = hooks.Names()
	}
	switch {
	case spec.Auth != "" && spec.Auth != AuthShared && spec.Auth != AuthSeparate:
		return nil, fmt.Errorf("%w: auth mode %q", ErrInvalid, spec.Auth)
	case spec.Dir != "" && !filepath.IsAbs(spec.Dir):
		return nil, fmt.Errorf("%w: hook dir %q is not absolute", ErrInvalid, spec.Dir)
	}
	var out []hookTarget
	for _, h := range names {
		if !hooks.Supported(h) {
			return nil, fmt.Errorf("%w: harness %s has no hooks", ErrUnsupported, h)
		}
		homes, err := v.hookHomes(h, spec)
		if err != nil {
			return nil, err
		}
		out = append(out, homes...)
	}
	return out, nil
}

// hookHomes gives the vendor homes of one harness that spec selects.
func (v *Visor) hookHomes(h string, spec HookSpec) ([]hookTarget, error) {
	if spec.Dir != "" {
		return []hookTarget{{harness: h, dir: spec.Dir}}, nil
	}
	var out []hookTarget
	if spec.Auth != AuthSeparate {
		dir := agentwire.DefaultHome(agentwire.Harness(h))
		if dir == "" && spec.Auth == AuthShared {
			return nil, fmt.Errorf("%w: no user login home for %s", ErrUnsupported, h)
		}
		if dir != "" {
			out = append(out, hookTarget{harness: h, auth: AuthShared, dir: dir})
		}
	}
	if spec.Auth != AuthShared {
		// The harness manager keeps the visor login here.
		dir := filepath.Join(v.opts.Home, "harness", h)
		if _, err := os.Stat(dir); err == nil || spec.Auth == AuthSeparate {
			out = append(out, hookTarget{harness: h, auth: AuthSeparate, dir: dir})
		}
	}
	return out, nil
}

// observed finds the observed session of a vendor session, or makes one in
// the workdir of its folder. It makes none for an end or an ignored event.
func (v *Visor) observed(ctx context.Context, e hooks.Event, ok bool, labels map[string]string) (*record, error) {
	if rec := v.findObserved(e); rec != nil || !ok || e.Kind == hooks.KindSessionEnd {
		return rec, nil
	}
	if e.Cwd == "" || !filepath.IsAbs(e.Cwd) {
		return nil, fmt.Errorf("%w: hook call has no absolute cwd", ErrInvalid)
	}
	w, err := v.workdirs.ForPath(ctx, e.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: workdir of the hook cwd: %w", ErrInvalid, err)
	}
	now := time.Now().UTC()
	rec := &record{s: Session{
		ID: newID("ses"), Project: w.Project, Workdir: w.ID, Kind: KindObserved, Role: RoleAgent, Harness: e.Harness,
		WorkingDir: e.Cwd, State: StateStarting, VendorID: e.VendorID, Restart: RestartPolicy{Mode: RestartNotify},
		Labels: maps.Clone(labels), CreatedAt: now, UpdatedAt: now,
	}}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil, ErrClosed
	}
	// A second call of the same vendor session can come first.
	if found := v.findObservedLocked(e); found != nil {
		v.mu.Unlock()
		return found, nil
	}
	v.sessions[rec.s.ID] = rec
	v.mu.Unlock()
	if err := v.save(ctx, rec); err != nil {
		return nil, err
	}
	v.emit(ctx, rec, Event{Type: EventSessionState, State: StateStarting})
	return rec, nil
}

// labeled gives the newest session that has not exited and has each label.
// Empty labels find none.
func (v *Visor) labeled(labels map[string]string) *record {
	if len(labels) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var out *record
	for _, rec := range v.sessions {
		s := rec.s
		if s.State != StateExited && matchLabels(labels, s.Labels) && (out == nil || s.CreatedAt.After(out.s.CreatedAt)) {
			out = rec
		}
	}
	return out
}

func (v *Visor) findObserved(e hooks.Event) *record {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.findObservedLocked(e)
}

// findObservedLocked gives the newest observed session of the vendor session
// that has not exited. The caller holds v.mu.
func (v *Visor) findObservedLocked(e hooks.Event) *record {
	var out *record
	for _, rec := range v.sessions {
		s := rec.s
		if s.Kind == KindObserved && s.Harness == e.Harness && s.VendorID == e.VendorID && s.State != StateExited &&
			(out == nil || s.CreatedAt.After(out.s.CreatedAt)) {
			out = rec
		}
	}
	return out
}

// applyHook changes the session for one hook event. A structured backend
// reports its own state, so a hook only gives it the vendor id. A hook of
// another harness, for example an agent in a shell session, changes nothing.
func (v *Visor) applyHook(ctx context.Context, rec *record, e hooks.Event) {
	v.mu.Lock()
	s := rec.s
	v.mu.Unlock()
	if s.Harness != e.Harness {
		return
	}
	if s.VendorID == "" || (s.Kind == KindObserved && s.VendorID != e.VendorID) {
		v.update(ctx, rec, func(s *Session) { s.VendorID = e.VendorID })
	}
	observed := s.Kind == KindObserved
	switch {
	case s.State == StateExited, !observed && v.caps(s).Structured, !observed && !s.State.Live():
		return
	case !s.State.Live():
		// An observed session that the last stop marked lost is back.
		if err := v.setState(ctx, rec, StateStarting); err != nil {
			v.log.Debug("ignore a state move", "session_id", s.ID, "err", err)
			return
		}
	}
	to, ok := v.hookEffect(ctx, rec, e, observed)
	if !ok {
		return
	}
	if err := v.setState(ctx, rec, to); err != nil {
		v.log.Debug("ignore a state move", "session_id", s.ID, "err", err)
	}
}

// hookEffect records the requests, prompts, and results of one hook event,
// and gives the state that the event moves the session to.
func (v *Visor) hookEffect(ctx context.Context, rec *record, e hooks.Event, observed bool) (State, bool) {
	switch e.Kind {
	case hooks.KindSessionStart:
		return StateIdle, true
	case hooks.KindPrompt:
		v.closePending(ctx, rec)
		v.emit(ctx, rec, Event{Type: EventUser, Text: e.Prompt})
		return StateRunning, true
	case hooks.KindTool, hooks.KindToolDone:
		v.closePending(ctx, rec)
		return StateRunning, true
	case hooks.KindRequest:
		v.addRequest(ctx, rec, Request{
			ID: cmp.Or(e.ToolUseID, newID("req")), Tool: e.Tool, Kind: string(agentwire.ClassifyTool(e.Tool)),
			Question: e.Question, Options: e.Options, Input: e.InputText(), ToolUseID: e.ToolUseID,
		})
		return StateWaiting, true
	case hooks.KindAttention:
		v.mu.Lock()
		open := len(rec.s.Pending) > 0
		v.mu.Unlock()
		if !open {
			v.addRequest(ctx, rec, Request{ID: newID("req"), Question: e.Message})
		}
		return StateWaiting, true
	case hooks.KindStop:
		v.closePending(ctx, rec)
		v.endTurn(ctx, rec, e)
		return StateIdle, true
	case hooks.KindSessionEnd:
		v.closePending(ctx, rec)
		return StateExited, observed
	}
	return "", false
}

func (v *Visor) addRequest(ctx context.Context, rec *record, r Request) {
	v.update(ctx, rec, func(s *Session) {
		s.Pending = append(slices.DeleteFunc(s.Pending, func(p Request) bool { return p.ID == r.ID }), r)
	})
	v.emit(ctx, rec, Event{Type: EventRequest, Request: &r})
}

// closePending closes the open requests: the user answered them in the
// vendor CLI.
func (v *Visor) closePending(ctx context.Context, rec *record) {
	v.mu.Lock()
	pending := rec.s.Pending
	v.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	v.update(ctx, rec, func(s *Session) { s.Pending = nil })
	for _, r := range pending {
		v.emit(ctx, rec, Event{Type: EventRequestClosed, RequestID: r.ID})
	}
}

func (v *Visor) endTurn(ctx context.Context, rec *record, e hooks.Event) {
	res := Result{Subtype: hookSuccess, Text: e.Message, VendorID: e.VendorID}
	switch {
	case e.Error != "":
		res.Subtype, res.IsError, res.Text = hookError, true, cmp.Or(e.Message, e.Error)
	case e.Interrupted:
		res.Subtype, res.IsError = hookInterrupted, true
	}
	var turn int
	v.update(ctx, rec, func(s *Session) {
		s.Turns++
		turn = s.Turns
	})
	ev := Event{Type: EventResult, Turn: turn, VendorID: e.VendorID, Result: &res}
	if e.Error != "" {
		ev.Error = e.Error
	}
	v.emit(ctx, rec, ev)
}

// replayHooks applies the spooled hook calls at boot.
func (v *Visor) replayHooks(ctx context.Context) {
	apply := func(ctx context.Context, c HookCall) error {
		_, err := v.Hook(ctx, c)
		return err
	}
	if err := NewHookSpool(v.opts.Home).replay(ctx, apply); err != nil {
		v.log.Warn("replay hook calls", "err", err)
	}
}

// values encodes the spec as query values.
func (s HookSpec) values() url.Values {
	q := url.Values{}
	for _, h := range s.Harnesses {
		q.Add("harness", h)
	}
	if s.Auth != "" {
		q.Set("auth", string(s.Auth))
	}
	if s.Dir != "" {
		q.Set("dir", s.Dir)
	}
	return q
}

// parseHookSpec reads the query values of values.
func parseHookSpec(q url.Values) HookSpec {
	return HookSpec{Harnesses: q["harness"], Auth: AuthMode(q.Get("auth")), Dir: q.Get("dir")}
}

func compactJSON(raw json.RawMessage) (json.RawMessage, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
