package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	"github.com/trebi-ai/agent-wire/tui"
)

// Login is the state of a vendor login. A token that the vendor prints
// never goes in it.
type Login struct {
	Harness string `json:"harness"`
	// URL is the address that the user opens.
	URL string `json:"url,omitempty"`
	// Code is the device code that the user types on that page.
	Code string `json:"code,omitempty"`
	// AwaitCode means that the vendor asks for a pasted code. A pipe login
	// cannot send it; a terminal login can.
	AwaitCode bool `json:"await_code,omitempty"`
	// InApp means that the harness logs in only inside its own terminal
	// view. No login process runs.
	InApp bool   `json:"in_app,omitempty"`
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
	// Done means that the login process ended.
	Done      bool      `json:"done,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

// login is one login process. The process writes into a pipe. A terminal
// login can replace the pipe later; it fills the same Login.
type login struct {
	state  Login
	out    []byte
	ready  chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

// loginOutput takes the output of a login process.
type loginOutput struct {
	m    *Manager
	name string
	l    *login
}

const (
	loginOutputMax = 64 << 10
	logoutTimeout  = 30 * time.Second
)

// Login starts the vendor login of the harness, in the login home of its
// auth mode. It returns when the vendor shows a URL or a code, when the
// process ends, or after Options.LoginWait. The process runs on until the
// user ends the login, or until Options.LoginTimeout. A second call while a
// login runs gives the same login.
func (m *Manager) Login(ctx context.Context, name string) (Login, error) {
	if !m.known(name) {
		return Login{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	h := agentwire.Harness(name)
	argv := m.o.Facts.LoginCommand(h)
	if len(argv) < 2 {
		return Login{Harness: name, InApp: true}, nil
	}
	m.mu.Lock()
	l, ok := m.logins[name]
	m.mu.Unlock()
	if !ok {
		var err error
		if l, err = m.startLogin(ctx, name, argv); err != nil {
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

// Logout runs the vendor logout in the login home of the auth mode. With
// no vendor logout, a separate login removes its credential files.
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
	home := ""
	if st.mode() == AuthSeparate {
		home = m.separateHome(name)
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
	default:
		return fmt.Errorf("%w: %s has no logout", ErrUnsupported, name)
	}
	m.log.Info("log out of harness", "harness", name)
	m.changed(name)
	return nil
}

// startLogin starts the login process and its watcher.
func (m *Manager) startLogin(ctx context.Context, name string, argv []string) (*login, error) {
	h := agentwire.Harness(name)
	bin, err := m.binary(ctx, name)
	if err != nil {
		return nil, err
	}
	if bin == "" {
		return nil, fmt.Errorf("%w: %s is not installed", ErrState, name)
	}
	st, err := m.state(ctx, name)
	if err != nil {
		return nil, err
	}
	home, dir := "", m.home
	if st.mode() == AuthSeparate {
		home = m.separateHome(name)
		dir = home
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, fmt.Errorf("log in to %s: %w", name, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("%w: the manager is closed", ErrState)
	}
	if l, ok := m.logins[name]; ok {
		return l, nil
	}
	lctx, cancel := context.WithTimeout(m.ctx, m.o.LoginTimeout)
	l := &login{state: Login{Harness: name, StartedAt: time.Now().UTC()}, ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	cmd := exec.CommandContext(lctx, bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range m.o.Facts.LoginEnv(h, home) {
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
	m.wg.Go(func() { m.waitLogin(name, l, cmd) })
	m.log.Info("start harness login", "harness", name)
	return l, nil
}

// waitLogin waits for the login process and ends the login.
func (m *Manager) waitLogin(name string, l *login, cmd *exec.Cmd) {
	err := cmd.Wait()
	m.mu.Lock()
	l.state.Done = true
	if err != nil && l.state.Error == "" && m.ctx.Err() == nil {
		l.state.Error = "the login did not end well: " + err.Error()
	}
	l.out = nil
	delete(m.logins, name)
	m.mu.Unlock()
	l.cancel()
	close(l.done)
	m.log.Info("end harness login", "harness", name, "ok", err == nil)
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
	m, l := o.m, o.l
	m.mu.Lock()
	l.out = append(l.out, p...)
	if len(l.out) > loginOutputMax {
		l.out = l.out[len(l.out)-loginOutputMax:]
	}
	got := tui.ParseLogin(o.name, l.out)
	before := l.state
	if got.URL != "" {
		l.state.URL = got.URL
	}
	if got.UserCode != "" {
		l.state.Code = got.UserCode
	}
	l.state.AwaitCode = l.state.AwaitCode || got.AwaitCode
	if got.Error != "" {
		l.state.Error, l.state.Hint = got.Error, got.Hint
	}
	changed := before != l.state
	select {
	case <-l.ready:
	default:
		if l.state.URL != "" || l.state.Code != "" || l.state.Error != "" || l.state.AwaitCode {
			close(l.ready)
		}
	}
	m.mu.Unlock()
	if changed && m.o.OnChange != nil {
		m.o.OnChange(o.name)
	}
	return len(p), nil
}
