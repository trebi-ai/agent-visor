package agentvisor

import "net/http"

func (s *Server) harnessRoutes() []route {
	return []route{
		{"GET /v1/harnesses", s.listHarnesses},
		{"GET /v1/harnesses/{name}", s.getHarness},
		{"POST /v1/harnesses/{name}/install", s.installHarness},
		{"POST /v1/harnesses/{name}/update", s.updateHarness},
		{"PUT /v1/harnesses/{name}/auth", s.setHarnessAuth},
		{"PUT /v1/harnesses/{name}/updates", s.setHarnessUpdates},
		{"POST /v1/harnesses/check", s.checkHarnesses},
		{"POST /v1/harnesses/{name}/login", s.loginHarness},
		{"GET /v1/harnesses/{name}/login", s.harnessLogin},
		{"DELETE /v1/harnesses/{name}/login", s.cancelHarnessLogin},
		{"POST /v1/harnesses/{name}/login/code", s.harnessLoginCode},
		{"POST /v1/harnesses/{name}/login/token", s.harnessLoginToken},
		{"POST /v1/harnesses/{name}/logout", s.logoutHarness},
		{"GET /v1/harnesses/{name}/models", s.harnessModels},
		{"PUT /v1/harnesses/{name}/env", s.setHarnessEnv},
		{"GET /v1/harnesses/{name}/files", s.harnessFiles},
		{"GET /v1/harnesses/{name}/files/{key}", s.readHarnessFile},
		{"PUT /v1/harnesses/{name}/files/{key}", s.writeHarnessFile},
		{"POST /v1/harnesses/{name}/files/{key}/share", s.shareHarnessFile},
		{"POST /v1/harnesses/{name}/files/{key}/own", s.ownHarnessFile},
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

// updatesRequest is the body of an update mode change.
type updatesRequest struct {
	Mode    UpdateMode `json:"mode"`
	Version string     `json:"version,omitempty"`
}

// codeRequest is the body of a pasted login code.
type codeRequest struct {
	Code string `json:"code"`
}

// tokenRequest is the body of a token login. The visor never returns the
// token.
type tokenRequest struct {
	Token string `json:"token"`
}

// fileWrite is the body of a home file write. An empty if_match skips the
// etag check.
type fileWrite struct {
	Content string `json:"content"`
	IfMatch string `json:"if_match,omitempty"`
}

// ownRequest is the body of an own copy request.
type ownRequest struct {
	Start OwnStart `json:"start"`
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

func (s *Server) setHarnessUpdates(w http.ResponseWriter, r *http.Request) {
	var in updatesRequest
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.SetHarnessUpdates(r.Context(), r.PathValue("name"), in.Mode, in.Version)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) checkHarnesses(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.CheckHarnesses(r.Context()))
}

func (s *Server) loginHarness(w http.ResponseWriter, r *http.Request) {
	// An empty body is the default spec.
	var in LoginSpec
	if r.ContentLength != 0 && !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.LoginHarness(r.Context(), r.PathValue("name"), in)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) harnessLogin(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.HarnessLogin(r.Context(), r.PathValue("name"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) cancelHarnessLogin(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.CancelHarnessLogin(r.Context(), r.PathValue("name")))
}

func (s *Server) harnessLoginCode(w http.ResponseWriter, r *http.Request) {
	var in codeRequest
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.HarnessLoginCode(r.Context(), r.PathValue("name"), in.Code)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) harnessLoginToken(w http.ResponseWriter, r *http.Request) {
	var in tokenRequest
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.HarnessLoginToken(r.Context(), r.PathValue("name"), in.Token)
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

func (s *Server) harnessFiles(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.HarnessFiles(r.Context(), r.PathValue("name"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) readHarnessFile(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.ReadHarnessFile(r.Context(), r.PathValue("name"), r.PathValue("key"), r.URL.Query().Get("side"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) writeHarnessFile(w http.ResponseWriter, r *http.Request) {
	var in fileWrite
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.WriteHarnessFile(r.Context(), r.PathValue("name"), r.PathValue("key"), in.Content, in.IfMatch)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) shareHarnessFile(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.ShareHarnessFile(r.Context(), r.PathValue("name"), r.PathValue("key"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) ownHarnessFile(w http.ResponseWriter, r *http.Request) {
	var in ownRequest
	if !s.decode(w, r, &in) {
		return
	}
	out, err := s.v.OwnHarnessFile(r.Context(), r.PathValue("name"), r.PathValue("key"), in.Start)
	s.reply(w, http.StatusOK, out, err)
}
