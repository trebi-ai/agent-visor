package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// statusView is the output of status.
type statusView struct {
	Running  bool                     `json:"running"`
	Socket   string                   `json:"socket"`
	Info     *agentvisor.Info         `json:"info,omitempty"`
	Sessions map[agentvisor.State]int `json:"sessions,omitempty"`
	Error    string                   `json:"error,omitempty"`
}

func (a *app) serveCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Run the visor in the foreground until SIGINT or SIGTERM",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "log-level", Usage: "debug, info, warn, or error", Value: "info"},
		},
		Action: a.serve,
	}
}

func (a *app) statusCommand() *cli.Command {
	return &cli.Command{Name: "status", Usage: "Show if the visor runs, and its sessions", Action: a.status}
}

func (a *app) versionCommand() *cli.Command {
	return &cli.Command{Name: "version", Usage: "Show the build and the protocol", Action: a.version}
}

func (a *app) upgradeCommand() *cli.Command {
	return &cli.Command{Name: "upgrade", Usage: "Replace the running visor with this build when no session needs it", Action: a.notAvailable}
}

func (a *app) hookCommand() *cli.Command {
	return &cli.Command{Name: "hook", Usage: "Send a hook call of a session to the visor", Action: a.notAvailable}
}

func (a *app) holdCommand() *cli.Command {
	return &cli.Command{Name: "hold", Usage: "Hold the terminal of one session (the visor starts it)", Hidden: true, Action: a.notAvailable}
}

// serve opens the socket first, so a second serve fails before it opens the
// store of a running visor.
func (a *app) serve(ctx context.Context, cmd *cli.Command) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	home := cmd.String("home")
	if home == "" {
		return fmt.Errorf("%w: serve needs --home", agentvisor.ErrInvalid)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cmd.String("log-level"))); err != nil {
		return fmt.Errorf("%w: --log-level: %w", agentvisor.ErrInvalid, err)
	}
	log := slog.New(slog.NewTextHandler(a.opts.Stderr, &slog.HandlerOptions{Level: level}))
	sock := a.socket(cmd)
	o := agentvisor.Options{Home: home, Build: a.opts.Build, Socket: sock, Logger: log}
	if a.opts.Configure != nil {
		if err := a.opts.Configure(ctx, &o); err != nil {
			return fmt.Errorf("configure the visor: %w", err)
		}
	}
	ln, err := agentvisor.Listen(ctx, sock)
	if err != nil {
		return err
	}
	v, err := agentvisor.New(ctx, o)
	if err != nil {
		_ = ln.Close() // the open error is the one to report
		return err
	}
	srv := &http.Server{Handler: agentvisor.NewServer(v), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // the connections close in any case
	}()
	log.Info("the visor serves", "socket", sock, "home", home)
	serr := srv.Serve(ln)
	if errors.Is(serr, http.ErrServerClosed) {
		serr = nil
	}
	return errors.Join(serr, v.Close(context.WithoutCancel(ctx)))
}

func (a *app) status(ctx context.Context, cmd *cli.Command) error {
	p := a.out(cmd)
	view := statusView{Socket: a.socket(cmd)}
	err := a.with(ctx, cmd, func(v Visor) error {
		info, err := v.Info(ctx)
		if err != nil {
			return err
		}
		all, err := v.Sessions(ctx, agentvisor.Filter{})
		if err != nil {
			return err
		}
		view.Running, view.Info, view.Sessions = true, &info, map[agentvisor.State]int{}
		for _, s := range all {
			view.Sessions[s.State]++
		}
		return nil
	})
	if err != nil {
		view.Error = err.Error()
		if p.json {
			return errors.Join(p.JSON(view), err)
		}
		return err
	}
	if p.json {
		return p.JSON(view)
	}
	counts := []string{}
	for _, st := range slices.Sorted(maps.Keys(view.Sessions)) {
		counts = append(counts, fmt.Sprintf("%d %s", view.Sessions[st], st))
	}
	t := p.Table()
	t.Row("build", view.Info.Build)
	t.Row("protocol", view.Info.Protocol)
	t.Row("home", view.Info.Home)
	t.Row("socket", view.Socket)
	t.Row("pid", fmt.Sprint(view.Info.PID))
	t.Row("seq", fmt.Sprint(view.Info.Seq))
	t.Row("sessions", strings.Join(counts, ", "))
	return t.Flush()
}

func (a *app) version(_ context.Context, cmd *cli.Command) error {
	p := a.out(cmd)
	build := cmp.Or(a.opts.Build, "dev")
	if p.json {
		return p.JSON(map[string]string{"build": build, "protocol": agentvisor.Protocol})
	}
	return p.Line("%s %s (%s)", a.opts.Name, build, agentvisor.Protocol)
}
