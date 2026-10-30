package workdir_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trebi-ai/agent-visor/internal/store"
	"github.com/trebi-ai/agent-visor/workdir"
)

type scripts struct {
	mu  sync.Mutex
	got []workdir.Script
}

func (s *scripts) add(r workdir.Script) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r)
}

func (s *scripts) names() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.got {
		out = append(out, r.Name+":"+strings.TrimSpace(string(r.Output)))
	}
	return strings.Join(out, ",")
}

func TestEnsureMainAndNestedFolder(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	repo := newRepo(t, map[string]string{})
	a, err := m.Ensure(t.Context(), workdir.Spec{Path: repo})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != workdir.KindMain || a.Path != repo || a.Branch != "main" || a.State != workdir.Active {
		t.Fatalf("main workdir: %+v", a)
	}
	b, err := m.ForPath(t.Context(), filepath.Join(repo, "sub", "deep"))
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID {
		t.Fatalf("a sub folder gives a new workdir %s, want %s", b.ID, a.ID)
	}
	ps, err := m.Projects(t.Context())
	if err != nil || len(ps) != 1 || ps[0].Path != repo || !ps[0].Git {
		t.Fatalf("projects: %+v %v", ps, err)
	}
}

func TestEnsurePlainFolder(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "inner"), 0o750); err != nil {
		t.Fatal(err)
	}
	a, err := m.ForPath(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.ForPath(t.Context(), filepath.Join(dir, "inner"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != workdir.KindFolder || b.ID != a.ID {
		t.Fatalf("folder workdirs: %+v %+v", a, b)
	}
	if _, err := m.ForPath(t.Context(), filepath.Join(dir, "none")); err == nil {
		t.Fatal("a missing folder gives no error")
	}
	if _, err := m.Ensure(t.Context(), workdir.Spec{Path: dir, Kind: workdir.KindWorktree}); err == nil {
		t.Fatal("a worktree in a plain folder gives no error")
	}
}

func TestWorktreeByKey(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	repo := newRepo(t, map[string]string{})
	spec := workdir.Spec{Path: filepath.Join(repo, "sub"), Key: "Fix-1", Kind: workdir.KindWorktree}
	a, err := m.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != workdir.KindWorktree || a.Branch != "fix-1" || a.Path == repo {
		t.Fatalf("worktree: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(a.Path, "sub", "deep", "a.txt")); err != nil {
		t.Fatalf("worktree content: %v", err)
	}
	b, err := m.Ensure(t.Context(), spec)
	if err != nil || b.ID != a.ID {
		t.Fatalf("the same key gives %+v %v, want %s", b, err, a.ID)
	}
	c, err := m.ForPath(t.Context(), a.Path)
	if err != nil || c.ID != a.ID {
		t.Fatalf("the worktree path gives %+v %v, want %s", c, err, a.ID)
	}
	main, err := m.ForPath(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if main.Project != a.Project || main.PortBase == a.PortBase {
		t.Fatalf("main %+v and worktree %+v: one project and two port ranges", main, a)
	}
	list, err := m.Workdirs(t.Context(), workdir.Filter{Key: "Fix-1"})
	if err != nil || len(list) != 1 {
		t.Fatalf("filter by key: %+v %v", list, err)
	}
}

func TestArchiveRestoreRemove(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	repo := newRepo(t, map[string]string{})
	w, err := m.Ensure(t.Context(), workdir.Spec{Path: repo, Key: "k", Kind: workdir.KindWorktree})
	if err != nil {
		t.Fatal(err)
	}
	if w, err = m.Archive(t.Context(), w.ID); err != nil || w.State != workdir.Archived {
		t.Fatalf("archive: %+v %v", w, err)
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf("archive removes the folder: %v", err)
	}
	if w, err = m.Restore(t.Context(), w.ID); err != nil || w.State != workdir.Active {
		t.Fatalf("restore: %+v %v", w, err)
	}
	if err := m.ForgetProject(t.Context(), w.Project); !errors.Is(err, workdir.ErrHasWork) {
		t.Fatalf("forget a project with workdirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(t.Context(), w.ID, false); !errors.Is(err, workdir.ErrDirty) {
		t.Fatalf("remove a dirty worktree: %v, want ErrDirty", err)
	}
	if err := m.Remove(t.Context(), w.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatalf("the folder stays: %v", err)
	}
	if _, err := m.Workdir(t.Context(), w.ID); !errors.Is(err, workdir.ErrNotFound) {
		t.Fatalf("removed workdir: %v", err)
	}
}

func TestScripts(t *testing.T) {
	t.Parallel()
	m, sc := newManager(t)
	repo := newRepo(t, map[string]string{
		workdir.RepoFile: `{"setup":["echo repo-setup $AGENT_VISOR_PORT"],"teardown":["echo repo-teardown"]}`,
	})
	w, err := m.Ensure(t.Context(), workdir.Spec{Path: repo, Key: "a", Kind: workdir.KindWorktree})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(t.Context(), w.ID, false); err != nil {
		t.Fatal(err)
	}
	want := "setup:repo-setup 41000,teardown:repo-teardown"
	if got := sc.names(); got != want {
		t.Fatalf("scripts %q, want %q", got, want)
	}
	other, err := m.Ensure(t.Context(), workdir.Spec{Path: repo, Key: "b", Setup: []string{"echo own"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := sc.names(); !strings.HasSuffix(got, ",setup:own") || len(other.Teardown) != 0 {
		t.Fatalf("spec scripts: %q %+v", got, other)
	}
}

func newManager(t *testing.T) (*workdir.Manager, *scripts) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "visor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // test cleanup
	sc := &scripts{}
	return workdir.New(st, filepath.Join(t.TempDir(), "worktrees"), nil, sc.add), sc
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// newRepo makes a git repository with one commit and a sub folder.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o750); err != nil {
		t.Fatal(err)
	}
	files["sub/deep/a.txt"] = "a"
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-q", "--no-verify", "-m", "first")
	return dir
}
