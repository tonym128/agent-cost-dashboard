package web

import (
	"encoding/json"
	"github.com/tonym128/agent-cost-dashboard/internal/model"
	"net/http"
	"strings"
	"testing"
	"time"
)

// /healthz is unauthenticated by design so a container healthcheck does not
// need the auth token in its command. The price of that is that anyone who can
// reach the port can ask for an answer as often as they like, so the answer has
// to be cheap.
func TestHealthzCachesItsAggregate(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())

	// Long enough that nothing expires during the test.
	srv.HealthCacheTTL = time.Minute
	if got := healthCalls(t, srv); got != 3 {
		t.Fatalf("first probe reports %d calls, want 3", got)
	}

	// More calls arrive; the cached count is served until the TTL expires,
	// which is the whole point.
	if err := st.ReplaceSession(model.SessionWrite{
		Session: model.Session{UID: "later", Agent: "pi", Project: "/p/three"},
		Calls: []model.Call{{
			SessionUID: "later", CallKey: "c1", Agent: "pi", Project: "/p/three",
			Model: "m", Time: time.Now(), TotalTokens: 1, Priced: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("later"); err != nil {
		t.Fatal(err)
	}
	if got := healthCalls(t, srv); got != 3 {
		t.Errorf("probe reports %d calls immediately after new data; the "+
			"aggregate should be cached for the TTL", got)
	}

	// Once it expires the count is current again. A short TTL rather than a
	// fake clock: the cache reads the wall clock, and a test that reached past
	// the store to fake it would not prove the store is not being queried.
	srv.HealthCacheTTL = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	if got := healthCalls(t, srv); got != 4 {
		t.Errorf("probe reports %d calls after the TTL expired, want 4", got)
	}
}

// The exemption from authentication is only defensible because there is nothing
// in the payload to disclose. If a project path or a model name gets added here,
// every unauthenticated reader on the network gets it too.
func TestHealthzLeaksNothingAboutWhatIsBeingWorkedOn(t *testing.T) {
	srv, st := newTestServer(t)
	const secret = "/home/somebody/secret-project"
	sess := model.SessionWrite{
		Session: model.Session{UID: "s", Agent: "pi", Project: secret,
			Title: "quarterly plan for the secret project"},
		Path: "/logs/s.jsonl",
		Calls: []model.Call{{
			SessionUID: "s", CallKey: "c", Agent: "pi", Project: secret,
			Model: "expensive-secret-model", Time: time.Now(),
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			CostUSD: 1, Priced: true,
		}},
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s"); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/healthz").Body.String()
	for _, leak := range []string{
		secret, "secret-project", "quarterly", "expensive-secret-model",
		"s.jsonl", "pi",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("healthz payload discloses %q: %s", leak, body)
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Errorf("healthz does not report ok: %s", body)
	}
	allowed := map[string]bool{
		"ok": true, "calls": true, "sessions": true,
		"last_scan": true, "scan_running": true,
	}
	for k := range out {
		if !allowed[k] {
			t.Errorf("healthz gained a %q field; check it discloses nothing "+
				"before it is served without a token", k)
		}
	}
}

// The healthcheck is wget --spider, so the status has to stay 200 and the body
// has to stay JSON whether or not the aggregate is cached.
func TestHealthzStaysUsableAsAContainerHealthcheck(t *testing.T) {
	srv, st := newTestServer(t)
	srv.HealthCacheTTL = time.Hour
	seed(t, st, time.Now())

	for i := 0; i < 3; i++ {
		rec := get(t, srv, "/healthz")
		if rec.Code != http.StatusOK {
			t.Fatalf("probe %d: status %d, want 200", i, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if out["ok"] != true {
			t.Errorf("probe %d does not report ok: %s", i, rec.Body)
		}
	}
}

// healthCalls reads the call count out of a /healthz response.
func healthCalls(t *testing.T, srv *Server) int {
	t.Helper()
	rec := get(t, srv, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	n, ok := out["calls"].(float64)
	if !ok {
		t.Fatalf("no calls field in %s", rec.Body)
	}
	return int(n)
}
