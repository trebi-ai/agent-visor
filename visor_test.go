package agentvisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/trebi-ai/agent-visor/internal/store"
)

// api is the method set that Visor and Client share.
type api interface {
	Info(ctx context.Context) (Info, error)
	Hello(ctx context.Context, h Hello) (Info, error)
	Upgrade(ctx context.Context, spec UpgradeSpec) (Info, error)
	Start(ctx context.Context, spec StartSpec) (Session, error)
	Session(ctx context.Context, id string) (Session, error)
	Sessions(ctx context.Context, f Filter) ([]Session, error)
	Send(ctx context.Context, id string, in Input) (Delivery, error)
	Interrupt(ctx context.Context, id string) error
	Answer(ctx context.Context, id, requestID string, a Answer) error
	Stop(ctx context.Context, id string) error
	Events(ctx context.Context, f Filter, after uint64) (*EventStream, error)
	Projects(ctx context.Context, f Filter) ([]Project, error)
	ForgetProject(ctx context.Context, id string) error
	Workdirs(ctx context.Context, f Filter) ([]Workdir, error)
	EnsureWorkdir(ctx context.Context, w WorkdirSpec) (Workdir, error)
	ArchiveWorkdir(ctx context.Context, id string) error
	RestoreWorkdir(ctx context.Context, id string) (Workdir, error)
	RemoveWorkdir(ctx context.Context, id string, force bool) error
	Hook(ctx context.Context, c HookCall) (Session, error)
	Hooks(ctx context.Context, spec HookSpec) ([]HookStatus, error)
	InstallHooks(ctx context.Context, spec HookSpec) ([]HookStatus, error)
	RemoveHooks(ctx context.Context, spec HookSpec) ([]HookStatus, error)
	Attach(ctx context.Context, id string, o AttachOptions) (*AttachStream, error)
	Handover(ctx context.Context, id string, to Kind) error
	Harnesses(ctx context.Context) ([]Harness, error)
	Harness(ctx context.Context, name string) (Harness, error)
	InstallHarness(ctx context.Context, name, version string) (Harness, error)
	UpdateHarness(ctx context.Context, name, version string) (Harness, error)
	SetHarnessAuth(ctx context.Context, name string, mode AuthMode) (Harness, error)
	LoginHarness(ctx context.Context, name string) (Login, error)
	LogoutHarness(ctx context.Context, name string) error
	HarnessModels(ctx context.Context, name string) ([]Model, error)
	SetHarnessEnv(ctx context.Context, name string, env map[string]string) error
	Close(ctx context.Context) error
}

type opener func(t *testing.T, home, script string, log *slog.Logger) api

// served is a Client of a visor that the test serves. Close also stops the
// server and the visor.
type served struct {
	*Client
	ts *testServer
}

var (
	_ api = (*Visor)(nil)
	_ api = (*Client)(nil)
)

func (s *served) Close(ctx context.Context) error {
	_ = s.Client.Close(ctx) // the visor close error is the one to report
	return s.ts.close(ctx)
}

// Fake harness scripts. Each reads one NDJSON line for each prompt.
const (
	scriptTurns = `while read -r line; do
  echo '{"type":"init","session_id":"v1"}'
  echo '{"type":"assistant","text":"working"}'
  echo '{"type":"result","subtype":"success","text":"done","session_id":"v1"}'
done`
	scriptPermission = `while read -r line; do
  case "$line" in
  *decision*) echo '{"type":"assistant","text":"allowed"}'; echo '{"type":"result","subtype":"success","session_id":"v1"}';;
  *) echo '{"type":"init","session_id":"v1"}'; echo '{"type":"permission","id":"p1","tool":"Bash","question":"Run it?"}';;
  esac
done`
	scriptHang = `while read -r line; do
  echo '{"type":"init","session_id":"v1"}'
  case "$line" in
  *hang*) echo '{"type":"assistant","text":"thinking"}';;
  *) echo '{"type":"result","subtype":"success","session_id":"v1"}';;
  esac
done`
	scriptFlood = `while read -r line; do
  echo '{"type":"init","session_id":"v1"}'
  i=0; while [ $i -lt 6000 ]; do echo '{"type":"assistant","text":"x","delta":true}'; i=$((i+1)); done
  echo '{"type":"result","subtype":"success","session_id":"v1"}'
done`
)

func TestStartTurnAndReplay(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		v := open(t, t.TempDir(), scriptTurns, nil)
		defer v.Close(context.Background())
		s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi", Labels: map[string]string{"app.run": "r1"}})
		if err != nil {
			t.Fatal(err)
		}
		if s.Kind != KindWire || s.Role != RoleAgent || s.Workdir == "" || s.Project == "" {
			t.Fatalf("session defaults: %+v", s)
		}
		got := waitState(t, v, s.ID, StateIdle)
		if got.VendorID != "v1" || got.Turns != 1 {
			t.Fatalf("after one turn: vendor %q turns %d", got.VendorID, got.Turns)
		}
		if d, err := v.Send(t.Context(), s.ID, Input{Text: "again"}); err != nil || d != DeliveryPrompted {
			t.Fatalf("send: %q %v", d, err)
		}
		for waitState(t, v, s.ID, StateIdle).Turns != 2 {
			time.Sleep(10 * time.Millisecond)
		}
		head := got.Seq
		for after := uint64(0); after < head; after++ {
			st, err := v.Events(t.Context(), Filter{}, after)
			if err != nil {
				t.Fatal(err)
			}
			events := collect(t, st, func(e Event) bool { return e.Seq >= head })
			checkSeq(t, events, after)
			_ = st.Close() // a test stream
		}
		st, err := v.Events(t.Context(), Filter{Labels: map[string]string{"app.run": "r1"}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		types := []EventType{}
		for _, e := range collect(t, st, func(e Event) bool { return e.Type == EventResult }) {
			types = append(types, e.Type)
			if e.Type == EventUser && !e.Input {
				t.Errorf("the prompt has no input mark: %+v", e)
			}
		}
		want := []EventType{EventSessionState, EventUser, EventSessionState, EventInit, EventAssistant, EventResult}
		if fmt.Sprint(types) != fmt.Sprint(want) {
			t.Fatalf("event types %v, want %v", types, want)
		}
	})
}

func TestPermissionAnswer(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		v := open(t, t.TempDir(), scriptPermission, nil)
		defer v.Close(context.Background())
		s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "go"})
		if err != nil {
			t.Fatal(err)
		}
		w := waitState(t, v, s.ID, StateWaiting)
		if len(w.Pending) != 1 || w.Pending[0].ID != "p1" || w.Pending[0].Question != "Run it?" {
			t.Fatalf("pending: %+v", w.Pending)
		}
		if err := v.Answer(t.Context(), s.ID, "p1", Answer{Allow: true}); err != nil {
			t.Fatal(err)
		}
		if got := waitState(t, v, s.ID, StateIdle); len(got.Pending) != 0 {
			t.Fatalf("pending after the answer: %+v", got.Pending)
		}
		st, err := v.Events(t.Context(), Filter{Session: s.ID}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		closed := false
		for _, e := range collect(t, st, func(e Event) bool { return e.Type == EventResult }) {
			closed = closed || (e.Type == EventRequestClosed && e.RequestID == "p1")
		}
		if !closed {
			t.Fatal("no request.closed event")
		}
	})
}

func TestStopAndInterruptStates(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		v := open(t, t.TempDir(), scriptHang, nil)
		defer v.Close(context.Background())
		s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang"})
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, v, s.ID, StateRunning)
		if err := v.Stop(t.Context(), s.ID); err != nil {
			t.Fatal(err)
		}
		waitState(t, v, s.ID, StateExited)
		if _, err := v.Send(t.Context(), s.ID, Input{Text: "more"}); !errors.Is(err, ErrState) {
			t.Fatalf("send to an exited session: %v, want ErrState", err)
		}
		if _, err := v.Session(t.Context(), "ses_none"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing session: %v", err)
		}
	})
}

func TestSuspendAndResumeAcrossRestart(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		home := t.TempDir()
		v := open(t, home, scriptTurns, nil)
		s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, v, s.ID, StateIdle)
		if err := v.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		v = open(t, home, scriptTurns, nil)
		defer v.Close(context.Background())
		waitState(t, v, s.ID, StateSuspended)
		if _, err := v.Send(t.Context(), s.ID, Input{Text: "back"}); err != nil {
			t.Fatal(err)
		}
		got := waitState(t, v, s.ID, StateIdle)
		if got.ID != s.ID || got.Turns != 2 {
			t.Fatalf("resumed session: %+v", got)
		}
	})
}

func TestLostSessionRestartPolicy(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		home := t.TempDir()
		v := open(t, home, scriptHang, nil)
		keep, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang", Restart: RestartPolicy{Mode: RestartResume, Prompt: "continue"}})
		if err != nil {
			t.Fatal(err)
		}
		notify, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang"})
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, v, keep.ID, StateRunning)
		waitState(t, v, notify.ID, StateRunning)
		if err := v.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		v = open(t, home, scriptHang, nil)
		defer v.Close(context.Background())
		waitState(t, v, notify.ID, StateLost)
		waitState(t, v, keep.ID, StateIdle)
	})
}

func TestCrashMarksSessionsLost(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		home := t.TempDir()
		st, err := store.Open(t.Context(), filepath.Join(home, "visor.db"))
		if err != nil {
			t.Fatal(err)
		}
		for id, state := range map[string]State{"ses_run": StateRunning, "ses_idle": StateIdle, "ses_shell": StateIdle} {
			s := Session{ID: id, Kind: KindWire, State: state, VendorID: "v1", CreatedAt: time.Now()}
			if id == "ses_shell" {
				s.Kind, s.VendorID = KindPTY, ""
			}
			data, _ := json.Marshal(s) //nolint:errchkjson // a plain struct
			if err := st.Put(t.Context(), store.KindSession, id, data); err != nil {
				t.Fatal(err)
			}
		}
		_ = st.Close() // test setup
		v := open(t, home, scriptTurns, nil)
		defer v.Close(context.Background())
		for id, want := range map[string]State{"ses_run": StateLost, "ses_idle": StateSuspended, "ses_shell": StateLost} {
			if s, _ := v.Session(t.Context(), id); s.State != want { //nolint:errcheck // the state check covers it
				t.Errorf("%s is %s, want %s", id, s.State, want)
			}
		}
	})
}

func TestSlowReaderGetsEveryEvent(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		v := open(t, t.TempDir(), scriptFlood, nil)
		defer v.Close(context.Background())
		st, err := v.Events(t.Context(), Filter{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "go"})
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, v, s.ID, StateIdle)
		events := collect(t, st, func(e Event) bool { return e.Type == EventSessionState && e.State == StateIdle })
		checkSeq(t, events, 0)
		if len(events) < 6000 {
			t.Fatalf("got %d events", len(events))
		}
	})
}

func TestEnvValuesStayInside(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		const secret = "s3cr3t-value-0042"
		var logs bytes.Buffer
		home := t.TempDir()
		v := open(t, home, scriptTurns, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hi", Env: map[string]string{"API_TOKEN": secret}})
		if err != nil {
			t.Fatal(err)
		}
		got := waitState(t, v, s.ID, StateIdle)
		if len(got.EnvKeys) != 1 || got.EnvKeys[0] != "API_TOKEN" {
			t.Fatalf("env keys: %v", got.EnvKeys)
		}
		api, _ := json.Marshal(got) //nolint:errchkjson // a plain struct
		if err := v.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		spec, err := os.Stat(filepath.Join(home, "sessions", s.ID, "spec.json"))
		if err != nil || spec.Mode().Perm() != 0o600 {
			t.Fatalf("spec.json mode: %v %v", spec, err)
		}
		db, err := os.ReadFile(filepath.Join(home, "visor.db"))
		if err != nil {
			t.Fatal(err)
		}
		wal, _ := os.ReadFile(filepath.Join(home, "visor.db-wal")) //nolint:errcheck // the file can be gone after a checkpoint
		for name, b := range map[string][]byte{"api": api, "logs": logs.Bytes(), "db": db, "wal": wal} {
			if strings.Contains(string(b), secret) {
				t.Errorf("the secret is in %s", name)
			}
		}
	})
}

func TestStartSpecValidate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		spec StartSpec
		ok   bool
	}{
		"agent":          {StartSpec{Harness: "claude"}, true},
		"shell":          {StartSpec{Role: RoleShell}, true},
		"agent no tool":  {StartSpec{Role: RoleAgent}, false},
		"wire no tool":   {StartSpec{Kind: KindWire}, false},
		"observed":       {StartSpec{Kind: KindObserved, Harness: "claude"}, false},
		"reserved label": {StartSpec{Harness: "claude", Labels: map[string]string{"visor.x": "1"}}, false},
		"bad restart":    {StartSpec{Harness: "claude", Restart: RestartPolicy{Mode: "again"}}, false},
	} {
		if err := tc.spec.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

func TestStateMoves(t *testing.T) {
	t.Parallel()
	all := []State{StateStarting, StateRunning, StateWaiting, StateIdle, StateSuspended, StateLost, StateExited}
	allowed := map[State]string{
		StateStarting:  "running waiting idle suspended lost exited",
		StateRunning:   "waiting idle suspended lost exited",
		StateWaiting:   "running idle suspended lost exited",
		StateIdle:      "running waiting suspended lost exited",
		StateSuspended: "starting exited",
		StateLost:      "starting exited",
		StateExited:    "",
	}
	for _, from := range all {
		for _, to := range all {
			want := from == to || strings.Contains(" "+allowed[from]+" ", " "+string(to)+" ")
			if got := from.CanMove(to); got != want {
				t.Errorf("%s -> %s: %v, want %v", from, to, got, want)
			}
		}
	}
}

func newTestVisor(t *testing.T, home, script string, log *slog.Logger) *Visor {
	t.Helper()
	v, err := New(t.Context(), Options{
		Home: home, Client: "test", Logger: log,
		Drivers: map[string]agentwire.Driver{"fake": agentwire.FakeDriver{Script: script}},
	})
	if err != nil {
		t.Fatalf("new visor: %v", err)
	}
	return v
}

// newServedVisor serves a visor on a temporary socket and dials it.
func newServedVisor(t *testing.T, home, script string, log *slog.Logger) api {
	t.Helper()
	sock := shortSocket(t)
	ts := serveTest(t, newTestVisor(t, home, script, log), sock)
	return &served{Client: dialTest(t, sock), ts: ts}
}

// eachHost runs fn on a Visor in the process and on a Client over a socket.
func eachHost(t *testing.T, fn func(t *testing.T, open opener)) {
	t.Helper()
	for name, open := range map[string]opener{
		"visor": func(t *testing.T, home, script string, log *slog.Logger) api {
			return newTestVisor(t, home, script, log)
		},
		"client": newServedVisor,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fn(t, open)
		})
	}
}

func waitState(t *testing.T, v api, id string, want State) Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := v.Session(t.Context(), id)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		if s.State == want {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s is %s, want %s", id, s.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// collect reads events until one matches stop.
func collect(t *testing.T, st *EventStream, stop func(Event) bool) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var out []Event
	for {
		e, err := st.Next(ctx)
		if err != nil {
			t.Fatalf("next event after %d events: %v", len(out), err)
		}
		out = append(out, e)
		if stop(e) {
			return out
		}
	}
}

func checkSeq(t *testing.T, events []Event, after uint64) {
	t.Helper()
	for i, e := range events {
		if e.Seq != after+uint64(i)+1 {
			t.Fatalf("event %d has seq %d, want %d (gap or duplicate)", i, e.Seq, after+uint64(i)+1)
		}
	}
}
