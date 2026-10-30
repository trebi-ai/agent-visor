package holder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
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

// TestDialOldHolder checks that a client speaks to a holder of protocol 1.
func TestDialOldHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortDir(t), "old.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan Hello, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := json.Marshal(Hello{Protocol: 1, ID: "old"}) //nolint:errchkjson // a plain struct
		_ = writeMsg(conn, MsgHello, data)                     // the client checks the result
		_, p, _ := readMsg(conn)                               //nolint:errcheck // the hello check covers it
		var h Hello
		_ = json.Unmarshal(p, &h) // the hello check covers it
		got <- h
	}()
	c := dial(t, path)
	defer c.Close()
	if h := <-got; h.Protocol != 1 {
		t.Fatalf("the client answers protocol %d", h.Protocol)
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
		Wire: &WireConfig{Harness: "fake", WorkingDir: dir, Drivers: map[string]agentwire.Driver{"fake": agentwire.FakeDriver{Script: wireTurns}}},
	}
}
