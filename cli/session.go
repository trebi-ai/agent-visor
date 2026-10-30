package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// defaultResume is the turn that resume sends when it gets no prompt.
const defaultResume = "Continue."

func (a *app) sessionCommand() *cli.Command {
	id := "<session-id>"
	return &cli.Command{
		Name:  "session",
		Usage: "Start, drive, and stop sessions",
		Commands: []*cli.Command{
			{
				Name: "ls", Usage: "List the sessions", Action: a.sessionLs,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "project", Usage: "only this project id"},
					&cli.StringFlag{Name: "workdir", Usage: "only this workdir id"},
					&cli.StringFlag{Name: "kind", Usage: "only this kind"},
					&cli.StringFlag{Name: "role", Usage: "only this role"},
					&cli.StringSliceFlag{Name: "state", Usage: "only this state (repeat)"},
					&cli.StringSliceFlag{Name: "label", Usage: "only sessions with the label k=v (repeat)"},
					&cli.BoolFlag{Name: "all", Usage: "also show exited sessions"},
				},
			},
			{Name: "start", Usage: "Start a session", Action: a.sessionStart, Flags: startFlags()},
			{
				Name: "shell", Usage: "Start a shell in a terminal and attach to it (Ctrl-] detaches)", Action: a.sessionShell,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "dir", Usage: "the working folder (default: the current folder)"},
					&cli.StringFlag{Name: "shell", Usage: "the shell program (default: $SHELL)"},
				},
			},
			{Name: "show", Usage: "Show one session", ArgsUsage: id, Action: a.sessionShow},
			{
				Name: "logs", Usage: "Print the events of a session", ArgsUsage: id, Action: a.sessionLogs,
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "keep printing new events until the session exits"},
					&cli.Uint64Flag{Name: "after", Usage: "only the events after this seq"},
				},
			},
			{Name: "attach", Usage: "Attach to the terminal of a session (Ctrl-] detaches)", ArgsUsage: id, Action: a.sessionAttach},
			{Name: "send", Usage: "Send a turn (\"-\" reads it from stdin)", ArgsUsage: id + " <text>...", Action: a.sessionSend},
			{
				Name: "answer", Usage: "Answer an open permission or question", ArgsUsage: id + " <request-id>", Action: a.sessionAnswer,
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "allow", Usage: "allow the request"},
					&cli.BoolFlag{Name: "deny", Usage: "deny the request"},
					&cli.StringFlag{Name: "message", Usage: "a message to the agent"},
				},
			},
			{Name: "interrupt", Usage: "Stop the current turn", ArgsUsage: id, Action: a.sessionInterrupt},
			{Name: "stop", Usage: "End a session", ArgsUsage: id, Action: a.sessionStop},
			{Name: "resume", Usage: "Resume a suspended or lost session (default prompt: " + strconv.Quote(defaultResume) + ")", ArgsUsage: id + " [prompt]", Action: a.sessionResume},
			{
				Name: "handover", Usage: "Move a session between the terminal and the chat view", ArgsUsage: id, Action: a.sessionHandover,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "to", Usage: fmt.Sprintf("%s or %s (default: the other one)", agentvisor.KindPTY, agentvisor.KindWire)},
				},
			},
		},
	}
}

func startFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "harness", Usage: "the agent CLI, for example claude"},
		&cli.StringFlag{Name: "model", Usage: "the model"},
		&cli.StringFlag{Name: "dir", Usage: "the working folder (default: the current folder)"},
		&cli.StringFlag{Name: "prompt", Usage: "the first turn"},
		&cli.StringFlag{Name: "title", Usage: "the title"},
		&cli.StringFlag{Name: "kind", Usage: fmt.Sprintf("%s or %s (default: %s with a harness)", agentvisor.KindWire, agentvisor.KindPTY, agentvisor.KindWire)},
		&cli.StringFlag{Name: "role", Usage: fmt.Sprintf("%s, %s, or %s", agentvisor.RoleAgent, agentvisor.RoleShell, agentvisor.RoleScript)},
		&cli.StringFlag{Name: "key", Usage: "the workdir key: find or make the workdir with this key"},
		&cli.StringSliceFlag{Name: "label", Usage: "a label k=v (repeat)"},
		&cli.StringSliceFlag{Name: "env", Usage: "an env var K=V for the session (repeat)"},
	}
}

func (a *app) sessionLs(ctx context.Context, cmd *cli.Command) error {
	labels, err := parseLabels(cmd.StringSlice("label"))
	if err != nil {
		return err
	}
	f := agentvisor.Filter{
		Project: cmd.String("project"), Workdir: cmd.String("workdir"), Labels: labels,
		Kind: agentvisor.Kind(cmd.String("kind")), Role: agentvisor.Role(cmd.String("role")),
	}
	for _, st := range cmd.StringSlice("state") {
		f.States = append(f.States, agentvisor.State(st))
	}
	if len(f.States) == 0 && !cmd.Bool("all") {
		f.States = liveStates()
	}
	return a.with(ctx, cmd, func(v Visor) error {
		ss, err := v.Sessions(ctx, f)
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(nonNil(ss))
		}
		t := p.Table("ID", "STATE", "ROLE", "KIND", "HARNESS", "MODEL", "TURNS", "WORKDIR", "TITLE")
		for _, s := range ss {
			t.Row(s.ID, string(s.State), string(s.Role), string(s.Kind), s.Harness, s.Model, strconv.Itoa(s.Turns), s.Workdir, s.Title)
		}
		return t.Flush()
	})
}

func (a *app) sessionStart(ctx context.Context, cmd *cli.Command) error {
	labels, err := parseLabels(cmd.StringSlice("label"))
	if err != nil {
		return err
	}
	env, err := parseEnv(cmd.StringSlice("env"))
	if err != nil {
		return err
	}
	// The visor runs in another folder, so send an absolute path.
	dir := cmd.String("dir")
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	spec := agentvisor.StartSpec{
		Kind: agentvisor.Kind(cmd.String("kind")), Role: agentvisor.Role(cmd.String("role")), Title: cmd.String("title"),
		Harness: cmd.String("harness"), Model: cmd.String("model"), WorkingDir: dir, Prompt: cmd.String("prompt"),
		Labels: labels, Env: env,
	}
	return a.with(ctx, cmd, func(v Visor) error {
		if key := cmd.String("key"); key != "" {
			w, err := v.EnsureWorkdir(ctx, agentvisor.WorkdirSpec{Path: dir, Key: key})
			if err != nil {
				return err
			}
			spec.Workdir, spec.WorkingDir = w.ID, ""
		}
		s, err := v.Start(ctx, spec)
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(s)
		}
		return p.Line("%s", s.ID)
	})
}

func (a *app) sessionShow(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<session-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error {
		s, err := v.Session(ctx, cmd.Args().First())
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(s)
		}
		t := p.Table()
		for _, kv := range [][2]string{
			{"id", s.ID}, {"state", string(s.State)}, {"role", string(s.Role)}, {"kind", string(s.Kind)},
			{"title", s.Title}, {"harness", s.Harness}, {"model", s.Model}, {"project", s.Project}, {"workdir", s.Workdir},
			{"dir", s.WorkingDir}, {"vendor id", s.VendorID}, {"pid", strconv.Itoa(s.PID)}, {"turns", strconv.Itoa(s.Turns)},
			{"seq", strconv.FormatUint(s.Seq, 10)}, {"restart", string(s.Restart.Mode)}, {"labels", labelText(s.Labels)},
			{"env keys", strings.Join(s.EnvKeys, " ")}, {"error", s.Error}, {"created", stamp(s.CreatedAt)}, {"updated", stamp(s.UpdatedAt)},
		} {
			t.Row(kv[0], kv[1])
		}
		for _, r := range s.Pending {
			t.Row("pending", requestText(r))
		}
		return t.Flush()
	})
}

// sessionLogs prints the events after --after. Without --follow, it stops at
// the last event of the session at the time of the call.
func (a *app) sessionLogs(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<session-id>"); err != nil {
		return err
	}
	follow, after := cmd.Bool("follow"), cmd.Uint64("after")
	return a.with(ctx, cmd, func(v Visor) error {
		s, err := v.Session(ctx, cmd.Args().First())
		if err != nil {
			return err
		}
		var until uint64
		if !follow {
			if s.Seq <= after {
				return nil
			}
			until = s.Seq
		}
		st, err := v.Events(ctx, agentvisor.Filter{Session: s.ID}, after)
		if err != nil {
			return err
		}
		defer st.Close()
		return a.out(cmd).events(ctx, st, until)
	})
}

func (a *app) sessionSend(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() < 2 {
		return fmt.Errorf("%w: %s needs <session-id> <text>", agentvisor.ErrInvalid, cmd.FullName())
	}
	text := strings.Join(cmd.Args().Tail(), " ")
	if text == "-" {
		data, err := io.ReadAll(a.opts.Stdin)
		if err != nil {
			return fmt.Errorf("read the turn: %w", err)
		}
		text = string(data)
	}
	return a.with(ctx, cmd, func(v Visor) error {
		return v.Send(ctx, cmd.Args().First(), agentvisor.Input{Text: text})
	})
}

func (a *app) sessionAnswer(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 2, "<session-id> <request-id>"); err != nil {
		return err
	}
	if cmd.Bool("allow") == cmd.Bool("deny") {
		return fmt.Errorf("%w: give --allow or --deny", agentvisor.ErrInvalid)
	}
	ans := agentvisor.Answer{Allow: cmd.Bool("allow"), Message: cmd.String("message")}
	return a.with(ctx, cmd, func(v Visor) error {
		return v.Answer(ctx, cmd.Args().Get(0), cmd.Args().Get(1), ans)
	})
}

func (a *app) sessionInterrupt(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<session-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error { return v.Interrupt(ctx, cmd.Args().First()) })
}

func (a *app) sessionStop(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<session-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error { return v.Stop(ctx, cmd.Args().First()) })
}

func (a *app) sessionResume(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() < 1 {
		return fmt.Errorf("%w: %s needs <session-id>", agentvisor.ErrInvalid, cmd.FullName())
	}
	prompt := cmp.Or(strings.Join(cmd.Args().Tail(), " "), defaultResume)
	return a.with(ctx, cmd, func(v Visor) error {
		s, err := v.Session(ctx, cmd.Args().First())
		if err != nil {
			return err
		}
		if s.State != agentvisor.StateSuspended && s.State != agentvisor.StateLost {
			return fmt.Errorf("%w: session %s is %s; only a suspended or lost session resumes", agentvisor.ErrState, s.ID, s.State)
		}
		return v.Send(ctx, s.ID, agentvisor.Input{Text: prompt})
	})
}

// events prints the events of st up to the seq until. With until 0, it
// prints until the session exits or ctx ends.
func (p printer) events(ctx context.Context, st *agentvisor.EventStream, until uint64) error {
	for {
		e, err := st.Next(ctx)
		if err != nil {
			if until == 0 && ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := p.event(e); err != nil {
			return err
		}
		if until > 0 && e.Seq >= until {
			return nil
		}
		if until == 0 && e.Type == agentvisor.EventSessionState && e.State == agentvisor.StateExited {
			return nil
		}
	}
}

// event prints one event: "seq type text", or one JSON line.
func (p printer) event(e agentvisor.Event) error {
	if p.json {
		return json.NewEncoder(p.w).Encode(e)
	}
	return p.Line("%6d  %-15s %s", e.Seq, e.Type, short(eventText(e), 120))
}

// liveStates are the states that ls shows without --all.
func liveStates() []agentvisor.State {
	return []agentvisor.State{
		agentvisor.StateStarting, agentvisor.StateRunning, agentvisor.StateWaiting, agentvisor.StateIdle,
		agentvisor.StateSuspended, agentvisor.StateLost,
	}
}

// eventText is the short text of an event.
func eventText(e agentvisor.Event) string {
	switch {
	case e.Type == agentvisor.EventSessionState:
		return string(e.State)
	case e.Type == agentvisor.EventInit:
		return e.VendorID
	case e.Type == agentvisor.EventRequestClosed:
		return e.RequestID
	case e.Type == agentvisor.EventHook, e.Type == agentvisor.EventScript:
		return string(e.Data)
	case e.Tool != nil:
		return strings.Join(slices.DeleteFunc([]string{e.Tool.Name, e.Tool.Status, e.Tool.Input}, isEmpty), " ")
	case e.Request != nil:
		return requestText(*e.Request)
	case e.Result != nil:
		return strings.Join(slices.DeleteFunc([]string{e.Result.Subtype, e.Result.Text}, isEmpty), " ")
	case e.Usage != nil:
		return fmt.Sprintf("in %d out %d", e.Usage.Input, e.Usage.Output)
	case e.ExitCode != nil:
		return strings.TrimSpace(fmt.Sprintf("code %d %s", *e.ExitCode, e.Error))
	}
	return cmp.Or(e.Text, e.Status, e.Error)
}

func requestText(r agentvisor.Request) string {
	return strings.Join(slices.DeleteFunc([]string{r.ID, r.Tool, r.Question}, isEmpty), " ")
}

func labelText(labels map[string]string) string {
	out := []string{}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		out = append(out, k+"="+labels[k])
	}
	return strings.Join(out, " ")
}

func parseLabels(in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, l := range in {
		k, v, err := agentvisor.ParseLabel(l)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// parseEnv reads K=V pairs. An error never holds the value.
func parseEnv(in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for i, kv := range in {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%w: --env number %d is not K=V", agentvisor.ErrInvalid, i+1)
		}
		out[k] = v
	}
	return out, nil
}

func isEmpty(s string) bool { return s == "" }
