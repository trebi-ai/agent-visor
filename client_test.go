package agentvisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

func TestClientAttachChat(t *testing.T) {
	t.Parallel()
	sock := shortSocket(t)
	ts := serveTest(t, newTestVisor(t, shortHome(t), scriptTurns, nil), sock)
	defer ts.stop(t)
	c := dialTest(t, sock)
	defer c.Close(t.Context())
	s, err := c.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach(t.Context(), "nope", AttachOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attach to an unknown session: %v", err)
	}
	if _, err := c.Attach(t.Context(), s.ID, AttachOptions{View: ViewTerminal}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("terminal of a wire session: %v", err)
	}
	a, err := c.Attach(t.Context(), s.ID, AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.View() != ViewChat {
		t.Fatalf("view %s", a.View())
	}
	if f := recvFrame(t, a); f.Type != FrameAttached || f.Source != KindWire {
		t.Fatalf("attached: %+v", f)
	}
	if turns := recvTurns(t, a, 1); !hasTurn(turns, TurnResult, "done") {
		t.Fatalf("turns: %+v", turns)
	}
	if err := a.Handle(t.Context(), ClientFrame{Type: ClientControl, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	waitState(t, c, s.ID, StateIdle)
	if err := a.Handle(t.Context(), ClientFrame{Type: ClientSend, Text: "more"}); err != nil {
		t.Fatal(err)
	}
	var sawError bool
	var turns []Turn
	for countTurns(turns, TurnResult) < 1 {
		f := recvFrame(t, a)
		sawError = sawError || (f.Type == FrameError && f.Code == "unsupported")
		turns = append(turns, f.Turns...)
	}
	if !sawError {
		t.Fatal("no error frame for control on the chat")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Recv(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("recv after close: %v", err)
	}
}

func TestClientAttachTerminal(t *testing.T) {
	t.Parallel()
	sock := shortSocket(t)
	ts := serveTest(t, newPTYVisor(t, shortHome(t), nil), sock)
	defer ts.stop(t)
	c := dialTest(t, sock)
	defer c.Close(t.Context())
	s, err := c.Start(t.Context(), StartSpec{Command: []string{"sh", "-c", fakeTUI}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, c, s.ID, StateRunning)
	a, err := c.Attach(t.Context(), s.ID, AttachOptions{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	tc := &termClient{t: t, a: a}
	if f := tc.waitFrame(func(f Frame) bool { return f.Type == FrameAttached }); f.View != ViewTerminal || !f.Resizable {
		t.Fatalf("attached: %+v", f)
	}
	tc.waitFrame(func(f Frame) bool { return f.Type == FrameLease && f.Holder == LeaseYou })
	tc.waitOutput("tui ready")
	tc.input("size\r")
	tc.waitOutput("30 100")
	tc.input("exit\r")
	tc.waitFrame(func(f Frame) bool { return f.Type == FrameEnded && f.Reason == EndExited })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := a.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("recv after the end: %v", err)
	}
}

func TestClientHarnesses(t *testing.T) {
	t.Parallel()
	const secret = "harness-secret-42"
	v, err := New(t.Context(), Options{
		Home: shortHome(t),
		Harness: HarnessOptions{
			Names: []string{"fakeh"},
			Path:  t.TempDir(),
			Node:  HarnessNodeOptions{Off: true},
			Facts: HarnessFacts{
				Recipe: func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
					return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallNPM, Package: "fake-pkg", Bin: "fakeh"}, true
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
	sock := shortSocket(t)
	ts := serveTest(t, v, sock)
	defer ts.stop(t)
	c := dialTest(t, sock)
	defer c.Close(t.Context())
	ctx := t.Context()
	if err := c.SetHarnessEnv(ctx, "fakeh", map[string]string{"KEY": secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetHarnessAuth(ctx, "fakeh", AuthSeparate); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Harness(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown harness: %v", err)
	}
	if _, err := c.InstallHarness(ctx, "fakeh", "1.0.0"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("install with no npm: %v", err)
	}
	all, err := c.Harnesses(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("harnesses: %+v %v", all, err)
	}
	data, err := json.Marshal(all[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || all[0].AuthMode != AuthSeparate || len(all[0].EnvKeys) != 1 {
		t.Fatalf("harness: %s", data)
	}
}

func TestClientDownloadError(t *testing.T) {
	t.Parallel()
	// Each request gets a 503, so the test never reaches a vendor site.
	down := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable",
			Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	})}
	v, err := New(t.Context(), Options{
		Home:    shortHome(t),
		Harness: HarnessOptions{Names: []string{"claude"}, HTTPClient: down, Node: HarnessNodeOptions{Off: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := shortSocket(t)
	ts := serveTest(t, v, sock)
	defer ts.stop(t)
	c := dialTest(t, sock)
	defer c.Close(t.Context())
	if _, err := c.InstallHarness(t.Context(), "claude", "1.0.0"); !errors.Is(err, ErrDownload) {
		t.Fatalf("install while the vendor is down: %v", err)
	}
	// The wire code is "download" with status 502.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://visor/v1/harnesses/claude/install", strings.NewReader(`{"version":"1.0.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // test cleanup
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || body.Error.Code != "download" {
		t.Fatalf("response: %d %+v", resp.StatusCode, body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
