package hooks

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// State is the hook state of one harness in one vendor home.
type State string

// Installer adds and removes the hook entries of one visor in the vendor
// hook configs. It keeps the other entries and the rest of each file.
type Installer struct {
	argv []string
	mark marker
}

// marker starts each command that one visor installs. The tag tells two
// visors on the same machine apart.
type marker struct{ tag string }

// Hook states.
const (
	StateInstalled State = "installed"
	StateMissing   State = "missing"
	StateOutdated  State = "outdated"
	// StateUntrusted means that the vendor runs the hooks only after the user
	// trusts them in the vendor CLI.
	StateUntrusted State = "untrusted"
)

// Version is the shape of the hook command. Increase it when the command
// changes, so that Status reports outdated.
const Version = 1

const markerPrefix = ": agent-visor-hook="

// ErrCommand means that the hook command is not usable.
var ErrCommand = errors.New("hooks: bad hook command")

// NewInstaller takes the hook command before the harness name, with an
// absolute program path first, and a tag that names the visor.
func NewInstaller(tag string, argv []string) (*Installer, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return nil, fmt.Errorf("%w: %q needs an absolute program path", ErrCommand, argv)
	}
	return &Installer{argv: append([]string(nil), argv...), mark: marker{tag: cleanTag(tag)}}, nil
}

// Command is the hook command line for a harness. It reads stdin and does
// nothing when the program is gone.
func (i *Installer) Command(harness string) string {
	q := make([]string, 0, len(i.argv)+1)
	for _, a := range append(append([]string(nil), i.argv...), harness) {
		q = append(q, shellQuote(a))
	}
	return fmt.Sprintf("%s%d:%s; if [ -x %s ]; then %s; else cat >/dev/null; fi",
		markerPrefix, Version, i.mark.tag, q[0], strings.Join(q, " "))
}

// Install adds or updates the hook entries of the harness in the vendor
// home dir.
func (i *Installer) Install(harness, dir string) error {
	v, err := lookup(harness)
	if err != nil {
		return err
	}
	return i.edit(filepath.Join(dir, v.config), func(cfg []byte) ([]byte, error) { return i.install(v, harness, cfg) })
}

// Remove takes out only the hook entries of this visor.
func (i *Installer) Remove(harness, dir string) error {
	v, err := lookup(harness)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, v.config)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return i.edit(path, func(cfg []byte) ([]byte, error) { return v.table.remove(i.mark, cfg) })
}

// Status gives the hook state of the harness in the vendor home dir.
func (i *Installer) Status(harness, dir string) (State, error) {
	v, err := lookup(harness)
	if err != nil {
		return "", err
	}
	cfg, err := os.ReadFile(filepath.Join(dir, v.config))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("hook status of %s: %w", harness, err)
	}
	var trust []byte
	if v.trust != "" {
		trust, err = os.ReadFile(filepath.Join(dir, v.trust))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("hook status of %s: %w", harness, err)
		}
	}
	return i.state(v, cfg, trust), nil
}

func (i *Installer) install(v vendor, harness string, cfg []byte) ([]byte, error) {
	return v.table.install(i.mark, cfg, i.Command(harness))
}

func (i *Installer) state(v vendor, cfg, trust []byte) State {
	p, err := v.table.places(i.mark, cfg)
	if err != nil {
		return StateOutdated
	}
	installed, outdated := v.table.state(p)
	switch {
	case !installed:
		return StateMissing
	case outdated:
		return StateOutdated
	case v.trust != "" && !v.trusted(p, trust):
		return StateUntrusted
	}
	return StateInstalled
}

// edit changes one config file. It writes only when the bytes change, through
// a temporary file and a rename, and keeps the mode of the old file.
func (i *Installer) edit(path string, change func([]byte) ([]byte, error)) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	cfg, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	out, err := change(cfg)
	if err != nil {
		return fmt.Errorf("edit %s: %w", path, err)
	}
	if bytes.Equal(out, cfg) {
		return nil
	}
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := path + ".agent-visor.tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// version gives the marker version of a command, or ok false when the
// command is not of this visor.
func (m marker) version(cmd string) (int, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(cmd), markerPrefix)
	if !found {
		return 0, false
	}
	head, _, found := strings.Cut(rest, ";")
	if !found {
		return 0, false
	}
	num, tag, found := strings.Cut(head, ":")
	if !found || tag != m.tag {
		return 0, false
	}
	v, err := strconv.Atoi(num)
	if err != nil {
		return 0, true
	}
	return v, true
}

// trusted reports whether the user trusts each hook entry of the visor. The
// trust file has [hooks.state."<path>:<event>:<group>:<handler>"] tables
// with a trusted_hash.
func (v vendor) trusted(p map[string]place, trust []byte) bool {
	keys := trustedKeys(trust, v.config)
	for name, pl := range p {
		if !keys[fmt.Sprintf("%s:%s:%d:%d", v.config, snake(name), pl.group, pl.handler)] {
			return false
		}
	}
	return true
}

// trustedKeys gives the keys of the hooks.state tables with a trusted_hash
// and not enabled = false. A key keeps only the part from "<config>:" on,
// since the path in front depends on the vendor home.
func trustedKeys(toml []byte, config string) map[string]bool {
	out := map[string]bool{}
	key, hash, enabled := "", false, true
	done := func() {
		if key != "" && hash && enabled {
			out[key] = true
		}
		key, hash, enabled = "", false, true
	}
	sc := bufio.NewScanner(bytes.NewReader(toml))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			done()
			if rest, ok := strings.CutPrefix(line, `[hooks.state."`); ok {
				if k, _, ok := strings.Cut(rest, `"]`); ok {
					if i := strings.LastIndex(k, config+":"); i >= 0 {
						key = k[i:]
					}
				}
			}
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		switch strings.TrimSpace(name) {
		case "trusted_hash":
			s, err := strconv.Unquote(strings.TrimSpace(value))
			hash = err == nil && s != ""
		case "enabled":
			enabled = strings.TrimSpace(value) != "false"
		}
	}
	done()
	return out
}

// snake gives the trust key of an event: PreToolUse is pre_tool_use.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cleanTag keeps the tag safe in a shell command.
func cleanTag(tag string) string {
	if tag == "" {
		return "default"
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, tag)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
