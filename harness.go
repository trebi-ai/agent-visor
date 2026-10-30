package agentvisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/trebi-ai/agent-visor/harness"
)

// Harness is one vendor agent CLI on this machine.
type Harness = harness.Harness

// HarnessOptions configures the harness inventory.
type HarnessOptions = harness.Options

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
)

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

// LoginHarness starts the vendor login and gives the URL or the code.
func (v *Visor) LoginHarness(ctx context.Context, name string) (Login, error) {
	if v.isClosed() {
		return Login{}, ErrClosed
	}
	out, err := v.harnesses.Login(ctx, name)
	return out, mapHarnessErr(err)
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

// harnessOptions are the product options plus the visor wiring.
func (v *Visor) harnessOptions() HarnessOptions {
	o := v.opts.Harness
	o.InUse, o.OnChange, o.Logger = v.harnessInUse, v.onHarness, v.opts.Logger
	return o
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
	case errors.Is(err, harness.ErrUnsupported):
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	default:
		return err
	}
}
