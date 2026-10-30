package hooks

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

//nolint:gochecknoglobals // a test flag
var update = flag.Bool("update", false, "write the golden files")

const testExe = "/usr/local/bin/example"

func TestCommand(t *testing.T) {
	t.Parallel()
	want := `: agent-visor-hook=1:example; if [ -x '/usr/local/bin/example' ]; then '/usr/local/bin/example' 'visor' 'hook' 'claude'; else cat >/dev/null; fi`
	if got := newTestInstaller(t, "example").Command("claude"); got != want {
		t.Fatalf("got %s", got)
	}
	i, err := NewInstaller("a b;c", []string{"/Users/o'neil/bin/example", "hook"})
	if err != nil {
		t.Fatal(err)
	}
	if got := i.Command("codex"); !strings.Contains(got, `'/Users/o'\''neil/bin/example'`) || !strings.HasPrefix(got, markerPrefix+"1:a_b_c;") {
		t.Fatalf("quote: %s", got)
	}
	for _, argv := range [][]string{nil, {"example", "hook"}} {
		if _, err := NewInstaller("x", argv); !errors.Is(err, ErrCommand) {
			t.Errorf("%q: %v", argv, err)
		}
	}
}

func TestInstallRemoveRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		harness, config, src, installed string
		after                           State
	}{
		{"claude", "settings.json", "claude/settings.json", "claude/settings.installed.json", StateInstalled},
		{"codex", "hooks.json", "codex/hooks.json", "codex/hooks.installed.json", StateUntrusted},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			dir := home(t, map[string]string{tc.config: tc.src})
			i := newTestInstaller(t, "example")
			if st, err := i.Status(tc.harness, dir); err != nil || st != StateMissing {
				t.Fatalf("status before: %s %v", st, err)
			}
			if err := i.Install(tc.harness, dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, tc.config)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tc.installed, got)
			if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o644 {
				t.Fatalf("mode: %v %v", st, err)
			}
			if err := i.Install(tc.harness, dir); err != nil {
				t.Fatal(err)
			}
			if again, _ := os.ReadFile(path); !bytes.Equal(again, got) { //nolint:errcheck // the compare fails on a read error
				t.Fatal("a second install changed the file")
			}
			if st, err := i.Status(tc.harness, dir); err != nil || st != tc.after {
				t.Fatalf("status after: %s %v", st, err)
			}
			if err := i.Remove(tc.harness, dir); err != nil {
				t.Fatal(err)
			}
			if back, _ := os.ReadFile(path); !bytes.Equal(back, readTestdata(t, tc.src)) { //nolint:errcheck // the compare fails on a read error
				t.Fatalf("remove did not give the original bytes:\n%s", back)
			}
		})
	}
}

func TestInstallKeepsOtherVisors(t *testing.T) {
	t.Parallel()
	dir := home(t, map[string]string{"settings.json": "claude/settings.json"})
	a, b := newTestInstaller(t, "one"), newTestInstaller(t, "two")
	for _, i := range []*Installer{a, b} {
		if err := i.Install("claude", dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []*Installer{a, b} {
		if st, err := i.Status("claude", dir); err != nil || st != StateInstalled {
			t.Fatalf("status: %s %v", st, err)
		}
	}
	if err := a.Remove("claude", dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.Status("claude", dir); st != StateMissing { //nolint:errcheck // the state check covers it
		t.Fatalf("one: %s", st)
	}
	if st, _ := b.Status("claude", dir); st != StateInstalled { //nolint:errcheck // the state check covers it
		t.Fatalf("two: %s", st)
	}
	if err := b.Remove("claude", dir); err != nil {
		t.Fatal(err)
	}
	if back, _ := os.ReadFile(filepath.Join(dir, "settings.json")); !bytes.Equal(back, readTestdata(t, "claude/settings.json")) { //nolint:errcheck // the compare fails on a read error
		t.Fatalf("remove did not give the original bytes:\n%s", back)
	}
}

func TestOutdated(t *testing.T) {
	t.Parallel()
	i := newTestInstaller(t, "example")
	installed := readTestdata(t, "claude/settings.installed.json")
	old := bytes.ReplaceAll(installed, []byte(markerPrefix+"1:"), []byte(markerPrefix+"0:"))
	old = bytes.ReplaceAll(old, []byte(testExe), []byte("/opt/old/example"))
	v := vendors["claude"]
	if st := i.state(v, old, nil); st != StateOutdated {
		t.Fatalf("status: %s", st)
	}
	got, err := i.install(v, "claude", old)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installed) {
		t.Fatalf("the outdated entries were not replaced:\n%s", got)
	}
}

func TestEmptyAndMissingConfig(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "new-home")
	i := newTestInstaller(t, "example")
	if err := i.Remove("claude", dir); err != nil {
		t.Fatalf("remove with no file: %v", err)
	}
	if err := i.Install("claude", dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("home mode: %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", st, err)
	}
	if st, err := i.Status("claude", dir); err != nil || st != StateInstalled {
		t.Fatalf("status: %s %v", st, err)
	}
	if err := i.Remove("claude", dir); err != nil {
		t.Fatal(err)
	}
	if back, _ := os.ReadFile(filepath.Join(dir, "settings.json")); string(back) != "{}\n" { //nolint:errcheck // the compare fails on a read error
		t.Fatalf("remove: %q", back)
	}
}

func TestCodexTrust(t *testing.T) {
	t.Parallel()
	i := newTestInstaller(t, "example")
	v := vendors["codex"]
	installed := readTestdata(t, "codex/hooks.installed.json")
	for name, tc := range map[string]struct {
		cfg   []byte
		trust string
		want  State
	}{
		"no trust":      {installed, "", StateUntrusted},
		"partial trust": {installed, "codex/config.untrusted.toml", StateUntrusted},
		"trusted":       {installed, "codex/config.trusted.toml", StateInstalled},
		"not installed": {readTestdata(t, "codex/hooks.json"), "codex/config.trusted.toml", StateMissing},
	} {
		var trust []byte
		if tc.trust != "" {
			trust = readTestdata(t, tc.trust)
		}
		if st := i.state(v, tc.cfg, trust); st != tc.want {
			t.Errorf("%s: %s, want %s", name, st, tc.want)
		}
	}
}

func TestInstallFollowsSymlink(t *testing.T) {
	t.Parallel()
	dir, real := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(real, readTestdata(t, "claude/settings.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := newTestInstaller(t, "example").Install("claude", dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v %v", st, err)
	}
	if got, _ := os.ReadFile(real); !bytes.Equal(got, readTestdata(t, "claude/settings.installed.json")) { //nolint:errcheck // the compare fails on a read error
		t.Fatal("the target did not change")
	}
}

func TestSnake(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"PreToolUse": "pre_tool_use", "Stop": "stop", "SessionStart": "session_start"} {
		if got := snake(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestVendorPaths(t *testing.T) {
	t.Parallel()
	if got, err := ConfigPath("claude", "/x"); err != nil || got != "/x/settings.json" {
		t.Fatalf("config: %s %v", got, err)
	}
	if _, err := ConfigPath("cursor", "/x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown: %v", err)
	}
	if strings.Join(Names(), ",") != "claude,codex" || !Supported("claude") || Supported("pi") {
		t.Fatalf("names: %v", Names())
	}
}

func newTestInstaller(t *testing.T, tag string) *Installer {
	t.Helper()
	i, err := NewInstaller(tag, []string{testExe, "visor", "hook"})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func readTestdata(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	full := filepath.Join("testdata", path)
	if *update {
		if err := os.WriteFile(full, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if want := readTestdata(t, path); !bytes.Equal(got, want) {
		t.Fatalf("%s differs:\n%s", path, got)
	}
}

// home makes a vendor home with the files of the testdata.
func home(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), readTestdata(t, src), 0o644); err != nil { //nolint:gosec // a user config file mode
			t.Fatal(err)
		}
	}
	return dir
}
