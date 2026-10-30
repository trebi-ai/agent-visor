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
)

// MsgType is the type of one message on the holder socket.
type MsgType byte

// Hello is the first message of each side. The holder sends its hello
// first, and the client answers with the protocol that it speaks.
type Hello struct {
	Protocol int    `json:"protocol"`
	ID       string `json:"id,omitempty"`
	// PID is the holder process, and ChildPID the process in the PTY.
	PID       int       `json:"pid,omitempty"`
	ChildPID  int       `json:"child_pid,omitempty"`
	Cols      int       `json:"cols,omitempty"`
	Rows      int       `json:"rows,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

// Size is a terminal size in cells.
type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Exit is the end of the process. Code is 128 plus the signal number when a
// signal ended it.
type Exit struct {
	Code int `json:"code"`
}

// Msg is one message from the holder.
type Msg struct {
	Type MsgType
	// Data is the screen buffer of a snapshot, or the bytes of an output.
	Data []byte
	Size Size
	Exit Exit
}

// Client is one connection of a visor to a holder.
type Client struct {
	conn  net.Conn
	r     *bufio.Reader
	hello Hello
	wmu   sync.Mutex
}

// Protocol is the version of the holder protocol of this build.
const Protocol = 1

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

// Dial connects to the holder at path and exchanges the hellos.
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial holder: %w", err)
	}
	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	if err := c.handshake(); err != nil {
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

// Close closes the connection. The holder and its process keep running.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) handshake() error {
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
	if c.hello.Protocol != Protocol {
		return fmt.Errorf("%w: holder protocol %d", ErrProtocol, c.hello.Protocol)
	}
	h, err := json.Marshal(Hello{Protocol: Protocol})
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
