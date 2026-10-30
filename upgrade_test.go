package agentvisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

func TestNewerBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.10.0", "v1.9.0", true},
		{"v1.1.0", "v1.2.0", false},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.2.0-rc.1", true},
		{"v1.2.0-rc.1", "v1.2.0", false},
		{"v1.2.0-rc.2", "v1.2.0-rc.1", true},
		{"v1.2.0+abc", "v1.2.0", false},
		{"0123456789ab", "ba9876543210", true},
		{"v1.2.0", "dev", true},
		{"", "v1.0.0", false},
	} {
		if got := newerBuild(tc.a, tc.b); got != tc.want {
			t.Errorf("newerBuild(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestHelloSchedulesUpgrade(t *testing.T) {
	t.Parallel()
	u := newUpgrader(t, scriptHang, "v1.2.0")
	defer u.v.Close(context.Background())
	if _, err := u.v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang"}); err != nil {
		t.Fatal(err)
	}
	exe := os.Args[0]
	info, err := u.v.Hello(t.Context(), Hello{Client: "p", Build: "v1.1.0", Protocol: Protocol, Binary: exe})
	if err != nil || info.Upgrade != "" {
		t.Fatalf("an older build: %+v %v", info, err)
	}
	info, err = u.v.Hello(t.Context(), Hello{Client: "p", Build: "v1.3.0", Protocol: Protocol, Binary: exe})
	if err != nil || info.Upgrade != "v1.3.0" {
		t.Fatalf("a newer build: %+v %v", info, err)
	}
	if got := u.deadline(); !got.Equal(u.clock.Now().Add(defaultUpgradeWait)) {
		t.Fatalf("deadline %v", got)
	}
	if _, err := u.v.Hello(t.Context(), Hello{Client: "p", Build: "v2.0.0", Protocol: "agent-visor/2", Binary: exe}); err != nil {
		t.Fatal(err)
	}
	if got := u.deadline(); !got.Equal(u.clock.Now().Add(majorUpgradeWait)) {
		t.Fatalf("deadline with a new protocol major %v", got)
	}
	if _, err := u.v.Upgrade(t.Context(), UpgradeSpec{Binary: "/no/such/binary"}); err == nil {
		t.Fatal("an upgrade to a missing binary")
	}
}

func TestUpgradeWaitsForQuiet(t *testing.T) {
	t.Parallel()
	u := newUpgrader(t, scriptHang, "v1.0.0")
	s, err := u.v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, u.v, s.ID, StateRunning)
	c := u.serve(t)
	info, err := c.Upgrade(t.Context(), UpgradeSpec{Binary: os.Args[0], Build: "v1.1.0"})
	if err != nil || info.Upgrade != "v1.1.0" {
		t.Fatalf("upgrade: %+v %v", info, err)
	}
	time.Sleep(100 * time.Millisecond)
	if len(u.execs) != 0 {
		t.Fatal("the visor restarts in a turn")
	}
	a, err := u.v.Attach(t.Context(), s.ID, AttachOptions{View: ViewChat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(t.Context(), s.ID, Input{Text: "end"}); err != nil {
		t.Fatal(err)
	}
	waitState(t, u.v, s.ID, StateIdle)
	time.Sleep(100 * time.Millisecond)
	if len(u.execs) != 0 {
		t.Fatal("the visor restarts with an attach client")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if got := u.waitExec(t); got[0] != os.Args[0] {
		t.Fatalf("exec %q", got)
	}
	if err := <-u.served; err != nil {
		t.Fatal(err)
	}
	v := newTestVisor(t, u.home, scriptHang, nil)
	defer v.Close(context.Background())
	if got := waitState(t, v, s.ID, StateSuspended); got.Turns != 1 {
		t.Fatalf("after the upgrade: %+v", got)
	}
	checkRestartEvent(t, v)
}

// TestUpgradeDeadline upgrades in a turn. The session in the visor process
// becomes lost, and the next visor resumes it.
func TestUpgradeDeadline(t *testing.T) {
	t.Parallel()
	u := newUpgrader(t, scriptHang, "v1.0.0")
	s, err := u.v.Start(t.Context(), StartSpec{
		Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang", Restart: RestartPolicy{Mode: RestartResume, Prompt: "continue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, u.v, s.ID, StateRunning)
	c := u.serve(t)
	if _, err := c.Upgrade(t.Context(), UpgradeSpec{Binary: os.Args[0], Build: "v1.1.0"}); err != nil {
		t.Fatal(err)
	}
	u.clock.add(defaultUpgradeWait + time.Second)
	u.waitExec(t)
	if err := <-u.served; err != nil {
		t.Fatal(err)
	}
	v := newTestVisor(t, u.home, scriptHang, nil)
	defer v.Close(context.Background())
	got := waitState(t, v, s.ID, StateIdle)
	lost := slices.ContainsFunc(sessionEvents(t, v, s.ID), func(e Event) bool { return e.Type == EventSessionState && e.State == StateLost })
	if got.Turns != 1 || !lost {
		t.Fatalf("after the upgrade: %+v, lost %v", got, lost)
	}
	checkRestartEvent(t, v)
}

// TestUpgradeKeepsHeldSession upgrades in a turn of a session in a holder.
// The visor does not wait, and the turn ends in the next visor.
func TestUpgradeKeepsHeldSession(t *testing.T) {
	t.Parallel()
	home := shortHome(t)
	gate := filepath.Join(home, "gate")
	u := newUpgraderWith(t, home, Options{Build: "v1.0.0", Env: map[string]string{agentwire.FakeScriptEnv: scriptGate, "GATE": gate}})
	s, err := u.v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, u.v, s.ID, StateRunning)
	c := u.serve(t)
	if _, err := c.Upgrade(t.Context(), UpgradeSpec{Binary: os.Args[0], Build: "v1.1.0"}); err != nil {
		t.Fatal(err)
	}
	u.waitExec(t)
	if err := <-u.served; err != nil {
		t.Fatal(err)
	}
	v := newHeldVisor(t, home, scriptGate, map[string]string{"GATE": gate})
	defer v.Close(context.Background())
	touch(t, gate)
	if got := waitState(t, v, s.ID, StateSuspended); got.Turns != 1 {
		t.Fatalf("after the upgrade: %+v", got)
	}
	if n := countEvents(sessionEvents(t, v, s.ID), EventAssistant); n != 2 {
		t.Fatalf("%d assistant events, want 2", n)
	}
	checkSessionSeq(t, v, s.ID)
	checkRestartEvent(t, v)
}

// TestClientEventsAcrossUpgrade reads the event log through an upgrade. The
// exec seam serves the next visor on the same socket.
func TestClientEventsAcrossUpgrade(t *testing.T) {
	t.Parallel()
	u := newUpgrader(t, scriptTurns, "v1.0.0")
	c := u.serve(t)
	st, err := c.Events(t.Context(), Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := c.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, c, s.ID, StateIdle)
	if _, err := c.Upgrade(t.Context(), UpgradeSpec{Binary: os.Args[0], Build: "v1.1.0"}); err != nil {
		t.Fatal(err)
	}
	u.waitExec(t)
	v := newTestVisor(t, u.home, scriptTurns, nil)
	ts := serveTest(t, v, u.sock)
	defer ts.stop(t)
	if _, err := c.Send(t.Context(), s.ID, Input{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	results := 0
	events := collect(t, st, func(e Event) bool {
		if e.Type == EventResult {
			results++
		}
		return results == 2
	})
	checkSeq(t, events, 0)
	if !slices.ContainsFunc(events, func(e Event) bool { return e.Type == EventRestarting }) {
		t.Fatal("no restart event")
	}
}

// upgrader is a visor with a fake clock and an exec seam that records each
// call.
type upgrader struct {
	home, sock string
	v          *Visor
	clock      *fakeClock
	execs      chan []string
	served     chan error
}

func newUpgrader(t *testing.T, script, build string) *upgrader {
	t.Helper()
	return newUpgraderWith(t, shortHome(t), Options{Build: build, Drivers: map[string]agentwire.Driver{"fake": agentwire.FakeDriver{Script: script}}})
}

// newUpgraderWith opens a visor with o, the fake clock, and the exec seam.
func newUpgraderWith(t *testing.T, home string, o Options) *upgrader {
	t.Helper()
	u := &upgrader{home: home, clock: &fakeClock{t: time.Unix(1_800_000_000, 0)}, execs: make(chan []string, 1), served: make(chan error, 1)}
	o.Home, o.Client, o.Now, o.upgradePoll = home, "test", u.clock.Now, 10*time.Millisecond
	o.Exec = func(binary string, args, _ []string) error {
		u.execs <- append([]string{binary}, args...)
		return nil
	}
	v, err := New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	u.v = v
	return u
}

// serve runs ServeListener and dials it.
func (u *upgrader) serve(t *testing.T) *Client {
	t.Helper()
	u.sock = shortSocket(t)
	ln, err := Listen(t.Context(), u.sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { u.served <- ServeListener(context.WithoutCancel(t.Context()), u.v, ln) }()
	c := dialTest(t, u.sock)
	t.Cleanup(func() { _ = c.Close(context.Background()) }) // test cleanup
	return c
}

func (u *upgrader) deadline() time.Time {
	u.v.mu.Lock()
	defer u.v.mu.Unlock()
	return u.v.upgrade.deadline
}

func (u *upgrader) waitExec(t *testing.T) []string {
	t.Helper()
	select {
	case got := <-u.execs:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("no exec")
		return nil
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func checkRestartEvent(t *testing.T, v *Visor) {
	t.Helper()
	st, err := v.Events(t.Context(), Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	collect(t, st, func(e Event) bool { return e.Type == EventRestarting })
}
