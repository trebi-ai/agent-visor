# agent-visor

agent-visor keeps agent and terminal sessions alive and lets you attach to them. It does not sandbox them.

It is a Go library and a process. It sits one layer above [agent-wire](https://github.com/trebi-ai/agent-wire). agent-wire drives one vendor CLI over its structured protocol and keeps no state. agent-visor holds many sessions on one machine, keeps their events in a log, and survives a restart of the product that uses it.

Status: early. The library core works in the process. The socket, the CLI, and the holders come next.

## What it does

- Starts agent sessions over agent-wire, vendor TUIs in a PTY, shells, and scripts.
- Keeps each session in a holder process, so a session survives a visor upgrade.
- Keeps an event log with a sequence number. A client that connects again reads the events that it missed.
- Lets a client attach to a session as a terminal or as a chat view, and hand a session over between the two.
- Finds projects from paths and manages workdirs: the main checkout, git worktrees, and plain folders.
- Detects, installs, updates, and logs in to the harnesses (Claude Code, Codex, and others), and lists their models.
- Takes hook calls from sessions that a user starts outside the visor, and shows them as observed sessions. A hook call can carry labels, which join it to a session of a backend.
- Takes optional backends. `backend/orca` runs sessions in the terminals of the Orca app.
- Upgrades itself when no session needs it.

## How a product uses it

Each product runs its own visor. Each visor has its own home, socket, harness installs, and logins. The product talks to the visor over a Unix socket with `Dial`, or it runs a `Visor` in its own process for tests.

```go
c, err := agentvisor.Ensure(ctx, svc) // install or start the service, then dial
s, err := c.Start(ctx, agentvisor.StartSpec{Harness: "claude", WorkingDir: dir, Prompt: "..."})
stream, err := c.Events(ctx, agentvisor.Filter{Session: s.ID}, 0)
```

The generic CLI `agent-visor` uses the same API.

## Optional backends

A product registers an optional backend in `Options.Backends`. A backend can also find sessions that it did not start (`Discoverer`), list folders that become projects (`Catalog`), give labels to its sessions (`Labeler`), and give a text terminal (`TextTerminal`).

`backend/orca` drives the `orca` CLI of the Orca app:

- `Start` opens a terminal in an Orca workspace and runs the agent command in it. The env goes in a start script with mode 0700, which removes itself. The label `orca.worktree` picks the workspace, and the label `orca.shell` types the command into a shell that is open.
- The backend polls the agent cards of Orca. A card gives the state, the tool, the prompt, and the permission requests of the session.
- `Answer` reads the prompt on the screen and types the choice. `Screen`, `Keys`, and `Paste` use the Orca terminal.
- A visor restart keeps the sessions: the bindings stay in `Options.Dir`.
- With `Discover`, the backend reports the agent panes that it did not start, and the visor adopts them.
- A hook call from an Orca pane joins its session when the hook sends `orca.HookLabels(os.Getenv)`.

The live test runs a script in a new Orca terminal and touches only that terminal:

```bash
ORCA_LIVE_DIR=$HOME/src/app go test -tags live -run TestLive ./backend/orca/
```

## CLI

`agent-visor` runs the visor and drives it. The default home is `~/.agent-visor`; `AGENT_VISOR_HOME` and `--home` change it. The socket is `<home>/visor.sock` unless `--socket` gives another path. `--json` prints JSON.

```bash
agent-visor serve &                      # run the visor in the foreground
agent-visor status
agent-visor ls                           # projects, workdirs, and sessions as a tree
agent-visor session start --harness claude --dir . --prompt "Fix the tests" --label app.task=t1
agent-visor session logs <id> --follow   # seq, type, and a short text for each event
agent-visor session answer <id> <request-id> --allow
agent-visor session stop <id>
agent-visor harness ls
```

A product mounts the same command tree under its own binary, with its own home and socket:

```go
root.Commands = append(root.Commands, cli.Command(cli.Options{
	Name: "visor", Home: home, Build: version,
	Configure: func(ctx context.Context, o *agentvisor.Options) error { o.Env = env; return nil },
}))
```

## Platforms

macOS and Linux. Windows is a later step.

## License

MIT
