package web

import (
	"strings"
	"testing"
	"time"
)

// The dashboard sets no security headers at all unless this holds. The headers
// are the second of two independent controls: the escaper in dashboard.js was
// bypassable once, and a policy with no 'unsafe-inline' in script-src would have
// refused the handler it produced.
func TestEveryResponseCarriesSecurityHeaders(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())

	for _, path := range []string{"/", "/assets/dashboard.js", "/assets/dashboard.css", "/healthz", "/nope"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, srv, path)
			h := rec.Header()
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := h.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want DENY", got)
			}
			if got := h.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want no-referrer", got)
			}
			if h.Get("Content-Security-Policy") == "" {
				t.Error("no Content-Security-Policy")
			}
		})
	}
}

// 'unsafe-inline' in script-src would permit exactly what the policy is for: an
// event handler an injected attribute managed to add. style-src is the one
// relaxation, and it is deliberate — the page writes style attributes — so this
// asserts the relaxation is no wider than that.
func TestCSPDoesNotAllowInlineScript(t *testing.T) {
	srv, _ := newTestServer(t)
	csp := get(t, srv, "/").Header().Get("Content-Security-Policy")

	scriptSrc := directive(t, csp, "script-src")
	if strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("script-src = %q: 'unsafe-inline' here would allow the "+
			"injected handler the policy exists to block", scriptSrc)
	}
	if !strings.Contains(scriptSrc, "'self'") {
		t.Errorf("script-src = %q, which would block /assets/dashboard.js", scriptSrc)
	}
	if !strings.Contains(scriptSrc, "'nonce-") {
		t.Errorf("script-src = %q, which would block the two inline script "+
			"blocks that carry the payload", scriptSrc)
	}
	if got := directive(t, csp, "default-src"); !strings.Contains(got, "'none'") {
		t.Errorf("default-src = %q, want 'none'", got)
	}
	for _, want := range []string{"base-uri", "object-src", "frame-ancestors", "form-action"} {
		if directive(t, csp, want) == "" {
			t.Errorf("no %s directive", want)
		}
	}
	// style-src is the documented relaxation; assert it is present rather than
	// leaving it to be discovered as a broken page.
	if got := directive(t, csp, "style-src"); !strings.Contains(got, "'unsafe-inline'") {
		t.Errorf("style-src = %q: the page writes style attributes, so this "+
			"relaxation is deliberate and should be stated, not accidental", got)
	}
}

// The nonce has to be in the CSP header and on the script tags, and they have to
// be the same value: a nonce in one and not the other blocks the page, and two
// different nonces do the same.
func TestCSPNonceMatchesTheInlineScripts(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := get(t, srv, "/")
	csp := rec.Header().Get("Content-Security-Policy")

	const marker = "'nonce-"
	i := strings.Index(csp, marker)
	if i < 0 {
		t.Fatalf("no nonce in %q", csp)
	}
	rest := csp[i+len(marker):]
	nonce := rest[:strings.Index(rest, "'")]

	body := rec.Body.String()
	if !strings.Contains(body, `<script nonce="`+nonce+`">`) {
		t.Errorf("no script carries the CSP nonce %q", nonce)
	}
	// Every inline script needs it; there are three.
	if got := strings.Count(body, `<script nonce="`+nonce+`">`); got != 3 {
		t.Errorf("%d inline scripts carry the nonce, want 3", got)
	}
	if strings.Contains(body, "<script>") {
		t.Error("an inline script has no nonce and will be blocked")
	}
}

// The nonce is per response: a static one would be a value an attacker can read
// off one page and reuse on every other.
func TestCSPNonceChangesPerResponse(t *testing.T) {
	srv, _ := newTestServer(t)
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		csp := get(t, srv, "/").Header().Get("Content-Security-Policy")
		i := strings.Index(csp, "'nonce-")
		if i < 0 {
			t.Fatal("no nonce in " + csp)
		}
		rest := csp[i+len("'nonce-"):]
		n := rest[:strings.Index(rest, "'")]
		if len(n) < 16 {
			t.Errorf("nonce %q is only %d characters", n, len(n))
		}
		if seen[n] {
			t.Fatalf("nonce %q was issued twice", n)
		}
		seen[n] = true
	}
}

// directive returns one directive's value from a policy, or "".
func directive(t *testing.T, csp, name string) string {
	t.Helper()
	for _, part := range strings.Split(csp, ";") {
		fields := strings.Fields(part)
		if len(fields) > 0 && fields[0] == name {
			return strings.Join(fields, " ")
		}
	}
	return ""
}
