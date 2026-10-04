package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// Builder writes a brand-new registry database into a temporary file.
// Readers never see it until Commit atomically renames it over the live path.
type Builder struct {
	db      *sql.DB
	tmpPath string
	path    string
	now     string
	tx      *sql.Tx
	insert  *sql.Stmt
}

// NewBuilder creates <path>.tmp with an empty schema.
func NewBuilder(ctx context.Context, path string, now time.Time) (*Builder, error) {
	tmp := path + ".tmp"
	for _, p := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
		_ = os.Remove(p)
	}
	db, err := sql.Open("sqlite", "file:"+tmp+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	return &Builder{db: db, tmpPath: tmp, path: path, now: now.UTC().Format(time.RFC3339)}, nil
}

// Insert adds one taxpayer. Calls are batched in a single transaction until Flush.
func (b *Builder) Insert(ctx context.Context, segment int, ruc string, dv int, name, oldCode, status string) error {
	if b.tx == nil {
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO rucs (ruc, dv, name, old_code, status, segment) VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(ruc) DO UPDATE SET dv = excluded.dv, name = excluded.name,
			     old_code = excluded.old_code, status = excluded.status, segment = excluded.segment`)
		if err != nil {
			tx.Rollback()
			return err
		}
		b.tx, b.insert = tx, stmt
	}
	_, err := b.insert.ExecContext(ctx, ruc, dv, name, oldCode, status, segment)
	return err
}

// Flush commits pending inserts.
func (b *Builder) Flush() error {
	if b.tx == nil {
		return nil
	}
	b.insert.Close()
	err := b.tx.Commit()
	b.tx, b.insert = nil, nil
	return err
}

// CopySegment reuses an unchanged segment's rows from the previous database instead of downloading it again.
func (b *Builder) CopySegment(ctx context.Context, previousPath string, digit int) (int, error) {
	if err := b.Flush(); err != nil {
		return 0, err
	}
	if err := b.attach(ctx, previousPath); err != nil {
		return 0, err
	}
	defer b.detach(ctx)

	res, err := b.db.ExecContext(ctx,
		`INSERT INTO rucs (ruc, dv, name, old_code, status, segment)
		 SELECT ruc, dv, name, old_code, status, segment FROM prev.rucs WHERE segment = ?`, digit)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RecordSegment stores which version of a registry file was imported.
func (b *Builder) RecordSegment(ctx context.Context, digit int, version, url string, rows int) error {
	if err := b.Flush(); err != nil {
		return err
	}
	_, err := b.db.ExecContext(ctx,
		`INSERT INTO segments (digit, version, url, rows, synced_at) VALUES (?, ?, ?, ?, ?)`,
		digit, version, url, rows, b.now)
	return err
}

// Commit builds the search index, records status changes against the previous database (if any),
// and atomically replaces the live file. It returns the number of changes detected.
func (b *Builder) Commit(ctx context.Context, previousPath string) (int, error) {
	defer b.db.Close()
	if err := b.Flush(); err != nil {
		return 0, err
	}

	if _, err := b.db.ExecContext(ctx, `INSERT INTO rucs_fts(rucs_fts) VALUES('rebuild')`); err != nil {
		return 0, fmt.Errorf("store: build search index: %w", err)
	}

	changes := 0
	if previousPath != "" {
		n, err := b.recordChanges(ctx, previousPath)
		if err != nil {
			return 0, err
		}
		changes = n
	}

	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('synced_at', ?)`, b.now); err != nil {
		return 0, err
	}
	if _, err := b.db.ExecContext(ctx, `PRAGMA optimize; VACUUM`); err != nil {
		return 0, err
	}
	if err := b.db.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(b.tmpPath, b.path); err != nil {
		return 0, fmt.Errorf("store: replace %s: %w", b.path, err)
	}
	return changes, nil
}

// Abort discards the temporary database.
func (b *Builder) Abort() {
	if b.tx != nil {
		b.tx.Rollback()
	}
	b.db.Close()
	_ = os.Remove(b.tmpPath)
}

func (b *Builder) recordChanges(ctx context.Context, previousPath string) (int, error) {
	if err := b.attach(ctx, previousPath); err != nil {
		return 0, err
	}
	defer b.detach(ctx)

	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO changes (detected_at, ruc, name, from_status, to_status)
		 SELECT detected_at, ruc, name, from_status, to_status FROM prev.changes ORDER BY id`); err != nil {
		return 0, err
	}

	res, err := b.db.ExecContext(ctx,
		`INSERT INTO changes (detected_at, ruc, name, from_status, to_status)
		 SELECT ?1, n.ruc, n.name, o.status, n.status
		   FROM rucs n LEFT JOIN prev.rucs o ON o.ruc = n.ruc
		  WHERE o.ruc IS NULL OR o.status <> n.status
		 UNION ALL
		 SELECT ?1, o.ruc, o.name, o.status, NULL
		   FROM prev.rucs o LEFT JOIN rucs n ON n.ruc = o.ruc
		  WHERE n.ruc IS NULL`, b.now)
	if err != nil {
		return 0, fmt.Errorf("store: record changes: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (b *Builder) attach(ctx context.Context, path string) error {
	_, err := b.db.ExecContext(ctx, `ATTACH DATABASE ? AS prev`, path)
	return err
}

func (b *Builder) detach(ctx context.Context) {
	_, _ = b.db.ExecContext(ctx, `DETACH DATABASE prev`)
}
