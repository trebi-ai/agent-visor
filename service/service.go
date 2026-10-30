// Package service installs the visor as a user service and dials it. The
// visor has its own service, separate from the product service, because
// launchd stops the whole process group of a job.
package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// Service describes the visor service of one product.
type Service struct {
	// Label names the service, for example com.example.visor.
	Label string
	// Command starts the visor in the foreground, for example
	// [binary, "visor", "serve"].
	Command []string
	// Socket is the socket that the visor listens on.
	Socket string
	// UnitDir holds the unit file. See UserUnitDir.
	UnitDir string
	// LogPath gets the output of the visor process. Empty discards it.
	LogPath string
	// Env is the env of the visor process. The unit file has mode 0600.
	Env map[string]string
	// GOOS selects launchd (darwin) or systemd (linux). Empty is the OS of
	// this process.
	GOOS string
	// Wait is how long Ensure waits for the socket. Zero is 15 s.
	Wait time.Duration
	// Run runs a service manager command. Nil runs the program.
	Run func(ctx context.Context, name string, args ...string) error
}

// ErrUnsupportedOS means that the OS has no service manager that the package
// knows.
var ErrUnsupportedOS = errors.New("service: this OS is not supported")

const defaultWait = 15 * time.Second

// Ensure dials the visor. When no visor listens, it installs and starts the
// service, then waits for the socket.
func Ensure(ctx context.Context, s Service) (*agentvisor.Client, error) {
	c, err := agentvisor.Dial(ctx, s.Socket)
	if err == nil || errors.Is(err, agentvisor.ErrProtocol) {
		return c, err
	}
	if err := s.Install(ctx); err != nil {
		return nil, err
	}
	wait := s.Wait
	if wait == 0 {
		wait = defaultWait
	}
	deadline := time.Now().Add(wait)
	for {
		c, err = agentvisor.Dial(ctx, s.Socket)
		if err == nil || errors.Is(err, agentvisor.ErrProtocol) {
			return c, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("start visor service %s: %w", s.Label, err)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// UserUnitDir gives the folder of the user services for goos.
func UserUnitDir(goos string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("unit folder: %w", err)
	}
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user"), nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedOS, goos)
	}
}

// UnitPath gives the path of the unit file.
func (s Service) UnitPath() string {
	if s.goos() == "darwin" {
		return filepath.Join(s.UnitDir, s.Label+".plist")
	}
	return filepath.Join(s.UnitDir, s.Label+".service")
}

// Install writes the unit file, and starts or restarts the service.
func (s Service) Install(ctx context.Context) error {
	if s.Label == "" || len(s.Command) == 0 || s.UnitDir == "" {
		return fmt.Errorf("install visor service: %w: Label, Command, and UnitDir are required", agentvisor.ErrInvalid)
	}
	var unit []byte
	switch s.goos() {
	case "darwin":
		unit = s.plist()
	case "linux":
		unit = s.systemdUnit()
	default:
		return fmt.Errorf("install visor service: %w: %s", ErrUnsupportedOS, s.goos())
	}
	if err := os.MkdirAll(s.UnitDir, 0o700); err != nil {
		return fmt.Errorf("install visor service: %w", err)
	}
	if err := os.WriteFile(s.UnitPath(), unit, 0o600); err != nil {
		return fmt.Errorf("install visor service: %w", err)
	}
	if s.goos() == "darwin" {
		// bootstrap fails when the job is loaded, so restart it then.
		if err := s.run(ctx, "launchctl", "bootstrap", s.domain(), s.UnitPath()); err != nil {
			return s.run(ctx, "launchctl", "kickstart", "-k", s.domain()+"/"+s.Label)
		}
		return nil
	}
	if err := s.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := s.run(ctx, "systemctl", "--user", "enable", s.Label+".service"); err != nil {
		return err
	}
	return s.run(ctx, "systemctl", "--user", "restart", s.Label+".service")
}

// Remove stops the service and deletes the unit file.
func (s Service) Remove(ctx context.Context) error {
	if s.goos() == "darwin" {
		_ = s.run(ctx, "launchctl", "bootout", s.domain()+"/"+s.Label) // the job can be stopped already
	} else {
		_ = s.run(ctx, "systemctl", "--user", "disable", "--now", s.Label+".service") // the unit can be stopped already
	}
	if err := os.Remove(s.UnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove visor service: %w", err)
	}
	if s.goos() == "linux" {
		return s.run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

func (s Service) goos() string {
	if s.GOOS == "" {
		return runtime.GOOS
	}
	return s.GOOS
}

func (s Service) domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (s Service) run(ctx context.Context, name string, args ...string) error {
	if s.Run != nil {
		return s.Run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func (s Service) plist() []byte {
	var b bytes.Buffer
	esc := func(v string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(v)) // a bytes.Buffer does not fail
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n  <key>ProgramArguments</key>\n  <array>\n", esc(s.Label))
	for _, a := range s.Command {
		fmt.Fprintf(&b, "    <string>%s</string>\n", esc(a))
	}
	b.WriteString("  </array>\n  <key>RunAtLoad</key>\n  <true/>\n  <key>KeepAlive</key>\n  <true/>\n")
	if len(s.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			fmt.Fprintf(&b, "    <key>%s</key>\n    <string>%s</string>\n", esc(k), esc(s.Env[k]))
		}
		b.WriteString("  </dict>\n")
	}
	if s.LogPath != "" {
		fmt.Fprintf(&b, "  <key>StandardOutPath</key>\n  <string>%s</string>\n  <key>StandardErrorPath</key>\n  <string>%s</string>\n", esc(s.LogPath), esc(s.LogPath))
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func (s Service) systemdUnit() []byte {
	quote := func(v string) string {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
		return `"` + r.Replace(v) + `"`
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n[Service]\nExecStart=", s.Label)
	for i, a := range s.Command {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quote(a))
	}
	b.WriteString("\nRestart=always\nRestartSec=1\n")
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		fmt.Fprintf(&b, "Environment=%s\n", quote(k+"="+s.Env[k]))
	}
	if s.LogPath != "" {
		fmt.Fprintf(&b, "StandardOutput=append:%s\nStandardError=append:%s\n", s.LogPath, s.LogPath)
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.Bytes()
}
