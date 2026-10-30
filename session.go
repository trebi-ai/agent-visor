package agentvisor

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// State is the state of a session.
type State string

// Session is one agent, shell, or script in a workdir.
type Session struct {
	ID         string            `json:"id"`
	Project    string            `json:"project"`
	Workdir    string            `json:"workdir"`
	Kind       Kind              `json:"kind"`
	Role       Role              `json:"role"`
	Title      string            `json:"title,omitempty"`
	Harness    string            `json:"harness,omitempty"`
	Model      string            `json:"model,omitempty"`
	Effort     string            `json:"effort,omitempty"`
	WorkingDir string            `json:"working_dir"`
	State      State             `json:"state"`
	VendorID   string            `json:"vendor_id,omitempty"`
	PID        int               `json:"pid,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	// Pending holds the open permissions and questions.
	Pending []Request     `json:"pending,omitempty"`
	Restart RestartPolicy `json:"restart"`
	// EnvKeys lists the session env keys. The values never leave the visor.
	EnvKeys []string `json:"env_keys,omitempty"`
	// Seq is the sequence number of the last event of the session.
	Seq       uint64    `json:"seq"`
	Turns     int       `json:"turns,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Session states.
const (
	StateStarting  State = "starting"
	StateRunning   State = "running"
	StateWaiting   State = "waiting"
	StateIdle      State = "idle"
	StateSuspended State = "suspended"
	StateLost      State = "lost"
	StateExited    State = "exited"
)

// moves is the state machine. A move to the same state is always allowed.
var moves = map[State][]State{ //nolint:gochecknoglobals // a constant table
	StateStarting:  {StateRunning, StateWaiting, StateIdle, StateSuspended, StateLost, StateExited},
	StateRunning:   {StateWaiting, StateIdle, StateSuspended, StateLost, StateExited},
	StateWaiting:   {StateRunning, StateIdle, StateSuspended, StateLost, StateExited},
	StateIdle:      {StateRunning, StateWaiting, StateSuspended, StateLost, StateExited},
	StateSuspended: {StateStarting, StateExited},
	StateLost:      {StateStarting, StateExited},
	StateExited:    {},
}

// Live reports whether a process can run for the session now.
func (s State) Live() bool {
	switch s {
	case StateStarting, StateRunning, StateWaiting, StateIdle:
		return true
	default:
		return false
	}
}

// Busy reports whether the session is in a turn.
func (s State) Busy() bool { return s == StateRunning || s == StateWaiting || s == StateStarting }

// CanMove reports whether the state machine allows the move.
func (s State) CanMove(to State) bool {
	return s == to || slices.Contains(moves[s], to)
}

// move changes the state, or returns ErrState.
func (s *Session) move(to State) error {
	if !s.State.CanMove(to) {
		return fmt.Errorf("%w: session %s cannot move from %s to %s", ErrState, s.ID, s.State, to)
	}
	s.State = to
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// resumable reports whether the session has what a resume needs.
func (s Session) resumable() bool {
	return s.Kind == KindWire && s.VendorID != ""
}

func (s Session) clone() Session {
	s.Pending = slices.Clone(s.Pending)
	s.EnvKeys = slices.Clone(s.EnvKeys)
	s.Labels = maps.Clone(s.Labels)
	return s
}
