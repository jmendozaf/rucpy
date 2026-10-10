package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmendozaf/rucpy/internal/store"
)

func newTestServer(t *testing.T, allowedOrigins ...string) *httptest.Server {
	t.Helper()
	_, ts := newTestAPI(t, allowedOrigins...)
	return ts
}

func newTestAPI(t *testing.T, allowedOrigins ...string) (*Server, *httptest.Server) {
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
	srv.AllowedOrigins = allowedOrigins
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return srv, ts
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

	for _, q := range []string{"ab", "a+b+c"} {
		if code, _ := getJSON(t, ts.URL+"/v1/search?q="+q); code != http.StatusBadRequest {
			t.Errorf("short query %q: %d", q, code)
		}
	}
}

func TestCORSOnlyForAllowedOrigins(t *testing.T) {
	ts := newTestServer(t, "https://jmendozaf.github.io")

	allow := func(origin string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/ruc/2038893-4", nil)
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Access-Control-Allow-Origin")
	}

	if got := allow("https://jmendozaf.github.io"); got != "https://jmendozaf.github.io" {
		t.Errorf("allowed origin got %q", got)
	}
	if got := allow("https://evil.example"); got == "https://evil.example" || got == "*" {
		t.Errorf("other origin got %q", got)
	}
	if got := allow(""); got != "https://jmendozaf.github.io" {
		t.Errorf("a request without Origin should carry the same header, so a CDN can cache it: %q", got)
	}
}

func TestSearchAnswersBusyWhenEverySlotIsTaken(t *testing.T) {
	srv, ts := newTestAPI(t)
	for range MaxConcurrentSearches {
		srv.searchSlots <- struct{}{}
	}

	if code, body := getJSON(t, ts.URL+"/v1/search?q=mendoza"); code != http.StatusServiceUnavailable || body["error"] != "busy" {
		t.Errorf("%d %v", code, body)
	}

	<-srv.searchSlots
	if code, _ := getJSON(t, ts.URL+"/v1/search?q=mendoza"); code != http.StatusOK {
		t.Errorf("with a free slot: %d", code)
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

// newGatedAPI requires Turnstile sessions, with a fake siteverify that accepts only the token "human".
func newGatedAPI(t *testing.T) *httptest.Server {
	t.Helper()
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"success": r.PostFormValue("secret") == "s3cret" && r.PostFormValue("response") == "human"})
	}))
	t.Cleanup(verify.Close)
	srv, ts := newTestAPI(t)
	srv.TurnstileSecret = "s3cret"
	srv.turnstileURL = verify.URL
	return ts
}

func newSession(t *testing.T, ts *httptest.Server, token string) (int, string) {
	t.Helper()
	resp, err := http.PostForm(ts.URL+"/v1/session", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body["session"]
}

func TestTurnstileSessionGatesTheAPI(t *testing.T) {
	ts := newGatedAPI(t)

	if code, _ := getJSON(t, ts.URL+"/v1/ruc/2038893-4"); code != http.StatusUnauthorized {
		t.Errorf("without session: %d", code)
	}
	if code, _ := getJSON(t, ts.URL+"/v1/ruc/2038893-4?session=123.forged"); code != http.StatusUnauthorized {
		t.Errorf("forged session: %d", code)
	}
	if code, _ := newSession(t, ts, "bot"); code != http.StatusForbidden {
		t.Errorf("failed Turnstile: %d", code)
	}

	code, session := newSession(t, ts, "human")
	if code != http.StatusOK || session == "" {
		t.Fatalf("session: %d %q", code, session)
	}
	if code, body := getJSON(t, ts.URL+"/v1/ruc/2038893-4?session="+url.QueryEscape(session)); code != http.StatusOK || body["formatted"] != "2038893-4" {
		t.Errorf("with session: %d %v", code, body)
	}
	if code, _ := getJSON(t, ts.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz stays open: %d", code)
	}
}

func TestSessionOnlyWorksFromTheIPItWasIssuedTo(t *testing.T) {
	ts := newGatedAPI(t)
	_, session := newSession(t, ts, "human")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/stats?session="+url.QueryEscape(session), nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("session from another IP: %d", resp.StatusCode)
	}
}
