package syncer

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmendozaf/rucpy/internal/dnit"
	"github.com/jmendozaf/rucpy/internal/store"
)

// fakeSource serves in-memory registry files, one per digit, and counts downloads.
type fakeSource struct {
	dir       string
	files     map[int]string // digit -> txt content
	versions  map[int]string
	downloads map[int]int
	fail      error
}

func newFakeSource(t *testing.T) *fakeSource {
	return &fakeSource{
		dir: t.TempDir(),
		files: map[int]string{
			0: "1023860A|PERSONA DE PRUEBA, CON LETRA|1||SUSPENSION TEMPORAL|\n",
			3: "2038893|MENDOZA FRANCO, ANIBAL JAVIER|4|MEFA8203705|ACTIVO|\n",
			7: "1000067|PEREZ, ANA|4||ACTIVO|\n80012347|ACME S.A.|6||ACTIVO|\n",
		},
		versions:  map[int]string{0: "v1", 3: "v1", 7: "v1"},
		downloads: map[int]int{},
	}
}

func (f *fakeSource) Segments(context.Context) ([]dnit.Segment, error) {
	var segments []dnit.Segment
	for _, d := range []int{0, 3, 7} {
		segments = append(segments, dnit.Segment{Digit: d, URL: fmt.Sprintf("https://example.test/ruc%d.zip?t=%s", d, f.versions[d]), Version: f.versions[d]})
	}
	return segments, nil
}

func (f *fakeSource) Download(_ context.Context, s dnit.Segment) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	f.downloads[s.Digit]++
	path := filepath.Join(f.dir, fmt.Sprintf("ruc%d-%d.zip", s.Digit, f.downloads[s.Digit]))
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(out)
	w, _ := zw.Create(fmt.Sprintf("ruc%d.txt", s.Digit))
	w.Write([]byte(f.files[s.Digit]))
	zw.Close()
	return path, out.Close()
}

func clock(day int) func() time.Time {
	return func() time.Time { return time.Date(2026, 10, day, 6, 0, 0, 0, time.UTC) }
}

func TestFirstSyncImportsEveryFile(t *testing.T) {
	src := newFakeSource(t)
	db := filepath.Join(t.TempDir(), "ruc.db")

	res, err := Run(context.Background(), src, db, Options{Now: clock(1)})
	if err != nil {
		t.Fatal(err)
	}

	if res.Rows != 4 || res.Downloaded != 3 || res.Reused != 0 || res.BadDV != 0 {
		t.Errorf("result = %+v", res)
	}
	s, _ := store.Open(db)
	defer s.Close()
	got, err := s.Get(context.Background(), "1023860a")
	if err != nil || got.Name != "PERSONA DE PRUEBA, CON LETRA" || got.Formatted != "1023860A-1" {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if hits, _ := s.Search(context.Background(), "aníbal mendoza", 10); len(hits) != 1 {
		t.Errorf("accent-insensitive search returned %d hits", len(hits))
	}
}

func TestSecondSyncReusesUnchangedFilesAndRecordsStatusChanges(t *testing.T) {
	src := newFakeSource(t)
	db := filepath.Join(t.TempDir(), "ruc.db")
	if _, err := Run(context.Background(), src, db, Options{Now: clock(1)}); err != nil {
		t.Fatal(err)
	}

	// DNIT republishes ruc7: ACME gets suspended, PEREZ disappears, a new company shows up.
	src.versions[7] = "v2"
	src.files[7] = "80012347|ACME S.A.|6||SUSPENSION TEMPORAL|\n80099997|NUEVA S.R.L.|5||ACTIVO|\n"

	res, err := Run(context.Background(), src, db, Options{Now: clock(2)})
	if err != nil {
		t.Fatal(err)
	}

	if src.downloads[0] != 1 || src.downloads[3] != 1 || src.downloads[7] != 2 || res.Reused != 2 {
		t.Errorf("downloads = %v, result = %+v", src.downloads, res)
	}
	if res.Changes != 3 {
		t.Errorf("changes = %d, want 3 (suspended, removed, new)", res.Changes)
	}

	s, _ := store.Open(db)
	defer s.Close()
	suspended, _ := s.Changes(context.Background(), clock(2)(), "SUSPENSION TEMPORAL", 10)
	if len(suspended) != 1 || suspended[0].RUC != "80012347" || *suspended[0].From != "ACTIVO" {
		t.Errorf("suspended changes = %+v", suspended)
	}
	if _, err := s.Get(context.Background(), "2038893"); err != nil {
		t.Errorf("reused segment lost rows: %v", err)
	}
	if _, err := s.Get(context.Background(), "1000067"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removed RUC still present: %v", err)
	}
}

func TestSyncWithNothingNewLeavesTheDatabaseUntouched(t *testing.T) {
	src := newFakeSource(t)
	db := filepath.Join(t.TempDir(), "ruc.db")
	if _, err := Run(context.Background(), src, db, Options{Now: clock(1)}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(db)

	res, err := Run(context.Background(), src, db, Options{Now: clock(2)})
	if err != nil {
		t.Fatal(err)
	}

	after, _ := os.Stat(db)
	if !res.UpToDate || res.Rows != 4 || res.Downloaded != 0 {
		t.Errorf("result = %+v", res)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("database was rewritten although nothing changed")
	}
}

func TestFailedDownloadKeepsTheCurrentDatabase(t *testing.T) {
	src := newFakeSource(t)
	db := filepath.Join(t.TempDir(), "ruc.db")
	if _, err := Run(context.Background(), src, db, Options{Now: clock(1)}); err != nil {
		t.Fatal(err)
	}

	src.fail = errors.New("dnit is down")
	if _, err := Run(context.Background(), src, db, Options{Now: clock(2), Force: true}); err == nil {
		t.Fatal("expected an error")
	}

	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Get(context.Background(), "80012347"); err != nil {
		t.Errorf("database damaged after failed sync: %v", err)
	}
	if _, err := os.Stat(db + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary database left behind")
	}
}

func TestEmptyFileAbortsInsteadOfWipingData(t *testing.T) {
	src := newFakeSource(t)
	db := filepath.Join(t.TempDir(), "ruc.db")
	if _, err := Run(context.Background(), src, db, Options{Now: clock(1)}); err != nil {
		t.Fatal(err)
	}

	src.versions[0] = "v2"
	src.files[0] = ""
	if _, err := Run(context.Background(), src, db, Options{Now: clock(2)}); err == nil {
		t.Fatal("expected an error for an empty file")
	}

	s, _ := store.Open(db)
	defer s.Close()
	if _, err := s.Get(context.Background(), "2038893"); err != nil {
		t.Errorf("rows lost: %v", err)
	}
}
