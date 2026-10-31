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

// SetHarnessUpdates sets the update mode of a harness.
func (c *Client) SetHarnessUpdates(ctx context.Context, name string, mode UpdateMode, version string) (Harness, error) {
	var out Harness
	return out, c.do(ctx, http.MethodPut, harnessPath(name)+"/updates", updatesRequest{Mode: mode, Version: version}, &out)
}

// CheckHarnesses looks for new releases now and applies the update modes.
func (c *Client) CheckHarnesses(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/harnesses/check", nil, nil)
}

// LoginHarness starts the vendor login and gives the URL or the code.
func (c *Client) LoginHarness(ctx context.Context, name string, spec LoginSpec) (Login, error) {
	var out Login
	return out, c.do(ctx, http.MethodPost, harnessPath(name)+"/login", spec, &out)
}

// HarnessLogin gives the login that runs, or the last one for a minute
// after its end.
func (c *Client) HarnessLogin(ctx context.Context, name string) (Login, error) {
	var out Login
	return out, c.do(ctx, http.MethodGet, harnessPath(name)+"/login", nil, &out)
}

// HarnessLoginCode types a pasted code into a terminal login.
func (c *Client) HarnessLoginCode(ctx context.Context, name, code string) (Login, error) {
	var out Login
	return out, c.do(ctx, http.MethodPost, harnessPath(name)+"/login/code", codeRequest{Code: code}, &out)
}

// HarnessLoginToken keeps a token or an API key as the login of a harness.
func (c *Client) HarnessLoginToken(ctx context.Context, name, token string) (Harness, error) {
	var out Harness
	return out, c.do(ctx, http.MethodPost, harnessPath(name)+"/login/token", tokenRequest{Token: token}, &out)
}

// CancelHarnessLogin stops a running login.
func (c *Client) CancelHarnessLogin(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, harnessPath(name)+"/login", nil, nil)
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

// HarnessFiles lists the home files of a harness and their share state.
func (c *Client) HarnessFiles(ctx context.Context, name string) ([]HomeFile, error) {
	var out []HomeFile
	return out, c.do(ctx, http.MethodGet, harnessPath(name)+"/files", nil, &out)
}

// ShareHarnessFile makes the harness home use the user file.
func (c *Client) ShareHarnessFile(ctx context.Context, name, key string) (HomeFile, error) {
	var out HomeFile
	return out, c.do(ctx, http.MethodPost, filePath(name, key)+"/share", nil, &out)
}

// OwnHarnessFile gives the harness home its own copy of a file.
func (c *Client) OwnHarnessFile(ctx context.Context, name, key string, start OwnStart) (HomeFile, error) {
	var out HomeFile
	return out, c.do(ctx, http.MethodPost, filePath(name, key)+"/own", ownRequest{Start: start}, &out)
}

// ReadHarnessFile reads one side of a home file. An empty side is the one
// that a session uses.
func (c *Client) ReadHarnessFile(ctx context.Context, name, key, side string) (FileContent, error) {
	var out FileContent
	path := filePath(name, key)
	if side != "" {
		path += "?side=" + url.QueryEscape(side)
	}
	return out, c.do(ctx, http.MethodGet, path, nil, &out)
}

// WriteHarnessFile replaces the own copy of a home file. An old etag gives
// ErrStale.
func (c *Client) WriteHarnessFile(ctx context.Context, name, key, content, ifMatch string) (FileContent, error) {
	var out FileContent
	return out, c.do(ctx, http.MethodPut, filePath(name, key), fileWrite{Content: content, IfMatch: ifMatch}, &out)
}

func filePath(name, key string) string { return harnessPath(name) + "/files/" + url.PathEscape(key) }

func harnessPath(name string) string { return "/v1/harnesses/" + url.PathEscape(name) }
