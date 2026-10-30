package agentvisor

import "net/http"

func (s *Server) workdirRoutes() []route {
	return []route{
		{"GET /v1/projects", s.listProjects},
		{"DELETE /v1/projects/{id}", s.forgetProject},
		{"GET /v1/workdirs", s.listWorkdirs},
		{"PUT /v1/workdirs", s.ensureWorkdir},
		{"POST /v1/workdirs/{id}/archive", s.archiveWorkdir},
		{"POST /v1/workdirs/{id}/restore", s.restoreWorkdir},
		{"DELETE /v1/workdirs/{id}", s.removeWorkdir},
	}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.v.Projects(r.Context(), f)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) forgetProject(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.ForgetProject(r.Context(), r.PathValue("id")))
}

func (s *Server) listWorkdirs(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.v.Workdirs(r.Context(), f)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) ensureWorkdir(w http.ResponseWriter, r *http.Request) {
	var spec WorkdirSpec
	if !s.decode(w, r, &spec) {
		return
	}
	out, err := s.v.EnsureWorkdir(r.Context(), spec)
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) archiveWorkdir(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.ArchiveWorkdir(r.Context(), r.PathValue("id")))
}

func (s *Server) restoreWorkdir(w http.ResponseWriter, r *http.Request) {
	out, err := s.v.RestoreWorkdir(r.Context(), r.PathValue("id"))
	s.reply(w, http.StatusOK, out, err)
}

func (s *Server) removeWorkdir(w http.ResponseWriter, r *http.Request) {
	s.reply(w, http.StatusNoContent, nil, s.v.RemoveWorkdir(r.Context(), r.PathValue("id"), queryBool(r.URL.Query(), "force")))
}
