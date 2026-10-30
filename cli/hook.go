package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/urfave/cli/v3"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/agent-visor/hooks"
)

// hookVisor is the hook API of the visor.
type hookVisor interface {
	Hook(ctx context.Context, c agentvisor.HookCall) (agentvisor.Session, error)
}

// hookWait limits the call to the visor. The vendor waits for the hook.
const hookWait = 3 * time.Second

var _ hookVisor = (*agentvisor.Client)(nil)

// hook sends the payload on stdin to the visor. When the visor does not
// answer, it keeps the call in the spool of the home. A call that the visor
// rejects does not fail the vendor CLI.
func (a *app) hook(ctx context.Context, cmd *cli.Command) error {
	if err := args(cmd, 1, "<harness>"); err != nil {
		return err
	}
	payload, err := io.ReadAll(io.LimitReader(a.opts.Stdin, hooks.MaxPayload+1))
	if err != nil {
		return fmt.Errorf("read the hook payload: %w", err)
	}
	if len(payload) > hooks.MaxPayload || !json.Valid(payload) {
		return fmt.Errorf("%w: the hook payload is not JSON of at most %d bytes", agentvisor.ErrInvalid, hooks.MaxPayload)
	}
	c := agentvisor.HookCall{Harness: cmd.Args().First(), Session: cmd.String("session"), Payload: payload, At: time.Now().UTC()}
	err = a.sendHook(ctx, cmd, c)
	switch {
	case err == nil:
		return nil
	case rejected(err):
		_, _ = fmt.Fprintln(a.opts.Stderr, "the visor rejects the hook call:", err) // only a note
		return nil
	}
	home := cmd.String("home")
	if home == "" {
		return fmt.Errorf("send the hook call: %w", err)
	}
	if serr := agentvisor.NewHookSpool(home).Write(c); serr != nil {
		return errors.Join(fmt.Errorf("send the hook call: %w", err), serr)
	}
	return nil
}

func (a *app) sendHook(ctx context.Context, cmd *cli.Command, c agentvisor.HookCall) error {
	ctx, cancel := context.WithTimeout(ctx, hookWait)
	defer cancel()
	return a.with(ctx, cmd, func(v Visor) error {
		h, ok := v.(hookVisor)
		if !ok {
			return a.notAvailable(ctx, cmd)
		}
		_, err := h.Hook(ctx, c)
		return err
	})
}

// rejected reports whether the visor read the call and refused it. A spool
// does not help such a call.
func rejected(err error) bool {
	for _, e := range []error{agentvisor.ErrInvalid, agentvisor.ErrNotFound, agentvisor.ErrUnsupported, agentvisor.ErrState, errNotAvailable} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
