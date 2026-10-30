package agentvisor

import (
	"context"
	"fmt"
)

// Handover moves an agent session to another kind. The session id and the
// vendor session stay. A wire session moves to pty at the end of its turn;
// the terminal program continues the vendor session. A pty session moves to
// wire when its terminal program exits; Handover waits for that exit. One
// process at a time writes to the vendor session.
func (v *Visor) Handover(ctx context.Context, id string, to Kind) error {
	rec, _, err := v.lookup(id)
	if err != nil {
		return err
	}
	v.mu.Lock()
	s := rec.s.clone()
	v.mu.Unlock()
	switch {
	case s.Kind == to:
		return nil
	case s.Role != RoleAgent || s.Harness == "":
		return fmt.Errorf("%w: session %s is not an agent session", ErrInvalid, id)
	case s.State == StateExited:
		return fmt.Errorf("%w: session %s exited", ErrState, id)
	case s.VendorID == "":
		return fmt.Errorf("%w: session %s has no vendor session yet", ErrState, id)
	case s.Kind == KindWire && to == KindPTY:
		return v.handoverToPTY(ctx, rec, s)
	case s.Kind == KindPTY && to == KindWire:
		return v.handoverToWire(ctx, rec)
	}
	return fmt.Errorf("%w: handover from %s to %s", ErrUnsupported, s.Kind, to)
}

// handoverToPTY stops the wire process at the end of the turn, then starts
// the terminal program with the resume command.
func (v *Visor) handoverToPTY(ctx context.Context, rec *record, s Session) error {
	b, ok := v.backends[KindPTY]
	if !ok {
		return fmt.Errorf("%w: no backend for kind %s", ErrUnsupported, KindPTY)
	}
	spec, err := newSpecFile(v.opts.Home, s.ID).read()
	if err != nil {
		return err
	}
	w, err := v.workdirs.Workdir(ctx, s.Workdir)
	if err != nil {
		return mapWorkdirErr(err)
	}
	// Check the command first, so that a harness with no resume command
	// changes nothing.
	if pb, ok := b.(*ptyBackend); ok {
		if _, err := pb.command(Launch{Session: s, Spec: spec, Resume: true}, nil); err != nil {
			return err
		}
	}
	h, done, err := v.claimHandover(ctx, rec, KindPTY)
	if err != nil {
		return err
	}
	defer v.clearHandover(rec, KindPTY)
	if h != nil {
		if err := h.Close(ctx); err != nil {
			v.log.Warn("close the wire process for a handover", "session_id", s.ID, "err", err)
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	v.update(ctx, rec, func(s *Session) { s.Kind, s.ExitCode = KindPTY, nil })
	if err := v.setState(ctx, rec, StateStarting); err != nil {
		return err
	}
	return v.launch(ctx, rec, b, spec, w, true, nil)
}

// handoverToWire marks the session, so that it becomes a suspended wire
// session when the terminal program exits. The next send resumes it.
func (v *Visor) handoverToWire(ctx context.Context, rec *record) error {
	if _, ok := v.backends[KindWire]; !ok {
		return fmt.Errorf("%w: no backend for kind %s", ErrUnsupported, KindWire)
	}
	v.mu.Lock()
	if rec.handover != "" {
		v.mu.Unlock()
		return fmt.Errorf("%w: session %s moves to %s", ErrBusy, rec.s.ID, rec.handover)
	}
	if rec.h == nil {
		rec.s.Kind, rec.s.ExitCode = KindWire, nil
		v.mu.Unlock()
		return v.save(ctx, rec)
	}
	rec.handover = KindWire
	done := rec.done
	v.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
	v.clearHandover(rec, KindWire)
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case rec.s.Kind == KindWire:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("%w: session %s ended before the handover", ErrState, rec.s.ID)
}

// claimHandover waits until the session is not in a turn, and marks it. It
// gives the live handle, which ends with the state suspended.
func (v *Visor) claimHandover(ctx context.Context, rec *record, to Kind) (Handle, chan struct{}, error) {
	id := rec.s.ID
	match := func(e Event) bool { return e.Session == id && e.Type == EventSessionState }
	sub, _ := v.events.subscribe(match)
	defer func() { v.events.unsubscribe(sub) }()
	for {
		if v.isClosed() {
			return nil, nil, ErrClosed
		}
		v.mu.Lock()
		if rec.handover != "" {
			v.mu.Unlock()
			return nil, nil, fmt.Errorf("%w: session %s moves to %s", ErrBusy, id, rec.handover)
		}
		if rec.h == nil || !rec.s.State.Busy() {
			rec.handover = to
			h, done := rec.h, rec.done
			if h != nil {
				rec.ending = StateSuspended
			}
			v.mu.Unlock()
			return h, done, nil
		}
		v.mu.Unlock()
		select {
		case _, ok := <-sub.ch:
			if !ok {
				sub, _ = v.events.subscribe(match)
			}
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

func (v *Visor) clearHandover(rec *record, to Kind) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if rec.handover == to {
		rec.handover = ""
	}
}

// handoverBusy refuses a send while the session moves to a terminal.
func (v *Visor) handoverBusy(rec *record) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if rec.handover == KindPTY {
		return fmt.Errorf("%w: session %s moves to %s", ErrBusy, rec.s.ID, KindPTY)
	}
	return nil
}
