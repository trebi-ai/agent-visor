package orca

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trebi-ai/agent-wire/tui"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// The keys that the handle types.
const (
	keyEscape = "\x1b"
	keyEnter  = "\r"
)

// sendTries and sendPause retry a prompt that Orca did not take.
const (
	sendTries = 3
	sendPause = 3 * time.Second
)

// handle is one agent session in an Orca terminal. Only the poll sends on
// events and closes it.
type handle struct {
	b      *Backend
	id     string
	events chan agentvisor.Event

	mu      sync.Mutex
	bd      binding
	sent    string // the last prompt of Send; the card echo of it is no user event
	last    Agent
	seen    bool // the agent card showed once
	idle    bool // the agent waits for its first prompt
	away    int  // polls with no agent card after it showed
	gone    int  // polls with no terminal
	closing bool
	born    time.Time
}

func (h *handle) Session() string                 { return h.id }
func (h *handle) VendorID() string                { return "" }
func (h *handle) PID() int                        { return 0 }
func (h *handle) Events() <-chan agentvisor.Event { return h.events }

// Labels gives the Orca terminal of the session.
func (h *handle) Labels() map[string]string {
	bd := h.binding()
	out := map[string]string{}
	for k, v := range map[string]string{LabelPane: bd.Pane, LabelTerminal: bd.Terminal, LabelWorktree: bd.Worktree} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// Send types the text as a prompt. It tries again while Orca says that the
// runtime is not ready.
func (h *handle) Send(ctx context.Context, in agentvisor.Input) (agentvisor.Delivery, error) {
	t, err := h.terminal()
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	h.sent, h.idle = strings.TrimSpace(in.Text), false
	h.mu.Unlock()
	for i := 1; ; i++ {
		_, err := h.b.cli.Submit(ctx, t, in.Text)
		if err == nil || i == sendTries || !IsCode(err, "runtime_unavailable") {
			if err != nil {
				return "", err
			}
			h.b.poke()
			return agentvisor.DeliveryPrompted, nil
		}
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(sendPause):
		}
	}
}

// Interrupt types Escape, which stops the turn of the agent CLIs.
func (h *handle) Interrupt(ctx context.Context) error {
	return h.Keys(ctx, []byte(keyEscape))
}

// Answer decides the permission prompt on the screen. Allow picks the first
// choice. Deny types Escape, then the message as the next prompt.
func (h *handle) Answer(ctx context.Context, _ string, a agentvisor.Answer) error {
	t, err := h.terminal()
	if err != nil {
		return err
	}
	rows, err := h.b.cli.Screen(ctx, t)
	if err != nil {
		return err
	}
	p, ok := tui.ParsePrompt(rows)
	if !ok || len(p.Options) == 0 {
		return fmt.Errorf("%w: no prompt on the screen of session %s", agentvisor.ErrState, h.id)
	}
	if a.Allow {
		return h.b.cli.Type(ctx, t, strconv.Itoa(p.Options[0].N))
	}
	if err := h.b.cli.Type(ctx, t, keyEscape); err != nil {
		return err
	}
	if msg := strings.Join(strings.Fields(a.Message), " "); msg != "" {
		return h.b.cli.Line(ctx, t, msg)
	}
	return nil
}

// Close closes the Orca terminal. The poll then ends the session.
func (h *handle) Close(ctx context.Context) error {
	t := h.binding().Terminal
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()
	defer h.b.poke()
	if t == "" {
		return nil
	}
	if err := h.b.cli.Close(ctx, t); err != nil && !IsCode(err, "terminal_not_found") && !IsCode(err, "terminal_handle_stale") {
		return err
	}
	return nil
}

// Screen gives the rows that the Orca terminal renders.
func (h *handle) Screen(ctx context.Context) (agentvisor.Screen, error) {
	t, err := h.terminal()
	if err != nil {
		return agentvisor.Screen{}, err
	}
	rows, err := h.b.cli.Screen(ctx, t)
	if err != nil {
		return agentvisor.Screen{}, err
	}
	cols := 0
	for _, r := range rows {
		cols = max(cols, len([]rune(r)))
	}
	return agentvisor.Screen{Cols: cols, Rows: len(rows), Lines: rows}, nil
}

// Keys types data verbatim.
func (h *handle) Keys(ctx context.Context, data []byte) error {
	t, err := h.terminal()
	if err != nil {
		return err
	}
	return h.b.cli.Type(ctx, t, string(data))
}

// Paste types text verbatim, with no return.
func (h *handle) Paste(ctx context.Context, text string) error {
	return h.Keys(ctx, []byte(text))
}

// Output gives the last lines of the terminal output.
func (h *handle) Output(ctx context.Context) (string, error) {
	t, err := h.terminal()
	if err != nil {
		return "", err
	}
	lines, err := h.b.cli.Tail(ctx, t, 500)
	if err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

// see gives the events of one poll. ok is false when the agent has no card.
// It is true in end when the session ended.
func (h *handle) see(a Agent, ok bool, t *Terminal) (events []agentvisor.Event, end bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if events, end = h.seeTerminal(t); end {
		return nil, true
	}
	if !ok {
		return h.seeNoCard(events)
	}
	h.away = 0
	a = a.Effective()
	prev, seen := h.last, h.seen
	h.last, h.seen = a, true
	if seen && a.State == prev.State && a.StateStartedAt == prev.StateStartedAt && a.ToolName == prev.ToolName && a.Prompt == prev.Prompt {
		return events, false
	}
	if p := strings.TrimSpace(a.Prompt); p != "" && p != strings.TrimSpace(prev.Prompt) {
		if p != h.sent {
			events = append(events, agentvisor.Event{Type: agentvisor.EventUser, Text: a.Prompt})
		}
		h.sent = ""
	}
	return append(events, stateEvents(a, prev, seen)...), false
}

// seeTerminal follows the terminal of the session. The caller holds h.mu.
func (h *handle) seeTerminal(t *Terminal) ([]agentvisor.Event, bool) {
	if t == nil {
		h.gone++
		// A new terminal can miss in the first list.
		return nil, h.closing || h.gone >= gonePolls && time.Since(h.born) > newGrace
	}
	h.gone = 0
	if bd := h.bd; bd.Terminal != t.Handle || bd.Worktree == "" && t.WorktreeID != "" {
		h.bd.Terminal, h.bd.Worktree = t.Handle, cmp.Or(bd.Worktree, t.WorktreeID)
		return []agentvisor.Event{{Type: agentvisor.EventInit}}, false
	}
	return nil, false
}

// seeNoCard handles a poll with no agent card. The caller holds h.mu.
func (h *handle) seeNoCard(events []agentvisor.Event) ([]agentvisor.Event, bool) {
	if h.idle && !h.seen {
		h.idle = false
		events = append(events, agentvisor.Event{Type: agentvisor.EventStatus, Status: "idle"})
	}
	if h.seen {
		h.away++
		if h.away >= endPolls {
			return append(events, agentvisor.Event{Type: agentvisor.EventExit}), true
		}
	}
	return events, false
}

// stateEvents turns a change of the agent card into visor events.
func stateEvents(a, prev Agent, seen bool) []agentvisor.Event {
	same := seen && a.State == prev.State && a.StateStartedAt == prev.StateStartedAt
	switch a.State {
	case AgentWorking:
		var out []agentvisor.Event
		if !same {
			out = append(out, agentvisor.Event{Type: agentvisor.EventStatus, Status: "running"})
		}
		if a.ToolName != "" && (a.ToolName != prev.ToolName || !same) {
			out = append(out, agentvisor.Event{Type: agentvisor.EventTool, Tool: &agentvisor.Tool{Name: a.ToolName, Input: a.ToolInput}})
		}
		return out
	case AgentBlocked, AgentWaiting:
		if same {
			return nil
		}
		r := agentvisor.Request{ID: "orca-" + strconv.FormatInt(a.StateStartedAt, 10), Tool: a.ToolName, Input: a.ToolInput, Kind: "question"}
		if a.State == AgentBlocked {
			r.Kind = "permission"
		} else {
			r.Question = a.LastAssistantMessage
		}
		return []agentvisor.Event{{Type: agentvisor.EventRequest, Request: &r}}
	case AgentDone:
		if same {
			return nil
		}
		if !seen {
			// The turn ended before the session came: no result.
			return []agentvisor.Event{{Type: agentvisor.EventStatus, Status: "idle"}}
		}
		res := agentvisor.Result{Subtype: "success", Text: a.LastAssistantMessage}
		if a.Interrupted {
			res.Subtype = "interrupted"
		}
		return []agentvisor.Event{{Type: agentvisor.EventResult, Result: &res}}
	}
	return nil
}

func (h *handle) binding() binding {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bd
}

func (h *handle) terminal() (string, error) {
	if t := h.binding().Terminal; t != "" {
		return t, nil
	}
	return "", fmt.Errorf("%w: session %s has no orca terminal yet", agentvisor.ErrState, h.id)
}

// firstPrompt waits until the agent is idle, then sends the prompt.
func (h *handle) firstPrompt(ctx context.Context, text string) error {
	t, err := h.terminal()
	if err != nil {
		return err
	}
	ok := false
	for range 2 {
		// A cold start of the agent can take longer than one wait.
		if ok, err = h.b.cli.WaitIdle(ctx, t); ok || ctx.Err() != nil {
			break
		}
	}
	if !ok {
		return errors.Join(fmt.Errorf("%w: the agent did not start in orca", agentvisor.ErrState), err, ctx.Err())
	}
	_, err = h.Send(ctx, agentvisor.Input{Text: text})
	return err
}
