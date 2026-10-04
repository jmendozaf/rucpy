// Package syncer downloads the DNIT registry and rebuilds the local database.
// Unchanged files (same DNIT version marker) are copied from the previous database instead of downloaded.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jmendozaf/rucpy/internal/dnit"
	"github.com/jmendozaf/rucpy/internal/ruc"
	"github.com/jmendozaf/rucpy/internal/store"
)

// Source is where registry files come from (the DNIT site in production, a fake in tests).
type Source interface {
	Segments(ctx context.Context) ([]dnit.Segment, error)
	Download(ctx context.Context, s dnit.Segment) (string, error)
}

// Options tune a sync.
type Options struct {
	Workers int                           // parallel downloads (default 4)
	Force   bool                          // download every file even if unchanged
	Now     func() time.Time              // clock, for tests
	Logf    func(format string, a ...any) // progress output (optional)
}

// Result summarises a sync.
type Result struct {
	Segments   int           `json:"segments"`
	Downloaded int           `json:"downloaded"`
	Reused     int           `json:"reused"`
	Rows       int           `json:"rows"`
	BadDV      int           `json:"bad_dv"`
	Changes    int           `json:"changes"`
	UpToDate   bool          `json:"up_to_date"`
	Duration   time.Duration `json:"duration"`
}

// Run syncs dbPath with the registry published by src.
func Run(ctx context.Context, src Source, dbPath string, opts Options) (Result, error) {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	started := opts.Now()

	segments, err := src.Segments(ctx)
	if err != nil {
		return Result{}, err
	}

	previous, versions := previousDatabase(ctx, dbPath)
	var download []dnit.Segment
	reuse := map[int]bool{}
	for _, s := range segments {
		if !opts.Force && previous != "" && s.Version != "" && versions[s.Digit] == s.Version {
			reuse[s.Digit] = true
			continue
		}
		download = append(download, s)
	}
	logf("%d files on DNIT, %d to download, %d unchanged", len(segments), len(download), len(reuse))

	if len(download) == 0 && len(reuse) == len(versions) {
		rows, err := countRows(ctx, previous)
		if err != nil {
			return Result{}, err
		}
		return Result{Segments: len(segments), Reused: len(reuse), Rows: rows, UpToDate: true,
			Duration: opts.Now().Sub(started).Round(time.Millisecond)}, nil
	}

	files, err := downloadAll(ctx, src, download, opts.Workers, logf)
	defer func() {
		for _, f := range files {
			os.Remove(f)
		}
	}()
	if err != nil {
		return Result{}, err
	}

	builder, err := store.NewBuilder(ctx, dbPath, started)
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		if !committed {
			builder.Abort()
		}
	}()

	res := Result{Segments: len(segments), Downloaded: len(download), Reused: len(reuse)}
	for _, s := range segments {
		var rows int
		if reuse[s.Digit] {
			if rows, err = builder.CopySegment(ctx, previous, s.Digit); err != nil {
				return Result{}, fmt.Errorf("reuse ruc%d: %w", s.Digit, err)
			}
		} else {
			err = dnit.ReadZip(files[s.Digit], func(r dnit.Row) error {
				rows++
				if ruc.CheckDigit(r.RUC) != r.DV {
					res.BadDV++
				}
				return builder.Insert(ctx, s.Digit, r.RUC, r.DV, r.Name, r.OldCode, r.Status)
			})
			if err != nil {
				return Result{}, fmt.Errorf("import ruc%d: %w", s.Digit, err)
			}
			if rows == 0 {
				return Result{}, fmt.Errorf("ruc%d.zip has no rows; keeping the current database", s.Digit)
			}
		}
		if err := builder.RecordSegment(ctx, s.Digit, s.Version, s.URL, rows); err != nil {
			return Result{}, err
		}
		res.Rows += rows
		logf("ruc%d: %d rows", s.Digit, rows)
	}

	res.Changes, err = builder.Commit(ctx, previous)
	if err != nil {
		return Result{}, err
	}
	committed = true
	res.Duration = opts.Now().Sub(started).Round(time.Millisecond)
	return res, nil
}

// previousDatabase returns the live database path (or "") and the versions it was built from.
func previousDatabase(ctx context.Context, dbPath string) (string, map[int]string) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", nil
	}
	s, err := store.Open(dbPath)
	if err != nil {
		return "", nil
	}
	defer s.Close()
	versions, err := s.SegmentVersions(ctx)
	if err != nil {
		return "", nil
	}
	return dbPath, versions
}

func countRows(ctx context.Context, dbPath string) (int, error) {
	s, err := store.Open(dbPath)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	stats, err := s.Stats(ctx)
	return stats.Total, err
}

func downloadAll(ctx context.Context, src Source, segments []dnit.Segment, workers int, logf func(string, ...any)) (map[int]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		files    = map[int]string{}
		firstErr error
		wg       sync.WaitGroup
		slots    = make(chan struct{}, workers)
	)
	for _, s := range segments {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()

			path, err := src.Download(ctx, s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil && !errors.Is(err, context.Canceled) {
					firstErr = err
				}
				cancel()
				return
			}
			files[s.Digit] = path
			logf("downloaded ruc%d.zip", s.Digit)
		}()
	}
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil && len(files) < len(segments) {
		firstErr = ctx.Err()
	}
	return files, firstErr
}
