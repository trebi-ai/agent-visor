package orca

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// poll reads the Orca state each Poll, and at once after a poke. When it
// ends, it detaches each handle.
func (b *Backend) poll(ctx context.Context) {
	defer close(b.done)
	defer close(b.found)
	defer b.detach()
	t := time.NewTicker(b.opts.Poll)
	defer t.Stop()
	for {
		b.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.wake:
		}
	}
}

// detach closes the events of each handle. The terminals keep running.
func (b *Backend) detach() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, h := range b.handles {
		close(h.events)
		delete(b.handles, id)
	}
}

// once reads the Orca state one time and applies it. When Orca does not
// answer, nothing changes.
func (b *Backend) once(ctx context.Context) {
	b.mu.Lock()
	idle := len(b.handles) == 0 && !b.opts.Discover
	b.mu.Unlock()
	if idle {
		return
	}
	wts, err := b.cli.Worktrees(ctx)
	var terms []Terminal
	if err == nil {
		terms, err = b.cli.Terminals(ctx)
	}
	if err != nil {
		if ctx.Err() == nil {
			b.log.Debug("read the orca state", "err", err)
		}
		return
	}
	agents, dirs := map[string]Agent{}, map[string]string{}
	for _, w := range wts {
		if w.IsArchived {
			continue
		}
		for _, a := range w.Agents {
			if a.PaneKey != "" {
				agents[a.PaneKey], dirs[a.PaneKey] = a, w.Path
			}
		}
	}
	byPane, byHandle := map[string]*Terminal{}, map[string]*Terminal{}
	for i := range terms {
		t := &terms[i]
		byHandle[t.Handle] = t
		if old, ok := byPane[t.PaneKey()]; !ok || old.Orphaned && !t.Orphaned {
			byPane[t.PaneKey()] = t
		}
	}
	b.apply(ctx, agents, byPane, byHandle)
	if b.opts.Discover {
		b.discover(ctx, agents, dirs, byPane)
	}
}

// apply gives each handle its events, and ends the handles whose session
// ended.
func (b *Backend) apply(ctx context.Context, agents map[string]Agent, byPane, byHandle map[string]*Terminal) {
	b.mu.Lock()
	handles := make([]*handle, 0, len(b.handles))
	for _, h := range b.handles {
		handles = append(handles, h)
	}
	b.mu.Unlock()
	for _, h := range handles {
		bd := h.binding()
		t := byHandle[bd.Terminal]
		if bd.Pane != "" && (t == nil || t.PaneKey() != bd.Pane) {
			t = byPane[bd.Pane]
		}
		if bd.Pane == "" && t != nil && t.PaneKey() != "" {
			h.mu.Lock()
			h.bd.Pane = t.PaneKey()
			h.mu.Unlock()
		}
		a, ok := agents[h.binding().Pane]
		events, end := h.see(a, ok, t)
		if nb := h.binding(); nb != bd {
			b.store(h.id, nb)
		}
		for _, e := range events {
			select {
			case h.events <- e:
			case <-ctx.Done():
				return
			}
		}
		if end {
			b.end(h)
		}
	}
}

// end forgets a handle whose session ended, and closes its events.
func (b *Backend) end(h *handle) {
	b.mu.Lock()
	if b.handles[h.id] != h {
		b.mu.Unlock()
		return
	}
	delete(b.handles, h.id)
	b.mu.Unlock()
	close(h.events)
	_ = os.Remove(filepath.Join(b.opts.Dir, h.id+".json"))         // the session ended
	_ = os.Remove(filepath.Join(b.opts.Dir, "launch", h.id+".sh")) // a script that never ran
}

// discover reports each agent pane that no handle has.
func (b *Backend) discover(ctx context.Context, agents map[string]Agent, dirs map[string]string, byPane map[string]*Terminal) {
	b.mu.Lock()
	bound := map[string]bool{}
	for _, h := range b.handles {
		bound[h.binding().Pane] = true
	}
	for pane := range b.reported {
		if _, ok := agents[pane]; !ok {
			delete(b.reported, pane)
		}
	}
	var found []agentvisor.Found
	for pane, a := range agents {
		t := byPane[pane]
		if bound[pane] || b.reported[pane] || t == nil || t.Orphaned || !filepath.IsAbs(dirs[pane]) {
			continue
		}
		labels := map[string]string{LabelPane: pane, LabelTerminal: t.Handle}
		if t.WorktreeID != "" {
			labels[LabelWorktree] = t.WorktreeID
		}
		found = append(found, agentvisor.Found{Key: pane, Harness: a.AgentType, Dir: dirs[pane], Title: title(t.Title), Labels: labels})
	}
	b.mu.Unlock()
	for _, f := range found {
		select {
		case b.found <- f:
			b.mu.Lock()
			b.reported[f.Key] = true
			b.mu.Unlock()
		case <-ctx.Done():
			return
		default:
			// The visor reads slowly: the next poll reports the rest.
			return
		}
	}
}

// title drops the status glyph that an agent puts in front of its title.
func title(s string) string {
	return strings.TrimSpace(strings.TrimLeftFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
}
