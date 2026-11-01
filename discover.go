package agentvisor

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"time"
)

// discover adopts the found sessions of each Discoverer backend, and makes a
// project for each folder of each Catalog backend.
func (v *Visor) discover() {
	for _, b := range v.backends {
		if c, ok := b.(Catalog); ok {
			v.wg.Add(1)
			go func() {
				defer v.wg.Done()
				v.catalog(v.ctx, c)
			}()
		}
		d, ok := b.(Discoverer)
		if !ok {
			continue
		}
		v.wg.Add(1)
		go func() {
			defer v.wg.Done()
			for {
				select {
				case f, ok := <-d.Found():
					if !ok {
						return
					}
					if err := v.adopt(v.ctx, b.Kind(), d, f); err != nil {
						v.log.Warn("adopt a found session", "kind", b.Kind(), "key", f.Key, "err", err)
					}
				case <-v.quit:
					return
				}
			}
		}()
	}
}

func (v *Visor) catalog(ctx context.Context, c Catalog) {
	dirs, err := c.Folders(ctx)
	if err != nil {
		v.log.Warn("list the backend folders", "err", err)
		return
	}
	for _, dir := range dirs {
		if _, err := v.workdirs.ForPath(ctx, dir); err != nil {
			v.log.Debug("make a project of a backend folder", "dir", dir, "err", err)
		}
	}
}

// adopt makes a session for f, or takes the observed session with its
// labels, and binds the backend handle to it.
func (v *Visor) adopt(ctx context.Context, kind Kind, d Discoverer, f Found) error {
	if !filepath.IsAbs(f.Dir) {
		return fmt.Errorf("%w: found session has no absolute folder", ErrInvalid)
	}
	if rec := v.labeled(f.Labels); rec != nil {
		return nil
	}
	rec := v.observedWithin(f.Labels)
	if rec != nil {
		v.update(ctx, rec, func(s *Session) {
			s.Kind, s.Title = kind, f.Title
			s.Harness = cmp.Or(s.Harness, f.Harness)
			maps.Copy(s.Labels, f.Labels)
		})
	} else {
		w, err := v.workdirs.ForPath(ctx, f.Dir)
		if err != nil {
			return fmt.Errorf("%w: workdir of the found session: %w", ErrInvalid, err)
		}
		now := time.Now().UTC()
		rec = &record{s: Session{
			ID: newID("ses"), Project: w.Project, Workdir: w.ID, Kind: kind, Role: RoleAgent, Harness: f.Harness,
			Title: f.Title, WorkingDir: f.Dir, State: StateStarting, Labels: maps.Clone(f.Labels),
			Restart: RestartPolicy{Mode: RestartNotify}, CreatedAt: now, UpdatedAt: now,
		}}
		v.mu.Lock()
		if v.closed {
			v.mu.Unlock()
			return ErrClosed
		}
		v.sessions[rec.s.ID] = rec
		v.mu.Unlock()
		if err := v.save(ctx, rec); err != nil {
			return err
		}
		v.emit(ctx, rec, Event{Type: EventSessionState, State: StateStarting})
	}
	v.mu.Lock()
	s := rec.s.clone()
	v.mu.Unlock()
	h, err := d.Adopt(ctx, s, f.Key)
	if err != nil {
		v.mu.Lock()
		rec.s.Error = err.Error()
		v.mu.Unlock()
		if serr := v.setState(ctx, rec, StateExited); serr != nil {
			v.log.Warn("mark a failed adoption exited", "session_id", s.ID, "err", serr)
		}
		return err
	}
	v.attach(rec, h)
	return nil
}

// observedWithin gives the newest observed session whose labels are all in
// labels. A hook gives fewer labels than the backend finds.
func (v *Visor) observedWithin(labels map[string]string) *record {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out *record
	for _, rec := range v.sessions {
		s := rec.s
		if s.Kind == KindObserved && s.State != StateExited && len(s.Labels) > 0 && matchLabels(s.Labels, labels) &&
			(out == nil || s.CreatedAt.After(out.s.CreatedAt)) {
			out = rec
		}
	}
	return out
}
