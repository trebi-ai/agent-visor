package orca

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
)

func TestCLIErrors(t *testing.T) {
	t.Parallel()
	f := newFakeOrca()
	f.fail["terminal close"] = &Error{Code: "terminal_not_found", Message: "gone"}
	err := f.cli().Close(t.Context(), "term_x")
	if !IsCode(err, "terminal_not_found") || err.Error() != "gone (terminal_not_found)" {
		t.Fatalf("error reply: %v", err)
	}
	if _, err := f.cli().call(t.Context(), callTimeout, "nope", "x"); err == nil || !strings.Contains(err.Error(), "unknown command nope x") {
		t.Fatalf("no JSON reply: %v", err)
	}
}

// replay answers each call with one recorded reply.
type replay []byte

func (r replay) Run(context.Context, string, []string) ([]byte, []byte, error) { return r, nil, nil }

func TestRecordedReplies(t *testing.T) {
	t.Parallel()
	ps, err := os.ReadFile("testdata/worktree-ps.json")
	if err != nil {
		t.Fatal(err)
	}
	wts, err := NewCLI("orca", replay(ps)).Worktrees(t.Context())
	if err != nil || len(wts) != 2 {
		t.Fatalf("worktrees: %v %+v", err, wts)
	}
	if w := wts[0]; w.DisplayName != "ENG-4" || len(w.Agents) != 1 || w.Agents[0].PaneKey == "" {
		t.Fatalf("first worktree: %+v", w)
	}
	read, err := os.ReadFile("testdata/terminal-read-screen.json")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := NewCLI("orca", replay(read)).Screen(t.Context(), "term_1")
	if err != nil || len(rows) != 3 || rows[2] != "> fix the test" {
		t.Fatalf("screen: %v %q", err, rows)
	}
}

func TestEffective(t *testing.T) {
	t.Parallel()
	a := Agent{State: AgentWorking, WorkingMode: "monitoring", StateStartedAt: 20}
	a.MainAgent = &struct {
		State          string `json:"state"`
		StateStartedAt int64  `json:"stateStartedAt"`
	}{State: AgentDone, StateStartedAt: 10}
	if got := a.Effective(); got.State != AgentDone || got.StateStartedAt != 10 {
		t.Fatalf("stale monitoring: %+v", got)
	}
	a.StateStartedAt = 5
	if got := a.Effective(); got.State != AgentWorking {
		t.Fatalf("a real background shell: %+v", got)
	}
}

func TestEnsureWorkspace(t *testing.T) {
	t.Parallel()
	f := newFakeOrca()
	f.repos = []Repo{{ID: "r1", Path: "/src/app"}}
	f.worktrees = []Worktree{{ID: "r1::/src/app-old", RepoID: "r1", Path: "/src/app-old", DisplayName: "ENG-1"}}
	c := f.cli()
	got, err := c.EnsureWorkspace(t.Context(), WorkspaceSpec{Repo: "/src/app", Name: "eng-1"}, 0)
	if err != nil || got.ID != "r1::/src/app-old" || got.New {
		t.Fatalf("the workspace that is there: %+v %v", got, err)
	}
	got, err = c.EnsureWorkspace(t.Context(), WorkspaceSpec{Repo: "/src/app", Name: "ENG-2", BaseBranch: "dev", Comment: "a note"}, 0)
	if err != nil || !got.New || got.Shell == "" || got.Path == "" {
		t.Fatalf("a new workspace: %+v %v", got, err)
	}
	create := f.callsOf("worktree create")[0]
	if flag(create, "--base-branch") != "dev" || flag(create, "--comment") != "a note" {
		t.Fatalf("create args: %q", create)
	}
	if _, err := c.EnsureWorkspace(t.Context(), WorkspaceSpec{Repo: "/src/other", Name: "x"}, 0); !IsCode(err, "repo_not_found") {
		t.Fatalf("no repo: %v", err)
	}
}

func TestStartRunsTheScript(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := newFakeOrca()
	f.worktrees = []Worktree{{ID: "r1::/src/app", RepoID: "r1", Path: "/src/app"}}
	v, _ := orcaVisor(t, home, f, false)
	defer v.Close(context.Background())
	dir := t.TempDir()
	s, err := v.Start(t.Context(), agentvisor.StartSpec{
		Kind: Kind, Harness: "claude", Title: "Fix it", WorkingDir: dir, Prompt: "go", ExtraArgs: []string{"-n", "ENG-4"},
		Env: map[string]string{"SECRET": "x y'z"}, Labels: map[string]string{LabelWorktree: "r1::/src/app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	create := f.callsOf("terminal create")[0]
	if flag(create, "--worktree") != "id:r1::/src/app" || flag(create, "--title") != "Fix it" || strings.Contains(strings.Join(create, " "), "x y") {
		t.Fatalf("create args: %q", create)
	}
	script := f.script("term_1")
	for _, want := range []string{`export SECRET='x y'\''z'`, "export AGENT_VISOR_SESSION='" + s.ID + "'", "cd '" + dir + "'", "exec 'claude' '-n' 'ENG-4'"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q:\n%s", want, script)
		}
	}
	sends := f.callsOf("terminal send")
	if len(sends) != 1 || flag(sends[0], "--text") != "go" || flag(sends[0], "--terminal") != "term_1" {
		t.Fatalf("prompt sends: %q", sends)
	}
	got := waitSession(t, v, s.ID, func(s agentvisor.Session) bool { return s.Labels[LabelPane] != "" })
	if got.Labels[LabelPane] != "tab1:leaf1" || got.Labels[LabelTerminal] != "term_1" || got.Labels[LabelWorktree] != "r1::/src/app" {
		t.Fatalf("labels: %v", got.Labels)
	}
	if got.State != agentvisor.StateRunning {
		t.Fatalf("state after the prompt: %s", got.State)
	}
}

func TestStartInAShell(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := newFakeOrca()
	f.repos = []Repo{{ID: "r1", Path: "/src/app"}}
	ws, err := f.cli().EnsureWorkspace(t.Context(), WorkspaceSpec{Repo: "/src/app", Name: "ENG-5"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := orcaVisor(t, home, f, false)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), agentvisor.StartSpec{Kind: Kind, Harness: "codex", WorkingDir: t.TempDir(), Labels: map[string]string{LabelShell: ws.Shell}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.callsOf("terminal create")) != 0 || !strings.Contains(f.script(ws.Shell), "exec 'codex'") {
		t.Fatalf("the command did not go into the shell: %q", f.script(ws.Shell))
	}
	waitSession(t, v, s.ID, inState(agentvisor.StateIdle))
}

func TestCardEvents(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := newFakeOrca()
	f.worktrees = []Worktree{{ID: "w1", Path: "/src/app"}}
	v, _ := orcaVisor(t, home, f, false)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), agentvisor.StartSpec{Kind: Kind, Harness: "claude", WorkingDir: t.TempDir(), Labels: map[string]string{LabelWorktree: "w1"}})
	if err != nil {
		t.Fatal(err)
	}
	pane := "tab1:leaf1"
	f.setAgent(pane, &Agent{State: AgentWorking, Prompt: "typed in orca", ToolName: "Bash", ToolInput: "ls", StateStartedAt: 1})
	waitSession(t, v, s.ID, inState(agentvisor.StateRunning))
	f.setAgent(pane, &Agent{State: AgentBlocked, Prompt: "typed in orca", ToolName: "Bash", ToolInput: "rm -rf build", StateStartedAt: 2})
	got := waitSession(t, v, s.ID, inState(agentvisor.StateWaiting))
	if len(got.Pending) != 1 || got.Pending[0].Kind != "permission" || got.Pending[0].Input != "rm -rf build" {
		t.Fatalf("pending: %+v", got.Pending)
	}
	f.setAgent(pane, &Agent{State: AgentDone, Prompt: "typed in orca", LastAssistantMessage: "Done.", StateStartedAt: 3})
	got = waitSession(t, v, s.ID, inState(agentvisor.StateIdle))
	if got.Turns != 1 || len(got.Pending) != 0 {
		t.Fatalf("after the turn: %+v", got)
	}
	events := sessionEvents(t, v, s.ID)
	var users, tools, results []string
	for _, e := range events {
		switch e.Type {
		case agentvisor.EventUser:
			users = append(users, e.Text)
		case agentvisor.EventTool:
			tools = append(tools, e.Tool.Name+" "+e.Tool.Input)
		case agentvisor.EventResult:
			results = append(results, e.Result.Text)
		}
	}
	if !slices.Equal(users, []string{"typed in orca"}) || !slices.Equal(tools, []string{"Bash ls"}) || !slices.Equal(results, []string{"Done."}) {
		t.Fatalf("users %q, tools %q, results %q", users, tools, results)
	}
	// The echo of a send is no second user event.
	if _, err := v.Send(t.Context(), s.ID, agentvisor.Input{Text: "next"}); err != nil {
		t.Fatal(err)
	}
	f.setAgent(pane, &Agent{State: AgentWorking, Prompt: "next", StateStartedAt: 4})
	f.setAgent(pane, &Agent{State: AgentDone, Prompt: "next", StateStartedAt: 5})
	waitSession(t, v, s.ID, func(s agentvisor.Session) bool { return s.Turns == 2 })
	n := 0
	for _, e := range sessionEvents(t, v, s.ID) {
		if e.Type == agentvisor.EventUser && e.Text == "next" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d user events for one send", n)
	}
}

func TestSessionEnds(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := newFakeOrca()
	f.worktrees = []Worktree{{ID: "w1", Path: "/src/app"}}
	v, _ := orcaVisor(t, home, f, false)
	defer v.Close(context.Background())
	spec := agentvisor.StartSpec{Kind: Kind, Harness: "claude", WorkingDir: t.TempDir(), Labels: map[string]string{LabelWorktree: "w1"}}
	exited, err := v.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// Only the card gives waiting, so the poll saw the card.
	f.setAgent("tab1:leaf1", &Agent{State: AgentBlocked, StateStartedAt: 1})
	waitSession(t, v, exited.ID, inState(agentvisor.StateWaiting))
	f.setAgent("tab1:leaf1", nil)
	waitSession(t, v, exited.ID, inState(agentvisor.StateExited))

	stopped, err := v.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Stop(t.Context(), stopped.ID); err != nil {
		t.Fatal(err)
	}
	waitSession(t, v, stopped.ID, inState(agentvisor.StateExited))
	if len(f.callsOf("terminal close")) != 1 {
		t.Fatalf("closes: %q", f.callsOf("terminal close"))
	}
	if left, _ := os.ReadDir(home + "/orca"); len(left) != 1 { //nolint:errcheck // the count check covers it
		t.Fatalf("binding files stay: %v", left)
	}
}

func TestRecover(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := newFakeOrca()
	f.worktrees = []Worktree{{ID: "w1", Path: "/src/app"}}
	v, _ := orcaVisor(t, home, f, false)
	s, err := v.Start(t.Context(), agentvisor.StartSpec{Kind: Kind, Harness: "claude", WorkingDir: t.TempDir(), Labels: map[string]string{LabelWorktree: "w1"}})
	if err != nil {
		t.Fatal(err)
	}
	f.setAgent("tab1:leaf1", &Agent{State: AgentWorking, StateStartedAt: 1})
	waitSession(t, v, s.ID, inState(agentvisor.StateRunning))
	if err := v.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.callsOf("terminal close")) != 0 {
		t.Fatal("a visor close closed the terminal")
	}
	v, b := orcaVisor(t, home, f, false)
	defer v.Close(context.Background())
	if got, _ := v.Session(t.Context(), s.ID); got.State != agentvisor.StateRunning { //nolint:errcheck // the state check covers it
		t.Fatalf("after the restart: %s", got.State)
	}
	if pane, term, _, ok := b.Binding(s.ID); !ok || pane != "tab1:leaf1" || term != "term_1" {
		t.Fatalf("binding: %s %s %v", pane, term, ok)
	}
	f.setAgent("tab1:leaf1", &Agent{State: AgentDone, StateStartedAt: 2})
	waitSession(t, v, s.ID, inState(agentvisor.StateIdle))
}

func TestDiscover(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	f := newFakeOrca()
	f.repos = []Repo{{ID: "r1", Path: dir}}
	f.worktrees = []Worktree{{ID: "w1", Path: dir}}
	f.terminals = []Terminal{{Handle: "term_u", WorktreeID: "w1", TabID: "tu", LeafID: "lu", Title: "✳ Fix the docs"}}
	f.setAgent("tu:lu", &Agent{State: AgentWorking, AgentType: "claude", StateStartedAt: 1})
	v, _ := orcaVisor(t, home, f, true)
	defer v.Close(context.Background())
	var s agentvisor.Session
	waitFor(t, "found session", func() bool {
		all, err := v.Sessions(t.Context(), agentvisor.Filter{Kind: Kind})
		if err == nil && len(all) == 1 {
			s = all[0]
		}
		return s.ID != ""
	})
	if s.Harness != "claude" || s.Title != "Fix the docs" || s.WorkingDir != dir || s.Labels[LabelTerminal] != "term_u" {
		t.Fatalf("found session: %+v", s)
	}
	waitSession(t, v, s.ID, inState(agentvisor.StateRunning))
	// A hook of the pane joins the session by its labels.
	payload, err := json.Marshal(map[string]string{"session_id": "v-9", "cwd": dir, "hook_event_name": "Stop", "last_assistant_message": "Done."})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Hook(t.Context(), agentvisor.HookCall{Harness: "claude", Labels: HookLabels(func(string) string { return "tu:lu" }), Payload: payload})
	if err != nil || got.ID != s.ID || got.VendorID != "v-9" {
		t.Fatalf("hook: %v %+v", err, got)
	}
	projects, err := v.Projects(t.Context(), agentvisor.Filter{})
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects: %v %+v", err, projects)
	}
	f.setAgent("tu:lu", nil)
	waitSession(t, v, s.ID, inState(agentvisor.StateExited))
}

func TestHookBeforeDiscover(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	f := newFakeOrca()
	f.worktrees = []Worktree{{ID: "w1", Path: dir}}
	f.terminals = []Terminal{{Handle: "term_u", WorktreeID: "w1", TabID: "tu", LeafID: "lu"}}
	v, _ := orcaVisor(t, home, f, true)
	defer v.Close(context.Background())
	payload, err := json.Marshal(map[string]string{"session_id": "v-1", "cwd": dir, "hook_event_name": "UserPromptSubmit", "prompt": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := v.Hook(t.Context(), agentvisor.HookCall{Harness: "claude", Labels: map[string]string{LabelPane: "tu:lu"}, Payload: payload})
	if err != nil || observed.Kind != agentvisor.KindObserved {
		t.Fatalf("hook: %v %+v", err, observed)
	}
	f.setAgent("tu:lu", &Agent{State: AgentWorking, AgentType: "claude", StateStartedAt: 1})
	got := waitSession(t, v, observed.ID, func(s agentvisor.Session) bool { return s.Kind == Kind })
	if got.VendorID != "v-1" || got.Labels[LabelTerminal] != "term_u" {
		t.Fatalf("adopted: %+v", got)
	}
	if all, _ := v.Sessions(t.Context(), agentvisor.Filter{}); len(all) != 1 { //nolint:errcheck // the count check covers it
		t.Fatalf("sessions: %+v", all)
	}
}

func TestAnswerAndTerminal(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	f := newFakeOrca()
	f.worktrees = []Worktree{{ID: "w1", Path: "/src/app"}}
	screen, err := os.ReadFile("testdata/permission-screen.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.screen = strings.Split(string(screen), "\n")
	v, _ := orcaVisor(t, home, f, false)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), agentvisor.StartSpec{Kind: Kind, Harness: "claude", WorkingDir: t.TempDir(), Labels: map[string]string{LabelWorktree: "w1"}})
	if err != nil {
		t.Fatal(err)
	}
	f.setAgent("tab1:leaf1", &Agent{State: AgentBlocked, ToolName: "Bash", StateStartedAt: 7})
	waitSession(t, v, s.ID, inState(agentvisor.StateWaiting))
	if err := v.Answer(t.Context(), s.ID, "orca-7", agentvisor.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if err := v.Answer(t.Context(), s.ID, "orca-7", agentvisor.Answer{Message: "use make clean"}); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range f.callsOf("terminal send") {
		texts = append(texts, flag(c, "--text"))
	}
	if !slices.Equal(texts, []string{"1", "\x1b", "use make clean"}) {
		t.Fatalf("answer keys: %q", texts)
	}
	got, err := v.Screen(t.Context(), s.ID)
	if err != nil || got.Rows != len(f.screen) || got.Lines[1] != " Bash command" {
		t.Fatalf("screen: %v %+v", err, got)
	}
	if err := v.Keys(t.Context(), s.ID, []byte("\x15")); err != nil {
		t.Fatal(err)
	}
	if err := v.Interrupt(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	sends := f.callsOf("terminal send")
	if flag(sends[3], "--text") != "\x15" || flag(sends[4], "--text") != "\x1b" || slices.Contains(sends[3], "--enter") {
		t.Fatalf("keys: %q", sends[3:])
	}
	f.screen = []string{"> plain"}
	if err := v.Answer(t.Context(), s.ID, "orca-7", agentvisor.Answer{Allow: true}); err == nil {
		t.Fatal("an answer with no prompt on the screen")
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	f := newFakeOrca()
	f.worktrees = []Worktree{{ID: "w1", Path: "/src/app"}}
	polled := make(chan struct{}, 1)
	b, err := New(Options{CLI: f.cli(), Dir: t.TempDir(), Poll: 20 * time.Millisecond, OnPoll: func() {
		select {
		case polled <- struct{}{}:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	select {
	case <-polled:
	case <-time.After(5 * time.Second):
		t.Fatal("no poll with no session")
	}
	if s := b.Snapshot(); s.At.IsZero() || len(s.Worktrees) != 1 || s.Worktrees[0].ID != "w1" {
		t.Fatalf("snapshot: %+v", s)
	}
}

func sessionEvents(t *testing.T, v *agentvisor.Visor, id string) []agentvisor.Event {
	t.Helper()
	st, err := v.Events(t.Context(), agentvisor.Filter{Session: id}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var out []agentvisor.Event
	for {
		// The log comes at once; a short pause ends the read.
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		e, err := st.Next(ctx)
		cancel()
		if err != nil {
			return out
		}
		out = append(out, e)
	}
}
