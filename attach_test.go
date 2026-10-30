package agentvisor

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAttachChat(t *testing.T) {
	t.Parallel()
	v := newTestVisor(t, shortHome(t), scriptTurns, nil)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := v.Attach(t.Context(), s.ID, AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.View() != ViewChat {
		t.Fatalf("view %s", a.View())
	}
	f := recvFrame(t, a)
	if f.Type != FrameAttached || f.Source != KindWire || !f.Writable {
		t.Fatalf("attached: %+v", f)
	}
	turns := recvTurns(t, a, 1)
	if !hasTurn(turns, TurnAssistant, "working") || !hasTurn(turns, TurnResult, "done") {
		t.Fatalf("turns: %+v", turns)
	}
	if err := a.Handle(t.Context(), ClientFrame{Type: ClientControl, Cols: 80, Rows: 24}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("control on the chat: %v", err)
	}
	if f := recvFrame(t, a); f.Type != FrameError || f.Code != "unsupported" {
		t.Fatalf("error frame: %+v", f)
	}
	waitState(t, v, s.ID, StateIdle)
	if err := a.Handle(t.Context(), ClientFrame{Type: ClientSend, Text: "more"}); err != nil {
		t.Fatal(err)
	}
	turns = recvTurns(t, a, 1)
	if !hasTurn(turns, TurnResult, "done") {
		t.Fatalf("turns of the second send: %+v", turns)
	}
	if _, err := v.Attach(t.Context(), s.ID, AttachOptions{View: ViewTerminal}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("terminal of a wire session: %v", err)
	}

	// A late client gets the same turns from the log, then the end.
	if err := v.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateExited)
	late, err := v.Attach(t.Context(), s.ID, AttachOptions{View: ViewChat})
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	var all []Turn
	for {
		f, err := late.Recv(t.Context())
		if err != nil {
			t.Fatalf("recv: %v (turns %+v)", err, all)
		}
		all = append(all, f.Turns...)
		if f.Type == FrameEnded {
			break
		}
	}
	if n := countTurns(all, TurnResult); n != 2 {
		t.Fatalf("late client has %d results: %+v", n, all)
	}
}

func TestFrameCodec(t *testing.T) {
	t.Parallel()
	after := uint64(9)
	for name, f := range map[string]ClientFrame{
		"attach":  {Type: ClientAttach, View: ViewChat, Cols: 80, Rows: 24, After: &after},
		"ack":     {Type: ClientAck, Seq: 42},
		"control": {Type: ClientControl, Cols: 100, Rows: 30},
		"release": {Type: ClientRelease},
		"send":    {Type: ClientSend, Text: "hi"},
		"input":   {Type: ClientInput, Data: []byte("ls\r")},
	} {
		isBinary, data, err := f.Encode()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := DecodeClientFrame(isBinary, data)
		if err != nil || got.Type != f.Type || got.Seq != f.Seq || got.Text != f.Text || !bytes.Equal(got.Data, f.Data) || got.Options().After != f.Options().After {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	for name, in := range map[string]struct {
		binary bool
		data   []byte
	}{
		"unknown type": {false, []byte(`{"type":"nope"}`)},
		"bad json":     {false, []byte(`{`)},
		"short binary": {true, []byte{3, 0}},
		"output op":    {true, encodeBinary(OpOutput, 1, []byte("x"))},
	} {
		if _, err := DecodeClientFrame(in.binary, in.data); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, f := range []Frame{
		{Op: OpSnapshot, Seq: 0, Data: []byte("screen")},
		{Op: OpOutput, Seq: 1 << 40, Data: []byte("x")},
		{Type: FrameLease, Holder: LeaseOther},
		{Type: FrameTurns, From: 1, To: 3, Turns: []Turn{{Seq: 1, Role: TurnUser, Text: "hi"}}},
	} {
		data, err := f.Encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeFrame(f.Binary(), data)
		if err != nil || got.Op != f.Op || got.Seq != f.Seq || !bytes.Equal(got.Data, f.Data) || got.Type != f.Type || len(got.Turns) != len(f.Turns) {
			t.Errorf("frame %+v: %+v %v", f, got, err)
		}
	}
}

func recvFrame(t *testing.T, a *AttachStream) Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	f, err := a.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	return f
}

// recvTurns reads turns frames until it has n more results.
func recvTurns(t *testing.T, a *AttachStream, n int) []Turn {
	t.Helper()
	var turns []Turn
	for countTurns(turns, TurnResult) < n {
		f := recvFrame(t, a)
		if f.Type != FrameTurns {
			t.Fatalf("frame %+v, want turns", f)
		}
		turns = append(turns, f.Turns...)
	}
	return turns
}

func hasTurn(turns []Turn, role TurnRole, text string) bool {
	for _, t := range turns {
		if t.Role == role && t.Text == text {
			return true
		}
	}
	return false
}

func countTurns(turns []Turn, role TurnRole) int {
	n := 0
	for _, t := range turns {
		if t.Role == role {
			n++
		}
	}
	return n
}
