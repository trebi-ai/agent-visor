// Package harness detects, installs, updates, and logs in to the vendor
// agent CLIs, and lists their models. The vendor facts come from agent-wire.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/internal/store"
)

// AuthMode tells which login a harness uses.
type AuthMode string

// Source tells where the harness binary comes from.
type Source string

// AuthState is the login state of a harness.
type AuthState string

// Harness is one vendor agent CLI on this machine.
type Harness struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// Latest is the newest vendor release, when known.
	Latest string `json:"latest,omitempty"`
	Path   string `json:"path,omitempty"`
	Source Source `json:"source,omitempty"`
	// Installs are the versions that the visor installed, oldest first.
	Installs []string `json:"installs,omitempty"`
	// Installable means that the visor can install its own copy.
	Installable bool `json:"installable"`
	// Supported means that the installed version meets the agent-wire floor.
	Supported bool `json:"supported"`
	// Detail tells why the harness is not usable.
	Detail   string    `json:"detail,omitempty"`
	AuthMode AuthMode  `json:"auth_mode"`
	Auth     AuthState `json:"auth"`
	// EnvKeys lists the harness env keys. The values never leave the visor.
	EnvKeys []string `json:"env_keys,omitempty"`
	Limits  *Limits  `json:"limits,omitempty"`
	// Models is the fixed model list. Manager.Models gives the live list.
	Models []Model `json:"models,omitempty"`
	// Login is the login that runs now, or nil.
	Login *Login `json:"login,omitempty"`
}

// Model is one model that a harness offers.
type Model struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default,omitempty"`
	// Efforts are the effort levels, from low to high.
	Efforts []string `json:"efforts,omitempty"`
}

// Limits is the plan limit state of the account behind a harness.
type Limits struct {
	Plan     string        `json:"plan,omitempty"`
	Rejected bool          `json:"rejected,omitempty"`
	Windows  []LimitWindow `json:"windows,omitempty"`
}

// LimitWindow is the use of one plan limit window.
type LimitWindow struct {
	Key         string    `json:"key"`
	Scope       string    `json:"scope,omitempty"`
	Minutes     int       `json:"minutes,omitempty"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// Launch is what a session start needs from the harness.
type Launch struct {
	// Binary is the visor copy. Empty searches PATH.
	Binary string
	// Home is the login home of a separate login. Empty uses the user login.
	Home string
	// Env is the harness env layer.
	Env map[string]string
	// Args are launch args that the harness needs, before the session args.
	Args []string
}

// Wire is what the manager needs from the agent-wire runtime.
type Wire interface {
	Detect(ctx context.Context, h agentwire.Harness) (agentwire.Detection, error)
	Version(ctx context.Context, bin string) string
	Models(ctx context.Context, h agentwire.Harness, q agentwire.ModelQuery) ([]agentwire.ModelInfo, error)
	LatestVersion(ctx context.Context, h agentwire.Harness) (string, error)
	InvalidateModels(h agentwire.Harness)
}

// Facts are the vendor facts with no runtime. A nil field uses agent-wire.
// Tests replace them.
type Facts struct {
	Recipe          func(h agentwire.Harness) (agentwire.InstallRecipe, bool)
	LoginCommand    func(h agentwire.Harness) []string
	LogoutCommand   func(h agentwire.Harness) []string
	LoginEnv        func(h agentwire.Harness, home string) map[string]string
	CredentialFiles func(h agentwire.Harness) []string
	AuthStatus      func(ctx context.Context, h agentwire.Harness, home string) (agentwire.AuthState, error)
}

// Options configures a Manager.
type Options struct {
	// Names are the harnesses that the manager knows. Empty means each
	// agent-wire harness with an install recipe.
	Names []string
	// Path is the search path for the install tools, such as npm. Empty
	// uses PATH.
	Path  string
	Facts Facts
	// LoginWait is how long Login waits for a URL or a code. Zero is 20 s.
	LoginWait time.Duration
	// LoginTimeout stops a login that does not end. Zero is 10 min.
	LoginTimeout time.Duration
	// InUse reports that a live session uses the harness. The visor sets it.
	InUse func(name string) bool
	// OnChange gets the name of a harness that changed. The visor sets it.
	OnChange func(name string)
	Logger   *slog.Logger
}

// Manager keeps the harness inventory of one visor.
type Manager struct {
	home  string
	st    *store.Store
	wire  Wire
	o     Options
	log   *slog.Logger
	names []string

	ctx    context.Context //nolint:containedctx // the lifetime context that Close cancels
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// stateMu orders the read, change, and write of a state document.
	stateMu sync.Mutex

	mu     sync.Mutex
	closed bool
	ops    map[string]string
	auth   map[string]authEntry
	latest map[string]latestEntry
	logins map[string]*login
}

// state is the stored state of one harness.
type state struct {
	AuthMode AuthMode `json:"auth_mode,omitempty"`
	Limits   *Limits  `json:"limits,omitempty"`
}

type authEntry struct {
	at    time.Time
	state AuthState
}

type latestEntry struct {
	at      time.Time
	version string
	failed  bool
	running bool
}

// Auth modes.
const (
	// AuthShared uses the login of the user, in the user home of the harness.
	AuthShared AuthMode = "shared"
	// AuthSeparate uses a login of the visor only, in <home>/harness/<name>.
	AuthSeparate AuthMode = "separate"
)

// Sources.
const (
	SourceVisor  Source = "visor"
	SourceSystem Source = "system"
)

// Auth states.
const (
	AuthUnknown AuthState = "unknown"
	AuthOK      AuthState = "ok"
	AuthMissing AuthState = "missing"
	AuthExpired AuthState = "expired"
)

const (
	authTTL         = time.Minute
	latestTTL       = 6 * time.Hour
	latestFailedTTL = 10 * time.Minute
	latestTimeout   = 15 * time.Second
)

// Errors.
var (
	ErrNotFound    = errors.New("harness: not found")
	ErrInvalid     = errors.New("harness: invalid request")
	ErrState       = errors.New("harness: wrong state")
	ErrBusy        = errors.New("harness: busy")
	ErrUnsupported = errors.New("harness: not supported")
)

var (
	semverRE  = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?`)
	versionRE = regexp.MustCompile(`^\d+(?:\.\d+){0,2}(?:-[0-9A-Za-z.-]+)?$`)
	envKeyRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// New makes the manager of the harnesses under home.
func New(home string, st *store.Store, wire Wire, o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	f := &o.Facts
	if f.Recipe == nil {
		f.Recipe = agentwire.Recipe
	}
	if f.LoginCommand == nil {
		f.LoginCommand = agentwire.LoginCommand
	}
	if f.LogoutCommand == nil {
		f.LogoutCommand = agentwire.LogoutCommand
	}
	if f.LoginEnv == nil {
		f.LoginEnv = agentwire.LoginEnv
	}
	if f.CredentialFiles == nil {
		f.CredentialFiles = agentwire.CredentialFiles
	}
	if f.AuthStatus == nil {
		f.AuthStatus = agentwire.AuthStatus
	}
	if o.LoginWait == 0 {
		o.LoginWait = 20 * time.Second
	}
	if o.LoginTimeout == 0 {
		o.LoginTimeout = 10 * time.Minute
	}
	names := slices.Clone(o.Names)
	if len(names) == 0 {
		for _, h := range agentwire.Harnesses() {
			if _, ok := f.Recipe(h); ok {
				names = append(names, string(h))
			}
		}
	}
	m := &Manager{
		home: home, st: st, wire: wire, o: o, log: o.Logger.With("source", "harness"), names: names,
		ops: map[string]string{}, auth: map[string]authEntry{}, latest: map[string]latestEntry{}, logins: map[string]*login{},
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

// List gives each known harness.
func (m *Manager) List(ctx context.Context) ([]Harness, error) {
	out := make([]Harness, len(m.names))
	errs := make([]error, len(m.names))
	var wg sync.WaitGroup
	for i, name := range m.names {
		wg.Go(func() { out[i], errs[i] = m.Harness(ctx, name) })
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// Harness gives one harness.
func (m *Manager) Harness(ctx context.Context, name string) (Harness, error) {
	if !m.known(name) {
		return Harness{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	h := agentwire.Harness(name)
	r, hasRecipe := m.o.Facts.Recipe(h)
	st, err := m.state(ctx, name)
	if err != nil {
		return Harness{}, err
	}
	env, err := m.env(name)
	if err != nil {
		return Harness{}, err
	}
	out := Harness{
		Name: name, Installable: hasRecipe && r.Method == agentwire.InstallNPM, Installs: m.installs(name),
		AuthMode: st.mode(), Limits: st.Limits, EnvKeys: slices.Sorted(maps.Keys(env)), Latest: m.cachedLatest(name),
	}
	if v, path, ok := m.current(name); ok {
		out.Installed, out.Version, out.Path, out.Source, out.Supported = true, v, path, SourceVisor, true
	} else {
		d, err := m.wire.Detect(ctx, h)
		if err != nil {
			return Harness{}, fmt.Errorf("detect %s: %w", name, err)
		}
		if d.Path != "" {
			out.Installed, out.Path, out.Source = true, d.Path, SourceSystem
			out.Version = cleanVersion(d.Version)
		}
		out.Supported, out.Detail = d.Supported, d.Detail
	}
	out.Auth = m.authState(ctx, name, out.AuthMode, env)
	for _, mi := range agentwire.StaticModels(h) {
		out.Models = append(out.Models, modelOf(h, mi))
	}
	m.mu.Lock()
	if l := m.logins[name]; l != nil {
		snap := l.state
		out.Login = &snap
	}
	m.mu.Unlock()
	return out, nil
}

// SetAuthMode moves the harness to a shared or a separate login. A live
// session keeps its login; the next session gets the new one.
func (m *Manager) SetAuthMode(ctx context.Context, name string, mode AuthMode) (Harness, error) {
	if !m.known(name) {
		return Harness{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	if mode != AuthShared && mode != AuthSeparate {
		return Harness{}, fmt.Errorf("%w: auth mode %q", ErrInvalid, mode)
	}
	if mode == AuthSeparate {
		if err := os.MkdirAll(m.separateHome(name), 0o700); err != nil {
			return Harness{}, fmt.Errorf("make the login home of %s: %w", name, err)
		}
	}
	changed, err := m.changeState(ctx, name, func(s *state) { s.AuthMode = mode })
	if err != nil {
		return Harness{}, err
	}
	if changed {
		m.changed(name)
	}
	return m.Harness(ctx, name)
}

// SetEnv replaces the env of a harness. An empty env removes it. The file
// has mode 0600.
func (m *Manager) SetEnv(_ context.Context, name string, env map[string]string) error {
	if !m.known(name) {
		return fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	for k, v := range env {
		if !envKeyRE.MatchString(k) {
			return fmt.Errorf("%w: env key %q", ErrInvalid, k)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("%w: the value of env key %s has a NUL byte", ErrInvalid, k)
		}
	}
	path := m.envPath(name)
	if len(env) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the env of %s: %w", name, err)
		}
	} else if err := writePrivate(path, env); err != nil {
		return fmt.Errorf("write the env of %s: %w", name, err)
	}
	m.changed(name)
	return nil
}

// Launch gives what a session of the harness needs. An unknown harness
// gives the zero Launch, so a session with no inventory entry still starts.
func (m *Manager) Launch(ctx context.Context, name string) (Launch, error) {
	var l Launch
	if !m.known(name) {
		return l, nil
	}
	h := agentwire.Harness(name)
	st, err := m.state(ctx, name)
	if err != nil {
		return l, err
	}
	env, err := m.env(name)
	if err != nil {
		return l, err
	}
	l.Env = map[string]string{}
	if _, path, ok := m.current(name); ok {
		// The visor owns the updates of its copy.
		l.Binary = path
		if r, ok := m.o.Facts.Recipe(h); ok {
			maps.Copy(l.Env, r.UpdateOffEnv)
			l.Args = slices.Clone(r.UpdateOffArgs)
		}
	}
	if st.mode() == AuthSeparate {
		l.Home = m.separateHome(name)
		if err := os.MkdirAll(l.Home, 0o700); err != nil {
			return l, fmt.Errorf("make the login home of %s: %w", name, err)
		}
		maps.Copy(l.Env, m.o.Facts.LoginEnv(h, l.Home))
	}
	maps.Copy(l.Env, env)
	return l, nil
}

// Models lists the models of a harness: the live list when the harness
// gives one, else the fixed list.
func (m *Manager) Models(ctx context.Context, name string) ([]Model, error) {
	l, err := m.Launch(ctx, name)
	if err != nil {
		return nil, err
	}
	if !m.known(name) {
		return nil, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	h := agentwire.Harness(name)
	list, err := m.wire.Models(ctx, h, agentwire.ModelQuery{Binary: l.Binary, Home: l.Home, Env: l.Env})
	if err != nil || len(list) == 0 {
		list = agentwire.StaticModels(h)
	}
	if len(list) == 0 {
		if err != nil {
			return nil, fmt.Errorf("models of %s: %w", name, err)
		}
		return []Model{}, nil
	}
	out := make([]Model, 0, len(list))
	for _, mi := range list {
		out = append(out, modelOf(h, mi))
	}
	return out, nil
}

// SetLimits keeps the last plan limits that a session of the harness saw.
func (m *Manager) SetLimits(ctx context.Context, name string, l Limits) {
	if !m.known(name) {
		return
	}
	changed, err := m.changeState(ctx, name, func(s *state) { s.Limits = &l })
	if err != nil {
		m.log.Warn("keep the plan limits", "harness", name, "err", err)
		return
	}
	if changed {
		m.changed(name)
	}
}

// Busy reports that a login, an install, or an update runs.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ops) > 0 || len(m.logins) > 0
}

// Close stops the logins and the release lookups.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) known(name string) bool { return slices.Contains(m.names, name) }

func (m *Manager) separateHome(name string) string { return filepath.Join(m.home, "harness", name) }

func (m *Manager) envPath(name string) string {
	return filepath.Join(m.home, "harness", name+".env.json")
}

// env reads the harness env. An error never holds a value.
func (m *Manager) env(name string) (map[string]string, error) {
	data, err := os.ReadFile(m.envPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the env of %s: %w", name, err)
	}
	env := map[string]string{}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("read the env of %s: the file does not parse", name)
	}
	return env, nil
}

func (m *Manager) state(ctx context.Context, name string) (state, error) {
	var s state
	data, err := m.st.Get(ctx, store.KindHarness, name)
	if errors.Is(err, store.ErrNotFound) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("decode harness %s: %w", name, err)
	}
	return s, nil
}

// changeState changes the stored state and reports whether it changed.
func (m *Manager) changeState(ctx context.Context, name string, f func(*state)) (bool, error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	s, err := m.state(ctx, name)
	if err != nil {
		return false, err
	}
	before := s
	f(&s)
	if reflect.DeepEqual(before, s) {
		return false, nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return false, fmt.Errorf("encode harness %s: %w", name, err)
	}
	return true, m.st.Put(context.WithoutCancel(ctx), store.KindHarness, name, data)
}

// authState gives the login state, from a short cache. A token key in the
// harness env counts as a login.
func (m *Manager) authState(ctx context.Context, name string, mode AuthMode, env map[string]string) AuthState {
	h := agentwire.Harness(name)
	for _, k := range agentwire.TokenEnvKeys(h) {
		if env[k] != "" {
			return AuthOK
		}
	}
	m.mu.Lock()
	e, ok := m.auth[name]
	m.mu.Unlock()
	if ok && time.Since(e.at) < authTTL {
		return e.state
	}
	home := ""
	if mode == AuthSeparate {
		home = m.separateHome(name)
	}
	st, err := m.o.Facts.AuthStatus(ctx, h, home)
	if err != nil {
		st = agentwire.AuthUnknown
	}
	out := AuthState(st)
	if err == nil {
		m.mu.Lock()
		m.auth[name] = authEntry{at: time.Now(), state: out}
		m.mu.Unlock()
	}
	return out
}

// cachedLatest gives the known latest release, and starts a lookup when it
// is old.
func (m *Manager) cachedLatest(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.latest[name]
	ttl := latestTTL
	if e.failed {
		ttl = latestFailedTTL
	}
	if m.closed || e.running || (!e.at.IsZero() && time.Since(e.at) < ttl) {
		return e.version
	}
	e.running = true
	m.latest[name] = e
	m.wg.Go(func() {
		ctx, cancel := context.WithTimeout(m.ctx, latestTimeout)
		defer cancel()
		v, err := m.lookupLatest(ctx, name)
		if err != nil {
			m.log.Debug("look up the latest release", "harness", name, "err", err)
		} else if v != e.version {
			m.changed(name)
		}
	})
	return e.version
}

// lookupLatest asks the vendor registry and keeps the answer.
func (m *Manager) lookupLatest(ctx context.Context, name string) (string, error) {
	v, err := m.wire.LatestVersion(ctx, agentwire.Harness(name))
	v = cleanVersion(v)
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.latest[name]
	e.at, e.running, e.failed = time.Now(), false, err != nil
	if err == nil {
		e.version = v
	}
	m.latest[name] = e
	return v, err
}

// changed drops the cached login state and tells the visor.
func (m *Manager) changed(name string) {
	m.mu.Lock()
	delete(m.auth, name)
	m.mu.Unlock()
	m.wire.InvalidateModels(agentwire.Harness(name))
	if m.o.OnChange != nil {
		m.o.OnChange(name)
	}
}

func (m *Manager) inUse(name string) bool { return m.o.InUse != nil && m.o.InUse(name) }

// begin marks one change of a harness. It returns ErrBusy when another
// change of the same harness runs.
func (m *Manager) begin(name, op string) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("%w: the manager is closed", ErrState)
	}
	if cur, ok := m.ops[name]; ok {
		return nil, fmt.Errorf("%w: %s of %s runs", ErrBusy, cur, name)
	}
	m.ops[name] = op
	return func() {
		m.mu.Lock()
		delete(m.ops, name)
		m.mu.Unlock()
	}, nil
}

func (s state) mode() AuthMode {
	if s.AuthMode == "" {
		return AuthShared
	}
	return s.AuthMode
}

func modelOf(h agentwire.Harness, mi agentwire.ModelInfo) Model {
	return Model{ID: mi.ID, Name: mi.Name, Description: mi.Description, Default: mi.Default, Efforts: agentwire.Efforts(h, mi.ID)}
}

// cleanVersion takes the version number from a --version line.
func cleanVersion(s string) string {
	if v := semverRE.FindString(s); v != "" {
		return v
	}
	return strings.TrimSpace(s)
}

// writePrivate writes v as JSON with mode 0600, in a folder with mode 0700.
func writePrivate(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
