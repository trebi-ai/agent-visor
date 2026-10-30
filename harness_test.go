package agentvisor

import (
	"context"
	"encoding/json"
	"errors"
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
}

type captureHandle struct {
	id   string
	ch   chan Event
	once sync.Once
}

func (b *captureBackend) Kind() Kind { return kindCapture }

func (b *captureBackend) Caps(string) Caps { return Caps{} }

func (b *captureBackend) Start(_ context.Context, l Launch) (Handle, error) {
	b.mu.Lock()
	b.launches = append(b.launches, l)
	b.mu.Unlock()
	return &captureHandle{id: l.Session.ID, ch: make(chan Event)}, nil
}

func (b *captureBackend) Recover(context.Context) ([]Handle, error) { return nil, nil }

func (b *captureBackend) Close(context.Context) error { return nil }

func (b *captureBackend) last() Launch {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.launches[len(b.launches)-1]
}

func (h *captureHandle) Session() string                              { return h.id }
func (h *captureHandle) VendorID() string                             { return "" }
func (h *captureHandle) PID() int                                     { return 0 }
func (h *captureHandle) Send(context.Context, Input) error            { return nil }
func (h *captureHandle) Interrupt(context.Context) error              { return nil }
func (h *captureHandle) Answer(context.Context, string, Answer) error { return nil }
func (h *captureHandle) Events() <-chan Event                         { return h.ch }

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
