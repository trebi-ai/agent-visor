package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/internal/store"
)

// fakeNPM installs a fake harness into $NPM_CONFIG_PREFIX. The version 9.9.9
// fails, and "latest" is 3.0.0.
const fakeNPM = `#!/bin/sh
spec="$3"; ver="${spec##*@}"
[ "$ver" = 9.9.9 ] && { echo "npm ERR! no such version" >&2; exit 1; }
[ "$ver" = latest ] && ver=3.0.0
mkdir -p "$NPM_CONFIG_PREFIX/bin"
cat > "$NPM_CONFIG_PREFIX/bin/fakeh" <<EOF
#!/bin/sh
case "\$1" in
--version) echo "$ver (Fake)";;
auth)
  if [ "\$2" = logout ]; then rm -f "\$FAKEH_HOME/cred"; exit 0; fi
  if [ "\$2" = paste ]; then echo "Paste code here:"; read c; [ "\$c" = XYZ ] && touch "\$FAKEH_HOME/cred"; exit 0; fi
  if [ "\$2" = token ]; then echo "Your token: sk-ant-oat01-abcDEF123"; sleep 0.5; exit 0; fi
  printf "Open https://login.example.test/device to log in.\nThen type the code ABCD-EFGH.\n"
  sleep 0.3
  [ -n "\$FAKEH_HOME" ] && touch "\$FAKEH_HOME/cred"
  ;;
esac
EOF
chmod +x "$NPM_CONFIG_PREFIX/bin/fakeh"
`

const secret = "s3cret-value-123"

type fakeWire struct {
	mu      sync.Mutex
	latest  string
	system  string
	lookups atomic.Int32
	// asset answers Asset. Nil means that the vendor has no download.
	asset func(version string) (agentwire.Asset, error)
}

type rig struct {
	m       *Manager
	home    string
	user    string
	wire    *fakeWire
	inUse   atomic.Bool
	changes chan string
	logs    *bytes.Buffer
}

func (w *fakeWire) Detect(_ context.Context, h agentwire.Harness) (agentwire.Detection, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := agentwire.Detection{Harness: h}
	if w.system != "" {
		d.Path, d.Version, d.Supported = w.system, "1.0.0 (Fake)", true
		if _, err := os.Stat(w.system); err == nil {
			d.Version = w.Version(context.Background(), w.system)
		}
	}
	return d, nil
}

func (w *fakeWire) Version(ctx context.Context, bin string) string {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (w *fakeWire) Models(context.Context, agentwire.Harness, agentwire.ModelQuery) ([]agentwire.ModelInfo, error) {
	return []agentwire.ModelInfo{{ID: "big", Default: true}, {ID: "small"}}, nil
}

func (w *fakeWire) LatestVersion(context.Context, agentwire.Harness) (string, error) {
	w.lookups.Add(1)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.latest == "" {
		return "", errors.New("offline")
	}
	return w.latest, nil
}

func (w *fakeWire) InvalidateModels(agentwire.Harness) {}

func (w *fakeWire) Asset(_ context.Context, _ agentwire.Harness, version, _, _ string) (agentwire.Asset, error) {
	w.mu.Lock()
	f := w.asset
	w.mu.Unlock()
	if f == nil {
		return agentwire.Asset{}, agentwire.ErrUnsupported
	}
	return f(version)
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestInstallUpdateAndRollback(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	h, err := r.m.Harness(ctx, "fakeh")
	if err != nil {
		t.Fatal(err)
	}
	if h.Installed || !h.Installable {
		t.Fatalf("before the install: %+v", h)
	}
	h, err = r.m.Install(ctx, "fakeh", "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Installed || h.Source != SourceVisor || h.Version != "1.2.0" || h.Path != filepath.Join(r.home, "bin", "fakeh", "1.2.0", "bin", "fakeh") {
		t.Fatalf("after the install: %+v", h)
	}
	// An empty version installs the latest release.
	h, err = r.m.Update(ctx, "fakeh", "")
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != "2.0.0" || !slices.Equal(h.Installs, []string{"1.2.0", "2.0.0"}) {
		t.Fatalf("after the update: %+v", h)
	}
	// A failed install keeps the current version and leaves no folder.
	if _, err := r.m.Install(ctx, "fakeh", "9.9.9"); err == nil || !strings.Contains(err.Error(), "no such version") {
		t.Fatalf("a failed install: %v", err)
	}
	h, err = r.m.Harness(ctx, "fakeh")
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != "2.0.0" || !slices.Equal(h.Installs, []string{"1.2.0", "2.0.0"}) {
		t.Fatalf("after a failed install: %+v", h)
	}
	entries, _ := os.ReadDir(filepath.Join(r.home, "bin", "fakeh")) // checked below
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Fatalf("a failed install left %s", e.Name())
		}
	}
	// A third version removes the oldest; the one before stays.
	if h, err = r.m.Install(ctx, "fakeh", "2.1.0"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.Installs, []string{"2.0.0", "2.1.0"}) {
		t.Fatalf("installs after the switch: %v", h.Installs)
	}
	// Switch back to an installed version with no install.
	if h, err = r.m.Install(ctx, "fakeh", "v2.0.0"); err != nil || h.Version != "2.0.0" {
		t.Fatalf("switch back: %+v %v", h, err)
	}
	l, err := r.m.Launch(ctx, "fakeh")
	if err != nil {
		t.Fatal(err)
	}
	if l.Binary != h.Path || l.Env["FAKEH_NO_UPDATE"] != "1" || !slices.Equal(l.Args, []string{"--no-update"}) {
		t.Fatalf("launch of the visor copy: %+v", l)
	}
}

func TestInstallChecks(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	for _, tc := range []struct {
		name, version string
		want          error
	}{
		{"nope", "", ErrNotFound},
		{"fakeh", "../x", ErrInvalid},
		{"fakeh", "latest", ErrInvalid},
	} {
		if _, err := r.m.Install(ctx, tc.name, tc.version); !errors.Is(err, tc.want) {
			t.Errorf("install %s %s: %v, want %v", tc.name, tc.version, err, tc.want)
		}
	}
	if _, err := r.m.Update(ctx, "fakeh", ""); !errors.Is(err, ErrState) {
		t.Errorf("update of a missing harness: %v", err)
	}
}

func TestUpdateBlockedWhileInUse(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	r.inUse.Store(true)
	if _, err := r.m.Update(ctx, "fakeh", "1.1.0"); !errors.Is(err, ErrBusy) {
		t.Fatalf("update in use: %v", err)
	}
	if h, _ := r.m.Harness(ctx, "fakeh"); h.Version != "1.0.0" { // checked here
		t.Fatalf("version changed under a session: %s", h.Version)
	}
	r.inUse.Store(false)
	if h, err := r.m.Update(ctx, "fakeh", "1.1.0"); err != nil || h.Version != "1.1.0" {
		t.Fatalf("update when free: %+v %v", h, err)
	}
}

func TestSystemInstallAndLatest(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	r.wire.mu.Lock()
	r.wire.system = "/opt/fake/bin/fakeh"
	r.wire.mu.Unlock()
	h, err := r.m.Harness(ctx, "fakeh")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Installed || h.Source != SourceSystem || h.Version != "1.0.0" || h.Path != "/opt/fake/bin/fakeh" {
		t.Fatalf("system install: %+v", h)
	}
	if len(h.Models) != 0 {
		t.Fatalf("fixed models of a fake harness: %+v", h.Models)
	}
	waitChange(t, r, "fakeh")
	deadline := time.Now().Add(5 * time.Second)
	for h.Latest != "2.0.0" && time.Now().Before(deadline) {
		h, _ = r.m.Harness(ctx, "fakeh") // a poll
	}
	if h.Latest != "2.0.0" {
		t.Fatalf("latest: %q", h.Latest)
	}
	l, err := r.m.Launch(ctx, "fakeh")
	if err != nil || l.Binary != "" || l.Home != "" || len(l.Args) != 0 {
		t.Fatalf("launch of a system install: %+v %v", l, err)
	}
	models, err := r.m.Models(ctx, "fakeh")
	if err != nil || len(models) != 2 || models[0].ID != "big" || !models[0].Default {
		t.Fatalf("models: %+v %v", models, err)
	}
}

func TestAuthModeAndLogin(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	h, err := r.m.Harness(ctx, "fakeh")
	if err != nil || h.AuthMode != AuthShared || h.Auth != AuthUnknown {
		t.Fatalf("default auth: %+v %v", h, err)
	}
	if _, err := r.m.SetAuthMode(ctx, "fakeh", "other"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad mode: %v", err)
	}
	h, err = r.m.SetAuthMode(ctx, "fakeh", AuthSeparate)
	if err != nil || h.AuthMode != AuthSeparate || h.Auth != AuthMissing {
		t.Fatalf("separate: %+v %v", h, err)
	}
	sep := filepath.Join(r.home, "harness", "fakeh")
	if fi, err := os.Stat(sep); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("separate home: %v %v", fi, err)
	}
	l, err := r.m.Launch(ctx, "fakeh")
	if err != nil || l.Home != sep || l.Env["FAKEH_HOME"] != sep {
		t.Fatalf("separate launch: %+v %v", l, err)
	}

	login, err := r.m.Login(ctx, "fakeh", LoginSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if login.URL != "https://login.example.test/device" || login.Code != "ABCD-EFGH" {
		t.Fatalf("login: %+v", login)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h, err = r.m.Harness(ctx, "fakeh")
		if err != nil {
			t.Fatal(err)
		}
		if h.Login != nil && h.Login.Phase == LoginDone && h.Auth == AuthOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("login did not end: %+v", h)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := r.m.Logout(ctx, "fakeh"); err != nil {
		t.Fatal(err)
	}
	if h, _ = r.m.Harness(ctx, "fakeh"); h.Auth != AuthMissing { // checked here
		t.Fatalf("after logout: %s", h.Auth)
	}

	// The mode stays after a restart; shared drops the login home.
	m2 := New(r.home, r.m.st, r.wire, r.m.o)
	defer m2.Close(context.Background())
	if h, _ = m2.Harness(ctx, "fakeh"); h.AuthMode != AuthSeparate { // checked here
		t.Fatalf("mode after a restart: %s", h.AuthMode)
	}
	if _, err := m2.SetAuthMode(ctx, "fakeh", AuthShared); err != nil {
		t.Fatal(err)
	}
	if l, _ = m2.Launch(ctx, "fakeh"); l.Home != "" || l.Env["FAKEH_HOME"] != "" { // checked here
		t.Fatalf("shared launch: %+v", l)
	}
}

func TestVariant(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Install(ctx, "fakeh-alt", ""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("install a variant: %v", err)
	}
	if _, err := r.m.Update(ctx, "fakeh-alt", ""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("update a variant: %v", err)
	}
	if _, err := r.m.Harness(ctx, "orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a variant of an unknown base: %v", err)
	}
	list, err := r.m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range list {
		names = append(names, h.Name)
	}
	if !slices.Equal(names, []string{"fakeh", "claude", "fakeh-alt"}) {
		t.Fatalf("list: %v", names)
	}
	h, err := r.m.Harness(ctx, "fakeh-alt")
	if err != nil || h.Base != "fakeh" || h.Version != "1.0.0" || h.Installable || h.AuthMode != AuthShared {
		t.Fatalf("variant: %+v %v", h, err)
	}
	if err := r.m.SetEnv(ctx, "fakeh-alt", map[string]string{"K": "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.SetAuthMode(ctx, "fakeh-alt", AuthSeparate); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.m.Harness(ctx, "fakeh"); h.AuthMode != AuthShared || len(h.EnvKeys) != 0 { // checked here
		t.Fatalf("the base changed: %+v", h)
	}
	l, err := r.m.Launch(ctx, "fakeh-alt")
	sep := filepath.Join(r.home, "harness", "fakeh-alt")
	if err != nil || l.Home != sep || l.Env["K"] != "v" || l.Env["FAKEH_NO_UPDATE"] != "1" || !strings.Contains(l.Binary, filepath.Join("bin", "fakeh")) {
		t.Fatalf("variant launch: %+v %v", l, err)
	}
	if r.m.Base("fakeh-alt") != "fakeh" || r.m.Base("claude") != "claude" || r.m.Base("orphan") != "orphan" {
		t.Fatal("base")
	}
}

func TestLoginInAppAndMissing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.m.Login(t.Context(), "fakeh", LoginSpec{}); !errors.Is(err, ErrState) {
		t.Fatalf("login with no install: %v", err)
	}
	r.m.o.Facts.LoginCommand = func(agentwire.Harness, agentwire.LoginOptions) ([]string, error) { return []string{"fakeh"}, nil }
	l, err := r.m.Login(t.Context(), "fakeh", LoginSpec{})
	if err != nil || !l.InApp {
		t.Fatalf("in-app login: %+v %v", l, err)
	}
}

func TestBinaryFact(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	bin := filepath.Join(r.home, "tools", "own")
	r.m.o.Facts.Binary = func(h agentwire.Harness) string {
		if h == "fakeh" {
			return bin
		}
		return ""
	}
	h, err := r.m.Harness(t.Context(), "fakeh")
	if err != nil || !h.Installed || h.Path != bin || h.Source != SourceSystem {
		t.Fatalf("harness: %+v %v", h, err)
	}
	r.m.o.Facts.LoginCommand = func(agentwire.Harness, agentwire.LoginOptions) ([]string, error) { return []string{"own"}, nil }
	l, err := r.m.Login(t.Context(), "fakeh", LoginSpec{})
	if err != nil || !l.InApp {
		t.Fatalf("login: %+v %v", l, err)
	}
	if h, _ := r.m.Harness(t.Context(), "claude"); h.Installed {
		t.Fatalf("claude: %+v", h)
	}
}

func TestEnvStaysSecret(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if err := r.m.SetEnv(ctx, "fakeh", map[string]string{"BAD-KEY": secret}); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), secret) {
		t.Fatalf("a bad key: %v", err)
	}
	if err := r.m.SetEnv(ctx, "fakeh", map[string]string{"FAKEH_API_KEY": secret}); err != nil {
		t.Fatal(err)
	}
	if err := r.m.SetEnv(ctx, "claude", map[string]string{"ANTHROPIC_API_KEY": secret}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.home, "harness", "fakeh.env.json")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("env folder: %v %v", fi, err)
	}
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Login(ctx, "fakeh", LoginSpec{}); err != nil {
		t.Fatal(err)
	}
	list, err := r.m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), `"FAKEH_API_KEY"`) {
		t.Fatalf("harness JSON: %s", data)
	}
	// A token key in the harness env is a login.
	for _, h := range list {
		if h.Name == "claude" && h.Auth != AuthOK {
			t.Fatalf("claude with a token key: %s", h.Auth)
		}
	}
	l, err := r.m.Launch(ctx, "fakeh")
	if err != nil || l.Env["FAKEH_API_KEY"] != secret {
		t.Fatalf("launch env: %v", err)
	}
	if err := r.m.SetEnv(ctx, "fakeh", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty env keeps the file: %v", err)
	}
	if err := r.m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.logs.String(), secret) {
		t.Fatalf("a log has the secret: %s", r.logs.String())
	}
}

func TestSetLimits(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	lim := Limits{Plan: "pro", Windows: []LimitWindow{{Key: "5h", UsedPercent: 40}}}
	r.m.SetLimits(ctx, "fakeh", lim)
	r.m.SetLimits(ctx, "fakeh", lim)
	r.m.SetLimits(ctx, "nope", lim)
	n := 0
	for len(r.changes) > 0 {
		if <-r.changes == "fakeh" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("changes for the same limits: %d", n)
	}
	h, err := r.m.Harness(ctx, "fakeh")
	if err != nil || h.Limits == nil || h.Limits.Plan != "pro" {
		t.Fatalf("limits: %+v %v", h.Limits, err)
	}
}

func TestCompareVersions(t *testing.T) {
	t.Parallel()
	got := []string{"10.0.0", "2.0.0", "2.0.0-beta.1", "1.9.12", "1.10.0"}
	slices.SortFunc(got, compareVersions)
	want := []string{"1.9.12", "1.10.0", "2.0.0", "2.0.0-beta.1", "10.0.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("sorted: %v", got)
	}
}

func waitChange(t *testing.T, r *rig, name string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-r.changes:
			if got == name {
				return
			}
		case <-timeout:
			t.Fatalf("no change of %s", name)
		}
	}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	home := t.TempDir()
	tools := filepath.Join(home, "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "npm"), []byte(fakeNPM), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(home, "visor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // test cleanup
	r := &rig{home: home, user: filepath.Join(home, "user"), wire: &fakeWire{latest: "2.0.0"}, changes: make(chan string, 256), logs: &bytes.Buffer{}}
	var logMu sync.Mutex
	r.m = New(home, st, r.wire, Options{
		Names:    []string{"fakeh", "claude"},
		Variants: func() map[string]string { return map[string]string{"fakeh-alt": "fakeh", "orphan": "nope"} },
		Path:     tools + string(os.PathListSeparator) + "/bin" + string(os.PathListSeparator) + "/usr/bin",
		Facts: Facts{
			Recipe: func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
				return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallNPM, Package: "fake-pkg", Bin: "fakeh",
					UpdateOffEnv: map[string]string{"FAKEH_NO_UPDATE": "1"}, UpdateOffArgs: []string{"--no-update"},
					Update: []string{"update"}, UpdateTo: []string{"update", "--to"}}, true
			},
			LoginCommand: func(agentwire.Harness, agentwire.LoginOptions) ([]string, error) {
				return []string{"fakeh", "auth", "login"}, nil
			},
			LogoutCommand:   func(agentwire.Harness) []string { return []string{"fakeh", "auth", "logout"} },
			LoginEnv:        fakeLoginEnv,
			CredentialFiles: func(agentwire.Harness) []string { return []string{"cred"} },
			AuthStatus: func(_ context.Context, _ agentwire.Harness, home string) (agentwire.AuthState, error) {
				if home == "" {
					return agentwire.AuthUnknown, nil
				}
				if _, err := os.Stat(filepath.Join(home, "cred")); err == nil {
					return agentwire.AuthOK, nil
				}
				return agentwire.AuthMissing, nil
			},
		},
		Policy: Policy{
			MCPServers:  []agentwire.MCPServer{{Name: "tool", Command: "/bin/tool", Args: []string{"mcp"}}},
			NoUserHooks: true,
			Keys:        map[string][]agentwire.HomeKey{"claude": {{File: ".claude.json", Key: "chrome", Value: false}}},
			Files: func(name, _ string) map[string][]byte {
				if name != "claude" {
					return nil
				}
				return map[string][]byte{"product.json": []byte(`{"p":1}`)}
			},
		},
		UserHome:  r.user,
		LoginWait: 5 * time.Second,
		InUse:     func(string) bool { return r.inUse.Load() },
		OnChange:  func(name string) { r.changes <- name },
		Logger:    slog.New(slog.NewTextHandler(lockedWriter{mu: &logMu, w: r.logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	t.Cleanup(func() { _ = r.m.Close(context.Background()) }) // test cleanup
	return r
}

func fakeLoginEnv(_ agentwire.Harness, home string) map[string]string {
	if home == "" {
		return nil
	}
	return map[string]string{"FAKEH_HOME": home}
}
