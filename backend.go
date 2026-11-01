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

// TextTerminal is a handle whose terminal the backend shows only as text,
// for example a terminal in another app. Screen, Keys, Paste, and Output use
// it.
type TextTerminal interface {
	Screen(ctx context.Context) (Screen, error)
	Keys(ctx context.Context, data []byte) error
	Paste(ctx context.Context, text string) error
	// Output gives the end of the terminal output as plain text.
	Output(ctx context.Context) (string, error)
}

// Labeler is a handle that adds labels to its session, for example the
// terminal of another app. The visor reads them at attach and after each
// init event.
type Labeler interface {
	Labels() map[string]string
}

// Found is a session that a backend found and the visor did not start, for
// example an agent that the user started in another app.
type Found struct {
	// Key names the session in the backend.
	Key     string
	Harness string
	// Dir is the absolute working folder.
	Dir    string
	Title  string
	Labels map[string]string
}

// Discoverer is a backend that finds sessions that it did not start. The
// visor makes a session for each one and adopts it. An observed session with
// the same labels becomes the adopted session.
type Discoverer interface {
	// Found gives the new sessions. It closes when the backend closes.
	Found() <-chan Found
	// Adopt binds the found session with key to the visor session s.
	Adopt(ctx context.Context, s Session, key string) (Handle, error)
}

// Catalog is a backend that knows project folders, for example the repos of
// another app. The visor makes a project for each folder at boot.
type Catalog interface {
	Folders(ctx context.Context) ([]string, error)
}
