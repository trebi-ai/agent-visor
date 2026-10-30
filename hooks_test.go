package agentvisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const settingsJSON = `{
  "model": "sonnet",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "say done"
          }
        ]
      }
    ]
  }
}
`

func TestHookObservedSession(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		v := open(t, t.TempDir(), scriptTurns, nil)
		defer v.Close(context.Background())
		cwd := t.TempDir()
		call := func(event string, extra map[string]any) Session {
			return hookCall(t, v, HookCall{Harness: "claude", Payload: hookPayload(t, event, "v-1", cwd, extra)})
		}
		if s, err := v.Hook(t.Context(), HookCall{Harness: "claude", Payload: hookPayload(t, "Notification", "v-0", cwd, map[string]any{"notification_type": "idle_prompt"})}); err != nil || s.ID != "" {
			t.Fatalf("an ignored call: %+v %v", s, err)
		}
		s := call("SessionStart", map[string]any{"source": "startup"})
		w, err := v.EnsureWorkdir(t.Context(), WorkdirSpec{Path: cwd})
		if err != nil {
			t.Fatal(err)
		}
		if s.Kind != KindObserved || s.Role != RoleAgent || s.Harness != "claude" || s.VendorID != "v-1" || s.State != StateIdle ||
			s.WorkingDir != cwd || s.Project != w.Project || s.Workdir != w.ID {
			t.Fatalf("observed session: %+v", s)
		}
		for _, step := range []struct {
			event string
			extra map[string]any
			state State
			open  int
		}{
			{"UserPromptSubmit", map[string]any{"prompt": "Add a route."}, StateRunning, 0},
			{"PermissionRequest", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "make test"}}, StateWaiting, 1},
			{"PreToolUse", map[string]any{"tool_name": "AskUserQuestion", "tool_use_id": "toolu_1", "tool_input": map[string]any{
				"questions": []any{map[string]any{"question": "Which port?", "options": []any{map[string]any{"label": "8080"}}}},
			}}, StateWaiting, 2},
			{"PostToolUse", map[string]any{"tool_name": "AskUserQuestion", "tool_use_id": "toolu_1"}, StateRunning, 0},
			{"Stop", map[string]any{"last_assistant_message": "The route is in place."}, StateIdle, 0},
		} {
			got := call(step.event, step.extra)
			if got.ID != s.ID || got.State != step.state || len(got.Pending) != step.open {
				t.Fatalf("after %s: %s with %d open, want %s with %d", step.event, got.State, len(got.Pending), step.state, step.open)
			}
		}
		if got, err := v.Session(t.Context(), s.ID); err != nil || got.Turns != 1 {
			t.Fatalf("turns: %+v %v", got, err)
		}
		if got := call("SessionEnd", nil); got.State != StateExited {
			t.Fatalf("after the end: %s", got.State)
		}
		again := call("SessionStart", map[string]any{"source": "resume"})
		if again.ID == s.ID || again.State != StateIdle {
			t.Fatalf("a call after the end: %+v", again)
		}
		list, err := v.Sessions(t.Context(), Filter{Kind: KindObserved})
		if err != nil || len(list) != 2 {
			t.Fatalf("observed sessions: %d %v", len(list), err)
		}
		st, err := v.Events(t.Context(), Filter{Session: s.ID}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		var hooks, questions int
		var result *Result
		for _, e := range collect(t, st, func(e Event) bool { return e.Type == EventSessionState && e.State == StateExited }) {
			switch e.Type {
			case EventHook:
				hooks++
				if !json.Valid(e.Data) || e.VendorID != "v-1" {
					t.Fatalf("hook event: %+v", e)
				}
			case EventRequest:
				if e.Request.Question != "" {
					questions++
				}
			case EventResult:
				result = e.Result
			}
		}
		if hooks != 7 || questions != 1 || result == nil || result.Text != "The route is in place." || result.Subtype != hookSuccess {
			t.Fatalf("events: %d hooks, %d questions, result %+v", hooks, questions, result)
		}
	})
}

func TestHookErrors(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		v := open(t, t.TempDir(), scriptTurns, nil)
		defer v.Close(context.Background())
		cwd := t.TempDir()
		for name, tc := range map[string]struct {
			call HookCall
			err  error
		}{
			"unknown session": {HookCall{Harness: "claude", Session: "ses_none", Payload: hookPayload(t, "Stop", "v-1", cwd, nil)}, ErrNotFound},
			"unknown harness": {HookCall{Harness: "fake", Payload: hookPayload(t, "Stop", "v-1", cwd, nil)}, ErrInvalid},
			"no session id":   {HookCall{Harness: "claude", Payload: json.RawMessage(`{"hook_event_name":"Stop"}`)}, ErrInvalid},
			"relative cwd":    {HookCall{Harness: "claude", Payload: hookPayload(t, "Stop", "v-1", "app", nil)}, ErrInvalid},
			"missing cwd":     {HookCall{Harness: "claude", Payload: hookPayload(t, "Stop", "v-1", filepath.Join(cwd, "gone"), nil)}, ErrInvalid},
		} {
			if _, err := v.Hook(t.Context(), tc.call); !errors.Is(err, tc.err) {
				t.Errorf("%s: %v, want %v", name, err, tc.err)
			}
		}
		if list, err := v.Sessions(t.Context(), Filter{}); err != nil || len(list) != 0 {
			t.Fatalf("sessions: %+v %v", list, err)
		}
	})
}

func TestHookWithSessionEnv(t *testing.T) {
	t.Parallel()
	v := newTestVisor(t, t.TempDir(), scriptTurns, nil)
	defer v.Close(context.Background())
	cwd := t.TempDir()
	add := func(kind Kind, harness string) Session {
		t.Helper()
		now := time.Now().UTC()
		rec := &record{s: Session{ID: newID("ses"), Kind: kind, Role: RoleAgent, Harness: harness, WorkingDir: cwd, State: StateStarting, CreatedAt: now, UpdatedAt: now}}
		if err := v.add(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
		return rec.s
	}
	pty, wire, shell := add(KindPTY, "claude"), add(KindWire, "claude"), add(KindPTY, "")
	for _, tc := range []struct {
		s      Session
		event  string
		vendor string
		state  State
	}{
		{pty, "SessionStart", "v-pty", StateIdle},
		{pty, "UserPromptSubmit", "v-pty", StateRunning},
		{pty, "PermissionRequest", "v-pty", StateWaiting},
		{pty, "Stop", "v-pty", StateIdle},
		// The backend owns the end of a session that the visor runs.
		{pty, "SessionEnd", "v-pty", StateIdle},
		// A structured backend reports its own state.
		{wire, "UserPromptSubmit", "v-wire", StateStarting},
		// A hook of an agent in a shell changes nothing.
		{shell, "UserPromptSubmit", "", StateStarting},
	} {
		got := hookCall(t, v, HookCall{Harness: "claude", Session: tc.s.ID, Payload: hookPayload(t, tc.event, "v-"+string(tc.s.Kind), cwd, map[string]any{"tool_name": "Bash"})})
		if got.ID != tc.s.ID || got.State != tc.state || got.VendorID != tc.vendor {
			t.Fatalf("%s %s: %s %q, want %s %q", tc.s.Kind, tc.event, got.State, got.VendorID, tc.state, tc.vendor)
		}
	}
	if list, err := v.Sessions(t.Context(), Filter{Kind: KindObserved}); err != nil || len(list) != 0 {
		t.Fatalf("a call with a session made a session: %+v %v", list, err)
	}
}

func TestHookSpoolReplay(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		home, cwd := t.TempDir(), t.TempDir()
		spool := NewHookSpool(home)
		t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
		for i, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
			c := HookCall{Harness: "claude", At: t0.Add(time.Duration(i) * time.Second), Payload: hookPayload(t, event, "v-1", cwd, nil)}
			if err := spool.Write(c); err != nil {
				t.Fatal(err)
			}
		}
		if err := spool.Write(HookCall{Harness: "fake", Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, "hooks", "spool")
		if err := os.WriteFile(filepath.Join(dir, "0-bad.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 5 {
			t.Fatalf("spool: %d %v", len(entries), err)
		}
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("spool mode: %v %v", st, err)
		}
		if st, err := os.Stat(filepath.Join(dir, entries[1].Name())); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("call mode: %v %v", st, err)
		}
		v := open(t, home, scriptTurns, nil)
		defer v.Close(context.Background())
		list, err := v.Sessions(t.Context(), Filter{Kind: KindObserved})
		if err != nil || len(list) != 1 {
			t.Fatalf("sessions: %+v %v", list, err)
		}
		if s := list[0]; s.State != StateIdle || s.Turns != 1 || s.VendorID != "v-1" {
			t.Fatalf("replayed session: %+v", s)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("spool after the replay: %d %v", len(entries), err)
		}
		st, err := v.Events(t.Context(), Filter{Session: list[0].ID}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		var at []time.Time
		for _, e := range collect(t, st, func(e Event) bool { return e.Type == EventResult }) {
			if e.Type == EventHook {
				at = append(at, e.At)
			}
		}
		if len(at) != 3 {
			t.Fatalf("hook events: %d", len(at))
		}
		for i, a := range at {
			if !a.Equal(t0.Add(time.Duration(i) * time.Second)) {
				t.Fatalf("hook %d at %s", i, a)
			}
		}
	})
}

func TestHookSpoolRejectsLargePayload(t *testing.T) {
	t.Parallel()
	big := json.RawMessage(`"` + strings.Repeat("a", 1<<20) + `"`)
	if err := NewHookSpool(t.TempDir()).Write(HookCall{Harness: "claude", Payload: big}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("large payload: %v", err)
	}
}

func TestInstallHooks(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		home := t.TempDir()
		v := open(t, home, scriptTurns, nil)
		defer v.Close(context.Background())
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(path, []byte(settingsJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		spec := HookSpec{Harnesses: []string{"claude"}, Dir: dir}
		check := func(want HookState) func([]HookStatus, error) {
			return func(out []HookStatus, err error) {
				t.Helper()
				if err != nil || len(out) != 1 || out[0].State != want || out[0].Dir != dir || out[0].Auth != "" {
					t.Fatalf("status: %+v %v, want %s", out, err, want)
				}
			}
		}
		check(HookMissing)(v.Hooks(t.Context(), spec))
		check(HookInstalled)(v.InstallHooks(t.Context(), spec))
		installed, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(installed, []byte("agent-visor-hook=1:test;")) || !bytes.Contains(installed, []byte(`"command": "say done"`)) {
			t.Fatalf("installed config:\n%s", installed)
		}
		check(HookInstalled)(v.InstallHooks(t.Context(), spec))
		if again, _ := os.ReadFile(path); !bytes.Equal(again, installed) { //nolint:errcheck // the compare fails on a read error
			t.Fatal("a second install changed the config")
		}
		check(HookMissing)(v.RemoveHooks(t.Context(), spec))
		if back, _ := os.ReadFile(path); string(back) != settingsJSON { //nolint:errcheck // the compare fails on a read error
			t.Fatalf("remove did not give the original bytes:\n%s", back)
		}
		out, err := v.InstallHooks(t.Context(), HookSpec{Auth: AuthSeparate})
		if err != nil || len(out) != 2 {
			t.Fatalf("separate: %+v %v", out, err)
		}
		for _, s := range out {
			if s.Auth != AuthSeparate || s.Dir != filepath.Join(home, "harness", s.Harness) || (s.State != HookInstalled && s.State != HookUntrusted) {
				t.Fatalf("separate status: %+v", s)
			}
		}
		for name, tc := range map[string]struct {
			spec HookSpec
			err  error
		}{
			"relative dir":  {HookSpec{Dir: "app"}, ErrInvalid},
			"bad auth":      {HookSpec{Auth: "both"}, ErrInvalid},
			"no hook facts": {HookSpec{Harnesses: []string{"fake"}, Dir: dir}, ErrUnsupported},
		} {
			if _, err := v.InstallHooks(t.Context(), tc.spec); !errors.Is(err, tc.err) {
				t.Errorf("%s: %v, want %v", name, err, tc.err)
			}
		}
	})
}

// TestInstallHooksSharedHome sets HOME, so it does not run in parallel.
func TestInstallHooksSharedHome(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	v := newTestVisor(t, t.TempDir(), scriptTurns, nil)
	defer v.Close(context.Background())
	out, err := v.InstallHooks(t.Context(), HookSpec{Harnesses: []string{"codex"}, Auth: AuthShared})
	if err != nil || len(out) != 1 || out[0].Dir != filepath.Join(user, ".codex") || out[0].State != HookUntrusted {
		t.Fatalf("shared: %+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(user, ".codex", "hooks.json")); err != nil {
		t.Fatal(err)
	}
}

// hookPayload gives a vendor payload of one hook event.
func hookPayload(t *testing.T, event, vendorID, cwd string, extra map[string]any) json.RawMessage {
	t.Helper()
	p := map[string]any{"session_id": vendorID, "cwd": cwd, "hook_event_name": event, "transcript_path": "/tmp/t.jsonl"}
	for k, v := range extra {
		p[k] = v
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hookCall(t *testing.T, v api, c HookCall) Session {
	t.Helper()
	s, err := v.Hook(t.Context(), c)
	if err != nil {
		t.Fatalf("hook %s: %v", c.Payload, err)
	}
	return s
}
