package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SessionTTL is how long a session lasts after the visitor passes Turnstile.
const SessionTTL = time.Hour

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// newSessionKey signs the sessions of this process; a restart only makes the page ask Turnstile again.
func newSessionKey() []byte {
	key := make([]byte, 32)
	rand.Read(key)
	return key
}

// createSession checks a Turnstile token with Cloudflare and answers with a session bound to the visitor's IP.
// The body is a plain form, so the browser sends it without a CORS preflight.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "missing_token", "token is required")
		return
	}
	ip := clientIP(r)
	ok, err := s.verifyTurnstile(r.Context(), token, ip)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "turnstile_failed", "the Turnstile check did not pass")
		return
	}
	expires := time.Now().Add(SessionTTL)
	writeJSON(w, http.StatusOK, map[string]string{
		"session":    s.signSession(expires, ip),
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

func (s *Server) verifyTurnstile(ctx context.Context, token, ip string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	form := url.Values{"secret": {s.TurnstileSecret}, "response": {token}, "remoteip": {ip}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.turnstileURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.Success, nil
}

// requireSession lets /v1 requests through only with a valid ?session=, when Turnstile is configured.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.TurnstileSecret == "" || !strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/v1/session" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.validSession(r.URL.Query().Get("session"), clientIP(r)) {
			writeError(w, http.StatusUnauthorized, "session_required", "pass the Turnstile check on the demo page first")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A session is "expiry.signature", where the signature covers the expiry and the IP it was issued to.
func (s *Server) signSession(expires time.Time, ip string) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	return exp + "." + s.sessionMAC(exp, ip)
}

func (s *Server) validSession(session, ip string) bool {
	exp, mac, ok := strings.Cut(session, ".")
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > unix {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(s.sessionMAC(exp, ip)))
}

func (s *Server) sessionMAC(exp, ip string) string {
	h := hmac.New(sha256.New, s.sessionKey)
	h.Write([]byte(exp + "|" + ip))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// clientIP is the last X-Forwarded-For entry, the one the closest proxy (nginx) added, or else the peer address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
