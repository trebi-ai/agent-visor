package agentvisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/coder/websocket"
)

func (s *Server) attachRoutes() []route {
	return []route{
		{"GET /v1/sessions/{id}/attach", s.attach},
		{"POST /v1/sessions/{id}/handover", s.handover},
		{"GET /v1/sessions/{id}/screen", s.screen},
		{"POST /v1/sessions/{id}/keys", s.keys},
		{"GET /v1/sessions/{id}/output", s.output},
	}
}

// outputReply is the reply of an output call.
type outputReply struct {
	Text string `json:"text"`
}

func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	text, err := s.v.Output(r.Context(), r.PathValue("id"))
	s.reply(w, http.StatusOK, outputReply{Text: text}, err)
}

// keysRequest is the body of a keys call. Data is base64 in JSON.
type keysRequest struct {
	Data []byte `json:"data"`
}

func (s *Server) screen(w http.ResponseWriter, r *http.Request) {
	sc, err := s.v.Screen(r.Context(), r.PathValue("id"))
	s.reply(w, http.StatusOK, sc, err)
}

func (s *Server) keys(w http.ResponseWriter, r *http.Request) {
	var in keysRequest
	if !s.decode(w, r, &in) {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.v.Keys(r.Context(), r.PathValue("id"), in.Data))
}

// handoverRequest is the body of a handover.
type handoverRequest struct {
	To Kind `json:"to"`
}

func (s *Server) handover(w http.ResponseWriter, r *http.Request) {
	var in handoverRequest
	if !s.decode(w, r, &in) {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.v.Handover(r.Context(), r.PathValue("id"), in.To))
}

// attach serves one attachment over a WebSocket. The first client frame is
// attach. A failed attach gives one error frame, then the socket closes.
func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Debug("accept attach", "err", err)
		return
	}
	defer c.CloseNow() //nolint:errcheck // a second close is harmless
	c.SetReadLimit(maxBody)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	a, err := s.openAttach(ctx, c, r.PathValue("id"))
	if err != nil {
		if data, eerr := errorFrame(err).Encode(); eerr == nil {
			_ = c.Write(ctx, websocket.MessageText, data) // the close follows in any case
		}
		_ = c.Close(closeStreamError, "attach failed") // the client read the error frame
		return
	}
	defer a.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		s.readAttach(ctx, c, a)
	}()
	defer func() { <-done }()
	for {
		f, err := a.Recv(ctx)
		switch {
		case errors.Is(err, io.EOF):
			_ = c.Close(websocket.StatusNormalClosure, "ended") // the client sees the code
			return
		case errors.Is(err, ErrClosed):
			_ = c.Close(closeVisorClosed, "the visor stops") // the client sees the code
			return
		case err != nil:
			return
		}
		data, err := f.Encode()
		if err != nil {
			s.log.Warn("encode attach frame", "err", err)
			return
		}
		typ := websocket.MessageText
		if f.Binary() {
			typ = websocket.MessageBinary
		}
		if err := c.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

func (s *Server) openAttach(ctx context.Context, c *websocket.Conn, id string) (*AttachStream, error) {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	f, err := DecodeClientFrame(typ == websocket.MessageBinary, data)
	if err != nil {
		return nil, err
	}
	if f.Type != ClientAttach {
		return nil, fmt.Errorf("%w: the first frame must be attach", ErrInvalid)
	}
	return s.v.Attach(ctx, id, f.Options())
}

// readAttach applies the client frames until the client goes or the
// attachment closes.
func (s *Server) readAttach(ctx context.Context, c *websocket.Conn, a *AttachStream) {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		f, err := DecodeClientFrame(typ == websocket.MessageBinary, data)
		if err != nil {
			a.push(errorFrame(err))
			continue
		}
		if err := a.Handle(ctx, f); errors.Is(err, ErrClosed) {
			return
		}
	}
}
