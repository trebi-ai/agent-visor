package agentvisor

import (
	"context"
	"fmt"
	"sync"

	agentwire "github.com/trebi-ai/agent-wire"
)

// wireBackend hosts agent-wire sessions in the visor process.
type wireBackend struct {
	rt *agentwire.Runtime
}

type wireHandle struct {
	id   string
	s    agentwire.Session
	ch   chan Event
	once sync.Once
}

var (
	_ Backend = (*wireBackend)(nil)
	_ Handle  = (*wireHandle)(nil)
)

func newWireBackend(rt *agentwire.Runtime) *wireBackend {
	return &wireBackend{rt: rt}
}

func (b *wireBackend) Kind() Kind { return KindWire }

func (b *wireBackend) Caps(string) Caps {
	return Caps{Structured: true, Permissions: true, Steer: true, Resume: true}
}

func (b *wireBackend) Start(ctx context.Context, l Launch) (Handle, error) {
	req := agentwire.StartRequest{
		Harness: agentwire.Harness(l.Spec.Harness), Binary: l.Binary, WorkingDir: l.Dir, Env: l.Env, Home: l.Home,
		Model: l.Spec.Model, Effort: l.Spec.Effort, Permissions: l.Spec.Permissions.wire(), MCPServers: wireMCPServers(l.Spec.MCPServers),
		Skills: wireSkills(l.Spec.Skills), Instructions: l.Spec.Instructions, ExtraArgs: l.Spec.ExtraArgs, Raw: l.Spec.Raw,
	}
	var (
		s   agentwire.Session
		err error
	)
	if l.Resume {
		req.SessionID = l.Session.VendorID
		s, err = b.rt.Resume(ctx, req)
	} else {
		s, err = b.rt.Start(ctx, req)
	}
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", l.Spec.Harness, err)
	}
	h := &wireHandle{id: l.Session.ID, s: s, ch: make(chan Event, 64)}
	go h.pump()
	if l.Prompt != nil {
		if err := h.Send(ctx, *l.Prompt); err != nil {
			_ = h.Close(context.WithoutCancel(ctx)) // the send error is the one to report
			return nil, err
		}
	}
	return h, nil
}

// Recover gives nothing: a wire session dies with the visor process.
func (b *wireBackend) Recover(context.Context) ([]Handle, error) { return nil, nil }

func (b *wireBackend) Close(ctx context.Context) error { return b.rt.Close(ctx) }

func (h *wireHandle) Session() string { return h.id }

func (h *wireHandle) VendorID() string { return h.s.ID() }

func (h *wireHandle) PID() int { return h.s.PID() }

func (h *wireHandle) Send(ctx context.Context, in Input) error {
	_, err := agentwire.Send(ctx, h.s, agentwire.Prompt{Text: in.Text, Kind: in.Kind, Attachments: wireAttachments(in.Attachments)})
	if err != nil {
		return fmt.Errorf("send to %s: %w", h.id, err)
	}
	return nil
}

func (h *wireHandle) Interrupt(ctx context.Context) error { return h.s.Interrupt(ctx) }

func (h *wireHandle) Answer(ctx context.Context, requestID string, a Answer) error {
	return h.s.AnswerPermission(ctx, requestID, agentwire.Decision{Allow: a.Allow, Message: a.Message, UpdatedInput: a.UpdatedInput})
}

func (h *wireHandle) Close(ctx context.Context) error {
	var err error
	h.once.Do(func() { err = h.s.Close(ctx) })
	return err
}

func (h *wireHandle) Events() <-chan Event { return h.ch }

func (h *wireHandle) pump() {
	defer close(h.ch)
	for e := range h.s.Events() {
		h.ch <- wireEvent(e)
	}
}

func (p Permissions) wire() agentwire.PermissionPolicy {
	return agentwire.PermissionPolicy{Mode: p.Mode, AllowTools: p.AllowTools}
}

func wireMCPServers(in []MCPServer) []agentwire.MCPServer {
	var out []agentwire.MCPServer
	for _, m := range in {
		out = append(out, agentwire.MCPServer{Name: m.Name, Command: m.Command, Args: m.Args, Env: m.Env, URL: m.URL, Headers: m.Headers})
	}
	return out
}

func wireSkills(in []Skill) []agentwire.Skill {
	var out []agentwire.Skill
	for _, s := range in {
		out = append(out, agentwire.Skill{Name: s.Name, Dir: s.Dir})
	}
	return out
}

func wireAttachments(in []Attachment) []agentwire.Attachment {
	var out []agentwire.Attachment
	for _, a := range in {
		out = append(out, agentwire.Attachment{MIME: a.MIME, Path: a.Path, Data: a.Data})
	}
	return out
}
