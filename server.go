package agentvisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Server serves one visor over HTTP on a Unix socket.
type Server struct {
	v   *Visor
	log *slog.Logger
	mux *http.ServeMux
}

// Hello is what a product sends when it connects.
type Hello struct {
	Client   string `json:"client"`
	Build    string `json:"build"`
	Protocol string `json:"protocol"`
	// Binary is the product binary that contains the visor build of Build.
	Binary string `json:"binary,omitempty"`
}

// route is one entry of a route table.
type route struct {
	pattern string
	h       http.HandlerFunc
}

// errorBody is the body of each failed request.
type errorBody struct {
	Error errorInfo `json:"error"`
}

type errorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes of the socket protocol, with the error and the HTTP status of
// each.
//
//nolint:gochecknoglobals // a fixed table
var errorCodes = []struct {
	code   string
	err    error
	status int
}{
	{"not_found", ErrNotFound, http.StatusNotFound},
	{"invalid", ErrInvalid, http.StatusBadRequest},
	{"state", ErrState, http.StatusConflict},
	{"busy", ErrBusy, http.StatusConflict},
	{"closed", ErrClosed, http.StatusServiceUnavailable},
	{"unsupported", ErrUnsupported, http.StatusNotImplemented},
	{"protocol", ErrProtocol, http.StatusBadRequest},
}

const maxBody = 8 << 20

// NewServer makes the HTTP handler for v.
func NewServer(v *Visor) *Server {
	s := &Server{v: v, log: v.log.With("source", "server"), mux: http.NewServeMux()}
	for _, table := range [][]route{s.serverRoutes(), s.sessionRoutes(), s.workdirRoutes(), s.eventRoutes(), s.hookRoutes(), s.attachRoutes(), s.harnessRoutes()} {
		for _, r := range table {
			s.mux.HandleFunc(r.pattern, r.h)
		}
	}
	return s
}

// Serve serves v on socket until ctx ends. The socket has mode 0600.
func Serve(ctx context.Context, v *Visor, socket string) error {
	ln, err := Listen(ctx, socket)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: NewServer(v), ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(ctx, func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // the connections close in any case
	})
	defer stop()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", socket, err)
	}
	return nil
}

// Listen opens the Unix socket with mode 0600. It removes a stale socket file,
// and it fails when a visor already listens on it.
func Listen(ctx context.Context, socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, fmt.Errorf("listen %s: %w", socket, err)
	}
	var d net.Dialer
	if c, err := d.DialContext(ctx, "unix", socket); err == nil {
		_ = c.Close() // only a probe
		return nil, fmt.Errorf("%w: a visor listens on %s", ErrBusy, socket)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("listen %s: %w", socket, err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close() // the chmod error is the one to report
		return nil, fmt.Errorf("listen %s: %w", socket, err)
	}
	return ln, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) serverRoutes() []route {
	return []route{
		{"GET /v1/info", s.info},
		{"POST /v1/hello", s.hello},
	}
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	info, err := s.v.Info(r.Context())
	s.reply(w, http.StatusOK, info, err)
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	var h Hello
	if !s.decode(w, r, &h) {
		return
	}
	info, err := s.v.Hello(r.Context(), h)
	s.reply(w, http.StatusOK, info, err)
}

// decode reads the JSON body into v. It writes the error and gives false
// when the body is bad.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		s.fail(w, r, fmt.Errorf("%w: bad body: %w", ErrInvalid, err))
		return false
	}
	return true
}

// reply writes v with status, or the error.
func (s *Server) reply(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		s.fail(w, nil, err)
		return
	}
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Debug("write reply", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, status := errorCode(err)
	if status == http.StatusInternalServerError {
		path := ""
		if r != nil {
			path = r.URL.Path
		}
		s.log.Warn("request failed", "path", path, "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorBody{Error: errorInfo{Code: code, Message: err.Error()}}); err != nil {
		s.log.Debug("write error", "err", err)
	}
}

// errorCode gives the protocol code and the HTTP status of err.
func errorCode(err error) (string, int) {
	for _, c := range errorCodes {
		if errors.Is(err, c.err) {
			return c.code, c.status
		}
	}
	return "internal", http.StatusInternalServerError
}

// remoteError is an error from the socket. It unwraps to the error of its
// code, so errors.Is works the same on a Client and on a Visor.
type remoteError struct {
	info errorInfo
}

func (e *remoteError) Error() string { return e.info.Message }

func (e *remoteError) Unwrap() error {
	for _, c := range errorCodes {
		if c.code == e.info.Code {
			return c.err
		}
	}
	return nil
}

// parseFilter reads a filter from the query of a request.
func parseFilter(q url.Values) (Filter, error) {
	f := Filter{
		Session: q.Get("session"), Project: q.Get("project"), Workdir: q.Get("workdir"), Key: q.Get("key"),
		Kind: Kind(q.Get("kind")), Role: Role(q.Get("role")),
	}
	for _, st := range q["state"] {
		for p := range strings.SplitSeq(st, ",") {
			f.States = append(f.States, State(p))
		}
	}
	for _, l := range q["label"] {
		k, v, err := ParseLabel(l)
		if err != nil {
			return Filter{}, err
		}
		if f.Labels == nil {
			f.Labels = map[string]string{}
		}
		f.Labels[k] = v
	}
	return f, nil
}

// values writes the filter as a query. parseFilter reads it back.
func (f Filter) values() url.Values {
	q := url.Values{}
	for k, v := range map[string]string{
		"session": f.Session, "project": f.Project, "workdir": f.Workdir, "key": f.Key,
		"kind": string(f.Kind), "role": string(f.Role),
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	for _, st := range f.States {
		q.Add("state", string(st))
	}
	for k, v := range f.Labels {
		q.Add("label", k+"="+v)
	}
	return q
}

func queryBool(q url.Values, key string) bool {
	b, _ := strconv.ParseBool(q.Get(key)) //nolint:errcheck // a bad value means false
	return b
}
