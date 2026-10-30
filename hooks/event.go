package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Kind is the neutral kind of a hook event.
type Kind string

// Ask tells what a request asks the user.
type Ask string

// Event is one hook call in a neutral form.
type Event struct {
	Harness string
	// Name is the vendor event name.
	Name string
	Kind Kind
	// VendorID is the vendor session id.
	VendorID   string
	Cwd        string
	Transcript string
	Prompt     string
	Tool       string
	ToolUseID  string
	ToolInput  json.RawMessage
	// Ask is set on a request.
	Ask Ask
	// Question and Options are the first question of a question tool.
	Question string
	Options  []string
	// Message is the last assistant message of a stop.
	Message     string
	Interrupted bool
	// Error is set on a stop that failed.
	Error string
}

// payload holds the vendor fields that Parse reads. Claude Code and Codex use
// the same names.
type payload struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolInput            json.RawMessage `json:"tool_input"`
	NotificationType     string          `json:"notification_type"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	AgentID              string          `json:"agent_id"`
	IsInterrupt          bool            `json:"is_interrupt"`
	Error                string          `json:"error"`
}

// Event kinds.
const (
	KindSessionStart Kind = "session_start"
	KindPrompt       Kind = "prompt"
	// KindTool is a tool that starts. The turn runs.
	KindTool     Kind = "tool"
	KindToolDone Kind = "tool_done"
	// KindRequest is a permission or a question with its details.
	KindRequest Kind = "request"
	// KindAttention means that the harness waits for the user, with no
	// details.
	KindAttention  Kind = "attention"
	KindStop       Kind = "stop"
	KindSessionEnd Kind = "session_end"
)

// Asks.
const (
	AskPermission Ask = "permission"
	AskQuestion   Ask = "question"
)

// MaxPayload is the largest hook payload, in bytes.
const MaxPayload = 1 << 20

// maxInputText is the length of the short text of a tool input, in runes.
const maxInputText = 300

// Errors.
var (
	ErrUnsupported = errors.New("hooks: harness has no hook support")
	ErrPayload     = errors.New("hooks: bad payload")
)

// Parse reads one hook payload of a harness. ok is false for an event that
// does not change a session, for example a subagent tool or a compact
// restart.
func Parse(harness string, data []byte) (Event, bool, error) {
	v, err := lookup(harness)
	if err != nil {
		return Event{}, false, err
	}
	if len(data) > MaxPayload {
		return Event{}, false, fmt.Errorf("%w: %d bytes", ErrPayload, len(data))
	}
	var p payload
	if err := json.Unmarshal(data, &p); err != nil {
		return Event{}, false, fmt.Errorf("%w: %w", ErrPayload, err)
	}
	if p.SessionID == "" || p.HookEventName == "" {
		return Event{}, false, fmt.Errorf("%w: no session_id or hook_event_name", ErrPayload)
	}
	e := Event{
		Harness: harness, Name: p.HookEventName, VendorID: p.SessionID, Cwd: p.Cwd, Transcript: p.TranscriptPath,
	}
	question := v.question(p.ToolName)
	// A subagent event changes the session only when it waits for the user.
	if p.AgentID != "" && p.HookEventName != evPermissionRequest && (p.HookEventName != evPreToolUse || !question) {
		return e, false, nil
	}
	e.Kind = v.kind(p, question)
	if e.Kind == "" {
		return e, false, nil
	}
	return e.fill(p, question), true, nil
}

// InputText gives a short text of the tool input: the command or the path
// when there is one, else the compact JSON.
func (e Event) InputText() string {
	raw := e.ToolInput
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) == nil {
		for _, k := range []string{"command", "file_path", "path", "url", "pattern", "plan"} {
			if s, ok := fields[k].(string); ok && s != "" {
				return clip(s)
			}
		}
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	return clip(b.String())
}

// fill copies the payload fields of the event kind.
func (e Event) fill(p payload, question bool) Event {
	switch e.Kind {
	case KindPrompt:
		e.Prompt = p.Prompt
	case KindRequest:
		e.Ask = AskPermission
		if question {
			e.Ask = AskQuestion
		}
		e.Tool, e.ToolUseID, e.ToolInput = p.ToolName, p.ToolUseID, p.ToolInput
		e.Question, e.Options = firstQuestion(p.ToolInput)
	case KindTool, KindToolDone:
		e.Tool, e.ToolUseID = p.ToolName, p.ToolUseID
	case KindStop:
		e.Message, e.Interrupted, e.Error = p.LastAssistantMessage, p.IsInterrupt, p.Error
		if p.HookEventName == evStopFailure && e.Error == "" {
			e.Error = "failed"
		}
	}
	return e
}

// kind maps a vendor event to its neutral kind, or "" to ignore it.
func (v vendor) kind(p payload, question bool) Kind {
	switch p.HookEventName {
	case evSessionStart:
		// A compact restart runs inside a turn.
		if p.Source == sourceCompact {
			return ""
		}
		return KindSessionStart
	case evUserPromptSubmit:
		return KindPrompt
	case evPreToolUse:
		if question || (v.plan != "" && p.ToolName == v.plan) {
			return KindRequest
		}
		return KindTool
	case evPostToolUse:
		return KindToolDone
	case evPermissionRequest:
		return KindRequest
	case evNotification:
		if p.NotificationType == notificationPermission {
			return KindAttention
		}
	case evStop, evStopFailure:
		return KindStop
	case evSessionEnd:
		return KindSessionEnd
	}
	return ""
}

// firstQuestion reads questions[0] of a question tool input.
func firstQuestion(raw json.RawMessage) (string, []string) {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Questions) == 0 {
		return "", nil
	}
	q := in.Questions[0]
	var opts []string
	for _, o := range q.Options {
		opts = append(opts, o.Label)
	}
	return q.Question, opts
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxInputText {
		return s
	}
	return string(r[:maxInputText-1]) + "…"
}
