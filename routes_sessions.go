package agentvisor

import "net/http"

func (s *Server) sessionRoutes() []route {
	return []route{
		{"POST /v1/sessions", s.startSession},
		{"GET /v1/sessions", s.listSessions},
		{"GET /v1/sessions/{id}", s.getSession},
		{"DELETE /v1/sessions/{id}", s.stopSession},
		{"POST /v1/sessions/{id}/send", s.sendSession},
		{"POST /v1/sessions/{id}/interrupt", s.interruptSession},
		{"POST /v1/sessions/{id}/answer", s.answerSession},
		{"POST /v1/purge", s.purge},
	}
}

// sendReply is the reply of a send.
type sendReply struct {
	Delivery Delivery `json:"delivery"`
}

// answerRequest is the body of an answer.
type answerRequest struct {
	RequestID string `json:"request_id"`
	Answer
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	var spec StartSpec
	if !s.decode(w, r, &spec) {
		return
	}
	out, err := s.v.Start(r.Context(), spec)
	s.reply(w, http.StatusCreated, out, err)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.v.Sessions(r.Context(), f)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.Session(r.Context(), r.PathValue("id"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.Stop(r.Context(), r.PathValue("id")))
}

func (s *Server) sendSession(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !s.decode(w, r, &in) {
		return
	}
	d, err := s.v.Send(r.Context(), r.PathValue("id"), in)
	s.reply(w, http.StatusOK, sendReply{Delivery: d}, err)
}

func (s *Server) interruptSession(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.Interrupt(r.Context(), r.PathValue("id")))
}

func (s *Server) answerSession(w http.ResponseWriter, r *http.Request) {
	var a answerRequest
	if !s.decode(w, r, &a) {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.v.Answer(r.Context(), r.PathValue("id"), a.RequestID, a.Answer))
}

func (s *Server) purge(w http.ResponseWriter, r *http.Request) {
	var p PurgeSpec
	if !s.decode(w, r, &p) {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.v.Purge(r.Context(), p))
}
