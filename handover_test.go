package agentvisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// scriptSlowTurns is a fake harness with a turn that takes some time.
const scriptSlowTurns = `while read -r line; do
  echo '{"type":"init","session_id":"v1"}'
  echo '{"type":"assistant","text":"working"}'
  sleep 0.5
  echo '{"type":"result","subtype":"success","text":"done","session_id":"v1"}'
done`

func TestHandoverBothWays(t *testing.T) {
	t.Parallel()
	v := newPTYVisor(t, shortHome(t), resumeTUI)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	wire := waitVendor(t, v, s.ID)
	if wire.Kind != KindWire || wire.State != StateRunning {
		t.Fatalf("wire session: %+v", wire)
	}
	// The handover waits for the end of the turn.
	if err := v.Handover(t.Context(), s.ID, KindPTY); err != nil {
		t.Fatal(err)
	}
	pty, err := v.Session(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pty.Kind != KindPTY || pty.State != StateRunning || pty.PID == wire.PID || pty.VendorID != "v1" || pty.Turns != 1 {
		t.Fatalf("pty session: %+v", pty)
	}
	waitDead(t, wire.PID)
	a := attachTerm(t, v, s.ID, AttachOptions{})
	a.waitOutput("tui resume v1")
	if err := v.Handover(t.Context(), s.ID, KindPTY); err != nil {
		t.Fatalf("handover to the same kind: %v", err)
	}

	errc := make(chan error, 1)
	go func() { errc <- v.Handover(t.Context(), s.ID, KindWire) }()
	select {
	case err := <-errc:
		t.Fatalf("the handover did not wait for the terminal program: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	a.input("exit\r")
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	back, err := v.Session(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Kind != KindWire || back.State != StateSuspended || back.ExitCode != nil {
		t.Fatalf("wire session after the handover: %+v", back)
	}
	waitDead(t, pty.PID)
	if _, err := v.Send(t.Context(), s.ID, Input{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	if got := waitState(t, v, s.ID, StateIdle); got.Turns != 2 || got.VendorID != "v1" {
		t.Fatalf("resumed session: %+v", got)
	}
	checkHandoverOrder(t, v, s.ID)
}

func TestHandoverWithNoResumeCommand(t *testing.T) {
	t.Parallel()
	v := newPTYVisor(t, shortHome(t), nil)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	waitVendor(t, v, s.ID)
	if err := v.Handover(t.Context(), s.ID, KindPTY); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("handover: %v", err)
	}
	if got := waitState(t, v, s.ID, StateIdle); got.Kind != KindWire {
		t.Fatalf("session: %+v", got)
	}
	sh, err := v.Start(t.Context(), StartSpec{Command: []string{"sh", "-c", fakeTUI}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Handover(t.Context(), sh.ID, KindWire); !errors.Is(err, ErrInvalid) {
		t.Fatalf("handover of a shell: %v", err)
	}
}

// resumeTUI continues the vendor session in a fake TUI.
func resumeTUI(l Launch) ([]string, error) {
	if !l.Resume {
		return []string{"sh", "-c", fakeTUI}, nil
	}
	return []string{"sh", "-c", `echo "tui resume $0"; ` + fakeTUI, l.Session.VendorID}, nil
}

func waitVendor(t *testing.T, v *Visor, id string) Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := v.Session(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if s.VendorID != "" {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("no vendor session: %+v", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still lives", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkHandoverOrder checks that the wire turn ends and the wire process
// stops before the terminal program starts, and that the sequence has no gaps.
func checkHandoverOrder(t *testing.T, v *Visor, id string) {
	t.Helper()
	st, err := v.Events(t.Context(), Filter{Session: id}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var states []State
	seen := 0
	last := func(e Event) bool {
		if e.Type == EventResult {
			seen++
		}
		return seen == 2
	}
	for _, e := range collect(t, st, last) {
		switch e.Type {
		case EventResult:
			states = append(states, "result")
		case EventSessionState:
			states = append(states, e.State)
		}
	}
	want := []State{
		StateStarting, StateRunning, "result", StateIdle, StateSuspended, StateStarting, StateRunning,
		StateSuspended, StateStarting, StateRunning, "result",
	}
	if len(states) != len(want) {
		t.Fatalf("states %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states %v, want %v", states, want)
		}
	}
	checkSessionSeq(t, v, id)
}
