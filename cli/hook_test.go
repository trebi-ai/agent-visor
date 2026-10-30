package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentvisor "github.com/trebi-ai/agent-visor"
)

func TestHookCommand(t *testing.T) {
	t.Parallel()
	sc := newScript(t)
	cwd := t.TempDir()
	payload := `{"session_id":"v-1","cwd":"` + cwd + `","hook_event_name":"UserPromptSubmit","prompt":"hi"}`
	if err := runHook(t, payload, "--socket", sc.socket, "hook", "claude"); err != nil {
		t.Fatal(err)
	}
	c, err := agentvisor.Dial(t.Context(), sc.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	list, err := c.Sessions(t.Context(), agentvisor.Filter{Kind: agentvisor.KindObserved})
	if err != nil || len(list) != 1 || list[0].State != agentvisor.StateRunning || list[0].VendorID != "v-1" {
		t.Fatalf("observed sessions: %+v %v", list, err)
	}
	// A call that the visor rejects does not fail the agent CLI.
	if err := runHook(t, payload, "--socket", sc.socket, "hook", "--session", "ses_none", "claude"); err != nil {
		t.Fatalf("rejected call: %v", err)
	}
	if err := runHook(t, "not json", "--socket", sc.socket, "hook", "claude"); err == nil {
		t.Fatal("a bad payload passes")
	}
}

func TestHookCommandSpools(t *testing.T) {
	t.Parallel()
	home, cwd := t.TempDir(), t.TempDir()
	payload := `{"session_id":"v-2","cwd":"` + cwd + `","hook_event_name":"SessionStart","source":"startup"}`
	sockDir, err := os.MkdirTemp("", "av")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	if err := runHook(t, payload, "--home", home, "--socket", filepath.Join(sockDir, "s"), "hook", "claude"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "hooks", "spool"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("spool: %v %v", entries, err)
	}
	v, err := agentvisor.New(t.Context(), agentvisor.Options{Home: home, Client: "test", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	list, err := v.Sessions(t.Context(), agentvisor.Filter{Kind: agentvisor.KindObserved})
	if err != nil || len(list) != 1 || list[0].State != agentvisor.StateIdle || list[0].VendorID != "v-2" {
		t.Fatalf("replayed sessions: %+v %v", list, err)
	}
}

// runHook runs the command tree with the payload on stdin.
func runHook(t *testing.T, payload string, args ...string) error {
	t.Helper()
	cmd := Command(Options{Stdin: strings.NewReader(payload), Stdout: io.Discard, Stderr: io.Discard})
	return cmd.Run(t.Context(), append([]string{"agent-visor"}, args...))
}
