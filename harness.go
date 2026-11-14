package agentvisor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/trebi-ai/agent-visor/harness"
)

// Harness is one vendor agent CLI on this machine.
type Harness = harness.Harness

// HarnessOptions configures the harness inventory.
type HarnessOptions = harness.Options

// HarnessNodeOptions sets the private Node runtime for npm installs.
type HarnessNodeOptions = harness.NodeOptions

// HarnessFacts are the vendor facts that the inventory reads, for tests.
type HarnessFacts = harness.Facts

// HarnessSource tells where a harness binary comes from.
type HarnessSource = harness.Source

// AuthMode tells which login a harness uses.
type AuthMode = harness.AuthMode

// AuthState is the login state of a harness.
type AuthState = harness.AuthState

// Login is the state of a vendor login.
type Login = harness.Login

// Model is one model that a harness offers.
type Model = harness.Model

// LoginSpec asks for a login.
type LoginSpec = harness.LoginSpec

// LoginPhase is the step of a login.
type LoginPhase = harness.LoginPhase

// UpdateMode tells whether the visor updates a harness.
type UpdateMode = harness.UpdateMode

// HarnessUpdate is one version change of a harness.
type HarnessUpdate = harness.Update

// HarnessPolicy is the product policy for the harness homes.
type HarnessPolicy = harness.Policy

// HomeFile is one file or folder of a harness home that can share the
// user's own setup.
type HomeFile = harness.HomeFile

// FileContent is the content of one side of a home file.
type FileContent = harness.FileContent

// OwnStart is the first content of an own copy.
type OwnStart = harness.OwnStart

// Limits is the plan limit state of the account behind a harness.
type Limits = harness.Limits

// LimitWindow is the use of one plan limit window.
type LimitWindow = harness.LimitWindow

// Auth modes, harness sources, and auth states.
const (
	AuthShared   = harness.AuthShared
	AuthSeparate = harness.AuthSeparate
	SourceVisor  = harness.SourceVisor
	SourceSystem = harness.SourceSystem
	AuthUnknown  = harness.AuthUnknown
	AuthOK       = harness.AuthOK
	AuthMissing  = harness.AuthMissing
	AuthExpired  = harness.AuthExpired

	LoginStarting  = harness.LoginStarting
	LoginBrowser   = harness.LoginBrowser
	LoginCode      = harness.LoginCode
	LoginVerifying = harness.LoginVerifying
	LoginDone      = harness.LoginDone
	LoginFailed    = harness.LoginFailed

	UpdatesAuto = harness.UpdatesAuto
	UpdatesKeep = harness.UpdatesKeep

	SideUser      = harness.SideUser
	SideHome      = harness.SideHome
	OwnFromUser   = harness.OwnFromUser
	OwnFromEmpty  = harness.OwnFromEmpty
	OwnFromBackup = harness.OwnFromBackup
)

// loginLabel marks the terminal session of a login. Its value is the
// harness.
const loginLabel = reservedLabelPrefix + "login"

// Harnesses lists the harnesses that the visor knows.
func (v *Visor) Harnesses(ctx context.Context) ([]Harness, error) {
	if v.isClosed() {
		return nil, ErrClosed
	}
	out, err := v.harnesses.List(ctx)
	return out, mapHarnessErr(err)
}

// Harness reads one harness.
func (v *Visor) Harness(ctx context.Context, name string) (Harness, error) {
	if v.isClosed() {
		return Harness{}, ErrClosed
	}
	out, err := v.harnesses.Harness(ctx, name)
	return out, mapHarnessErr(err)
}

// InstallHarness installs a visor copy of a harness. An empty version is
// the latest release. It returns ErrBusy when a live session uses the
// harness.
func (v *Visor) InstallHarness(ctx context.Context, name, version string) (Harness, error) {
	if v.isClosed() {
		return Harness{}, ErrClosed
	}
	out, err := v.harnesses.Install(ctx, name, version)
	return out, mapHarnessErr(err)
}

// UpdateHarness updates a harness. It returns ErrBusy when a live session
// uses the harness.
func (v *Visor) UpdateHarness(ctx context.Context, name, version string) (Harness, error) {
	if v.isClosed() {
		return Harness{}, ErrClosed
	}
	out, err := v.harnesses.Update(ctx, name, version)
	return out, mapHarnessErr(err)
}

// SetHarnessAuth moves a harness to a shared or a separate login.
func (v *Visor) SetHarnessAuth(ctx context.Context, name string, mode AuthMode) (Harness, error) {
	if v.isClosed() {
		return Harness{}, ErrClosed
	}
	out, err := v.harnesses.SetAuthMode(ctx, name, mode)
	return out, mapHarnessErr(err)
}

// SetHarnessUpdates sets the update mode of a harness. Keep mode holds a
// version: an empty version is the current one.
func (v *Visor) SetHarnessUpdates(ctx context.Context, name string, mode UpdateMode, version string) (Harness, error) {
	if v.isClosed() {
		return Harness{}, ErrClosed
	}
	out, err := v.harnesses.SetUpdates(ctx, name, mode, version)
	return out, mapHarnessErr(err)
}

// CheckHarnesses looks for new releases now and applies the update modes.
func (v *Visor) CheckHarnesses(ctx context.Context) error {
	if v.isClosed() {
		return ErrClosed
	}
	v.harnesses.Check(ctx)
	return nil
}

// LoginHarness starts the vendor login and gives the URL or the code. A
// terminal login runs in a session with the label visor.login.
func (v *Visor) LoginHarness(ctx context.Context, name string, spec LoginSpec) (Login, error) {
	if v.isClosed() {
		return Login{}, ErrClosed
	}
	out, err := v.harnesses.Login(ctx, name, spec)
	return out, mapHarnessErr(err)
}

// HarnessLogin gives the login that runs, or the last one for a minute
// after its end.
func (v *Visor) HarnessLogin(ctx context.Context, name string) (Login, error) {
	if v.isClosed() {
		return Login{}, ErrClosed
	}
	if _, err := v.harnesses.Harness(ctx, name); err != nil {
		return Login{}, mapHarnessErr(err)
	}
	out, ok := v.harnesses.LoginState(name)
	if !ok {
		return Login{}, fmt.Errorf("%w: no login of %s", ErrNotFound, name)
	}
	return out, nil
}

// HarnessLoginCode types a pasted code into a terminal login.
func (v *Visor) HarnessLoginCode(ctx context.Context, name, code string) (Login, error) {
	if v.isClosed() {
		return Login{}, ErrClosed
	}
	out, err := v.harnesses.LoginCode(ctx, name, code)
	return out, mapHarnessErr(err)
}

// HarnessLoginToken keeps a token or an API key as the login of a harness.
func (v *Visor) HarnessLoginToken(ctx context.Context, name, token string) (Harness, error) {
	if v.isClosed() {
		return Harness{}, ErrClosed
	}
	out, err := v.harnesses.LoginToken(ctx, name, token)
	return out, mapHarnessErr(err)
}

// CancelHarnessLogin stops a running login.
func (v *Visor) CancelHarnessLogin(ctx context.Context, name string) error {
	if v.isClosed() {
		return ErrClosed
	}
	if _, err := v.harnesses.Harness(ctx, name); err != nil {
		return mapHarnessErr(err)
	}
	v.harnesses.CancelLogin(ctx, name)
	return nil
}

// LogoutHarness logs out of a harness.
func (v *Visor) LogoutHarness(ctx context.Context, name string) error {
	if v.isClosed() {
		return ErrClosed
	}
	return mapHarnessErr(v.harnesses.Logout(ctx, name))
}

// HarnessModels lists the models of a harness.
func (v *Visor) HarnessModels(ctx context.Context, name string) ([]Model, error) {
	if v.isClosed() {
		return nil, ErrClosed
	}
	out, err := v.harnesses.Models(ctx, name)
	return out, mapHarnessErr(err)
}

// SetHarnessEnv replaces the env of a harness. Each session of the harness
// gets it, under the session env. An empty env removes it.
func (v *Visor) SetHarnessEnv(ctx context.Context, name string, env map[string]string) error {
	if v.isClosed() {
		return ErrClosed
	}
	return mapHarnessErr(v.harnesses.SetEnv(ctx, name, env))
}

// HarnessFiles lists the home files of a harness and their share state.
func (v *Visor) HarnessFiles(ctx context.Context, name string) ([]HomeFile, error) {
	if v.isClosed() {
		return nil, ErrClosed
	}
	out, err := v.harnesses.Files(ctx, name)
	return out, mapHarnessErr(err)
}

// ShareHarnessFile makes the harness home use the user file. An own copy
// goes to a backup.
func (v *Visor) ShareHarnessFile(ctx context.Context, name, key string) (HomeFile, error) {
	if v.isClosed() {
		return HomeFile{}, ErrClosed
	}
	out, err := v.harnesses.ShareFile(ctx, name, key)
	return out, mapHarnessErr(err)
}

// OwnHarnessFile gives the harness home its own copy of a file.
func (v *Visor) OwnHarnessFile(ctx context.Context, name, key string, start OwnStart) (HomeFile, error) {
	if v.isClosed() {
		return HomeFile{}, ErrClosed
	}
	out, err := v.harnesses.OwnFile(ctx, name, key, start)
	return out, mapHarnessErr(err)
}

// ReadHarnessFile reads one side of a home file. An empty side is the one
// that a session uses.
func (v *Visor) ReadHarnessFile(ctx context.Context, name, key, side string) (FileContent, error) {
	if v.isClosed() {
		return FileContent{}, ErrClosed
	}
	out, err := v.harnesses.ReadFile(ctx, name, key, side)
	return out, mapHarnessErr(err)
}

// WriteHarnessFile replaces the own copy of a home file. An empty ifMatch
// skips the etag check. An old etag gives ErrStale.
func (v *Visor) WriteHarnessFile(ctx context.Context, name, key, content, ifMatch string) (FileContent, error) {
	if v.isClosed() {
		return FileContent{}, ErrClosed
	}
	out, err := v.harnesses.WriteFile(ctx, name, key, content, ifMatch)
	return out, mapHarnessErr(err)
}

// harnessOptions are the product options plus the visor wiring.
func (v *Visor) harnessOptions() HarnessOptions {
	o := v.opts.Harness
	o.InUse, o.OnChange, o.Logger = v.harnessInUse, v.onHarness, v.opts.Logger
	o.Terminal = loginTerminal{v}
	return o
}

// loginTerminal runs the harness logins in PTY sessions.
type loginTerminal struct{ v *Visor }

func (t loginTerminal) Start(ctx context.Context, name string, argv []string, dir string, env map[string]string) (string, error) {
	s, err := t.v.start(ctx, StartSpec{
		Kind: KindPTY, Role: RoleScript, Title: "Log in to " + name, WorkingDir: dir, Command: argv, Env: env,
		Labels: map[string]string{loginLabel: name},
	})
	return s.ID, err
}

func (t loginTerminal) Screen(ctx context.Context, id string) (string, bool, error) {
	if sc, err := t.v.Screen(ctx, id); err == nil {
		return strings.Join(sc.Lines, "\n"), true, nil
	}
	out, err := t.v.Output(ctx, id)
	return out, false, err
}

func (t loginTerminal) Send(ctx context.Context, id, text string) error {
	return t.v.Keys(ctx, id, []byte(text))
}

func (t loginTerminal) Stop(ctx context.Context, id string) error {
	return t.v.Stop(ctx, id)
}

// harnessInUse reports whether a live session uses the harness.
func (v *Visor) harnessInUse(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, rec := range v.sessions {
		if rec.s.Harness == name && rec.s.State.Live() {
			return true
		}
	}
	return false
}

func (v *Visor) onHarness(name string) {
	if _, err := v.events.append(v.ctx, Event{Type: EventHarnessChanged, Harness: name}); err != nil {
		v.log.Warn("append harness event", "harness", name, "err", err)
	}
}

func mapHarnessErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, harness.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, harness.ErrInvalid):
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	case errors.Is(err, harness.ErrState):
		return fmt.Errorf("%w: %w", ErrState, err)
	case errors.Is(err, harness.ErrBusy):
		return fmt.Errorf("%w: %w", ErrBusy, err)
	case errors.Is(err, harness.ErrStale):
		return fmt.Errorf("%w: %w", ErrStale, err)
	case errors.Is(err, harness.ErrNoBackup):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, harness.ErrDownload):
		return fmt.Errorf("%w: %w", ErrDownload, err)
	case errors.Is(err, harness.ErrUnsupported):
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	default:
		return err
	}
}
