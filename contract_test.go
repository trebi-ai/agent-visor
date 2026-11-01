package agentvisor

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

// fixture is one contract file and the Go value that encodes to it.
type fixture struct {
	name string
	v    any
}

//nolint:gochecknoglobals // a test flag
var updateContract = flag.Bool("update", false, "write the contract files from the Go types")

// TestContract checks that the Go types encode to the fixtures, the schema,
// and the route list in contract/. Run it with -update after a change.
func TestContract(t *testing.T) {
	t.Parallel()
	defs := map[string]any{}
	messages := map[string]any{}
	for _, f := range contractFixtures() {
		got := encodeContract(t, f.v)
		checkContractFile(t, filepath.Join("contract", "fixtures", f.name+".json"), got)
		back := reflect.New(reflect.TypeOf(f.v))
		if err := json.Unmarshal(got, back.Interface()); err != nil {
			t.Fatalf("%s: decode: %v", f.name, err)
		}
		if again := encodeContract(t, back.Elem().Interface()); !bytes.Equal(again, got) {
			t.Errorf("%s: the fixture does not round-trip:\n%s", f.name, again)
		}
		messages[f.name] = jsonSchema(reflect.TypeOf(f.v), defs)
	}
	schema := map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"$id":        Protocol,
		"properties": messages,
		"$defs":      defs,
	}
	checkContractFile(t, filepath.Join("contract", "schema.json"), encodeContract(t, schema))
	checkContractFile(t, filepath.Join("contract", "routes.json"), encodeContract(t, contractRoutes()))
}

func contractFixtures() []fixture {
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	code := 0
	req := Request{ID: "perm_1", Tool: "Bash", Kind: "permission", Question: "Run it?", Input: `{"command":"ls"}`}
	return []fixture{
		{"hello", Hello{Client: "example", Build: "v1.2.3", Protocol: Protocol, Binary: "/usr/local/bin/example"}},
		{"info", Info{Build: "v1.2.3", Protocol: Protocol, Client: "example", Home: "/home/u/.example/visor", Seq: 42, Upgrade: "v1.2.4", PID: 4242}},
		{"upgrade_spec", UpgradeSpec{Binary: "/usr/local/bin/example", Build: "v1.2.4", Protocol: Protocol}},
		{"purge_spec", PurgeSpec{Session: "ses_1", Harness: "claude", VendorID: "56fc5395-9134-4a60-9bfb-6d2048569e1c"}},
		{"error", errorBody{Error: errorInfo{Code: "not_found", Message: "session ses_1: agentvisor: not found"}}},
		{"start_spec", StartSpec{
			Harness: "claude", Model: "sonnet", Effort: "high", Binary: "/usr/local/bin/claude", WorkingDir: "/src/app", Prompt: "Fix the test.",
			Env: map[string]string{"API_TOKEN": "secret"}, Labels: map[string]string{"app.run": "r_1"},
			Permissions: Permissions{Mode: agentwire.PermissionAsk, AllowTools: []string{"Read"}}, Skills: []Skill{{Dir: "/src/skills/review"}}, Restart: RestartPolicy{Mode: RestartResume, Prompt: "Continue."},
		}},
		{"session", Session{
			ID: "ses_1", Project: "prj_1", Workdir: "wd_1", Kind: KindWire, Role: RoleAgent, Harness: "claude", Model: "sonnet",
			WorkingDir: "/src/app", State: StateWaiting, VendorID: "v-1", PID: 4243, Labels: map[string]string{"app.run": "r_1"},
			Pending: []Request{req}, Restart: RestartPolicy{Mode: RestartResume, Prompt: "Continue."}, EnvKeys: []string{"API_TOKEN"},
			Seq: 41, Turns: 1, CreatedAt: at, UpdatedAt: at,
		}},
		{"input", Input{Text: "Also run the linter."}},
		{"send_reply", sendReply{Delivery: DeliverySteered}},
		{"answer", answerRequest{RequestID: "perm_1", Answer: Answer{Allow: false, Message: "Use the make target."}}},
		{"workdir_spec", WorkdirSpec{Path: "/src/app", Key: "fix-1", Kind: WorkdirWorktree, Base: "main"}},
		{"workdir", Workdir{
			ID: "wd_1", Project: "prj_1", Key: "fix-1", Kind: WorkdirWorktree, Path: "/home/u/.example/visor/worktrees/app-prj_1/fix-1",
			Branch: "fix-1", Base: "main", State: WorkdirActive, PortBase: 41010, Setup: []string{"make deps"}, CreatedAt: at, UpdatedAt: at,
		}},
		{"project", Project{ID: "prj_1", Name: "app", Path: "/src/app", Git: true, CreatedAt: at}},
		{"events", []Event{
			{Seq: 39, Session: "ses_1", At: at, Type: EventUser, Text: "Also run the linter.", Input: true},
			{Seq: 40, Session: "ses_1", At: at, Type: EventSessionState, State: StateRunning},
			{Seq: 41, Session: "ses_1", At: at, Type: EventTool, Tool: &Tool{ID: "t1", Name: "Bash", Input: `{"command":"ls"}`, Status: "done", ExitCode: &code}},
			{Seq: 42, Session: "ses_1", At: at, Type: EventRequest, Request: &req},
			{Seq: 43, Session: "ses_1", At: at, Type: EventRequestClosed, RequestID: "perm_1"},
			{Seq: 44, Session: "ses_1", At: at, Type: EventResult, Turn: 1, Result: &Result{Subtype: "success", Text: "Done.", Usage: Usage{Input: 1200, Output: 300, Model: "sonnet"}}},
			{Seq: 45, At: at, Type: EventRestarting, Data: json.RawMessage(`{"build":"v1.2.4"}`)},
		}},
		{"hook_call", HookCall{
			Harness: "claude", Session: "ses_1", At: at,
			Payload: json.RawMessage(`{"session_id":"v-1","cwd":"/src/app","hook_event_name":"Stop","last_assistant_message":"Done."}`),
		}},
		{"handover", handoverRequest{To: KindPTY}},
		{"screen", Screen{Cols: 80, Rows: 3, Lines: []string{"$ make test", "ok", "$"}, CursorRow: 2, CursorCol: 2}},
		{"output", outputReply{Text: "$ make test\nok\n"}},
		{"keys", keysRequest{Data: []byte("\x1b[B\r")}},
		{"paste", pasteRequest{Text: "line one\nline two"}},
		{"attach_client_frames", []ClientFrame{
			{Type: ClientAttach, View: ViewTerminal, Cols: 120, Rows: 40},
			{Type: ClientAck, Seq: 65536},
			{Type: ClientControl, Cols: 100, Rows: 30},
			{Type: ClientRelease},
			{Type: ClientSend, Text: "Also run the linter."},
			{Type: ClientInterrupt},
		}},
		{"attach_frames", []Frame{
			{Type: FrameAttached, View: ViewTerminal, Source: KindPTY, Cols: 120, Rows: 40, Writable: true, Resizable: true, Title: "app"},
			{Type: FrameResized, Cols: 100, Rows: 30},
			{Type: FrameLease, Holder: LeaseYou},
			{Type: FrameTurns, From: 40, To: 44, Turns: []Turn{
				{Seq: 41, Role: TurnUser, At: at, Text: "Fix the test."},
				{Seq: 44, Role: TurnResult, At: at, Text: "Done."},
			}},
			{Type: FrameEnded, Reason: EndExited},
			{Type: FrameError, Code: "unsupported", Message: "agentvisor: unsupported: the chat view takes no control frame"},
		}},
		{"harness", Harness{
			Name: "claude", Installed: true, Version: "2.1.3", Latest: "2.1.4", Path: "/home/u/.example/visor/bin/claude/2.1.3/bin/claude",
			Source: SourceVisor, Installs: []string{"2.1.3"}, Installable: true, Supported: true, AuthMode: AuthSeparate, Auth: AuthOK,
			EnvKeys: []string{"API_TOKEN"}, Models: []Model{{ID: "sonnet", Name: "Sonnet", Default: true, Efforts: []string{"low", "medium", "high"}}},
			Updates: UpdatesKeep, KeptAt: &at, Pending: true,
			History: []HarnessUpdate{{At: at, From: "2.1.2", To: "2.1.3", OK: true}, {At: at, From: "2.1.3", To: "2.1.4", Error: "the version check failed"}},
		}},
		{"harness_updates", updatesRequest{Mode: UpdatesKeep, Version: "2.1.3"}},
		{"login_spec", LoginSpec{Provider: "anthropic"}},
		{"login_code", codeRequest{Code: "ABCD-1234"}},
		{"login_token", tokenRequest{Token: "secret"}},
		{"home_files", []HomeFile{
			{Key: "login", Kind: "link", Shared: true, Login: true, Locked: true, UserPath: "/home/u/.claude/.credentials.json", HomePath: "/home/u/.example/visor/harness/claude/.credentials.json", UserExists: true, UserModAt: &at},
			{Key: "settings_prefs", Kind: "keys", Format: "json", UserPath: "/home/u/.claude/settings.json", HomePath: "/home/u/.example/visor/harness/claude/settings.json", UserExists: true, OwnSince: &at, UserChanged: true, BackupUntil: &at},
		}},
		{"file_content", FileContent{Key: "settings_prefs", Side: SideHome, Content: "{}", ETag: "0123456789abcdef0123456789abcdef"}},
		{"file_write", fileWrite{Content: "{}", IfMatch: "0123456789abcdef0123456789abcdef"}},
		{"file_own", ownRequest{Start: OwnFromBackup}},
		{"harness_version", versionRequest{Version: "2.1.4"}},
		{"harness_auth", authRequest{Mode: AuthShared}},
		{"harness_env", envRequest{Env: map[string]string{"API_TOKEN": "secret"}}},
		{"login", Login{
			Harness: "claude", Phase: LoginCode, URL: "https://login.example.com/device", Code: "ABCD-1234", AwaitCode: true,
			Session: "ses_01", Screen: "Paste code here:", StartedAt: at,
		}},
		{"hook_spec", HookSpec{Harnesses: []string{"claude", "codex"}, Auth: AuthShared}},
		{"hook_status", []HookStatus{
			{Harness: "claude", Auth: AuthShared, Dir: "/home/u/.claude", State: HookInstalled},
			{Harness: "codex", Auth: AuthShared, Dir: "/home/u/.codex", State: HookUntrusted},
		}},
	}
}

func contractRoutes() []string {
	s := &Server{}
	var out []string
	for _, table := range [][]route{s.serverRoutes(), s.sessionRoutes(), s.workdirRoutes(), s.eventRoutes(), s.hookRoutes(), s.attachRoutes(), s.harnessRoutes()} {
		for _, r := range table {
			out = append(out, r.pattern)
		}
	}
	return out
}

func encodeContract(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func checkContractFile(t *testing.T, path string, want []byte) {
	t.Helper()
	if *updateContract {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the test with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s does not match the Go types; run the test with -update", path)
	}
}

// jsonSchema gives the JSON schema of t. Named structs go to defs.
func jsonSchema(t reflect.Type, defs map[string]any) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == reflect.TypeFor[time.Time]():
		return map[string]any{"type": "string", "format": "date-time"}
	case t == reflect.TypeFor[json.RawMessage]():
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.Struct:
		return structSchema(t, defs)
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": jsonSchema(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": jsonSchema(t.Elem(), defs)}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{}
	}
}

func structSchema(t reflect.Type, defs map[string]any) any {
	name := t.Name()
	ref := map[string]any{"$ref": "#/$defs/" + name}
	if _, ok := defs[name]; ok {
		return ref
	}
	defs[name] = nil // stops recursion
	props := map[string]any{}
	var required []string
	addFields(t, defs, props, &required)
	obj := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		obj["required"] = required
	}
	defs[name] = obj
	return ref
}

func addFields(t reflect.Type, defs, props map[string]any, required *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && tag == "" {
			addFields(f.Type, defs, props, required)
			continue
		}
		if !f.IsExported() || tag == "-" {
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		props[tag] = jsonSchema(f.Type, defs)
		if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") {
			*required = append(*required, tag)
		}
	}
}
