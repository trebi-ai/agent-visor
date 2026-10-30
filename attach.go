package agentvisor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// AttachView is what an attachment shows.
type AttachView string

// AttachOptions configures an attachment. The empty value attaches the
// terminal of a session that has one, else the chat.
type AttachOptions struct {
	View AttachView `json:"view,omitempty"`
	// Cols and Rows are the size of the client. A client that gives a size
	// takes the size lease when nobody holds it.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// After is the event sequence after which the chat view starts.
	After uint64 `json:"after,omitempty"`
}

// ClientFrameType is the type of a frame from an attach client.
type ClientFrameType string

// ClientFrame is one frame from an attach client. Each is a JSON frame,
// except input, which is a binary frame with op OpInput.
type ClientFrame struct {
	Type ClientFrameType `json:"type"`
	// View, Cols, Rows, and After belong to attach. Cols and Rows also
	// belong to control.
	View  AttachView `json:"view,omitempty"`
	Cols  int        `json:"cols,omitempty"`
	Rows  int        `json:"rows,omitempty"`
	After *uint64    `json:"after,omitempty"`
	// Seq is the binary sequence of an ack.
	Seq uint64 `json:"seq,omitempty"`
	// Text is the text of a send.
	Text string `json:"text,omitempty"`
	// Data is the bytes of an input.
	Data []byte `json:"-"`
}

// FrameType is the type of a JSON frame to an attach client.
type FrameType string

// FrameOp is the op of a binary frame: op (1 byte) | seq (8 bytes, big
// endian) | data.
type FrameOp byte

// LeaseHolder tells a client who holds the size lease.
type LeaseHolder string

// EndReason tells why an attachment ended.
type EndReason string

// TurnRole is the role of one turn of the chat view.
type TurnRole string

// Turn is one turn of the chat view. A turn that changes comes again with
// the same Seq.
type Turn struct {
	Seq  uint64    `json:"seq"`
	Role TurnRole  `json:"role"`
	At   time.Time `json:"at"`
	Text string    `json:"text,omitempty"`
	Tool *Tool     `json:"tool,omitempty"`
}

// Frame is one frame to an attach client: a binary frame when Op is not
// zero, else a JSON frame of Type.
type Frame struct {
	Type FrameType `json:"type"`
	// Op, Seq, and Data belong to a binary frame. Seq counts the binary
	// bytes that the attachment sent, and an ack gives it back.
	Op   FrameOp `json:"-"`
	Seq  uint64  `json:"-"`
	Data []byte  `json:"-"`
	// View, Source, Writable, Resizable, and Title belong to attached.
	View      AttachView `json:"view,omitempty"`
	Source    Kind       `json:"source,omitempty"`
	Cols      int        `json:"cols,omitempty"`
	Rows      int        `json:"rows,omitempty"`
	Writable  bool       `json:"writable,omitempty"`
	Resizable bool       `json:"resizable,omitempty"`
	Title     string     `json:"title,omitempty"`
	// Holder belongs to lease.
	Holder LeaseHolder `json:"holder,omitempty"`
	// From, To, and Turns belong to turns.
	From  uint64 `json:"from,omitempty"`
	To    uint64 `json:"to,omitempty"`
	Turns []Turn `json:"turns,omitempty"`
	// Reason belongs to ended.
	Reason EndReason `json:"reason,omitempty"`
	// Code and Message belong to error.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// AttachStream is one client on the terminal or the chat of a session. One
// goroutine calls Recv; others can call Handle, Input, and Close.
type AttachStream struct {
	// remote is set on an attachment of a Client.
	remote  *remoteAttach
	view    AttachView
	session string
	v       *Visor
	term    *ptyHandle
	stream  *EventStream
	turns   turnBuilder
	// endAt is the last event of an exited session, for the chat view.
	endAt uint64
	wake  chan struct{}

	mu     sync.Mutex
	queue  []Frame
	buf    []byte
	stale  bool
	ending EndReason
	ended  bool
	closed bool
	sent   uint64
	acked  uint64
	acking bool
}

// turnBuilder makes chat turns from events.
type turnBuilder struct {
	// open is the assistant turn that deltas add to.
	open  *Turn
	tools map[string]*Turn
}

// Attach views.
const (
	ViewTerminal AttachView = "terminal"
	ViewChat     AttachView = "chat"
)

// Client frame types.
const (
	ClientAttach    ClientFrameType = "attach"
	ClientAck       ClientFrameType = "ack"
	ClientControl   ClientFrameType = "control"
	ClientRelease   ClientFrameType = "release"
	ClientSend      ClientFrameType = "send"
	ClientInterrupt ClientFrameType = "interrupt"
	ClientInput     ClientFrameType = "input"
)

// Frame types.
const (
	FrameAttached FrameType = "attached"
	FrameResized  FrameType = "resized"
	FrameLease    FrameType = "lease"
	FrameTurns    FrameType = "turns"
	FrameEnded    FrameType = "ended"
	FrameError    FrameType = "error"
)

// Binary frame ops.
const (
	OpSnapshot FrameOp = 1
	OpOutput   FrameOp = 2
	OpInput    FrameOp = 3
)

// Lease holders, as one client sees them.
const (
	LeaseYou   LeaseHolder = "you"
	LeaseOther LeaseHolder = "other"
	LeaseNone  LeaseHolder = "none"
)

// End reasons.
const (
	EndExited EndReason = "exited"
	EndClosed EndReason = "closed"
)

// Turn roles.
const (
	TurnUser      TurnRole = "user"
	TurnAssistant TurnRole = "assistant"
	TurnTool      TurnRole = "tool"
	TurnResult    TurnRole = "result"
)

// Flow rules of one attachment.
const (
	// MaxInFlight is the binary bytes that a client that acks can have
	// without an ack. A client that never acks has no limit.
	MaxInFlight = 512 << 10
	// maxBuffered is the output that waits for a slow client. More makes
	// the attachment stale, and the client gets a new snapshot.
	maxBuffered    = 1 << 20
	maxOutputFrame = 64 << 10
	maxTurnBatch   = 64
	binaryHeader   = 9
)

// DecodeClientFrame reads one frame from an attach client.
func DecodeClientFrame(isBinary bool, data []byte) (ClientFrame, error) {
	if isBinary {
		op, _, p, err := decodeBinary(data)
		if err != nil {
			return ClientFrame{}, err
		}
		if op != OpInput {
			return ClientFrame{}, fmt.Errorf("%w: a client sends only input in a binary frame", ErrInvalid)
		}
		return ClientFrame{Type: ClientInput, Data: p}, nil
	}
	var f ClientFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return ClientFrame{}, fmt.Errorf("%w: decode frame: %w", ErrInvalid, err)
	}
	switch f.Type {
	case ClientAttach, ClientAck, ClientControl, ClientRelease, ClientSend, ClientInterrupt:
		return f, nil
	}
	return ClientFrame{}, fmt.Errorf("%w: unknown frame type %q", ErrInvalid, f.Type)
}

// Encode gives the frame as it goes on the socket.
func (f ClientFrame) Encode() (isBinary bool, data []byte, err error) {
	if f.Type == ClientInput {
		return true, encodeBinary(OpInput, 0, f.Data), nil
	}
	data, err = json.Marshal(f)
	return false, data, err
}

// Options gives the attach options of an attach frame.
func (f ClientFrame) Options() AttachOptions {
	o := AttachOptions{View: f.View, Cols: f.Cols, Rows: f.Rows}
	if f.After != nil {
		o.After = *f.After
	}
	return o
}

// DecodeFrame reads one frame to an attach client.
func DecodeFrame(isBinary bool, data []byte) (Frame, error) {
	if isBinary {
		op, seq, p, err := decodeBinary(data)
		if err != nil {
			return Frame{}, err
		}
		return Frame{Op: op, Seq: seq, Data: p}, nil
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, fmt.Errorf("%w: decode frame: %w", ErrInvalid, err)
	}
	return f, nil
}

// Binary reports whether the frame is a binary frame.
func (f Frame) Binary() bool { return f.Op != 0 }

// Encode gives the frame as it goes on the socket.
func (f Frame) Encode() ([]byte, error) {
	if f.Binary() {
		return encodeBinary(f.Op, f.Seq, f.Data), nil
	}
	return json.Marshal(f)
}

// Attach attaches a client to the terminal or the chat of a session. The
// attachment lives until Close, the end of the session, or the end of the
// visor.
func (v *Visor) Attach(_ context.Context, id string, o AttachOptions) (*AttachStream, error) {
	rec, h, err := v.lookup(id)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	s := rec.s.clone()
	v.mu.Unlock()
	caps := v.caps(s)
	view := o.View
	if view == "" {
		view = ViewChat
		if caps.Terminal {
			view = ViewTerminal
		}
	}
	a := &AttachStream{view: view, session: id, v: v, wake: make(chan struct{}, 1)}
	switch view {
	case ViewTerminal:
		ph, ok := h.(*ptyHandle)
		if !ok {
			if h == nil {
				return nil, fmt.Errorf("%w: session %s has no process", ErrState, id)
			}
			return nil, fmt.Errorf("%w: session %s has no terminal", ErrUnsupported, id)
		}
		a.term = ph
		if err := ph.join(a, o, s.Title); err != nil {
			return nil, err
		}
	case ViewChat:
		if !caps.Structured {
			return nil, fmt.Errorf("%w: session %s has no chat", ErrUnsupported, id)
		}
		a.queue = []Frame{{Type: FrameAttached, View: ViewChat, Source: s.Kind, Writable: true, Title: s.Title}}
		if s.State == StateExited {
			a.endAt = s.Seq
			if o.After >= s.Seq {
				a.ending = EndExited
			}
		}
		match := func(e Event) bool { return e.Session == id }
		a.stream = v.events.stream(v.ctx, o.After, func() []string { return []string{id} }, match)
	default:
		return nil, fmt.Errorf("%w: view %q", ErrInvalid, view)
	}
	return a, nil
}

// View is the view of the attachment.
func (a *AttachStream) View() AttachView { return a.view }

// Recv waits for the next frame to the client. After the ended frame, it
// returns io.EOF.
func (a *AttachStream) Recv(ctx context.Context) (Frame, error) {
	if a.remote != nil {
		return a.remote.recv(ctx)
	}
	if a.view == ViewChat {
		return a.recvChat(ctx)
	}
	for {
		f, ok, resync, err := a.next()
		if err != nil || ok {
			return f, err
		}
		if resync {
			a.term.resnapshot(a)
			continue
		}
		select {
		case <-a.wake:
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

// Handle applies one frame from the client. A failed frame also queues an
// error frame, so a server can ignore each error except ErrClosed. On a
// Client, Handle only sends the frame, and a failure comes as an error frame.
func (a *AttachStream) Handle(ctx context.Context, f ClientFrame) error {
	if a.remote != nil {
		return a.remote.send(ctx, f)
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return ErrClosed
	}
	err := a.handle(ctx, f)
	if err != nil {
		a.push(errorFrame(err))
	}
	return err
}

// Input writes bytes to the terminal.
func (a *AttachStream) Input(ctx context.Context, p []byte) error {
	return a.Handle(ctx, ClientFrame{Type: ClientInput, Data: p})
}

// Close ends the attachment. It gives back the size lease.
func (a *AttachStream) Close() error {
	if a.remote != nil {
		return a.remote.close()
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()
	a.signal()
	if a.term != nil {
		a.term.leave(a)
	}
	if a.stream != nil {
		return a.stream.Close()
	}
	return nil
}

func (a *AttachStream) handle(ctx context.Context, f ClientFrame) error {
	switch f.Type {
	case ClientAck:
		a.mu.Lock()
		a.acking, a.acked = true, max(a.acked, f.Seq)
		a.mu.Unlock()
		a.signal()
		return nil
	case ClientAttach:
		return fmt.Errorf("%w: the client is attached already", ErrInvalid)
	}
	if a.view == ViewChat {
		switch f.Type {
		case ClientSend:
			_, err := a.v.Send(ctx, a.session, Input{Text: f.Text})
			return err
		case ClientInterrupt:
			return a.v.Interrupt(ctx, a.session)
		}
		return fmt.Errorf("%w: the chat view takes no %s frame", ErrUnsupported, f.Type)
	}
	switch f.Type {
	case ClientInput:
		return a.term.write(f.Data)
	case ClientSend:
		return a.term.write([]byte(f.Text))
	case ClientInterrupt:
		return a.term.write([]byte{0x03})
	case ClientControl:
		if f.Cols <= 0 || f.Rows <= 0 {
			return fmt.Errorf("%w: control needs cols and rows", ErrInvalid)
		}
		return a.term.take(a, f.Cols, f.Rows)
	case ClientRelease:
		a.term.release(a)
		return nil
	}
	return fmt.Errorf("%w: frame type %q", ErrInvalid, f.Type)
}

// next takes the next terminal frame. resync means that the attachment
// needs a new snapshot first.
func (a *AttachStream) next() (f Frame, ok, resync bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	flow := !a.acking || a.sent-min(a.acked, a.sent) < MaxInFlight
	switch {
	case a.closed:
		return f, false, false, ErrClosed
	case len(a.queue) > 0 && (!a.queue[0].Binary() || flow):
		f, a.queue = a.queue[0], a.queue[1:]
		if f.Binary() {
			a.sent += uint64(len(f.Data))
			f.Seq = a.sent
		}
		return f, true, false, nil
	case a.stale:
		return f, false, flow, nil
	case len(a.buf) > 0:
		if !flow {
			return f, false, false, nil
		}
		n := min(len(a.buf), maxOutputFrame)
		data := append([]byte(nil), a.buf[:n]...)
		a.buf = a.buf[n:]
		a.sent += uint64(n)
		return Frame{Op: OpOutput, Seq: a.sent, Data: data}, true, false, nil
	case a.ending != "" && len(a.queue) == 0:
		f = Frame{Type: FrameEnded, Reason: a.ending}
		a.ending, a.ended = "", true
		return f, true, false, nil
	case a.ended:
		return f, false, false, io.EOF
	}
	return f, false, false, nil
}

// recvChat gives the next frame of the chat view. It batches the events
// that are ready into one turns frame.
func (a *AttachStream) recvChat(ctx context.Context) (Frame, error) {
	for {
		a.mu.Lock()
		switch {
		case a.closed:
			a.mu.Unlock()
			return Frame{}, ErrClosed
		case len(a.queue) > 0:
			f := a.queue[0]
			a.queue = a.queue[1:]
			a.mu.Unlock()
			return f, nil
		case a.ending != "":
			reason := a.ending
			a.ending, a.ended = "", true
			a.mu.Unlock()
			return Frame{Type: FrameEnded, Reason: reason}, nil
		case a.ended:
			a.mu.Unlock()
			return Frame{}, io.EOF
		}
		a.mu.Unlock()
		e, err := a.stream.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return Frame{}, ctx.Err()
			}
			a.end(EndClosed)
			continue
		}
		if f, ok := a.chatBatch(e); ok {
			return f, nil
		}
	}
}

// chatBatch adds e and the events that are ready, and gives the turns that
// changed.
func (a *AttachStream) chatBatch(e Event) (Frame, bool) {
	var changed []Turn
	add := func(e Event) {
		if t, ok := a.turns.add(e); ok {
			changed = mergeTurn(changed, t)
		}
		if (e.Type == EventSessionState && e.State == StateExited) || (a.endAt > 0 && e.Seq >= a.endAt) {
			a.end(EndExited)
		}
	}
	add(e)
	for len(changed) < maxTurnBatch {
		select {
		case e, ok := <-a.stream.C():
			if !ok {
				a.end(EndClosed)
				break
			}
			add(e)
			continue
		default:
		}
		break
	}
	if len(changed) == 0 {
		return Frame{}, false
	}
	return Frame{Type: FrameTurns, From: changed[0].Seq, To: changed[len(changed)-1].Seq, Turns: changed}, true
}

// push queues a JSON frame.
func (a *AttachStream) push(f Frame) {
	a.mu.Lock()
	a.queue = append(a.queue, f)
	a.mu.Unlock()
	a.signal()
}

// output queues terminal output.
func (a *AttachStream) output(p []byte) {
	a.mu.Lock()
	switch {
	case a.stale:
	case len(a.buf)+len(p) > maxBuffered:
		a.buf, a.stale = nil, true
	default:
		a.buf = append(a.buf, p...)
	}
	a.mu.Unlock()
	a.signal()
}

// resync replaces the queued output with a snapshot.
func (a *AttachStream) resync(snapshot []byte) {
	a.mu.Lock()
	a.buf, a.stale = nil, false
	a.queue = append(a.queue, Frame{Op: OpSnapshot, Data: snapshot})
	a.mu.Unlock()
	a.signal()
}

func (a *AttachStream) markStale() {
	a.mu.Lock()
	a.buf, a.stale = nil, true
	a.mu.Unlock()
	a.signal()
}

func (a *AttachStream) end(reason EndReason) {
	a.mu.Lock()
	if !a.ended && a.ending == "" {
		a.ending = reason
	}
	a.mu.Unlock()
	a.signal()
}

func (a *AttachStream) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// add gives the turn that e makes or changes.
func (b *turnBuilder) add(e Event) (Turn, bool) {
	switch e.Type {
	case EventUser:
		b.open = nil
		return Turn{Seq: e.Seq, Role: TurnUser, At: e.At, Text: e.Text}, true
	case EventAssistant:
		switch {
		case b.open != nil && e.Delta:
			b.open.Text += e.Text
		case b.open != nil:
			b.open.Text = e.Text
		default:
			b.open = &Turn{Seq: e.Seq, Role: TurnAssistant, At: e.At, Text: e.Text}
		}
		t := *b.open
		if !e.Delta {
			b.open = nil
		}
		return t, true
	case EventTool:
		if e.Tool == nil {
			return Turn{}, false
		}
		b.open = nil
		if b.tools == nil {
			b.tools = map[string]*Turn{}
		}
		tool := *e.Tool
		t, ok := b.tools[tool.ID]
		if !ok || tool.ID == "" {
			t = &Turn{Seq: e.Seq, Role: TurnTool, At: e.At}
			b.tools[tool.ID] = t
		}
		t.Text, t.Tool = tool.Name, &tool
		return *t, true
	case EventResult:
		b.open, b.tools = nil, nil
		t := Turn{Seq: e.Seq, Role: TurnResult, At: e.At}
		if e.Result != nil {
			t.Text = e.Result.Text
		}
		return t, true
	}
	return Turn{}, false
}

// mergeTurn adds t to the list, or replaces the turn with the same Seq.
func mergeTurn(turns []Turn, t Turn) []Turn {
	for i := range turns {
		if turns[i].Seq == t.Seq {
			turns[i] = t
			return turns
		}
	}
	return append(turns, t)
}

func errorFrame(err error) Frame {
	code, _ := errorCode(err)
	return Frame{Type: FrameError, Code: code, Message: err.Error()}
}

func encodeBinary(op FrameOp, seq uint64, data []byte) []byte {
	out := make([]byte, binaryHeader+len(data))
	out[0] = byte(op)
	binary.BigEndian.PutUint64(out[1:binaryHeader], seq)
	copy(out[binaryHeader:], data)
	return out
}

func decodeBinary(b []byte) (FrameOp, uint64, []byte, error) {
	if len(b) < binaryHeader {
		return 0, 0, nil, fmt.Errorf("%w: a binary frame of %d bytes", ErrInvalid, len(b))
	}
	op := FrameOp(b[0])
	if op < OpSnapshot || op > OpInput {
		return 0, 0, nil, fmt.Errorf("%w: binary op %d", ErrInvalid, op)
	}
	return op, binary.BigEndian.Uint64(b[1:binaryHeader]), b[binaryHeader:], nil
}
