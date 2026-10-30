// Command agent-visor is the generic visor CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/agent-visor/cli"
)

// envHome overrides the default home.
const envHome = "AGENT_VISOR_HOME"

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	home := os.Getenv(envHome)
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".agent-visor")
		}
	}
	cmd := cli.Command(cli.Options{
		Name:   "agent-visor",
		Home:   home,
		Socket: os.Getenv(agentvisor.EnvSocket),
		Build:  build(),
	})
	err := cmd.Run(ctx, os.Args)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-visor:", err)
		os.Exit(1)
	}
}

// build is the -X version, else the module version, else empty.
func build() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return ""
}
