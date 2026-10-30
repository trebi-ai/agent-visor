package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// scriptPermission asks for a permission on each prompt, and ends the turn
// after the answer.
const scriptPermission = `while read -r line; do
  case "$line" in
  *decision*) echo '{"type":"assistant","text":"allowed"}'; echo '{"type":"result","subtype":"success","text":"done","session_id":"v1"}';;
  *) echo '{"type":"init","session_id":"v1"}'; echo '{"type":"permission","id":"p1","tool":"Bash","question":"Run it?"}';;
  esac
done`

// script drives a served visor only through the command tree.
type script struct {
	t      *testing.T
	socket string
}

func TestScriptSessionLifecycle(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value-0042"
	sc := newScript(t)
	dir := t.TempDir()

	id := strings.TrimSpace(sc.run("session", "start", "--harness", "fake", "--dir", dir, "--prompt", "go",
		"--title", "Script test", "--label", "app.run=r1", "--env", "API_TOKEN="+secret))
	if !strings.HasPrefix(id, "ses_") {
		t.Fatalf("start printed %q", id)
	}
	s := sc.wait(id, agentvisor.StateWaiting)
	if len(s.Pending) != 1 || s.Pending[0].ID != "p1" {
		t.Fatalf("pending: %+v", s.Pending)
	}
	if show := sc.run("session", "show", id); !strings.Contains(show, "p1 Bash Run it?") || !strings.Contains(show, "API_TOKEN") {
		t.Fatalf("show:\n%s", show)
	}
	sc.run("session", "answer", id, "p1", "--allow")
	s = sc.wait(id, agentvisor.StateIdle)

	follow := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- sc.runTo(follow, "session", "logs", id, "--follow", "--after", strconv.FormatUint(s.Seq, 10))
	}()

	sc.run("session", "send", id, "more")
	sc.wait(id, agentvisor.StateWaiting)
	sc.run("session", "answer", id, "p1", "--deny", "--message", "no")
	sc.wait(id, agentvisor.StateIdle)

	logs := sc.run("session", "logs", id)
	for _, want := range []string{"session.state   starting", "request         p1 Bash Run it?", "request.closed  p1", "result          success done"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs miss %q:\n%s", want, logs)
		}
	}
	var last uint64
	for line := range strings.Lines(sc.run("--json", "session", "logs", id)) {
		var e agentvisor.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("logs --json line %q: %v", line, err)
		}
		if e.Session != id || e.Seq <= last {
			t.Fatalf("event %+v after seq %d", e, last)
		}
		last = e.Seq
	}

	var listed []agentvisor.Session
	sc.json(&listed, "session", "ls", "--label", "app.run=r1")
	if len(listed) != 1 || listed[0].ID != id || listed[0].Turns != 2 {
		t.Fatalf("session ls --label: %+v", listed)
	}
	if tree := sc.run("ls"); !strings.Contains(tree, "    "+id) || !strings.Contains(tree, "Script test") {
		t.Fatalf("ls:\n%s", tree)
	}

	sc.run("session", "stop", id)
	sc.wait(id, agentvisor.StateExited)
	if out := sc.run("session", "ls"); strings.Contains(out, id) {
		t.Fatalf("session ls shows an exited session:\n%s", out)
	}
	if out := sc.run("session", "ls", "--all"); !strings.Contains(out, id) {
		t.Fatalf("session ls --all misses the session:\n%s", out)
	}
	if err := sc.runTo(io.Discard, "session", "resume", id); !errors.Is(err, agentvisor.ErrState) {
		t.Fatalf("resume an exited session: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("logs --follow: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("logs --follow did not end at exit")
	}
	if got := follow.String(); !strings.Contains(got, "user            more") || !strings.Contains(got, "session.state   exited") {
		t.Fatalf("logs --follow:\n%s", got)
	}
	if strings.Contains(logs+follow.String(), secret) {
		t.Fatal("an env value is in the output")
	}
}

func TestScriptWorkdirs(t *testing.T) {
	t.Parallel()
	sc := newScript(t)
	dir := t.TempDir()
	wd := strings.TrimSpace(sc.run("workdir", "ensure", dir, "--key", "k1"))
	if again := strings.TrimSpace(sc.run("workdir", "ensure", dir, "--key", "k1")); again != wd {
		t.Fatalf("ensure with a key gave %s, then %s", wd, again)
	}
	var ws []agentvisor.Workdir
	sc.json(&ws, "workdir", "ls", "--key", "k1")
	if len(ws) != 1 || ws[0].ID != wd {
		t.Fatalf("workdir ls --key: %+v", ws)
	}
	sc.run("workdir", "archive", wd)
	if out := sc.run("workdir", "ls"); !strings.Contains(out, "archived") {
		t.Fatalf("workdir ls after archive:\n%s", out)
	}
	if out := sc.run("ls"); strings.Contains(out, wd) {
		t.Fatalf("ls shows an archived workdir:\n%s", out)
	}
	sc.run("workdir", "restore", wd)
	sc.run("workdir", "rm", wd)
	project := ws[0].Project
	sc.run("project", "forget", project)
	if out := sc.run("project", "ls"); strings.Contains(out, project) {
		t.Fatalf("project ls after forget:\n%s", out)
	}
}

// newScript serves an in-process visor with the fake harness on a short
// socket.
func newScript(t *testing.T) *script {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "av")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) }) // test cleanup
	sock := filepath.Join(sockDir, "s")
	v, err := agentvisor.New(t.Context(), agentvisor.Options{
		Home: t.TempDir(), Client: "test", Logger: slog.New(slog.DiscardHandler),
		Drivers: map[string]agentwire.Driver{"fake": agentwire.FakeDriver{Script: scriptPermission}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- agentvisor.Serve(ctx, v, sock) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
		if err := v.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; {
		c, err := agentvisor.Dial(t.Context(), sock)
		if err == nil {
			_ = c.Close(t.Context()) // a probe
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the visor does not serve: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &script{t: t, socket: sock}
}

// run runs one command and fails the test on an error.
func (s *script) run(args ...string) string {
	s.t.Helper()
	var out bytes.Buffer
	if err := s.runTo(&out, args...); err != nil {
		s.t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
	return out.String()
}

func (s *script) runTo(w io.Writer, args ...string) error {
	cmd := Command(Options{Stdout: w, Stderr: io.Discard})
	return cmd.Run(s.t.Context(), append([]string{"agent-visor", "--socket", s.socket}, args...))
}

func (s *script) json(v any, args ...string) {
	s.t.Helper()
	if err := json.Unmarshal([]byte(s.run(append([]string{"--json"}, args...)...)), v); err != nil {
		s.t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
}

// wait reads the session with "session show" until it is in state.
func (s *script) wait(id string, state agentvisor.State) agentvisor.Session {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got agentvisor.Session
		s.json(&got, "session", "show", id)
		if got.State == state {
			return got
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("session %s is %s, want %s", id, got.State, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
