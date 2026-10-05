package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "correct-horse-battery-staple"

// okHandler is a comparable http.Handler, so a test can assert on identity —
// which is the cheapest way to prove the no-token path really is untouched.
type okHandler struct{}

func (okHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "dashboard")
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestWithAuthNoTokenIsPassThrough(t *testing.T) {
	// The default must be indistinguishable from no middleware at all, because
	// that is what every localhost user runs.
	next := okHandler{}
	got := WithAuth("", nil, next)
	if got != next {
		t.Error("WithAuth(\"\") wrapped the handler; want next returned untouched")
	}

	rec := httptest.NewRecorder()
	got.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with no token configured", rec.Code)
	}
	if body := rec.Body.String(); body != "dashboard" {
		t.Errorf("body = %q, want the wrapped handler's body", body)
	}
	if h := rec.Header().Get("WWW-Authenticate"); h != "" {
		t.Errorf("WWW-Authenticate = %q, want it unset when authentication is off", h)
	}
}

func TestWithAuth(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		header map[string]string
		want   int
	}{
		{name: "correct token in Authorization", path: "/", header: map[string]string{"Authorization": "Bearer " + testToken}, want: 200},
		{name: "lowercase scheme", path: "/", header: map[string]string{"Authorization": "bearer " + testToken}, want: 200},
		{name: "X-Auth-Token", path: "/", header: map[string]string{"X-Auth-Token": testToken}, want: 200},
		{name: "X-Auth-Token for a subpath", path: "/session?x=1", header: map[string]string{"X-Auth-Token": testToken}, want: 200},
		{name: "X-Auth-Token on the JSON API", path: "/api/activity", header: map[string]string{"X-Auth-Token": testToken}, want: 200},
		{name: "both headers, one right", path: "/", header: map[string]string{"Authorization": "Bearer wrong", "X-Auth-Token": testToken}, want: 200},

		{name: "wrong token", path: "/", header: map[string]string{"Authorization": "Bearer wrong"}, want: 401},
		{name: "token is a prefix of the real one", path: "/", header: map[string]string{"Authorization": "Bearer " + testToken[:20]}, want: 401},
		{name: "real token is a prefix of the guess", path: "/", header: map[string]string{"Authorization": "Bearer " + testToken + "x"}, want: 401},
		{name: "case-shifted token", path: "/", header: map[string]string{"Authorization": "Bearer CORRECT-HORSE-BATTERY-STAPLE"}, want: 401},
		{name: "wrong token in the other header", path: "/", header: map[string]string{"X-Auth-Token": "wrong"}, want: 401},
		{name: "no header at all", path: "/", header: nil, want: 401},
		{name: "empty bearer", path: "/", header: map[string]string{"Authorization": "Bearer "}, want: 401},
		{name: "empty X-Auth-Token", path: "/", header: map[string]string{"X-Auth-Token": ""}, want: 401},
		{name: "bare token, no scheme", path: "/", header: map[string]string{"Authorization": testToken}, want: 401},
		{name: "wrong scheme", path: "/", header: map[string]string{"Authorization": "Basic " + testToken}, want: 401},
		{name: "trailing whitespace is trimmed", path: "/", header: map[string]string{"Authorization": "Bearer " + testToken + "  "}, want: 200},

		// Healthchecks and uptime monitors must keep working without a secret.
		{name: "healthz needs no token", path: "/healthz", header: nil, want: 200},
		{name: "healthz ignores a wrong token", path: "/healthz", header: map[string]string{"Authorization": "Bearer wrong"}, want: 200},
		{name: "healthz accepts a right token too", path: "/healthz", header: map[string]string{"X-Auth-Token": testToken}, want: 200},
	}

	h := WithAuth(testToken, discardLog(), okHandler{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}

			if tc.want == 200 {
				if body := rec.Body.String(); body != "dashboard" {
					t.Errorf("body = %q, want the wrapped handler's body", body)
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != "" {
					t.Errorf("WWW-Authenticate = %q on a successful response", got)
				}
				return
			}

			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Error("401 without a WWW-Authenticate header; the browser and curl both need it")
			}
			body := rec.Body.String()
			if strings.Contains(body, testToken) || strings.Contains(body, "correct") {
				t.Errorf("401 body leaks the token: %q", body)
			}
			if !strings.Contains(body, "unauthorized") {
				t.Errorf("body = %q, want a plain unauthorized message", body)
			}
		})
	}
}

// TestWithAuthEmptyTokenRejected checks the case that separates "no token
// configured" from "the token is the empty string": if an empty presented token
// matched an empty configured one, forgetting -auth-token would silently open
// the dashboard instead of closing it.
func TestWithAuthEmptyTokenRejected(t *testing.T) {
	for _, configured := range []string{"", "x"} {
		h := WithAuth(configured, discardLog(), okHandler{})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if configured == "" {
			if rec.Code != 200 {
				t.Errorf("no token configured: status = %d, want 200", rec.Code)
			}
			continue
		}
		if rec.Code != 401 {
			t.Errorf("configured token %q with an empty header: status = %d, want 401", configured, rec.Code)
		}
	}
}

// TestWithAuthNilLog exercises the nil-logger default, since run() can hand
// over a nil logger in a test or a future embedding.
func TestWithAuthNilLog(t *testing.T) {
	h := WithAuth(testToken, nil, okHandler{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestWithAuthDoesNotMutateTheRequest guards the header parsing, which must not
// strip or rewrite what the wrapped handler sees.
func TestWithAuthDoesNotMutateTheRequest(t *testing.T) {
	var seen http.Header
	h := WithAuth(testToken, discardLog(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got := seen.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("wrapped handler saw Authorization = %q, want it unchanged", got)
	}
}
