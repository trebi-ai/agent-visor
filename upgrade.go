package agentvisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// UpgradeSpec names the visor build that replaces the running one.
type UpgradeSpec struct {
	// Binary is the program of the new build. The visor runs it with its
	// own arguments and env.
	Binary   string `json:"binary"`
	Build    string `json:"build,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// pendingUpgrade is an upgrade that waits until the visor is quiet, or
// until its deadline.
type pendingUpgrade struct {
	spec     UpgradeSpec
	deadline time.Time
}

const (
	defaultUpgradeWait = 24 * time.Hour
	// majorUpgradeWait is the wait when the protocol major changes, because
	// the products cannot talk to this visor.
	majorUpgradeWait   = 10 * time.Minute
	defaultUpgradePoll = 5 * time.Second
)

// Upgrade replaces the visor with spec.Binary when no session, attach
// client, login, or harness change needs it, or after Options.UpgradeWait.
// A session in a holder keeps running. A session in a turn that is not in
// a holder becomes lost at the deadline, and its restart policy applies in
// the next visor. Only a visor that ServeListener serves replaces itself; the
// upgrade waits for it.
func (v *Visor) Upgrade(ctx context.Context, spec UpgradeSpec) (Info, error) {
	if spec.Binary == "" {
		return Info{}, fmt.Errorf("%w: an upgrade needs a binary", ErrInvalid)
	}
	if st, err := os.Stat(spec.Binary); err != nil || st.IsDir() {
		return Info{}, fmt.Errorf("%w: upgrade binary %s is not a program", ErrInvalid, spec.Binary)
	}
	wait := v.opts.UpgradeWait
	if spec.Protocol != "" && protocolMajor(spec.Protocol) != protocolMajor(Protocol) {
		wait = min(wait, majorUpgradeWait)
	}
	deadline := v.opts.Now().Add(wait)
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return Info{}, ErrClosed
	}
	if p := v.upgrade; p != nil && p.deadline.Before(deadline) {
		deadline = p.deadline
	}
	v.upgrade = &pendingUpgrade{spec: spec, deadline: deadline}
	v.mu.Unlock()
	v.log.Info("an upgrade waits", "build", spec.Build, "deadline", deadline)
	v.checkUpgrade()
	return v.Info(ctx)
}

// upgradeLoop checks the pending upgrade until the visor closes.
func (v *Visor) upgradeLoop() {
	defer v.wg.Done()
	t := time.NewTicker(v.opts.upgradePoll)
	defer t.Stop()
	for {
		select {
		case <-v.quit:
			return
		case <-t.C:
			v.checkUpgrade()
		}
	}
}

// checkUpgrade starts the restart when an upgrade waits, a server runs, and
// the visor is quiet or the deadline passed.
func (v *Visor) checkUpgrade() {
	v.mu.Lock()
	p, fired, served := v.upgrade, v.restarting, v.serving > 0
	v.mu.Unlock()
	if p == nil || fired || !served {
		return
	}
	late := !v.opts.Now().Before(p.deadline)
	if !late && !v.quiet() {
		return
	}
	v.mu.Lock()
	if v.restarting || v.closed {
		v.mu.Unlock()
		return
	}
	v.restarting = true
	close(v.restartCh)
	v.mu.Unlock()
	v.log.Info("the visor restarts for an upgrade", "build", p.spec.Build, "deadline", late)
}

// quiet reports that no session in a turn needs this process, and no
// attach client, login, or harness change runs.
func (v *Visor) quiet() bool {
	if v.harnesses.Busy() {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.attached > 0 {
		return false
	}
	for _, rec := range v.sessions {
		if rec.h == nil {
			continue
		}
		if rec.s.State == StateStarting || (rec.s.State.Busy() && !v.caps(rec.s).Survives) {
			return false
		}
	}
	return true
}

// restartForUpgrade writes visor.restarting, closes the visor, and runs the
// new binary in this process. It returns only on an error.
func (v *Visor) restartForUpgrade(ctx context.Context) error {
	v.mu.Lock()
	spec := v.upgrade.spec
	v.mu.Unlock()
	if err := v.Close(ctx); err != nil {
		v.log.Warn("close the visor for an upgrade", "err", err)
	}
	args := append([]string{spec.Binary}, os.Args[1:]...)
	if err := v.opts.Exec(spec.Binary, args, os.Environ()); err != nil {
		return fmt.Errorf("run the upgrade %s: %w", spec.Binary, err)
	}
	return nil
}

// serve counts a server of v while it runs.
func (v *Visor) serve() func() {
	v.mu.Lock()
	v.serving++
	v.mu.Unlock()
	return func() {
		v.mu.Lock()
		v.serving--
		v.mu.Unlock()
	}
}

// announceRestart writes the visor.restarting event. A client reads it,
// connects again, and continues after its last seq.
func (v *Visor) announceRestart(ctx context.Context) {
	v.mu.Lock()
	spec := v.upgrade.spec
	v.mu.Unlock()
	data, err := json.Marshal(map[string]string{"build": spec.Build})
	if err != nil {
		return
	}
	if _, err := v.events.append(ctx, Event{Type: EventRestarting, Data: data}); err != nil {
		v.log.Warn("append the restart event", "err", err)
	}
}

func (v *Visor) upgradeBuild() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.upgrade == nil {
		return ""
	}
	if v.upgrade.spec.Build == "" {
		return "unknown"
	}
	return v.upgrade.spec.Build
}

// newerBuild reports whether build a is newer than build b. Two semantic
// versions compare by number; other builds are newer when they differ.
func newerBuild(a, b string) bool {
	if a == "" || a == b {
		return false
	}
	va, oka := parseSemver(a)
	vb, okb := parseSemver(b)
	if !oka || !okb {
		return true
	}
	for i := range 3 {
		if va.n[i] != vb.n[i] {
			return va.n[i] > vb.n[i]
		}
	}
	switch {
	case va.pre == vb.pre:
		return false
	case va.pre == "":
		return true
	case vb.pre == "":
		return false
	}
	return va.pre > vb.pre
}

type semver struct {
	n   [3]int
	pre string
}

// parseSemver reads vMAJOR.MINOR.PATCH with an optional pre-release. It
// drops build metadata.
func parseSemver(s string) (semver, bool) {
	var out semver
	s, ok := strings.CutPrefix(s, "v")
	if !ok {
		return out, false
	}
	s, _, _ = strings.Cut(s, "+")
	s, out.pre, _ = strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out.n[i] = n
	}
	return out, true
}
