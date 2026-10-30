package agentvisor

import (
	"context"
	"net/http"
	"net/url"
)

// Hook applies one hook call. The session is empty when the call changes no
// session.
func (c *Client) Hook(ctx context.Context, call HookCall) (Session, error) {
	var out Session
	return out, c.do(ctx, http.MethodPost, "/v1/hooks/"+url.PathEscape(call.Harness), call, &out)
}

// Hooks gives the hook state of the vendor configs in spec.
func (c *Client) Hooks(ctx context.Context, spec HookSpec) ([]HookStatus, error) {
	var out []HookStatus
	return out, c.do(ctx, http.MethodGet, "/v1/hooks?"+spec.values().Encode(), nil, &out)
}

// InstallHooks adds or updates the hook entries of the visor in the vendor
// configs.
func (c *Client) InstallHooks(ctx context.Context, spec HookSpec) ([]HookStatus, error) {
	var out []HookStatus
	return out, c.do(ctx, http.MethodPut, "/v1/hooks", spec, &out)
}

// RemoveHooks takes the hook entries of the visor out of the vendor configs.
func (c *Client) RemoveHooks(ctx context.Context, spec HookSpec) ([]HookStatus, error) {
	var out []HookStatus
	return out, c.do(ctx, http.MethodDelete, "/v1/hooks?"+spec.values().Encode(), nil, &out)
}
