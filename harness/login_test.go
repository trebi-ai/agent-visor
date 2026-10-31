package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

// fakeTerm runs a login with pipes and gives its output as the screen.
type fakeTerm struct {
	mu    sync.Mutex
	n     int
	procs map[string]*fakeProc
}

type fakeProc struct {
	out   bytes.Buffer
	stdin io.WriteCloser
	cmd   *exec.Cmd
	done  chan struct{}
}

func (f *fakeTerm) Start(ctx context.Context, _ string, argv []string, dir string, env map[string]string) (string, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	p := &fakeProc{cmd: cmd, done: make(chan struct{})}
	w := &lockedWriter{mu: &f.mu, w: &p.out}
	cmd.Stdout, cmd.Stderr = w, w
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	p.stdin = in
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() {
		_ = cmd.Wait() // the end shows in done
		close(p.done)
	}()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("ses_%d", f.n)
	if f.procs == nil {
		f.procs = map[string]*fakeProc{}
	}
	f.procs[id] = p
	return id, nil
}

func (f *fakeTerm) Screen(_ context.Context, id string) (string, bool, error) {
	p, err := f.proc(id)
	if err != nil {
		return "", false, err
	}
	live := true
	select {
	case <-p.done:
		live = false
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return p.out.String(), live, nil
}

func (f *fakeTerm) Send(_ context.Context, id, text string) error {
	p, err := f.proc(id)
	if err != nil {
		return err
	}
	// A terminal takes a return as the end of a line; a pipe needs a newline.
	_, err = io.WriteString(p.stdin, strings.ReplaceAll(text, "\r", "\n"))
	return err
}

func (f *fakeTerm) Stop(_ context.Context, id string) error {
	p, err := f.proc(id)
	if err != nil {
		return err
	}
	_ = p.cmd.Process.Kill() // it may have ended
	<-p.done
	return nil
}

func (f *fakeTerm) proc(id string) (*fakeProc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[id]
	if !ok {
		return nil, errors.New("no session")
	}
	return p, nil
}

func TestTerminalLoginCode(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	r.m.o.Terminal = &fakeTerm{}
	r.m.o.Facts.LoginCommand = func(agentwire.Harness, agentwire.LoginOptions) ([]string, error) {
		return []string{"fakeh", "auth", "paste"}, nil
	}
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.SetAuthMode(ctx, "fakeh", AuthSeparate); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.LoginCode(ctx, "fakeh", "XYZ"); !errors.Is(err, ErrState) {
		t.Fatalf("code with no login: %v", err)
	}
	l, err := r.m.Login(ctx, "fakeh", LoginSpec{})
	if err != nil || l.Session == "" || !l.AwaitCode || l.Phase != LoginCode || l.Screen == "" {
		t.Fatalf("login: %+v %v", l, err)
	}
	if _, err := r.m.LoginCode(ctx, "fakeh", "XYZ"); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, r, "fakeh", LoginDone)
	if h, _ := r.m.Harness(ctx, "fakeh"); h.Auth != AuthOK || h.Login == nil {
		t.Fatalf("after the login: %+v", h)
	}
}

func TestTerminalLoginFailsAndCancels(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	r.m.o.Terminal = &fakeTerm{}
	r.m.o.Facts.LoginCommand = func(agentwire.Harness, agentwire.LoginOptions) ([]string, error) {
		return []string{"fakeh", "auth", "paste"}, nil
	}
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.SetAuthMode(ctx, "fakeh", AuthSeparate); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Login(ctx, "fakeh", LoginSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.LoginCode(ctx, "fakeh", "WRONG"); err != nil {
		t.Fatal(err)
	}
	if l := waitLogin(t, r, "fakeh", LoginFailed); l.Error == "" {
		t.Fatalf("no error: %+v", l)
	}
	if _, err := r.m.Login(ctx, "fakeh", LoginSpec{}); err != nil {
		t.Fatal(err)
	}
	r.m.CancelLogin(ctx, "fakeh")
	if l := waitLogin(t, r, "fakeh", LoginFailed); l.Error != "the login was cancelled" {
		t.Fatalf("cancel: %+v", l)
	}
}

func TestTokenLogin(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	r.m.o.Terminal = &fakeTerm{}
	r.m.o.Facts.LoginCommand = func(agentwire.Harness, agentwire.LoginOptions) ([]string, error) {
		return []string{"fakeh", "auth", "token"}, nil
	}
	if _, err := r.m.Install(ctx, "claude", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Login(ctx, "claude", LoginSpec{}); err != nil {
		t.Fatal(err)
	}
	l := waitLogin(t, r, "claude", LoginDone)
	if bytes.Contains([]byte(fmt.Sprintf("%+v", l)), []byte("sk-ant")) {
		t.Fatalf("the login state has the token: %+v", l)
	}
	h, err := r.m.Harness(ctx, "claude")
	if err != nil || h.Auth != AuthOK || len(h.EnvKeys) != 1 || h.EnvKeys[0] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("token login: %+v %v", h, err)
	}
	if err := r.m.Logout(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	if h, _ = r.m.Harness(ctx, "claude"); len(h.EnvKeys) != 0 {
		t.Fatalf("logout keeps the token: %v", h.EnvKeys)
	}
	if _, err := r.m.LoginToken(ctx, "fakeh", "k"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("token for fakeh: %v", err)
	}
	if h, err = r.m.LoginToken(ctx, "claude", " sk-x "); err != nil || h.Auth != AuthOK {
		t.Fatalf("token: %+v %v", h, err)
	}
}

func waitLogin(t *testing.T, r *rig, name string, phase LoginPhase) Login {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if l, ok := r.m.LoginState(name); ok && l.Phase == phase {
			return l
		}
		if time.Now().After(deadline) {
			l, _ := r.m.LoginState(name)
			t.Fatalf("login of %s not %s: %+v", name, phase, l)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
