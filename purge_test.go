package agentvisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPurge(t *testing.T) {
	t.Parallel()
	eachHost(t, func(t *testing.T, open opener) {
		home := t.TempDir()
		v := open(t, home, scriptHang, nil)
		defer v.Close(context.Background())
		s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang"})
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, v, s.ID, StateRunning)
		if err := v.Purge(t.Context(), PurgeSpec{Session: s.ID}); !errors.Is(err, ErrState) {
			t.Fatalf("purge a live session: %v, want ErrState", err)
		}
		if err := v.Stop(t.Context(), s.ID); err != nil {
			t.Fatal(err)
		}
		waitState(t, v, s.ID, StateExited)
		frag := filepath.Join(home, "fragments", s.ID+".ndjson")
		touchFile(t, frag)
		if err := v.Purge(t.Context(), PurgeSpec{Session: s.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := v.Session(t.Context(), s.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session after purge: %v, want ErrNotFound", err)
		}
		if exists(filepath.Join(home, "sessions", s.ID)) {
			t.Fatal("the session folder stays after purge")
		}
		if !exists(frag) {
			t.Fatal("purge removed the fragment file")
		}

		// A vendor session with no visor session.
		id := "56fc5395-9134-4a60-9bfb-6d2048569e1c"
		mine := filepath.Join(home, "harness", "claude", "projects", "-repo", id+".jsonl")
		other := filepath.Join(home, "harness", "claude", "projects", "-repo", "other.jsonl")
		touchFile(t, mine)
		touchFile(t, other)
		if err := v.Purge(t.Context(), PurgeSpec{Harness: "claude", VendorID: id}); err != nil {
			t.Fatal(err)
		}
		if exists(mine) || !exists(other) {
			t.Fatalf("vendor files: mine %v other %v, want only other", exists(mine), exists(other))
		}
	})
}

func TestPurgeSpecValidate(t *testing.T) {
	t.Parallel()
	for _, p := range []PurgeSpec{{}, {Harness: "claude"}, {VendorID: "x"}, {Harness: "../x", VendorID: "y"}} {
		if err := p.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", p, err)
		}
	}
	for _, p := range []PurgeSpec{{Session: "ses_1"}, {Harness: "claude", VendorID: "x"}} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

func TestSweep(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	v := newTestVisor(t, home, scriptHang, nil)
	defer v.Close(context.Background())
	s, err := v.Start(t.Context(), StartSpec{Harness: "fake", WorkingDir: t.TempDir(), Prompt: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateRunning)
	if err := v.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, v, s.ID, StateExited)

	old := filepath.Join(home, "fragments", "ses_old.ndjson")
	fresh := filepath.Join(home, "fragments", "ses_new.ndjson")
	touchFile(t, old)
	touchFile(t, fresh)
	past := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	v.opts.KeepExited = time.Nanosecond
	v.sweep(t.Context())
	if exists(old) || !exists(fresh) {
		t.Fatalf("fragments: old %v fresh %v, want only fresh", exists(old), exists(fresh))
	}
	if _, err := v.Session(t.Context(), s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exited session after sweep: %v, want ErrNotFound", err)
	}
}

func touchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
