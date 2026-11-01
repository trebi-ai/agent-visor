//go:build live

package orca

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// TestLive runs a script in a new terminal of the real Orca. It touches only
// that terminal. ORCA_LIVE_DIR is a worktree folder that Orca knows.
//
//	ORCA_LIVE_DIR=$HOME/src/app go test -tags live -run TestLive ./backend/orca/
func TestLive(t *testing.T) {
	dir := os.Getenv("ORCA_LIVE_DIR")
	if dir == "" {
		t.Skip("set ORCA_LIVE_DIR")
	}
	exe, ok := FindCLI()
	if !ok {
		t.Skip("no orca CLI")
	}
	cli := NewCLI(exe, nil)
	if !cli.Reachable(t.Context()) {
		t.Skip("orca does not answer")
	}
	home := t.TempDir()
	b, err := New(Options{CLI: cli, Dir: home + "/orca", Poll: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	v, err := agentvisor.New(t.Context(), agentvisor.Options{Home: home, Client: "live", Backends: []agentvisor.Backend{b}})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), agentvisor.StartSpec{
		Kind: Kind, Role: agentvisor.RoleScript, Title: "agent-visor live test", WorkingDir: dir,
		Command: []string{"sh", "-c", `echo "live-$LIVE_MARK"; read x; echo "got-$x"; sleep 120`},
		Env:     map[string]string{"LIVE_MARK": "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pane, term, _, ok := b.Binding(s.ID)
	if !ok || pane == "" || term == "" {
		t.Fatalf("binding: %q %q %v", pane, term, ok)
	}
	defer cli.Close(context.Background(), term) //nolint:errcheck // a second close after Stop
	screenHas(t, v, s.ID, "live-ok")
	if err := v.Keys(t.Context(), s.ID, []byte("abc\r")); err != nil {
		t.Fatal(err)
	}
	screenHas(t, v, s.ID, "got-abc")
	if err := v.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	// The close ends in the background.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		terms, err := cli.Terminals(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(terms, func(x Terminal) bool { return x.Handle == term }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminal stays after Stop")
		}
	}
}

func screenHas(t *testing.T, v *agentvisor.Visor, id, text string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		sc, err := v.Screen(t.Context(), id)
		if err == nil && strings.Contains(strings.Join(sc.Lines, "\n"), text) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q on the screen: %v %q", text, err, sc.Lines)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
