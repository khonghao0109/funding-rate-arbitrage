package instruments

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"futures-arbitrage-scanner/exchanges"
)

// RefreshInterval is how often the registry re-fetches every venue's rules.
// Instrument metadata is class-A data (docs/DATA-REQUIREMENTS.md §1): it
// changes on the venue's schedule, not the market's, and once a day is the
// documented cadence — the daily check is also the delisting watch, since no
// venue announces one through an API (§7.10).
const RefreshInterval = 24 * time.Hour

// refreshRetryInterval is used instead when the last refresh had errors. At
// process start the registry is empty, and one transient venue blip must not
// leave a source ruleless for a full day.
const refreshRetryInterval = 5 * time.Minute

// refreshRetryCeiling caps how far consecutive failures back the retry off.
// Retrying doubles from refreshRetryInterval, because a PERMANENTLY broken
// source — a pair the venue simply does not list — otherwise held the whole
// registry at 5-minute polling forever: Refresh re-fetches every source, two
// of which are ~1MB unfiltered lists, so that was 288 full sweeps a day with
// the same missing-symbols line logged every 5 minutes. At the ceiling a
// broken source still gets four chances a day against the healthy 24h cycle.
const refreshRetryCeiling = 6 * time.Hour

// Source is one venue feed the registry keeps rules for: its wire name, the
// symbols it serves (in both spellings), and the fetcher that reads its rules.
type Source struct {
	Name    string
	Symbols []exchanges.Symbol
	Fetch   exchanges.InstrumentFetchFunc
}

// Registry caches every source's instrument rules in memory, refreshed daily.
// Reads are lock-cheap and never block a refresh; a failed refresh keeps the
// previous rules — day-old rules beat no rules, and the error says which
// venue is stale.
type Registry struct {
	sources []Source

	mu          sync.RWMutex
	bySource    map[string]map[string]exchanges.Instrument // source → symbol → rules
	refreshedAt map[string]time.Time
}

func New(sources []Source) *Registry {
	return &Registry{
		sources:     sources,
		bySource:    make(map[string]map[string]exchanges.Instrument, len(sources)),
		refreshedAt: make(map[string]time.Time, len(sources)),
	}
}

// Refresh fetches every source's rules, concurrently — a venue timing out
// must cost one timeout, not stall the sources behind it. Sources fail
// independently: the ones that answered are applied, the ones that did not
// keep their previous data, and the joined error names each failure.
//
// Application is atomic per SOURCE, not per cycle: between two sources'
// swaps a reader can see today's rules on one venue and yesterday's on
// another. Each source's rules are internally consistent, so this is
// accepted — a whole-cycle swap would buy little and delay fresh rules
// behind the slowest venue.
func (r *Registry) Refresh(ctx context.Context) error {
	errs := make([]error, len(r.sources))
	var wg sync.WaitGroup
	for i, src := range r.sources {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			fetched, err := src.Fetch(ctx, src.Name, src.Symbols)
			if err != nil {
				errs[i] = fmt.Errorf("%s: %w", src.Name, err)
				return
			}
			// A "successful" fetch of nothing for a source that asked for
			// symbols is a wipe, not a refresh: a decode-to-empty response or
			// a venue-wide incident must keep yesterday's rules and be named,
			// exactly like a transport failure. It can also be legitimate —
			// a venue that lists none of the configured pairs — so the
			// message names both readings instead of asserting an outage.
			if len(fetched) == 0 && len(src.Symbols) > 0 {
				errs[i] = fmt.Errorf("%s: fetch returned 0 instruments for %d symbols — keeping previous rules "+
					"(either the venue answered empty, or it lists none of these pairs and the source does not belong in config)", src.Name, len(src.Symbols))
				return
			}
			// Symbols the venue did not return are absent by the
			// InstrumentFetchFunc contract — a pair it genuinely does not
			// list, OR a symbol_map typo. Absence is silent everywhere else
			// (the hedge mapping self-excludes it), so name it here: this is
			// the only place that knows what was ASKED for.
			if missing := missingSymbols(src.Symbols, fetched); len(missing) > 0 {
				log.Printf("instrument registry: %s does not list %s — absent from the hedge mapping "+
					"(check symbol_map if the venue does list it)", src.Name, strings.Join(missing, ", "))
			}
			bySymbol := make(map[string]exchanges.Instrument, len(fetched))
			for _, inst := range fetched {
				bySymbol[inst.Symbol] = inst
			}
			r.mu.Lock()
			r.bySource[src.Name] = bySymbol
			r.refreshedAt[src.Name] = time.Now()
			r.mu.Unlock()
		}(i, src)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// missingSymbols names the configured symbols a fetch did not return, in the
// configured order.
func missingSymbols(asked []exchanges.Symbol, fetched []exchanges.Instrument) []string {
	got := make(map[string]bool, len(fetched))
	for _, inst := range fetched {
		got[inst.Symbol] = true
	}
	var missing []string
	for _, s := range asked {
		if !got[s.Standard] {
			missing = append(missing, s.Standard)
		}
	}
	return missing
}

// Run refreshes immediately and then keeps the registry fresh until the
// context ends: daily on success, every few minutes while any source is
// failing (at startup the cache is empty — a transient blip must not leave a
// venue ruleless for 24h). It owns the loop so the policy is testable here,
// not buried in cmd wiring.
//
// afterRefresh, when non-nil, runs on this goroutine after every refresh
// attempt — failed ones included, since yesterday's kept rules are still the
// truth being served. Step 2.4's hedge-mapping log hangs off it; step 2.7's
// dashboard feed is expected to as well.
func (r *Registry) Run(ctx context.Context, afterRefresh func()) {
	retryWait := time.Duration(refreshRetryInterval)
	for {
		wait := time.Duration(RefreshInterval)
		if err := r.Refresh(ctx); err != nil {
			log.Printf("instrument refresh: %v", err)
			// Escalate while the failure persists, reset once it clears: the
			// fast retry exists for the transient startup blip, not for a
			// misconfigured pair that will fail identically all day.
			wait = retryWait
			retryWait = min(retryWait*2, refreshRetryCeiling)
		} else {
			retryWait = refreshRetryInterval
		}
		log.Printf("instrument registry: %d instruments across %d sources", r.Count(), len(r.sources))
		if afterRefresh != nil {
			afterRefresh()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Instrument returns the cached rules for one symbol on one source. A miss
// means the venue does not list the market, or its source has never been
// fetched successfully — either way there are no rules to act on.
func (r *Registry) Instrument(source, symbol string) (exchanges.Instrument, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.bySource[source][symbol]
	return inst, ok
}

// Snapshot returns a copy of every cached instrument, sorted by source then
// symbol — a stable input for BuildHedgeMapping.
func (r *Registry) Snapshot() []exchanges.Instrument {
	r.mu.RLock()
	out := make([]exchanges.Instrument, 0, 8*len(r.bySource))
	for _, bySymbol := range r.bySource {
		for _, inst := range bySymbol {
			out = append(out, inst)
		}
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

// Count reports how many instruments are cached, for startup logging.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, bySymbol := range r.bySource {
		n += len(bySymbol)
	}
	return n
}

// RefreshedAt reports when a source last refreshed successfully; zero time
// means never.
func (r *Registry) RefreshedAt(source string) time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.refreshedAt[source]
}
