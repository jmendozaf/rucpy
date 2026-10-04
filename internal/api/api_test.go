package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmendozaf/rucpy/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db := filepath.Join(t.TempDir(), "ruc.db")
	ctx := context.Background()
	b, err := store.NewBuilder(ctx, db, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	b.Insert(ctx, 3, "2038893", 4, "MENDOZA FRANCO, ANIBAL JAVIER", "MEFA8203705", "ACTIVO")
	b.Insert(ctx, 7, "80012347", 6, "ACME S.A.", "", "SUSPENSION TEMPORAL")
	b.RecordSegment(ctx, 0, "v1", "https://example.test/ruc0.zip", 1)
	if _, err := b.Commit(ctx, ""); err != nil {
		t.Fatal(err)
	}

	srv := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return ts
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestLookupByRUCWithOrWithoutCheckDigit(t *testing.T) {
	ts := newTestServer(t)

	for _, input := range []string{"2038893-4", "2.038.893-4", "2038893", "MEFA8203705"} {
		code, body := getJSON(t, ts.URL+"/v1/ruc/"+input)
		if code != http.StatusOK || body["name"] != "MENDOZA FRANCO, ANIBAL JAVIER" || body["formatted"] != "2038893-4" {
			t.Errorf("%s: %d %v", input, code, body)
		}
	}
}

func TestWrongCheckDigitReportsTheExpectedOne(t *testing.T) {
	ts := newTestServer(t)

	code, body := getJSON(t, ts.URL+"/v1/ruc/2038893-5")

	if code != http.StatusUnprocessableEntity || body["expected_dv"] != float64(4) || body["formatted"] != "2038893-4" {
		t.Errorf("%d %v", code, body)
	}
}

func TestUnknownAndMalformedRUCs(t *testing.T) {
	ts := newTestServer(t)

	if code, _ := getJSON(t, ts.URL+"/v1/ruc/99999999"); code != http.StatusNotFound {
		t.Errorf("unknown RUC: %d", code)
	}
	if code, _ := getJSON(t, ts.URL+"/v1/ruc/hola"); code != http.StatusBadRequest {
		t.Errorf("malformed RUC: %d", code)
	}
}

func TestSearchIgnoresAccentsAndCase(t *testing.T) {
	ts := newTestServer(t)

	code, body := getJSON(t, ts.URL+"/v1/search?q=mendoza%20franco")

	data, _ := body["data"].([]any)
	if code != http.StatusOK || len(data) != 1 {
		t.Fatalf("%d %v", code, body)
	}
	if first := data[0].(map[string]any); first["ruc"] != "2038893" {
		t.Errorf("first hit = %v", first)
	}
}

func TestSearchRequiresThreeCharacters(t *testing.T) {
	ts := newTestServer(t)

	if code, _ := getJSON(t, ts.URL+"/v1/search?q=ab"); code != http.StatusBadRequest {
		t.Errorf("short query: %d", code)
	}
}

func TestStats(t *testing.T) {
	ts := newTestServer(t)

	code, body := getJSON(t, ts.URL+"/v1/stats")

	if code != http.StatusOK || body["total"] != float64(2) || body["synced_at"] != "2026-10-01T00:00:00Z" {
		t.Errorf("%d %v", code, body)
	}
}

func TestReportsWhenNotSyncedYet(t *testing.T) {
	srv := New(filepath.Join(t.TempDir(), "missing.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if code, _ := getJSON(t, ts.URL+"/v1/ruc/2038893-4"); code != http.StatusServiceUnavailable {
		t.Errorf("got %d", code)
	}
	if code, _ := getJSON(t, ts.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz: %d", code)
	}
}
