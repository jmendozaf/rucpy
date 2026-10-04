// Package store keeps the registry in a single SQLite file: exact lookups by RUC,
// full-text search by name (accent-insensitive) and a history of status changes between syncs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE rucs (
    id       INTEGER PRIMARY KEY,
    ruc      TEXT    NOT NULL UNIQUE,
    dv       INTEGER NOT NULL,
    name     TEXT    NOT NULL,
    old_code TEXT    NOT NULL,
    status   TEXT    NOT NULL,
    segment  INTEGER NOT NULL
);
CREATE INDEX rucs_old_code ON rucs(old_code) WHERE old_code <> '';
CREATE VIRTUAL TABLE rucs_fts USING fts5(
    name, content='rucs', content_rowid='id', tokenize='unicode61 remove_diacritics 2'
);
CREATE TABLE changes (
    id          INTEGER PRIMARY KEY,
    detected_at TEXT NOT NULL,
    ruc         TEXT NOT NULL,
    name        TEXT NOT NULL,
    from_status TEXT,
    to_status   TEXT
);
CREATE INDEX changes_detected_at ON changes(detected_at);
CREATE INDEX changes_ruc ON changes(ruc);
CREATE TABLE segments (
    digit     INTEGER PRIMARY KEY,
    version   TEXT    NOT NULL,
    url       TEXT    NOT NULL,
    rows      INTEGER NOT NULL,
    synced_at TEXT    NOT NULL
);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// ErrNotFound is returned when a RUC is not in the registry.
var ErrNotFound = errors.New("store: not found")

// Taxpayer is one registry entry.
type Taxpayer struct {
	RUC       string `json:"ruc"`
	DV        int    `json:"dv"`
	Formatted string `json:"formatted"`
	Name      string `json:"name"`
	OldCode   string `json:"old_code,omitempty"`
	Status    string `json:"status"`
}

// Change is a status transition detected between two syncs. A nil From means the RUC is new.
type Change struct {
	DetectedAt string  `json:"detected_at"`
	RUC        string  `json:"ruc"`
	Name       string  `json:"name"`
	From       *string `json:"from"`
	To         *string `json:"to"`
}

// SegmentInfo describes an imported registry file.
type SegmentInfo struct {
	Digit    int    `json:"digit"`
	Version  string `json:"version"`
	URL      string `json:"url"`
	Rows     int    `json:"rows"`
	SyncedAt string `json:"synced_at"`
}

// Stats summarises the database.
type Stats struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
	SyncedAt string         `json:"synced_at"`
	Segments []SegmentInfo  `json:"segments"`
}

// Store is a read-only handle on a registry database.
type Store struct {
	db *sql.DB
}

// Open opens an existing registry database for reading.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

const taxpayerColumns = `ruc, dv, name, old_code, status`

func scanTaxpayer(row interface{ Scan(...any) error }) (Taxpayer, error) {
	var t Taxpayer
	if err := row.Scan(&t.RUC, &t.DV, &t.Name, &t.OldCode, &t.Status); err != nil {
		return Taxpayer{}, err
	}
	t.Formatted = fmt.Sprintf("%s-%d", t.RUC, t.DV)
	return t, nil
}

// Get looks a RUC up by its base (without check digit). It also matches the old SET code.
func (s *Store) Get(ctx context.Context, base string) (Taxpayer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+taxpayerColumns+` FROM rucs WHERE ruc = ?1
		 UNION ALL
		 SELECT `+taxpayerColumns+` FROM rucs WHERE old_code = ?1 AND ?1 <> ''
		 LIMIT 1`, strings.ToUpper(base))
	t, err := scanTaxpayer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Taxpayer{}, ErrNotFound
	}
	return t, err
}

// Search finds taxpayers whose name contains every word of query (prefix match, accent-insensitive).
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Taxpayer, error) {
	match := ftsQuery(query)
	if match == "" {
		return []Taxpayer{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.ruc, r.dv, r.name, r.old_code, r.status
		 FROM rucs_fts f JOIN rucs r ON r.id = f.rowid
		 WHERE rucs_fts MATCH ?
		 ORDER BY (r.status = 'ACTIVO') DESC, f.rank
		 LIMIT ?`, match, clamp(limit, 1, 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []Taxpayer{}
	for rows.Next() {
		t, err := scanTaxpayer(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, t)
	}
	return results, rows.Err()
}

// ftsQuery turns free text into a safe FTS5 query: each word quoted and prefix-matched.
func ftsQuery(query string) string {
	words := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	terms := make([]string, 0, len(words))
	for _, w := range words {
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " ")
}

// Changes lists status transitions detected since the given time, newest first.
func (s *Store) Changes(ctx context.Context, since time.Time, status string, limit int) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT detected_at, ruc, name, from_status, to_status FROM changes
		 WHERE detected_at >= ? AND (? = '' OR to_status = ?)
		 ORDER BY detected_at DESC, id DESC LIMIT ?`,
		since.UTC().Format(time.RFC3339), status, status, clamp(limit, 1, 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	changes := []Change{}
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.DetectedAt, &c.RUC, &c.Name, &c.From, &c.To); err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// Stats returns totals, counts per status and segment versions.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{ByStatus: map[string]int{}, Segments: []SegmentInfo{}}

	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM rucs GROUP BY status`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.ByStatus[status] = n
		st.Total += n
	}
	rows.Close()

	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'synced_at'`).Scan(&st.SyncedAt)

	segs, err := s.db.QueryContext(ctx, `SELECT digit, version, url, rows, synced_at FROM segments ORDER BY digit`)
	if err != nil {
		return st, err
	}
	defer segs.Close()
	for segs.Next() {
		var si SegmentInfo
		if err := segs.Scan(&si.Digit, &si.Version, &si.URL, &si.Rows, &si.SyncedAt); err != nil {
			return st, err
		}
		st.Segments = append(st.Segments, si)
	}
	return st, segs.Err()
}

// SegmentVersions returns digit → version for the files currently imported.
func (s *Store) SegmentVersions(ctx context.Context) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT digit, version FROM segments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := map[int]string{}
	for rows.Next() {
		var d int
		var v string
		if err := rows.Scan(&d, &v); err != nil {
			return nil, err
		}
		versions[d] = v
	}
	return versions, rows.Err()
}

func clamp(n, lo, hi int) int {
	return min(max(n, lo), hi)
}
