package agentvisor

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/workdir"
)

// Kind is the kind of a session: the backend that hosts it.
type Kind string

// Role is what a session does.
type Role string

// Project is a repository root or a plain folder.
type Project = workdir.Project

// Workdir is one folder to work in, and for git one branch.
type Workdir = workdir.Workdir

// WorkdirSpec asks for a workdir.
type WorkdirSpec = workdir.Spec

// WorkdirKind is the kind of a workdir.
type WorkdirKind = workdir.Kind

// WorkdirState is the state of a workdir.
type WorkdirState = workdir.State

// RestartMode tells what the visor does with a session that it lost.
type RestartMode string

// RestartPolicy applies when the visor finds a session lost at boot.
type RestartPolicy struct {
	Mode RestartMode `json:"mode,omitempty"`
	// Prompt is the first turn after a resume.
	Prompt string `json:"prompt,omitempty"`
}

// StartSpec asks for a new session. The empty value starts a shell in the
// working folder of the visor process.
type StartSpec struct {
	// Kind is empty for wire when the harness has a driver, else pty.
	Kind Kind `json:"kind,omitempty"`
	// Role is empty for agent, or shell when there is no harness.
	Role    Role   `json:"role,omitempty"`
	Title   string `json:"title,omitempty"`
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	// Binary is the harness program. Empty uses the visor install, else
	// PATH.
	Binary string `json:"binary,omitempty"`
	// Workdir is a workdir id. Empty finds or makes it from WorkingDir.
	Workdir string `json:"workdir,omitempty"`
	// WorkingDir is a folder in the workdir. Empty is the workdir path.
	WorkingDir string `json:"working_dir,omitempty"`
	// Command is the shell or script command. Empty is the login shell.
	Command []string `json:"command,omitempty"`
	// Env wins over the harness env and the visor env.
	Env          map[string]string `json:"env,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	// Prompt is the first turn. Empty starts the session idle.
	Prompt      string      `json:"prompt,omitempty"`
	Permissions Permissions `json:"permissions"`
	MCPServers  []MCPServer `json:"mcp_servers,omitempty"`
	Skills      []Skill     `json:"skills,omitempty"`
	ExtraArgs   []string    `json:"extra_args,omitempty"`
	// ResumeID is a vendor session id to resume.
	ResumeID string            `json:"resume_id,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Restart  RestartPolicy     `json:"restart"`
	Raw      map[string]any    `json:"raw,omitempty"`
}

// Permissions is the permission policy of a session.
type Permissions struct {
	// Mode is ask, auto_edit, auto, or inherit. Empty is ask.
	Mode agentwire.PermissionMode `json:"mode,omitempty"`
	// AllowTools are tools that need no answer. A pattern can end in "*".
	AllowTools []string `json:"allow_tools,omitempty"`
}

// MCPServer is one MCP server for a session. Command selects a stdio server;
// URL selects an HTTP server.
type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Skill is one extra skill for a session.
type Skill struct {
	// Name replaces the name in the SKILL.md front matter.
	Name string `json:"name,omitempty"`
	// Dir is the folder that holds SKILL.md.
	Dir string `json:"dir"`
}

// Attachment is a file or an image that goes with a turn. Data wins over
// Path.
type Attachment struct {
	MIME string `json:"mime"`
	Path string `json:"path,omitempty"`
	Data []byte `json:"data,omitempty"`
}

// Input is one user turn.
type Input struct {
	Text string `json:"text"`
	// Kind is an opaque origin tag that the visor passes to the harness.
	Kind        string       `json:"kind,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Delivery tells how a send reached the session.
type Delivery string

// Deliveries. The values are the agent-wire values.
const (
	// DeliveryPrompted means that a new turn starts, or that the vendor
	// queues it.
	DeliveryPrompted Delivery = Delivery(agentwire.DeliveryPrompted)
	// DeliverySteered means that the input joined the running turn.
	DeliverySteered Delivery = Delivery(agentwire.DeliverySteered)
	// DeliveryHeld means that the input starts a new turn when the running
	// turn ends.
	DeliveryHeld Delivery = Delivery(agentwire.DeliveryHeld)
)

// Answer decides an open request.
type Answer struct {
	Allow   bool   `json:"allow"`
	Message string `json:"message,omitempty"`
	// UpdatedInput replaces the tool input, when the harness takes it.
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
}

// Filter selects sessions, workdirs, projects, or events. The empty value
// selects all.
type Filter struct {
	Session string            `json:"session,omitempty"`
	Project string            `json:"project,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Key     string            `json:"key,omitempty"`
	Kind    Kind              `json:"kind,omitempty"`
	Role    Role              `json:"role,omitempty"`
	States  []State           `json:"states,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// Session kinds.
const (
	KindWire     Kind = "wire"
	KindPTY      Kind = "pty"
	KindObserved Kind = "observed"
)

// Session roles.
const (
	RoleAgent  Role = "agent"
	RoleShell  Role = "shell"
	RoleScript Role = "script"
)

// Workdir kinds and states.
const (
	WorkdirMain     = workdir.KindMain
	WorkdirWorktree = workdir.KindWorktree
	WorkdirFolder   = workdir.KindFolder
	WorkdirActive   = workdir.Active
	WorkdirArchived = workdir.Archived
)

// Restart modes.
const (
	RestartNotify RestartMode = "notify"
	RestartResume RestartMode = "resume"
)

// reservedLabelPrefix is the label namespace of the visor.
const reservedLabelPrefix = "visor."

// Validate checks the spec before a start.
func (s StartSpec) Validate() error {
	if s.Kind == KindObserved {
		return fmt.Errorf("%w: the visor does not start an observed session", ErrInvalid)
	}
	switch s.Role {
	case "", RoleAgent, RoleShell, RoleScript:
	default:
		return fmt.Errorf("%w: role %q", ErrInvalid, s.Role)
	}
	if s.Role == RoleAgent && s.Harness == "" {
		return fmt.Errorf("%w: an agent session needs a harness", ErrInvalid)
	}
	if s.Kind == KindWire && s.Harness == "" {
		return fmt.Errorf("%w: a wire session needs a harness", ErrInvalid)
	}
	for k := range s.Labels {
		if strings.HasPrefix(k, reservedLabelPrefix) {
			return fmt.Errorf("%w: label %s uses the reserved prefix %s", ErrInvalid, k, reservedLabelPrefix)
		}
	}
	switch s.Restart.Mode {
	case "", RestartNotify, RestartResume:
	default:
		return fmt.Errorf("%w: restart mode %q", ErrInvalid, s.Restart.Mode)
	}
	return nil
}

func (s StartSpec) withDefaults() StartSpec {
	if s.Role == "" {
		s.Role = RoleAgent
		if s.Harness == "" {
			s.Role = RoleShell
		}
	}
	if s.Kind == "" {
		s.Kind = KindWire
		if s.Harness == "" {
			s.Kind = KindPTY
		}
	}
	if s.Restart.Mode == "" {
		s.Restart.Mode = RestartNotify
	}
	s.Labels = maps.Clone(s.Labels)
	return s
}

// matchSession reports whether a session matches the filter.
func (f Filter) matchSession(s Session) bool {
	if (f.Session != "" && f.Session != s.ID) || (f.Workdir != "" && f.Workdir != s.Workdir) ||
		(f.Project != "" && f.Project != s.Project) || (f.Kind != "" && f.Kind != s.Kind) || (f.Role != "" && f.Role != s.Role) {
		return false
	}
	if len(f.States) > 0 && !containsState(f.States, s.State) {
		return false
	}
	return matchLabels(f.Labels, s.Labels)
}

func (f Filter) workdirFilter() workdir.Filter {
	return workdir.Filter{Project: f.Project, Key: f.Key}
}

// ParseLabel reads one k=v (or k:v) label selector.
func ParseLabel(s string) (string, string, error) {
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		k, v, ok = strings.Cut(s, ":")
	}
	if !ok || k == "" {
		return "", "", fmt.Errorf("%w: label %q is not key=value", ErrInvalid, s)
	}
	return k, v, nil
}

func matchLabels(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func containsState(states []State, s State) bool {
	for _, st := range states {
		if st == s {
			return true
		}
	}
	return false
}
