package hooks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	claudeID = "5d0c7a52-3f1e-4b8e-9a51-2c6d1f0e8b11"
	codexID  = "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b"
	appPath  = "/Users/ada/Projects/app"
)

// view is an event with the fields that the cases check.
type view struct {
	Kind                      Kind
	Ask                       Ask
	Tool, ToolUseID, Input    string
	Prompt, Question, Message string
	Error                     string
	Interrupted               bool
	Options                   string
}

func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		harness, file string
		ok            bool
		want          view
	}{
		{"claude", "SessionStart", true, view{Kind: KindSessionStart}},
		{"claude", "SessionStart-compact", false, view{}},
		{"claude", "UserPromptSubmit", true, view{Kind: KindPrompt, Prompt: "Add a health route."}},
		{"claude", "PreToolUse-AskUserQuestion", true, view{
			Kind: KindRequest, Ask: AskQuestion, Tool: "AskUserQuestion", ToolUseID: "toolu_01Q",
			Input:    `{"questions":[{"question":"Which database?","header":"Database","multiSelect":false,"options":[{"label":"Postgres","description":"Use Postgres"},{"label":"SQLite","description":"Use SQLite"}]}]}`,
			Question: "Which database?", Options: "Postgres,SQLite",
		}},
		{"claude", "PreToolUse-ExitPlanMode", true, view{Kind: KindRequest, Ask: AskPermission, Tool: "ExitPlanMode", ToolUseID: "toolu_01P", Input: "1. Add the route.\n2. Add a test."}},
		{"claude", "PostToolUse-AskUserQuestion", true, view{Kind: KindToolDone, Tool: "AskUserQuestion", ToolUseID: "toolu_01Q"}},
		{"claude", "PermissionRequest", true, view{Kind: KindRequest, Ask: AskPermission, Tool: "Bash", Input: "rm -rf build"}},
		{"claude", "PermissionRequest-subagent", true, view{Kind: KindRequest, Ask: AskPermission, Tool: "Bash", Input: "ls"}},
		{"claude", "PreToolUse-subagent", false, view{}},
		{"claude", "Notification-permission", true, view{Kind: KindAttention}},
		{"claude", "Notification-idle", false, view{}},
		{"claude", "Stop", true, view{Kind: KindStop, Message: "The route is in place. All tests pass."}},
		{"claude", "Stop-question", true, view{Kind: KindStop, Message: "I found two options.\n\nShould I use Postgres?"}},
		{"claude", "Stop-transcript", true, view{Kind: KindStop}},
		{"claude", "StopFailure", true, view{Kind: KindStop, Error: "rate_limit"}},
		{"claude", "SessionEnd", true, view{Kind: KindSessionEnd}},
		{"codex", "SessionStart", true, view{Kind: KindSessionStart}},
		{"codex", "UserPromptSubmit", true, view{Kind: KindPrompt, Prompt: "Fix the build."}},
		{"codex", "PreToolUse-request_user_input", true, view{
			Kind: KindRequest, Ask: AskQuestion, Tool: "request_user_input", ToolUseID: "call_1",
			Input:    `{"questions":[{"id":"db","header":"Database","question":"Which database?","options":[{"label":"Postgres","description":""}]}]}`,
			Question: "Which database?", Options: "Postgres",
		}},
		{"codex", "PreToolUse-shell", true, view{Kind: KindTool, Tool: "Bash", ToolUseID: "call_2"}},
		{"codex", "PermissionRequest", true, view{Kind: KindRequest, Ask: AskPermission, Tool: "Bash", Input: "git push"}},
		{"codex", "Stop", true, view{Kind: KindStop, Message: "The build passes now."}},
	}
	for _, c := range cases {
		t.Run(c.harness+"/"+c.file, func(t *testing.T) {
			t.Parallel()
			e, ok, err := Parse(c.harness, payloadFile(t, c.harness, c.file))
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			wantID := claudeID
			if c.harness == "codex" {
				wantID = codexID
			}
			if e.Harness != c.harness || e.VendorID != wantID || e.Cwd != appPath || e.Name == "" {
				t.Fatalf("identity: %+v", e)
			}
			if !ok {
				return
			}
			if got := viewOf(e); got != c.want {
				t.Fatalf("\ngot  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		harness string
		data    string
		err     error
	}{
		"unknown harness": {"cursor", `{}`, ErrUnsupported},
		"bad json":        {"claude", `{`, ErrPayload},
		"no session":      {"claude", `{"hook_event_name":"Stop"}`, ErrPayload},
		"too large":       {"claude", `"` + strings.Repeat("a", MaxPayload) + `"`, ErrPayload},
	} {
		if _, _, err := Parse(tc.harness, []byte(tc.data)); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", name, err, tc.err)
		}
	}
}

func TestInputTextClips(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]string{"command": strings.Repeat("a", 400)})
	if err != nil {
		t.Fatal(err)
	}
	if got := []rune(Event{ToolInput: raw}.InputText()); len(got) != maxInputText || got[len(got)-1] != '…' {
		t.Fatalf("clip: %d", len(got))
	}
	if (Event{}).InputText() != "" || (Event{ToolInput: json.RawMessage("null")}).InputText() != "" {
		t.Fatal("empty input")
	}
}

func viewOf(e Event) view {
	return view{
		Kind: e.Kind, Ask: e.Ask, Tool: e.Tool, ToolUseID: e.ToolUseID, Input: e.InputText(), Prompt: e.Prompt,
		Question: e.Question, Message: e.Message, Error: e.Error, Interrupted: e.Interrupted, Options: strings.Join(e.Options, ","),
	}
}

func payloadFile(t *testing.T, harness, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "payloads", harness, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
