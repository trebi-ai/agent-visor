# Changelog

## Unreleased

- Add the library core: `Visor`, `StartSpec`, `Session`, `Event`, `Filter`, and the `Backend` and `Handle` interfaces.
- Add the built-in `wire` backend over agent-wire.
- Add the event log in SQLite with a global sequence. A replay after a sequence has no gaps and no duplicates.
- Add the session states, the restart policy, suspend at close, and lost at boot.
- Add the `workdir` package: projects from paths, the main checkout, git worktrees by key, plain folders, setup and teardown scripts, and port ranges.
- Keep the start spec with env in `sessions/<id>/spec.json` with mode 0600. The API returns only the env keys.
