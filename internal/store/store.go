// Package store keeps the visor state in visor.db: documents (projects,
// workdirs, sessions, harness state, meta) and the event log.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // the SQLite driver
)

// Store is one open visor.db.
type Store struct {
	db *sql.DB
}

// EventRow is one stored event.
type EventRow struct {
	Seq     uint64
	Session string
	Data    []byte
	// Source and SourceSeq name the holder run and the holder sequence of
	// an event that came from a holder.
	Source    string
	SourceSeq uint64
}

// Document kinds.
const (
	KindMeta      = "meta"
	KindProject   = "project"
	KindWorkdir   = "workdir"
	KindSession   = "session"
	KindHarness   = "harness"
	schemaVersion = 2
)

// ErrNotFound reports a missing document.
var ErrNotFound = errors.New("store: not found")

// Open opens or creates the database at path with mode 0600.
func Open(ctx context.Context, path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// One writer keeps the event sequence in order and avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close() // the migrate error is the one to report
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Put writes a document.
func (s *Store) Put(ctx context.Context, kind, id string, data []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO docs(kind, id, data) VALUES(?, ?, ?)
		 ON CONFLICT(kind, id) DO UPDATE SET data = excluded.data`, kind, id, data)
	if err != nil {
		return fmt.Errorf("put %s %s: %w", kind, id, err)
	}
	return nil
}

// Get reads a document. It returns ErrNotFound when there is none.
func (s *Store) Get(ctx context.Context, kind, id string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM docs WHERE kind = ? AND id = ?`, kind, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s: %w", kind, id, err)
	}
	return data, nil
}

// List reads all documents of one kind, in id order.
func (s *Store) List(ctx context.Context, kind string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM docs WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("list %s: %w", kind, err)
		}
		out = append(out, data)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	return out, nil
}

// Delete removes a document. A missing document is not an error.
func (s *Store) Delete(ctx context.Context, kind, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM docs WHERE kind = ? AND id = ?`, kind, id); err != nil {
		return fmt.Errorf("delete %s %s: %w", kind, id, err)
	}
	return nil
}

// AppendEvent writes one event with its sequence number.
func (s *Store) AppendEvent(ctx context.Context, e EventRow) error {
	var src, srcSeq any
	if e.Source != "" {
		src, srcSeq = e.Source, int64(e.SourceSeq) //nolint:gosec // a sequence number never reaches 2^63
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO events(seq, session, data, src_run, src_seq) VALUES(?, ?, ?, ?, ?)`,
		int64(e.Seq), e.Session, e.Data, src, srcSeq) //nolint:gosec // a sequence number never reaches 2^63
	if err != nil {
		return fmt.Errorf("append event %d: %w", e.Seq, err)
	}
	return nil
}

// LastSeq returns the highest sequence number, or 0.
func (s *Store) LastSeq(ctx context.Context) (uint64, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("last seq: %w", err)
	}
	return uint64(seq.Int64), nil //nolint:gosec // the column holds only positive values
}

// LastSource gives the holder run and the holder sequence of the last
// event of a session that came from a holder. An empty run means none.
func (s *Store) LastSource(ctx context.Context, session string) (string, uint64, error) {
	var (
		run sql.NullString
		seq sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT src_run, src_seq FROM events WHERE session = ? AND src_run IS NOT NULL ORDER BY seq DESC LIMIT 1`, session).Scan(&run, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("last source of %s: %w", session, err)
	}
	return run.String, uint64(seq.Int64), nil //nolint:gosec // the column holds only positive values
}

// Events reads the events with after < seq <= until, in order, at most limit
// rows. An empty sessions list reads all sessions.
func (s *Store) Events(ctx context.Context, after, until uint64, sessions []string, limit int) ([]EventRow, error) {
	q := `SELECT seq, session, data FROM events WHERE seq > ? AND seq <= ?`
	args := []any{int64(after), int64(until)} //nolint:gosec // a sequence number never reaches 2^63
	if len(sessions) > 0 {
		q += ` AND session IN (` + placeholders(len(sessions)) + `)`
		for _, id := range sessions {
			args = append(args, id)
		}
	}
	q += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var (
			r   EventRow
			seq int64
		)
		if err := rows.Scan(&seq, &r.Session, &r.Data); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		r.Seq = uint64(seq) //nolint:gosec // the column holds only positive values
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return out, nil
}

// DeleteSessionEvents removes the events of one session.
func (s *Store) DeleteSessionEvents(ctx context.Context, session string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE session = ?`, session); err != nil {
		return fmt.Errorf("delete events of %s: %w", session, err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for ; v < schemaVersion; v++ {
		if _, err := s.db.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
	}
	return nil
}

// migrations[i] moves the schema from version i to i+1.
var migrations = [schemaVersion]string{ //nolint:gochecknoglobals // a fixed table
	`
CREATE TABLE IF NOT EXISTS docs (
	kind TEXT NOT NULL,
	id   TEXT NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY (kind, id)
);
CREATE TABLE IF NOT EXISTS events (
	seq     INTEGER PRIMARY KEY,
	session TEXT NOT NULL,
	data    BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS events_session ON events(session, seq);
PRAGMA user_version = 1;`,
	`
ALTER TABLE events ADD COLUMN src_run TEXT;
ALTER TABLE events ADD COLUMN src_seq INTEGER;
PRAGMA user_version = 2;`,
}

func placeholders(n int) string {
	b := make([]byte, 0, n*2)
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
