package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// printer writes the output of one command: a table, or indented JSON.
type printer struct {
	w    io.Writer
	json bool
}

// table collects rows and aligns their columns.
type table struct {
	tw *tabwriter.Writer
}

// JSON writes v as indented JSON.
func (p printer) JSON(v any) error {
	enc := json.NewEncoder(p.w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Table starts a table with a header row. An empty header has no row.
func (p printer) Table(header ...string) *table {
	t := &table{tw: tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)}
	if len(header) > 0 {
		t.Row(header...)
	}
	return t
}

// Line writes one line of text.
func (p printer) Line(format string, a ...any) error {
	_, err := fmt.Fprintf(p.w, format+"\n", a...)
	return err
}

// Row adds one row. An empty cell prints as "-".
func (t *table) Row(cells ...string) {
	for i, c := range cells {
		if c == "" {
			cells[i] = "-"
		}
	}
	fmt.Fprintln(t.tw, strings.Join(cells, "\t"))
}

// Flush writes the table.
func (t *table) Flush() error { return t.tw.Flush() }

// short makes text one line of at most n runes.
func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
