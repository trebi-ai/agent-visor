# Changelog

## v0.5.2 (2026-11-09)

- Fix the lint error in the tests of v0.5.1. The code does not change.

## v0.5.1 (2026-11-09)

- `Event.Kind` keeps the kind of the input of a user event. `StartSpec.PromptKind` gives the kind of the first prompt. The restart prompt has the kind `restart` (`InputKindRestart`).

## v0.5.0 (2026-11-07)

- Add harness variants. `harness.Options.Variants` gives each variant name with the name of its base harness. A variant runs the program and the driver of its base, with its own home (`<home>/harness/<variant>`), auth mode, login, and env. `Install`, `Update`, and `SetUpdates` refuse a variant with `ErrUnsupported`; a variant uses the install and the updates of its base.
- `Harness.Base` names the base of a variant. `List` gives the variants after the base harnesses.
- `Launch.Kind` names the harness whose program and driver run. A backend uses it in place of `Spec.Harness`. `Manager.Base` gives it for a name.

## v0.4.2 (2026-11-01)

- Fix: a Claude session in the separate harness home showed the folder trust dialog. Before each launch, the visor marks the working dir (and its real path) as trusted in the home `.claude.json` (agent-wire v0.12.0). The start of a session in a folder is the approval.

## v0.4.1 (2026-11-01)

- `Purge` removes the vendor files only when the spec names `Harness` and `VendorID`. It no longer takes them from the session, because a product can resume the same vendor session in a later session.

## v0.4.0 (2026-11-01)

- Add `Purge` (`POST /v1/purge`) on `Visor` and `Client`. It removes an ended session, its events, its files, and the files that the vendor keeps for it in the harness home of the visor (agent-wire `SessionFiles`). With `Harness` and `VendorID` and no `Session`, it removes only the vendor files. It refuses a live session, and a vendor session that a live session uses.
- The frame log of a wire session leaves out the stream fragments (agent-wire v0.11.0). When the stream fails, the fragments go to `fragments/<id>.ndjson`. `StartSpec.LogFragments` writes every fragment to the frame log.
- `Options.KeepFragments` (default 30 days) is how long the visor keeps a fragment file.
- The visor forgets an exited session after `KeepExited` each hour, not only at start.

## v0.3.3 (2026-11-01)

- Fix: two state changes of one session at the same time could give their events in the wrong order. The last state event then did not agree with the state of the session.

## v0.3.2 (2026-11-01)

- Fix a test of the Orca backend that failed when the poll came late.

## v0.3.1 (2026-11-01)

- Add `orca.Backend.Snapshot` and `orca.Options.OnPoll`. A product can read the Orca state of the last poll in place of a second poll of Orca.
- Fix: a hook call with labels for an observed session that the visor knows already gives the labels to that session.

## v0.3.0 (2026-10-31)

- Add the optional backend `backend/orca`. It runs sessions in the terminals of the Orca app, follows the Orca agent cards, answers prompts through the screen, keeps its sessions across a visor restart, and can adopt the agent panes that it did not start.
- Add the optional backend interfaces `TextTerminal`, `Labeler`, `Discoverer` (with `Found`), and `Catalog`. `Screen`, `Keys`, `Paste`, and `Output` work on a `TextTerminal` session.
- Add `HookCall.Labels`. A hook call with labels joins the newest session with the same labels. An observed session joins a session that a backend adopts later.
- Fix: a `Send` after the end of the handle gave a state event for a session with no process.
- Fix the Windows build. CI vets the module for Windows.

## v0.2.1 (2026-10-31)

- Add `HarnessFacts.Binary`. It finds the program of a harness that agent-wire does not find, such as a test harness of the product. The inventory shows it as a system install, and a login and a logout run it.

## v0.2.0 (2026-10-31)

Breaking: `LoginHarness` takes a `LoginSpec`, and `cli.Visor` has more methods.

- The visor builds the harness home in `harness/<harness>/` from the agent-wire file map. In shared mode the home links the login of the user. Each other file is shared (a link, or a copy of its keys) or an own copy. `Options.Harness.UserHome` sets the user home for tests.
- Add `HarnessPolicy` in `HarnessOptions.Policy`: the MCP servers of each home, no user hooks, the default share of a file, fixed keys, and product files.
- Add the files API: `HarnessFiles`, `ShareHarnessFile`, `OwnHarnessFile`, `ReadHarnessFile`, and `WriteHarnessFile`. An own copy keeps a backup for 30 days after a share. A write with an old etag gives `ErrStale` (code `stale`, status 412).
- Add update modes: `auto` updates to the newest release, and `keep` holds one version and keeps a copy of a system binary. `SetHarnessUpdates` sets the mode. `Harness` has `Updates`, `KeptAt`, `Pending`, `Updating`, and `History`.
- The visor looks for new releases each `CheckEvery` (default 6 h) and applies the update modes. `HarnessOptions.Enabled` selects the harnesses; nil checks only the visor copies. An update waits while a live session uses the harness, checks the new version and the login, and rolls back when the check fails. A version change from outside the visor goes into the history. `CheckHarnesses` checks now.
- `InstallHarness` runs the vendor install for a recipe that is not npm.
- A login runs in a PTY session with the label `visor.login`, so a client can attach to it. `Login` has `Phase`, `Session`, `Screen`, and `EndedAt`. Add `HarnessLogin`, `HarnessLoginCode`, `HarnessLoginToken`, and `CancelHarnessLogin`. A login stays readable for a minute after its end. A separate login of Claude Code uses its token command, and a token goes into the harness env.
- Add the routes `PUT /v1/harnesses/{name}/updates`, `POST /v1/harnesses/check`, `GET` and `DELETE /v1/harnesses/{name}/login`, `POST .../login/code`, `POST .../login/token`, and `/v1/harnesses/{name}/files/...`, with their fixtures.
- Add the CLI commands `harness updates`, `harness check`, `harness code`, `harness files`, `harness share`, and `harness own`, and `harness login --provider`.
- Require agent-wire v0.9.0.

## v0.1.2 (2026-10-30)

- Fix: a wire holder sends the reply of an open call before its exit. Before, a send that ended the vendor process could lose its reply, and the visor reported "has no process".

## v0.1.1 (2026-10-30)

The first release. v0.1.0 has the same API, but its workdir code fails with git before 2.31. Use v0.1.1.


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
- Add the `agent-visor` binary with `serve`, `status`, `version`, `ls`, `project`, `workdir`, `session`, and `harness`, and the global flags `--home`, `--socket`, and `--json`.
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
- Run each wire session in a wire holder (`hold` reads the mode from its config), so a turn lives through a visor restart, a crash, or an upgrade. The holder owns the stdio pipes, the open permission requests, and the agent-wire ledger (`sessions/<id>/ledger.json`). It numbers its events. The visor acks each event after it stores it, and a holder sends the events after the last ack again, so no event is lost or stored twice. `Caps.Survives` is true for the wire backend.
- The wire holder ends when its session is idle, and the session becomes `suspended`. The next send starts a holder of the current build, which resumes the vendor session. A holder that ends with no visor keeps the events that the visor did not store in `wire/<id>.spool`, and the next visor reads them.
- A harness with a driver in `Options.Drivers` runs its wire sessions in the visor process, as before.
- The holder protocol is version 2. It adds the wire mode and the event, ack, call, and reply messages. `holder.Dial` takes a `holder.Cursor` and speaks to holders of version 1 and 2.
- The store schema is version 2. An event keeps the holder run and sequence that it came from.
- Add the quiet upgrade: `Visor.Upgrade`, `Client.Upgrade`, `UpgradeSpec`, and `POST /v1/upgrade`. A `Hello` with a `Binary` of a newer build also schedules it. `Info.Upgrade` gives the pending build. The visor waits until no session in a turn needs its process, no attach client is open, and no harness login, install, or update runs. A session in a holder does not stop the upgrade.
- An upgrade waits `Options.UpgradeWait` (default 24 hours), or 10 minutes at most when the protocol major changes. Then a session in a turn that is not in a holder becomes `lost`, and its restart policy applies in the next visor.
- Add `ServeListener`. `Serve` and the `serve` command use it. Only a visor that it serves upgrades. When an upgrade is ready, it writes `visor.restarting` with the new build in `data`, closes the socket, closes the visor, and runs the new binary with the same arguments and env (`Options.Exec`, default `syscall.Exec`). `Options.Now` sets the clock of the deadline.
- `Start` gives `ErrBusy` while the visor restarts for an upgrade.
- The `upgrade` command asks the running visor to upgrade to this binary. `status` shows a pending upgrade.
- Add `harness.Manager.Busy`.
- Fix: an answer that returns after the end of the turn no longer moves an idle session back to running.
- Fix: a PTY session that ends while the holder has dropped the visor gets its last output from `pty.log`.
- Add `Visor.Screen`, `Client.Screen`, `Visor.Keys`, and `Client.Keys`, with the routes `GET /v1/sessions/{id}/screen` and `POST /v1/sessions/{id}/keys`. A terminal emulator in the visor renders the PTY output as text. While no client holds the size lease, the emulator answers the terminal queries of the program, for example the cursor position.
- Add the CLI command `session screen`.
- Add `Visor.Output` and `Client.Output`, with the route `GET /v1/sessions/{id}/output`: the recent output of a PTY session as plain text, also after the session ends.
- The terminal emulator reads the output only when a client asks for the screen, or when the program asks a status query.
- When the home path is too long for a Unix socket path, the holder sockets go to `/tmp/agent-visor-<uid>/<hash of home>/`. The visor checks that this folder belongs to the user and has mode 0700. `holder.MaxSocketPath` is exported.
- Add `Visor.Paste` and `Client.Paste`, with the route `POST /v1/sessions/{id}/paste`. When the program asks for bracketed paste, the text goes in the paste markers.
