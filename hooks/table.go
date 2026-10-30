package hooks

import (
	"encoding/json"
	"fmt"
)

// table edits the hook table of a config file: hooks.<Event>[] of groups
// {matcher, hooks[]} of handlers {type, command, timeout}. Claude Code
// settings.json and Codex hooks.json have the same shape.
type table struct {
	events []hookEvent
	// timeout is in seconds.
	timeout int
}

// place is where the handler of the visor is in the table.
type place struct {
	group, handler, version int
}

type handlerRef struct {
	raw     json.RawMessage
	command string
}

// install puts the group of the visor in each event. An old group of the
// visor is replaced in place, so a second install keeps the positions.
func (t table) install(m marker, cfg []byte, cmd string) ([]byte, error) {
	root, hooks, err := t.read(cfg)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, mem := range hooks {
		e, want := t.wants(mem.Key)
		groups, err := groupsOf(mem.Value)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", mem.Key, err)
		}
		var mine json.RawMessage
		if want {
			seen[mem.Key] = true
			if mine, err = t.group(e, cmd); err != nil {
				return nil, err
			}
		}
		if hooks[i].Value, err = replaceOurs(m, groups, mine); err != nil {
			return nil, err
		}
	}
	for _, e := range t.events {
		if seen[e.Name] {
			continue
		}
		g, err := t.group(e, cmd)
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, member{e.Name, json.RawMessage("[" + string(g) + "]")})
	}
	return t.write(root, hooks)
}

// remove takes out only the handlers of the visor. An event with no groups
// left and an empty table go away.
func (t table) remove(m marker, cfg []byte) ([]byte, error) {
	root, hooks, err := t.read(cfg)
	if err != nil {
		return nil, err
	}
	kept := object{}
	for _, mem := range hooks {
		groups, err := groupsOf(mem.Value)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", mem.Key, err)
		}
		v, err := replaceOurs(m, groups, nil)
		if err != nil {
			return nil, err
		}
		if string(v) != "[]" || len(groups) == 0 {
			kept = append(kept, member{mem.Key, v})
		}
	}
	return t.write(root, kept)
}

// places finds the handler of the visor in each event of the table.
func (t table) places(m marker, cfg []byte) (map[string]place, error) {
	_, hooks, err := t.read(cfg)
	if err != nil {
		return nil, err
	}
	out := map[string]place{}
	for _, mem := range hooks {
		groups, err := groupsOf(mem.Value)
		if err != nil {
			return nil, err
		}
		for gi, g := range groups {
			handlers, err := handlersOf(g)
			if err != nil {
				return nil, err
			}
			for hi, h := range handlers {
				if v, ok := m.version(h.command); ok {
					if _, dup := out[mem.Key]; !dup {
						out[mem.Key] = place{gi, hi, v}
					}
				}
			}
		}
	}
	return out, nil
}

// state compares the places with the events of the table.
func (t table) state(p map[string]place) (installed, outdated bool) {
	if len(p) == 0 {
		return false, false
	}
	for _, e := range t.events {
		if pl, ok := p[e.Name]; !ok || pl.version != Version {
			return true, true
		}
	}
	for name := range p {
		if _, ok := t.wants(name); !ok {
			return true, true
		}
	}
	return true, false
}

func (t table) group(e hookEvent, cmd string) (json.RawMessage, error) {
	type handler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	type group struct {
		Matcher string    `json:"matcher,omitempty"`
		Hooks   []handler `json:"hooks"`
	}
	return marshal(group{e.Matcher, []handler{{"command", cmd, t.timeout}}})
}

func (t table) wants(event string) (hookEvent, bool) {
	for _, e := range t.events {
		if e.Name == event {
			return e, true
		}
	}
	return hookEvent{}, false
}

func (t table) read(cfg []byte) (root, hooks object, err error) {
	if root, err = parseObject(cfg); err != nil {
		return nil, nil, err
	}
	raw, ok := root.get("hooks")
	if !ok || string(raw) == "null" {
		return root, object{}, nil
	}
	if hooks, err = parseObject(raw); err != nil {
		return nil, nil, fmt.Errorf("hooks: %w", err)
	}
	return root, hooks, nil
}

func (t table) write(root, hooks object) ([]byte, error) {
	if len(hooks) == 0 {
		return root.without("hooks").file()
	}
	raw, err := hooks.compact()
	if err != nil {
		return nil, err
	}
	return root.set("hooks", raw).file()
}

func groupsOf(raw json.RawMessage) ([]json.RawMessage, error) {
	var groups []json.RawMessage
	if string(raw) == "null" {
		return nil, nil
	}
	err := json.Unmarshal(raw, &groups)
	return groups, err
}

func handlersOf(group json.RawMessage) ([]handlerRef, error) {
	g, err := parseObject(group)
	if err != nil {
		return nil, err
	}
	raw, ok := g.get("hooks")
	if !ok {
		return nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]handlerRef, 0, len(list))
	for _, h := range list {
		var c struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(h, &c) //nolint:errcheck // a handler with no command is not ours
		out = append(out, handlerRef{h, c.Command})
	}
	return out, nil
}

// replaceOurs takes the handlers of the visor out of the groups. mine, when
// set, goes at the place of the first group of the visor, else at the end.
// A user group that also holds a handler of the visor keeps its other
// handlers.
func replaceOurs(m marker, groups []json.RawMessage, mine json.RawMessage) (json.RawMessage, error) {
	out := []json.RawMessage{}
	placed := false
	for _, g := range groups {
		handlers, err := handlersOf(g)
		if err != nil {
			return nil, err
		}
		var rest []json.RawMessage
		ours := false
		for _, h := range handlers {
			if _, ok := m.version(h.command); ok {
				ours = true
			} else {
				rest = append(rest, h.raw)
			}
		}
		if !ours {
			out = append(out, g)
			continue
		}
		if mine != nil && !placed {
			out = append(out, mine)
			placed = true
		}
		if len(rest) > 0 {
			obj, err := parseObject(g)
			if err != nil {
				return nil, err
			}
			list, err := marshal(rest)
			if err != nil {
				return nil, err
			}
			c, err := obj.set("hooks", list).compact()
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	if mine != nil && !placed {
		out = append(out, mine)
	}
	return marshal(out)
}
