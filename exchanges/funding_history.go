package exchanges

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Historical funding rates (step 2.6) — the corpus the phase-3 backtest replays.
//
// This is a different thing from the live readings of step 2.5 and must not be
// confused with them. A live reading is the rate for a period still RUNNING; it
// moves with the premium and may never be paid at the value last seen. An entry
// here is what the venue reports in hindsight: the rate that actually settled.
//
// What every venue was measured to deliver, 2026-09-04 (full payloads and the
// probe commands: docs/DATA-REQUIREMENTS.md §9):
//
//	Binance      ≥12 months  1000 rows/req, ascending, startTime walks forward
//	Bybit        ≥12 months   200 rows/req, descending, endTime walks back
//	Kraken       ≥12 months  ONE request, ascending, no pagination at all
//	Hyperliquid  ≥12 months   500 rows/req, ascending, startTime walks forward
//	OKX          ~3 months    100 rows/req, descending, `after` walks back
//	Gate          180 days   1000 rows/req, descending, from/to window
//	Paradex      no settlements at all — a 5-second sample of a funding index
//
// The last three are why a caller must never assume the corpus is as deep as it
// asked for: OKX returns an EMPTY array beyond ~3 months and Gate answers
// "from time exceeds 180-day limit". Both are the venue's retention, not a
// failure, so the fetchers return what exists and the caller reports coverage
// from the rows themselves.

const (
	// FundingHistoryPageDelay paces a paginating fetcher. Backfill is a one-off
	// job with no deadline, and spending a venue's rate limit on it would cost
	// the live feeds — which share the same IP budget — for no gain.
	//
	// It is the DEFAULT, not a rule: pacing is a per-venue fact and a venue that
	// publishes a budget gets paced to it (hyperliquidHistoryPageDelay). Six of
	// the seven answer this rate without complaint.
	FundingHistoryPageDelay = 200 * time.Millisecond

	// MaxFundingHistoryPages bounds a paginating loop. It is a runaway guard,
	// not a budget: a venue that keeps answering with rows it already sent
	// would otherwise spin forever. Paradex needs one request per hour, so it
	// has to be large enough for a month of those.
	MaxFundingHistoryPages = 2000
)

// FundingHistoryEntry is one funding rate as the venue reports it in hindsight.
//
// It deliberately carries NO RecvAt. RecvAt means "when this measurement came
// off the wire", and is stamped in exactly three places, one per transport
// (CLAUDE.md rule 13) — a settled rate from last March has no meaningful
// receive time, and inventing one here would add a fourth stamping site whose
// value nothing may compare against a staleness threshold. When the row was
// stored is the store's business (recorded_at_ms), not the venue's.
type FundingHistoryEntry struct {
	Symbol string // normalized: BTCUSDT
	Source string // wire id: binance_futures, ...
	Model  FundingModel

	// SettledAtMs is the settlement this rate was paid at, absolute epoch ms,
	// as the venue stamps it — never rounded to a boundary. Hyperliquid's
	// stamps carry tens of milliseconds of jitter (1788480000030) and Gate's
	// land a second or three after the hour (1788480003); both are the venue's
	// own record of the event and are what a re-fetch returns again, which is
	// what makes them usable as a primary key.
	//
	// For FundingContinuous there is no settlement: this is the instant the
	// funding index was sampled, and Model says so.
	SettledAtMs int64

	RawRate      float64
	RawRateField string

	// RatePerIntervalFrac is the rate for one interval of this venue;
	// IntervalSec is that interval. RatePer8hFrac and APRFrac are derived from
	// them by DeriveFundingRates — the same arithmetic the live path uses, on
	// purpose: two implementations of "per 8h" would drift, and the phase-3
	// gate compares the two paths' numbers.
	RatePerIntervalFrac float64
	IntervalSec         int64
	RatePer8hFrac       float64
	APRFrac             float64

	// GapPrevSec is the MEASURED distance to the previous settlement in the
	// same series, which is not the same thing as IntervalSec. No venue
	// publishes an interval alongside a historical rate (Paradex is the lone
	// exception), so IntervalSec is the series' modal gap — the cadence — while
	// this is what actually happened at this row. They differ exactly when a
	// settlement was missed or the cadence changed, and keeping both puts that
	// fact in the DATA instead of in a comment nobody queries. The first row of
	// a series has no predecessor and borrows the gap to its successor.
	GapPrevSec int64

	// RateType is Binance's "Regular"/"Special" label — a Special rate is
	// dividend-driven and must be filtered out of a backtest (PLAN.md 3.3).
	// Empty everywhere else: no other venue publishes one.
	RateType string

	// MarkPriceQuote is the price funding was charged on, in the market's
	// QUOTE asset, when the venue publishes it with the historical row
	// (Binance only). 0 means not supplied.
	MarkPriceQuote float64
}

// FundingWindow is the closed-open time range a history fetch covers.
type FundingWindow struct {
	StartMs int64 // inclusive
	EndMs   int64 // exclusive
}

// contains reports whether a settlement stamp falls inside the window.
func (w FundingWindow) Contains(stampMs int64) bool {
	return stampMs >= w.StartMs && stampMs < w.EndMs
}

// FundingHistoryFetchFunc reads the settled funding rates one source published
// for one symbol inside a window, oldest first.
//
// It paginates internally, because every venue does it differently and pushing
// that into the caller would put seven cursor idioms in one loop. It returns
// what EXISTS, which may be a shorter span than asked for — a venue's retention
// limit is not an error and must not read as one.
type FundingHistoryFetchFunc func(ctx context.Context, source string, symbol Symbol, window FundingWindow) ([]FundingHistoryEntry, error)

// FundingHistoryRow is one row as a venue's own parser produces it: the venue's
// numbers, already in fractional units, before any cross-venue arithmetic.
type FundingHistoryRow struct {
	SettledAtMs    int64
	RateFrac       float64 // the rate for ONE interval of this venue
	RawRate        float64
	RawRateField   string
	RateType       string
	MarkPriceQuote float64

	// IntervalSec is filled only by a venue that publishes the interval with
	// the row (Paradex: funding_period_hours). 0 means "measure it from the
	// settlement spacing", which is what the other six require.
	IntervalSec int64
}

// FinishFundingHistory turns one venue's rows into entries: sorted, deduplicated,
// with the cadence measured and the comparison figures derived.
//
// The cadence has to be measured because no venue publishes it next to a
// historical rate. Using today's declared interval instead would be worse than
// it looks: Binance moved most symbols from 8h to 4h, so a 12-month backfill
// annotated with today's number would misstate the older half of the corpus by
// 2× — in the APR figure phase 3 ranks on.
func FinishFundingHistory(source string, symbol Symbol, model FundingModel, rows []FundingHistoryRow) ([]FundingHistoryEntry, error) {
	kept := make([]FundingHistoryRow, 0, len(rows))
	for _, row := range rows {
		if row.SettledAtMs > 0 {
			kept = append(kept, row)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}

	sort.Slice(kept, func(i, j int) bool { return kept[i].SettledAtMs < kept[j].SettledAtMs })
	// A venue asked for overlapping pages answers with the same settlement
	// twice; keeping both would put a phantom zero-second gap into the cadence
	// measurement below.
	deduped := kept[:1]
	for _, row := range kept[1:] {
		if row.SettledAtMs == deduped[len(deduped)-1].SettledAtMs {
			deduped[len(deduped)-1] = row
			continue
		}
		deduped = append(deduped, row)
	}

	gaps := make([]int64, len(deduped))
	for i := 1; i < len(deduped); i++ {
		gaps[i] = roundedGapSec(deduped[i-1].SettledAtMs, deduped[i].SettledAtMs)
	}
	if len(deduped) > 1 {
		gaps[0] = gaps[1] // the first row borrows its successor's gap
	}
	modal := modalGapSec(gaps)

	out := make([]FundingHistoryEntry, 0, len(deduped))
	for i, row := range deduped {
		interval := row.IntervalSec
		if interval == 0 {
			interval = modal
		}
		if interval <= 0 {
			// One row on its own carries no cadence, and every consumer of
			// IntervalSec divides by it. Refusing beats publishing a guess.
			return nil, fmt.Errorf("funding history %s/%s: %d row(s) give no measurable settlement spacing",
				source, symbol.Standard, len(deduped))
		}
		entry := FundingHistoryEntry{
			Symbol:              symbol.Standard,
			Source:              source,
			Model:               model,
			SettledAtMs:         row.SettledAtMs,
			RawRate:             row.RawRate,
			RawRateField:        row.RawRateField,
			RatePerIntervalFrac: row.RateFrac,
			IntervalSec:         interval,
			GapPrevSec:          gaps[i],
			RateType:            row.RateType,
			MarkPriceQuote:      row.MarkPriceQuote,
		}
		derived, err := DeriveFundingRates(FundingData{
			Source: source, Symbol: symbol.Standard,
			RatePerIntervalFrac: entry.RatePerIntervalFrac,
			IntervalSec:         entry.IntervalSec,
		})
		if err != nil {
			return nil, err
		}
		entry.RatePer8hFrac, entry.APRFrac = derived.RatePer8hFrac, derived.APRFrac
		out = append(out, entry)
	}
	return out, nil
}

// roundedGapSec is the distance between two settlement stamps in whole seconds.
//
// Rounded, not truncated: Hyperliquid stamps an hourly settlement at
// 1788480000030 and the next at 1788483600062, which truncates to 3600 but the
// pair before it truncates to 3599. A cadence measured in two values is not a
// cadence.
func roundedGapSec(fromMs, toMs int64) int64 {
	return (toMs - fromMs + MsPerSecond/2) / MsPerSecond
}

// modalGapSec is the most common settlement spacing in a series — the venue's
// cadence, measured.
//
// The mode rather than the mean or the median because outages are the normal
// distortion: Kraken's year of hourly rates has 8,764 gaps of 3600s, six of
// 7200s and one of 10800s (measured 2026-09-04), and a mean would report 3601
// while the mode reports the truth. Ties go to the smaller gap, which is the
// one a missed settlement cannot manufacture.
func modalGapSec(gaps []int64) int64 {
	counts := make(map[int64]int, len(gaps))
	for _, gap := range gaps {
		if gap > 0 {
			counts[gap]++
		}
	}
	var best, bestCount int64
	for gap, count := range counts {
		if int64(count) > bestCount || (int64(count) == bestCount && gap < best) {
			best, bestCount = gap, int64(count)
		}
	}
	return best
}

// FundingGapReport describes the settlement spacing actually observed in a
// fetched series. It is derived from the entries rather than recorded beside
// them, so it cannot drift from the data it describes.
//
// The backfill prints it, and the Kraken golden test asserts the modal gap is
// 3600 — the standing obligation from step 2.3, where the venue's hourly
// cadence had to be pinned as a constant because no Kraken funding message
// carries an interval. This is the re-measurement that constant depends on.
type FundingGapReport struct {
	Rows        int
	ModalGapSec int64
	// Counts is every observed gap and how often it occurred. More than one
	// value with real weight means either an outage or a cadence change, and
	// the two are indistinguishable from timestamps alone — which is why this
	// is reported rather than silently smoothed.
	Counts map[int64]int
}

// cadenceMinorityShare is where "an outage" stops and "two cadences" starts.
//
// The two are indistinguishable from timestamps alone, so the split is by
// WEIGHT: Kraken's year of hourly rates has six double-gaps in 8,771 (0.07%),
// while a venue that moved a symbol from 8h to 4h mid-corpus leaves the older
// spacing with tens of percent. Ten percent sits an order of magnitude clear of
// both.
const cadenceMinorityShare = 0.10

// CadenceLooksMixed reports whether more than one settlement spacing carries
// real weight in the series.
//
// It matters because IntervalSec is the series' MODAL gap, so a corpus that
// spans a cadence change has the minority era annotated with the majority's
// interval — and RatePer8hFrac and APRFrac are derived from it. Binance moved
// most symbols from 8h to 4h, which makes this a live possibility on a 12-month
// backfill rather than a theoretical one. Occasional outages do not trip it.
func (r FundingGapReport) CadenceLooksMixed() bool {
	total := 0
	for _, count := range r.Counts {
		total += count
	}
	if total == 0 {
		return false
	}
	for gap, count := range r.Counts {
		if gap != r.ModalGapSec && float64(count)/float64(total) >= cadenceMinorityShare {
			return true
		}
	}
	return false
}

// FundingGaps measures the spacing of a fetched series.
func FundingGaps(entries []FundingHistoryEntry) FundingGapReport {
	report := FundingGapReport{Rows: len(entries), Counts: map[int64]int{}}
	gaps := make([]int64, 0, len(entries))
	// The first entry's GapPrevSec is borrowed from its successor and would
	// double-count that spacing.
	for i, entry := range entries {
		if i == 0 {
			continue
		}
		gaps = append(gaps, entry.GapPrevSec)
		report.Counts[entry.GapPrevSec]++
	}
	report.ModalGapSec = modalGapSec(gaps)
	return report
}

// fundingHistoryPageAttempts is how many times one page is tried before the
// whole series is given up on.
const fundingHistoryPageAttempts = 3

// rateLimitBackoff is the wait before retrying a request the venue rate
// limited, by attempt number.
//
// Seconds, not the page delay. A rate limit is a budget over a window — a
// minute, on every venue here that publishes one — so coming back 200ms later
// asks the same question inside the same exhausted window and burns an attempt
// for nothing. Measured 2026-09-04: with the page delay as the only backoff,
// two Hyperliquid series lost their whole year to three 429s in 600ms.
func rateLimitBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 5 * time.Second
	default:
		return 20 * time.Second
	}
}

// FetchFundingHistoryPage runs one page request, retrying a transient failure.
//
// It exists because the alternative is arithmetic nobody would choose: a series
// is up to 2,000 requests and nine minutes on Paradex, and aborting all of it
// because request 1,999 came back a 502 throws away everything already
// collected. Two things are never retried — a market the venue does not list is
// not transient, and a cancelled context means the caller is shutting down.
//
// delay is the venue's own page pacing, which is also the floor for an ordinary
// retry; a rate limit overrides it with something the venue's window can
// actually absorb.
func FetchFundingHistoryPage(ctx context.Context, delay time.Duration, do func() error) error {
	var err error
	for attempt := 0; attempt < fundingHistoryPageAttempts; attempt++ {
		if attempt > 0 {
			if waitErr := FundingHistoryPause(ctx, retryDelay(err, delay, attempt)); waitErr != nil {
				return waitErr
			}
		}
		err = do()
		if err == nil || errors.Is(err, ErrNotListed) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// retryDelay picks how long to wait after a failed attempt. A venue that named
// its own Retry-After wins over both defaults — it knows its window and we are
// guessing at it.
func retryDelay(err error, pageDelay time.Duration, attempt int) time.Duration {
	var limited *rateLimitError
	if !errors.As(err, &limited) {
		return pageDelay
	}
	delay := rateLimitBackoff(attempt)
	if limited.RetryAfter > delay {
		return limited.RetryAfter
	}
	return delay
}

// FundingHistoryPause waits between pages, or returns ctx's error if the fetch
// was cancelled while waiting.
func FundingHistoryPause(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}
