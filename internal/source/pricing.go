package source

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Rates are prices per million tokens. A zero field means "not charged", which
// is the same thing as absent for the arithmetic.
type Rates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// manualPricingJSON is the fallback price table, compiled into the binary so a
// misconfigured or missing path can never leave the dashboard unable to price
// anything at all.
//
//go:embed manual_pricing.json
var manualPricingJSON []byte

// Pricer resolves a model label to rates.
//
// Resolution has two steps, and the order matters. A live price is looked up by
// exact normalised key, because substring-matching against a catalogue of a
// thousand models would let a short unrelated id match a long unrelated one —
// "claude-opus-4-6" is a substring of "anthropic-claude-opus-4-6", but also of
// names it should not match. Only when nothing live matches does the smaller
// hand-maintained fallback table get a substring test, where the blast radius
// of a wrong match is limited to the few dozen models it lists.
type Pricer struct {
	openRouter map[string]Rates
	manual     map[string]Rates
	// manualKeys is sorted longest-first so the first substring hit is also the
	// most specific one, and the lookup stays a plain linear scan.
	manualKeys []string

	mu       sync.RWMutex
	cache    map[string]resolved
	liveKeys []string
}

type resolved struct {
	rates Rates
	ok    bool
}

// NewPricer builds a pricer from a live catalogue and the embedded fallback
// table. Either path may be missing; at least one must be readable.
func NewPricer(modelsPath, manualPath string) (*Pricer, error) {
	p := &Pricer{
		openRouter: map[string]Rates{},
		manual:     map[string]Rates{},
		cache:      map[string]resolved{},
	}
	if modelsPath != "" {
		if err := p.loadOpenRouter(modelsPath); err != nil {
			return nil, err
		}
	}
	// The embedded table is the floor; an explicit path replaces it when the
	// caller has one.
	if manualPath == "" {
		if err := p.loadManualBytes(manualPricingJSON, "embedded table"); err != nil {
			return nil, err
		}
	} else if err := p.loadManual(manualPath); err != nil {
		return nil, err
	}
	if len(p.openRouter) == 0 && len(p.manual) == 0 {
		return nil, fmt.Errorf("no pricing available: neither %q nor %q could be read", modelsPath, manualPath)
	}
	return p, nil
}

type openRouterDoc struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
			CacheRead  string `json:"input_cache_read"`
			CacheWrite string `json:"input_cache_write"`
		} `json:"pricing"`
	} `json:"data"`
}

func (p *Pricer) loadOpenRouter(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var doc openRouterDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	const perMillion = 1_000_000
	for _, m := range doc.Data {
		if m.ID == "" {
			continue
		}
		entry := Rates{
			Input:      perMillion * atof(m.Pricing.Prompt),
			Output:     perMillion * atof(m.Pricing.Completion),
			CacheRead:  perMillion * atof(m.Pricing.CacheRead),
			CacheWrite: perMillion * atof(m.Pricing.CacheWrite),
		}
		key := NormalizeModel(m.ID)
		// A zero-priced variant must not clobber a real entry for the same key:
		// catalogues routinely list both "vendor/model" and "vendor/model:free".
		if existing, ok := p.openRouter[key]; ok && entry.Input == 0 && entry.Output == 0 && existing.Input != 0 {
			continue
		}
		p.openRouter[key] = entry
	}
	p.liveKeys = make([]string, 0, len(p.openRouter))
	for k := range p.openRouter {
		p.liveKeys = append(p.liveKeys, k)
	}
	sortLongestFirst(p.liveKeys)
	return nil
}

func (p *Pricer) loadManual(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	return p.loadManualBytes(raw, path)
}

func (p *Pricer) loadManualBytes(raw []byte, origin string) error {
	if err := json.Unmarshal(raw, &p.manual); err != nil {
		return fmt.Errorf("parse %s: %w", origin, err)
	}
	p.manualKeys = make([]string, 0, len(p.manual))
	for k := range p.manual {
		p.manualKeys = append(p.manualKeys, k)
	}
	sortLongestFirst(p.manualKeys)
	return nil
}

// Resolve returns the rates for a model and whether any source knew it.
//
// ok == false is meaningful: the tokens are still counted, but reporting a cost
// of zero would read as "this model is free" rather than "we do not know what
// this costs".
func (p *Pricer) Resolve(model string) (Rates, bool) {
	p.mu.RLock()
	r, hit := p.cache[model]
	p.mu.RUnlock()
	if hit {
		return r.rates, r.ok
	}

	out := p.resolveUncached(model)

	p.mu.Lock()
	if len(p.cache) > 16384 {
		// A scanner could in principle see unbounded ids from a corrupt log;
		// cap rather than trust that the key space is small.
		p.cache = make(map[string]resolved, 16384)
	}
	p.cache[model] = out
	p.mu.Unlock()
	return out.rates, out.ok
}

func (p *Pricer) resolveUncached(model string) resolved {
	if model == "" {
		return resolved{}
	}
	// 1. Live catalogue, exact key.
	if r, ok := p.openRouter[NormalizeModel(model)]; ok {
		return resolved{rates: r, ok: true}
	}
	// 2. Fallback table, most specific substring.
	lower := strings.ToLower(model)
	for _, pattern := range p.manualKeys {
		if strings.Contains(lower, pattern) {
			return resolved{rates: p.manual[pattern], ok: true}
		}
	}
	return resolved{}
}

// Cost computes the dollar cost of one call, and whether it could be priced.
func (p *Pricer) Cost(model string, input, output, cacheRead, cacheWrite int) (float64, bool) {
	rates, ok := p.Resolve(model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000
	return float64(input)/perMillion*rates.Input +
		float64(output)/perMillion*rates.Output +
		float64(cacheRead)/perMillion*rates.CacheRead +
		float64(cacheWrite)/perMillion*rates.CacheWrite, true
}

// FallbackReachability reports, for each pattern in the embedded table, whether
// it is reachable. A pattern the live catalogue always wins for is dead weight:
// editing it changes nothing, which is how the two tables drifted apart before
// a test caught it.
func (p *Pricer) FallbackReachability() map[string]bool {
	out := make(map[string]bool, len(p.manual))
	for pattern := range p.manual {
		_, viaLive := p.openRouter[NormalizeModel(pattern)]
		out[pattern] = !viaLive
	}
	return out
}

// LiveModelKeys returns every model name known to either source, which is what
// the filter dropdown offers.
func (p *Pricer) LiveModelKeys() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := make(map[string]struct{}, len(p.openRouter)+len(p.manual))
	out := make([]string, 0, len(p.openRouter)+len(p.manual))
	for k := range p.openRouter {
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	for k := range p.manual {
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- helpers

// dateStamp matches the trailing release date some providers embed in an id.
var dateStamp = regexp.MustCompile(`-20\d{6}$`)

// NormalizeModel collapses a model id to a vendor- and format-agnostic key, so
// that "claude-opus-4-8", "anthropic/claude-opus-4.8" and
// "claude-opus-4-8-20260528" all reduce to one entry.
func NormalizeModel(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(name)
	name = dateStamp.ReplaceAllString(name, "")
	return strings.ReplaceAll(name, ".", "-")
}

// Hyphenate converts a display label such as "Gemini 3.1 Pro (High)" into a
// matchable form.
//
// Antigravity identifies models by display name plus a tier suffix rather than by
// provider id, so the tier has to be removed for the fallback table to price
// every tier of a model from one entry. The whole parenthesised span goes, not
// just its brackets: keeping the tier word would leave "gemini-3.1-pro-high",
// which matches no pattern and prices at zero.
func Hyphenate(label string) string {
	var b strings.Builder
	skipping := false
	for _, r := range label {
		switch {
		case r == '(':
			skipping = true
		case r == ')':
			skipping = false
		case skipping:
			// Dropped, along with the tier it names.
		case r == ' ' || r == '_' || r == '\t':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(strings.ToLower(b.String()), "-")
}

func sortLongestFirst(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
}

func atof(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// cacheLen reports how many resolutions are memoised. Exposed for tests that
// assert resolution is cached rather than recomputed per call.
func (p *Pricer) cacheLen() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.cache)
}
