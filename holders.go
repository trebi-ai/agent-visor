package agentvisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/trebi-ai/agent-visor/holder"
)

// holders starts holder processes and connects to them. Each backend has
// its own folder of sockets, exit files, and spools.
type holders struct {
	home string
	dir  string
	cmd  []string
}

func newHolders(home, dir string, cmd []string) *holders {
	return &holders{home: home, dir: filepath.Join(home, dir), cmd: cmd}
}

func (hs *holders) socket(id string) string { return filepath.Join(hs.dir, id+".sock") }

func (hs *holders) exitFile(id string) string { return filepath.Join(hs.dir, id+".exit") }

func (hs *holders) spool(id string) string { return filepath.Join(hs.dir, id+".spool") }

// spawn starts the holder process in its own process session, so that it
// outlives the visor. A nil env gives the holder the env of the visor. The
// channel gives the end of the process.
func (hs *holders) spawn(ctx context.Context, cfg holder.Config, env []string) (<-chan error, error) {
	if len(hs.cmd) == 0 {
		return nil, fmt.Errorf("%w: no holder command", ErrUnsupported)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	dir := filepath.Join(hs.home, "sessions", cfg.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	logf, err := os.OpenFile(filepath.Join(dir, "holder.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	defer logf.Close()
	args := append(append([]string{}, hs.cmd[1:]...), cfg.ID)
	cmd := exec.CommandContext(context.WithoutCancel(ctx), hs.cmd[0], args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = bytes.NewReader(data), logf, logf, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}
	exited := make(chan error, 1)
	// The goroutine reaps the holder; it ends with the holder process.
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// dial waits for the socket of a new holder.
func (hs *holders) dial(ctx context.Context, id string, exited <-chan error, timeout time.Duration) (*holder.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(holderPoll)
	defer t.Stop()
	for {
		if _, err := os.Stat(hs.socket(id)); err == nil {
			if c, err := holder.Dial(ctx, hs.socket(id), holder.Cursor{}); err == nil {
				return c, nil
			}
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("holder of %s ended: %w: %s", id, err, hs.holderLog(id))
		case <-ctx.Done():
			return nil, fmt.Errorf("dial holder of %s: %w", id, ctx.Err())
		case <-t.C:
		}
	}
}

// scan gives the sessions with a holder socket and the sessions with only
// an exit file.
func (hs *holders) scan() (socks, exits []string, err error) {
	entries, err := os.ReadDir(hs.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("recover holders: %w", err)
	}
	live := map[string]bool{}
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".sock"); ok {
			live[id] = true
			socks = append(socks, id)
		}
	}
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".exit"); ok && !live[id] {
			exits = append(exits, id)
		}
	}
	return socks, exits, nil
}

// readExit reads the exit file that a holder wrote.
func (hs *holders) readExit(id string) (holder.Exit, bool) {
	var e holder.Exit
	data, err := os.ReadFile(hs.exitFile(id))
	if err != nil || json.Unmarshal(data, &e) != nil {
		return e, false
	}
	return e, true
}

// remove deletes the exit file and the spool of a session that ended.
func (hs *holders) remove(id string) error {
	var errs []error
	for _, p := range []string{hs.exitFile(id), hs.spool(id)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// holderLog gives the end of the log of a holder, for an error.
func (hs *holders) holderLog(id string) string {
	data, err := os.ReadFile(filepath.Join(hs.home, "sessions", id, "holder.log"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data[max(0, len(data)-512):]))
}
