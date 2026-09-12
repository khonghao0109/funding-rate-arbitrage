package main

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"futures-arbitrage-scanner/exchanges"
	"futures-arbitrage-scanner/exchanges/venues"
	"futures-arbitrage-scanner/internal/config"
	"futures-arbitrage-scanner/internal/history"
	"futures-arbitrage-scanner/internal/scanner"
)

func TestPriceSamplesShareOneSampleInstant(t *testing.T) {
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	recvAt := at.Add(-250 * time.Millisecond)

	samples := priceSamples([]scanner.PriceReading{
		{Symbol: "BTCUSDT", Source: "binance_futures", PricePoint: scanner.PricePoint{
			Price: 80000, BestBid: 79999, BestAsk: 80001,
			BestBidQtyCoin: 1.5, BestAskQtyCoin: 2, RecvAt: recvAt,
		}},
		{Symbol: "BTCUSDT", Source: "okx_futures", PricePoint: scanner.PricePoint{
			Price: 80010, BestBid: 80009, BestAsk: 80011, RecvAt: recvAt.Add(-time.Second),
		}},
	}, at)

	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	// Every row of a round carries the SAME instant: a cross-venue spread is
	// only meaningful between prices sampled together, and per-row stamps would
	// turn that query into a join on approximate times.
	if samples[0].SampledAtMs != samples[1].SampledAtMs || samples[0].SampledAtMs != at.UnixMilli() {
		t.Errorf("sample instants = %d and %d, want both %d",
			samples[0].SampledAtMs, samples[1].SampledAtMs, at.UnixMilli())
	}
	// The receive stamps stay per row — that is what makes staleness
	// reconstructible from the stored data later.
	if samples[0].RecvAtMs == samples[1].RecvAtMs {
		t.Error("both rows got the same RecvAtMs; the two feeds were read a second apart")
	}
	if samples[1].BestBidQtyCoin != 0 {
		t.Errorf("okx quantity = %g, want 0 — a contract-denominated book is NOT KNOWN, not empty",
			samples[1].BestBidQtyCoin)
	}
}

func TestPriceSamplesDropUnstampedReadings(t *testing.T) {
	// A zero time.Time is -6795364578 in Unix milliseconds. Stored, it would be
	// a price sample dated 1754 that passes every window filter phase 3 writes.
	samples := priceSamples([]scanner.PriceReading{
		{Symbol: "BTCUSDT", Source: "binance_futures", PricePoint: scanner.PricePoint{Price: 80000}},
	}, time.Now())

	if len(samples) != 0 {
		t.Fatalf("stored %+v, want nothing: the reading carries no receive time", samples)
	}
}

// The scanner's funding history jobs are built from the same config the
// connectors are, so a source added to config.yaml starts being collected
// without a second list to keep in step.
func TestFundingHistoryJobsCoverEveryPerpSource(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}

	jobs := fundingHistoryJobs(cfg)
	byConnector := map[string]int{}
	for _, job := range jobs {
		byConnector[job.Connector] = len(job.Symbols)
	}

	fetchers := venues.FundingHistoryFetchers()
	for connector := range fetchers {
		symbols, ok := byConnector[connector]
		if !ok {
			t.Errorf("connector %q has a funding history fetcher but no configured source reaches it", connector)
			continue
		}
		if symbols == 0 {
			t.Errorf("connector %q was given no symbols to collect", connector)
		}
	}
	// And the other direction: a spot source must not be handed to a history
	// fetcher, because a spot market has no funding at all.
	for _, source := range cfg.Sources {
		if source.MarketType == "perp" {
			continue
		}
		if _, ok := fetchers[source.Connector]; ok {
			t.Errorf("source %q is %s but its connector has a funding history fetcher",
				source.Source, source.MarketType)
		}
	}
}

// A tick that fires later than its schedule allows is LOGGED and REPORTED
// to the sink with its job name (PLAN 3.5, debt of 2026-09-10). The wall
// clock is injected and JUMPS five hours between the second arming and the
// second firing — the loop reads wallNow exactly four times up to that
// firing (due, fired, due, fired), so the fourth reading is the one after
// the "sleep" — which is deterministic and needs no tolerance games.
func TestTickLoop_ReportsLateTicksToTheSinkWithTheJobName(t *testing.T) {
	oldNow, oldSink := wallNow, lateTickSink
	defer func() { wallNow, lateTickSink = oldNow, oldSink }()

	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	const sleep = 5 * time.Hour
	calls := 0 // read on the loop goroutine only
	wallNow = func() time.Time {
		calls++
		if calls >= 4 {
			return base.Add(sleep)
		}
		return base
	}

	var mu sync.Mutex
	var jobs []string
	var ats []time.Time
	var lateBys []time.Duration
	lateTickSink = func(job string, at time.Time, lateBy time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		jobs = append(jobs, job)
		ats = append(ats, at)
		lateBys = append(lateBys, lateBy)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const every = 5 * time.Millisecond
	ticks := 0
	var seen []time.Time
	done := make(chan struct{})
	go func() {
		defer close(done)
		tickLoop(ctx, "test job", every, every, func(at time.Time) {
			ticks++
			seen = append(seen, at)
			if ticks == 2 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tickLoop did not stop after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(jobs) != 1 {
		t.Fatalf("sink saw %d late ticks, want exactly 1 (the tick after the 5h jump)", len(jobs))
	}
	if jobs[0] != "test job" {
		t.Fatalf("late tick reported job %q, want %q", jobs[0], "test job")
	}
	if want := sleep - every; lateBys[0] != want {
		t.Fatalf("late by %s, want %s (5h sleep minus the 5ms the timer was due)", lateBys[0], want)
	}
	if !ats[0].Equal(base.Add(sleep)) {
		t.Fatalf("late tick stamped %s, want the wall instant of the firing %s", ats[0], base.Add(sleep))
	}
	// fn is handed the RECEIPT instant, never the backdated scheduled one.
	if len(seen) != 2 || !seen[1].Equal(base.Add(sleep)) {
		t.Fatalf("fn saw %v, want the second call at the post-sleep wall time", seen)
	}
}

// At the real tolerance an on-time tick is NOT late: a loop that reports
// every tick would make the counter noise.
func TestTickLoop_DoesNotReportOnTimeTicks(t *testing.T) {
	oldSink := lateTickSink
	defer func() { lateTickSink = oldSink }()
	var reported int32
	lateTickSink = func(string, time.Time, time.Duration) { atomic.AddInt32(&reported, 1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		tickLoop(ctx, "on time", 5*time.Millisecond, 5*time.Millisecond, func(time.Time) {
			ticks++
			if ticks == 3 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tickLoop did not stop after cancellation")
	}
	if n := atomic.LoadInt32(&reported); n != 0 {
		t.Fatalf("%d on-time ticks were reported late", n)
	}
}

// The funding top-up is a periodic job like any other and must be counted like
// any other. It ran on internal/history's own ticker until 2026-09-12, which
// left exactly one loop in the process invisible to prices.tick_status — the
// one whose whole job is to fetch the settlements that happened while the
// machine was asleep.
func TestTopUpFundingHistory_IsCountedByTheSharedScheduler(t *testing.T) {
	oldNow, oldSink := wallNow, lateTickSink
	defer func() { wallNow, lateTickSink = oldNow, oldSink }()

	// tickLoop reads wallNow three times before the SECOND firing — arming
	// due, stamping the first firing, re-arming due — so the fourth reading is
	// the one after the "sleep". Counting the calls rather than sleeping is
	// what makes this deterministic; if a wallNow call is ever added inside
	// tickLoop, this number moves with it.
	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	calls := 0 // read on the loop goroutine only
	wallNow = func() time.Time {
		calls++
		if calls >= 4 {
			return base.Add(5 * time.Hour)
		}
		return base
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var jobs []string
	lateTickSink = func(job string, _ time.Time, _ time.Duration) {
		mu.Lock()
		jobs = append(jobs, job)
		mu.Unlock()
		cancel()
	}

	// A collector with one job it CAN serve: the loop's guard reads the
	// collector's own filtered list, and a fetcher that returns nothing keeps
	// the test offline while leaving a real job to schedule.
	job := history.Job{Source: "s", Connector: "c", Symbols: []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}}}
	collector := history.New(openTempStore(t), []history.Job{job},
		map[string]exchanges.FundingHistoryFetchFunc{"c": emptyFundingFetcher})

	done := make(chan struct{})
	go func() {
		defer close(done)
		topUpFundingHistory(ctx, collector, time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the top-up loop did not stop after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(jobs) == 0 {
		t.Fatal("a five-hour jump produced no late tick for the funding top-up; the loop is still uncounted")
	}
	// The literal, not the constant: the constant's own doc says this exact
	// string is what the operator reads on the wire and in the log, so a
	// rename is a wire change and must fail here rather than pass silently.
	if jobs[0] != "storage: funding top-up" {
		t.Fatalf("late tick reported job %q, want %q", jobs[0], "storage: funding top-up")
	}
	// And it must stay DISTINCT from the collection report's prefix, which has
	// been in the log since step 2.6 and which operators grep. Collapsing the
	// two would make a scheduling fault and a collection result look alike.
	if fundingTopUpJob == "funding history top-up" {
		t.Error("the scheduler's job name and the collection report's prefix have been merged")
	}
}

// Nothing to collect must not spin a timer: the guard is what keeps a scanner
// with storage on but no fetchable series from waking every period forever.
func TestTopUpFundingHistory_DoesNothingWithoutJobsOrAPeriod(t *testing.T) {
	db := openTempStore(t)
	job := history.Job{Source: "s", Connector: "c", Symbols: []exchanges.Symbol{{Standard: "BTCUSDT", Venue: "BTCUSDT"}}}
	fetchers := map[string]exchanges.FundingHistoryFetchFunc{"c": emptyFundingFetcher}

	for name, tc := range map[string]struct {
		collector *history.Collector
		every     time.Duration
	}{
		"no jobs at all": {history.New(db, nil, fetchers), time.Hour},
		// The job list is not empty, but NO connector in it has a history
		// fetcher, so the collector keeps none. Guarding on the list this
		// process built rather than on the collector's own would arm a timer
		// here and wake the process every period forever with nothing to do.
		"jobs the collector cannot serve": {history.New(db, []history.Job{job}, nil), time.Hour},
		"no period":                       {history.New(db, []history.Job{job}, fetchers), 0},
		"nil collector":                   {nil, time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				topUpFundingHistory(context.Background(), tc.collector, tc.every)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("topUpFundingHistory did not return; it armed a timer with nothing to do")
			}
		})
	}
}

// emptyFundingFetcher is a fetcher that opens no socket and returns nothing,
// so a scheduling test can have a real job without going near a venue.
func emptyFundingFetcher(context.Context, string, exchanges.Symbol, exchanges.FundingWindow) ([]exchanges.FundingHistoryEntry, error) {
	return nil, nil
}
