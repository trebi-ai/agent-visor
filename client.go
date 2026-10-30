package agentvisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Client talks to a visor in another process over its socket. It has the
// same method set as Visor.
type Client struct {
	socket string
	hc     *http.Client

	ctx    context.Context //nolint:containedctx // the client lifetime; Close cancels it
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// The host name in URLs. The transport dials the socket and ignores it.
const clientBase = "http://visor"

const (
	reconnectFirst = 50 * time.Millisecond
	reconnectMax   = 2 * time.Second
)

// Dial connects to the visor on socket and checks its protocol.
func Dial(ctx context.Context, socket string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost: 8,
	}
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Client{socket: socket, hc: &http.Client{Transport: tr}, ctx: cctx, cancel: cancel}
	info, err := c.Info(ctx)
	if err != nil {
		_ = c.Close(ctx) // the dial error is the one to report
		return nil, fmt.Errorf("dial visor %s: %w", socket, err)
	}
	if protocolMajor(info.Protocol) != protocolMajor(Protocol) {
		_ = c.Close(ctx) // the protocol error is the one to report
		return nil, fmt.Errorf("%w: the visor speaks %s, the client %s", ErrProtocol, info.Protocol, Protocol)
	}
	return c, nil
}

// Info describes the visor.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	return out, c.do(ctx, http.MethodGet, "/v1/info", nil, &out)
}

// Hello tells the visor which product connects.
func (c *Client) Hello(ctx context.Context, h Hello) (Info, error) {
	var out Info
	return out, c.do(ctx, http.MethodPost, "/v1/hello", h, &out)
}

// Start starts a session.
func (c *Client) Start(ctx context.Context, spec StartSpec) (Session, error) {
	var out Session
	return out, c.do(ctx, http.MethodPost, "/v1/sessions", spec, &out)
}

// Session reads one session.
func (c *Client) Session(ctx context.Context, id string) (Session, error) {
	var out Session
	return out, c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, &out)
}

// Sessions lists the sessions that match f, oldest first.
func (c *Client) Sessions(ctx context.Context, f Filter) ([]Session, error) {
	var out []Session
	return out, c.do(ctx, http.MethodGet, "/v1/sessions?"+f.values().Encode(), nil, &out)
}

// Send writes a user turn.
func (c *Client) Send(ctx context.Context, id string, in Input) (Delivery, error) {
	var out sendReply
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/send", in, &out)
	return out.Delivery, err
}

// Interrupt stops the current turn.
func (c *Client) Interrupt(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/interrupt", nil, nil)
}

// Answer decides an open request.
func (c *Client) Answer(ctx context.Context, id, requestID string, a Answer) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/answer", answerRequest{RequestID: requestID, Answer: a}, nil)
}

// Stop ends a session.
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(id), nil, nil)
}

// Events gives the events after a sequence number that match f. The stream
// connects again after a drop or a visor restart, and continues after the
// last event that it gave.
func (c *Client) Events(ctx context.Context, f Filter, after uint64) (*EventStream, error) {
	if c.ctx.Err() != nil {
		return nil, ErrClosed
	}
	s, sctx := newEventStream(ctx)
	stop := context.AfterFunc(c.ctx, s.cancel)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer stop()
		s.finish(c.pumpEvents(sctx, s, f, after))
	}()
	return s, nil
}

// Projects lists the projects.
func (c *Client) Projects(ctx context.Context, f Filter) ([]Project, error) {
	var out []Project
	return out, c.do(ctx, http.MethodGet, "/v1/projects?"+f.values().Encode(), nil, &out)
}

// ForgetProject removes a project with no workdirs.
func (c *Client) ForgetProject(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(id), nil, nil)
}

// Workdirs lists the workdirs that match f.
func (c *Client) Workdirs(ctx context.Context, f Filter) ([]Workdir, error) {
	var out []Workdir
	return out, c.do(ctx, http.MethodGet, "/v1/workdirs?"+f.values().Encode(), nil, &out)
}

// EnsureWorkdir gives the workdir for w, and makes it once.
func (c *Client) EnsureWorkdir(ctx context.Context, w WorkdirSpec) (Workdir, error) {
	var out Workdir
	return out, c.do(ctx, http.MethodPut, "/v1/workdirs", w, &out)
}

// ArchiveWorkdir stops the sessions of a workdir and archives it.
func (c *Client) ArchiveWorkdir(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/workdirs/"+url.PathEscape(id)+"/archive", nil, nil)
}

// RestoreWorkdir makes an archived workdir active again.
func (c *Client) RestoreWorkdir(ctx context.Context, id string) (Workdir, error) {
	var out Workdir
	return out, c.do(ctx, http.MethodPost, "/v1/workdirs/"+url.PathEscape(id)+"/restore", nil, &out)
}

// RemoveWorkdir removes a workdir.
func (c *Client) RemoveWorkdir(ctx context.Context, id string, force bool) error {
	return c.do(ctx, http.MethodDelete, "/v1/workdirs/"+url.PathEscape(id)+"?force="+strconv.FormatBool(force), nil, nil)
}

// Close ends the event streams of the client and its connections. The visor
// keeps running.
func (c *Client) Close(context.Context) error {
	c.cancel()
	c.wg.Wait()
	c.hc.CloseIdleConnections()
	return nil
}

// do sends one request. in is the JSON body or nil; out receives the JSON
// reply or is nil.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, clientBase+path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		var eb errorBody
		if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil || eb.Error.Code == "" {
			return fmt.Errorf("%s %s: status %d", method, path, resp.StatusCode)
		}
		return &remoteError{info: eb.Error}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode reply: %w", method, path, err)
	}
	return nil
}

// pumpEvents reads the event socket into s, and connects again after a drop.
func (c *Client) pumpEvents(ctx context.Context, s *EventStream, f Filter, last uint64) error {
	wait := reconnectFirst
	for {
		got, err := c.readEvents(ctx, s, f, last)
		if got > last {
			last, wait = got, reconnectFirst
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrInvalid) {
			return err
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		wait = min(wait*2, reconnectMax)
	}
}

// readEvents reads one event socket until it drops, and gives the last seq
// that it gave to s.
func (c *Client) readEvents(ctx context.Context, s *EventStream, f Filter, last uint64) (uint64, error) {
	q := f.values()
	q.Set("after", strconv.FormatUint(last, 10))
	ws, resp, err := websocket.Dial(ctx, "ws://visor/v1/events?"+q.Encode(), &websocket.DialOptions{HTTPClient: c.hc})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close() // only the status matters
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusBadRequest {
			return last, fmt.Errorf("%w: events: %w", ErrInvalid, err)
		}
		return last, err
	}
	defer ws.CloseNow() //nolint:errcheck // a second close is harmless
	ws.SetReadLimit(maxBody)
	for {
		var e Event
		if err := wsjson.Read(ctx, ws, &e); err != nil {
			return last, err
		}
		if e.Seq <= last {
			continue
		}
		if !s.send(ctx, e) {
			return last, ctx.Err()
		}
		last = e.Seq
	}
}
