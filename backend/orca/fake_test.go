package orca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// fakeOrca is an Orca CLI in memory. It keeps worktrees, terminals, and
// agent cards, and records each call.
type fakeOrca struct {
	mu        sync.Mutex
	repos     []Repo
	worktrees []Worktree
	terminals []Terminal
	screen    []string
	scripts   map[string]string // command of a terminal create: the script text
	calls     [][]string
	fail      map[string]*Error // the next call of a command fails
	next      int
}

func newFakeOrca() *fakeOrca {
	return &fakeOrca{scripts: map[string]string{}, fail: map[string]*Error{}}
}

func (f *fakeOrca) Run(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slices.Clone(args))
	cmd := strings.Join(args[:min(2, len(args))], " ")
	if e, ok := f.fail[cmd]; ok {
		delete(f.fail, cmd)
		return reply(nil, e), nil, nil
	}
	switch cmd {
	case "status":
		return reply(map[string]any{"runtime": map[string]any{"reachable": true}}, nil), nil, nil
	case "repo list":
		return reply(map[string]any{"repos": f.repos}, nil), nil, nil
	case "worktree ps":
		return reply(map[string]any{"worktrees": f.worktrees}, nil), nil, nil
	case "worktree create":
		f.next++
		repo := strings.TrimPrefix(flag(args, "--repo"), "path:")
		w := Worktree{ID: fmt.Sprintf("r1::%s-%d", repo, f.next), RepoID: "r1", Path: fmt.Sprintf("%s-%d", repo, f.next), DisplayName: flag(args, "--name"), Comment: flag(args, "--comment")}
		f.worktrees = append(f.worktrees, w)
		f.terminals = append(f.terminals, f.newTerminal(w.ID, w.Path))
		return reply(map[string]any{"worktree": map[string]any{"id": w.ID, "path": w.Path}}, nil), nil, nil
	case "terminal list":
		var out []Terminal
		for _, t := range f.terminals {
			if w := strings.TrimPrefix(flag(args, "--worktree"), "id:"); w == "" || w == t.WorktreeID {
				out = append(out, t)
			}
		}
		return reply(map[string]any{"terminals": out}, nil), nil, nil
	case "terminal create":
		where := flag(args, "--worktree")
		t := f.newTerminal(strings.TrimPrefix(where, "id:"), strings.TrimPrefix(where, "path:"))
		t.Title = flag(args, "--title")
		f.terminals = append(f.terminals, t)
		f.readScript(t.Handle, flag(args, "--command"))
		return reply(map[string]any{"terminal": t}, nil), nil, nil
	case "terminal show":
		if t, ok := f.terminal(flag(args, "--terminal")); ok {
			return reply(map[string]any{"terminal": t}, nil), nil, nil
		}
		return reply(nil, &Error{Code: "terminal_not_found", Message: "no terminal"}), nil, nil
	case "terminal close":
		h := flag(args, "--terminal")
		f.terminals = slices.DeleteFunc(f.terminals, func(t Terminal) bool { return t.Handle == h })
		return reply(map[string]any{}, nil), nil, nil
	case "terminal send":
		if slices.Contains(args, "--enter") && !slices.Contains(args, "--wait-submit") {
			f.readScript(flag(args, "--terminal"), flag(args, "--text"))
		}
		return reply(map[string]any{"send": map[string]any{"prompt": map[string]any{"stages": []string{"turn_started"}}}}, nil), nil, nil
	case "terminal wait":
		return reply(map[string]any{"wait": map[string]any{"satisfied": true}}, nil), nil, nil
	case "terminal read":
		return reply(map[string]any{"terminal": map[string]any{"tail": f.screen}}, nil), nil, nil
	}
	return nil, []byte("unknown command " + cmd), errors.New("exit status 1")
}

func (f *fakeOrca) newTerminal(worktree, path string) Terminal {
	f.next++
	n := fmt.Sprint(f.next)
	return Terminal{Handle: "term_" + n, WorktreeID: worktree, WorktreePath: path, TabID: "tab" + n, LeafID: "leaf" + n, Connected: true}
}

// readScript keeps the text of a start script, before the script removes
// itself.
func (f *fakeOrca) readScript(handle, command string) {
	path, ok := strings.CutPrefix(command, "sh ")
	if !ok {
		return
	}
	data, err := os.ReadFile(strings.Trim(path, "'"))
	if err == nil {
		f.scripts[handle] = string(data)
	}
}

func (f *fakeOrca) terminal(handle string) (Terminal, bool) {
	i := slices.IndexFunc(f.terminals, func(t Terminal) bool { return t.Handle == handle })
	if i < 0 {
		return Terminal{}, false
	}
	return f.terminals[i], true
}

// setAgent puts the agent card of the pane in the worktree of its terminal,
// or takes it out when a is nil.
func (f *fakeOrca) setAgent(pane string, a *Agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.worktrees {
		f.worktrees[i].Agents = slices.DeleteFunc(f.worktrees[i].Agents, func(x Agent) bool { return x.PaneKey == pane })
	}
	if a == nil {
		return
	}
	a.PaneKey = pane
	for _, t := range f.terminals {
		if t.PaneKey() != pane {
			continue
		}
		for i := range f.worktrees {
			if f.worktrees[i].ID == t.WorktreeID {
				f.worktrees[i].Agents = append(f.worktrees[i].Agents, *a)
				return
			}
		}
	}
	panic("no worktree for pane " + pane)
}

func (f *fakeOrca) callsOf(cmd string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if strings.Join(c[:min(2, len(c))], " ") == cmd {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeOrca) script(handle string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scripts[handle]
}

func reply(result any, e *Error) []byte {
	r := map[string]any{"ok": e == nil}
	if e != nil {
		r["error"] = map[string]string{"code": e.Code, "message": e.Message}
	} else {
		r["result"] = result
	}
	data, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return data
}

func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// orcaVisor starts a visor with the orca backend on the fake.
func orcaVisor(t *testing.T, home string, f *fakeOrca, discover bool) (*agentvisor.Visor, *Backend) {
	t.Helper()
	b, err := New(Options{CLI: f.cli(), Dir: home + "/orca", Poll: 20 * time.Millisecond, Discover: discover, ShellWait: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	v, err := agentvisor.New(t.Context(), agentvisor.Options{Home: home, Client: "test", Backends: []agentvisor.Backend{b}})
	if err != nil {
		t.Fatal(err)
	}
	return v, b
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitSession(t *testing.T, v *agentvisor.Visor, id string, ok func(agentvisor.Session) bool) agentvisor.Session {
	t.Helper()
	var s agentvisor.Session
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		if s, err = v.Session(t.Context(), id); err == nil && ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s stays %s: %v", id, s.State, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func inState(st agentvisor.State) func(agentvisor.Session) bool {
	return func(s agentvisor.Session) bool { return s.State == st }
}

func (f *fakeOrca) cli() *CLI { return NewCLI("orca", f) }
