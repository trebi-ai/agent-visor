// Package hooks reads the hook calls of vendor agent CLIs and edits their
// hook config files. It does not import the visor.
package hooks

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"

	agentwire "github.com/trebi-ai/agent-wire"
)

// vendor holds the hook facts of one harness. Each fact here is a candidate
// to move to agent-wire.
type vendor struct {
	// config is the hook config file, relative to the vendor home.
	config string
	// trust is the file that keeps the hook trust, or empty when the vendor
	// runs a new hook with no trust step.
	trust string
	// table is the hook table that the visor installs.
	table table
	// questions are the tools that ask the user a question.
	questions []string
	// plan is the tool that asks the user to approve a plan, or empty.
	plan string
}

// hookEvent is one vendor event that the visor hooks. Matcher is a tool name
// pattern, or empty for all tools.
type hookEvent struct{ Name, Matcher string }

// Vendor hook event names. Claude Code and Codex use the same names.
const (
	evSessionStart      = "SessionStart"
	evUserPromptSubmit  = "UserPromptSubmit"
	evPreToolUse        = "PreToolUse"
	evPostToolUse       = "PostToolUse"
	evPermissionRequest = "PermissionRequest"
	evNotification      = "Notification"
	evStop              = "Stop"
	evStopFailure       = "StopFailure"
	evSessionEnd        = "SessionEnd"
)

// Vendor payload values.
const (
	sourceCompact          = "compact"
	notificationPermission = "permission_prompt"
)

// hookTimeout is the timeout of each installed hook, in seconds.
const hookTimeout = 5

// vendors is the table of vendor hook facts.
//
//nolint:gochecknoglobals // a constant table of vendor facts
var vendors = map[string]vendor{
	string(agentwire.Claude): {
		config: "settings.json",
		table: table{timeout: hookTimeout, events: []hookEvent{
			{Name: evSessionStart},
			{Name: evUserPromptSubmit},
			{Name: evPreToolUse, Matcher: "AskUserQuestion|ExitPlanMode"},
			{Name: evPostToolUse, Matcher: "AskUserQuestion|ExitPlanMode"},
			{Name: evPermissionRequest},
			{Name: evNotification},
			{Name: evStop},
			{Name: evStopFailure},
			{Name: evSessionEnd},
		}},
		questions: []string{"AskUserQuestion"}, plan: "ExitPlanMode",
	},
	// Codex runs a new hook only after the user trusts it in Codex. The
	// visor never writes that trust.
	string(agentwire.Codex): {
		config: "hooks.json", trust: "config.toml",
		table: table{timeout: hookTimeout, events: []hookEvent{
			{Name: evSessionStart},
			{Name: evUserPromptSubmit},
			{Name: evPreToolUse, Matcher: "request_user_input"},
			{Name: evPermissionRequest},
			{Name: evStop},
		}},
		questions: []string{"request_user_input"},
	},
}

// Names lists the harnesses with hook support, sorted.
func Names() []string {
	out := make([]string, 0, len(vendors))
	for n := range vendors {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Supported reports whether the harness has hook support.
func Supported(harness string) bool {
	_, ok := vendors[harness]
	return ok
}

// ConfigPath gives the hook config file in the vendor home dir.
func ConfigPath(harness, dir string) (string, error) {
	v, err := lookup(harness)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, v.config), nil
}

func lookup(harness string) (vendor, error) {
	v, ok := vendors[harness]
	if !ok {
		return vendor{}, fmt.Errorf("%w: harness %q", ErrUnsupported, harness)
	}
	return v, nil
}

func (v vendor) question(tool string) bool { return slices.Contains(v.questions, tool) }
