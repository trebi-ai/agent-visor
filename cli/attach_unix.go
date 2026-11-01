//go:build !windows

package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	agentvisor "github.com/trebi-ai/agent-visor"
)

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
