package harness

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

const (
	downloadTimeout = 30 * time.Minute
	metaTimeout     = 30 * time.Second
	maxMetaBytes    = 8 << 20
	maxUnpackBytes  = 4 << 30
)

// download is one file to fetch, check, and put in a folder.
type download struct {
	url string
	// digest is "sha256:<hex>".
	digest string
	// size is the byte size, or zero when unknown.
	size int64
	// kind is the file format: raw, tar.gz, or zip.
	kind agentwire.ArchiveKind
	// strip is the number of leading path parts to cut from archive entries.
	strip int
}

// fetchTo downloads d into dir. A raw file goes to dir/rawPath. An archive is
// unpacked into dir. It checks the size and the digest before it unpacks.
func (m *Manager) fetchTo(ctx context.Context, d download, dir, rawPath string) error {
	if d.kind == "" {
		d.kind = agentwire.ArchiveTarGz
	}
	switch d.kind {
	case agentwire.ArchiveRaw:
		dst := filepath.Join(dir, filepath.FromSlash(rawPath))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // the install folder is not secret
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) //nolint:gosec // a program
		if err != nil {
			return err
		}
		err = m.stream(ctx, d, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	case agentwire.ArchiveTarGz, agentwire.ArchiveZip:
	default:
		return fmt.Errorf("%w: archive format %q", ErrUnsupported, d.kind)
	}
	f, err := os.CreateTemp(filepath.Dir(dir), ".download-")
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close() // a read-only use after the check
		m.removeAll(f.Name())
	}()
	if err := m.stream(ctx, d, f); err != nil {
		return err
	}
	u, err := newUnpacker(dir, d.strip)
	if err != nil {
		return err
	}
	if d.kind == agentwire.ArchiveZip {
		return u.zip(f)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return u.tarGz(f)
}

// stream copies the body of d.url to w, and checks the size and the digest.
func (m *Manager) stream(ctx context.Context, d download, w io.Writer) error {
	want, ok := strings.CutPrefix(d.digest, "sha256:")
	if !ok || len(want) != sha256.Size*2 {
		return fmt.Errorf("%w: no sha256 digest to check %s", ErrDownload, d.url)
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDownload, err)
	}
	resp, err := m.downloadClient().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDownload, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body is read to the end or dropped
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("%w: GET %s: %s", ErrDownload, d.url, resp.Status)
	}
	limit := int64(maxUnpackBytes)
	if d.size > 0 {
		limit = d.size + 1
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, limit))
	if err != nil {
		return fmt.Errorf("%w: GET %s: %w", ErrDownload, d.url, err)
	}
	if d.size > 0 && n != d.size {
		return fmt.Errorf("%w: GET %s: %d bytes, want %d", ErrDownload, d.url, n, d.size)
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want) {
		return fmt.Errorf("%w: GET %s: the sha256 does not match", ErrDownload, d.url)
	}
	return nil
}

// getMeta reads a small file, such as a checksum list.
func (m *Manager) getMeta(ctx context.Context, url string) ([]byte, error) {
	c := m.o.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: metaTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDownload, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDownload, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body is read below
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%w: GET %s: %s", ErrDownload, url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %w", ErrDownload, url, err)
	}
	return body, nil
}

// downloadClient is the client of Options.HTTPClient with no total timeout.
func (m *Manager) downloadClient() *http.Client {
	if m.o.HTTPClient == nil {
		return &http.Client{}
	}
	c := *m.o.HTTPClient
	c.Timeout = 0
	return &c
}

func (m *Manager) removeAll(path string) {
	if err := os.RemoveAll(path); err != nil {
		m.log.Warn("remove a failed install", "path", filepath.Base(path), "err", err)
	}
}

// unpacker puts the entries of an archive into root. It refuses an entry
// that leaves root, and each entry that is not a file, a folder, or a link
// that stays in root.
type unpacker struct {
	root  string
	real  string
	strip int
	total int64
}

func newUnpacker(root string, strip int) (*unpacker, error) {
	if err := os.MkdirAll(root, 0o755); err != nil { //nolint:gosec // the install folder is not secret
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return &unpacker{root: root, real: real, strip: strip}, nil
}

func (u *unpacker) tarGz(r io.Reader) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	defer func() { _ = zr.Close() }() // a read-only use
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the archive: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeXGlobalHeader:
		case tar.TypeDir:
			err = u.dir(h.Name)
		case tar.TypeReg:
			err = u.file(h.Name, tr, h.Mode&0o111 != 0)
		case tar.TypeSymlink:
			err = u.symlink(h.Name, h.Linkname)
		case tar.TypeLink:
			err = u.hardlink(h.Name, h.Linkname)
		default:
			err = fmt.Errorf("%w: archive entry %q has an unsupported type", ErrInvalid, h.Name)
		}
		if err != nil {
			return err
		}
	}
}

func (u *unpacker) zip(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	for _, e := range zr.File {
		mode := e.Mode()
		switch {
		case mode.IsDir():
			err = u.dir(e.Name)
		case mode.IsRegular():
			err = u.zipFile(e)
		case mode&os.ModeSymlink != 0:
			err = u.zipLink(e)
		default:
			err = fmt.Errorf("%w: archive entry %q has an unsupported type", ErrInvalid, e.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *unpacker) zipFile(e *zip.File) error {
	rc, err := e.Open()
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	defer func() { _ = rc.Close() }() // a read-only use
	return u.file(e.Name, rc, e.Mode()&0o111 != 0)
}

func (u *unpacker) zipLink(e *zip.File) error {
	rc, err := e.Open()
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	defer func() { _ = rc.Close() }() // a read-only use
	target, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	return u.symlink(e.Name, string(target))
}

// rel gives the path of an entry under root, with the leading parts cut. ok
// is false for an entry that the cut removes.
func (u *unpacker) rel(name string) (string, bool, error) {
	if strings.ContainsRune(name, '\\') || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", false, fmt.Errorf("%w: archive entry %q", ErrInvalid, name)
	}
	var parts []string
	for p := range strings.SplitSeq(name, "/") {
		switch p {
		case "", ".":
		case "..":
			return "", false, fmt.Errorf("%w: archive entry %q", ErrInvalid, name)
		default:
			parts = append(parts, p)
		}
	}
	if len(parts) <= u.strip {
		return "", false, nil
	}
	return filepath.Join(parts[u.strip:]...), true, nil
}

// dst gives the path of an entry and makes its folder. The real folder must
// stay in root.
func (u *unpacker) dst(name string) (string, string, error) {
	rel, ok, err := u.rel(name)
	if err != nil || !ok {
		return "", "", err
	}
	dst := filepath.Join(u.root, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // the install folder is not secret
		return "", "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(dst))
	if err != nil {
		return "", "", err
	}
	if dir != u.real && !strings.HasPrefix(dir, u.real+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: archive entry %q leaves the folder", ErrInvalid, name)
	}
	return dst, rel, nil
}

func (u *unpacker) dir(name string) error {
	dst, _, err := u.dst(name)
	if err != nil || dst == "" {
		return err
	}
	return os.MkdirAll(dst, 0o755) //nolint:gosec // the install folder is not secret
}

func (u *unpacker) file(name string, r io.Reader, exec bool) error {
	dst, _, err := u.dst(name)
	if err != nil || dst == "" {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // mode from the archive, 0644 or 0755
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxUnpackBytes-u.total+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	if u.total += n; u.total > maxUnpackBytes {
		return fmt.Errorf("%w: the archive is too large", ErrInvalid)
	}
	return nil
}

func (u *unpacker) symlink(name, target string) error {
	dst, rel, err := u.dst(name)
	if err != nil || dst == "" {
		return err
	}
	if target == "" || filepath.IsAbs(target) || strings.HasPrefix(target, "/") || strings.ContainsRune(target, '\\') {
		return fmt.Errorf("%w: link %q points out of the folder", ErrInvalid, name)
	}
	if !filepath.IsLocal(filepath.Join(filepath.Dir(rel), filepath.FromSlash(target))) {
		return fmt.Errorf("%w: link %q points out of the folder", ErrInvalid, name)
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(target, dst)
}

func (u *unpacker) hardlink(name, target string) error {
	dst, _, err := u.dst(name)
	if err != nil || dst == "" {
		return err
	}
	src, ok, err := u.rel(target)
	if err != nil || !ok {
		return fmt.Errorf("%w: link %q points out of the folder", ErrInvalid, name)
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Link(filepath.Join(u.root, src), dst)
}
