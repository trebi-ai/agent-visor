package harness

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"
)

// fakeSystem is a system install of fakeh. It keeps its version in a side
// file, so a copy of the program keeps the version too. Version 6.6.6 is
// broken.
const fakeSystem = `#!/bin/sh
d=$(dirname "$0")
case "$1" in
--version) cat "$d/fakeh-version";;
update)
  v=2.0.0; [ "$2" = --to ] && v="$3"
  [ "$v" = 6.6.6 ] && v=broken
  echo "$v" > "$d/fakeh-version";;
esac
`

func TestSystemUpdate(t *testing.T) {
	t.Parallel()
	r, bin := systemRig(t)
	ctx := t.Context()
	h, err := r.m.Update(ctx, "fakeh", "")
	if err != nil || h.Source != SourceSystem || h.Version != "2.0.0" || h.Path != bin {
		t.Fatalf("update: %+v %v", h, err)
	}
	if n := len(h.History); n != 1 || !h.History[0].OK || h.History[0].From != "1.0.0" || h.History[0].To != "2.0.0" {
		t.Fatalf("history: %+v", h.History)
	}
	if len(h.Installs) != 0 {
		t.Fatalf("auto mode keeps a copy: %v", h.Installs)
	}

	// A broken update goes back to a copy of the old program.
	if _, err := r.m.Update(ctx, "fakeh", "6.6.6"); err == nil {
		t.Fatal("a broken update: no error")
	}
	h, err = r.m.Harness(ctx, "fakeh")
	if err != nil || h.Source != SourceVisor || h.Version != "2.0.0" {
		t.Fatalf("after the roll back: %+v %v", h, err)
	}
	last := h.History[len(h.History)-1]
	if last.OK || last.To != "6.6.6" || last.Error == "" {
		t.Fatalf("failed update: %+v", last)
	}
	l, err := r.m.Launch(ctx, "fakeh")
	if err != nil || l.Binary != filepath.Join(r.home, "bin", "fakeh", "2.0.0", "bin", "fakeh") || l.Env["FAKEH_NO_UPDATE"] != "1" {
		t.Fatalf("launch of the copy: %+v %v", l, err)
	}

	// A good update runs the system install again.
	if h, err = r.m.Update(ctx, "fakeh", "2.1.0"); err != nil || h.Source != SourceSystem || h.Version != "2.1.0" || len(h.Installs) != 0 {
		t.Fatalf("update after a roll back: %+v %v", h, err)
	}
}

func TestKeepMode(t *testing.T) {
	t.Parallel()
	r, _ := systemRig(t)
	ctx := t.Context()
	if _, err := r.m.SetUpdates(ctx, "fakeh", "sometimes", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad mode: %v", err)
	}
	if _, err := r.m.SetUpdates(ctx, "fakeh", UpdatesAuto, "1.0.0"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("auto with a version: %v", err)
	}
	h, err := r.m.SetUpdates(ctx, "fakeh", UpdatesKeep, "")
	if err != nil || h.Updates != UpdatesKeep || h.KeptAt == nil || h.Source != SourceVisor || h.Version != "1.0.0" {
		t.Fatalf("keep: %+v %v", h, err)
	}
	// The check does not update a kept harness.
	r.m.Check(ctx)
	if h, _ = r.m.Harness(ctx, "fakeh"); h.Version != "1.0.0" || len(h.History) != 0 {
		t.Fatalf("check in keep mode: %+v", h)
	}
	// A request updates the system install and keeps the new version.
	if h, err = r.m.Update(ctx, "fakeh", "1.5.0"); err != nil || h.Source != SourceVisor || h.Version != "1.5.0" {
		t.Fatalf("update in keep mode: %+v %v", h, err)
	}
	h, err = r.m.SetUpdates(ctx, "fakeh", UpdatesAuto, "")
	if err != nil || h.Source != SourceSystem || h.KeptAt != nil || len(h.Installs) != 0 {
		t.Fatalf("auto: %+v %v", h, err)
	}
}

func TestCheckAutoAndPending(t *testing.T) {
	t.Parallel()
	r, _ := systemRig(t)
	ctx := t.Context()
	r.inUse.Store(true)
	if _, err := r.m.Update(ctx, "fakeh", "1.2.0"); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	h, _ := r.m.Harness(ctx, "fakeh")
	if !h.Pending {
		t.Fatalf("no pending update: %+v", h)
	}
	r.m.Check(ctx)
	if h, _ = r.m.Harness(ctx, "fakeh"); h.Version != "1.0.0" {
		t.Fatalf("update under a session: %s", h.Version)
	}
	r.inUse.Store(false)
	r.m.Check(ctx)
	if h, _ = r.m.Harness(ctx, "fakeh"); h.Version != "1.2.0" || h.Pending {
		t.Fatalf("pending update: %+v", h)
	}
	// The next check finds 2.0.0 and updates in auto mode.
	r.m.Check(ctx)
	if h, _ = r.m.Harness(ctx, "fakeh"); h.Version != "2.0.0" {
		t.Fatalf("auto update: %+v", h)
	}
	// An update outside the visor goes into the history.
	writeFile(t, filepath.Join(r.home, "sys", "fakeh-version"), "2.2.0\n")
	r.m.Check(ctx)
	h, _ = r.m.Harness(ctx, "fakeh")
	if last := h.History[len(h.History)-1]; !last.Outside || last.From != "2.0.0" || last.To != "2.2.0" {
		t.Fatalf("outside update: %+v", h.History)
	}
	// A version check that fails or gives no number is not an update.
	n := len(h.History)
	for _, out := range []string{"", "WARNING: could not create PATH aliases\n", "2.2.0\n"} {
		writeFile(t, filepath.Join(r.home, "sys", "fakeh-version"), out)
		r.m.Check(ctx)
	}
	if h, _ = r.m.Harness(ctx, "fakeh"); len(h.History) != n {
		t.Fatalf("a failed version check went into the history: %+v", h.History[n:])
	}
}

func TestUpdateBinaryCopy(t *testing.T) {
	t.Parallel()
	r, bin := systemRig(t)
	// The program of a binary recipe sits outside bin/, with its side file.
	prog := "libexec/fakeh"
	r.m.o.Facts.Recipe = func(h agentwire.Harness) (agentwire.InstallRecipe, bool) {
		return agentwire.InstallRecipe{Harness: h, Method: agentwire.InstallBinary, Bin: "fakeh", Update: []string{"update"}, UpdateTo: []string{"update", "--to"},
			Download: &agentwire.Download{Program: prog, Platforms: map[string]string{runtime.GOOS + "/" + runtime.GOARCH: "p"}}}, true
	}
	ctx := t.Context()
	h, err := r.m.SetUpdates(ctx, "fakeh", UpdatesKeep, "")
	want := filepath.Join(r.home, "bin", "fakeh", "1.0.0", filepath.FromSlash(prog))
	if err != nil || h.Source != SourceVisor || h.Version != "1.0.0" || h.Path != want {
		t.Fatalf("keep: %+v %v", h, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(want), "fakeh-version")); err != nil {
		t.Fatalf("side file: %v", err)
	}
	// An update of the system install keeps the new version as a copy.
	if h, err = r.m.Update(ctx, "fakeh", "1.5.0"); err != nil || h.Source != SourceVisor || h.Version != "1.5.0" {
		t.Fatalf("update: %+v %v", h, err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}
	if h, err = r.m.SetUpdates(ctx, "fakeh", UpdatesAuto, ""); err != nil || h.Source != SourceSystem || len(h.Installs) != 0 {
		t.Fatalf("auto: %+v %v", h, err)
	}
}

func systemRig(t *testing.T) (*rig, string) {
	t.Helper()
	r := newRig(t)
	// The fake wire gives the same system install for each harness.
	r.m.o.Enabled = func(name string) bool { return name == "fakeh" }
	bin := filepath.Join(r.home, "sys", "fakeh")
	writeFile(t, bin, fakeSystem)
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.home, "sys", "fakeh-version"), "1.0.0\n")
	r.wire.mu.Lock()
	r.wire.system = bin
	r.wire.mu.Unlock()
	return r, bin
}
