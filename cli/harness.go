package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	agentvisor "github.com/trebi-ai/agent-visor"
)

func (a *app) harnessCommand() *cli.Command {
	name := "<harness>"
	version := &cli.StringFlag{Name: "version", Usage: "the version (default: the latest release)"}
	return &cli.Command{
		Name:  "harness",
		Usage: "Find, install, update, and log in to the agent CLIs",
		Commands: []*cli.Command{
			{Name: "ls", Usage: "List the harnesses", Action: a.harnessLs},
			{Name: "show", Usage: "Show one harness", ArgsUsage: name, Action: a.harnessShow},
			{Name: "models", Usage: "List the models of a harness", ArgsUsage: name, Action: a.harnessModels},
			{Name: "install", Usage: "Install a visor copy of a harness", ArgsUsage: name, Flags: []cli.Flag{version}, Action: a.harnessInstall},
			{Name: "update", Usage: "Update a harness", ArgsUsage: name, Flags: []cli.Flag{version}, Action: a.harnessUpdate},
			{
				Name: "auth", Usage: "Use the login of the user on this machine, or a separate login of the visor",
				ArgsUsage: fmt.Sprintf("%s %s|%s", name, agentvisor.AuthShared, agentvisor.AuthSeparate), Action: a.harnessAuth,
			},
			{
				Name: "updates", Usage: "Update a harness by itself, or keep one version",
				ArgsUsage: fmt.Sprintf("%s %s|%s", name, agentvisor.UpdatesAuto, agentvisor.UpdatesKeep), Flags: []cli.Flag{version}, Action: a.harnessUpdates,
			},
			{Name: "check", Usage: "Look for new releases now", Action: a.harnessCheck},
			{
				Name: "login", Usage: "Start the login of a harness and show the address or the code", ArgsUsage: name,
				Flags: []cli.Flag{&cli.StringFlag{Name: "provider", Usage: "the model provider of a harness with many providers"}}, Action: a.harnessLogin,
			},
			{Name: "code", Usage: "Give the code that the login page shows", ArgsUsage: name + " <code>", Action: a.harnessCode},
			{Name: "logout", Usage: "Log out of a harness", ArgsUsage: name, Action: a.harnessLogout},
			{Name: "files", Usage: "List the files that a harness can share with your own setup", ArgsUsage: name, Action: a.harnessFiles},
			{Name: "share", Usage: "Use your own file in a harness", ArgsUsage: name + " <key>", Action: a.harnessShare},
			{
				Name: "own", Usage: "Give a harness its own copy of a file", ArgsUsage: name + " <key>", Action: a.harnessOwn,
				Flags: []cli.Flag{&cli.StringFlag{Name: "from", Value: string(agentvisor.OwnFromUser), Usage: "the first content: user, empty, or backup"}},
			},
		},
	}
}

func (a *app) harnessLs(ctx context.Context, cmd *cli.Command) error {
	return a.with(ctx, cmd, func(h Visor) error {
		hs, err := h.Harnesses(ctx)
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(nonNil(hs))
		}
		t := p.Table("NAME", "VERSION", "LATEST", "SOURCE", "SUPPORTED", "AUTH MODE", "AUTH", "DETAIL")
		for _, x := range hs {
			t.Row(x.Name, x.Version, x.Latest, string(x.Source), strconv.FormatBool(x.Supported), string(x.AuthMode), string(x.Auth), x.Detail)
		}
		return t.Flush()
	})
}

func (a *app) harnessShow(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		x, err := h.Harness(ctx, cmd.Args().First())
		if err != nil {
			return err
		}
		return a.printHarness(cmd, x, true)
	})
}

func (a *app) harnessModels(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		ms, err := h.HarnessModels(ctx, cmd.Args().First())
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(nonNil(ms))
		}
		t := p.Table("ID", "NAME", "DEFAULT", "EFFORTS")
		for _, m := range ms {
			t.Row(m.ID, m.Name, strconv.FormatBool(m.Default), strings.Join(m.Efforts, " "))
		}
		return t.Flush()
	})
}

func (a *app) harnessInstall(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		x, err := h.InstallHarness(ctx, cmd.Args().First(), cmd.String("version"))
		if err != nil {
			return err
		}
		return a.printHarness(cmd, x, false)
	})
}

func (a *app) harnessUpdate(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		x, err := h.UpdateHarness(ctx, cmd.Args().First(), cmd.String("version"))
		if err != nil {
			return err
		}
		return a.printHarness(cmd, x, false)
	})
}

func (a *app) harnessAuth(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 2, "<harness> <mode>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		x, err := h.SetHarnessAuth(ctx, cmd.Args().Get(0), agentvisor.AuthMode(cmd.Args().Get(1)))
		if err != nil {
			return err
		}
		return a.printHarness(cmd, x, false)
	})
}

func (a *app) harnessUpdates(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 2, "<harness> <mode>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		x, err := h.SetHarnessUpdates(ctx, cmd.Args().Get(0), agentvisor.UpdateMode(cmd.Args().Get(1)), cmd.String("version"))
		if err != nil {
			return err
		}
		return a.printHarness(cmd, x, false)
	})
}

func (a *app) harnessCheck(ctx context.Context, cmd *cli.Command) error {
	return a.with(ctx, cmd, func(h Visor) error { return h.CheckHarnesses(ctx) })
}

func (a *app) harnessLogin(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		l, err := h.LoginHarness(ctx, cmd.Args().First(), agentvisor.LoginSpec{Provider: cmd.String("provider")})
		if err != nil {
			return err
		}
		return a.printLogin(cmd, l)
	})
}

func (a *app) harnessCode(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 2, "<harness> <code>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		l, err := h.HarnessLoginCode(ctx, cmd.Args().Get(0), cmd.Args().Get(1))
		if err != nil {
			return err
		}
		return a.printLogin(cmd, l)
	})
}

func (a *app) printLogin(cmd *cli.Command, l agentvisor.Login) error {
	p := a.out(cmd)
	if p.json {
		return p.JSON(l)
	}
	t := p.Table()
	t.Row("harness", l.Harness)
	if l.InApp {
		t.Row("login", "log in inside the terminal view of the harness")
	}
	for _, kv := range [][2]string{{"phase", string(l.Phase)}, {"open", l.URL}, {"code", l.Code}, {"session", l.Session}, {"hint", l.Hint}, {"error", l.Error}} {
		if kv[1] != "" {
			t.Row(kv[0], kv[1])
		}
	}
	return t.Flush()
}

func (a *app) harnessLogout(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error { return h.LogoutHarness(ctx, cmd.Args().First()) })
}

func (a *app) harnessFiles(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		fs, err := h.HarnessFiles(ctx, cmd.Args().First())
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(nonNil(fs))
		}
		t := p.Table("KEY", "KIND", "SHARED", "LOCKED", "USER PATH", "HOME PATH")
		for _, f := range fs {
			t.Row(f.Key, string(f.Kind), strconv.FormatBool(f.Shared), strconv.FormatBool(f.Locked), f.UserPath, f.HomePath)
		}
		return t.Flush()
	})
}

func (a *app) harnessShare(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 2, "<harness> <key>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		f, err := h.ShareHarnessFile(ctx, cmd.Args().Get(0), cmd.Args().Get(1))
		return a.printFile(cmd, f, err)
	})
}

func (a *app) harnessOwn(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 2, "<harness> <key>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(h Visor) error {
		f, err := h.OwnHarnessFile(ctx, cmd.Args().Get(0), cmd.Args().Get(1), agentvisor.OwnStart(cmd.String("from")))
		return a.printFile(cmd, f, err)
	})
}

func (a *app) printFile(cmd *cli.Command, f agentvisor.HomeFile, err error) error {
	if err != nil {
		return err
	}
	p := a.out(cmd)
	if p.json {
		return p.JSON(f)
	}
	state := "own copy"
	if f.Shared {
		state = "shared"
	}
	return p.Line("%s %s", f.Key, state)
}

// printHarness prints one harness: all fields, or one summary line.
func (a *app) printHarness(cmd *cli.Command, x agentvisor.Harness, full bool) error {
	p := a.out(cmd)
	if p.json {
		return p.JSON(x)
	}
	if !full {
		return p.Line("%s %s (%s, auth %s %s)", x.Name, x.Version, x.Source, x.AuthMode, x.Auth)
	}
	t := p.Table()
	for _, kv := range [][2]string{
		{"name", x.Name}, {"installed", strconv.FormatBool(x.Installed)}, {"version", x.Version}, {"latest", x.Latest},
		{"path", x.Path}, {"source", string(x.Source)}, {"installs", strings.Join(x.Installs, " ")},
		{"installable", strconv.FormatBool(x.Installable)}, {"supported", strconv.FormatBool(x.Supported)},
		{"detail", x.Detail}, {"auth mode", string(x.AuthMode)}, {"auth", string(x.Auth)}, {"env keys", strings.Join(x.EnvKeys, " ")},
		{"updates", string(x.Updates)}, {"pending", strconv.FormatBool(x.Pending)},
	} {
		t.Row(kv[0], kv[1])
	}
	for _, u := range x.History {
		res := "ok"
		if !u.OK {
			res = "failed: " + u.Error
		}
		t.Row("update", fmt.Sprintf("%s %s -> %s %s", u.At.Format(time.RFC3339), u.From, u.To, res))
	}
	for _, m := range x.Models {
		t.Row("model", m.ID)
	}
	return t.Flush()
}
