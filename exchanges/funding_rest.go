package exchanges

import (
	"context"
	"log"
	"sync"
	"time"
)

// The REST side of funding collection (step 2.5).
//
// Two venues cannot be served by their WebSocket feed alone:
//
//   - Binance publishes funding on a mark-price stream that DELIVERS NOTHING
//     to this environment. Measured 2026-09-04: one socket subscribed to
//     btcusdt@markPrice@1s and btcusdt@bookTicker together carried 4,782
//     bookTicker frames and zero markPriceUpdate frames in 45 seconds, after
//     the server acknowledged the subscription ({"result":null,"id":1}). REST
//     premiumIndex returns the same numbers and answers every time, so
//     Binance funding is polled. Documented in docs/DATA-REQUIREMENTS.md §3.4
//     rather than worked around silently.
//   - Hyperliquid's activeAssetCtx carries the rate but neither the interval
//     nor a settlement stamp; predictedFundings publishes both, and the venue
//     declaring its own cadence is what CLAUDE.md rule 3 asks for.
//
// Both run on the same tiny scheduler below. A REST failure must never take
// the price feed with it: these run beside RunStream, not inside it, so a
// venue that stops answering REST loses its funding readings and keeps its
// prices.

const (
	// FundingPollEvery is how often a REST-sourced funding reading is
	// refreshed. Funding moves continuously with the premium but the figure
	// that matters settles on a schedule measured in hours, so seconds of
	// staleness cost nothing — while a tight poll would spend rate-limit
	// budget the order path needs in phase 4.
	FundingPollEvery = 15 * time.Second

	// FundingMetaEvery is how often the slow-moving metadata is refreshed:
	// intervals change on a venue's own schedule (class-A data, once a day
	// would do), but Hyperliquid's settlement stamp rides along in the same
	// response and moves hourly, so this is the cadence the FASTEST field in
	// the payload needs.
	FundingMetaEvery = 5 * time.Minute

	// fundingRetryEvery is used after a failure, so a transient outage costs
	// one gap rather than a full interval.
	fundingRetryEvery = 30 * time.Second
)

// PollFunding runs work immediately and then on a schedule until ctx ends,
// retrying sooner after a failure. It is the shared loop for every REST-backed
// funding job; the caller owns what one tick does.
func PollFunding(ctx context.Context, source, job string, every time.Duration, work func(context.Context) error) {
	for {
		wait := every
		if err := work(ctx); err != nil {
			if ctx.Err() != nil {
				return // shutting down: the error describes the cancellation
			}
			log.Printf("%s: %s: %v", source, job, err)
			wait = fundingRetryEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// FundingMetaCache holds per-symbol funding metadata a venue publishes only
// over REST, keyed by the STANDARD symbol. Reads happen on the WebSocket
// handler's goroutine while the refresher writes, so it is mutex-guarded.
type FundingMetaCache struct {
	mu       sync.RWMutex
	bySymbol map[string]FundingMetaEntry
}

// FundingMetaEntry is what a venue's REST metadata contributes to a reading
// its WebSocket feed cannot supply on its own.
type FundingMetaEntry struct {
	IntervalHours   int64
	NextFundingAtMs int64 // 0 when the venue publishes none
	CapFrac         float64
	FloorFrac       float64
	HasCap          bool
	HasFloor        bool
}

func NewFundingMetaCache() *FundingMetaCache {
	return &FundingMetaCache{bySymbol: map[string]FundingMetaEntry{}}
}

func (c *FundingMetaCache) Put(bySymbol map[string]FundingMetaEntry) {
	c.mu.Lock()
	c.bySymbol = bySymbol
	c.mu.Unlock()
}

// get reports the metadata for one standard symbol. The second result is
// false when nothing has been fetched for it yet — the caller must then
// publish NO funding reading at all, because every consumer of IntervalSec
// divides by it.
func (c *FundingMetaCache) Get(symbol string) (FundingMetaEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.bySymbol[symbol]
	return entry, ok
}

// FutureStampMs returns stampMs when it is still ahead of now, and 0 when it
// has already passed.
//
// A cached settlement stamp goes stale between refreshes — Hyperliquid settles
// hourly while its metadata is re-read every few minutes — and publishing a
// moment that has gone is worse than publishing none: a dashboard countdown
// would run backwards and phase-3 settlement counting would credit a
// settlement that already happened. 0 means "not supplied", which every
// consumer already handles.
func FutureStampMs(stampMs int64, now time.Time) int64 {
	if stampMs > now.UnixMilli() {
		return stampMs
	}
	return 0
}
