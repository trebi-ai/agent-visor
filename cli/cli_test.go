package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
)

var update = flag.Bool("update", false, "write the golden files")

// fakeVisor gives fixed projects, workdirs, and sessions.
type fakeVisor struct {
	projects []agentvisor.Project
	workdirs []agentvisor.Workdir
	sessions []agentvisor.Session
}

// syncBuffer is a bytes.Buffer that two goroutines can use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (f *fakeVisor) Info(context.Context) (agentvisor.Info, error) {
	return agentvisor.Info{Build: "test", Protocol: agentvisor.Protocol, Home: "/visor", Seq: 30, PID: 1}, nil
}

func (f *fakeVisor) Start(context.Context, agentvisor.StartSpec) (agentvisor.Session, error) {
	return agentvisor.Session{}, agentvisor.ErrUnsupported
}

func (f *fakeVisor) Session(_ context.Context, id string) (agentvisor.Session, error) {
	for _, s := range f.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return agentvisor.Session{}, agentvisor.ErrNotFound
}

func (f *fakeVisor) Sessions(_ context.Context, fl agentvisor.Filter) ([]agentvisor.Session, error) {
	var out []agentvisor.Session
	for _, s := range f.sessions {
		if (fl.Project != "" && s.Project != fl.Project) || (fl.Workdir != "" && s.Workdir != fl.Workdir) ||
			(len(fl.States) > 0 && !slices.Contains(fl.States, s.State)) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeVisor) Send(context.Context, string, agentvisor.Input) error {
	return agentvisor.ErrUnsupported
}
func (f *fakeVisor) Interrupt(context.Context, string) error { return agentvisor.ErrUnsupported }
func (f *fakeVisor) Answer(context.Context, string, string, agentvisor.Answer) error {
	return agentvisor.ErrUnsupported
}
func (f *fakeVisor) Stop(context.Context, string) error { return agentvisor.ErrUnsupported }
func (f *fakeVisor) Events(context.Context, agentvisor.Filter, uint64) (*agentvisor.EventStream, error) {
	return nil, agentvisor.ErrUnsupported
}

func (f *fakeVisor) Projects(context.Context, agentvisor.Filter) ([]agentvisor.Project, error) {
	return f.projects, nil
}
func (f *fakeVisor) ForgetProject(context.Context, string) error { return agentvisor.ErrUnsupported }
func (f *fakeVisor) Workdirs(_ context.Context, fl agentvisor.Filter) ([]agentvisor.Workdir, error) {
	var out []agentvisor.Workdir
	for _, w := range f.workdirs {
		if fl.Project == "" || w.Project == fl.Project {
			out = append(out, w)
		}
	}
	return out, nil
}

func (f *fakeVisor) EnsureWorkdir(context.Context, agentvisor.WorkdirSpec) (agentvisor.Workdir, error) {
	return agentvisor.Workdir{}, agentvisor.ErrUnsupported
}
func (f *fakeVisor) ArchiveWorkdir(context.Context, string) error { return agentvisor.ErrUnsupported }
func (f *fakeVisor) RestoreWorkdir(context.Context, string) (agentvisor.Workdir, error) {
	return agentvisor.Workdir{}, agentvisor.ErrUnsupported
}
func (f *fakeVisor) RemoveWorkdir(context.Context, string, bool) error {
	return agentvisor.ErrUnsupported
}
func (f *fakeVisor) Close(context.Context) error { return nil }

func (f *fakeVisor) Attach(context.Context, string, agentvisor.AttachOptions) (*agentvisor.AttachStream, error) {
	return nil, agentvisor.ErrUnsupported
}

func (f *fakeVisor) Handover(context.Context, string, agentvisor.Kind) error {
	return agentvisor.ErrUnsupported
}

func (f *fakeVisor) Harnesses(context.Context) ([]agentvisor.Harness, error) {
	return []agentvisor.Harness{
		{Name: "claude", Installed: true, Version: "2.1.0", Latest: "2.2.0", Path: "/visor/bin/claude/current/claude", Source: agentvisor.SourceVisor, Installs: []string{"2.0.0", "2.1.0"}, Installable: true, Supported: true, AuthMode: agentvisor.AuthSeparate, Auth: agentvisor.AuthOK, EnvKeys: []string{"API_TOKEN"}},
		{Name: "codex", Installed: true, Version: "0.9.0", Path: "/usr/local/bin/codex", Source: agentvisor.SourceSystem, Installable: true, Supported: true, AuthMode: agentvisor.AuthShared, Auth: agentvisor.AuthMissing},
		{Name: "gemini", Installable: true, AuthMode: agentvisor.AuthShared, Auth: agentvisor.AuthUnknown, Detail: "not installed"},
	}, nil
}

func (f *fakeVisor) Harness(ctx context.Context, name string) (agentvisor.Harness, error) {
	hs, _ := f.Harnesses(ctx) //nolint:errcheck // a fixed list
	for _, h := range hs {
		if h.Name == name {
			return h, nil
		}
	}
	return agentvisor.Harness{}, agentvisor.ErrNotFound
}

func (f *fakeVisor) InstallHarness(context.Context, string, string) (agentvisor.Harness, error) {
	return agentvisor.Harness{}, agentvisor.ErrUnsupported
}

func (f *fakeVisor) UpdateHarness(context.Context, string, string) (agentvisor.Harness, error) {
	return agentvisor.Harness{}, agentvisor.ErrUnsupported
}

func (f *fakeVisor) SetHarnessAuth(context.Context, string, agentvisor.AuthMode) (agentvisor.Harness, error) {
	return agentvisor.Harness{}, agentvisor.ErrUnsupported
}

func (f *fakeVisor) LoginHarness(_ context.Context, name string) (agentvisor.Login, error) {
	return agentvisor.Login{Harness: name, URL: "https://login.example/device", Code: "ABCD-1234"}, nil
}

func (f *fakeVisor) LogoutHarness(context.Context, string) error { return agentvisor.ErrUnsupported }

func (f *fakeVisor) HarnessModels(context.Context, string) ([]agentvisor.Model, error) {
	return []agentvisor.Model{
		{ID: "opus", Name: "Opus", Default: true, Efforts: []string{"low", "medium", "high"}},
		{ID: "haiku", Name: "Haiku"},
	}, nil
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestGolden(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"ls.txt":             {"ls"},
		"ls.json":            {"--json", "ls"},
		"ls_all.txt":         {"ls", "--all"},
		"session_ls.txt":     {"session", "ls"},
		"session_ls.json":    {"session", "ls", "--json"},
		"session_ls_all.txt": {"session", "ls", "--all"},
		"session_show.txt":   {"session", "show", "ses_b"},
		"workdir_ls.txt":     {"workdir", "ls"},
		"project_show.txt":   {"project", "show", "prj_notes"},
		"status.txt":         {"status"},
		"harness_ls.txt":     {"harness", "ls"},
		"harness_ls.json":    {"--json", "harness", "ls"},
		"harness_show.txt":   {"harness", "show", "claude"},
		"harness_models.txt": {"harness", "models", "claude"},
		"harness_login.txt":  {"harness", "login", "codex"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := runFake(t, args...)
			if err != nil {
				out += "error: " + err.Error() + "\n"
			}
			golden(t, name, out)
		})
	}
}

func TestSocketFromFlags(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		opts Options
		args []string
		want string
	}{
		"default home":   {Options{Home: "/h"}, nil, "/h/visor.sock"},
		"default socket": {Options{Home: "/h", Socket: "/s.sock"}, nil, "/s.sock"},
		"home flag":      {Options{Home: "/h", Socket: "/s.sock"}, []string{"--home", "/x"}, "/x/visor.sock"},
		"socket flag":    {Options{Home: "/h"}, []string{"--home", "/x", "--socket", "/y.sock"}, "/y.sock"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got string
			tc.opts.Stdout, tc.opts.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
			tc.opts.Dial = func(_ context.Context, socket string) (Visor, error) {
				got = socket
				return newFakeVisor(), nil
			}
			args := append(append([]string{"agent-visor"}, tc.args...), "project", "ls")
			if err := Command(tc.opts).Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("socket %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMountUnderProduct(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := Command(Options{Name: "visor", Home: "/h", Stdout: &out, Stderr: &out, Dial: func(context.Context, string) (Visor, error) {
		return nil, os.ErrNotExist
	}})
	err := root.Run(t.Context(), []string{"visor", "status"})
	if err == nil || !strings.Contains(err.Error(), `"visor serve"`) {
		t.Fatalf("status with no visor: %v", err)
	}
	err = root.Run(t.Context(), []string{"visor", "upgrade"})
	if !errors.Is(err, errNotAvailable) || !strings.HasPrefix(err.Error(), "upgrade:") {
		t.Fatalf("upgrade seam: %v", err)
	}
}

func TestParseEnvHidesValues(t *testing.T) {
	t.Parallel()
	_, err := parseEnv([]string{"A=1", "s3cr3t"})
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("parse env error: %v", err)
	}
}

// runFake runs the command tree against the fake visor.
func runFake(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := Command(Options{
		Home: "/visor", Build: "test", Stdout: &out, Stderr: &errOut,
		Dial: func(context.Context, string) (Visor, error) { return newFakeVisor(), nil },
	})
	err := cmd.Run(t.Context(), append([]string{"agent-visor"}, args...))
	return out.String(), err
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run with -update): %v", err)
	}
	if got != string(want) {
		t.Fatalf("%s differs:\n--- got\n%s--- want\n%s", name, got, want)
	}
}

func newFakeVisor() *fakeVisor {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &fakeVisor{
		projects: []agentvisor.Project{
			{ID: "prj_app", Name: "app", Path: "/src/app", Git: true, CreatedAt: at},
			{ID: "prj_notes", Name: "notes", Path: "/src/notes", CreatedAt: at},
		},
		workdirs: []agentvisor.Workdir{
			{ID: "wd_main", Project: "prj_app", Kind: agentvisor.WorkdirMain, Path: "/src/app", Branch: "main", State: agentvisor.WorkdirActive, PortBase: 41000, CreatedAt: at, UpdatedAt: at},
			{ID: "wd_feat", Project: "prj_app", Key: "feat", Kind: agentvisor.WorkdirWorktree, Path: "/visor/worktrees/app/feat", Branch: "feat-x", Base: "main", State: agentvisor.WorkdirActive, PortBase: 41010, CreatedAt: at, UpdatedAt: at},
			{ID: "wd_old", Project: "prj_app", Key: "old", Kind: agentvisor.WorkdirWorktree, Path: "/visor/worktrees/app/old", Branch: "old", State: agentvisor.WorkdirArchived, PortBase: 41020, CreatedAt: at, UpdatedAt: at},
			{ID: "wd_notes", Project: "prj_notes", Kind: agentvisor.WorkdirFolder, Path: "/src/notes", State: agentvisor.WorkdirActive, PortBase: 41030, CreatedAt: at, UpdatedAt: at},
		},
		sessions: []agentvisor.Session{
			{ID: "ses_a", Project: "prj_app", Workdir: "wd_main", Kind: agentvisor.KindWire, Role: agentvisor.RoleAgent, Title: "Fix the flaky test", Harness: "claude", Model: "opus", WorkingDir: "/src/app", State: agentvisor.StateIdle, VendorID: "v-a", Labels: map[string]string{"app.run": "r1"}, EnvKeys: []string{"API_TOKEN"}, Seq: 12, Turns: 3, CreatedAt: at, UpdatedAt: at},
			{ID: "ses_b", Project: "prj_app", Workdir: "wd_feat", Kind: agentvisor.KindWire, Role: agentvisor.RoleAgent, Harness: "codex", WorkingDir: "/visor/worktrees/app/feat", State: agentvisor.StateWaiting, Pending: []agentvisor.Request{{ID: "p1", Tool: "Bash", Question: "Run it?"}}, Seq: 20, Turns: 1, CreatedAt: at, UpdatedAt: at},
			{ID: "ses_c", Project: "prj_app", Workdir: "wd_main", Kind: agentvisor.KindPTY, Role: agentvisor.RoleShell, WorkingDir: "/src/app", State: agentvisor.StateRunning, PID: 4242, Seq: 21, CreatedAt: at, UpdatedAt: at},
			{ID: "ses_d", Project: "prj_notes", Workdir: "wd_notes", Kind: agentvisor.KindWire, Role: agentvisor.RoleAgent, Harness: "claude", WorkingDir: "/src/notes", State: agentvisor.StateExited, Seq: 30, Turns: 2, CreatedAt: at, UpdatedAt: at},
		},
	}
}
