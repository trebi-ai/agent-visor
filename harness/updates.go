package harness

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

// UpdateMode tells how the visor updates a harness.
type UpdateMode string

// Update modes.
const (
	// UpdatesAuto updates to each new release when no session uses the
	// harness.
	UpdatesAuto UpdateMode = "auto"
	// UpdatesKeep keeps the version. A system install runs from a kept
	// copy, so the vendor update cannot change it.
	UpdatesKeep UpdateMode = "keep"
)

// Update is one version change of a harness.
type Update struct {
	At    time.Time `json:"at"`
	From  string    `json:"from,omitempty"`
	To    string    `json:"to,omitempty"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	// Outside means that the harness changed outside the visor.
	Outside bool `json:"outside,omitempty"`
}

const (
	checkTick     = time.Minute
	historyKeep   = 10
	updateGateMax = 2 * time.Minute
	// copyMark marks a version folder that holds a copy of a system install.
	copyMark = ".copy"
)

// Update updates the harness. A visor copy gets a new visor copy. A system
// install runs the vendor update, after a copy of the old program. A check
// after the update needs a version and, when the login worked before, a
// working login. A failed check goes back to the old program. When a live
// session uses the harness, the update waits for its end and Update returns
// ErrBusy.
func (m *Manager) Update(ctx context.Context, name, version string) (Harness, error) {
	if !m.known(name) {
		return Harness{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	version = strings.TrimPrefix(version, "v")
	if version != "" && !versionRE.MatchString(version) {
		return Harness{}, fmt.Errorf("%w: version %q", ErrInvalid, version)
	}
	if m.inUse(name) {
		if _, err := m.changeState(ctx, name, func(s *state) { s.Pending, s.PendingTo = true, version }); err != nil {
			return Harness{}, err
		}
		m.changed(name)
		return Harness{}, fmt.Errorf("%w: a live session uses %s; the update runs when it ends", ErrBusy, name)
	}
	return m.update(ctx, name, version)
}

// SetUpdates sets the update mode. Keep with a version moves to that
// version first; an empty version keeps the version that runs now.
func (m *Manager) SetUpdates(ctx context.Context, name string, mode UpdateMode, version string) (Harness, error) {
	if !m.known(name) {
		return Harness{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	version = strings.TrimPrefix(version, "v")
	switch {
	case mode != UpdatesAuto && mode != UpdatesKeep:
		return Harness{}, fmt.Errorf("%w: update mode %q", ErrInvalid, mode)
	case version != "" && (mode != UpdatesKeep || !versionRE.MatchString(version)):
		return Harness{}, fmt.Errorf("%w: version %q", ErrInvalid, version)
	}
	if mode == UpdatesAuto {
		if err := m.dropCopy(name); err != nil {
			return Harness{}, err
		}
	} else if err := m.keep(ctx, name, version); err != nil {
		return Harness{}, err
	}
	if _, err := m.changeState(ctx, name, func(s *state) {
		s.Updates = mode
		s.KeptAt = time.Time{}
		if mode == UpdatesKeep {
			s.KeptAt = time.Now().UTC()
		}
	}); err != nil {
		return Harness{}, err
	}
	m.changed(name)
	return m.Harness(ctx, name)
}

// Run checks the harnesses each minute until ctx ends: it looks for a new
// release each Options.CheckEvery, records an update outside the visor,
// and runs the auto and the waiting updates.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(checkTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.Check(ctx)
	}
}

// Check does one pass of Run.
func (m *Manager) Check(ctx context.Context) {
	for _, name := range m.names {
		if m.o.Enabled != nil && !m.o.Enabled(name) {
			continue
		}
		if _, ok := m.o.Facts.Recipe(agentwire.Harness(name)); !ok {
			continue
		}
		if err := m.checkOne(ctx, name, m.o.Enabled != nil); err != nil {
			m.log.Warn("check harness", "harness", name, "err", err)
		}
	}
}

// checkOne checks one harness. With system false, it skips a binary that
// the visor did not install.
func (m *Manager) checkOne(ctx context.Context, name string, system bool) error {
	h, err := m.Harness(ctx, name)
	if err != nil || !h.Installed || m.running(name) || (!system && h.Source != SourceVisor) {
		return err
	}
	st, err := m.state(ctx, name)
	if err != nil {
		return err
	}
	if err := m.noteVersion(ctx, name, h.Version, st.Seen); err != nil {
		return err
	}
	latest, err := m.latestFor(ctx, name, h.Latest, st.CheckedAt)
	if err != nil {
		return err
	}
	auto := st.updates() == UpdatesAuto && newerVersion(latest, h.Version) && !st.failedTo(latest)
	if (!st.Pending && !auto) || m.inUse(name) {
		return nil
	}
	_, err = m.update(ctx, name, st.PendingTo)
	return err
}

// latestFor gives the newest release. It looks it up once each CheckEvery,
// else it gives known.
func (m *Manager) latestFor(ctx context.Context, name, known string, checked time.Time) (string, error) {
	if time.Since(checked) < m.o.CheckEvery {
		return known, nil
	}
	if v, err := m.lookupLatest(ctx, name); err == nil {
		known = v
	}
	_, err := m.changeState(ctx, name, func(s *state) { s.CheckedAt = time.Now().UTC() })
	return known, err
}

// noteVersion keeps the version that runs. A change since the last check
// came from outside the visor; the visor checks it and records it.
func (m *Manager) noteVersion(ctx context.Context, name, version, seen string) error {
	if seen == version {
		return nil
	}
	if seen != "" {
		u := m.verify(ctx, name, seen, version, m.cachedAuth(name) == AuthOK)
		u.Outside = true
		m.record(ctx, name, u)
	}
	_, err := m.changeState(ctx, name, func(s *state) { s.Seen = version })
	return err
}

func (m *Manager) update(ctx context.Context, name, version string) (Harness, error) {
	h := agentwire.Harness(name)
	r, hasRecipe := m.o.Facts.Recipe(h)
	cur, err := m.Harness(ctx, name)
	if err != nil {
		return Harness{}, err
	}
	if !cur.Installed {
		return Harness{}, fmt.Errorf("%w: %s is not installed", ErrState, name)
	}
	visorCopy := cur.Source == SourceVisor && !m.isCopy(name, cur.Version)
	if !visorCopy && (!hasRecipe || len(r.UpdateCommand(version)) == 0) {
		return Harness{}, fmt.Errorf("%w: no update for %s", ErrUnsupported, name)
	}
	if _, err := m.changeState(ctx, name, func(s *state) { s.Pending, s.PendingTo = false, "" }); err != nil {
		return Harness{}, err
	}
	authOK := m.freshAuth(ctx, name) == AuthOK
	from := cur.Version
	var u Update
	if visorCopy {
		if _, err := m.Install(ctx, name, version); err != nil {
			return Harness{}, err
		}
		after, _, _ := m.current(name)
		u = m.verify(ctx, name, from, after, authOK)
		if !u.OK {
			m.rollback(name, from)
		}
	} else {
		if u, err = m.updateSystem(ctx, name, r, from, version, authOK); err != nil {
			return Harness{}, err
		}
	}
	m.record(ctx, name, u)
	m.changed(name)
	out, err := m.Harness(ctx, name)
	if err != nil {
		return Harness{}, err
	}
	if _, err := m.changeState(ctx, name, func(s *state) { s.Seen = out.Version }); err != nil {
		return Harness{}, err
	}
	if !u.OK {
		return Harness{}, fmt.Errorf("update %s: %s", name, u.Error)
	}
	m.log.Info("update harness", "harness", name, "from", u.From, "to", u.To)
	return out, nil
}

// updateSystem runs the vendor update of a system install. It copies the
// old program first, and runs that copy when the check fails.
func (m *Manager) updateSystem(ctx context.Context, name string, r agentwire.InstallRecipe, from, version string, authOK bool) (Update, error) {
	d, err := m.wire.Detect(ctx, agentwire.Harness(name))
	if err != nil || d.Path == "" {
		return Update{}, fmt.Errorf("%w: %s is not installed", ErrState, name)
	}
	done, err := m.begin(name, "update")
	if err != nil {
		return Update{}, err
	}
	defer done()
	if from != "" {
		if err := m.copyProgram(name, r, d.Path, from); err != nil {
			return Update{}, fmt.Errorf("copy %s %s: %w", name, from, err)
		}
	}
	argv := r.UpdateCommand(version)
	if argv[0] == r.Bin {
		argv[0] = d.Path
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := m.run(ctx, argv, nil, installTimeout); err != nil {
		u := Update{At: time.Now().UTC(), From: from, To: cmp.Or(version, m.cachedLatestOnly(name)), Error: err.Error()}
		m.rollback(name, from)
		return u, nil
	}
	to := cleanVersion(m.wire.Version(ctx, d.Path))
	u := m.verify(ctx, name, from, to, authOK)
	if !u.OK && !versionRE.MatchString(to) {
		u.To = cmp.Or(version, m.cachedLatestOnly(name))
	}
	switch {
	case !u.OK:
		m.rollback(name, from)
	case m.updatesMode(ctx, name) == UpdatesKeep:
		err := m.copyProgram(name, r, d.Path, to)
		if err == nil {
			err = m.switchTo(name, to)
		}
		if err != nil {
			u.OK, u.Error = false, err.Error()
		}
	default:
		if err := m.dropCopy(name); err != nil {
			m.log.Warn("remove the old copy", "harness", name, "err", err)
		}
	}
	return u, nil
}

// keep makes the version that runs now, or version, the one that stays.
func (m *Manager) keep(ctx context.Context, name, version string) error {
	h := agentwire.Harness(name)
	r, ok := m.o.Facts.Recipe(h)
	cur, err := m.Harness(ctx, name)
	if err != nil {
		return err
	}
	if version == "" || version == cur.Version {
		if !cur.Installed || cur.Source == SourceVisor || !ok {
			return nil
		}
		if err := m.copyProgram(name, r, cur.Path, cur.Version); err != nil {
			return err
		}
		return m.switchTo(name, cur.Version)
	}
	if ok && m.hasVersion(name, r, version) {
		if m.inUse(name) {
			return fmt.Errorf("%w: a live session uses %s", ErrBusy, name)
		}
		return m.switchTo(name, version)
	}
	_, err = m.Install(ctx, name, version)
	return err
}

// verify checks the program after a version change.
func (m *Manager) verify(ctx context.Context, name, from, to string, authOK bool) Update {
	u := Update{At: time.Now().UTC(), From: from, To: to, OK: true}
	switch {
	case to == "" || !versionRE.MatchString(to):
		u.OK, u.Error = false, "the new version gives no version number"
	case authOK && m.freshAuth(ctx, name) != AuthOK:
		u.OK, u.Error = false, "the login check failed after the update"
	}
	return u
}

// rollback runs version again: the visor copy, or the copy of the system
// install.
func (m *Manager) rollback(name, version string) {
	r, ok := m.o.Facts.Recipe(agentwire.Harness(name))
	if version == "" || !ok || !m.hasVersion(name, r, version) {
		return
	}
	if err := m.switchTo(name, version); err != nil {
		m.log.Warn("go back to the old version", "harness", name, "version", version, "err", err)
	}
}

// record adds an update to the history.
func (m *Manager) record(ctx context.Context, name string, u Update) {
	if _, err := m.changeState(ctx, name, func(s *state) {
		s.History = append(s.History, u)
		if n := len(s.History); n > historyKeep {
			s.History = s.History[n-historyKeep:]
		}
	}); err != nil {
		m.log.Warn("record an update", "harness", name, "err", err)
	}
}

// copyProgram copies the real file of a system install, and the files next
// to it whose names start with its name and "-", into the version folder.
func (m *Manager) copyProgram(name string, r agentwire.InstallRecipe, bin, version string) error {
	if m.hasVersion(name, r, version) {
		return nil
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return err
	}
	dir := filepath.Join(m.binDir(name), version)
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmp, "bin"), 0o700); err != nil {
		return err
	}
	if err := copyFile(real, filepath.Join(tmp, "bin", r.Bin)); err != nil {
		return err
	}
	base := filepath.Base(real)
	entries, _ := os.ReadDir(filepath.Dir(real)) // no folder: no side files
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), base+"-") {
			if err := copyFile(filepath.Join(filepath.Dir(real), e.Name()), filepath.Join(tmp, "bin", e.Name())); err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, copyMark), nil, 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

// dropCopy removes the current link to a copy of a system install, and
// the copies, so that sessions run the system install again.
func (m *Manager) dropCopy(name string) error {
	dir := m.binDir(name)
	if v, err := os.Readlink(filepath.Join(dir, currentLink)); err == nil && m.isCopy(name, v) {
		if err := os.Remove(filepath.Join(dir, currentLink)); err != nil {
			return err
		}
	}
	for _, v := range m.installs(name) {
		if m.isCopy(name, v) {
			if err := os.RemoveAll(filepath.Join(dir, v)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) isCopy(name, version string) bool {
	_, err := os.Stat(filepath.Join(m.binDir(name), version, copyMark))
	return err == nil
}

// waitUpdate waits while an update of the harness runs, at most
// updateGateMax.
func (m *Manager) waitUpdate(ctx context.Context, name string) {
	deadline := time.Now().Add(updateGateMax)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		op := m.ops[name]
		m.mu.Unlock()
		if op != "update" && op != "install" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *Manager) running(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.ops[name]
	return ok
}

func (m *Manager) updatesMode(ctx context.Context, name string) UpdateMode {
	st, err := m.state(ctx, name)
	if err != nil {
		return UpdatesAuto
	}
	return st.updates()
}

// freshAuth asks for the login state with no cache.
func (m *Manager) freshAuth(ctx context.Context, name string) AuthState {
	m.mu.Lock()
	delete(m.auth, name)
	m.mu.Unlock()
	st, err := m.state(ctx, name)
	if err != nil {
		return AuthUnknown
	}
	env, err := m.env(name)
	if err != nil {
		return AuthUnknown
	}
	return m.authState(ctx, name, st.mode(), env)
}

// cachedLatestOnly gives the known latest release with no lookup.
func (m *Manager) cachedLatestOnly(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest[name].version
}

func (m *Manager) cachedAuth(name string) AuthState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.auth[name].state
}

func (s state) updates() UpdateMode {
	if s.Updates == "" {
		return UpdatesAuto
	}
	return s.Updates
}

// failedTo reports that the last update to version failed. The visor tries
// again for a newer version or on a request.
func (s state) failedTo(version string) bool {
	n := len(s.History)
	return n > 0 && !s.History[n-1].OK && s.History[n-1].To == version
}

// newerVersion reports whether a is newer than b.
func newerVersion(a, b string) bool {
	return a != "" && b != "" && versionRE.MatchString(a) && compareVersions(a, b) > 0
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // a read-only file
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() // the copy error wins
		return err
	}
	return out.Close()
}
