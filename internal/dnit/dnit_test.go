package dnit

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSegmentsFindsTheTenRegistryFiles(t *testing.T) {
	f, err := os.Open("../../testdata/listado.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	base, _ := url.Parse(DefaultPageURL)

	segments, err := ParseSegments(f, base)
	if err != nil {
		t.Fatal(err)
	}

	if len(segments) != 10 {
		t.Fatalf("got %d segments, want 10", len(segments))
	}
	first := segments[0]
	if first.Digit != 0 || first.Version != "1790859279224" {
		t.Errorf("first segment = %+v", first)
	}
	if !strings.HasPrefix(first.URL, "https://www.dnit.gov.py/documents/20123/3759873/ruc0.zip/") {
		t.Errorf("URL not resolved against the page: %s", first.URL)
	}
	if segments[9].Digit != 9 {
		t.Errorf("last segment digit = %d", segments[9].Digit)
	}
}

func TestParseRowReadsRegistryLines(t *testing.T) {
	row, ok := ParseRow("2038893|MENDOZA FRANCO, ANIBAL JAVIER|4|MEFA8203705|ACTIVO|")
	if !ok {
		t.Fatal("row not parsed")
	}
	want := Row{RUC: "2038893", DV: 4, Name: "MENDOZA FRANCO, ANIBAL JAVIER", OldCode: "MEFA8203705", Status: "ACTIVO"}
	if row != want {
		t.Errorf("got %+v", row)
	}
}

func TestParseRowKeepsPipesInsideNamesAndSquashesSpaces(t *testing.T) {
	row, ok := ParseRow("80012345|EMPRESA A|B  S.A.|0||SUSPENSION TEMPORAL|\r\n")
	if !ok {
		t.Fatal("row not parsed")
	}
	if row.Name != "EMPRESA A|B S.A." || row.DV != 0 || row.OldCode != "" || row.Status != "SUSPENSION TEMPORAL" {
		t.Errorf("got %+v", row)
	}
}

func TestParseRowSkipsBrokenLines(t *testing.T) {
	for _, line := range []string{"", "solo|tres|campos", "|NOMBRE|1||ACTIVO|", "123|NOMBRE|X||ACTIVO|"} {
		if _, ok := ParseRow(line); ok {
			t.Errorf("ParseRow(%q) should be skipped", line)
		}
	}
}

func TestReadZipIgnoresHostileEntryNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ruc0.zip")
	f, _ := os.Create(path)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../../evil/ruc0.txt")
	w.Write([]byte("2038893|MENDOZA FRANCO, ANIBAL JAVIER|4||ACTIVO|\n80012345|EMPRESA DE PRUEBA S.A.|0||CANCELADO|\n"))
	zw.Close()
	f.Close()

	var rows []Row
	if err := ReadZip(path, func(r Row) error { rows = append(rows, r); return nil }); err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 || rows[1].Status != "CANCELADO" {
		t.Errorf("rows = %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "..", "..", "evil")); err == nil {
		t.Error("zip entry was written outside the temp dir")
	}
}
