package agentvisor

import (
	"net/http"
)

func (s *Server) hookRoutes() []route {
	return []route{
		{"POST /v1/hooks/{harness}", s.hook},
		{"GET /v1/hooks", s.hooks},
		{"PUT /v1/hooks", s.installHooks},
		{"DELETE /v1/hooks", s.removeHooks},
	}
}

// hook applies one hook call. The harness in the path wins over the body.
// It replies 204 when the call changes no session.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	var c HookCall
	if !s.decode(w, r, &c) {
		return
	}
	c.Harness = r.PathValue("harness")
	out, err := s.v.Hook(r.Context(), c)
	if err == nil && out.ID == "" {
		s.reply(w, http.StatusNoContent, nil, nil)
		return
	}
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) hooks(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.Hooks(r.Context(), parseHookSpec(r.URL.Query()))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) installHooks(w http.ResponseWriter, r *http.Request) {
	var spec HookSpec
	if !s.decode(w, r, &spec) {
		return
	}
	out, err := s.v.InstallHooks(r.Context(), spec)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) removeHooks(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.RemoveHooks(r.Context(), parseHookSpec(r.URL.Query()))
	s.reply(w, http.StatusOK, out, err)
}
