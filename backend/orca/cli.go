package orca

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MacPath is the Orca CLI in the Orca app on macOS.
const MacPath = "/Applications/Orca.app/Contents/Resources/bin/orca"

// Timeouts of the CLI calls.
const (
	listTimeout = 15 * time.Second
	callTimeout = 20 * time.Second
	makeTimeout = 30 * time.Second
	// waitTimeout is the limit of one tui-idle wait. The CLI waits less.
	waitTimeout = 60 * time.Second
)

// Runner runs one child process.
type Runner interface {
	Run(ctx context.Context, exe string, args []string) (stdout, stderr []byte, err error)
}

// ExecRunner runs the process with os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, exe string, args []string) ([]byte, []byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// FindCLI gives the Orca CLI of the app on macOS, else orca on PATH. ok is
// false when there is none.
func FindCLI() (path string, ok bool) {
	if st, err := os.Stat(MacPath); err == nil && !st.IsDir() {
		return MacPath, true
	}
	p, err := exec.LookPath("orca")
	return p, err == nil
}

// Error is an error reply of the Orca CLI.
type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Message + " (" + e.Code + ")" }

// IsCode tells whether err is an Orca error with the code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// CLI calls the Orca CLI of this machine. Each call has an argument array and
// --json. No call has --environment: the CLI talks only to the local Orca.
type CLI struct {
	exe string
	run Runner
}

func NewCLI(exe string, run Runner) *CLI {
	if run == nil {
		run = ExecRunner{}
	}
	return &CLI{exe: exe, run: run}
}

// Worktree is one workspace of `orca worktree ps`.
type Worktree struct {
	ID          string  `json:"worktreeId"`
	RepoID      string  `json:"repoId"`
	Repo        string  `json:"repo"`
	Path        string  `json:"path"`
	Branch      string  `json:"branch"`
	DisplayName string  `json:"displayName"`
	Comment     string  `json:"comment"`
	IsArchived  bool    `json:"isArchived"`
	IsMain      bool    `json:"isMainWorktree"`
	Agents      []Agent `json:"agents"`
}

// BranchName is Branch without refs/heads/.
func (w Worktree) BranchName() string { return strings.TrimPrefix(w.Branch, "refs/heads/") }

// Agent is one agent pane of a workspace. The times are Unix milliseconds.
type Agent struct {
	PaneKey              string `json:"paneKey"`
	State                string `json:"state"`       // working, waiting, blocked, done
	WorkingMode          string `json:"workingMode"` // "monitoring" when only a background shell runs
	AgentType            string `json:"agentType"`
	Prompt               string `json:"prompt"`
	LastAssistantMessage string `json:"lastAssistantMessage"`
	ToolName             string `json:"toolName"`
	ToolInput            string `json:"toolInput"`
	Interrupted          bool   `json:"interrupted"`
	StateStartedAt       int64  `json:"stateStartedAt"`
	UpdatedAt            int64  `json:"updatedAt"`
	MainAgent            *struct {
		State          string `json:"state"`
		StateStartedAt int64  `json:"stateStartedAt"`
	} `json:"mainAgent"`
}

// The agent states of Orca.
const (
	AgentWorking = "working"
	AgentWaiting = "waiting"
	AgentBlocked = "blocked"
	AgentDone    = "done"
)

// Effective gives the agent with a stale monitoring state replaced by the
// state of the main agent. A real background shell starts in the turn, so its
// monitoring state starts before the main agent finishes. Orca can go to
// monitoring after the turn with no shell, for example on a recap.
func (a Agent) Effective() Agent {
	m := a.MainAgent
	if a.State != AgentWorking || a.WorkingMode != "monitoring" || m == nil || m.State != AgentDone || a.StateStartedAt <= m.StateStartedAt {
		return a
	}
	a.State, a.WorkingMode, a.StateStartedAt = m.State, "", m.StateStartedAt
	return a
}

// Terminal is one terminal of `orca terminal list`, `create`, or `show`.
type Terminal struct {
	Handle          string `json:"handle"`
	WorktreeID      string `json:"worktreeId"`
	WorktreePath    string `json:"worktreePath"`
	TabID           string `json:"tabId"`
	LeafID          string `json:"leafId"`
	Key             string `json:"paneKey"`
	RecordedPaneKey string `json:"recordedPaneKey"`
	Title           string `json:"title"`
	AgentIdentity   string `json:"agentIdentity"`
	Orphaned        bool   `json:"orphaned"`
	Connected       bool   `json:"connected"`
}

// PaneKey is the pane of the terminal (tabId:leafId). An orphaned terminal
// keeps its old pane in recordedPaneKey.
func (t Terminal) PaneKey() string {
	switch {
	case t.Key != "":
		return t.Key
	case t.RecordedPaneKey != "":
		return t.RecordedPaneKey
	case t.TabID == "" || t.LeafID == "":
		return ""
	}
	return t.TabID + ":" + t.LeafID
}

// Repo is one repo of `orca repo list`.
type Repo struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	DisplayName string `json:"displayName"`
	Kind        string `json:"kind"` // git or folder
}

// Worktrees lists the workspaces with their agents.
func (c *CLI) Worktrees(ctx context.Context) ([]Worktree, error) {
	var r struct {
		Worktrees []Worktree `json:"worktrees"`
	}
	err := c.callInto(ctx, listTimeout, &r, "worktree", "ps", "--limit", "500")
	return r.Worktrees, err
}

// Terminals lists the live terminals.
func (c *CLI) Terminals(ctx context.Context) ([]Terminal, error) {
	var r struct {
		Terminals []Terminal `json:"terminals"`
	}
	err := c.callInto(ctx, listTimeout, &r, "terminal", "list", "--limit", "500")
	return r.Terminals, err
}

// Repos lists the repos of Orca.
func (c *CLI) Repos(ctx context.Context) ([]Repo, error) {
	var r struct {
		Repos []Repo `json:"repos"`
	}
	err := c.callInto(ctx, listTimeout, &r, "repo", "list")
	return r.Repos, err
}

// Reachable tells whether `orca status` reaches the Orca runtime.
func (c *CLI) Reachable(ctx context.Context) bool {
	var r struct {
		Runtime struct {
			Reachable bool `json:"reachable"`
		} `json:"runtime"`
	}
	return c.callInto(ctx, listTimeout, &r, "status") == nil && r.Runtime.Reachable
}

// WorkspaceSpec names a workspace to find or make in a repo.
type WorkspaceSpec struct {
	// Repo is the path of the repo in Orca.
	Repo string
	Name string
	// BaseBranch and Comment apply to a new workspace.
	BaseBranch string
	Comment    string
}

// Workspace is a workspace that EnsureWorkspace found or made. Shell is the
// first shell of a new workspace, or empty.
type Workspace struct {
	ID    string
	Path  string
	Shell string
	New   bool
}

// EnsureWorkspace finds the workspace with the name in the repo, or makes it.
// A new workspace runs the Orca setup of the repo.
func (c *CLI) EnsureWorkspace(ctx context.Context, w WorkspaceSpec, shellWait time.Duration) (Workspace, error) {
	if w.Repo == "" || w.Name == "" {
		return Workspace{}, errors.New("a workspace needs a repo and a name")
	}
	repos, err := c.Repos(ctx)
	if err != nil {
		return Workspace{}, err
	}
	i := slices.IndexFunc(repos, func(r Repo) bool { return r.Path == w.Repo })
	if i < 0 {
		return Workspace{}, &Error{Code: "repo_not_found", Message: "Orca has no repo at " + w.Repo}
	}
	all, err := c.Worktrees(ctx)
	if err != nil {
		return Workspace{}, err
	}
	for _, wt := range all {
		if !wt.IsArchived && wt.RepoID == repos[i].ID && strings.EqualFold(wt.DisplayName, w.Name) {
			return Workspace{ID: wt.ID, Path: wt.Path}, nil
		}
	}
	args := []string{"worktree", "create", "--repo", "path:" + w.Repo, "--name", w.Name}
	if w.Comment != "" {
		args = append(args, "--comment", w.Comment)
	}
	if w.BaseBranch != "" {
		args = append(args, "--base-branch", w.BaseBranch)
	}
	var made struct {
		Worktree struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"worktree"`
	}
	if err := c.callInto(ctx, makeTimeout, &made, args...); err != nil {
		return Workspace{}, err
	}
	if made.Worktree.ID == "" {
		return Workspace{}, errors.New("orca made no workspace")
	}
	out := Workspace{ID: made.Worktree.ID, Path: made.Worktree.Path, New: true}
	if out.Path == "" {
		out.Path = pathOfID(out.ID)
	}
	out.Shell = c.firstShell(ctx, out.ID, shellWait)
	return out, nil
}

// pathOfID reads the path in a worktree id: <repo>::<path>[::workspace:<id>].
func pathOfID(id string) string {
	parts := strings.Split(id, "::")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// CreateTerminal opens a terminal in the worktree selector and runs command.
func (c *CLI) CreateTerminal(ctx context.Context, worktree, title, command string) (Terminal, error) {
	var made struct {
		Terminal Terminal `json:"terminal"`
	}
	args := []string{"terminal", "create", "--worktree", worktree, "--command", command}
	if title != "" {
		args = append(args, "--title", title)
	}
	if err := c.callInto(ctx, makeTimeout, &made, args...); err != nil {
		return Terminal{}, err
	}
	if made.Terminal.Handle == "" {
		return Terminal{}, errors.New("orca made no terminal")
	}
	return made.Terminal, nil
}

// Show gives the metadata of one terminal.
func (c *CLI) Show(ctx context.Context, handle string) (Terminal, error) {
	var r struct {
		Terminal Terminal `json:"terminal"`
	}
	err := c.callInto(ctx, callTimeout, &r, "terminal", "show", "--terminal", handle)
	return r.Terminal, err
}

// Close closes one terminal.
func (c *CLI) Close(ctx context.Context, handle string) error {
	_, err := c.call(ctx, callTimeout, "terminal", "close", "--terminal", handle)
	return err
}

// Type writes text to the terminal verbatim. Control bytes pass through.
func (c *CLI) Type(ctx context.Context, handle, text string) error {
	_, err := c.call(ctx, callTimeout, "terminal", "send", "--terminal", handle, "--text", text)
	return err
}

// Line types text and Enter. It does not wait for a prompt submit.
func (c *CLI) Line(ctx context.Context, handle, text string) error {
	_, err := c.call(ctx, callTimeout, "terminal", "send", "--terminal", handle, "--text", text, "--enter")
	return err
}

// Submit sends text as a prompt to the agent in the terminal. It is true when
// the agent started a turn on it.
func (c *CLI) Submit(ctx context.Context, handle, text string) (bool, error) {
	var r struct {
		Send struct {
			Prompt struct {
				Stages []string `json:"stages"`
			} `json:"prompt"`
		} `json:"send"`
	}
	if err := c.callInto(ctx, makeTimeout, &r, "terminal", "send", "--terminal", handle, "--text", text, "--enter", "--wait-submit", "15"); err != nil {
		return false, err
	}
	return slices.Contains(r.Send.Prompt.Stages, "turn_started"), nil
}

// WaitIdle waits until the agent TUI in the terminal is idle. It is false
// when the wait ended first.
func (c *CLI) WaitIdle(ctx context.Context, handle string) (bool, error) {
	var r struct {
		Wait struct {
			Satisfied bool `json:"satisfied"`
		} `json:"wait"`
	}
	err := c.callInto(ctx, waitTimeout, &r, "terminal", "wait", "--terminal", handle, "--for", "tui-idle", "--timeout-ms", "45000")
	return r.Wait.Satisfied, err
}

// Screen gives the rows that the terminal renders now.
func (c *CLI) Screen(ctx context.Context, handle string) ([]string, error) {
	return c.read(ctx, "--terminal", handle, "--screen")
}

// Tail gives the last lines of the terminal output, with no escape codes.
func (c *CLI) Tail(ctx context.Context, handle string, lines int) ([]string, error) {
	return c.read(ctx, "--terminal", handle, "--limit", strconv.Itoa(max(lines, 1)))
}

func (c *CLI) read(ctx context.Context, args ...string) ([]string, error) {
	var r struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	if err := c.callInto(ctx, callTimeout, &r, append([]string{"terminal", "read"}, args...)...); err != nil {
		return nil, err
	}
	out := make([]string, len(r.Terminal.Tail))
	for i, l := range r.Terminal.Tail {
		out[i] = strings.ReplaceAll(l, " ", " ")
	}
	return out, nil
}

func (c *CLI) callInto(ctx context.Context, timeout time.Duration, v any, args ...string) error {
	raw, err := c.call(ctx, timeout, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("orca %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

// call runs the CLI and gives the result object.
func (c *CLI) call(ctx context.Context, timeout time.Duration, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, runErr := c.run.Run(ctx, c.exe, append(slices.Clone(args), "--json"))
	command := strings.Join(args[:min(2, len(args))], " ")
	var reply struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout, &reply); err != nil {
		text := strings.TrimSpace(string(stderr))
		if text == "" && runErr != nil {
			text = runErr.Error()
		}
		return nil, fmt.Errorf("orca %s: %s", command, cmp.Or(text, "no JSON reply"))
	}
	if reply.OK {
		if len(reply.Result) == 0 {
			return json.RawMessage("{}"), nil
		}
		return reply.Result, nil
	}
	e := &Error{Code: "error", Message: "orca " + command + " failed"}
	if reply.Error != nil {
		e.Code, e.Message = cmp.Or(reply.Error.Code, e.Code), cmp.Or(reply.Error.Message, e.Message)
	}
	return nil, e
}

// firstShell waits for the first shell of a new workspace. It gives empty
// when none shows up in ten tries.
func (c *CLI) firstShell(ctx context.Context, worktree string, pause time.Duration) string {
	for i := range 10 {
		var list struct {
			Terminals []Terminal `json:"terminals"`
		}
		if c.callInto(ctx, callTimeout, &list, "terminal", "list", "--worktree", "id:"+worktree) == nil {
			for _, t := range list.Terminals {
				if t.Handle != "" && !t.Orphaned {
					return t.Handle
				}
			}
		}
		if i < 9 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(pause):
			}
		}
	}
	return ""
}
