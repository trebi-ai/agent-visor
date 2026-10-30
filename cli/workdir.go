package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/urfave/cli/v3"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// projectTree is one project of ls with its workdirs.
type projectTree struct {
	agentvisor.Project
	Workdirs []workdirTree `json:"workdirs"`
}

// workdirTree is one workdir of ls with its sessions.
type workdirTree struct {
	agentvisor.Workdir
	Sessions []agentvisor.Session `json:"sessions"`
}

// projectView is the output of project show.
type projectView struct {
	agentvisor.Project
	Workdirs []agentvisor.Workdir `json:"workdirs"`
}

func (a *app) lsCommand() *cli.Command {
	return &cli.Command{
		Name:  "ls",
		Usage: "Show the projects, their workdirs, and their sessions as a tree",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "project", Usage: "only this project id"},
			&cli.BoolFlag{Name: "all", Usage: "also show exited sessions and archived workdirs"},
		},
		Action: a.ls,
	}
}

func (a *app) projectCommand() *cli.Command {
	return &cli.Command{
		Name:  "project",
		Usage: "Show and forget projects",
		Commands: []*cli.Command{
			{Name: "ls", Usage: "List the projects", Action: a.projectLs},
			{Name: "show", Usage: "Show one project and its workdirs", ArgsUsage: "<project-id>", Action: a.projectShow},
			{Name: "forget", Usage: "Forget a project with no workdirs", ArgsUsage: "<project-id>", Action: a.projectForget},
		},
	}
}

func (a *app) workdirCommand() *cli.Command {
	return &cli.Command{
		Name:  "workdir",
		Usage: "Manage the workdirs: main checkouts, worktrees, and folders",
		Commands: []*cli.Command{
			{
				Name: "ls", Usage: "List the workdirs", Action: a.workdirLs,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "project", Usage: "only this project id"},
					&cli.StringFlag{Name: "key", Usage: "only the workdir with this key"},
				},
			},
			{
				Name: "ensure", Usage: "Find the workdir for a path, or make it", ArgsUsage: "[path]", Action: a.workdirEnsure,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "key", Usage: "a key that gives the same workdir on each call"},
					&cli.StringFlag{Name: "kind", Usage: fmt.Sprintf("%s, %s, or %s", agentvisor.WorkdirMain, agentvisor.WorkdirWorktree, agentvisor.WorkdirFolder)},
					&cli.StringFlag{Name: "branch", Usage: "the branch of a worktree"},
					&cli.StringFlag{Name: "base", Usage: "the base of a new worktree branch"},
					&cli.StringSliceFlag{Name: "label", Usage: "a label k=v (repeat)"},
				},
			},
			{Name: "archive", Usage: "Stop the sessions of a workdir and archive it", ArgsUsage: "<workdir-id>", Action: a.workdirArchive},
			{Name: "restore", Usage: "Make an archived workdir active again", ArgsUsage: "<workdir-id>", Action: a.workdirRestore},
			{
				Name: "rm", Usage: "Remove a workdir", ArgsUsage: "<workdir-id>", Action: a.workdirRm,
				Flags: []cli.Flag{&cli.BoolFlag{Name: "force", Usage: "also stop live sessions and drop changes"}},
			},
		},
	}
}

func (a *app) ls(ctx context.Context, cmd *cli.Command) error {
	return a.with(ctx, cmd, func(v Visor) error {
		f := agentvisor.Filter{Project: cmd.String("project")}
		projects, err := v.Projects(ctx, f)
		if err != nil {
			return err
		}
		workdirs, err := v.Workdirs(ctx, f)
		if err != nil {
			return err
		}
		if !cmd.Bool("all") {
			f.States = liveStates()
		}
		sessions, err := v.Sessions(ctx, f)
		if err != nil {
			return err
		}
		tree := buildTree(projects, workdirs, sessions, cmd.Bool("all"), cmd.String("project"))
		p := a.out(cmd)
		if p.json {
			return p.JSON(tree)
		}
		t := p.Table("ID", "STATE", "KIND", "NAME", "DETAIL")
		for _, pt := range tree {
			kind := "folder"
			if pt.Git {
				kind = "git"
			}
			t.Row(pt.ID, "", kind, pt.Name, pt.Path)
			for _, wt := range pt.Workdirs {
				t.Row("  "+wt.ID, string(wt.State), string(wt.Kind), wt.Branch, wt.Path)
				for _, s := range wt.Sessions {
					t.Row("    "+s.ID, string(s.State), string(s.Role), s.Harness, s.Title)
				}
			}
		}
		return t.Flush()
	})
}

func (a *app) projectLs(ctx context.Context, cmd *cli.Command) error {
	return a.with(ctx, cmd, func(v Visor) error {
		projects, err := v.Projects(ctx, agentvisor.Filter{})
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(projects)
		}
		t := p.Table("ID", "NAME", "GIT", "PATH")
		for _, pr := range projects {
			t.Row(pr.ID, pr.Name, strconv.FormatBool(pr.Git), pr.Path)
		}
		return t.Flush()
	})
}

func (a *app) projectShow(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<project-id>"); err != nil {
		return err
	}
	id := cmd.Args().First()
	return a.with(ctx, cmd, func(v Visor) error {
		projects, err := v.Projects(ctx, agentvisor.Filter{Project: id})
		if err != nil {
			return err
		}
		var view projectView
		for _, pr := range projects {
			if pr.ID == id {
				view.Project = pr
			}
		}
		if view.ID == "" {
			return fmt.Errorf("project %s: %w", id, agentvisor.ErrNotFound)
		}
		if view.Workdirs, err = v.Workdirs(ctx, agentvisor.Filter{Project: id}); err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(view)
		}
		t := p.Table()
		t.Row("id", view.ID)
		t.Row("name", view.Name)
		t.Row("path", view.Path)
		t.Row("git", strconv.FormatBool(view.Git))
		t.Row("created", stamp(view.CreatedAt))
		if err := t.Flush(); err != nil {
			return err
		}
		if err := p.Line(""); err != nil {
			return err
		}
		return a.workdirTable(p, view.Workdirs)
	})
}

func (a *app) projectForget(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<project-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error { return v.ForgetProject(ctx, cmd.Args().First()) })
}

func (a *app) workdirLs(ctx context.Context, cmd *cli.Command) error {
	return a.with(ctx, cmd, func(v Visor) error {
		ws, err := v.Workdirs(ctx, agentvisor.Filter{Project: cmd.String("project"), Key: cmd.String("key")})
		if err != nil {
			return err
		}
		p := a.out(cmd)
		if p.json {
			return p.JSON(ws)
		}
		return a.workdirTable(p, ws)
	})
}

func (a *app) workdirEnsure(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 1 {
		return fmt.Errorf("%w: %s takes one path", agentvisor.ErrInvalid, cmd.FullName())
	}
	path := cmd.Args().First()
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		path = wd
	}
	labels, err := parseLabels(cmd.StringSlice("label"))
	if err != nil {
		return err
	}
	spec := agentvisor.WorkdirSpec{
		Path: path, Key: cmd.String("key"), Kind: agentvisor.WorkdirKind(cmd.String("kind")),
		Branch: cmd.String("branch"), Base: cmd.String("base"), Labels: labels,
	}
	return a.with(ctx, cmd, func(v Visor) error {
		w, err := v.EnsureWorkdir(ctx, spec)
		if err != nil {
			return err
		}
		return a.printWorkdir(cmd, w)
	})
}

func (a *app) workdirArchive(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<workdir-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error { return v.ArchiveWorkdir(ctx, cmd.Args().First()) })
}

func (a *app) workdirRestore(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<workdir-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error {
		w, err := v.RestoreWorkdir(ctx, cmd.Args().First())
		if err != nil {
			return err
		}
		return a.printWorkdir(cmd, w)
	})
}

func (a *app) workdirRm(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<workdir-id>"); err != nil {
		return err
	}
	return a.with(ctx, cmd, func(v Visor) error { return v.RemoveWorkdir(ctx, cmd.Args().First(), cmd.Bool("force")) })
}

// printWorkdir prints one workdir: its id, or JSON.
func (a *app) printWorkdir(cmd *cli.Command, w agentvisor.Workdir) error {
	p := a.out(cmd)
	if p.json {
		return p.JSON(w)
	}
	return p.Line("%s", w.ID)
}

func (a *app) workdirTable(p printer, ws []agentvisor.Workdir) error {
	t := p.Table("ID", "PROJECT", "KIND", "STATE", "BRANCH", "KEY", "PORT", "PATH")
	for _, w := range ws {
		t.Row(w.ID, w.Project, string(w.Kind), string(w.State), w.Branch, w.Key, strconv.Itoa(w.PortBase), w.Path)
	}
	return t.Flush()
}

// buildTree puts the workdirs under their projects and the sessions under
// their workdirs. Without all, it leaves out archived workdirs that have no
// sessions.
func buildTree(projects []agentvisor.Project, workdirs []agentvisor.Workdir, sessions []agentvisor.Session, all bool, project string) []projectTree {
	bySession := map[string][]agentvisor.Session{}
	for _, s := range sessions {
		bySession[s.Workdir] = append(bySession[s.Workdir], s)
	}
	byProject := map[string][]workdirTree{}
	for _, w := range workdirs {
		ss := bySession[w.ID]
		if !all && w.State == agentvisor.WorkdirArchived && len(ss) == 0 {
			continue
		}
		byProject[w.Project] = append(byProject[w.Project], workdirTree{Workdir: w, Sessions: nonNil(ss)})
	}
	out := []projectTree{}
	for _, p := range projects {
		if project != "" && p.ID != project {
			continue
		}
		out = append(out, projectTree{Project: p, Workdirs: nonNil(byProject[p.ID])})
	}
	return out
}

// nonNil makes JSON print [] for an empty list.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
