package store_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/agent-visor/internal/store"
)

func TestDocs(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "visor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Put(t.Context(), store.KindSession, "a", []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(t.Context(), store.KindSession, "a", []byte(`{"n":2}`)); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(t.Context(), store.KindSession, "a")
	if err != nil || string(got) != `{"n":2}` {
		t.Fatalf("get: %s %v", got, err)
	}
	if _, err := st.Get(t.Context(), store.KindProject, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get from another kind: %v", err)
	}
	if err := st.Delete(t.Context(), store.KindSession, "a"); err != nil {
		t.Fatal(err)
	}
	if all, err := st.List(t.Context(), store.KindSession); err != nil || len(all) != 0 {
		t.Fatalf("list after delete: %d %v", len(all), err)
	}
}

func TestEvents(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "visor.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 10; seq++ {
		row := store.EventRow{Seq: seq, Session: fmt.Sprintf("s%d", seq%2), Data: []byte("{}")}
		if err := st.AppendEvent(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = store.Open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if last, err := st.LastSeq(t.Context()); err != nil || last != 10 {
		t.Fatalf("last seq after reopen: %d %v", last, err)
	}
	for name, tc := range map[string]struct {
		after, until uint64
		sessions     []string
		limit        int
		want         string
	}{
		"all":      {0, 10, nil, 100, "1 2 3 4 5 6 7 8 9 10"},
		"window":   {3, 6, nil, 100, "4 5 6"},
		"limit":    {0, 10, nil, 2, "1 2"},
		"session":  {0, 10, []string{"s1"}, 100, "1 3 5 7 9"},
		"sessions": {4, 10, []string{"s0", "s1"}, 3, "5 6 7"},
	} {
		rows, err := st.Events(t.Context(), tc.after, tc.until, tc.sessions, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		var seqs []uint64
		for _, r := range rows {
			seqs = append(seqs, r.Seq)
		}
		if got := fmt.Sprint(seqs); got != "["+tc.want+"]" {
			t.Errorf("%s: %s, want [%s]", name, got, tc.want)
		}
	}
	if err := st.DeleteSessionEvents(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	if rows, err := st.Events(t.Context(), 0, 10, []string{"s1"}, 100); err != nil || len(rows) != 0 {
		t.Fatalf("events after delete: %d %v", len(rows), err)
	}
}
