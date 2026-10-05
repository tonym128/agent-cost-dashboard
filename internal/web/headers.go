package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
)

// securityHeaders documents the response headers set on every response, and why
// each one is here. None of them were set before, which meant the only thing
// standing between a crafted log and a script running in the page was the
// escaper in dashboard.js. That is one control to get right, and it was got
// wrong once: a DOM round-trip through innerHTML escapes & < > and neither
// quote, so a project path could close its title= attribute and append an event
// handler. A CSP with no 'unsafe-inline' in script-src refuses to run that
// handler however it got there, which is the point of two independent controls.
//
// What had to be relaxed, and why:
//
//   - `style-src 'unsafe-inline'`. The templates and the renderers both write
//     `style="..."` attributes — bar widths, colours, the activity chart's
//     columns. Moving all of them into the stylesheet is a large refactor for no
//     security gain: a style attribute cannot execute script in any browser that
//     matters, CSS expression() having been dead for well over a decade.
//   - `script-src` keeps `'self'` plus a per-response nonce. The page needs two
//     inline scripts: `window.dashboardData` and `window.filterState` are built
//     server-side and written into the document, and there is no earlier fetch
//     that could produce them. A nonce is unpredictable and per-response, so it
//     allows exactly those blocks and nothing else.
//
// The inline onclick attributes are gone precisely so that the nonce is enough:
// an inline handler would force 'unsafe-inline' into script-src, which would
// equally permit the payload this policy exists to stop.
//
// `default-src 'none'` rather than `'self'` because nothing is loaded from
// anywhere else — no fonts, no CDN, no analytics. The four directives that need
// naming are the four the page actually uses.
const cspFormat = "default-src 'none'; " +
	"script-src 'self' 'nonce-%s'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'"

// nonceContextKey is where withSecurityHeaders leaves the nonce for the
// template. An unexported key type, so nothing outside this package can collide
// with it or mint a nonce of its own.
type nonceContextKey struct{}

// withSecurityHeaders sets the response headers and passes the nonce down.
//
// The nonce has to appear in two places — the CSP header and the <script nonce>
// attribute — so it is generated once here and read from the context by the
// handler that renders the template. Generating it in the template instead
// would leave the header and the attribute as two things that can drift.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := newNonce()
		h := w.Header()
		h.Set("Content-Security-Policy", cspFor(nonce))
		// Nothing off-origin is loaded, so there is no referrer worth sending
		// and no reason to let another page frame or sniff this one.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(),
			nonceContextKey{}, nonce)))
	})
}

// newNonce returns a fresh random nonce.
//
// An entropy failure yields an empty nonce, which leaves the policy refusing the
// inline scripts rather than permitting them. A security control whose absence
// is silent should fail closed.
func newNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// cspFor renders the policy for one nonce.
func cspFor(nonce string) string {
	return strings.Replace(cspFormat, "%s", nonce, 1)
}

// nonceFrom returns the nonce withSecurityHeaders put on this request, or ""
// when the handler was not wrapped (which is the case in a bare httptest).
func nonceFrom(ctx context.Context) string {
	n, _ := ctx.Value(nonceContextKey{}).(string)
	return n
}
