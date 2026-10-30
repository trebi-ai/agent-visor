package agentvisor

import "context"

// Caps tells what a backend can do for one harness.
type Caps struct {
	// Structured means typed events: tools, requests, and results.
	Structured bool `json:"structured"`
	// Terminal means a terminal view that a client can attach to.
	Terminal bool `json:"terminal"`
	// Permissions means typed permission requests and answers.
	Permissions bool `json:"permissions"`
	// Steer means a send during a turn joins the turn.
	Steer bool `json:"steer"`
	// Resume means a stopped session can continue with its vendor id.
	Resume bool `json:"resume"`
	// Survives means the session lives through a visor restart.
	Survives bool `json:"survives"`
}

// Launch is what a backend needs to start one session. The visor resolves
// the workdir, the env layers, and the harness program first.
type Launch struct {
	Session Session
	Spec    StartSpec
	// Dir is the working folder.
	Dir string
	// Env is the merged env: visor, harness, session, and the visor values.
	Env map[string]string
	// Binary is the harness program. Empty searches PATH.
	Binary string
	// Home is the harness home for a separate login. Empty uses the login of
	// the user.
	Home string
	// Resume continues the vendor session in Session.VendorID.
	Resume bool
	// Prompt is the first turn, or nil.
	Prompt *Input
}

// Backend hosts sessions of one kind. A product can register more backends
// in Options.Backends.
type Backend interface {
	Kind() Kind
	Caps(harness string) Caps
	Start(ctx context.Context, l Launch) (Handle, error)
	// Recover gives the sessions that lived through a visor restart.
	Recover(ctx context.Context) ([]Handle, error)
	Close(ctx context.Context) error
}

// Handle is one live session in a backend. Events closes when the session
// ends.
type Handle interface {
	Session() string
	VendorID() string
	PID() int
	Send(ctx context.Context, in Input) (Delivery, error)
	Interrupt(ctx context.Context) error
	Answer(ctx context.Context, requestID string, a Answer) error
	Close(ctx context.Context) error
	Events() <-chan Event
}
