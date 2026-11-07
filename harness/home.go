package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

// Policy is the product policy for the harness homes.
type Policy struct {
	// MCPServers go into the MCP file of each harness home.
	MCPServers []agentwire.MCPServer
	// NoUserHooks keeps the user hooks out of the harness homes.
	NoUserHooks bool
	// Share changes the default share of a home file, by harness and key.
	Share map[string]map[string]bool
	// Keys are values that the home files must hold, by harness.
	Keys map[string][]agentwire.HomeKey
	// Files gives the product files of a harness home, by path relative to
	// the home. The visor writes a file when its content changes. The
	// function can read the home. It runs on each launch.
	Files func(name, home string) map[string][]byte
}

// HomeFile is one file or folder of a harness home that can share the
// user's own setup.
type HomeFile struct {
	Key    string                 `json:"key"`
	Kind   agentwire.HomeFileKind `json:"kind"`
	Format string                 `json:"format,omitempty"`
	// Shared means that the home uses the user file. Else the home keeps
	// its own copy.
	Shared bool `json:"shared"`
	// Login marks a login file. The auth mode decides its share.
	Login bool `json:"login,omitempty"`
	// Hooks marks a file that holds user hooks.
	Hooks bool `json:"hooks,omitempty"`
	// Locked means that the policy or the auth mode decides the share.
	Locked bool `json:"locked,omitempty"`
	// Blocked means that the home holds a real file where a shared file
	// links to the user file. The home uses its own copy until the user
	// removes it.
	Blocked    bool       `json:"blocked,omitempty"`
	UserPath   string     `json:"user_path"`
	HomePath   string     `json:"home_path"`
	UserExists bool       `json:"user_exists"`
	UserModAt  *time.Time `json:"user_modified_at,omitempty"`
	// OwnSince is when the home made its own copy.
	OwnSince *time.Time `json:"own_since,omitempty"`
	// UserChanged means that the user file changed after OwnSince.
	UserChanged bool `json:"user_changed_since_own,omitempty"`
	// BackupUntil is when the newest backup of an own copy expires.
	BackupUntil *time.Time `json:"backup_until,omitempty"`
}

// FileContent is the content of one side of a home file.
type FileContent struct {
	Key     string `json:"key"`
	Side    string `json:"side"`
	Content string `json:"content"`
	ETag    string `json:"etag"`
}

// OwnStart is the first content of an own copy.
type OwnStart string

// File sides and own starts.
const (
	SideUser = "user"
	SideHome = "home"

	OwnFromUser   OwnStart = "user"
	OwnFromEmpty  OwnStart = "empty"
	OwnFromBackup OwnStart = "backup"
)

// fileState is the stored share state of one home file.
type fileState struct {
	Own   bool      `json:"own,omitempty"`
	OwnAt time.Time `json:"own_at,omitzero"`
}

// Home file errors.
var (
	// ErrStale is a write with an old etag.
	ErrStale = errors.New("harness: the file changed since the read")
	// ErrNoBackup means that no backup of the own copy exists.
	ErrNoBackup = errors.New("harness: no backup")
)

const (
	maxFileBytes = 1 << 20
	backupKeep   = 30 * 24 * time.Hour
	ownSuffix    = ".own"
)

// Files lists the home files of a harness.
func (m *Manager) Files(ctx context.Context, name string) ([]HomeFile, error) {
	if !m.known(name) {
		return nil, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	st, err := m.state(ctx, name)
	if err != nil {
		return nil, err
	}
	out := []HomeFile{}
	for _, f := range agentwire.HomeFiles(m.kind(name)) {
		out = append(out, m.fileRow(name, f, st))
	}
	return out, nil
}

// ShareFile makes the home use the user file again. An own copy moves to
// the backups for 30 days.
func (m *Manager) ShareFile(ctx context.Context, name, key string) (HomeFile, error) {
	f, err := m.homeFile(name, key)
	if err != nil {
		return HomeFile{}, err
	}
	st, err := m.state(ctx, name)
	if err != nil {
		return HomeFile{}, err
	}
	if m.lockedShare(f, st) != nil {
		return HomeFile{}, fmt.Errorf("%w: the share of %s is fixed", ErrState, key)
	}
	path := filepath.Join(m.separateHome(name), f.Home)
	if f.Kind != agentwire.HomeKeys {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			if err := m.backup(name, key, path); err != nil {
				return HomeFile{}, err
			}
		}
	}
	if _, err := m.changeState(ctx, name, func(s *state) { delete(s.Files, key) }); err != nil {
		return HomeFile{}, err
	}
	return m.afterFileChange(ctx, name, f)
}

// OwnFile makes an own copy of a home file. The copy starts from the user
// file, from nothing, or from the newest backup.
func (m *Manager) OwnFile(ctx context.Context, name, key string, start OwnStart) (HomeFile, error) {
	f, err := m.homeFile(name, key)
	if err != nil {
		return HomeFile{}, err
	}
	st, err := m.state(ctx, name)
	if err != nil {
		return HomeFile{}, err
	}
	if m.lockedShare(f, st) != nil {
		return HomeFile{}, fmt.Errorf("%w: the share of %s is fixed", ErrState, key)
	}
	home := m.separateHome(name)
	path := filepath.Join(home, f.Home)
	switch f.Kind {
	case agentwire.HomeKeys:
		// The keys stay in the home file; the visor stops the copy.
		if start == OwnFromEmpty {
			if err := m.editFile(path, func(b []byte) ([]byte, error) { return agentwire.DropKeys(f.Format, b, m.fileKeys(name, f)) }); err != nil {
				return HomeFile{}, err
			}
		}
	case agentwire.HomeEach:
		return HomeFile{}, fmt.Errorf("%w: %s is a folder of links", ErrUnsupported, key)
	default:
		if err := m.writeOwnCopy(name, f, path, start); err != nil {
			return HomeFile{}, err
		}
	}
	if _, err := m.changeState(ctx, name, func(s *state) {
		if s.Files == nil {
			s.Files = map[string]fileState{}
		}
		s.Files[key] = fileState{Own: true, OwnAt: time.Now().UTC()}
	}); err != nil {
		return HomeFile{}, err
	}
	return m.afterFileChange(ctx, name, f)
}

// writeOwnCopy replaces a link at path with an own copy.
func (m *Manager) writeOwnCopy(name string, f agentwire.HomeFile, path string, start OwnStart) error {
	if f.Format == "" {
		return fmt.Errorf("%w: %s is a folder", ErrUnsupported, f.Key)
	}
	content, err := m.ownContent(name, f, start)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return writeIfChanged(path, content, 0o600)
}

// ownContent is the first content of an own copy.
func (m *Manager) ownContent(name string, f agentwire.HomeFile, start OwnStart) ([]byte, error) {
	switch start {
	case OwnFromUser, "":
		content, err := readOptional(filepath.Join(m.userHome(), f.User))
		if err != nil {
			return nil, err
		}
		if f.Format == "json" && m.o.Policy.NoUserHooks && len(bytes.TrimSpace(content)) > 0 {
			if out, err := agentwire.DropKeys("json", content, []string{"hooks"}); err == nil {
				content = out
			}
		}
		return content, nil
	case OwnFromEmpty:
		if f.Format == "json" {
			return []byte("{}\n"), nil
		}
		return nil, nil
	case OwnFromBackup:
		b := m.backups(name, f.Key)
		if len(b) == 0 {
			return nil, ErrNoBackup
		}
		return os.ReadFile(b[0])
	default:
		return nil, fmt.Errorf("%w: start %q", ErrInvalid, start)
	}
}

// ReadFile reads one side of a home file. An empty side is the one that a
// session uses.
func (m *Manager) ReadFile(ctx context.Context, name, key, side string) (FileContent, error) {
	f, err := m.homeFile(name, key)
	if err != nil {
		return FileContent{}, err
	}
	if f.Format == "" {
		return FileContent{}, fmt.Errorf("%w: %s is a folder", ErrUnsupported, key)
	}
	st, err := m.state(ctx, name)
	if err != nil {
		return FileContent{}, err
	}
	if side == "" {
		side = SideUser
		if !m.shares(name, f, st) {
			side = SideHome
		}
	}
	var path string
	switch side {
	case SideUser:
		path = filepath.Join(m.userHome(), f.User)
	case SideHome:
		path = filepath.Join(m.separateHome(name), f.Home)
	default:
		return FileContent{}, fmt.Errorf("%w: side %q", ErrInvalid, side)
	}
	b, err := readOptional(path)
	if err != nil {
		return FileContent{}, err
	}
	return FileContent{Key: key, Side: side, Content: string(b), ETag: etag(b)}, nil
}

// WriteFile replaces the own copy of a home file. An empty ifMatch skips
// the etag check. A stale write gives ErrStale and the current content.
func (m *Manager) WriteFile(ctx context.Context, name, key, content, ifMatch string) (FileContent, error) {
	f, err := m.homeFile(name, key)
	if err != nil {
		return FileContent{}, err
	}
	if len(content) > maxFileBytes {
		return FileContent{}, fmt.Errorf("%w: the file is larger than 1 MiB", ErrInvalid)
	}
	st, err := m.state(ctx, name)
	if err != nil {
		return FileContent{}, err
	}
	if m.shares(name, f, st) || f.Format == "" || f.Kind == agentwire.HomeEach {
		return FileContent{}, fmt.Errorf("%w: %s has no own copy", ErrState, key)
	}
	if f.Format == "json" && strings.TrimSpace(content) != "" && !json.Valid([]byte(content)) {
		return FileContent{}, fmt.Errorf("%w: the content is not JSON", ErrInvalid)
	}
	path := filepath.Join(m.separateHome(name), f.Home)
	m.homeMu.Lock()
	cur, err := readOptional(path)
	if err == nil && ifMatch != "" && ifMatch != etag(cur) {
		m.homeMu.Unlock()
		return FileContent{Key: key, Side: SideHome, Content: string(cur), ETag: etag(cur)}, ErrStale
	}
	if err == nil {
		err = writeIfChanged(path, []byte(content), 0o600)
	}
	m.homeMu.Unlock()
	if err != nil {
		return FileContent{}, err
	}
	m.changed(name)
	return FileContent{Key: key, Side: SideHome, Content: content, ETag: etag([]byte(content))}, nil
}

// afterFileChange builds the home again and gives the new row.
func (m *Manager) afterFileChange(ctx context.Context, name string, f agentwire.HomeFile) (HomeFile, error) {
	st, err := m.state(ctx, name)
	if err != nil {
		return HomeFile{}, err
	}
	if _, _, err := m.prepareHome(name, st); err != nil {
		return HomeFile{}, err
	}
	m.changed(name)
	return m.fileRow(name, f, st), nil
}

// prepareHome builds the home of a harness for its auth mode and gives the
// home and its env. An empty home means the user's own home.
func (m *Manager) prepareHome(name string, st state) (string, map[string]string, error) {
	h := m.kind(name)
	f := m.o.Facts
	mode := st.mode()
	files := agentwire.HomeFiles(h)
	var product map[string][]byte
	if mode == AuthShared && (len(files) == 0 || agentwire.SharesUserHome(h)) {
		return "", nil, nil
	}
	home := m.separateHome(name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", nil, fmt.Errorf("make the home of %s: %w", name, err)
	}
	m.homeMu.Lock()
	defer m.homeMu.Unlock()
	if m.o.Policy.Files != nil {
		product = m.o.Policy.Files(name, home)
	}
	env := maps.Clone(f.LoginEnv(h, filepath.Join(home, agentwire.LoginDir(h))))
	if env == nil {
		env = map[string]string{}
	}
	keyFiles, err := m.linkHome(name, home, files, st, product, env)
	errs := []error{err}
	for _, rel := range m.policyFiles(h) {
		keyFiles[rel] = append(keyFiles[rel], agentwire.HomeFile{})
	}
	for _, rel := range slices.Sorted(maps.Keys(keyFiles)) {
		errs = append(errs, m.buildKeyFile(name, home, rel, keyFiles[rel], st))
	}
	errs = append(errs, writeProductFiles(home, product))
	if mode == AuthShared {
		maps.Copy(env, agentwire.SharedLoginEnv(h))
	}
	if err := errors.Join(errs...); err != nil {
		m.log.Warn("build the harness home", "harness", name, "err", err)
	}
	return home, env, nil
}

// linkHome links the link and folder entries of a home, sets the env of the
// own read files, and groups the keys entries by home file.
func (m *Manager) linkHome(name, home string, files []agentwire.HomeFile, st state, product map[string][]byte, env map[string]string) (map[string][]agentwire.HomeFile, error) {
	user := m.userHome()
	var errs []error
	keyFiles := map[string][]agentwire.HomeFile{}
	for _, hf := range files {
		share := m.shares(name, hf, st)
		src, dst := filepath.Join(user, hf.User), filepath.Join(home, hf.Home)
		switch hf.Kind {
		case agentwire.HomeLink:
			errs = append(errs, linkFile(src, dst, share, hf.Login))
		case agentwire.HomeEach:
			errs = append(errs, linkEach(src, dst, share, product, hf.Home))
		case agentwire.HomeKeys:
			keyFiles[hf.Home] = append(keyFiles[hf.Home], hf)
		case agentwire.HomeRead:
			if !share && hf.OwnEnv != "" {
				env[hf.OwnEnv] = dst
			}
		}
	}
	return keyFiles, errors.Join(errs...)
}

// TrustDir marks dir as a trusted folder in the separate home of a harness,
// so that the vendor CLI shows no folder trust dialog there. The start of a
// session in dir is the approval. A harness with no trust file changes
// nothing.
func (m *Manager) TrustDir(name, dir string) error {
	h := m.kind(name)
	rel := agentwire.HomeTrustFile(h)
	if rel == "" || dir == "" {
		return nil
	}
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append(dirs, real)
	}
	m.homeMu.Lock()
	defer m.homeMu.Unlock()
	return m.editFile(filepath.Join(m.separateHome(name), rel), func(b []byte) ([]byte, error) {
		return agentwire.SetHomeTrust(h, b, dirs)
	})
}

// writeProductFiles writes the product files of a home.
func writeProductFiles(home string, product map[string][]byte) error {
	var errs []error
	for _, rel := range slices.Sorted(maps.Keys(product)) {
		path := filepath.Join(home, rel)
		if !strings.HasPrefix(path, home+string(filepath.Separator)) {
			errs = append(errs, fmt.Errorf("%w: product file %q", ErrInvalid, rel))
			continue
		}
		errs = append(errs, writeIfChanged(path, product[rel], 0o600))
	}
	return errors.Join(errs...)
}

// buildKeyFile writes the shared keys, the defaults, the policy keys, and
// the MCP servers into one home file.
func (m *Manager) buildKeyFile(name, home, rel string, entries []agentwire.HomeFile, st state) error {
	h := m.kind(name)
	path := filepath.Join(home, rel)
	return m.editFile(path, func(b []byte) ([]byte, error) {
		var named []string
		for _, e := range entries {
			named = append(named, e.Keys...)
		}
		for _, e := range entries {
			if e.Key == "" {
				continue
			}
			keys := e.Keys
			if m.shares(name, e, st) {
				user, err := readOptional(filepath.Join(m.userHome(), e.User))
				if err != nil {
					return nil, err
				}
				var skip []string
				if keys == nil {
					skip = named
				}
				if out, err := agentwire.MergeKeys(e.Format, b, user, keys, skip); err == nil {
					b = out
				} else {
					m.log.Warn("copy the user keys", "harness", name, "key", e.Key, "err", err)
				}
				continue
			}
			// A separate login keeps the keys that it wrote.
			if e.Hooks && m.o.Policy.NoUserHooks {
				out, err := agentwire.DropKeys(e.Format, b, keys)
				if err != nil {
					return nil, err
				}
				b = out
			}
		}
		return m.setPolicyKeys(h, rel, b)
	})
}

// policyFiles lists the home files that get vendor defaults, policy keys,
// or MCP servers.
func (m *Manager) policyFiles(h agentwire.Harness) []string {
	var out []string
	for _, k := range slices.Concat(agentwire.HomeDefaults(h), m.o.Policy.Keys[string(h)]) {
		out = append(out, k.File)
	}
	if mcp := agentwire.HomeMCPFile(h); mcp != "" && len(m.o.Policy.MCPServers) > 0 {
		out = append(out, mcp)
	}
	return out
}

// setPolicyKeys sets the vendor defaults, the policy keys, and the MCP
// servers in the home file rel.
func (m *Manager) setPolicyKeys(h agentwire.Harness, rel string, b []byte) ([]byte, error) {
	var set []agentwire.HomeKey
	for _, k := range slices.Concat(agentwire.HomeDefaults(h), m.o.Policy.Keys[string(h)]) {
		if k.File == rel {
			set = append(set, k)
		}
	}
	if len(set) > 0 {
		out, err := agentwire.SetKeys(rel, b, set)
		if err != nil {
			return nil, err
		}
		b = out
	}
	if agentwire.HomeMCPFile(h) == rel && len(m.o.Policy.MCPServers) > 0 {
		return agentwire.SetHomeMCP(h, b, m.o.Policy.MCPServers)
	}
	return b, nil
}

// shares reports whether a home file uses the user file.
func (m *Manager) shares(name string, f agentwire.HomeFile, st state) bool {
	if v := m.lockedShare(f, st); v != nil {
		return *v
	}
	if fs, ok := st.Files[f.Key]; ok && fs.Own {
		return false
	}
	if v, ok := m.o.Policy.Share[name][f.Key]; ok {
		return v
	}
	if v, ok := m.o.Policy.Share[m.base(name)][f.Key]; ok {
		return v
	}
	return f.Share
}

// lockedShare gives the share that the policy or the auth mode fixes, or
// nil.
func (m *Manager) lockedShare(f agentwire.HomeFile, st state) *bool {
	switch {
	case f.Login:
		v := st.mode() == AuthShared
		return &v
	case f.Hooks && m.o.Policy.NoUserHooks:
		v := false
		return &v
	}
	return nil
}

// fileKeys gives the keys that a keys entry owns in its home file.
func (m *Manager) fileKeys(name string, f agentwire.HomeFile) []string {
	if f.Keys != nil {
		return f.Keys
	}
	data, _ := readOptional(filepath.Join(m.userHome(), f.User)) // a missing user file has no keys
	var named []string
	for _, e := range agentwire.HomeFiles(m.kind(name)) {
		if e.Home == f.Home {
			named = append(named, e.Keys...)
		}
	}
	var keys []string
	if f.Format == "json" {
		var u map[string]any
		_ = json.Unmarshal(data, &u) // a bad user file has no keys
		for k := range u {
			if !slices.Contains(named, k) {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func (m *Manager) fileRow(name string, f agentwire.HomeFile, st state) HomeFile {
	row := HomeFile{
		Key: f.Key, Kind: f.Kind, Format: f.Format, Login: f.Login, Hooks: f.Hooks,
		Shared: m.shares(name, f, st), Locked: m.lockedShare(f, st) != nil,
		UserPath: filepath.Join(m.userHome(), f.User), HomePath: filepath.Join(m.separateHome(name), f.Home),
	}
	if fi, err := os.Stat(row.UserPath); err == nil {
		row.UserExists = true
		t := fi.ModTime().UTC()
		row.UserModAt = &t
	}
	if fs, ok := st.Files[f.Key]; ok && fs.Own && !row.Shared {
		t := fs.OwnAt
		row.OwnSince = &t
		row.UserChanged = row.UserModAt != nil && row.UserModAt.After(t)
	}
	if row.Shared && (f.Kind == agentwire.HomeLink) && row.UserExists {
		if fi, err := os.Lstat(row.HomePath); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			row.Blocked = true
		}
	}
	if b := m.backups(name, f.Key); len(b) > 0 {
		if fi, err := os.Stat(b[0]); err == nil {
			t := fi.ModTime().Add(backupKeep).UTC()
			row.BackupUntil = &t
		}
	}
	return row
}

func (m *Manager) homeFile(name, key string) (agentwire.HomeFile, error) {
	if !m.known(name) {
		return agentwire.HomeFile{}, fmt.Errorf("harness %s: %w", name, ErrNotFound)
	}
	for _, f := range agentwire.HomeFiles(m.kind(name)) {
		if f.Key == key {
			return f, nil
		}
	}
	return agentwire.HomeFile{}, fmt.Errorf("home file %s of %s: %w", key, name, ErrNotFound)
}

func (m *Manager) userHome() string {
	if m.o.UserHome != "" {
		return m.o.UserHome
	}
	h, _ := os.UserHomeDir() // no home: the user paths do not exist
	return h
}

func (m *Manager) backupDir(name string) string {
	return filepath.Join(m.home, "harness", name+".backups")
}

// backup moves an own copy to the backups.
func (m *Manager) backup(name, key, path string) error {
	dir := m.backupDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dir, key+"-"+time.Now().UTC().Format("20060102T150405.000Z")))
}

// backups lists the backups of a key, newest first, and removes the old
// ones.
func (m *Manager) backups(name, key string) []string {
	matches, _ := filepath.Glob(filepath.Join(m.backupDir(name), key+"-*")) // a bad pattern has no matches
	slices.Sort(matches)
	slices.Reverse(matches)
	var keep []string
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if time.Since(fi.ModTime()) > backupKeep {
			if err := os.Remove(p); err != nil {
				m.log.Warn("remove an old backup", "path", p, "err", err)
			}
			continue
		}
		keep = append(keep, p)
	}
	return keep
}

// editFile changes a file. It writes only a changed content.
func (m *Manager) editFile(path string, f func([]byte) ([]byte, error)) error {
	b, err := readOptional(path)
	if err != nil {
		return err
	}
	out, err := f(b)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if bytes.Equal(out, b) {
		return nil
	}
	return writeIfChanged(path, out, 0o600)
}

// linkFile links dst to src when share is true, and removes that link when
// it is false. A real file at dst stays. A login file keeps the own login
// at dst.own while the link is there.
func linkFile(src, dst string, share, login bool) error {
	if !share {
		return unlinkFile(src, dst, login)
	}
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	fi, err := os.Lstat(dst)
	switch {
	case err != nil:
	case fi.Mode()&os.ModeSymlink != 0:
		if t, _ := os.Readlink(dst); t == src { // a bad link reads as ""
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	case !login:
		return nil
	default:
		if err := os.Rename(dst, dst+ownSuffix); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.Symlink(src, dst)
}

// unlinkFile removes the link from dst to src, and gives a login file its
// own login back.
func unlinkFile(src, dst string, login bool) error {
	fi, err := os.Lstat(dst)
	exists := err == nil
	if exists && fi.Mode()&os.ModeSymlink != 0 {
		if t, _ := os.Readlink(dst); t == src { // a bad link reads as ""
			if err := os.Remove(dst); err != nil {
				return err
			}
			exists = false
		}
	}
	if login && !exists {
		if err := os.Rename(dst+ownSuffix, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// linkEach links each entry of the user folder src into dst. A product
// file and a real entry stay.
func linkEach(src, dst string, share bool, product map[string][]byte, rel string) error {
	if !share {
		entries, err := os.ReadDir(dst)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			p := filepath.Join(dst, e.Name())
			if t, err := os.Readlink(p); err == nil && strings.HasPrefix(t, src+string(filepath.Separator)) {
				if err := os.Remove(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil
	}
	var errs []error
	for _, e := range entries {
		if _, ok := product[filepath.Join(rel, e.Name())]; ok {
			continue
		}
		errs = append(errs, linkFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), true, false))
	}
	return errors.Join(errs...)
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeIfChanged writes a file through a temporary file. An equal content
// leaves the file as it is.
func writeIfChanged(path string, data []byte, perm os.FileMode) error {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func etag(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}
