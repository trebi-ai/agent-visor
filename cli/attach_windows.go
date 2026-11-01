package cli

import (
	"context"
	"os"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// watchSize does nothing: Windows has no SIGWINCH.
func (a *app) watchSize(context.Context, *agentvisor.AttachStream, *os.File) func() { return func() {} }
