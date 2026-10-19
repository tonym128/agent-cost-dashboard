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
	// Tiers are the higher rates some models charge once a prompt is long
	// enough — Gemini Pro above 200K tokens, GPT above 272K — ordered by
	// threshold. A call is priced entirely at the highest tier its prompt
	// reaches, which is how the providers bill it.
	Tiers []Tier `json:"tiers,omitempty"`
}

// Tier is a set of rates that applies from a prompt size upward.
type Tier struct {
	MinPrompt int `json:"min_prompt_tokens"`
	Rates
}

// forPrompt returns the rates a call with this many prompt tokens pays.
func (r Rates) forPrompt(prompt int) Rates {
	out := r
	for _, t := range r.Tiers {
		if prompt >= t.MinPrompt {
			out = t.Rates
		}
	}
	out.Tiers = nil
	return out
}

// cacheWrite1hMultiplier is Anthropic's price for a one-hour cache write, as a
// multiple of the base input rate. A five-minute write is 1.25x, which is what
// the catalogue's cache-write rate is; Claude Code writes the one-hour kind.
const cacheWrite1hMultiplier = 2.0

// manualPricingJSON is the fallback price table, compiled into the binary so a
// misconfigured or missing path can never leave the dashboard unable to price
// anything at all.
//
//go:embed manual_pricing.json
var manualPricingJSON []byte

// pricingOverridesJSON holds prices checked against a provider's own price
// list, for models where the OpenRouter catalogue charges something else — a
// reseller margin, typically. It is consulted before the catalogue, by exact
// normalised key, so it can only ever affect the models it names.
//
//go:embed pricing_overrides.json
var pricingOverridesJSON []byte

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
	overrides  map[string]Rates
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
	if err := p.loadOverrides(); err != nil {
		return nil, err
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

// NewFallbackPricer builds a pricer from the embedded table alone, with no live
// catalogue.
//
// This is the configuration that exists precisely because models.json can go
// missing — a `go install`ed binary has no dump beside it. It is also the
// configuration the tests need in order to ask the only question about the
// fallback table that matters: with the dump absent, does every model still get
// a real number, or an explicit "unpriced"?
func NewFallbackPricer() (*Pricer, error) {
	return NewPricer("", "")
}

type openRouterDoc struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			openRouterPrice
			Overrides []struct {
				MinPrompt int `json:"min_prompt_tokens"`
				openRouterPrice
			} `json:"overrides"`
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
	for _, m := range doc.Data {
		if m.ID == "" {
			continue
		}
		entry := m.Pricing.rates(Rates{})
		// A long-prompt override lists only the rates that change; the rest
		// carry over from the base price.
		for _, o := range m.Pricing.Overrides {
			if o.MinPrompt <= 0 {
				continue
			}
			entry.Tiers = append(entry.Tiers, Tier{MinPrompt: o.MinPrompt, Rates: o.rates(entry)})
		}
		sort.Slice(entry.Tiers, func(i, j int) bool { return entry.Tiers[i].MinPrompt < entry.Tiers[j].MinPrompt })
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

// openRouterPrice is one price as the catalogue spells it: dollars per token,
// as strings, with an absent field meaning "not listed".
type openRouterPrice struct {
	Prompt     *string `json:"prompt"`
	Completion *string `json:"completion"`
	CacheRead  *string `json:"input_cache_read"`
	CacheWrite *string `json:"input_cache_write"`
}

// rates converts a catalogue price to per-million rates, taking any field the
// price does not list from base.
func (o openRouterPrice) rates(base Rates) Rates {
	const perMillion = 1_000_000
	pick := func(v *string, fallback float64) float64 {
		if v == nil {
			return fallback
		}
		return perMillion * atof(*v)
	}
	return Rates{
		Input:      pick(o.Prompt, base.Input),
		Output:     pick(o.Completion, base.Output),
		CacheRead:  pick(o.CacheRead, base.CacheRead),
		CacheWrite: pick(o.CacheWrite, base.CacheWrite),
	}
}

func (p *Pricer) loadOverrides() error {
	p.overrides = map[string]Rates{}
	raw := map[string]Rates{}
	if err := json.Unmarshal(pricingOverridesJSON, &raw); err != nil {
		return fmt.Errorf("parse embedded price overrides: %w", err)
	}
	for k, r := range raw {
		p.overrides[NormalizeModel(k)] = r
	}
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
	// 0. A price checked against the provider's own list, exact key.
	if r, ok := p.overrides[NormalizeModel(model)]; ok {
		return resolved{rates: r, ok: true}
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
	return p.CostCall(model, input, output, cacheRead, cacheWrite, 0)
}

// CostCall is Cost with the share of the cache writes that were one-hour
// writes, which Anthropic bills at twice the input rate rather than at the
// five-minute rate the catalogue lists. cacheWrite1h is part of cacheWrite,
// not in addition to it.
//
// The prompt that selects a long-prompt tier is every input bucket together:
// the parsers store them disjoint, and the providers count all of them.
func (p *Pricer) CostCall(model string, input, output, cacheRead, cacheWrite, cacheWrite1h int) (float64, bool) {
	all, ok := p.Resolve(model)
	if !ok {
		return 0, false
	}
	rates := all.forPrompt(input + cacheRead + cacheWrite)
	if cacheWrite1h > cacheWrite {
		cacheWrite1h = cacheWrite
	}
	if cacheWrite1h < 0 {
		cacheWrite1h = 0
	}
	const perMillion = 1_000_000
	return float64(input)/perMillion*rates.Input +
		float64(output)/perMillion*rates.Output +
		float64(cacheRead)/perMillion*rates.CacheRead +
		float64(cacheWrite-cacheWrite1h)/perMillion*rates.CacheWrite +
		float64(cacheWrite1h)/perMillion*rates.Input*cacheWrite1hMultiplier, true
}

// FallbackReachability reports, for each pattern in the embedded table, whether
// the live catalogue would shadow it.
//
// A shadowed pattern is *not* dead weight. It is the only thing that prices
// that model when models.json is absent, so shadowing is the normal, expected
// state for most of the table — it means the two sources agree, not that the
// entry is redundant. This is diagnostic output, not a health metric, and
// nothing should assert on how many patterns are shadowed.
func (p *Pricer) FallbackReachability() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]bool, len(p.manual))
	for pattern := range p.manual {
		_, viaLive := p.openRouter[NormalizeModel(pattern)]
		out[pattern] = !viaLive
	}
	return out
}

// LiveCatalogueSize reports how many models the OpenRouter dump contributed.
//
// Zero means the dump was absent or empty and every price is coming from the
// embedded fallback table, which is a materially less accurate source — the
// caller uses this to warn rather than to refuse, because the fallback is a
// deliberate feature and running with it beats not running.
func (p *Pricer) LiveCatalogueSize() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.openRouter)
}

// FallbackOnly reports whether the pricer is running on the embedded table with
// no live catalogue behind it.
func (p *Pricer) FallbackOnly() bool {
	return p.LiveCatalogueSize() == 0
}

// FallbackSize reports how many patterns the fallback table holds, which is the
// ceiling on what a dump-less run can price.
func (p *Pricer) FallbackSize() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.manual)
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
