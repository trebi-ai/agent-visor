# Socket contract

This folder is the contract of the socket protocol `agent-visor/1`: HTTP over a Unix socket, with WebSocket for the event stream and attach.

- `routes.json` lists the routes.
- `schema.json` is the JSON schema of each message. The Go types make it.
- `fixtures/` holds one example of each message.

The Go types are the source. `TestContract` checks that the Go types encode to these files. After a change to a type, run `go test -run TestContract -update .` and commit the files with the change.

A client in another language vendors these files byte-identical. A break bumps the protocol version that `GET /v1/info` returns.

`POST /v1/sessions/{id}/send` answers 200 with `{"delivery": "prompted" | "steered" | "held"}` (`fixtures/send_reply.json`). A user event that the visor writes for a send or a first prompt has `"input": true`; a vendor echo has no mark. An input event has the `kind` of its input. The restart prompt has the kind `restart`.

`POST /v1/upgrade` takes an upgrade spec (`fixtures/upgrade_spec.json`) and answers with `info`. `info.upgrade` gives the build of a pending upgrade. A hello with a `binary` of a newer build schedules the same upgrade. The visor waits until no session in a turn needs its process, no attach client is open, and no harness login, install, or update runs, for 24 hours at most, or 10 minutes when the protocol major changes. Then it writes the event `visor.restarting`, closes the socket, and runs the binary. A client connects again and reads the events after its last seq.

Errors have the body `{"error": {"code": "...", "message": "..."}}`. The codes are `not_found`, `invalid`, `state`, `busy`, `closed`, `unsupported`, `protocol`, `stale`, `download`, and `internal`.

The event stream `GET /v1/events?after=<seq>` sends one JSON event in each text frame: first the events after `after` from the log, then the live events, with no gaps and no duplicates. Close code 4001 means that the visor stops; 4002 means that the stream failed. A client connects again with the last seq that it got.

Attach `GET /v1/sessions/{id}/attach` is a WebSocket. The client sends an `attach` frame first (`fixtures/attach_client_frames.json`). The visor answers with `attached`, or with one `error` frame and close code 4002. Then the visor sends the frames in `fixtures/attach_frames.json`, and the client sends `ack`, `control`, `release`, `send`, and `interrupt`. Terminal bytes go in binary frames: op (1 byte) | seq (8 bytes, big endian) | data. The ops are 1 snapshot, 2 output, and 3 input. Only input goes from the client to the visor. The seq of an output frame counts the binary bytes that the visor sent, and an `ack` gives it back. A normal close (1000) follows `ended`; 4001 means that the visor stops.
