package service_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/agent-visor/service"
)

type commands struct {
	mu   sync.Mutex
	got  []string
	fail string
	on   func(line string)
}

func TestInstallAndRemove(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		goos, file, unit, install, remove string
	}{
		{
			"darwin", "ex.visor.plist", "<string>/bin/ex</string>\n    <string>visor</string>",
			"launchctl bootstrap", "launchctl bootout",
		},
		{
			"linux", "ex.visor.service", `ExecStart="/bin/ex" "visor"`,
			"systemctl --user daemon-reload|systemctl --user enable ex.visor.service|systemctl --user restart ex.visor.service",
			"systemctl --user disable --now ex.visor.service|systemctl --user daemon-reload",
		},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			t.Parallel()
			cmds := &commands{}
			s := service.Service{
				Label: "ex.visor", Command: []string{"/bin/ex", "visor"}, UnitDir: t.TempDir(), GOOS: tc.goos,
				Env: map[string]string{"TOKEN": "a&b"}, Run: cmds.run,
			}
			if err := s.Install(t.Context()); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(filepath.Join(s.UnitDir, tc.file))
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("unit file: %v %v", st, err)
			}
			unit, _ := os.ReadFile(s.UnitPath()) //nolint:errcheck // the stat above covers it
			if !strings.Contains(string(unit), tc.unit) {
				t.Fatalf("unit:\n%s", unit)
			}
			if got := cmds.take(); !strings.HasPrefix(got, tc.install) {
				t.Fatalf("install commands %q, want %q", got, tc.install)
			}
			if err := s.Remove(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.UnitPath()); !os.IsNotExist(err) {
				t.Fatalf("the unit file stays: %v", err)
			}
			if got := cmds.take(); !strings.HasPrefix(got, tc.remove) {
				t.Fatalf("remove commands %q, want %q", got, tc.remove)
			}
		})
	}
}

func TestBootstrapOfALoadedJobRestartsIt(t *testing.T) {
	t.Parallel()
	cmds := &commands{fail: "launchctl bootstrap"}
	s := service.Service{Label: "ex.visor", Command: []string{"/bin/ex"}, UnitDir: t.TempDir(), GOOS: "darwin", Run: cmds.run}
	if err := s.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := cmds.take(); !strings.Contains(got, "|launchctl kickstart -k gui/") {
		t.Fatalf("commands %q", got)
	}
}

func TestEnsureStartsTheService(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "avs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "v.sock")
	v, err := agentvisor.New(t.Context(), agentvisor.Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	var srv *http.Server
	cmds := &commands{on: func(line string) {
		if !strings.HasPrefix(line, "systemctl --user restart") {
			return
		}
		ln, err := agentvisor.Listen(context.Background(), sock)
		if err != nil {
			t.Error(err)
			return
		}
		srv = &http.Server{Handler: agentvisor.NewServer(v), ReadHeaderTimeout: time.Second}
		go srv.Serve(ln) //nolint:errcheck // Close ends it
	}}
	s := service.Service{Label: "ex.visor", Command: []string{"/bin/ex"}, Socket: sock, UnitDir: t.TempDir(), GOOS: "linux", Run: cmds.run}
	c, err := service.Ensure(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	defer c.Close(t.Context())
	if _, err := service.Ensure(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if got := cmds.take(); strings.Count(got, "restart") != 1 {
		t.Fatalf("the second Ensure starts the service again: %q", got)
	}
}

func (c *commands) run(_ context.Context, name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	c.mu.Lock()
	c.got = append(c.got, line)
	c.mu.Unlock()
	if c.on != nil {
		c.on(line)
	}
	if c.fail != "" && strings.HasPrefix(line, c.fail) {
		return errors.New("failed")
	}
	return nil
}

func (c *commands) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := strings.Join(c.got, "|")
	c.got = nil
	return out
}
