package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHomeShared(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	writeFile(t, filepath.Join(r.user, ".claude/skills/a/SKILL.md"), "a")
	writeFile(t, filepath.Join(r.user, ".claude/.credentials.json"), `{"t":1}`)
	writeFile(t, filepath.Join(r.user, ".claude/settings.json"), `{"model":"m","hooks":{"Stop":[]},"statusLine":"s"}`)
	writeFile(t, filepath.Join(r.user, ".claude.json"), `{"oauthAccount":{"id":"x"},"theme":"dark","projects":{"p":1}}`)
	home := filepath.Join(r.home, "harness", "claude")
	// A login of the visor waits in .own while the user login is shared.
	writeFile(t, filepath.Join(home, ".credentials.json"), `{"own":1}`)

	l, err := r.m.Launch(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if l.Home != home || l.Env["FAKEH_HOME"] != home {
		t.Fatalf("launch: %+v", l)
	}
	if got := linkTarget(t, filepath.Join(home, "skills")); got != filepath.Join(r.user, ".claude/skills") {
		t.Fatalf("skills link: %q", got)
	}
	if got := linkTarget(t, filepath.Join(home, ".credentials.json")); got != filepath.Join(r.user, ".claude/.credentials.json") {
		t.Fatalf("login link: %q", got)
	}
	settings := readJSON(t, filepath.Join(home, "settings.json"))
	if settings["model"] != "m" || settings["hooks"] != nil || settings["statusLine"] != "s" {
		t.Fatalf("settings: %v", settings)
	}
	cj := readJSON(t, filepath.Join(home, ".claude.json"))
	servers, _ := cj["mcpServers"].(map[string]any)
	if cj["oauthAccount"] == nil || cj["theme"] != "dark" || cj["projects"] != nil || cj["hasCompletedOnboarding"] != true || cj["chrome"] != false || servers["tool"] == nil {
		t.Fatalf(".claude.json: %v", cj)
	}
	if b, err := os.ReadFile(filepath.Join(home, "product.json")); err != nil || string(b) != `{"p":1}` {
		t.Fatalf("product file: %q %v", b, err)
	}

	// The separate login gets its own login back.
	if _, err := r.m.SetAuthMode(ctx, "claude", AuthSeparate); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".credentials.json")); err != nil || string(b) != `{"own":1}` || linkTarget(t, filepath.Join(home, ".credentials.json")) != "" {
		t.Fatalf("own login: %q %v", b, err)
	}
	if got := linkTarget(t, filepath.Join(home, "skills")); got == "" {
		t.Fatal("a separate login drops the shared skills")
	}

	// A harness with no home layout uses the user home.
	l, err = r.m.Launch(ctx, "fakeh")
	if err != nil || l.Home != "" {
		t.Fatalf("fakeh: %+v %v", l, err)
	}
}

func TestTrustDir(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.m.Launch(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := r.m.TrustDir("claude", link); err != nil {
		t.Fatal(err)
	}
	if err := r.m.TrustDir("fakeh", link); err != nil {
		t.Fatal(err)
	}
	cj := readJSON(t, filepath.Join(r.home, "harness", "claude", ".claude.json"))
	projects, _ := cj["projects"].(map[string]any)
	realDir, _ := filepath.EvalSymlinks(real)
	for _, d := range []string{link, realDir} {
		p, _ := projects[d].(map[string]any)
		if p["hasTrustDialogAccepted"] != true {
			t.Fatalf("%s not trusted: %v", d, projects)
		}
	}
	if cj["hasCompletedOnboarding"] != true {
		t.Fatalf("the other keys: %v", cj)
	}
	// A new launch keeps the trust marks.
	if _, err := r.m.Launch(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	if cj = readJSON(t, filepath.Join(r.home, "harness", "claude", ".claude.json")); cj["projects"] == nil {
		t.Fatalf("launch dropped the trust: %v", cj)
	}
}

func TestHomeFiles(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	writeFile(t, filepath.Join(r.user, ".claude/settings.local.json"), `{"a":1}`)
	writeFile(t, filepath.Join(r.user, ".claude/settings.json"), `{"model":"m","hooks":{}}`)
	home := filepath.Join(r.home, "harness", "claude")
	if _, err := r.m.Launch(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	rows, err := r.m.Files(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]HomeFile{}
	for _, f := range rows {
		byKey[f.Key] = f
	}
	if f := byKey["hooks"]; f.Shared || !f.Locked {
		t.Fatalf("hooks row: %+v", f)
	}
	if f := byKey["login"]; !f.Shared || !f.Locked {
		t.Fatalf("login row: %+v", f)
	}
	if f := byKey["settings_local"]; !f.Shared || !f.UserExists || f.UserModAt == nil {
		t.Fatalf("settings_local row: %+v", f)
	}
	if _, err := r.m.OwnFile(ctx, "claude", "hooks", OwnFromUser); !errors.Is(err, ErrState) {
		t.Fatalf("own a locked file: %v", err)
	}
	if _, err := r.m.OwnFile(ctx, "nope", "hooks", OwnFromUser); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown harness: %v", err)
	}

	f, err := r.m.OwnFile(ctx, "claude", "settings_local", OwnFromUser)
	if err != nil || f.Shared || f.OwnSince == nil {
		t.Fatalf("own: %+v %v", f, err)
	}
	path := filepath.Join(home, "settings.local.json")
	if linkTarget(t, path) != "" {
		t.Fatal("the own copy is a link")
	}
	c, err := r.m.ReadFile(ctx, "claude", "settings_local", "")
	if err != nil || c.Side != SideHome || c.Content != `{"a":1}` {
		t.Fatalf("read: %+v %v", c, err)
	}
	if _, err := r.m.WriteFile(ctx, "claude", "settings_local", `{"a":`, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad JSON: %v", err)
	}
	if _, err := r.m.WriteFile(ctx, "claude", "settings_local", `{"a":2}`, "stale"); !errors.Is(err, ErrStale) {
		t.Fatalf("stale write: %v", err)
	}
	w, err := r.m.WriteFile(ctx, "claude", "settings_local", `{"a":2}`, c.ETag)
	if err != nil || w.ETag == c.ETag {
		t.Fatalf("write: %+v %v", w, err)
	}
	// A rebuild keeps the own copy.
	if _, err := r.m.Launch(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"a":2}` {
		t.Fatalf("rebuild: %q", b)
	}

	f, err = r.m.ShareFile(ctx, "claude", "settings_local")
	if err != nil || !f.Shared || f.BackupUntil == nil {
		t.Fatalf("share: %+v %v", f, err)
	}
	if linkTarget(t, path) != filepath.Join(r.user, ".claude/settings.local.json") {
		t.Fatal("no link after share")
	}
	if _, err := r.m.WriteFile(ctx, "claude", "settings_local", `{}`, ""); !errors.Is(err, ErrState) {
		t.Fatalf("write a shared file: %v", err)
	}
	if _, err := r.m.OwnFile(ctx, "claude", "settings_local", OwnFromBackup); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"a":2}` {
		t.Fatalf("from backup: %q", b)
	}

	// A keys entry: the own copy keeps the keys and stops the copy.
	if _, err := r.m.OwnFile(ctx, "claude", "settings_prefs", OwnFromUser); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.user, ".claude/settings.json"), `{"model":"new"}`)
	if _, err := r.m.Launch(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	if s := readJSON(t, filepath.Join(home, "settings.json")); s["model"] != "m" {
		t.Fatalf("own keys: %v", s)
	}
}

func TestHomeBlocked(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	writeFile(t, filepath.Join(r.user, ".claude/CLAUDE.md"), "user")
	writeFile(t, filepath.Join(r.home, "harness/claude/CLAUDE.md"), "real")
	if _, err := r.m.Launch(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	rows, err := r.m.Files(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rows {
		if f.Key == "user_memory" && !f.Blocked {
			t.Fatalf("not blocked: %+v", f)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func linkTarget(t *testing.T, path string) string {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return got
}
