package agentvisor

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func (s *Server) eventRoutes() []route {
	return []route{
		{"GET /v1/events", s.events},
	}
}

// WebSocket close codes of the event stream.
const (
	// closeVisorClosed means that the visor stops. A client reconnects.
	closeVisorClosed websocket.StatusCode = 4001
	// closeStreamError means that the stream failed. A client reconnects.
	closeStreamError websocket.StatusCode = 4002
)

// events streams the events after the after query value over a WebSocket:
// first from the log, then live.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := parseFilter(q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	after, err := strconv.ParseUint(q.Get("after"), 10, 64)
	if q.Get("after") != "" && err != nil {
		s.fail(w, r, fmt.Errorf("%w: after %q", ErrInvalid, q.Get("after")))
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Debug("accept events", "err", err)
		return
	}
	defer c.CloseNow() //nolint:errcheck // a second close is harmless
	ctx := c.CloseRead(r.Context())
	st, err := s.v.Events(ctx, f, after)
	if err != nil {
		_ = c.Close(closeVisorClosed, err.Error()) // the client sees the code
		return
	}
	defer st.Close()
	for {
		e, err := st.Next(ctx)
		switch {
		case errors.Is(err, ErrClosed):
			_ = c.Close(closeVisorClosed, "the visor stops") // the client sees the code
			return
		case ctx.Err() != nil:
			return
		case err != nil:
			_ = c.Close(closeStreamError, err.Error()) // the client sees the code
			return
		}
		if err := wsjson.Write(ctx, c, e); err != nil {
			return
		}
	}
}
