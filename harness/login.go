package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/tui"
)

// LoginPhase is the step of a login.
type LoginPhase string

// Login phases.
const (
	LoginStarting  LoginPhase = "starting"
	LoginBrowser   LoginPhase = "awaiting_browser"
	LoginCode      LoginPhase = "awaiting_code"
	LoginVerifying LoginPhase = "verifying"
	LoginDone      LoginPhase = "done"
	LoginFailed    LoginPhase = "failed"
)

// Login is the state of a vendor login. A token that the vendor prints
// never goes in it.
type Login struct {
	Harness string     `json:"harness"`
	Phase   LoginPhase `json:"phase"`
	// URL is the address that the user opens.
	URL string `json:"url,omitempty"`
	// Code is the device code that the user types on that page.
	Code string `json:"code,omitempty"`
	// AwaitCode means that the vendor asks for a pasted code. A terminal
	// login takes it with LoginCode; a pipe login cannot.
	AwaitCode bool `json:"await_code,omitempty"`
	// InApp means that the harness logs in only inside its own terminal
	// view. No login process runs.
	InApp bool   `json:"in_app,omitempty"`
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
	// Session is the terminal session of the login, when the visor runs
	// it in a terminal. A client can attach to it.
	Session string `json:"session,omitempty"`
	// Screen is the end of the login screen.
	Screen string `json:"screen,omitempty"`
	// Done means that the login process ended.
	Done      bool      `json:"done,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
}

// LoginSpec asks for a login.
type LoginSpec struct {
	// Provider is the model provider of a harness with many providers.
	Provider string `json:"provider,omitempty"`
}

// Terminal runs a login in a terminal session. The visor sets it.
type Terminal interface {
	Start(ctx context.Context, name string, argv []string, dir string, env map[string]string) (string, error)
	// Screen gives the screen text and whether the session still runs.
	Screen(ctx context.Context, id string) (string, bool, error)
	Send(ctx context.Context, id, text string) error
	Stop(ctx context.Context, id string) error
}

// login is one login process: a terminal session, or a process that
// writes into a pipe.
type login struct {
	state  Login
	out    []byte
	ready  chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

// loginOutput takes the output of a pipe login.
type loginOutput struct {
	m    *Manager
	name string
	l    *login
}

const (
	loginOutputMax = 64 << 10
	loginScreenMax = 2000
	logoutTimeout  = 30 * time.Second
	loginKeep      = time.Minute
	loginTick      = 300 * time.Millisecond
	verifyTimeout  = 30 * time.Second
)

// Login starts the vendor login of the harness, in the login home of its
// auth mode. It returns when the vendor shows a URL or a code, when the
// process ends, or after Options.LoginWait. The process runs on until the
// user ends the login, or until Options.LoginTimeout. A second call while a
// login runs gives the same login. At the end, the visor checks the login.
func (m *Manager) Login(ctx context.Context, name string, spec LoginSpec) (Login, error) {
	if !m.known(name) {
		return Login{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	h := agentwire.Harness(name)
	st, err := m.state(ctx, name)
	if err != nil {
		return Login{}, err
	}
	argv, err := m.o.Facts.LoginCommand(h, agentwire.LoginOptions{Provider: spec.Provider, Separate: st.mode() == AuthSeparate})
	if err != nil {
		return Login{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if len(argv) < 2 {
		return Login{Harness: name, Phase: LoginFailed, InApp: true}, nil
	}
	m.mu.Lock()
	l, ok := m.logins[name]
	m.mu.Unlock()
	if !ok {
		if l, err = m.startLogin(ctx, name, argv, st); err != nil {
			return Login{}, err
		}
	}
	wait := time.NewTimer(m.o.LoginWait)
	defer wait.Stop()
	select {
	case <-l.ready:
	case <-l.done:
	case <-wait.C:
	case <-ctx.Done():
		return Login{}, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return l.state, nil
}

// LoginState gives the login that runs, or the last one for a minute after
// its end.
func (m *Manager) LoginState(name string) (Login, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loginLocked(name)
}

func (m *Manager) loginLocked(name string) (Login, bool) {
	if l := m.logins[name]; l != nil {
		return l.state, true
	}
	if e, ok := m.ended[name]; ok && time.Since(e.EndedAt) < loginKeep {
		return e, true
	}
	return Login{}, false
}

// LoginCode types a pasted code into a terminal login.
func (m *Manager) LoginCode(ctx context.Context, name, code string) (Login, error) {
	code = strings.TrimSpace(code)
	if code == "" || strings.ContainsAny(code, "\r\n") {
		return Login{}, fmt.Errorf("%w: the code is empty or has a line break", ErrInvalid)
	}
	m.mu.Lock()
	l := m.logins[name]
	m.mu.Unlock()
	switch {
	case l == nil:
		return Login{}, fmt.Errorf("%w: no login of %s runs", ErrState, name)
	case l.state.Session == "":
		return Login{}, fmt.Errorf("%w: the login of %s takes no code", ErrUnsupported, name)
	}
	if err := m.o.Terminal.Send(ctx, l.state.Session, code+"\r"); err != nil {
		return Login{}, fmt.Errorf("send the code to %s: %w", name, err)
	}
	m.mu.Lock()
	l.state.Phase, l.state.AwaitCode = LoginVerifying, false
	out := l.state
	m.mu.Unlock()
	m.changed(name)
	return out, nil
}

// LoginToken keeps a token or an API key as the login of the harness. It
// goes into the harness env.
func (m *Manager) LoginToken(ctx context.Context, name, token string) (Harness, error) {
	if !m.known(name) {
		return Harness{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return Harness{}, fmt.Errorf("%w: the token is empty", ErrInvalid)
	}
	if err := m.storeToken(ctx, name, token); err != nil {
		return Harness{}, err
	}
	return m.Harness(ctx, name)
}

// CancelLogin stops a running login.
func (m *Manager) CancelLogin(ctx context.Context, name string) {
	m.stopLogin(ctx, name)
}

// Logout runs the vendor logout in the login home of the auth mode. With
// no vendor logout, a separate login removes its credential files. A token
// in the harness env goes too.
func (m *Manager) Logout(ctx context.Context, name string) error {
	if !m.known(name) {
		return fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	m.stopLogin(ctx, name)
	h := agentwire.Harness(name)
	st, err := m.state(ctx, name)
	if err != nil {
		return err
	}
	dropped, err := m.dropToken(name)
	if err != nil {
		return err
	}
	home := ""
	if st.mode() == AuthSeparate {
		home = m.loginHome(name)
	}
	argv := m.o.Facts.LogoutCommand(h)
	bin := ""
	if len(argv) > 0 {
		bin, err = m.binary(ctx, name)
		if err != nil {
			return err
		}
	}
	switch {
	case bin != "":
		argv[0] = bin
		if _, err := m.run(ctx, argv, m.o.Facts.LoginEnv(h, home), logoutTimeout); err != nil {
			return fmt.Errorf("log out of %s: %w", name, err)
		}
	case home != "":
		for _, f := range m.o.Facts.CredentialFiles(h) {
			if err := os.Remove(filepath.Join(home, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("log out of %s: %w", name, err)
			}
		}
	case !dropped:
		return fmt.Errorf("%w: %s has no logout", ErrUnsupported, name)
	}
	m.log.Info("log out of harness", "harness", name)
	m.changed(name)
	return nil
}

// startLogin starts the login process and its watcher.
func (m *Manager) startLogin(ctx context.Context, name string, argv []string, st state) (*login, error) {
	h := agentwire.Harness(name)
	bin, err := m.binary(ctx, name)
	if err != nil {
		return nil, err
	}
	if bin == "" {
		return nil, fmt.Errorf("%w: %s is not installed", ErrState, name)
	}
	home, dir := "", m.home
	if st.mode() == AuthSeparate {
		if _, _, err := m.prepareHome(name, st); err != nil {
			return nil, err
		}
		home = m.loginHome(name)
		dir = m.separateHome(name)
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, fmt.Errorf("log in to %s: %w", name, err)
		}
	}
	env := m.o.Facts.LoginEnv(h, home)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("%w: the manager is closed", ErrState)
	}
	if l, ok := m.logins[name]; ok {
		return l, nil
	}
	lctx, cancel := context.WithTimeout(m.ctx, m.o.LoginTimeout)
	l := &login{state: Login{Harness: name, Phase: LoginStarting, StartedAt: time.Now().UTC()}, ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	argv = append([]string{bin}, argv[1:]...)
	if m.o.Terminal != nil {
		id, err := m.o.Terminal.Start(ctx, name, argv, dir, env)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("log in to %s: %w", name, err)
		}
		l.state.Session = id
		m.logins[name] = l
		m.wg.Go(func() { m.watchTerminal(lctx, name, l) })
	} else {
		cmd := exec.CommandContext(lctx, bin, argv[1:]...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out := &loginOutput{m: m, name: name, l: l}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.WaitDelay = time.Second
		if err := cmd.Start(); err != nil {
			cancel()
			return nil, fmt.Errorf("log in to %s: %w", name, err)
		}
		m.logins[name] = l
		m.wg.Go(func() {
			err := cmd.Wait()
			fail := ""
			if err != nil && m.ctx.Err() == nil {
				fail = "the login did not end well: " + err.Error()
			}
			m.endLogin(name, l, fail)
		})
	}
	m.log.Info("start harness login", "harness", name, "terminal", l.state.Session != "")
	return l, nil
}

// watchTerminal reads the screen of a terminal login until it ends.
func (m *Manager) watchTerminal(ctx context.Context, name string, l *login) {
	t := time.NewTicker(loginTick)
	defer t.Stop()
	id := l.state.Session
	for {
		select {
		case <-ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), logoutTimeout)
			if err := m.o.Terminal.Stop(stopCtx, id); err != nil {
				m.log.Debug("stop the login terminal", "harness", name, "err", err)
			}
			cancel()
			fail := "the login was cancelled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fail = "the login took too long; start it again"
			}
			m.endLogin(name, l, fail)
			return
		case <-t.C:
		}
		screen, live, err := m.o.Terminal.Screen(ctx, id)
		if err != nil && live {
			continue
		}
		if screen != "" {
			m.applyLogin(name, l, []byte(screen))
		}
		m.mu.Lock()
		fail := l.state.Error
		m.mu.Unlock()
		if !live || fail != "" {
			if live {
				if err := m.o.Terminal.Stop(ctx, id); err != nil {
					m.log.Debug("stop the login terminal", "harness", name, "err", err)
				}
			}
			m.endLogin(name, l, fail)
			return
		}
	}
}

// applyLogin reads the login screen into the state. A token on the screen
// goes into the harness env.
func (m *Manager) applyLogin(name string, l *login, screen []byte) {
	got := tui.ParseLogin(name, screen)
	m.mu.Lock()
	before := l.state
	l.state.apply(got, screen)
	changed := before != l.state
	select {
	case <-l.ready:
	default:
		if l.state.URL != "" || l.state.Code != "" || l.state.Error != "" || l.state.AwaitCode {
			close(l.ready)
		}
	}
	m.mu.Unlock()
	if got.Token != "" {
		if err := m.storeToken(m.ctx, name, got.Token); err != nil {
			m.log.Warn("keep the login token", "harness", name, "err", err)
		}
	}
	if changed && m.o.OnChange != nil {
		m.o.OnChange(name)
	}
}

// apply merges a parsed login screen into the state.
func (s *Login) apply(got tui.Login, screen []byte) {
	if got.URL != "" {
		s.URL = got.URL
	}
	if got.UserCode != "" {
		s.Code = got.UserCode
	}
	if s.Phase != LoginVerifying {
		s.AwaitCode = s.AwaitCode || got.AwaitCode
	}
	if got.Error != "" {
		s.Error, s.Hint = got.Error, got.Hint
	}
	switch {
	case s.Phase == LoginVerifying:
	case s.AwaitCode:
		s.Phase = LoginCode
	case s.URL != "" || s.Code != "":
		s.Phase = LoginBrowser
	}
	if s.Session == "" {
		return
	}
	tail := strings.TrimRight(string(screen), " \n")
	if got.Token != "" {
		tail = strings.ReplaceAll(tail, got.Token, "[token]")
	}
	if len(tail) > loginScreenMax {
		tail = tail[len(tail)-loginScreenMax:]
	}
	s.Screen = tail
}

// endLogin checks the login and ends it as done or failed.
func (m *Manager) endLogin(name string, l *login, fail string) {
	if fail == "" {
		m.mu.Lock()
		l.state.Phase = LoginVerifying
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
		if m.freshAuth(ctx, name) != AuthOK {
			fail = "the login did not finish"
		}
		cancel()
	}
	m.mu.Lock()
	l.state.Done, l.state.EndedAt = true, time.Now().UTC()
	l.state.Phase = LoginDone
	if fail != "" {
		l.state.Phase = LoginFailed
		if l.state.Error == "" {
			l.state.Error = fail
		}
	}
	l.out = nil
	m.ended[name] = l.state
	delete(m.logins, name)
	m.mu.Unlock()
	l.cancel()
	close(l.done)
	m.log.Info("end harness login", "harness", name, "ok", fail == "")
	m.changed(name)
}

// stopLogin stops a running login and waits for its end.
func (m *Manager) stopLogin(ctx context.Context, name string) {
	m.mu.Lock()
	l, ok := m.logins[name]
	m.mu.Unlock()
	if !ok {
		return
	}
	l.cancel()
	select {
	case <-l.done:
	case <-ctx.Done():
	}
}

// storeToken writes a token login into the harness env.
func (m *Manager) storeToken(ctx context.Context, name, token string) error {
	key := agentwire.LoginTokenEnv(agentwire.Harness(name))
	if key == "" {
		return fmt.Errorf("%w: %s takes no token", ErrUnsupported, name)
	}
	env, err := m.env(name)
	if err != nil {
		return err
	}
	if env[key] == token {
		return nil
	}
	env[key] = token
	return m.SetEnv(ctx, name, env)
}

// dropToken removes a token login from the harness env.
func (m *Manager) dropToken(name string) (bool, error) {
	key := agentwire.LoginTokenEnv(agentwire.Harness(name))
	env, err := m.env(name)
	if err != nil || key == "" || env[key] == "" {
		return false, err
	}
	delete(env, key)
	return true, m.SetEnv(context.Background(), name, env)
}

// loginHome is the folder that holds a separate login.
func (m *Manager) loginHome(name string) string {
	return filepath.Join(m.separateHome(name), agentwire.LoginDir(agentwire.Harness(name)))
}

// binary gives the program of the harness: the visor copy, else the system
// install, else "".
func (m *Manager) binary(ctx context.Context, name string) (string, error) {
	if _, path, ok := m.current(name); ok {
		return path, nil
	}
	d, err := m.wire.Detect(ctx, agentwire.Harness(name))
	if err != nil {
		return "", fmt.Errorf("detect %s: %w", name, err)
	}
	return d.Path, nil
}

// Write keeps the tail of the output and reads the login screen from it.
func (o *loginOutput) Write(p []byte) (int, error) {
	o.m.mu.Lock()
	o.l.out = append(o.l.out, p...)
	if len(o.l.out) > loginOutputMax {
		o.l.out = o.l.out[len(o.l.out)-loginOutputMax:]
	}
	out := append([]byte(nil), o.l.out...)
	o.m.mu.Unlock()
	o.m.applyLogin(o.name, o.l, out)
	return len(p), nil
}
