package agentvisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/holder"
)

// fakeTUI prints, reads lines, and answers a few commands.
const fakeTUI = `stty -echo 2>/dev/null; echo "tui ready"; while IFS= read -r l; do case "$l" in exit) exit 7;; size) stty size;; *) echo "echo:$l";; esac; done`

// TestMain runs the test binary as a holder when the visor starts one, or
// as a visor process that a test kills.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "hold" {
		os.Exit(runTestHolder(os.Args[2]))
	}
	if len(os.Args) == 2 && os.Args[1] == "visor-child" {
		os.Exit(runChildVisor())
	}
	os.Exit(m.Run())
}

func TestPTYAttachLeaseAndExit(t *testing.T) {
	t.Parallel()
	home := shortHome(t)
	v := newPTYVisor(t, home, nil)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Command: []string{"sh", "-c", fakeTUI}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != KindPTY || s.Role != RoleShell {
		t.Fatalf("session: %+v", s)
	}
	if got := waitState(t, v, s.ID, StateRunning); got.PID == 0 {
		t.Fatalf("no PID: %+v", got)
	}
	a1 := attachTerm(t, v, s.ID, AttachOptions{Cols: 100, Rows: 30})
	first := a1.waitFrame(func(f Frame) bool { return f.Type == FrameAttached })
	if first.Source != KindPTY || !first.Writable || !first.Resizable {
		t.Fatalf("attached: %+v", first)
	}
	a1.waitFrame(func(f Frame) bool { return f.Type == FrameLease && f.Holder == LeaseYou })
	a1.waitOutput("tui ready")
	a1.input("size\r")
	a1.waitOutput("30 100")

	a2 := attachTerm(t, v, s.ID, AttachOptions{})
	a2.waitFrame(func(f Frame) bool { return f.Type == FrameLease && f.Holder == LeaseOther })
	a2.waitOutput("tui ready")
	a1.input("hello\r")
	a1.waitOutput("echo:hello")
	a2.waitOutput("echo:hello")

	if err := a2.a.Handle(t.Context(), ClientFrame{Type: ClientControl, Cols: 120, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	a2.waitFrame(func(f Frame) bool { return f.Type == FrameLease && f.Holder == LeaseYou })
	a1.waitFrame(func(f Frame) bool { return f.Type == FrameLease && f.Holder == LeaseOther })
	a1.waitFrame(func(f Frame) bool { return f.Type == FrameResized && f.Cols == 120 && f.Rows == 40 })
	a2.input("size\r")
	a2.waitOutput("40 120")
	if err := a2.a.Handle(t.Context(), ClientFrame{Type: ClientControl}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("control with no size: %v", err)
	}
	a2.waitFrame(func(f Frame) bool { return f.Type == FrameError && f.Code == "invalid" })

	if err := a2.a.Close(); err != nil {
		t.Fatal(err)
	}
	a1.waitFrame(func(f Frame) bool { return f.Type == FrameLease && f.Holder == LeaseNone })
	if _, err := v.Send(t.Context(), s.ID, Input{Text: "more"}); err != nil {
		t.Fatal(err)
	}
	a1.waitOutput("echo:more")

	a1.input("exit\r")
	a1.waitFrame(func(f Frame) bool { return f.Type == FrameEnded && f.Reason == EndExited })
	if _, err := a1.a.Recv(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("recv after the end: %v", err)
	}
	got := waitState(t, v, s.ID, StateExited)
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("exit code: %v", got.ExitCode)
	}
	log, err := os.Stat(filepath.Join(home, "sessions", s.ID, "pty.log"))
	if err != nil || log.Mode().Perm() != 0o600 {
		t.Fatalf("pty.log: %v %v", log, err)
	}
	if _, err := v.Attach(t.Context(), s.ID, AttachOptions{}); !errors.Is(err, ErrState) {
		t.Fatalf("attach to an exited session: %v", err)
	}
	checkSessionSeq(t, v, s.ID)
}

func TestPTYSlowClientGetsSnapshot(t *testing.T) {
	t.Parallel()
	home := shortHome(t)
	v := newPTYVisor(t, home, nil)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Role: RoleScript, Command: []string{"sh", "-c", "read -r l; head -c 3000000 /dev/zero | tr '\\0' x; echo; echo flood-end"}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateRunning)
	a, err := v.Attach(t.Context(), s.ID, AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Input(t.Context(), []byte("go\r")); err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateExited)
	snapshots, total := 0, 0
	var last, tail []byte
	for {
		f, err := a.Recv(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case f.Op == OpSnapshot:
			snapshots++
			last, tail = f.Data, nil
		case snapshots > 0:
			tail = append(tail, f.Data...)
		}
		total += len(f.Data)
	}
	// Output after the last snapshot continues it.
	if snapshots < 2 || len(last) > holder.DefaultRing || !bytes.Contains(append(last, tail...), []byte("flood-end")) {
		t.Fatalf("snapshots %d, last %d bytes, tail %d bytes, total %d", snapshots, len(last), len(tail), total)
	}
	if total > 2*maxBuffered {
		t.Fatalf("the slow client got %d bytes", total)
	}
}

func TestPTYSurvivesVisorRestart(t *testing.T) {
	t.Parallel()
	home := shortHome(t)
	v := newPTYVisor(t, home, nil)
	s, err := v.Start(t.Context(), StartSpec{Command: []string{"sh", "-c", fakeTUI}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pid := waitState(t, v, s.ID, StateRunning).PID
	a := attachTerm(t, v, s.ID, AttachOptions{})
	a.input("before\r")
	a.waitOutput("echo:before")
	if err := v.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.waitFrame(func(f Frame) bool { return f.Type == FrameEnded && f.Reason == EndClosed })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the process ended with the visor: %v", err)
	}

	v = newPTYVisor(t, home, nil)
	defer v.Close(context.Background())
	got := waitState(t, v, s.ID, StateRunning)
	if got.PID != pid {
		t.Fatalf("recovered PID %d, want %d", got.PID, pid)
	}
	b := attachTerm(t, v, s.ID, AttachOptions{})
	b.waitOutput("echo:before")
	b.input("after\r")
	b.waitOutput("echo:after")
	if err := v.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateExited)
	checkSessionSeq(t, v, s.ID)
}

func TestPTYScreenAndKeys(t *testing.T) {
	t.Parallel()
	v := newPTYVisor(t, shortHome(t), nil)
	defer v.Close(context.Background())
	// The program asks for the cursor position. No client is attached, so the
	// emulator of the visor answers.
	query := `stty -echo -icanon min 1; printf '\033[6n'; r=$(dd bs=1 count=6 2>/dev/null); printf 'got:%s\n' "$(printf %s "$r" | tr -d '\033')"; ` + fakeTUI
	s, err := v.Start(t.Context(), StartSpec{Command: []string{"sh", "-c", query}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitScreen(t, v, s.ID, "got:[1;1R")
	waitScreen(t, v, s.ID, "tui ready")
	if err := v.Keys(t.Context(), s.ID, []byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	sc := waitScreen(t, v, s.ID, "echo:hello")
	// No bracketed paste mode: the text goes as typed.
	if err := v.Paste(t.Context(), s.ID, "plain\r"); err != nil {
		t.Fatal(err)
	}
	waitScreen(t, v, s.ID, "echo:plain")
	if sc.Cols != 80 || sc.Rows != 24 || len(sc.Lines) != 24 {
		t.Fatalf("screen size %dx%d with %d lines", sc.Cols, sc.Rows, len(sc.Lines))
	}
	out, err := v.Output(t.Context(), s.ID)
	if err != nil || !strings.Contains(out, "echo:hello\n") || strings.Contains(out, "\x1b") {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if err := v.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateExited)
	if out, err := v.Output(t.Context(), s.ID); err != nil || !strings.Contains(out, "echo:hello") {
		t.Fatalf("Output after the exit = %q, %v", out, err)
	}
	if _, err := v.Screen(t.Context(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Screen of a missing session: %v", err)
	}
}

// A home path too long for a socket path puts the sockets in /tmp. The
// session still survives a visor restart.
func TestPTYLongHome(t *testing.T) {
	t.Parallel()
	home := filepath.Join(shortHome(t), strings.Repeat("h", holder.MaxSocketPath))
	hs := newHolders(home, "holders", nil)
	if !strings.HasPrefix(hs.sockDir, shortSocketRoot()) {
		t.Fatalf("socket folder %s", hs.sockDir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(hs.sockDir)) }) // a test folder
	v := newPTYVisor(t, home, nil)
	s, err := v.Start(t.Context(), StartSpec{Command: []string{"sh", "-c", fakeTUI}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pid := waitState(t, v, s.ID, StateRunning).PID
	if err := v.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	v = newPTYVisor(t, home, nil)
	defer v.Close(context.Background())
	if got := waitState(t, v, s.ID, StateRunning); got.PID != pid {
		t.Fatalf("recovered PID %d, want %d", got.PID, pid)
	}
	if err := v.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateExited)
}

func TestPTYExitWhileVisorDown(t *testing.T) {
	t.Parallel()
	home := shortHome(t)
	v := newPTYVisor(t, home, nil)
	s, err := v.Start(t.Context(), StartSpec{Command: []string{"sh", "-c", fakeTUI}, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateRunning)
	if err := v.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(home, "holders", s.ID+".sock")
	c, err := holder.Dial(t.Context(), sock, holder.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Input([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	for {
		m, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == holder.MsgExit {
			break
		}
	}
	_ = c.Close() // the holder ended
	v = newPTYVisor(t, home, nil)
	defer v.Close(context.Background())
	got := waitState(t, v, s.ID, StateExited)
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("exit code: %v", got.ExitCode)
	}
	if _, err := os.Stat(filepath.Join(home, "holders", s.ID+".exit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the exit file stays: %v", err)
	}
}

func TestPTYCommand(t *testing.T) {
	t.Parallel()
	b := newPTYBackend("", nil, nil, nil)
	env := []string{"SHELL=/bin/zsh"}
	for name, tc := range map[string]struct {
		l    Launch
		want string
		err  error
	}{
		"login shell":   {Launch{Spec: StartSpec{Role: RoleShell}}, "/bin/zsh -l", nil},
		"shell command": {Launch{Spec: StartSpec{Role: RoleShell, Command: []string{"bash"}}}, "bash", nil},
		"no script":     {Launch{Spec: StartSpec{Role: RoleScript}}, "", ErrInvalid},
		"agent":         {Launch{Spec: StartSpec{Role: RoleAgent, Harness: "claude", Model: "m1", ExtraArgs: []string{"--x"}}}, "claude --model m1 --x", nil},
		"agent binary":  {Launch{Binary: "/opt/c", Spec: StartSpec{Role: RoleAgent, Harness: "claude"}}, "/opt/c", nil},
		"agent resume":  {Launch{Resume: true, Spec: StartSpec{Role: RoleAgent, Harness: "claude"}}, "", ErrUnsupported},
	} {
		argv, err := b.command(tc.l, env)
		if !errors.Is(err, tc.err) || strings.Join(argv, " ") != tc.want {
			t.Errorf("%s: %q %v", name, argv, err)
		}
	}
}

func TestPasteMode(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		out  string
		cur  bool
		want bool
	}{
		{"", false, false},
		{"", true, true},
		{"a\x1b[?2004hb", false, true},
		{"\x1b[?2004h\x1b[?2004l", true, false},
		{"\x1b[?2004l x \x1b[?2004h", false, true},
	} {
		if got := pasteMode([]byte(c.out), c.cur); got != c.want {
			t.Errorf("pasteMode(%q, %v) = %v", c.out, c.cur, got)
		}
	}
	h := &ptyHandle{paste: true}
	if got := string(h.pasteData("a\nb")); got != "\x1b[200~a\nb\x1b[201~" {
		t.Errorf("pasteData = %q", got)
	}
}

func TestPlainText(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"a\x1b[1;32mb\x1b[0mc":       "abc",
		"one\r\ntwo\rthree":          "one\ntwo\nthree",
		"x\x1b[2Cy":                  "x y",
		"\x1b]0;title\x07after":      "after",
		"\x1b]8;;http://e\x1b\\link": "link",
		"\x1b(Bok\x00\x7f":           "ok",
	} {
		if got := plainText([]byte(in)); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}

func runTestHolder(id string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := holder.ReadConfig(os.Stdin, id)
	if err == nil {
		err = holder.Run(ctx, cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// termClient reads the frames of one attach stream in the test.
type termClient struct {
	t      *testing.T
	a      *AttachStream
	out    bytes.Buffer
	frames []Frame
}

func attachTerm(t *testing.T, v *Visor, id string, o AttachOptions) *termClient {
	t.Helper()
	a, err := v.Attach(t.Context(), id, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() }) // a test stream
	return &termClient{t: t, a: a}
}

func (c *termClient) input(s string) {
	c.t.Helper()
	if err := c.a.Input(c.t.Context(), []byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *termClient) waitOutput(s string) {
	c.t.Helper()
	if strings.Contains(c.out.String(), s) {
		return
	}
	c.waitFrame(func(f Frame) bool { return f.Binary() && strings.Contains(c.out.String(), s) })
}

// waitFrame reads frames until one matches. It acks each binary frame.
func (c *termClient) waitFrame(match func(Frame) bool) Frame {
	c.t.Helper()
	for i, f := range c.frames {
		if match(f) {
			c.frames = c.frames[i+1:]
			return f
		}
	}
	ctx, cancel := context.WithTimeout(c.t.Context(), 10*time.Second)
	defer cancel()
	for {
		f, err := c.a.Recv(ctx)
		if err != nil {
			c.t.Fatalf("recv: %v (output %q)", err, c.out.String())
		}
		if f.Binary() {
			c.out.Write(f.Data)
			if err := c.a.Handle(ctx, ClientFrame{Type: ClientAck, Seq: f.Seq}); err != nil {
				c.t.Fatal(err)
			}
		}
		if match(f) {
			return f
		}
		if !f.Binary() {
			c.frames = append(c.frames, f)
		}
	}
}

func newPTYVisor(t *testing.T, home string, term TerminalCommand) *Visor {
	t.Helper()
	v, err := New(t.Context(), Options{
		Home: home, Client: "test", TerminalCommand: term,
		Drivers: map[string]agentwire.Driver{"fake": agentwire.FakeDriver{Script: scriptSlowTurns}},
	})
	if err != nil {
		t.Fatalf("new visor: %v", err)
	}
	return v
}

// shortHome gives a home with a short path, for the socket limit of macOS.
// Its cleanup ends the holders that a test leaves.
func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "av")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killHolders(home)
		_ = os.RemoveAll(home) // a test folder
	})
	return home
}

func killHolders(home string) {
	socks, _ := filepath.Glob(filepath.Join(home, "*", "*.sock")) //nolint:errcheck // a fixed pattern
	for _, sock := range socks {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if c, err := holder.Dial(ctx, sock, holder.Cursor{}); err == nil {
			if c.Hello().Mode == holder.ModeWire {
				_ = c.Call(holder.Call{ID: 1, Op: holder.OpClose}) // test cleanup
			} else {
				_ = c.Kill() // test cleanup
			}
			for {
				if m, err := c.Read(); err != nil || m.Type == holder.MsgExit {
					break
				}
			}
			_ = c.Close() // test cleanup
		}
		cancel()
	}
}

// checkSessionSeq checks that a replay of a session has no gaps and no
// duplicates in the global sequence.
func checkSessionSeq(t *testing.T, v *Visor, id string) {
	t.Helper()
	s, err := v.Session(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	st, err := v.Events(t.Context(), Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkSeq(t, collect(t, st, func(e Event) bool { return e.Seq >= s.Seq }), 0)
}

// waitScreen waits until a line of the screen of session id is want.
func waitScreen(t *testing.T, v *Visor, id, want string) Screen {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sc, err := v.Screen(t.Context(), id)
		if err == nil && slices.Contains(sc.Lines, want) {
			return sc
		}
		if time.Now().After(deadline) {
			t.Fatalf("no line %q on the screen: %v %q", want, err, sc.Lines)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
