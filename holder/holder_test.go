package holder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tuiScript = `echo ready; while IFS= read -r l; do case "$l" in exit) exit 3;; size) stty size;; *) echo "got:$l";; esac; done`

func TestRunServesPeers(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	cfg := Config{
		ID: "s1", Socket: filepath.Join(dir, "h.sock"), Command: []string{"sh", "-c", tuiScript},
		Log: filepath.Join(dir, "pty.log"), ExitFile: filepath.Join(dir, "s1.exit"), Cols: 100, Rows: 30,
	}
	errc := make(chan error, 1)
	go func() { errc <- Run(t.Context(), cfg) }()
	c := dial(t, cfg.Socket)
	defer c.Close()
	if h := c.Hello(); h.Protocol != Protocol || h.ID != "s1" || h.ChildPID == 0 || h.Cols != 100 || h.Rows != 30 {
		t.Fatalf("hello: %+v", h)
	}
	var out bytes.Buffer
	readUntil(t, c, &out, func(Msg) bool { return strings.Contains(out.String(), "ready") })
	if err := c.Input([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, &out, func(Msg) bool { return strings.Contains(out.String(), "got:hi") })
	if err := c.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, &out, func(m Msg) bool { return m.Type == MsgResized && m.Size == Size{Cols: 120, Rows: 40} })
	if err := c.Input([]byte("size\r")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, &out, func(Msg) bool { return strings.Contains(out.String(), "40 120") })

	second := dial(t, cfg.Socket)
	defer second.Close()
	var snap bytes.Buffer
	readUntil(t, second, &snap, func(m Msg) bool { return m.Type == MsgSnapshot })
	if !strings.Contains(snap.String(), "got:hi") {
		t.Fatalf("snapshot of the second peer: %q", snap.String())
	}

	if err := c.Input([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Client{c, second} {
		var exit Msg
		readUntil(t, p, &out, func(m Msg) bool { exit = m; return m.Type == MsgExit })
		if exit.Exit.Code != 3 {
			t.Fatalf("exit code %d, want 3", exit.Exit.Code)
		}
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{cfg.Log, cfg.ExitFile} {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
	var e Exit
	if data, err := os.ReadFile(cfg.ExitFile); err != nil || json.Unmarshal(data, &e) != nil || e.Code != 3 {
		t.Fatalf("exit file: %v %+v", err, e)
	}
	if log, _ := os.ReadFile(cfg.Log); !strings.Contains(string(log), "got:hi") { //nolint:errcheck // the content check covers it
		t.Fatalf("pty.log: %q", log)
	}
	if _, err := os.Stat(cfg.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket stays: %v", err)
	}
}

func TestKillEndsTheProcess(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	cfg := Config{ID: "s2", Socket: filepath.Join(dir, "h.sock"), Command: []string{"sh", "-c", "trap '' TERM; sleep 30"}}
	errc := make(chan error, 1)
	go func() { errc <- Run(t.Context(), cfg) }()
	c := dial(t, cfg.Socket)
	defer c.Close()
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	var exit Msg
	var out bytes.Buffer
	readUntil(t, c, &out, func(m Msg) bool { exit = m; return m.Type == MsgExit })
	if exit.Exit.Code < 128 {
		t.Fatalf("exit code %d, want a signal", exit.Exit.Code)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestConfig(t *testing.T) {
	t.Parallel()
	if _, err := ReadConfig(strings.NewReader(`{"id":"a"}`), "b"); err == nil {
		t.Fatal("a config for another session")
	}
	for name, c := range map[string]Config{
		"no id":      {Socket: "x", Command: []string{"sh"}},
		"no command": {ID: "a", Socket: "x"},
		"no socket":  {ID: "a", Command: []string{"sh"}},
		"long path":  {ID: "a", Command: []string{"sh"}, Socket: strings.Repeat("x", 200)},
	} {
		if err := Run(t.Context(), c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestDialRefusesOtherProtocol checks that a client refuses a holder of
// another protocol version.
func TestDialRefusesOtherProtocol(t *testing.T) {
	t.Parallel()
	for _, version := range []int{1, 3} {
		path := filepath.Join(shortDir(t), "h.sock")
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			data, _ := json.Marshal(Hello{Protocol: version}) //nolint:errchkjson // a plain struct
			_ = writeMsg(conn, MsgHello, data)                // the client checks the result
		}()
		if _, err := Dial(t.Context(), path, Cursor{}); !errors.Is(err, ErrProtocol) {
			t.Errorf("protocol %d: %v", version, err)
		}
	}
}

// TestPTYHolderRefusesOldPeer checks that a PTY holder closes the
// connection of a peer with another protocol version.
func TestPTYHolderRefusesOldPeer(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	cfg := Config{ID: "s3", Socket: filepath.Join(dir, "h.sock"), Command: []string{"sh", "-c", "sleep 30"}}
	errc := make(chan error, 1)
	go func() { errc <- Run(t.Context(), cfg) }()
	good := dial(t, cfg.Socket)
	defer good.Close()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", cfg.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if typ, _, err := readMsg(conn); err != nil || typ != MsgHello {
		t.Fatalf("holder hello: %v %v", typ, err)
	}
	data, _ := json.Marshal(Hello{Protocol: 1}) //nolint:errchkjson // a plain struct
	if err := writeMsg(conn, MsgHello, data); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readMsg(conn); err == nil {
		t.Fatal("the holder keeps an old peer")
	}
	again := dial(t, cfg.Socket)
	defer again.Close()

	if err := good.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestPTYSnapshotAfterWrap(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	cfg := Config{
		ID: "wrap", Socket: filepath.Join(dir, "h.sock"), ExitFile: filepath.Join(dir, "wrap.exit"),
		Command: []string{"sh", "-c", `yes 0123456789abcdef | head -c 614400; echo END; read -r l`},
	}
	go func() { _ = Run(t.Context(), cfg) }() //nolint:errcheck // the test ends the holder with its context
	c := dial(t, cfg.Socket)
	defer c.Close()
	var out bytes.Buffer
	readUntil(t, c, &out, func(Msg) bool { return bytes.HasSuffix(out.Bytes(), []byte("END\r\n")) })
	if out.Len() < DefaultRing {
		t.Fatalf("output %d bytes", out.Len())
	}
	second := dial(t, cfg.Socket)
	defer second.Close()
	var snap bytes.Buffer
	readUntil(t, second, &snap, func(m Msg) bool { return m.Type == MsgSnapshot })
	if want := out.Bytes()[out.Len()-DefaultRing:]; !bytes.Equal(snap.Bytes(), want) {
		t.Fatalf("snapshot of %d bytes differs from the last %d bytes of the output", snap.Len(), DefaultRing)
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) }) // a test folder
	return dir
}

func dial(t *testing.T, path string) *Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		c, err := Dial(ctx, path, Cursor{})
		cancel()
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readUntil reads messages, keeps the bytes in out, and stops at the first
// message that matches.
func readUntil(t *testing.T, c *Client, out *bytes.Buffer, stop func(Msg) bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		for {
			m, err := c.Read()
			if err != nil {
				done <- err
				return
			}
			out.Write(m.Data)
			if stop(m) {
				done <- nil
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, out.String())
		}
	case <-time.After(10 * time.Second):
		_ = c.Close() // ends the reader
		<-done
		t.Fatalf("timeout (output %q)", out.String())
	}
}
