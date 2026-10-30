package agentvisor

import "net/http"

func (s *Server) harnessRoutes() []route {
	return []route{
		{"GET /v1/harnesses", s.listHarnesses},
		{"GET /v1/harnesses/{name}", s.getHarness},
		{"POST /v1/harnesses/{name}/install", s.installHarness},
		{"POST /v1/harnesses/{name}/update", s.updateHarness},
		{"PUT /v1/harnesses/{name}/auth", s.setHarnessAuth},
		{"POST /v1/harnesses/{name}/login", s.loginHarness},
		{"POST /v1/harnesses/{name}/logout", s.logoutHarness},
		{"GET /v1/harnesses/{name}/models", s.harnessModels},
		{"PUT /v1/harnesses/{name}/env", s.setHarnessEnv},
	}
}

// versionRequest is the body of an install or an update. An empty version
// is the latest.
type versionRequest struct {
	Version string `json:"version,omitempty"`
}

// authRequest is the body of an auth mode change.
type authRequest struct {
	Mode AuthMode `json:"mode"`
}

// envRequest is the body of a harness env change. The visor never returns
// the values.
type envRequest struct {
	Env map[string]string `json:"env"`
}

func (s *Server) listHarnesses(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.Harnesses(r.Context())
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) getHarness(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.Harness(r.Context(), r.PathValue("name"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) installHarness(w http.ResponseWriter, r *http.Request) {
	var in versionRequest
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.InstallHarness(r.Context(), r.PathValue("name"), in.Version)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) updateHarness(w http.ResponseWriter, r *http.Request) {
	var in versionRequest
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.UpdateHarness(r.Context(), r.PathValue("name"), in.Version)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) setHarnessAuth(w http.ResponseWriter, r *http.Request) {
	var in authRequest
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.SetHarnessAuth(r.Context(), r.PathValue("name"), in.Mode)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) loginHarness(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.LoginHarness(r.Context(), r.PathValue("name"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) logoutHarness(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.LogoutHarness(r.Context(), r.PathValue("name")))
}

func (s *Server) harnessModels(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.HarnessModels(r.Context(), r.PathValue("name"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) setHarnessEnv(w http.ResponseWriter, r *http.Request) {
	var in envRequest
	if !s.decode(w, r, &in) {
		return
	}
	s.reply(w, http.StatusNoContent, nil, s.v.SetHarnessEnv(r.Context(), r.PathValue("name"), in.Env))
}
