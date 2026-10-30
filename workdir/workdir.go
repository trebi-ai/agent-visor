// Package workdir finds projects from paths and keeps workdirs: the main
// checkout of a repository, git worktrees, and plain folders.
package workdir

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kind is the kind of a workdir.
type Kind string

// State is the state of a workdir.
type State string

// Project is a repository root or a plain folder.
type Project struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Git       bool              `json:"git"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// Workdir is one folder to work in, and for git one branch.
type Workdir struct {
	ID        string            `json:"id"`
	Project   string            `json:"project"`
	Key       string            `json:"key,omitempty"`
	Kind      Kind              `json:"kind"`
	Path      string            `json:"path"`
	Branch    string            `json:"branch,omitempty"`
	Base      string            `json:"base,omitempty"`
	State     State             `json:"state"`
	PortBase  int               `json:"port_base"`
	Setup     []string          `json:"setup,omitempty"`
	Teardown  []string          `json:"teardown,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Spec asks for a workdir. The empty Kind means main for a repository and
// folder for a plain folder.
type Spec struct {
	// Path is a folder in the repository, or a plain folder.
	Path string `json:"path"`
	// Key is optional and unique in the project. A key gives the same
	// workdir on each call.
	Key    string `json:"key,omitempty"`
	Kind   Kind   `json:"kind,omitempty"`
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	// Setup and Teardown are shell commands. When both are empty, the
	// repository file .agent-visor.json gives them.
	Setup    []string          `json:"setup,omitempty"`
	Teardown []string          `json:"teardown,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// Filter selects workdirs. The empty value selects all of them.
type Filter struct {
	Project string
	Key     string
	State   State
}

// Script is the result of one setup or teardown script.
type Script struct {
	Workdir string
	Name    string // setup or teardown
	Command string
	Output  []byte
	Err     error
}

// Docs is the document store that the manager uses.
type Docs interface {
	Put(ctx context.Context, kind, id string, data []byte) error
	Get(ctx context.Context, kind, id string) ([]byte, error)
	List(ctx context.Context, kind string) ([][]byte, error)
	Delete(ctx context.Context, kind, id string) error
}

// Manager owns the project and workdir rows and the worktree folders.
type Manager struct {
	docs     Docs
	root     string
	log      *slog.Logger
	onScript func(Script)
	mu       sync.Mutex
}

// Workdir kinds and states.
const (
	KindMain     Kind  = "main"
	KindWorktree Kind  = "worktree"
	KindFolder   Kind  = "folder"
	Active       State = "active"
	Archived     State = "archived"

	docProject = "project"
	docWorkdir = "workdir"
	// RepoFile is the repository file with the setup and teardown scripts.
	RepoFile  = ".agent-visor.json"
	portFirst = 41000
	portSpan  = 10
)

// Errors.
var (
	ErrNotFound = errors.New("workdir: not found")
	ErrDirty    = errors.New("workdir: the worktree has changes")
	ErrHasWork  = errors.New("workdir: the project has workdirs")
)

// New makes a manager. root holds the worktrees that the manager makes.
// onScript receives the result of each script; it can be nil.
func New(docs Docs, root string, log *slog.Logger, onScript func(Script)) *Manager {
	if log == nil {
		log = slog.Default()
	}
	if onScript == nil {
		onScript = func(Script) {}
	}
	return &Manager{docs: docs, root: root, log: log.With("source", "workdir"), onScript: onScript}
}

// Projects lists the projects. It forgets a project whose folder is gone and
// that has no workdirs.
func (m *Manager) Projects(ctx context.Context) ([]Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	projects, err := m.projects(ctx)
	if err != nil {
		return nil, err
	}
	workdirs, err := m.workdirs(ctx)
	if err != nil {
		return nil, err
	}
	out := projects[:0]
	for _, p := range projects {
		if _, err := os.Stat(p.Path); errors.Is(err, os.ErrNotExist) && !slices.ContainsFunc(workdirs, func(w Workdir) bool { return w.Project == p.ID }) {
			if err := m.docs.Delete(ctx, docProject, p.ID); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// Project reads one project.
func (m *Manager) Project(ctx context.Context, id string) (Project, error) {
	var p Project
	return p, m.get(ctx, docProject, id, &p)
}

// ForgetProject removes a project with no workdirs.
func (m *Manager) ForgetProject(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.Project(ctx, id); err != nil {
		return err
	}
	workdirs, err := m.workdirs(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(workdirs, func(w Workdir) bool { return w.Project == id }) {
		return fmt.Errorf("forget project %s: %w", id, ErrHasWork)
	}
	return m.docs.Delete(ctx, docProject, id)
}

// Workdirs lists the workdirs that match f.
func (m *Manager) Workdirs(ctx context.Context, f Filter) ([]Workdir, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.workdirs(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, w := range all {
		if f.match(w) {
			out = append(out, w)
		}
	}
	return out, nil
}

// Workdir reads one workdir.
func (m *Manager) Workdir(ctx context.Context, id string) (Workdir, error) {
	var w Workdir
	return w, m.get(ctx, docWorkdir, id, &w)
}

// ForPath gives the workdir that holds dir. It makes the project and a main
// or folder workdir the first time that it sees the place.
func (m *Manager) ForPath(ctx context.Context, dir string) (Workdir, error) {
	return m.Ensure(ctx, Spec{Path: dir})
}

// Ensure gives the workdir for s. With a key, it makes the workdir once and
// then returns the same row.
func (m *Manager) Ensure(ctx context.Context, s Spec) (Workdir, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, err := filepath.Abs(s.Path)
	if err != nil {
		return Workdir{}, fmt.Errorf("ensure workdir: %w", err)
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return Workdir{}, fmt.Errorf("ensure workdir: %w", err)
	}
	place, err := locate(ctx, dir)
	if err != nil {
		return Workdir{}, err
	}
	if s.Kind == KindWorktree && !place.git {
		return Workdir{}, fmt.Errorf("ensure workdir: %s is not a git repository", dir)
	}
	p, err := m.ensureProject(ctx, place)
	if err != nil {
		return Workdir{}, err
	}
	if !place.git {
		place.project, place.top = p.Path, p.Path
	}
	all, err := m.workdirs(ctx)
	if err != nil {
		return Workdir{}, err
	}
	if w, ok := find(all, p.ID, s.Key, place, dir); ok {
		return w, nil
	}
	w, err := m.create(ctx, p, place, s, all)
	if err != nil {
		return Workdir{}, err
	}
	m.runScripts(ctx, w, "setup", w.Setup)
	return w, nil
}

// Archive marks a workdir archived. It keeps the folder and the branch.
func (m *Manager) Archive(ctx context.Context, id string) (Workdir, error) {
	return m.setState(ctx, id, Archived)
}

// Restore makes an archived workdir active again.
func (m *Manager) Restore(ctx context.Context, id string) (Workdir, error) {
	return m.setState(ctx, id, Active)
}

// Remove runs the teardown script and removes the workdir. A worktree folder
// is deleted; force also deletes one with changes. A main or folder workdir
// keeps its folder.
func (m *Manager) Remove(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.Workdir(ctx, id)
	if err != nil {
		return err
	}
	if w.Kind == KindWorktree {
		if err := m.removeWorktree(ctx, w, force); err != nil {
			return err
		}
	} else {
		m.runScripts(ctx, w, "teardown", w.Teardown)
	}
	return m.docs.Delete(ctx, docWorkdir, id)
}

func (m *Manager) removeWorktree(ctx context.Context, w Workdir, force bool) error {
	if _, err := os.Stat(w.Path); err != nil {
		return nil //nolint:nilerr // a folder that is gone needs no git step
	}
	status, err := git(ctx, w.Path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" && !force {
		return fmt.Errorf("remove workdir %s: %w", w.ID, ErrDirty)
	}
	m.runScripts(ctx, w, "teardown", w.Teardown)
	p, err := m.Project(ctx, w.Project)
	if err != nil {
		return err
	}
	args := []string{"worktree", "remove", w.Path}
	if force {
		args = append(args, "--force")
	}
	_, err = git(ctx, p.Path, args...)
	return err
}

func (m *Manager) setState(ctx context.Context, id string, st State) (Workdir, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.Workdir(ctx, id)
	if err != nil {
		return Workdir{}, err
	}
	w.State, w.UpdatedAt = st, time.Now().UTC()
	return w, m.put(ctx, docWorkdir, w.ID, w)
}

func (m *Manager) ensureProject(ctx context.Context, pl place) (Project, error) {
	projects, err := m.projects(ctx)
	if err != nil {
		return Project{}, err
	}
	for _, p := range projects {
		// A folder in a plain folder project belongs to that project.
		if p.Path == pl.project || (!pl.git && !p.Git && within(pl.project, p.Path)) {
			return p, nil
		}
	}
	p := Project{ID: newID("prj"), Name: filepath.Base(pl.project), Path: pl.project, Git: pl.git, CreatedAt: time.Now().UTC()}
	return p, m.put(ctx, docProject, p.ID, p)
}

func (m *Manager) create(ctx context.Context, p Project, pl place, s Spec, all []Workdir) (Workdir, error) {
	now := time.Now().UTC()
	w := Workdir{
		ID: newID("wd"), Project: p.ID, Key: s.Key, Kind: s.Kind, Path: pl.top, Branch: s.Branch, Base: s.Base,
		State: Active, PortBase: freePortBase(all), Setup: s.Setup, Teardown: s.Teardown, Labels: s.Labels,
		CreatedAt: now, UpdatedAt: now,
	}
	switch {
	case w.Kind == KindWorktree:
		if err := m.addWorktree(ctx, p, &w); err != nil {
			return Workdir{}, err
		}
	case !p.Git:
		w.Kind, w.Path = KindFolder, pl.top
	case pl.worktree:
		w.Kind = KindWorktree
	default:
		w.Kind = KindMain
	}
	if w.Branch == "" && p.Git {
		w.Branch, _ = git(ctx, w.Path, "rev-parse", "--abbrev-ref", "HEAD") //nolint:errcheck // a branch name is optional
	}
	if len(w.Setup) == 0 && len(w.Teardown) == 0 {
		w.Setup, w.Teardown = repoScripts(p.Path)
	}
	return w, m.put(ctx, docWorkdir, w.ID, w)
}

func (m *Manager) addWorktree(ctx context.Context, p Project, w *Workdir) error {
	name := w.Key
	if name == "" {
		name = w.ID
	}
	if w.Branch == "" {
		w.Branch = strings.ToLower(name)
	}
	parent := filepath.Join(m.root, filepath.Base(p.Path)+"-"+p.ID)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("ensure workdir: %w", err)
	}
	// git gives real paths, so the row keeps the real path too.
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("ensure workdir: %w", err)
	}
	w.Path = filepath.Join(parent, name)
	// A worktree folder that someone removed stays registered until a prune.
	if _, err := git(ctx, p.Path, "worktree", "prune"); err != nil {
		return err
	}
	args := []string{"worktree", "add", w.Path, w.Branch}
	if _, err := git(ctx, p.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+w.Branch); err != nil {
		args = []string{"worktree", "add", "-b", w.Branch, w.Path}
		if w.Base != "" {
			args = append(args, w.Base)
		}
	}
	_, err = git(ctx, p.Path, args...)
	return err
}

func (m *Manager) runScripts(ctx context.Context, w Workdir, name string, cmds []string) {
	for _, c := range cmds {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", c)
		cmd.Dir = w.Path
		cmd.Env = append(os.Environ(), fmt.Sprintf("AGENT_VISOR_PORT=%d", w.PortBase), "AGENT_VISOR_WORKDIR="+w.ID)
		out, err := cmd.CombinedOutput()
		if err != nil {
			m.log.Warn("workdir script failed", "workdir_id", w.ID, "script", name, "err", err)
		}
		m.onScript(Script{Workdir: w.ID, Name: name, Command: c, Output: out, Err: err})
	}
}

func (m *Manager) projects(ctx context.Context) ([]Project, error) {
	return listDocs[Project](ctx, m.docs, docProject)
}

func (m *Manager) workdirs(ctx context.Context) ([]Workdir, error) {
	return listDocs[Workdir](ctx, m.docs, docWorkdir)
}

func (m *Manager) get(ctx context.Context, kind, id string, v any) error {
	data, err := m.docs.Get(ctx, kind, id)
	if err != nil {
		return fmt.Errorf("%s %s: %w", kind, id, ErrNotFound)
	}
	return json.Unmarshal(data, v)
}

func (m *Manager) put(ctx context.Context, kind, id string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.docs.Put(ctx, kind, id, data)
}

func (f Filter) match(w Workdir) bool {
	return (f.Project == "" || f.Project == w.Project) && (f.Key == "" || f.Key == w.Key) && (f.State == "" || f.State == w.State)
}

// place is where a path lives: its project root and its checkout root.
type place struct {
	project  string // the main checkout, or the folder
	top      string // the checkout that holds the path
	git      bool
	worktree bool // top is a linked worktree of project
}

func locate(ctx context.Context, dir string) (place, error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return place{}, fmt.Errorf("ensure workdir: %s is not a folder", dir)
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return place{project: dir, top: dir}, nil //nolint:nilerr // a plain folder is not an error
	}
	// git before 2.31 has no --path-format, and gives a path relative to dir.
	common, err := git(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return place{}, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	main := top
	if filepath.Base(common) == ".git" {
		main = filepath.Dir(common)
	}
	if r, err := filepath.EvalSymlinks(main); err == nil {
		main = r
	}
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	return place{project: main, top: top, git: true, worktree: main != top}, nil
}

// find gives the workdir for a key, or the active workdir that holds dir.
func find(all []Workdir, project, key string, pl place, dir string) (Workdir, bool) {
	for _, w := range all {
		if w.Project != project {
			continue
		}
		if key != "" {
			if w.Key == key {
				return w, true
			}
			continue
		}
		if w.Path == pl.top || (!pl.git && within(dir, w.Path)) {
			return w, true
		}
	}
	return Workdir{}, false
}

func within(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func freePortBase(all []Workdir) int {
	used := map[int]bool{}
	for _, w := range all {
		used[w.PortBase] = true
	}
	base := portFirst
	for used[base] {
		base += portSpan
	}
	return base
}

func repoScripts(root string) (setup, teardown []string) {
	data, err := os.ReadFile(filepath.Join(root, RepoFile))
	if err != nil {
		return nil, nil
	}
	var f struct {
		Setup    []string `json:"setup"`
		Teardown []string `json:"teardown"`
	}
	if json.Unmarshal(data, &f) != nil {
		return nil, nil
	}
	return f.Setup, f.Teardown
}

func listDocs[T any](ctx context.Context, d Docs, kind string) ([]T, error) {
	raw, err := d.List(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(raw))
	for _, b := range raw {
		var v T
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", kind, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		text := strings.TrimSpace(stderr.String())
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], text)
	}
	return strings.TrimSpace(string(out)), nil
}

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return prefix + "_" + hex.EncodeToString(b)
}
