package agentvisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

const kindCapture Kind = "capture"

// captureBackend keeps the Launch of each start.
type captureBackend struct {
	mu       sync.Mutex
	launches []Launch
	handles  []*captureHandle
	onAnswer func(*captureHandle)
}

type captureHandle struct {
	id       string
	ch       chan Event
	once     sync.Once
	onAnswer func(*captureHandle)
}

func (b *captureBackend) Kind() Kind { return kindCapture }

func (b *captureBackend) Caps(string) Caps { return Caps{} }

func (b *captureBackend) Start(_ context.Context, l Launch) (Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.launches = append(b.launches, l)
	h := &captureHandle{id: l.Session.ID, ch: make(chan Event), onAnswer: b.onAnswer}
	b.handles = append(b.handles, h)
	return h, nil
}

func (b *captureBackend) Recover(context.Context) ([]Handle, error) { return nil, nil }

func (b *captureBackend) Close(context.Context) error { return nil }

func (b *captureBackend) last() Launch {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.launches[len(b.launches)-1]
}

func (h *captureHandle) Session() string                               { return h.id }
func (h *captureHandle) VendorID() string                              { return "" }
func (h *captureHandle) PID() int                                      { return 0 }
func (h *captureHandle) Send(context.Context, Input) (Delivery, error) { return DeliveryPrompted, nil }
func (h *captureHandle) Interrupt(context.Context) error               { return nil }
func (h *captureHandle) Answer(context.Context, string, Answer) error {
	if h.onAnswer != nil {
		h.onAnswer(h)
	}
	return nil
}
func (h *captureHandle) Events() <-chan Event { return h.ch }

func (h *captureHandle) Close(context.Context) error {
	h.once.Do(func() { close(h.ch) })
	return nil
}

func TestHarnessLaunchLayers(t *testing.T) {
	t.Parallel()
	const secret = "harness-secret-42"
	home := t.TempDir()
	b := &captureBackend{}
	v, err := New(t.Context(), Options{
		Home: home, Backends: []Backend{b},
		Env: map[string]string{"A": "visor", "B": "visor", "C": "visor"},
		Harness: HarnessOptions{
			Names: []string{"fakeh"},
			Path:  t.TempDir(),
			Facts: HarnessFacts{
				Recipe: func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
					return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallNPM, Package: "fake-pkg", Bin: "fakeh"}, true
				},
				LoginEnv: func(_ agentwire.Harness, home string) map[string]string {
					return map[string]string{"FAKEH_HOME": home}
				},
				AuthStatus: func(context.Context, agentwire.Harness, string) (agentwire.AuthState, error) {
					return agentwire.AuthMissing, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	ctx := t.Context()
	if err := v.SetHarnessEnv(ctx, "fakeh", map[string]string{"B": "harness", "C": "harness", "KEY": secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.SetHarnessAuth(ctx, "fakeh", AuthSeparate); err != nil {
		t.Fatal(err)
	}
	if err := v.SetHarnessEnv(ctx, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("env of an unknown harness: %v", err)
	}
	s, err := v.Start(ctx, StartSpec{Kind: kindCapture, Harness: "fakeh", WorkingDir: t.TempDir(), Env: map[string]string{"C": "session"}})
	if err != nil {
		t.Fatal(err)
	}
	l := b.last()
	sep := filepath.Join(home, "harness", "fakeh")
	if l.Env["A"] != "visor" || l.Env["B"] != "harness" || l.Env["C"] != "session" || l.Env["KEY"] != secret {
		t.Fatalf("env layers: A=%s B=%s C=%s", l.Env["A"], l.Env["B"], l.Env["C"])
	}
	if l.Home != sep || l.Env["FAKEH_HOME"] != sep || l.Binary != "" {
		t.Fatalf("launch: home %q binary %q", l.Home, l.Binary)
	}
	own, err := v.Start(ctx, StartSpec{Kind: kindCapture, Harness: "fakeh", WorkingDir: t.TempDir(), Binary: "/opt/fakeh"})
	if err != nil {
		t.Fatal(err)
	}
	if l := b.last(); l.Binary != "/opt/fakeh" {
		t.Fatalf("spec binary: %q", l.Binary)
	}
	if err := v.Stop(ctx, own.ID); err != nil {
		t.Fatal(err)
	}
	// A live session blocks a switch of the harness binary.
	if _, err := v.InstallHarness(ctx, "fakeh", "1.0.0"); !errors.Is(err, ErrBusy) {
		t.Fatalf("install under a live session: %v", err)
	}
	if err := v.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.InstallHarness(ctx, "fakeh", "1.0.0"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("install with no npm: %v", err)
	}
	h, err := v.Harness(ctx, "fakeh")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || h.AuthMode != AuthSeparate || len(h.EnvKeys) != 3 {
		t.Fatalf("harness: %s", data)
	}
	st, err := v.Events(ctx, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	changes := 0
	for {
		rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		e, err := st.Next(rctx)
		cancel()
		if err != nil {
			break
		}
		raw, _ := json.Marshal(e) // an event always encodes
		if strings.Contains(string(raw), secret) {
			t.Fatalf("an event has the secret: %s", raw)
		}
		if e.Type == EventHarnessChanged && e.Harness == "fakeh" {
			changes++
		}
	}
	if changes < 2 {
		t.Fatalf("harness.changed events: %d", changes)
	}
}

// TestAnswerAfterTurnEnd checks that a turn that ends before Answer returns stays idle.
func TestAnswerAfterTurnEnd(t *testing.T) {
	t.Parallel()
	b := &captureBackend{}
	v, err := New(t.Context(), Options{Home: t.TempDir(), Backends: []Backend{b}})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Kind: kindCapture, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	h := b.handles[0]
	b.mu.Unlock()
	h.ch <- Event{Type: EventRequest, Request: &Request{ID: "p1"}}
	waitState(t, v, s.ID, StateWaiting)
	h.onAnswer = func(h *captureHandle) {
		h.ch <- Event{Type: EventResult, Result: &Result{Subtype: "success"}}
		waitState(t, v, s.ID, StateIdle)
	}
	if err := v.Answer(t.Context(), s.ID, "p1", Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.Session(t.Context(), s.ID); got.State != StateIdle { //nolint:errcheck // the state check covers it
		t.Fatalf("state after the answer: %s", got.State)
	}
}

func TestHarnessTerminalLogin(t *testing.T) {
	t.Parallel()
	for _, overSocket := range []bool{false, true} {
		t.Run(fmt.Sprintf("socket=%v", overSocket), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			var v api = loginVisor(t, t.TempDir())
			if overSocket {
				sock := shortSocket(t)
				ts := serveTest(t, v.(*Visor), sock)
				v = &served{Client: dialTest(t, sock), ts: ts}
			}
			defer v.Close(context.Background())
			if _, err := v.SetHarnessAuth(ctx, "fakeh", AuthSeparate); err != nil {
				t.Fatal(err)
			}
			if _, err := v.HarnessLogin(ctx, "fakeh"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("no login: %v", err)
			}
			l, err := v.LoginHarness(ctx, "fakeh", LoginSpec{})
			if err != nil {
				t.Fatal(err)
			}
			if l.Session == "" || l.URL != "https://login.example.test/device" {
				t.Fatalf("login: %+v", l)
			}
			s, err := v.Session(ctx, l.Session)
			if err != nil || s.Kind != KindPTY || s.Harness != "" || s.Labels[loginLabel] != "fakeh" {
				t.Fatalf("login session: %+v %v", s, err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for !l.AwaitCode {
				if time.Now().After(deadline) {
					t.Fatalf("no code ask: %+v", l)
				}
				time.Sleep(50 * time.Millisecond)
				if l, err = v.HarnessLogin(ctx, "fakeh"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := v.HarnessLoginCode(ctx, "fakeh", "XYZ"); err != nil {
				t.Fatal(err)
			}
			for l.Phase != LoginDone {
				if time.Now().After(deadline) || l.Phase == LoginFailed {
					t.Fatalf("login end: %+v", l)
				}
				time.Sleep(50 * time.Millisecond)
				if l, err = v.HarnessLogin(ctx, "fakeh"); err != nil {
					t.Fatal(err)
				}
			}
			h, err := v.Harness(ctx, "fakeh")
			if err != nil || h.Auth != AuthOK {
				t.Fatalf("auth after login: %+v %v", h, err)
			}
		})
	}
}

func TestHarnessFilesAPI(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	user := t.TempDir()
	v, err := New(ctx, Options{
		Home: t.TempDir(), Client: "test",
		Harness: HarnessOptions{Names: []string{"claude"}, Path: t.TempDir(), UserHome: user},
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := shortSocket(t)
	ts := serveTest(t, v, sock)
	c := &served{Client: dialTest(t, sock), ts: ts}
	defer c.Close(context.Background())
	if err := os.MkdirAll(filepath.Join(user, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".claude", "settings.local.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := c.HarnessFiles(ctx, "claude")
	if err != nil || len(fs) == 0 {
		t.Fatalf("files: %v %v", fs, err)
	}
	f, err := c.OwnHarnessFile(ctx, "claude", "settings_local", OwnFromUser)
	if err != nil || f.Shared {
		t.Fatalf("own: %+v %v", f, err)
	}
	fc, err := c.ReadHarnessFile(ctx, "claude", "settings_local", SideHome)
	if err != nil || fc.Content != `{"a":1}` {
		t.Fatalf("read: %+v %v", fc, err)
	}
	if _, err := c.WriteHarnessFile(ctx, "claude", "settings_local", `{"a":2}`, "old"); !errors.Is(err, ErrStale) {
		t.Fatalf("stale write: %v", err)
	}
	if _, err := c.WriteHarnessFile(ctx, "claude", "settings_local", `{"a":2}`, fc.ETag); err != nil {
		t.Fatal(err)
	}
	if f, err = c.ShareHarnessFile(ctx, "claude", "settings_local"); err != nil || !f.Shared {
		t.Fatalf("share: %+v %v", f, err)
	}
	if _, err := c.OwnHarnessFile(ctx, "claude", "nope", OwnFromUser); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
}

// loginVisor is a visor with a fake harness whose login runs in a PTY. The
// login asks for a code, and the code XYZ logs it in.
func loginVisor(t *testing.T, home string) *Visor {
	t.Helper()
	bin := filepath.Join(home, "bin", "fakeh", "1.0.0", "bin", "fakeh")
	script := `#!/bin/sh
if [ "$1" = login ]; then
  printf 'Open https://login.example.test/device to log in.\nPaste code here: '
  read c
  [ "$c" = XYZ ] && touch "$FAKEH_HOME/cred"
  exit 0
fi
echo 1.0.0
`
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // a test program
		t.Fatal(err)
	}
	if err := os.Symlink("1.0.0", filepath.Join(home, "bin", "fakeh", "current")); err != nil {
		t.Fatal(err)
	}
	v, err := New(t.Context(), Options{
		Home: home, Client: "test",
		Harness: HarnessOptions{
			Names: []string{"fakeh"}, Path: t.TempDir(), UserHome: t.TempDir(),
			Facts: HarnessFacts{
				Recipe: func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
					return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallNPM, Package: "fake-pkg", Bin: "fakeh"}, true
				},
				LoginCommand: func(agentwire.Harness, agentwire.LoginOptions) ([]string, error) {
					return []string{"fakeh", "login"}, nil
				},
				LoginEnv: func(_ agentwire.Harness, home string) map[string]string {
					return map[string]string{"FAKEH_HOME": home}
				},
				AuthStatus: func(_ context.Context, _ agentwire.Harness, home string) (agentwire.AuthState, error) {
					if _, err := os.Stat(filepath.Join(home, "cred")); err == nil {
						return agentwire.AuthOK, nil
					}
					return agentwire.AuthMissing, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
