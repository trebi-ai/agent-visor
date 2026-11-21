// Package hooks reads the hook calls of vendor agent CLIs and edits their
// hook config files. It does not import the visor.
package hooks

import (
	"fmt"
	"path/filepath"

	agentwire "github.com/trebi-ai/agent-wire"
)

// vendor is the hook facts of one harness from agent-wire, plus the table
// that the visor installs.
type vendor struct {
	agentwire.HookRecipe
	table table
}

// hookTimeout is the timeout of each installed hook, in seconds.
const hookTimeout = 5

// Names lists the harnesses with hook support, sorted.
func Names() []string {
	hs := agentwire.HookHarnesses()
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = string(h)
	}
	return out
}

// Supported reports whether the harness has hook support.
func Supported(harness string) bool {
	_, ok := agentwire.Hooks(agentwire.Harness(harness))
	return ok
}

// ConfigPath gives the hook config file in the vendor home dir.
func ConfigPath(harness, dir string) (string, error) {
	v, err := lookup(harness)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, v.Config), nil
}

// lookup gives the vendor of a harness. Codex runs a new hook only after the
// user trusts it in Codex. The visor never writes that trust.
func lookup(harness string) (vendor, error) {
	r, ok := agentwire.Hooks(agentwire.Harness(harness))
	if !ok {
		return vendor{}, fmt.Errorf("%w: harness %q", ErrUnsupported, harness)
	}
	return vendor{HookRecipe: r, table: table{events: r.Events, timeout: hookTimeout}}, nil
}
