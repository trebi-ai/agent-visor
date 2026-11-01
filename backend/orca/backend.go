// Package orca is a visor backend for the terminals of Orca on this machine.
// A session is an agent in an Orca terminal. Orca owns the terminal, so the
// session lives through a visor restart. The backend reads the agent state
// from the Orca agent cards. It has no typed events: it answers a permission
// prompt through the screen.
package orca

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	agentwire "github.com/trebi-ai/agent-wire"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// Kind is the session kind of this backend.
const Kind agentvisor.Kind = "orca"

// The labels of an Orca session. The backend puts them on each session, and a
// start spec can set LabelWorktree and LabelShell.
const (
	// LabelPane is the Orca pane (tabId:leafId). A hook call with it finds
	// the session.
	LabelPane = "orca.pane"
	// LabelTerminal is the Orca terminal handle.
	LabelTerminal = "orca.terminal"
	// LabelWorktree is the Orca worktree id. In a start spec, the terminal
	// opens in this worktree.
	LabelWorktree = "orca.worktree"
	// LabelShell is a terminal handle in a start spec. The agent command goes
	// into this shell, and no new terminal opens.
	LabelShell = "orca.shell"
)

// Env names of the hook process in an Orca terminal.
const EnvPane = "ORCA_PANE_KEY"

// HookLabels gives the labels of a hook call from the env of the hook
// process. They are nil outside an Orca terminal.
func HookLabels(getenv func(string) string) map[string]string {
	if p := getenv(EnvPane); p != "" {
		return map[string]string{LabelPane: p}
	}
	return nil
}

// Defaults of Options.
const (
	defaultPoll = 2 * time.Second
	// endPolls is how many polls an agent card is gone before its session ends.
	endPolls = 3
	// gonePolls is how many polls a terminal is gone before its session ends.
	gonePolls = 2
	newGrace  = 10 * time.Second
)

// Options configures the backend.
type Options struct {
	// CLI is the Orca CLI. Nil uses FindCLI.
	CLI *CLI
	// Dir keeps the session bindings and the launch scripts.
	Dir string
	// Poll is the time between two reads of the Orca state. Zero is 2 s.
	Poll time.Duration
	// Command gives the command of an agent session. Nil runs the harness
	// program with the model args and the extra args. A resume needs it.
	Command agentvisor.TerminalCommand
	// Discover reports the agents that the visor did not start, for example
	// the agents that the user started in Orca.
	Discover bool
	// ShellWait is the pause between two looks for the first shell of a new
	// workspace. Zero is 300 ms.
	ShellWait time.Duration
	// OnPoll runs after each read of the Orca state. With OnPoll, the backend
	// reads Orca also when it has no session.
	OnPoll func()
	Logger *slog.Logger
}

// Snapshot is the Orca state of the last read.
type Snapshot struct {
	Worktrees []Worktree
	Terminals []Terminal
	At        time.Time // zero before the first read
}

// Backend hosts agent sessions in Orca terminals.
type Backend struct {
	cli  *CLI
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	handles  map[string]*handle // by visor session
	reported map[string]bool    // found panes that wait for Adopt
	found    chan agentvisor.Found
	wake     chan struct{}
	last     Snapshot

	cancel context.CancelFunc
	done   chan struct{}
}

// New starts the poll of the Orca state.
func New(o Options) (*Backend, error) {
	if o.Dir == "" {
		return nil, fmt.Errorf("%w: the orca backend needs a folder", agentvisor.ErrInvalid)
	}
	if o.CLI == nil {
		exe, ok := FindCLI()
		if !ok {
			return nil, fmt.Errorf("%w: no orca CLI", agentvisor.ErrUnsupported)
		}
		o.CLI = NewCLI(exe, nil)
	}
	o.Poll = cmp.Or(o.Poll, defaultPoll)
	o.ShellWait = cmp.Or(o.ShellWait, 300*time.Millisecond)
	o.Logger = cmp.Or(o.Logger, slog.New(slog.DiscardHandler))
	if err := os.MkdirAll(filepath.Join(o.Dir, "launch"), 0o700); err != nil {
		return nil, fmt.Errorf("make the orca folder: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Backend{
		cli: o.CLI, opts: o, log: o.Logger.With("source", "orca"),
		handles: map[string]*handle{}, reported: map[string]bool{},
		found: make(chan agentvisor.Found, 16), wake: make(chan struct{}, 1),
		cancel: cancel, done: make(chan struct{}),
	}
	go b.poll(ctx)
	return b, nil
}

// CLI gives the Orca CLI of the backend.
func (b *Backend) CLI() *CLI { return b.cli }

// Snapshot gives the Orca state of the last read. A consumer can read it in
// place of a second poll of Orca.
func (b *Backend) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

func (b *Backend) Kind() agentvisor.Kind { return Kind }

// Caps has no typed events and no structured permissions: the backend
// answers a prompt through the screen.
func (b *Backend) Caps(string) agentvisor.Caps {
	return agentvisor.Caps{Survives: true, Resume: b.opts.Command != nil}
}

// Start opens a terminal in Orca and runs the agent command in it. Then it
// waits until the agent is idle and sends the first prompt.
func (b *Backend) Start(ctx context.Context, l agentvisor.Launch) (agentvisor.Handle, error) {
	argv, err := b.command(l)
	if err != nil {
		return nil, err
	}
	script, err := b.writeScript(l.Session.ID, l.Dir, l.Env, argv)
	if err != nil {
		return nil, err
	}
	command := "sh " + quote(script)
	labels := l.Spec.Labels
	var t Terminal
	if shell := labels[LabelShell]; shell != "" {
		if err := b.cli.Line(ctx, shell, command); err != nil {
			_ = os.Remove(script) // the script did not run
			return nil, err
		}
		t, err = b.cli.Show(ctx, shell)
		t.Handle = shell
	} else {
		where := "path:" + l.Dir
		if w := labels[LabelWorktree]; w != "" {
			where = "id:" + w
		}
		t, err = b.cli.CreateTerminal(ctx, where, cmp.Or(l.Spec.Title, l.Session.Title), command)
	}
	if err != nil {
		_ = os.Remove(script) // the script did not run
		return nil, err
	}
	h := b.bind(l.Session.ID, binding{Terminal: t.Handle, Pane: t.PaneKey(), Worktree: cmp.Or(t.WorktreeID, labels[LabelWorktree])}, l.Prompt == nil)
	if l.Prompt != nil {
		if err := h.firstPrompt(ctx, l.Prompt.Text); err != nil {
			_ = h.Close(context.WithoutCancel(ctx)) // the prompt error is the one to report
			return nil, err
		}
	}
	return h, nil
}

// Recover gives a handle for each stored binding. The poll ends the handles
// whose terminal is gone.
func (b *Backend) Recover(context.Context) ([]agentvisor.Handle, error) {
	entries, err := os.ReadDir(b.opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("read the orca folder: %w", err)
	}
	var out []agentvisor.Handle
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(b.opts.Dir, e.Name()))
		var bd binding
		if err == nil {
			err = json.Unmarshal(data, &bd)
		}
		if err != nil {
			b.log.Warn("read a session binding", "session_id", id, "err", err)
			continue
		}
		out = append(out, b.bind(id, bd, false))
	}
	return out, nil
}

// Close stops the poll and detaches each handle. The terminals keep running.
func (b *Backend) Close(context.Context) error {
	b.cancel()
	<-b.done
	return nil
}

// Found gives the agents that the visor did not start, when Options.Discover
// is set.
func (b *Backend) Found() <-chan agentvisor.Found { return b.found }

// Adopt binds the found pane to the visor session s.
func (b *Backend) Adopt(_ context.Context, s agentvisor.Session, pane string) (agentvisor.Handle, error) {
	b.mu.Lock()
	delete(b.reported, pane)
	for _, h := range b.handles {
		if h.binding().Pane == pane {
			b.mu.Unlock()
			return nil, fmt.Errorf("%w: pane %s has a session", agentvisor.ErrState, pane)
		}
	}
	b.mu.Unlock()
	return b.bind(s.ID, binding{Pane: pane, Terminal: s.Labels[LabelTerminal], Worktree: s.Labels[LabelWorktree]}, false), nil
}

// Folders gives the repo paths of Orca.
func (b *Backend) Folders(ctx context.Context) ([]string, error) {
	repos, err := b.cli.Repos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if filepath.IsAbs(r.Path) {
			out = append(out, r.Path)
		}
	}
	return out, nil
}

// Binding gives the Orca terminal of a session.
func (b *Backend) Binding(session string) (pane, terminal, worktree string, ok bool) {
	b.mu.Lock()
	h, ok := b.handles[session]
	b.mu.Unlock()
	if !ok {
		return "", "", "", false
	}
	bd := h.binding()
	return bd.Pane, bd.Terminal, bd.Worktree, true
}

// binding is the Orca terminal of a session. The backend keeps it in
// <dir>/<session>.json.
type binding struct {
	Pane     string `json:"pane,omitempty"`
	Terminal string `json:"terminal,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

// bind tracks a new handle and stores its binding. A fresh handle with no
// prompt is idle.
func (b *Backend) bind(id string, bd binding, idle bool) *handle {
	h := &handle{b: b, id: id, bd: bd, events: make(chan agentvisor.Event, 32), idle: idle, born: time.Now()}
	b.mu.Lock()
	b.handles[id] = h
	b.mu.Unlock()
	b.store(id, bd)
	b.poke()
	return h
}

func (b *Backend) store(id string, bd binding) {
	data, err := json.Marshal(bd)
	if err == nil {
		err = os.WriteFile(filepath.Join(b.opts.Dir, id+".json"), data, 0o600)
	}
	if err != nil {
		b.log.Warn("store a session binding", "session_id", id, "err", err)
	}
}

// poke asks for a poll now.
func (b *Backend) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// quote puts s in single quotes for sh.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func envName(k string) bool {
	if k == "" || unicode.IsDigit(rune(k[0])) {
		return false
	}
	return !strings.ContainsFunc(k, func(r rune) bool {
		return r != '_' && (r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r))
	})
}

// command gives the argv of the session, as the pty backend does.
func (b *Backend) command(l agentvisor.Launch) ([]string, error) {
	switch l.Spec.Role {
	case agentvisor.RoleShell:
		if len(l.Spec.Command) > 0 {
			return l.Spec.Command, nil
		}
		return []string{cmp.Or(os.Getenv("SHELL"), "/bin/sh"), "-l"}, nil
	case agentvisor.RoleScript:
		if len(l.Spec.Command) == 0 {
			return nil, fmt.Errorf("%w: a script session needs a command", agentvisor.ErrInvalid)
		}
		return l.Spec.Command, nil
	}
	if b.opts.Command != nil {
		return b.opts.Command(l)
	}
	if l.Resume {
		return nil, fmt.Errorf("%w: no resume command for harness %s", agentvisor.ErrUnsupported, l.Spec.Harness)
	}
	if len(l.Spec.Command) > 0 {
		return l.Spec.Command, nil
	}
	argv := []string{cmp.Or(l.Binary, l.Spec.Harness)}
	argv = append(argv, agentwire.ModelArgs(agentwire.Harness(l.Spec.Harness), l.Spec.Model, l.Spec.Effort)...)
	return append(argv, l.Spec.ExtraArgs...), nil
}

// writeScript writes the start script of a session with mode 0700. The env
// goes in the script, so that no secret shows in the terminal or in the
// shell history. The script removes itself when it starts.
func (b *Backend) writeScript(id, dir string, env map[string]string, argv []string) (string, error) {
	var s strings.Builder
	s.WriteString("#!/bin/sh\nrm -f -- \"$0\"\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if !envName(k) {
			return "", fmt.Errorf("%w: env name %q", agentvisor.ErrInvalid, k)
		}
		fmt.Fprintf(&s, "export %s=%s\n", k, quote(env[k]))
	}
	if dir != "" {
		fmt.Fprintf(&s, "cd %s || exit 1\n", quote(dir))
	}
	s.WriteString("exec")
	for _, a := range argv {
		s.WriteString(" " + quote(a))
	}
	s.WriteString("\n")
	path := filepath.Join(b.opts.Dir, "launch", id+".sh")
	if err := os.WriteFile(path, []byte(s.String()), 0o700); err != nil {
		return "", fmt.Errorf("write the start script: %w", err)
	}
	return path, nil
}
