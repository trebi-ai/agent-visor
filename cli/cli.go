// Package cli is the command tree of the visor. A product mounts it under its
// own binary with its own home, for example "<product> visor".
package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// Options configures the command tree.
type Options struct {
	// Name is the command name. Empty is "agent-visor".
	Name string
	// Usage is the one-line help. Empty uses a generic text.
	Usage string
	// Home is the default visor home. --home replaces it.
	Home string
	// Socket is the default socket. Empty is <home>/visor.sock.
	Socket string
	// Build is the build of this binary.
	Build  string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Configure changes the visor options of serve, for example to add env
	// or backends. It can be nil.
	Configure func(ctx context.Context, o *agentvisor.Options) error
	// Dial connects to the visor. Nil uses agentvisor.Dial.
	Dial func(ctx context.Context, socket string) (Visor, error)
}

// Visor is the visor API that the commands use. *agentvisor.Client and
// *agentvisor.Visor satisfy it.
type Visor interface {
	Info(ctx context.Context) (agentvisor.Info, error)
	Upgrade(ctx context.Context, spec agentvisor.UpgradeSpec) (agentvisor.Info, error)
	Start(ctx context.Context, spec agentvisor.StartSpec) (agentvisor.Session, error)
	Session(ctx context.Context, id string) (agentvisor.Session, error)
	Sessions(ctx context.Context, f agentvisor.Filter) ([]agentvisor.Session, error)
	Send(ctx context.Context, id string, in agentvisor.Input) (agentvisor.Delivery, error)
	Interrupt(ctx context.Context, id string) error
	Answer(ctx context.Context, id, requestID string, a agentvisor.Answer) error
	Stop(ctx context.Context, id string) error
	Events(ctx context.Context, f agentvisor.Filter, after uint64) (*agentvisor.EventStream, error)
	Projects(ctx context.Context, f agentvisor.Filter) ([]agentvisor.Project, error)
	ForgetProject(ctx context.Context, id string) error
	Workdirs(ctx context.Context, f agentvisor.Filter) ([]agentvisor.Workdir, error)
	EnsureWorkdir(ctx context.Context, w agentvisor.WorkdirSpec) (agentvisor.Workdir, error)
	ArchiveWorkdir(ctx context.Context, id string) error
	RestoreWorkdir(ctx context.Context, id string) (agentvisor.Workdir, error)
	RemoveWorkdir(ctx context.Context, id string, force bool) error
	Attach(ctx context.Context, id string, o agentvisor.AttachOptions) (*agentvisor.AttachStream, error)
	Handover(ctx context.Context, id string, to agentvisor.Kind) error
	Screen(ctx context.Context, id string) (agentvisor.Screen, error)
	Harnesses(ctx context.Context) ([]agentvisor.Harness, error)
	Harness(ctx context.Context, name string) (agentvisor.Harness, error)
	InstallHarness(ctx context.Context, name, version string) (agentvisor.Harness, error)
	UpdateHarness(ctx context.Context, name, version string) (agentvisor.Harness, error)
	SetHarnessAuth(ctx context.Context, name string, mode agentvisor.AuthMode) (agentvisor.Harness, error)
	SetHarnessUpdates(ctx context.Context, name string, mode agentvisor.UpdateMode, version string) (agentvisor.Harness, error)
	CheckHarnesses(ctx context.Context) error
	LoginHarness(ctx context.Context, name string, spec agentvisor.LoginSpec) (agentvisor.Login, error)
	HarnessLoginCode(ctx context.Context, name, code string) (agentvisor.Login, error)
	HarnessFiles(ctx context.Context, name string) ([]agentvisor.HomeFile, error)
	ShareHarnessFile(ctx context.Context, name, key string) (agentvisor.HomeFile, error)
	OwnHarnessFile(ctx context.Context, name, key string, start agentvisor.OwnStart) (agentvisor.HomeFile, error)
	LogoutHarness(ctx context.Context, name string) error
	HarnessModels(ctx context.Context, name string) ([]agentvisor.Model, error)
	Close(ctx context.Context) error
}

// app holds the options for the actions of one command tree.
type app struct {
	opts Options
}

var (
	_ Visor = (*agentvisor.Client)(nil)
	_ Visor = (*agentvisor.Visor)(nil)
)

// errNotAvailable marks a command whose API is not in this build.
var errNotAvailable = errors.New("not available yet")

// Command gives the command tree.
func Command(opts Options) *cli.Command {
	a := &app{opts: opts}
	a.opts.Name = cmp.Or(a.opts.Name, "agent-visor")
	a.opts.Usage = cmp.Or(a.opts.Usage, "Keep agent and terminal sessions alive on this machine")
	if a.opts.Stdin == nil {
		a.opts.Stdin = os.Stdin
	}
	if a.opts.Stdout == nil {
		a.opts.Stdout = os.Stdout
	}
	if a.opts.Stderr == nil {
		a.opts.Stderr = os.Stderr
	}
	if a.opts.Dial == nil {
		a.opts.Dial = func(ctx context.Context, socket string) (Visor, error) { return agentvisor.Dial(ctx, socket) }
	}
	return &cli.Command{
		Name:      a.opts.Name,
		Usage:     a.opts.Usage,
		Writer:    a.opts.Stdout,
		ErrWriter: a.opts.Stderr,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "home", Usage: "the visor home", Value: a.opts.Home},
			&cli.StringFlag{Name: "socket", Usage: "the visor socket (default <home>/visor.sock)"},
			&cli.BoolFlag{Name: "json", Usage: "print JSON"},
		},
		Commands: []*cli.Command{
			a.serveCommand(), a.statusCommand(), a.versionCommand(), a.upgradeCommand(),
			a.harnessCommand(), a.lsCommand(), a.projectCommand(), a.workdirCommand(), a.sessionCommand(),
			a.hookCommand(), a.holdCommand(),
		},
	}
}

// socket gives the socket from the flags: --socket, else <--home>/visor.sock,
// else the default.
func (a *app) socket(cmd *cli.Command) string {
	if s := cmd.String("socket"); s != "" {
		return s
	}
	if cmd.IsSet("home") || a.opts.Socket == "" {
		return filepath.Join(cmd.String("home"), "visor.sock")
	}
	return a.opts.Socket
}

// dial connects to the visor. The caller closes it.
func (a *app) dial(ctx context.Context, cmd *cli.Command) (Visor, error) {
	sock := a.socket(cmd)
	v, err := a.opts.Dial(ctx, sock)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return nil, fmt.Errorf("the visor does not run on %s; start it with %q", sock, a.fullName(cmd)+" serve")
	case err != nil:
		return nil, err
	}
	return v, nil
}

// fullName gives the full name of the root of this tree, with the parents
// of a product binary.
func (a *app) fullName(cmd *cli.Command) string {
	for _, c := range cmd.Lineage() {
		if c.Name == a.opts.Name {
			return c.FullName()
		}
	}
	return a.opts.Name
}

// with dials the visor, runs fn, and closes the connection.
func (a *app) with(ctx context.Context, cmd *cli.Command, fn func(v Visor) error) error {
	v, err := a.dial(ctx, cmd)
	if err != nil {
		return err
	}
	defer v.Close(context.WithoutCancel(ctx)) //nolint:errcheck // a client close has nothing to report
	return fn(v)
}

func (a *app) out(cmd *cli.Command) printer {
	return printer{w: a.opts.Stdout, json: cmd.Bool("json")}
}

// notAvailable is the action of a command whose API is not in this build.
func (a *app) notAvailable(_ context.Context, cmd *cli.Command) error {
	return fmt.Errorf("%s: %w", strings.TrimPrefix(cmd.FullName(), a.fullName(cmd)+" "), errNotAvailable)
}

// args checks the count of positional arguments.
func args(cmd *cli.Command, n int, names string) error {
	if cmd.NArg() != n {
		return fmt.Errorf("%w: %s needs %s", agentvisor.ErrInvalid, cmd.FullName(), names)
	}
	return nil
}
