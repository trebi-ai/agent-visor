package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// detachKey (Ctrl-]) ends an attachment and leaves the session running.
const detachKey = 0x1d

// sessionAttach attaches the terminal of this process to a session.
func (a *app) sessionAttach(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<session-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error { return a.attach(ctx, v, cmd.Args().First()) })
}

// sessionShell starts a shell in a terminal session and attaches to it.
func (a *app) sessionShell(ctx context.Context, cmd *cli.Command) error {
	dir := cmd.String("dir")
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	shell := cmd.String("shell")
	if shell == "" {
		shell = cmp.Or(os.Getenv("SHELL"), "/bin/sh")
	}
	return a.with(ctx, cmd, func(v Visor) error {
		s, err := v.Start(ctx, agentvisor.StartSpec{Kind: agentvisor.KindPTY, Role: agentvisor.RoleShell, Command: []string{shell, "-l"}, WorkingDir: dir})
		if err != nil {
			return err
		}
		return a.attach(ctx, v, s.ID)
	})
}

// sessionHandover moves a session to the other backend: wire to pty, or pty
// to wire.
func (a *app) sessionHandover(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<session-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error {
		to := agentvisor.Kind(cmd.String("to"))
		if to == "" {
			s, err := v.Session(ctx, cmd.Args().First())
			if err != nil {
				return err
			}
			to = agentvisor.KindPTY
			if s.Kind == agentvisor.KindPTY {
				to = agentvisor.KindWire
			}
		}
		if err := v.Handover(ctx, cmd.Args().First(), to); err != nil {
			return err
		}
		return a.out(cmd).Line("%s: %s", cmd.Args().First(), to)
	})
}

// attach copies the terminal of the session to stdout and stdin to the
// session until the session ends or the user presses Ctrl-].
func (a *app) attach(ctx context.Context, v Visor, id string) error {
	in, _ := a.opts.Stdin.(*os.File)
	o := agentvisor.AttachOptions{View: agentvisor.ViewTerminal}
	if in != nil && term.IsTerminal(int(in.Fd())) {
		if cols, rows, err := term.GetSize(int(in.Fd())); err == nil {
			o.Cols, o.Rows = cols, rows
		}
		old, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return fmt.Errorf("attach: %w", err)
		}
		defer term.Restore(int(in.Fd()), old) //nolint:errcheck // the terminal is gone in any case
	}
	st, err := v.Attach(ctx, id, o)
	if err != nil {
		return err
	}
	defer st.Close()
	go a.copyInput(ctx, st)
	if in != nil && o.Cols > 0 {
		stop := a.watchSize(ctx, st, in)
		defer stop()
	}
	for {
		f, err := st.Recv(ctx)
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, agentvisor.ErrClosed):
			return nil
		case err != nil:
			return err
		}
		switch {
		case f.Binary():
			if _, err := a.opts.Stdout.Write(f.Data); err != nil {
				return err
			}
			_ = st.Handle(ctx, agentvisor.ClientFrame{Type: agentvisor.ClientAck, Seq: f.Seq}) // a lost ack only slows the output
		case f.Type == agentvisor.FrameError:
			_, _ = fmt.Fprintf(a.opts.Stderr, "\r\n%s\r\n", f.Message) // only a note
		case f.Type == agentvisor.FrameEnded:
			_, _ = fmt.Fprintf(a.opts.Stderr, "\r\n[%s]\r\n", f.Reason) // only a note
		}
	}
}

// copyInput sends stdin to the session. Ctrl-] closes the attachment.
func (a *app) copyInput(ctx context.Context, st *agentvisor.AttachStream) {
	buf := make([]byte, 4096)
	for {
		n, err := a.opts.Stdin.Read(buf)
		p := buf[:n]
		for i, b := range p {
			if b == detachKey {
				if i > 0 {
					_ = st.Input(ctx, p[:i]) // the attachment closes next
				}
				_ = st.Close() // Recv then ends
				return
			}
		}
		if n > 0 && st.Input(ctx, p) != nil {
			return
		}
		if err != nil {
			return
		}
	}
}

// watchSize sends the new size of the terminal after each SIGWINCH.
func (a *app) watchSize(ctx context.Context, st *agentvisor.AttachStream, in *os.File) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				if cols, rows, err := term.GetSize(int(in.Fd())); err == nil {
					_ = st.Handle(ctx, agentvisor.ClientFrame{Type: agentvisor.ClientControl, Cols: cols, Rows: rows}) // the next resize tries again
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
