package agentvisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// Env keys that the visor adds to each session.
const (
	EnvSession = "AGENT_VISOR_SESSION"
	EnvWorkdir = "AGENT_VISOR_WORKDIR"
	EnvPort    = "AGENT_VISOR_PORT"
	EnvSocket  = "AGENT_VISOR_SOCKET"
)

// specFile keeps the start spec of one session, with its env, mode 0600.
type specFile struct {
	dir string
}

// mergeEnv layers the env. The last layer wins.
func mergeEnv(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, l := range layers {
		maps.Copy(out, l)
	}
	return out
}

// visorEnv gives the values that the visor adds to a session.
func visorEnv(s Session, w Workdir, socket string) map[string]string {
	env := map[string]string{
		EnvSession: s.ID,
		EnvWorkdir: w.ID,
		EnvPort:    strconv.Itoa(w.PortBase),
	}
	if socket != "" {
		env[EnvSocket] = socket
	}
	return env
}

func envKeys(env map[string]string) []string {
	return slices.Sorted(maps.Keys(env))
}

func newSpecFile(home, id string) specFile {
	return specFile{dir: filepath.Join(home, "sessions", id)}
}

func (f specFile) write(s StartSpec) error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("write spec: %w", err)
	}
	tmp := filepath.Join(f.dir, "spec.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}
	return os.Rename(tmp, filepath.Join(f.dir, "spec.json"))
}

func (f specFile) read() (StartSpec, error) {
	var s StartSpec
	data, err := os.ReadFile(filepath.Join(f.dir, "spec.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, fmt.Errorf("%w: the session has no start spec", ErrState)
	}
	if err != nil {
		return s, fmt.Errorf("read spec: %w", err)
	}
	return s, json.Unmarshal(data, &s)
}

func (f specFile) remove() error { return os.RemoveAll(f.dir) }
