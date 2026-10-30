package agentvisor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/internal/store"
)

// EventType is the type of an event.
type EventType string

// Event is one entry in the event log. Seq is global for one visor.
type Event struct {
	Seq     uint64    `json:"seq"`
	Session string    `json:"session,omitempty"`
	At      time.Time `json:"at"`
	Type    EventType `json:"type"`
	// State is the new state of a session.state event.
	State State `json:"state,omitempty"`
	// Turn counts the finished user turns of a wire session.
	Turn     int    `json:"turn,omitempty"`
	VendorID string `json:"vendor_id,omitempty"`
	Text     string `json:"text,omitempty"`
	// Input marks a user event that the visor wrote for a send or a first
	// prompt. A vendor echo of the turn has no mark.
	Input bool `json:"input,omitempty"`
	Delta bool `json:"delta,omitempty"`
	// Status is the activity hint of a status event.
	Status  string   `json:"status,omitempty"`
	Tool    *Tool    `json:"tool,omitempty"`
	Request *Request `json:"request,omitempty"`
	// RequestID names the request of a request.closed event.
	RequestID string  `json:"request_id,omitempty"`
	Result    *Result `json:"result,omitempty"`
	Usage     *Usage  `json:"usage,omitempty"`
	Limits    *Limits `json:"limits,omitempty"`
	Error     string  `json:"error,omitempty"`
	Code      string  `json:"code,omitempty"`
	EndReason string  `json:"end_reason,omitempty"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	// Harness names the harness of a harness.changed event.
	Harness string `json:"harness,omitempty"`
	// Data is the payload of a hook event, or the output of a script.
	Data json.RawMessage `json:"data,omitempty"`
}

// Tool is one tool call, as agent-wire reports it.
type Tool struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind,omitempty"`
	Input    string   `json:"input,omitempty"`
	Output   string   `json:"output,omitempty"`
	Status   string   `json:"status,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
}

// Request is an open permission or question.
type Request struct {
	ID        string   `json:"id"`
	Tool      string   `json:"tool,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Question  string   `json:"question,omitempty"`
	Input     string   `json:"input,omitempty"`
	Options   []string `json:"options,omitempty"`
	ToolUseID string   `json:"tool_use_id,omitempty"`
}

// Result ends a turn.
type Result struct {
	Subtype   string `json:"subtype,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Text      string `json:"text,omitempty"`
	VendorID  string `json:"vendor_id,omitempty"`
	Usage     Usage  `json:"usage"`
	EndReason string `json:"end_reason,omitempty"`
	Code      string `json:"code,omitempty"`
}

// Usage is the token and cost account of a turn, in vendor units.
type Usage struct {
	Input         int64   `json:"input,omitempty"`
	Output        int64   `json:"output,omitempty"`
	CacheRead     int64   `json:"cache_read,omitempty"`
	CacheCreation int64   `json:"cache_creation,omitempty"`
	Model         string  `json:"model,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
}

// EventStream gives the events after a sequence number: first the log, then
// the live events. It continues after a slow read with no gap.
type EventStream struct {
	ch     chan Event
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

// Event types.
const (
	EventSessionState   EventType = "session.state"
	EventInit           EventType = "init"
	EventAssistant      EventType = "assistant"
	EventThought        EventType = "thought"
	EventUser           EventType = "user"
	EventTool           EventType = "tool"
	EventRequest        EventType = "request"
	EventRequestClosed  EventType = "request.closed"
	EventUsage          EventType = "usage"
	EventLimits         EventType = "limits"
	EventStatus         EventType = "status"
	EventResult         EventType = "result"
	EventError          EventType = "error"
	EventExit           EventType = "exit"
	EventHook           EventType = "hook"
	EventScript         EventType = "workdir.script"
	EventHarnessChanged EventType = "harness.changed"
	EventRestarting     EventType = "visor.restarting"
)

const (
	subBuffer  = 4096
	replayPage = 512
)

// errOverflow ends a live subscription that a slow reader let fill up.
var errOverflow = errors.New("event subscription overflow")

// wireEvent converts an agent-wire event. Raw vendor frames stay out.
func wireEvent(e agentwire.Event) Event {
	out := Event{
		At: e.At, Turn: e.Turn, VendorID: e.SessionID, Text: e.Text, Delta: e.Delta, Status: string(e.Status),
		Error: e.Error, Code: e.Code, EndReason: e.EndReason, ExitCode: e.ExitCode,
	}
	switch e.Type {
	case agentwire.EventPermission:
		out.Type = EventRequest
	default:
		out.Type = EventType(e.Type)
	}
	if t := e.Tool; t != nil {
		out.Tool = &Tool{ID: t.ID, Name: t.Name, Kind: string(t.Kind), Input: t.Input, Output: t.Output, Status: t.Status, Paths: t.Paths, ExitCode: t.ExitCode}
	}
	if p := e.Permission; p != nil {
		out.Request = &Request{ID: p.ID, Tool: p.Tool, Kind: string(p.Kind), Question: p.Question, Input: p.Input, Options: p.Options, ToolUseID: p.ToolUseID}
	}
	if r := e.Result; r != nil {
		out.Result = &Result{Subtype: r.Subtype, IsError: r.IsError, Text: r.Text, VendorID: r.SessionID, Usage: usageOf(r.Usage), EndReason: r.EndReason, Code: r.Code}
	}
	if u := e.Usage; u != nil {
		v := usageOf(*u)
		out.Usage = &v
	}
	if l := e.Limits; l != nil {
		out.Limits = &Limits{Plan: l.Plan, Rejected: l.Rejected}
		for _, w := range l.Windows {
			out.Limits.Windows = append(out.Limits.Windows, LimitWindow{Key: w.Key, Scope: w.Scope, Minutes: w.Minutes, UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt})
		}
	}
	return out
}

func usageOf(u agentwire.Usage) Usage {
	return Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation, Model: u.Model, CostUSD: u.CostUSD}
}

// Wire converts the event back to an agent-wire event, so a product can feed
// its agent-wire tracker. ok is false for a visor event type.
func (e Event) Wire() (agentwire.Event, bool) {
	out := agentwire.Event{
		At: e.At, Turn: e.Turn, SessionID: e.VendorID, Text: e.Text, Delta: e.Delta, Status: agentwire.SessionStatus(e.Status),
		Error: e.Error, Code: e.Code, EndReason: e.EndReason, ExitCode: e.ExitCode,
	}
	switch e.Type {
	case EventRequest:
		out.Type = agentwire.EventPermission
	case EventInit, EventAssistant, EventThought, EventUser, EventTool, EventUsage, EventLimits, EventStatus, EventResult, EventError, EventExit:
		out.Type = agentwire.EventType(e.Type)
	default:
		return agentwire.Event{}, false
	}
	if t := e.Tool; t != nil {
		out.Tool = &agentwire.ToolEvent{ID: t.ID, Name: t.Name, Kind: agentwire.ToolKind(t.Kind), Input: t.Input, Output: t.Output, Status: t.Status, Paths: t.Paths, ExitCode: t.ExitCode}
	}
	if p := e.Request; p != nil {
		out.Permission = &agentwire.Permission{ID: p.ID, Tool: p.Tool, Kind: agentwire.ToolKind(p.Kind), Question: p.Question, Input: p.Input, Options: p.Options, ToolUseID: p.ToolUseID}
	}
	if r := e.Result; r != nil {
		out.Result = &agentwire.Result{Subtype: r.Subtype, IsError: r.IsError, Text: r.Text, SessionID: r.VendorID, Usage: r.Usage.wire(), EndReason: r.EndReason, Code: r.Code}
	}
	if u := e.Usage; u != nil {
		w := u.wire()
		out.Usage = &w
	}
	if l := e.Limits; l != nil {
		out.Limits = &agentwire.Limits{Plan: l.Plan, Rejected: l.Rejected}
		for _, w := range l.Windows {
			out.Limits.Windows = append(out.Limits.Windows, agentwire.LimitWindow{Key: w.Key, Scope: w.Scope, Minutes: w.Minutes, UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt})
		}
	}
	return out, true
}

func (u Usage) wire() agentwire.Usage {
	return agentwire.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation, Model: u.Model, CostUSD: u.CostUSD}
}

func newEventStream(ctx context.Context) (*EventStream, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &EventStream{ch: make(chan Event), cancel: cancel, done: make(chan struct{})}, ctx
}

// C gives the events. It closes when the stream ends; Err then tells why.
func (s *EventStream) C() <-chan Event { return s.ch }

// Next waits for the next event.
func (s *EventStream) Next(ctx context.Context) (Event, error) {
	select {
	case e, ok := <-s.ch:
		if !ok {
			if err := s.Err(); err != nil {
				return Event{}, err
			}
			return Event{}, ErrClosed
		}
		return e, nil
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
}

// Err is the reason that the stream ended, or nil.
func (s *EventStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the stream and waits for its goroutine.
func (s *EventStream) Close() error {
	s.cancel()
	<-s.done
	return nil
}

func (s *EventStream) finish(err error) {
	s.mu.Lock()
	if s.err == nil && err != nil && !errors.Is(err, context.Canceled) {
		s.err = err
	}
	s.mu.Unlock()
	close(s.ch)
	close(s.done)
}

// send gives one event to the reader, or false when the stream ends.
func (s *EventStream) send(ctx context.Context, e Event) bool {
	select {
	case s.ch <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// eventLog appends events with a global sequence and fans them out.
type eventLog struct {
	st   *store.Store
	log  *slog.Logger
	mu   sync.Mutex
	seq  uint64
	subs map[*subscriber]struct{}
}

type subscriber struct {
	match func(Event) bool
	ch    chan Event
	err   error
}

func newEventLog(ctx context.Context, st *store.Store, log *slog.Logger) (*eventLog, error) {
	seq, err := st.LastSeq(ctx)
	if err != nil {
		return nil, err
	}
	return &eventLog{st: st, log: log, seq: seq, subs: map[*subscriber]struct{}{}}, nil
}

// append stores the event and gives it to the live readers.
func (l *eventLog) append(ctx context.Context, e Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.Seq = l.seq + 1
	data, err := json.Marshal(e)
	if err != nil {
		return Event{}, fmt.Errorf("encode event: %w", err)
	}
	if err := l.st.AppendEvent(context.WithoutCancel(ctx), store.EventRow{Seq: e.Seq, Session: e.Session, Data: data}); err != nil {
		return Event{}, err
	}
	l.seq = e.Seq
	for sub := range l.subs {
		if !sub.match(e) {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			sub.err = errOverflow
			delete(l.subs, sub)
			close(sub.ch)
		}
	}
	return e, nil
}

func (l *eventLog) head() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

func (l *eventLog) subscribe(match func(Event) bool) (*subscriber, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sub := &subscriber{match: match, ch: make(chan Event, subBuffer)}
	l.subs[sub] = struct{}{}
	return sub, l.seq
}

func (l *eventLog) unsubscribe(sub *subscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.subs[sub]; ok {
		delete(l.subs, sub)
		close(sub.ch)
	}
}

// close ends all live readers.
func (l *eventLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for sub := range l.subs {
		sub.err = ErrClosed
		delete(l.subs, sub)
		close(sub.ch)
	}
}

// stream replays the log after after, then gives the live events. sessions
// narrows the replay query; nil reads all.
func (l *eventLog) stream(ctx context.Context, after uint64, sessions func() []string, match func(Event) bool) *EventStream {
	s, ctx := newEventStream(ctx)
	go func() {
		s.finish(l.pump(ctx, s, after, sessions, match))
	}()
	return s
}

func (l *eventLog) pump(ctx context.Context, s *EventStream, last uint64, sessions func() []string, match func(Event) bool) error {
	for {
		sub, head := l.subscribe(match)
		var err error
		if last, err = l.replay(ctx, s, last, head, sessions(), match); err != nil {
			l.unsubscribe(sub)
			return err
		}
		if last, err = l.live(ctx, s, sub, last); err != nil {
			return err
		}
		l.log.Debug("event reader fell behind; replay from the log", "after", last)
	}
}

// live gives the events of sub until the stream ends. A nil error means
// that sub overflowed and the reader must replay from last.
func (l *eventLog) live(ctx context.Context, s *EventStream, sub *subscriber, last uint64) (uint64, error) {
	for {
		select {
		case e, ok := <-sub.ch:
			if !ok {
				if errors.Is(sub.err, errOverflow) {
					return last, nil
				}
				return last, cmp.Or(sub.err, ErrClosed)
			}
			if e.Seq <= last {
				continue
			}
			if !s.send(ctx, e) {
				l.unsubscribe(sub)
				return last, ctx.Err()
			}
			last = e.Seq
		case <-ctx.Done():
			l.unsubscribe(sub)
			return last, ctx.Err()
		}
	}
}

func (l *eventLog) replay(ctx context.Context, s *EventStream, after, until uint64, sessions []string, match func(Event) bool) (uint64, error) {
	for after < until {
		rows, err := l.st.Events(ctx, after, until, sessions, replayPage)
		if err != nil {
			return after, err
		}
		if len(rows) == 0 {
			return until, nil
		}
		for _, r := range rows {
			var e Event
			if err := json.Unmarshal(r.Data, &e); err != nil {
				return after, fmt.Errorf("decode event %d: %w", r.Seq, err)
			}
			if match(e) && !s.send(ctx, e) {
				return after, ctx.Err()
			}
			after = r.Seq
		}
	}
	return after, nil
}
