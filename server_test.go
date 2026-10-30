package agentvisor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const scriptSlow = `while read -r line; do
  echo '{"type":"init","session_id":"v1"}'
  echo '{"type":"assistant","text":"one"}'
  sleep 0.3
  echo '{"type":"assistant","text":"two"}'
  echo '{"type":"result","subtype":"success","session_id":"v1"}'
done`

// testServer serves one visor on a socket until stop.
type testServer struct {
	v    *Visor
	srv  *http.Server
	done chan error
}

func TestClientDropInTurn(t *testing.T) {
	t.Parallel()
	sock := shortSocket(t)
	ts := serveTest(t, newTestVisor(t, t.TempDir(), scriptSlow, nil), sock)
	defer ts.stop(t)
	c1 := dialTest(t, sock)
	st, err := c1.Events(t.Context(), Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c1.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	seen := collect(t, st, func(e Event) bool { return e.Type == EventAssistant })
	_ = c1.Close(t.Context()) // the client drops in the turn
	if _, err := st.Next(t.Context()); err == nil {
		t.Fatal("the stream gives events after the client closes")
	}
	c2 := dialTest(t, sock)
	defer c2.Close(t.Context())
	waitState(t, c2, s.ID, StateIdle)
	last := seen[len(seen)-1].Seq
	st2, err := c2.Events(t.Context(), Filter{}, last)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	rest := collect(t, st2, func(e Event) bool { return e.Type == EventSessionState && e.State == StateIdle })
	checkSeq(t, rest, last)
}

func TestClientStreamSurvivesVisorRestart(t *testing.T) {
	t.Parallel()
	home, sock := t.TempDir(), shortSocket(t)
	ts := serveTest(t, newTestVisor(t, home, scriptTurns, nil), sock)
	c := dialTest(t, sock)
	defer c.Close(t.Context())
	st, err := c.Events(t.Context(), Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := c.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	first := collect(t, st, func(e Event) bool { return e.Type == EventResult })
	ts.stop(t)
	ts = serveTest(t, newTestVisor(t, home, scriptTurns, nil), sock)
	defer ts.stop(t)
	if _, err := c.Send(t.Context(), s.ID, Input{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	rest := collect(t, st, func(e Event) bool { return e.Type == EventResult })
	checkSeq(t, append(first, rest...), 0)
}

func TestListen(t *testing.T) {
	t.Parallel()
	sock := shortSocket(t)
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(t.Context(), sock)
	if err != nil {
		t.Fatalf("listen over a stale file: %v", err)
	}
	defer ln.Close()
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", st, err)
	}
	if _, err := Listen(t.Context(), sock); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second listen: %v, want ErrBusy", err)
	}
}

func TestDialRefusesOtherProtocol(t *testing.T) {
	t.Parallel()
	sock := shortSocket(t)
	ln, err := Listen(t.Context(), sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"protocol":"agent-visor/2"}`)) // test reply
	})}
	go srv.Serve(ln) //nolint:errcheck // Close ends it
	defer srv.Close()
	if _, err := Dial(t.Context(), sock); !errors.Is(err, ErrProtocol) {
		t.Fatalf("dial: %v, want ErrProtocol", err)
	}
}

func TestFilterQuery(t *testing.T) {
	t.Parallel()
	for _, f := range []Filter{
		{},
		{Session: "s", Project: "p", Workdir: "w", Key: "k", Kind: KindPTY, Role: RoleShell},
		{States: []State{StateIdle, StateRunning}, Labels: map[string]string{"app.run": "r_1", "x": "a=b"}},
	} {
		got, err := parseFilter(f.values())
		if err != nil || !reflect.DeepEqual(got, f) {
			t.Errorf("filter %+v reads back as %+v (%v)", f, got, err)
		}
	}
	if _, err := parseFilter(map[string][]string{"label": {"novalue"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad label: %v", err)
	}
}

func serveTest(t *testing.T, v *Visor, sock string) *testServer {
	t.Helper()
	ln, err := Listen(t.Context(), sock)
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{v: v, srv: &http.Server{Handler: NewServer(v), ReadHeaderTimeout: time.Second}, done: make(chan error, 1)}
	go func() { ts.done <- ts.srv.Serve(ln) }()
	return ts
}

func (ts *testServer) stop(t *testing.T) {
	t.Helper()
	if err := ts.close(context.Background()); err != nil {
		t.Errorf("close visor: %v", err)
	}
}

func (ts *testServer) close(ctx context.Context) error {
	_ = ts.srv.Close() // the visor close error is the one to report
	<-ts.done
	return ts.v.Close(ctx)
}

func dialTest(t *testing.T, sock string) *Client {
	t.Helper()
	c, err := Dial(t.Context(), sock)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// shortSocket gives a socket path under the 104 byte limit of macOS.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "av")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) }) // test cleanup
	return filepath.Join(dir, "v.sock")
}
