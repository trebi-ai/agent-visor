package holder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

const wireTurns = `while read -r line; do
  echo '{"type":"init","session_id":"v1"}'
  echo '{"type":"assistant","text":"a"}'
  echo '{"type":"assistant","text":"b"}'
  echo '{"type":"result","subtype":"success","session_id":"v1"}'
done`

func TestWireReplayAfterAckAndIdleEnd(t *testing.T) {
	t.Parallel()
	cfg := wireConfig(t, "w1")
	errc := make(chan error, 1)
	go func() { errc <- Run(t.Context(), cfg) }()
	c := dial(t, cfg.Socket)
	h := c.Hello()
	if h.Protocol != Protocol || h.Mode != ModeWire || h.Run == "" {
		t.Fatalf("hello: %+v", h)
	}
	if err := c.Call(Call{ID: 1, Op: OpSend, Prompt: &agentwire.Prompt{Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	var events []WireEvent
	var out bytes.Buffer
	readUntil(t, c, &out, func(m Msg) bool {
		if m.Type == MsgReply && (m.Reply.ID != 1 || m.Reply.Delivery != agentwire.DeliveryPrompted) {
			t.Errorf("reply: %+v", m.Reply)
		}
		if m.Type == MsgEvent {
			events = append(events, m.Event)
		}
		return m.Type == MsgEvent && m.Event.Event.Type == agentwire.EventResult
	})
	if err := c.Ack(2); err != nil {
		t.Fatal(err)
	}
	_ = c.Close() // a new client takes over

	c = dialFrom(t, cfg.Socket, Cursor{Run: h.Run, Seq: 2})
	defer c.Close()
	var exit Exit
	readUntil(t, c, &out, func(m Msg) bool {
		switch m.Type {
		case MsgEvent:
			if m.Event.Seq <= 2 {
				t.Errorf("event %d again after the cursor", m.Event.Seq)
			}
			if err := c.Ack(m.Event.Seq); err != nil {
				t.Error(err)
			}
		case MsgExit:
			exit = m.Exit
		}
		return m.Type == MsgExit
	})
	if !exit.Idle || exit.Last != uint64(len(events)) {
		t.Fatalf("exit: %+v, %d events", exit, len(events))
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Spool); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a spool with each event acked: %v", err)
	}
	var e Exit
	if data, err := os.ReadFile(cfg.ExitFile); err != nil || json.Unmarshal(data, &e) != nil || !e.Idle {
		t.Fatalf("exit file: %v %+v", err, e)
	}
}

func TestWireSpoolKeepsUnackedEvents(t *testing.T) {
	t.Parallel()
	cfg := wireConfig(t, "w2")
	errc := make(chan error, 1)
	go func() { errc <- Run(t.Context(), cfg) }()
	c := dial(t, cfg.Socket)
	run := c.Hello().Run
	if err := c.Call(Call{ID: 1, Op: OpSend, Prompt: &agentwire.Prompt{Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	readUntil(t, c, &out, func(m Msg) bool { return m.Type == MsgEvent && m.Event.Seq == 1 })
	_ = c.Close() // the client goes away with no ack
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	sp, err := ReadSpool(cfg.Spool)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Run != run || len(sp.Events) < 4 || sp.Events[0].Seq != 1 {
		t.Fatalf("spool: %+v", sp)
	}
	if st, err := os.Stat(cfg.Spool); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("spool mode: %v %v", st, err)
	}
}

// lateSend returns from a prompt after the vendor process ended.
type lateSend struct{ agentwire.FakeDriver }

func (d lateSend) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	s, err := d.FakeDriver.Start(ctx, req)
	return lateSession{s}, err
}

type lateSession struct{ agentwire.Session }

func (s lateSession) Prompt(ctx context.Context, p agentwire.Prompt) error {
	err := s.Session.Prompt(ctx, p)
	time.Sleep(300 * time.Millisecond)
	return err
}

// TestWireReplyBeforeExit checks that a send gets its reply when the vendor
// process ends before the send returns.
func TestWireReplyBeforeExit(t *testing.T) {
	t.Parallel()
	cfg := wireConfig(t, "w3")
	cfg.Wire.Drivers = map[string]agentwire.Driver{"fake": lateSend{agentwire.FakeDriver{Script: agentwire.DefaultFakeScript}}}
	errc := make(chan error, 1)
	go func() { errc <- Run(t.Context(), cfg) }()
	c := dial(t, cfg.Socket)
	defer c.Close()
	if err := c.Call(Call{ID: 1, Op: OpSend, Prompt: &agentwire.Prompt{Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	var reply *Reply
	var out bytes.Buffer
	readUntil(t, c, &out, func(m Msg) bool {
		switch m.Type {
		case MsgEvent:
			if err := c.Ack(m.Event.Seq); err != nil {
				t.Error(err)
			}
		case MsgReply:
			reply = &m.Reply
		}
		return m.Type == MsgExit
	})
	if reply == nil || reply.Error != "" || reply.Delivery != agentwire.DeliveryPrompted {
		t.Fatalf("reply before the exit: %+v", reply)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestWaitCallsReturnsOnReply(t *testing.T) {
	t.Parallel()
	s := newCallServer()
	s.calls = 1
	replied := make(chan time.Time, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		finishCall(s)
		replied <- time.Now()
	}()
	s.waitCalls()
	done := time.Now()
	if late := done.Sub(<-replied); late > 25*time.Millisecond {
		t.Fatalf("waitCalls returned %s after the reply", late)
	}
}

func TestWaitCallsDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for ackWait")
	}
	t.Parallel()
	s := newCallServer()
	s.calls = 1
	start := time.Now()
	s.waitCalls()
	if d := time.Since(start); d < ackWait || d > ackWait+time.Second {
		t.Fatalf("waitCalls took %s, want about %s", d, ackWait)
	}
}

func TestWireConfigJSON(t *testing.T) {
	t.Parallel()
	in := WireConfig{
		StartRequest: agentwire.StartRequest{
			Harness: "claude", Binary: "/bin/claude", WorkingDir: "/w", Env: map[string]string{"A": "b"}, Home: "/h",
			Model: "m", Effort: "high", Permissions: agentwire.PermissionPolicy{},
			MCPServers: []agentwire.MCPServer{{Name: "s"}}, Skills: []agentwire.Skill{{Name: "k"}}, Instructions: "i",
			ExtraArgs: []string{"-x"}, Raw: map[string]any{"r": "v"},
			LogPath: "/f", FragmentLogPath: "/g", LogFragments: true,
		},
		Resume: true, VendorID: "v-1", Ledger: "/l", ClientName: "c", ClientVersion: "1",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want, err := os.ReadFile("testdata/wire-config.keys")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(keys, "\n") + "\n"; got != string(want) {
		t.Fatalf("keys:\n%s\nwant:\n%s", got, want)
	}
	var out WireConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip:\n%+v\n%+v", out, in)
	}
}

// recordDriver keeps the call and the request of the last open.
type recordDriver struct {
	agentwire.Driver
	call string
	req  agentwire.StartRequest
}

func (d *recordDriver) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	d.call, d.req = "start", req
	return d.Driver.Start(ctx, req)
}

func (d *recordDriver) Resume(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	d.call, d.req = "resume", req
	return d.Driver.Resume(ctx, req)
}

func TestWireResumeSetsSessionID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		resume   bool
		call, id string
	}{{true, "resume", "v-1"}, {false, "start", ""}} {
		d := &recordDriver{Driver: agentwire.FakeDriver{Script: wireTurns}}
		rt := agentwire.New(agentwire.Options{})
		rt.SetDriver("fake", d)
		s := &wireServer{rt: rt, cfg: Config{Wire: &WireConfig{
			StartRequest: agentwire.StartRequest{Harness: "fake", WorkingDir: t.TempDir()}, Resume: tc.resume, VendorID: "v-1",
		}}}
		sess, err := s.open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_ = sess.Close(t.Context())
		if d.call != tc.call || d.req.SessionID != tc.id {
			t.Fatalf("resume=%v: call %q, session id %q", tc.resume, d.call, d.req.SessionID)
		}
	}
}

func dialFrom(t *testing.T, path string, from Cursor) *Client {
	t.Helper()
	c, err := Dial(t.Context(), path, from)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wireConfig(t *testing.T, id string) Config {
	t.Helper()
	dir := shortDir(t)
	return Config{
		ID: id, Mode: ModeWire, Socket: filepath.Join(dir, "w.sock"), ExitFile: filepath.Join(dir, "w.exit"), Spool: filepath.Join(dir, "w.spool"),
		Wire: &WireConfig{StartRequest: agentwire.StartRequest{Harness: "fake", WorkingDir: dir}, Drivers: map[string]agentwire.Driver{"fake": agentwire.FakeDriver{Script: wireTurns}}},
	}
}

func newCallServer() *wireServer {
	return &wireServer{acceptor: acceptor{log: slog.New(slog.DiscardHandler)}, callDone: make(chan struct{}, 1)}
}

func finishCall(s *wireServer) {
	s.mu.Lock()
	s.calls--
	s.mu.Unlock()
	select {
	case s.callDone <- struct{}{}:
	default:
	}
}
