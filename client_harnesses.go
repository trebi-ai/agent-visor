package agentvisor

import (
	"context"
	"net/http"
	"net/url"
)

// Harnesses lists the harnesses that the visor knows.
func (c *Client) Harnesses(ctx context.Context) ([]Harness, error) {
	var out []Harness
	return out, c.do(ctx, http.MethodGet, "/v1/harnesses", nil, &out)
}

// Harness reads one harness.
func (c *Client) Harness(ctx context.Context, name string) (Harness, error) {
	var out Harness
	return out, c.do(ctx, http.MethodGet, harnessPath(name), nil, &out)
}

// InstallHarness installs a visor copy of a harness. An empty version is
// the latest.
func (c *Client) InstallHarness(ctx context.Context, name, version string) (Harness, error) {
	var out Harness
	return out, c.do(ctx, http.MethodPost, harnessPath(name)+"/install", versionRequest{Version: version}, &out)
}

// UpdateHarness updates a harness. An empty version is the latest.
func (c *Client) UpdateHarness(ctx context.Context, name, version string) (Harness, error) {
	var out Harness
	return out, c.do(ctx, http.MethodPost, harnessPath(name)+"/update", versionRequest{Version: version}, &out)
}

// SetHarnessAuth moves a harness to a shared or a separate login.
func (c *Client) SetHarnessAuth(ctx context.Context, name string, mode AuthMode) (Harness, error) {
	var out Harness
	return out, c.do(ctx, http.MethodPut, harnessPath(name)+"/auth", authRequest{Mode: mode}, &out)
}

// LoginHarness starts the vendor login and gives the URL or the code.
func (c *Client) LoginHarness(ctx context.Context, name string) (Login, error) {
	var out Login
	return out, c.do(ctx, http.MethodPost, harnessPath(name)+"/login", nil, &out)
}

// LogoutHarness logs a harness out.
func (c *Client) LogoutHarness(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, harnessPath(name)+"/logout", nil, nil)
}

// HarnessModels lists the models of a harness.
func (c *Client) HarnessModels(ctx context.Context, name string) ([]Model, error) {
	var out []Model
	return out, c.do(ctx, http.MethodGet, harnessPath(name)+"/models", nil, &out)
}

// SetHarnessEnv replaces the env of a harness.
func (c *Client) SetHarnessEnv(ctx context.Context, name string, env map[string]string) error {
	return c.do(ctx, http.MethodPut, harnessPath(name)+"/env", envRequest{Env: env}, nil)
}

func harnessPath(name string) string { return "/v1/harnesses/" + url.PathEscape(name) }
