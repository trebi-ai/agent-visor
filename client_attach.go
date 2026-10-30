package agentvisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// remoteAttach is the socket side of an attachment of a Client. One
// goroutine reads the socket into in, so the end of a Recv context does not
// close the socket.
type remoteAttach struct {
	ws     *websocket.Conn
	in     chan remoteFrame
	first  *Frame
	cancel context.CancelFunc
	stop   func() bool
	once   sync.Once
	closed atomic.Bool
	done   chan struct{}
}

type remoteFrame struct {
	f   Frame
	err error
}

// Attach attaches to the terminal or the chat of a session.
func (c *Client) Attach(ctx context.Context, id string, o AttachOptions) (*AttachStream, error) {
	if c.ctx.Err() != nil {
		return nil, ErrClosed
	}
	ws, resp, err := websocket.Dial(ctx, "ws://visor/v1/sessions/"+url.PathEscape(id)+"/attach", &websocket.DialOptions{HTTPClient: c.hc})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close() // only the status matters
	}
	if err != nil {
		return nil, fmt.Errorf("attach %s: %w", id, err)
	}
	ws.SetReadLimit(maxBody)
	f := ClientFrame{Type: ClientAttach, View: o.View, Cols: o.Cols, Rows: o.Rows}
	if o.After > 0 {
		f.After = &o.After
	}
	r := &remoteAttach{ws: ws, in: make(chan remoteFrame, 16), done: make(chan struct{})}
	if err := r.send(ctx, f); err != nil {
		_ = ws.CloseNow() // the send error is the one to report
		return nil, fmt.Errorf("attach %s: %w", id, err)
	}
	first, err := r.read(ctx)
	if err != nil {
		_ = ws.CloseNow() // the read error is the one to report
		return nil, fmt.Errorf("attach %s: %w", id, err)
	}
	if first.Type == FrameError {
		_ = ws.CloseNow() // the error frame is the one to report
		return nil, &remoteError{info: errorInfo{Code: first.Code, Message: first.Message}}
	}
	r.first = &first
	rctx, cancel := context.WithCancel(c.ctx)
	r.cancel = cancel
	r.stop = context.AfterFunc(c.ctx, func() { _ = r.close() }) // Client.Close ends the attachment
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		r.pump(rctx)
	}()
	return &AttachStream{remote: r, view: first.View, session: id}, nil
}

// Screen gives the terminal of a session as text.
func (c *Client) Screen(ctx context.Context, id string) (Screen, error) {
	var out Screen
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id)+"/screen", nil, &out)
	return out, err
}

// Keys writes data to the terminal of a session as typed keys.
func (c *Client) Keys(ctx context.Context, id string, data []byte) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/keys", keysRequest{Data: data}, nil)
}

// Output gives the end of the terminal output of a session as plain text.
func (c *Client) Output(ctx context.Context, id string) (string, error) {
	var out outputReply
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id)+"/output", nil, &out)
	return out.Text, err
}

// Handover moves a session between the wire and the pty backend.
func (c *Client) Handover(ctx context.Context, id string, to Kind) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/handover", handoverRequest{To: to}, nil)
}

func (r *remoteAttach) pump(ctx context.Context) {
	defer close(r.done)
	defer close(r.in)
	for {
		f, err := r.read(ctx)
		select {
		case r.in <- remoteFrame{f: f, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// read reads one frame. A normal close is io.EOF, and the visor close code
// is ErrClosed.
func (r *remoteAttach) read(ctx context.Context) (Frame, error) {
	typ, data, err := r.ws.Read(ctx)
	switch websocket.CloseStatus(err) {
	case -1:
	case websocket.StatusNormalClosure:
		return Frame{}, io.EOF
	case closeVisorClosed:
		return Frame{}, ErrClosed
	}
	if err != nil {
		return Frame{}, err
	}
	return DecodeFrame(typ == websocket.MessageBinary, data)
}

func (r *remoteAttach) recv(ctx context.Context) (Frame, error) {
	if r.closed.Load() {
		return Frame{}, ErrClosed
	}
	if f := r.first; f != nil {
		r.first = nil
		return *f, nil
	}
	select {
	case rf, ok := <-r.in:
		if !ok {
			return Frame{}, ErrClosed
		}
		return rf.f, rf.err
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

func (r *remoteAttach) send(ctx context.Context, f ClientFrame) error {
	isBinary, data, err := f.Encode()
	if err != nil {
		return err
	}
	typ := websocket.MessageText
	if isBinary {
		typ = websocket.MessageBinary
	}
	return r.ws.Write(ctx, typ, data)
}

func (r *remoteAttach) close() error {
	r.once.Do(func() {
		r.closed.Store(true)
		if r.stop != nil {
			r.stop()
		}
		_ = r.ws.Close(websocket.StatusNormalClosure, "detach") // the server sees the close
		if r.cancel != nil {
			r.cancel()
			<-r.done
		}
	})
	return nil
}
