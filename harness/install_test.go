package harness

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"
)

type tarEntry struct {
	name, body, link string
	typ              byte
}

// files serves fixed bodies by path, and counts the requests.
type files struct {
	mu   sync.Mutex
	body map[string][]byte
	hits map[string]int
	srv  *httptest.Server
}

func (f *files) put(path string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body[path] = b
}

func (f *files) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (r *rig) setAsset(f func(version string) (agentwire.Asset, error)) {
	r.wire.mu.Lock()
	defer r.wire.mu.Unlock()
	r.wire.asset = f
}

func TestInstallBinaryRaw(t *testing.T) {
	t.Parallel()
	r, srv := binaryRig(t, "bin/fakeh", agentwire.ArchiveRaw)
	ctx := t.Context()
	r.setAsset(func(v string) (agentwire.Asset, error) {
		b := []byte(fakeProgram(v))
		srv.put("/raw/"+v, b)
		return agentwire.Asset{URL: srv.srv.URL + "/raw/" + v, Digest: digestOf(b), Size: int64(len(b)), Archive: agentwire.ArchiveRaw, Program: "bin/fakeh"}, nil
	})
	h, err := r.m.Install(ctx, "fakeh", "1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(r.home, "bin", "fakeh", "1.4.0", "bin", "fakeh")
	if !h.Installed || h.Source != SourceVisor || h.Version != "1.4.0" || h.Path != want {
		t.Fatalf("after the install: %+v", h)
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("program: %v %v", fi, err)
	}
	// An empty version installs the latest release.
	if h, err = r.m.Install(ctx, "fakeh", ""); err != nil || h.Version != "2.0.0" {
		t.Fatalf("latest: %+v %v", h, err)
	}
	// The visor launches the copy.
	if l, err := r.m.Launch(ctx, "fakeh"); err != nil || l.Binary != h.Path {
		t.Fatalf("launch: %+v %v", l, err)
	}
	// A lookup that fails stops a binary install with an empty version.
	r.wire.mu.Lock()
	r.wire.latest = ""
	r.wire.mu.Unlock()
	r.m.mu.Lock()
	delete(r.m.latest, "fakeh")
	r.m.mu.Unlock()
	if _, err := r.m.Install(ctx, "fakeh", ""); !errors.Is(err, ErrDownload) {
		t.Fatalf("install while the lookup fails: %v", err)
	}
}

func TestInstallBinaryChecksum(t *testing.T) {
	t.Parallel()
	prog := []byte(fakeProgram("1.4.0"))
	for _, tc := range []struct {
		name   string
		asset  func(url string) agentwire.Asset
		status int
	}{
		{"digest", func(u string) agentwire.Asset {
			return agentwire.Asset{URL: u, Digest: digestOf([]byte("other")), Archive: agentwire.ArchiveRaw, Program: "bin/fakeh"}
		}, 0},
		{"size", func(u string) agentwire.Asset {
			return agentwire.Asset{URL: u, Digest: digestOf(prog), Size: int64(len(prog)) + 1, Archive: agentwire.ArchiveRaw, Program: "bin/fakeh"}
		}, 0},
		{"no digest", func(u string) agentwire.Asset {
			return agentwire.Asset{URL: u, Archive: agentwire.ArchiveRaw, Program: "bin/fakeh"}
		}, 0},
		{"http", func(u string) agentwire.Asset {
			return agentwire.Asset{URL: u + "-missing", Digest: digestOf(prog), Archive: agentwire.ArchiveRaw, Program: "bin/fakeh"}
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, srv := binaryRig(t, "bin/fakeh", agentwire.ArchiveRaw)
			srv.put("/raw", prog)
			r.setAsset(func(string) (agentwire.Asset, error) { return tc.asset(srv.srv.URL + "/raw"), nil })
			if _, err := r.m.Install(t.Context(), "fakeh", "1.4.0"); !errors.Is(err, ErrDownload) {
				t.Fatalf("install: %v", err)
			}
			if h, _ := r.m.Harness(t.Context(), "fakeh"); h.Installed || len(h.Installs) != 0 {
				t.Fatalf("after a failed install: %+v", h)
			}
			noLeftovers(t, filepath.Join(r.home, "bin", "fakeh"))
		})
	}
}

func TestInstallBinaryErrors(t *testing.T) {
	t.Parallel()
	r, _ := binaryRig(t, "bin/fakeh", agentwire.ArchiveRaw)
	ctx := t.Context()
	// No asset for the platform.
	if _, err := r.m.Install(ctx, "fakeh", "1.4.0"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("no download: %v", err)
	}
	// A metadata failure is a download failure.
	r.setAsset(func(string) (agentwire.Asset, error) {
		return agentwire.Asset{}, errors.New("GET x: 500 Internal Server Error")
	})
	if _, err := r.m.Install(ctx, "fakeh", "1.4.0"); !errors.Is(err, ErrDownload) {
		t.Fatalf("metadata failure: %v", err)
	}
}

func TestInstallBinaryArchive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		kind    agentwire.ArchiveKind
		program string
		data    func(t *testing.T) []byte
	}{
		{"tar.gz", agentwire.ArchiveTarGz, "bin/fakeh", func(t *testing.T) []byte {
			return tarGz(t,
				tarEntry{name: "bin/", typ: tar.TypeDir},
				tarEntry{name: "bin/fakeh", body: fakeProgram("1.4.0")},
				tarEntry{name: "bin/alias", typ: tar.TypeSymlink, link: "fakeh"},
				tarEntry{name: "share/note.txt", body: "note"})
		}},
		{"zip with the program at the root", agentwire.ArchiveZip, "bin/fakeh", func(t *testing.T) []byte {
			return zipFile(t, map[string]string{"fakeh": fakeProgram("1.4.0")})
		}},
		{"tar.gz with the program at the root", agentwire.ArchiveTarGz, "bin/fakeh", func(t *testing.T) []byte {
			return tarGz(t, tarEntry{name: "fakeh", body: fakeProgram("1.4.0")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, srv := binaryRig(t, tc.program, tc.kind)
			data := tc.data(t)
			srv.put("/a", data)
			r.setAsset(func(string) (agentwire.Asset, error) {
				return agentwire.Asset{URL: srv.srv.URL + "/a", Digest: digestOf(data), Size: int64(len(data)), Archive: tc.kind, Program: tc.program}, nil
			})
			h, err := r.m.Install(t.Context(), "fakeh", "1.4.0")
			if err != nil || !h.Installed || h.Version != "1.4.0" {
				t.Fatalf("install: %+v %v", h, err)
			}
			out, err := exec.CommandContext(t.Context(), h.Path, "--version").Output()
			if err != nil || !strings.HasPrefix(string(out), "1.4.0") {
				t.Fatalf("run the program: %q %v", out, err)
			}
			noLeftovers(t, filepath.Join(r.home, "bin", "fakeh"))
		})
	}
}

func TestInstallBinaryUnsafeArchive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind agentwire.ArchiveKind
		data func(t *testing.T) []byte
	}{
		{"dot dot", agentwire.ArchiveTarGz, func(t *testing.T) []byte {
			return tarGz(t, tarEntry{name: "../evil", body: "x"})
		}},
		{"inner dot dot", agentwire.ArchiveTarGz, func(t *testing.T) []byte {
			return tarGz(t, tarEntry{name: "a/../../evil", body: "x"})
		}},
		{"absolute", agentwire.ArchiveTarGz, func(t *testing.T) []byte {
			return tarGz(t, tarEntry{name: "/tmp/evil", body: "x"})
		}},
		{"absolute link", agentwire.ArchiveTarGz, func(t *testing.T) []byte {
			return tarGz(t, tarEntry{name: "bin/fakeh", typ: tar.TypeSymlink, link: "/bin/sh"})
		}},
		{"link out", agentwire.ArchiveTarGz, func(t *testing.T) []byte {
			return tarGz(t, tarEntry{name: "bin/fakeh", typ: tar.TypeSymlink, link: "../../../outside"})
		}},
		{"write through a link", agentwire.ArchiveTarGz, func(t *testing.T) []byte {
			return tarGz(t,
				tarEntry{name: "d", typ: tar.TypeSymlink, link: "../.."},
				tarEntry{name: "d/evil", body: "x"})
		}},
		{"device", agentwire.ArchiveTarGz, func(t *testing.T) []byte {
			return tarGz(t, tarEntry{name: "dev", typ: tar.TypeChar})
		}},
		{"zip dot dot", agentwire.ArchiveZip, func(t *testing.T) []byte {
			return zipFile(t, map[string]string{"../evil": "x"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, srv := binaryRig(t, "bin/fakeh", tc.kind)
			data := tc.data(t)
			srv.put("/a", data)
			r.setAsset(func(string) (agentwire.Asset, error) {
				return agentwire.Asset{URL: srv.srv.URL + "/a", Digest: digestOf(data), Archive: tc.kind, Program: "bin/fakeh"}, nil
			})
			if _, err := r.m.Install(t.Context(), "fakeh", "1.4.0"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("install: %v", err)
			}
			for _, p := range []string{"evil", filepath.Join("bin", "evil"), filepath.Join("bin", "fakeh", "evil")} {
				if _, err := os.Lstat(filepath.Join(r.home, p)); err == nil {
					t.Errorf("an entry wrote %s", p)
				}
			}
			noLeftovers(t, filepath.Join(r.home, "bin", "fakeh"))
		})
	}
}

func TestInstallableBinary(t *testing.T) {
	t.Parallel()
	r, _ := binaryRig(t, "bin/fakeh", agentwire.ArchiveRaw)
	ctx := t.Context()
	if h, err := r.m.Harness(ctx, "fakeh"); err != nil || !h.Installable {
		t.Fatalf("a listed platform: %+v %v", h, err)
	}
	r.m.o.Facts.Recipe = func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
		return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallBinary, Bin: "fakeh",
			Download: &agentwire.Download{Program: "bin/fakeh", Platforms: map[string]string{"plan9/386": "p"}}}, true
	}
	if h, err := r.m.Harness(ctx, "fakeh"); err != nil || h.Installable {
		t.Fatalf("an unlisted platform: %+v %v", h, err)
	}
	r.m.o.Facts.Recipe = func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
		return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallBinary, Bin: "fakeh"}, true
	}
	if h, err := r.m.Harness(ctx, "fakeh"); err != nil || h.Installable {
		t.Fatalf("no download: %+v %v", h, err)
	}

	// An npm recipe needs npm or the private Node, and its other tools.
	npm := func(requires ...string) {
		r.m.o.Facts.Recipe = func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
			return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallNPM, Package: "p", Bin: "fakeh", Requires: requires}, true
		}
	}
	npm("npm")
	if h, _ := r.m.Harness(ctx, "fakeh"); !h.Installable {
		t.Fatalf("npm on the path: %+v", h)
	}
	npm("npm", "bun")
	if h, _ := r.m.Harness(ctx, "fakeh"); h.Installable {
		t.Fatalf("bun missing: %+v", h)
	}
	empty := t.TempDir()
	r.m.o.Path = empty
	npm("npm")
	if _, _, err := r.m.nodePlatform(); err == nil {
		if h, _ := r.m.Harness(ctx, "fakeh"); !h.Installable {
			t.Fatalf("no npm, private Node on: %+v", h)
		}
	}
	r.m.o.Node.Off = true
	if h, _ := r.m.Harness(ctx, "fakeh"); h.Installable {
		t.Fatalf("no npm, private Node off: %+v", h)
	}
}

const fakePrivateNPM = `#!/bin/sh
spec="$3"; ver="${spec##*@}"
[ "$ver" = latest ] && ver=3.0.0
mkdir -p "$NPM_CONFIG_PREFIX/bin" "$NPM_CONFIG_PREFIX/lib/node_modules/fake-pkg"
echo "$PATH" > "$NPM_CONFIG_PREFIX/npm-path"
echo "$ver" > "$NPM_CONFIG_PREFIX/lib/node_modules/fake-pkg/cli.js"
ln -s ../lib/node_modules/fake-pkg/cli.js "$NPM_CONFIG_PREFIX/bin/fakeh"
`

const fakeNode = `#!/bin/sh
echo "$(cat "$1") (Fake node)"
`

func TestInstallNPMPrivateNode(t *testing.T) {
	t.Parallel()
	r, srv, name := nodeRig(t)
	ctx := t.Context()
	archive := tarGz(t,
		tarEntry{name: "node-v24.0.0/", typ: tar.TypeDir},
		tarEntry{name: "node-v24.0.0/bin/node", body: fakeNode},
		tarEntry{name: "node-v24.0.0/bin/npm", body: fakePrivateNPM})
	srv.put("/v24.0.0/"+name, archive)
	srv.put("/v24.0.0/SHASUMS256.txt", []byte("0000  other.tar.gz\n"+strings.TrimPrefix(digestOf(archive), "sha256:")+"  "+name+"\n"))

	h, err := r.m.Install(ctx, "fakeh", "")
	if err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(r.home, "runtime", "node", "v24.0.0")
	if h.Version != "2.0.0" || !h.Installed {
		t.Fatalf("after the install: %+v", h)
	}
	if fi, err := os.Lstat(h.Path); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the program is a wrapper: %v %v", fi, err)
	}
	script, _ := os.ReadFile(h.Path) // read below
	if !strings.HasPrefix(string(script), "#!/bin/sh\n") || !strings.Contains(string(script), "'"+filepath.Join(node, "bin", "node")+"'") {
		t.Fatalf("wrapper: %s", script)
	}
	// The wrapper runs the package script with the private Node.
	out, err := exec.CommandContext(ctx, h.Path, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "2.0.0 (Fake node)" {
		t.Fatalf("run the wrapper: %q %v", out, err)
	}
	pathFile, _ := os.ReadFile(filepath.Join(r.home, "bin", "fakeh", "2.0.0", "npm-path")) // read below
	if !strings.HasPrefix(string(pathFile), filepath.Join(node, "bin")+string(os.PathListSeparator)) {
		t.Fatalf("npm path: %q", pathFile)
	}
	// The next install reuses Node.
	if _, err := r.m.Install(ctx, "fakeh", "2.1.0"); err != nil {
		t.Fatal(err)
	}
	if n := srv.count("/v24.0.0/" + name); n != 1 {
		t.Fatalf("Node downloads: %d", n)
	}
	noLeftovers(t, filepath.Join(r.home, "runtime", "node"))
}

func TestInstallNPMNodeOff(t *testing.T) {
	t.Parallel()
	r, srv, _ := nodeRig(t)
	r.m.o.Node.Off = true
	if _, err := r.m.Install(t.Context(), "fakeh", "1.0.0"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("install with no npm: %v", err)
	}
	if h, _ := r.m.Harness(t.Context(), "fakeh"); h.Installable || h.Installed {
		t.Fatalf("harness: %+v", h)
	}
	if len(srv.hits) != 0 {
		t.Fatalf("requests with Node off: %v", srv.hits)
	}
	if _, err := os.Stat(filepath.Join(r.home, "runtime")); err == nil {
		t.Fatal("Node off made a runtime folder")
	}
}

func TestNodeChecksum(t *testing.T) {
	t.Parallel()
	r, srv, name := nodeRig(t)
	ctx := t.Context()
	archive := tarGz(t, tarEntry{name: "node-v24.0.0/bin/node", body: fakeNode})
	srv.put("/v24.0.0/"+name, archive)

	// A wrong checksum.
	srv.put("/v24.0.0/SHASUMS256.txt", []byte(strings.TrimPrefix(digestOf([]byte("other")), "sha256:")+"  "+name+"\n"))
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); !errors.Is(err, ErrDownload) {
		t.Fatalf("wrong checksum: %v", err)
	}
	// No line for the archive.
	srv.put("/v24.0.0/SHASUMS256.txt", []byte("0000  other.tar.gz\n"))
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); !errors.Is(err, ErrDownload) {
		t.Fatalf("no checksum line: %v", err)
	}
	// No checksum file.
	srv.mu.Lock()
	delete(srv.body, "/v24.0.0/SHASUMS256.txt")
	srv.mu.Unlock()
	if _, err := r.m.Install(ctx, "fakeh", "1.0.0"); !errors.Is(err, ErrDownload) {
		t.Fatalf("no checksum file: %v", err)
	}
	noLeftovers(t, filepath.Join(r.home, "runtime", "node"))
	noLeftovers(t, filepath.Join(r.home, "bin", "fakeh"))
	if entries, _ := os.ReadDir(filepath.Join(r.home, "runtime", "node")); len(entries) != 0 { // a missing folder has none
		t.Fatalf("Node folders after a failure: %v", entries)
	}
}

// fakeProgram is a program that prints its version.
func fakeProgram(version string) string {
	return "#!/bin/sh\necho \"" + version + " (Fake)\"\n"
}

func tarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if e.typ == 0 {
			e.typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Linkname: e.link}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipFile(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newFiles(t *testing.T) *files {
	t.Helper()
	f := &files{body: map[string][]byte{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b, ok := f.body[r.URL.Path]
		f.hits[r.URL.Path]++
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b) // a closed test client
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// binaryRig is a rig whose recipe is a binary download of program.
func binaryRig(t *testing.T, program string, kind agentwire.ArchiveKind) (*rig, *files) {
	t.Helper()
	r := newRig(t)
	srv := newFiles(t)
	r.m.o.Facts.Recipe = func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
		return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallBinary, Package: "fake-pkg", Bin: "fakeh",
			Download: &agentwire.Download{Source: agentwire.SourceManifest, Archive: kind, Program: program,
				Platforms: map[string]string{runtime.GOOS + "/" + runtime.GOARCH: "p"}}}, true
	}
	return r, srv
}

// noLeftovers checks that the install folder holds no temporary file.
func noLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir) // a missing folder has none
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("a failed install left %s", e.Name())
		}
	}
}

// nodeRig is a rig with no npm on the path, a fake Node archive on a test
// site, and its SHASUMS256.txt.
func nodeRig(t *testing.T) (*rig, *files, string) {
	t.Helper()
	r := newRig(t)
	if _, _, err := r.m.nodePlatform(); err != nil {
		t.Skip(err)
	}
	// The fake npm needs a few tools. The path holds links to them, but no npm.
	tools := filepath.Join(r.home, "bare")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"mkdir", "cat", "chmod", "ln", "dirname"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(tools, tool)); err != nil {
			t.Fatal(err)
		}
	}
	r.m.o.Path = tools
	srv := newFiles(t)
	r.m.o.Node = NodeOptions{Version: "24.0.0", BaseURL: srv.srv.URL}
	goos, arch, _ := r.m.nodePlatform()
	name := fmt.Sprintf("node-v24.0.0-%s-%s.tar.gz", goos, arch)
	return r, srv, name
}
