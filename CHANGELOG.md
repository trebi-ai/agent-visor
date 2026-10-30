# Changelog

## Unreleased

- Add the library core: `Visor`, `StartSpec`, `Session`, `Event`, `Filter`, and the `Backend` and `Handle` interfaces.
- Add the built-in `wire` backend over agent-wire.
- Add the event log in SQLite with a global sequence. A replay after a sequence has no gaps and no duplicates.
- Add the session states, the restart policy, suspend at close, and lost at boot.
- Add the `workdir` package: projects from paths, the main checkout, git worktrees by key, plain folders, setup and teardown scripts, and port ranges.
- Keep the start spec with env in `sessions/<id>/spec.json` with mode 0600. The API returns only the env keys.
- Add the socket server (`Serve`, `Listen`, `NewServer`) and `Client` (`Dial`). `Client` has the same method set as `Visor`, and its event stream connects again after a drop and continues after the last seq.
- Add `Hello` and `ErrProtocol`. `Dial` refuses a visor with another protocol major.
- Add the `contract/` folder: the routes, the JSON schema, and fixtures, checked by `TestContract`.
- Add the `service` package: install the visor as a LaunchAgent or a systemd user unit, and `Ensure`.
- Add JSON types for permissions, MCP servers, skills, and attachments in `StartSpec` and `Input`.
- Add the `harness` package and the `Visor` harness methods: `Harnesses`, `Harness`, `InstallHarness`, `UpdateHarness`, `SetHarnessAuth`, `LoginHarness`, `LogoutHarness`, `HarnessModels`, and `SetHarnessEnv`.
- Install a visor copy of a harness from the agent-wire recipe in `bin/<harness>/<version>/` and move the `current` link. A failed install keeps the current version. An install or an update gives `ErrBusy` while a live session uses the harness.
- Add the auth modes `shared` (the user login) and `separate` (`harness/<harness>/` in the visor home), and a login in a pipe that gives the URL or the device code.
- Keep the harness env in `harness/<harness>.env.json` with mode 0600. A session gets the env layers visor, harness, and session. The API returns only the env keys.
- Set `Launch.Binary`, `Launch.Home`, and the harness env when a session starts. Emit `harness.changed` on each harness change.
- Move `Limits` and `LimitWindow` to the `harness` package. The root types are aliases.
- Add the `cli` package: `Command(Options)` gives the command tree, so a product mounts it under its own binary with its own home. It talks to the visor through the `cli.Visor` interface, and `Options.Dial` and `Options.Configure` let a product or a test replace the connection and the `serve` options.
- Add the `agent-visor` binary with `serve`, `status`, `version`, `ls`, `project`, `workdir`, `session`, and `harness`, and the global flags `--home`, `--socket`, and `--json`. `upgrade` says "not available yet".
- Add the `holder` package: `holder.Run` owns the PTY of one session, writes `sessions/<id>/pty.log` with mode 0600, keeps a ring for snapshots, and serves `holders/<id>.sock` with mode 0600. Its protocol is small and has a version in the hello.
- Add the built-in `pty` backend. A session runs in a holder (`Options.HolderCommand`, default this program with `hold`) and survives a visor restart; `Recover` finds it again. A shell runs `$SHELL` or the spec command. An agent runs the harness program with `ExtraArgs`, or `Options.TerminalCommand`.
- Add `Visor.Attach` and `AttachStream` (`Recv`, `Handle`, `Input`, `Close`), and the exported attach frames. Many clients attach, and one holds the size lease. A slow client gets a new snapshot. The chat view gives the turns of a wire session from the event log.
- Add `Visor.Handover`: wire to pty at the end of the turn, with the resume command of `Options.TerminalCommand` (else `ErrUnsupported`), and pty to wire when the terminal program exits. The session id stays, and one process at a time writes to the vendor session.
- `Visor.Close` detaches a session of a backend with `Caps.Survives` and does not stop it. Backends close before the visor waits for its pumps.
- Add hooks intake and observed sessions. The `hooks` package reads the hook payloads of Claude Code and Codex and edits their hook configs. `Visor.Hook` takes one `HookCall`: with a session id it updates that session, else it makes or updates an `observed` session in the workdir of its folder. `Hooks`, `InstallHooks`, and `RemoveHooks` read and change the hook entries of the visor (`HookSpec`, `HookStatus`), and `Options.HookCommand` sets the hook command. `HookSpool` keeps the calls that do not reach the visor, and the visor applies them at boot. The `hook` command sends the payload on stdin, or keeps it in the spool. Routes: `POST /v1/hooks/{harness}`, `GET`, `PUT`, and `DELETE /v1/hooks`.
- Add the socket routes and the `Client` methods for harnesses (`/v1/harnesses/...`), attach (`GET /v1/sessions/{id}/attach`, a WebSocket), and handover (`POST /v1/sessions/{id}/handover`). The attached frame now has `view`. An attach error frame uses the same codes as the error body.
- Add the CLI commands `hold`, `session attach`, `session shell`, and `session handover`. Ctrl-] detaches.
- Require agent-wire v0.8.0.
- `Visor.Send`, `Client.Send`, and `Handle.Send` return a `Delivery`: `prompted`, `steered`, or `held`. The send route answers 200 with `{"delivery": "..."}`.
- Mark the user event that the visor writes for a send or a first prompt with `input: true`. A vendor echo has no mark.
- Add `StartSpec.Binary`: the harness program. It wins over the visor install in `Launch.Binary`.
- The wire backend writes the agent-wire frame log to `sessions/<id>/frames.ndjson` with mode 0600.
- Fix: an answer that returns after the end of the turn no longer moves an idle session back to running.
- Fix: a PTY session that ends while the holder has dropped the visor gets its last output from `pty.log`.
