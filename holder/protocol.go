package holder

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
)

// MsgType is the type of one message on the holder socket.
type MsgType byte

// Hello is the first message of each side. The holder sends its hello
// first, and the client answers with the protocol of the holder.
type Hello struct {
	Protocol int    `json:"protocol"`
	Mode     Mode   `json:"mode,omitempty"`
	ID       string `json:"id,omitempty"`
	// PID is the holder process, and ChildPID the process in the PTY or the
	// vendor process.
	PID       int       `json:"pid,omitempty"`
	ChildPID  int       `json:"child_pid,omitempty"`
	Cols      int       `json:"cols,omitempty"`
	Rows      int       `json:"rows,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	// Run names one run of a wire holder. Its event seq starts at 1.
	Run      string `json:"run,omitempty"`
	VendorID string `json:"vendor_id,omitempty"`
	// Status and Pending are the activity and the open requests of a wire
	// session.
	Status  agentwire.SessionStatus `json:"status,omitempty"`
	Pending []agentwire.Permission  `json:"pending,omitempty"`
	// After is the last event seq of Run that the client has. The holder
	// sends the events after it.
	After uint64 `json:"after,omitempty"`
}

// Mode is the kind of a holder.
type Mode string

// Cursor is the last event of a wire holder run that a client has.
type Cursor struct {
	Run string
	Seq uint64
}

// WireEvent is one agent-wire event with the seq of its holder run.
type WireEvent struct {
	Seq   uint64          `json:"seq"`
	Event agentwire.Event `json:"event"`
}

// Op is the operation of a call to a wire holder.
type Op string

// Call is one operation on the session of a wire holder.
type Call struct {
	ID        uint64              `json:"id"`
	Op        Op                  `json:"op"`
	Prompt    *agentwire.Prompt   `json:"prompt,omitempty"`
	RequestID string              `json:"request_id,omitempty"`
	Decision  *agentwire.Decision `json:"decision,omitempty"`
}

// Reply is the result of a call. Code CodeEnding means that the holder ends
// at idle and takes no new call.
type Reply struct {
	ID       uint64             `json:"id"`
	Delivery agentwire.Delivery `json:"delivery,omitempty"`
	Error    string             `json:"error,omitempty"`
	Code     string             `json:"code,omitempty"`
}

// Size is a terminal size in cells.
type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Exit is the end of the process. Code is 128 plus the signal number when a
// signal ended it. Idle means that a wire holder closed its session at the
// end of a turn, and Last is the seq of its last event.
type Exit struct {
	Code  int    `json:"code"`
	Idle  bool   `json:"idle,omitempty"`
	Last  uint64 `json:"last,omitempty"`
	Error string `json:"error,omitempty"`
}

// Msg is one message from the holder.
type Msg struct {
	Type MsgType
	// Data is the screen buffer of a snapshot, or the bytes of an output.
	Data  []byte
	Size  Size
	Exit  Exit
	Event WireEvent
	Reply Reply
}

// Client is one connection of a visor to a holder.
type Client struct {
	conn  net.Conn
	r     *bufio.Reader
	hello Hello
	wmu   sync.Mutex
}

// Protocol is the version of the holder protocol of this build. Version 2
// adds the wire mode. MinProtocol is the oldest holder that a client speaks
// to.
const (
	Protocol    = 2
	MinProtocol = 1
)

// Modes.
const (
	ModePTY  Mode = ""
	ModeWire Mode = "wire"
)

// Ops.
const (
	OpSend      Op = "send"
	OpInterrupt Op = "interrupt"
	OpAnswer    Op = "answer"
	OpClose     Op = "close"
)

// CodeEnding is the reply code of a holder that ends at idle.
const CodeEnding = "ending"

// Messages. Each is type (1 byte) | length (4 bytes, big endian) | payload.
const (
	MsgHello    MsgType = 1 // both ways: JSON Hello
	MsgSnapshot MsgType = 2 // holder: the screen buffer
	MsgOutput   MsgType = 3 // holder: new output
	MsgResized  MsgType = 4 // holder: JSON Size
	MsgExit     MsgType = 5 // holder: JSON Exit
	MsgInput    MsgType = 6 // client: bytes for the PTY
	MsgResize   MsgType = 7 // client: JSON Size
	MsgKill     MsgType = 8 // client: end the process
	// Version 2, wire mode.
	MsgEvent MsgType = 9  // holder: JSON WireEvent
	MsgAck   MsgType = 10 // client: JSON seq; the client stored the events up to it
	MsgCall  MsgType = 11 // client: JSON Call
	MsgReply MsgType = 12 // holder: JSON Reply
)

const (
	maxPayload   = 4 << 20
	headerSize   = 5
	writeTimeout = 10 * time.Second
	helloTimeout = 5 * time.Second
)

// ErrProtocol means that the other side speaks a holder protocol that this
// build does not know.
var ErrProtocol = errors.New("holder: unknown protocol")

// Dial connects to the holder at path and exchanges the hellos. A wire
// holder sends the events after from, when from is of its run.
func Dial(ctx context.Context, path string, from Cursor) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial holder: %w", err)
	}
	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	if err := c.handshake(from); err != nil {
		_ = conn.Close() // the handshake error is the one to report
		return nil, err
	}
	return c, nil
}

// Hello is the hello of the holder.
func (c *Client) Hello() Hello { return c.hello }

// Read waits for the next message from the holder.
func (c *Client) Read() (Msg, error) {
	for {
		t, p, err := readMsg(c.r)
		if err != nil {
			return Msg{}, err
		}
		m := Msg{Type: t}
		switch t {
		case MsgSnapshot, MsgOutput:
			m.Data = p
		case MsgResized:
			err = json.Unmarshal(p, &m.Size)
		case MsgExit:
			err = json.Unmarshal(p, &m.Exit)
		case MsgEvent:
			err = json.Unmarshal(p, &m.Event)
		case MsgReply:
			err = json.Unmarshal(p, &m.Reply)
		default:
			continue // a newer holder can add messages
		}
		if err != nil {
			return Msg{}, fmt.Errorf("decode holder message %d: %w", t, err)
		}
		return m, nil
	}
}

// Input writes bytes to the PTY.
func (c *Client) Input(p []byte) error { return c.write(MsgInput, p) }

// Resize sets the size of the PTY.
func (c *Client) Resize(cols, rows int) error {
	p, err := json.Marshal(Size{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	return c.write(MsgResize, p)
}

// Kill ends the process. The holder sends the exit message when it ends.
func (c *Client) Kill() error { return c.write(MsgKill, nil) }

// Ack tells a wire holder that the client stored the events up to seq.
func (c *Client) Ack(seq uint64) error {
	p, err := json.Marshal(seq)
	if err != nil {
		return err
	}
	return c.write(MsgAck, p)
}

// Call sends one call to a wire holder. The reply comes as a MsgReply.
func (c *Client) Call(call Call) error {
	p, err := json.Marshal(call)
	if err != nil {
		return err
	}
	return c.write(MsgCall, p)
}

// Close closes the connection. The holder and its process keep running.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) handshake(from Cursor) error {
	if err := c.conn.SetDeadline(time.Now().Add(helloTimeout)); err != nil {
		return err
	}
	t, p, err := readMsg(c.r)
	if err != nil {
		return fmt.Errorf("read holder hello: %w", err)
	}
	if t != MsgHello {
		return fmt.Errorf("%w: the first message is %d", ErrProtocol, t)
	}
	if err := json.Unmarshal(p, &c.hello); err != nil {
		return fmt.Errorf("decode holder hello: %w", err)
	}
	if c.hello.Protocol < MinProtocol || c.hello.Protocol > Protocol {
		return fmt.Errorf("%w: holder protocol %d", ErrProtocol, c.hello.Protocol)
	}
	mine := Hello{Protocol: c.hello.Protocol}
	if from.Run != "" && from.Run == c.hello.Run {
		mine.After = from.Seq
	}
	h, err := json.Marshal(mine)
	if err != nil {
		return err
	}
	if err := writeMsg(c.conn, MsgHello, h); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}
	return c.conn.SetDeadline(time.Time{})
}

func (c *Client) write(t MsgType, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return writeMsg(c.conn, t, p)
}

func encodeMsg(t MsgType, p []byte) []byte {
	out := make([]byte, headerSize+len(p))
	out[0] = byte(t)
	binary.BigEndian.PutUint32(out[1:headerSize], uint32(len(p)))
	copy(out[headerSize:], p)
	return out
}

func writeMsg(w io.Writer, t MsgType, p []byte) error {
	_, err := w.Write(encodeMsg(t, p))
	return err
}

func readMsg(r io.Reader) (MsgType, []byte, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxPayload {
		return 0, nil, fmt.Errorf("%w: a message of %d bytes", ErrProtocol, n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return MsgType(h[0]), p, nil
}
