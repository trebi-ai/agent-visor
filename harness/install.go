package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

const (
	installTimeout = 10 * time.Minute
	currentLink    = "current"
	outputTail     = 400
)

// Install installs a visor copy of the harness in bin/<name>/<version>/ and
// moves the current link to it. An empty version is the latest release. A
// harness with no npm recipe runs the vendor install instead, and gets the
// latest release. A failed install leaves the current link as it was. It
// returns ErrBusy when a live session uses the harness, because the switch
// must not change the binary under a running session.
func (m *Manager) Install(ctx context.Context, name, version string) (Harness, error) {
	if !m.known(name) {
		return Harness{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	version = strings.TrimPrefix(version, "v")
	if version != "" && !versionRE.MatchString(version) {
		return Harness{}, fmt.Errorf("%w: version %q", ErrInvalid, version)
	}
	r, ok := m.o.Facts.Recipe(agentwire.Harness(name))
	if ok && r.Method != agentwire.InstallNPM && len(r.Command(version)) > 0 {
		return m.installSystem(ctx, name, r)
	}
	if !ok || r.Method != agentwire.InstallNPM {
		return Harness{}, fmt.Errorf("%w: the visor cannot install %s", ErrUnsupported, name)
	}
	return m.installCopy(ctx, name, r, version)
}

// installCopy installs a visor copy with the npm recipe and switches to it.
func (m *Manager) installCopy(ctx context.Context, name string, r agentwire.InstallRecipe, version string) (Harness, error) {
	done, err := m.begin(name, "install")
	if err != nil {
		return Harness{}, err
	}
	defer done()
	if m.inUse(name) {
		return Harness{}, fmt.Errorf("%w: a live session uses %s", ErrBusy, name)
	}
	if version == "" {
		if v, err := m.lookupLatest(ctx, name); err == nil {
			version = v
		}
	}
	if version == "" || !m.hasVersion(name, r, version) {
		if version, err = m.installNPM(ctx, name, r, version); err != nil {
			return Harness{}, err
		}
	}
	// A session can start during the install. Keep the new version, but do
	// not switch under it.
	if m.inUse(name) {
		return Harness{}, fmt.Errorf("%w: a live session uses %s; version %s is ready", ErrBusy, name, version)
	}
	if err := m.switchTo(name, version); err != nil {
		return Harness{}, err
	}
	m.log.Info("install harness", "harness", name, "version", version)
	m.changed(name)
	return m.Harness(ctx, name)
}

// installSystem runs the vendor install, such as a brew formula or an
// install script. The install goes where the vendor puts it.
func (m *Manager) installSystem(ctx context.Context, name string, r agentwire.InstallRecipe) (Harness, error) {
	done, err := m.begin(name, "install")
	if err != nil {
		return Harness{}, err
	}
	defer done()
	if _, err := m.run(ctx, r.Command(""), nil, installTimeout); err != nil {
		return Harness{}, fmt.Errorf("install %s: %w", name, err)
	}
	m.log.Info("install harness", "harness", name, "method", r.Method)
	m.changed(name)
	return m.Harness(ctx, name)
}

// installNPM runs the npm recipe into a new folder and moves the folder to
// bin/<name>/<version>/. An empty version reads the version from the new
// binary. On a failure it removes the new folder.
func (m *Manager) installNPM(ctx context.Context, name string, r agentwire.InstallRecipe, version string) (string, error) {
	dir := m.binDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	tmp, err := os.MkdirTemp(dir, ".install-")
	if err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	ok := false
	defer func() {
		if !ok {
			if err := os.RemoveAll(tmp); err != nil {
				m.log.Warn("remove a failed install", "harness", name, "err", err)
			}
		}
	}()
	if _, err := m.run(ctx, r.Command(version), map[string]string{"NPM_CONFIG_PREFIX": tmp}, installTimeout); err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	bin := filepath.Join(tmp, "bin", r.Bin)
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("install %s: the install made no %s", name, r.Bin)
	}
	if version == "" {
		version = cleanVersion(m.wire.Version(ctx, bin))
		if !versionRE.MatchString(version) {
			return "", fmt.Errorf("install %s: %s gives no version", name, r.Bin)
		}
	}
	dst := filepath.Join(dir, version)
	if err := os.RemoveAll(dst); err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("install %s: %w", name, err)
	}
	ok = true
	return version, nil
}

// switchTo moves the current link to version, and removes the versions
// other than the new one and the one before it.
func (m *Manager) switchTo(name, version string) error {
	dir := m.binDir(name)
	prev, _ := os.Readlink(filepath.Join(dir, currentLink)) // no link: nothing to keep
	tmp := filepath.Join(dir, ".current.tmp")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("switch %s: %w", name, err)
	}
	if err := os.Symlink(version, tmp); err != nil {
		return fmt.Errorf("switch %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, currentLink)); err != nil {
		return fmt.Errorf("switch %s: %w", name, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("switch %s: %w", name, err)
	}
	for _, e := range entries {
		if n := e.Name(); n != currentLink && n != version && n != prev {
			if err := os.RemoveAll(filepath.Join(dir, n)); err != nil {
				m.log.Warn("remove an old install", "harness", name, "version", n, "err", err)
			}
		}
	}
	return nil
}

// current gives the version and the binary of the visor copy.
func (m *Manager) current(name string) (string, string, bool) {
	v, err := os.Readlink(filepath.Join(m.binDir(name), currentLink))
	if err != nil {
		return "", "", false
	}
	r, ok := m.o.Facts.Recipe(agentwire.Harness(name))
	if !ok {
		return "", "", false
	}
	if !m.hasVersion(name, r, v) {
		return "", "", false
	}
	return v, m.binPath(name, r, v), true
}

// installs lists the visor installs, oldest first.
func (m *Manager) installs(name string) []string {
	entries, err := os.ReadDir(m.binDir(name))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); e.IsDir() && !strings.HasPrefix(n, ".") {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, compareVersions)
	return out
}

func (m *Manager) hasVersion(name string, r agentwire.InstallRecipe, version string) bool {
	_, err := os.Stat(m.binPath(name, r, version))
	return err == nil
}

func (m *Manager) binDir(name string) string { return filepath.Join(m.home, "bin", name) }

func (m *Manager) binPath(name string, r agentwire.InstallRecipe, version string) string {
	return filepath.Join(m.binDir(name), version, "bin", r.Bin)
}

// run runs one command with env on top of the process env. The error holds
// the tail of the output.
func (m *Manager) run(ctx context.Context, argv []string, env map[string]string, timeout time.Duration) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%w: no command", ErrUnsupported)
	}
	bin, err := m.lookPath(argv[0])
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Env = os.Environ()
	if m.o.Path != "" {
		cmd.Env = append(cmd.Env, "PATH="+m.o.Path)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Dir = m.home
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > outputTail {
			tail = tail[len(tail)-outputTail:]
		}
		return out, fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, tail)
	}
	return out, nil
}

// lookPath finds a program in Options.Path, or in PATH when it is empty.
func (m *Manager) lookPath(file string) (string, error) {
	if m.o.Path == "" || strings.ContainsRune(file, filepath.Separator) {
		p, err := exec.LookPath(file)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrUnsupported, err)
		}
		return p, nil
	}
	for _, dir := range filepath.SplitList(m.o.Path) {
		p := filepath.Join(dir, file)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: %s is not in the install path", ErrUnsupported, file)
}

// compareVersions orders version folder names by number.
func compareVersions(a, b string) int {
	pa, pb := strings.FieldsFunc(a, versionSep), strings.FieldsFunc(b, versionSep)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		var na, nb int
		_, ea := fmt.Sscan(pa[i], &na)
		_, eb := fmt.Sscan(pb[i], &nb)
		switch {
		case ea == nil && eb == nil && na != nb:
			return na - nb
		case ea != nil || eb != nil:
			if c := strings.Compare(pa[i], pb[i]); c != 0 {
				return c
			}
		}
	}
	return len(pa) - len(pb)
}

func versionSep(r rune) bool { return r == '.' || r == '-' }
