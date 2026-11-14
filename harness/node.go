package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// DefaultNodeVersion is the Node 24 LTS release that the visor downloads
	// when an npm install finds no npm.
	DefaultNodeVersion = "24.22.2"

	defaultNodeBaseURL = "https://nodejs.org/dist"
)

// nodePlatform gives the OS and CPU names of the Node download.
func (m *Manager) nodePlatform() (string, string, error) {
	goos, arch := runtime.GOOS, ""
	switch runtime.GOARCH {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "x64"
	}
	if (goos != "darwin" && goos != "linux") || arch == "" {
		return "", "", fmt.Errorf("%w: no Node download for %s/%s", ErrUnsupported, runtime.GOOS, runtime.GOARCH)
	}
	return goos, arch, nil
}

// node gives the folder of the private Node. It downloads Node on the first
// use, and checks the archive against SHASUMS256.txt.
func (m *Manager) node(ctx context.Context) (string, error) {
	m.nodeMu.Lock()
	defer m.nodeMu.Unlock()
	ver := strings.TrimPrefix(m.o.Node.Version, "v")
	if ver == "" {
		ver = DefaultNodeVersion
	}
	root := filepath.Join(m.home, "runtime", "node")
	dst := filepath.Join(root, "v"+ver)
	if fi, err := os.Stat(filepath.Join(dst, "bin", "node")); err == nil && fi.Mode().IsRegular() {
		return dst, nil
	}
	goos, arch, err := m.nodePlatform()
	if err != nil {
		return "", err
	}
	base := strings.TrimSuffix(m.o.Node.BaseURL, "/")
	if base == "" {
		base = defaultNodeBaseURL
	}
	name := fmt.Sprintf("node-v%s-%s-%s.tar.gz", ver, goos, arch)
	digest, err := m.nodeDigest(ctx, base+"/v"+ver+"/SHASUMS256.txt", name)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", ver, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("node %s: %w", ver, err)
	}
	tmp, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return "", fmt.Errorf("node %s: %w", ver, err)
	}
	defer m.removeAll(tmp)
	f := download{url: base + "/v" + ver + "/" + name, digest: digest, kind: "tar.gz", strip: 1}
	if err := m.fetchTo(ctx, f, tmp, ""); err != nil {
		return "", fmt.Errorf("node %s: %w", ver, err)
	}
	if err := os.RemoveAll(dst); err != nil {
		return "", fmt.Errorf("node %s: %w", ver, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("node %s: %w", ver, err)
	}
	m.log.Info("install node", "version", ver)
	return dst, nil
}

// nodeDigest reads the sha256 of one Node archive from SHASUMS256.txt.
func (m *Manager) nodeDigest(ctx context.Context, url, name string) (string, error) {
	sums, err := m.getMeta(ctx, url)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return "sha256:" + strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%w: SHASUMS256.txt has no %s", ErrDownload, name)
}
