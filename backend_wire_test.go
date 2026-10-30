package agentvisor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

// scriptGate stops each turn after the first message, until the file in
// GATE exists.
const scriptGate = `while read -r line; do
  echo '{"type":"init","session_id":"v1"}'
  echo '{"type":"assistant","text":"step1"}'
  while [ ! -f "$GATE" ]; do sleep 0.05; done
  echo '{"type":"assistant","text":"step2"}'
  echo '{"type":"result","subtype":"success","session_id":"v1"}'
done`

func TestWireHolderEndsAtIdle(t *testing.T) {
	t.Parallel()
	home := shortHome(t)
	v := newHeldVisor(t, home, scriptTurns, nil)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.caps(s).Survives {
		t.Fatal("a wire session in a holder does not survive")
	}
	got := waitState(t, v, s.ID, StateSuspended)
	if got.Turns != 1 || got.VendorID != "v1" || got.PID != 0 {
		t.Fatalf("after the first turn: %+v", got)
	}
	first, _ := v.cursor(t.Context(), s.ID) //nolint:errcheck // the run check covers it
	if _, err := os.Stat(filepath.Join(home, "wire", s.ID+".sock")); !os.IsNotExist(err) {
		t.Fatalf("the holder socket stays: %v", err)
	}
	if d, err := v.Send(t.Context(), s.ID, Input{Text: "again"}); err != nil || d != DeliveryPrompted {
		t.Fatalf("send: %q %v", d, err)
	}
	for waitState(t, v, s.ID, StateSuspended).Turns != 2 {
		time.Sleep(10 * time.Millisecond)
	}
	second, _ := v.cursor(t.Context(), s.ID) //nolint:errcheck // the run check covers it
	if first.Run == "" || second.Run == first.Run {
		t.Fatalf("the second turn ran in holder %q, the first in %q", second.Run, first.Run)
	}
	events := sessionEvents(t, v, s.ID)
	if n := countEvents(events, EventUser); n != 2 {
		t.Fatalf("%d user events, want 2", n)
	}
	checkSessionSeq(t, v, s.ID)
}

// TestWireHolderSurvivesVisorKill kills a visor process in a turn. The turn
// ends in the holder, and the next visor stores each event once.
func TestWireHolderSurvivesVisorKill(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"live", "spool"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home := shortHome(t)
			gate := filepath.Join(home, "gate")
			id := runKilledVisor(t, home, gate)
			if mode == "spool" {
				// The holder ends with no visor and keeps the events in its spool.
				touch(t, gate)
				waitFile(t, filepath.Join(home, "wire", id+".exit"))
			}
			v := newHeldVisor(t, home, scriptGate, map[string]string{"GATE": gate})
			defer v.Close(context.Background())
			touch(t, gate)
			got := waitState(t, v, id, StateSuspended)
			if got.Turns != 1 {
				t.Fatalf("turns: %d", got.Turns)
			}
			events := sessionEvents(t, v, id)
			var texts []string
			for _, e := range events {
				if e.Type == EventAssistant {
					texts = append(texts, e.Text)
				}
			}
			if strings.Join(texts, " ") != "step1 step2" || countEvents(events, EventResult) != 1 || countEvents(events, EventInit) != 1 {
				t.Fatalf("assistant events %q, %d results", texts, countEvents(events, EventResult))
			}
			checkSessionSeq(t, v, id)
			for _, f := range []string{".exit", ".spool"} {
				if _, err := os.Stat(filepath.Join(home, "wire", id+f)); !os.IsNotExist(err) {
					t.Fatalf("the holder file %s stays: %v", f, err)
				}
			}
		})
	}
}

func TestWireHolderKeepsRequestAcrossRestart(t *testing.T) {
	t.Parallel()
	home := shortHome(t)
	v := newHeldVisor(t, home, scriptPermission, nil)
	s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateWaiting)
	if err := v.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	v = newHeldVisor(t, home, scriptPermission, nil)
	defer v.Close(context.Background())
	got := waitState(t, v, s.ID, StateWaiting)
	if len(got.Pending) != 1 || got.Pending[0].ID != "p1" || got.PID == 0 {
		t.Fatalf("after the restart: %+v", got)
	}
	if err := v.Answer(t.Context(), s.ID, "p1", Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if got := waitState(t, v, s.ID, StateSuspended); got.Turns != 1 || len(got.Pending) != 0 {
		t.Fatalf("after the answer: %+v", got)
	}
	events := sessionEvents(t, v, s.ID)
	allowed := slices.ContainsFunc(events, func(e Event) bool { return e.Type == EventAssistant && e.Text == "allowed" })
	if countEvents(events, EventRequest) != 1 || !allowed {
		t.Fatalf("events: %+v", events)
	}
	checkSessionSeq(t, v, s.ID)
}

// runChildVisor is the visor process that TestWireHolderSurvivesVisorKill
// kills. It prints the session id when the turn runs.
func runChildVisor() int {
	ctx := context.Background()
	home, gate := os.Getenv("AV_TEST_HOME"), os.Getenv("AV_TEST_GATE")
	v, err := New(ctx, Options{Home: home, Client: "test", Env: map[string]string{agentwire.FakeScriptEnv: scriptGate, "GATE": gate}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	s, err := v.Start(ctx, StartSpec{Harness: "fake", WorkingDir: home, Prompt: "go"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := v.Events(ctx, Filter{Session: s.ID}, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for {
		e, err := st.Next(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if e.Type == EventAssistant {
			break
		}
	}
	fmt.Println(s.ID)
	select {} // the test kills this process
}

func runKilledVisor(t *testing.T, home, gate string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "visor-child")
	cmd.Env = append(os.Environ(), "AV_TEST_HOME="+home, "AV_TEST_GATE="+gate)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill() // the read error is the one to report
		t.Fatalf("read the session id: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // the process got SIGKILL
	return strings.TrimSpace(line)
}

func newHeldVisor(t *testing.T, home, script string, env map[string]string) *Visor {
	t.Helper()
	o := Options{Home: home, Client: "test", Env: map[string]string{agentwire.FakeScriptEnv: script}}
	for k, val := range env {
		o.Env[k] = val
	}
	v, err := New(t.Context(), o)
	if err != nil {
		t.Fatalf("new visor: %v", err)
	}
	return v
}

// sessionEvents reads the stored events of a session.
func sessionEvents(t *testing.T, v *Visor, id string) []Event {
	t.Helper()
	s, err := v.Session(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	st, err := v.Events(t.Context(), Filter{Session: id}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return collect(t, st, func(e Event) bool { return e.Seq >= s.Seq })
}

func countEvents(events []Event, typ EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no file %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
