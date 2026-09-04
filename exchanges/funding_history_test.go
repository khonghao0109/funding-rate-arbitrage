package exchanges

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Golden tests for the settled-funding parsers (step 2.6), against the real
// responses recorded 2026-09-04 into testdata/funding_history_*.json.
//
// What these are actually guarding is not "does JSON Decode". It is the four
// ways a funding history parser produces numbers that are wrong but not
// obviously wrong: reading Kraken's absolute price amount as a rate, reading
// Gate's seconds as milliseconds, measuring Paradex's five-second sampling
// interval as its funding period, and annotating every row with today's
// interval when the venue's cadence changed mid-corpus.

func TestModalGapSec(t *testing.T) {
	for _, tc := range []struct {
		name string
		gaps []int64
		want int64
	}{
		{"hourly with two outages", []int64{3600, 3600, 7200, 3600, 3600, 10800}, 3600},
		{"eight hourly", []int64{28800, 28800, 28800}, 28800},
		{"a tie goes to the smaller gap, which an outage cannot manufacture",
			[]int64{3600, 7200}, 3600},
		{"zeros are not a cadence", []int64{0, 0}, 0},
		{"nothing to measure", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modalGapSec(tc.gaps); got != tc.want {
				t.Errorf("modalGapSec(%v) = %d, want %d", tc.gaps, got, tc.want)
			}
		})
	}
}

func TestRoundedGapSecAbsorbsVenueJitter(t *testing.T) {
	// The two consecutive Hyperliquid stamps that make truncation fail.
	if got := roundedGapSec(1788480000030, 1788483600062); got != 3600 {
		t.Errorf("gap = %d, want 3600", got)
	}
	if got := roundedGapSec(1788483600062, 1788487200002); got != 3600 {
		t.Errorf("gap = %d, want 3600", got)
	}
}

func TestFinishFundingHistoryDeduplicatesAndOrders(t *testing.T) {
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	// Overlapping pages: the same settlement arrives twice, out of order.
	rows := []FundingHistoryRow{
		{SettledAtMs: 3000 * MsPerSecond, RateFrac: 0.0003, RawRateField: "r"},
		{SettledAtMs: 1000 * MsPerSecond, RateFrac: 0.0001, RawRateField: "r"},
		{SettledAtMs: 2000 * MsPerSecond, RateFrac: 0.0002, RawRateField: "r"},
		{SettledAtMs: 2000 * MsPerSecond, RateFrac: 0.0002, RawRateField: "r"},
	}
	entries, err := FinishFundingHistory("test", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 — the duplicate settlement was kept", len(entries))
	}
	for i, want := range []int64{1000, 2000, 3000} {
		if entries[i].SettledAtMs != want*MsPerSecond {
			t.Errorf("entry %d at %d, want %d", i, entries[i].SettledAtMs, want*MsPerSecond)
		}
	}
	// A duplicate kept would have shown up here as a 0-second gap winning the
	// mode, and every APR would be an infinity.
	if entries[0].IntervalSec != 1000 {
		t.Errorf("IntervalSec = %d, want 1000", entries[0].IntervalSec)
	}
	// The first row has no predecessor and borrows its successor's spacing;
	// leaving it 0 would make its own APR undefined.
	if entries[0].GapPrevSec != 1000 {
		t.Errorf("first GapPrevSec = %d, want 1000 borrowed from the next row", entries[0].GapPrevSec)
	}
}

func TestFinishFundingHistoryRefusesUnmeasurableCadence(t *testing.T) {
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	rows := []FundingHistoryRow{{SettledAtMs: 1000, RateFrac: 0.0001, RawRateField: "r"}}
	if _, err := FinishFundingHistory("test", symbol, FundingDiscrete, rows); err == nil {
		t.Fatal("one row with no declared interval was accepted; every consumer divides by IntervalSec")
	}
}

func TestFinishFundingHistoryKeepsMeasuredGapBesideCadence(t *testing.T) {
	// An hourly series with one settlement missed. The cadence stays hourly for
	// every row — that is what the comparison figures must use — while the row
	// after the gap records what actually happened.
	symbol := Symbol{Standard: "BTCUSDT", Venue: "BTCUSDT"}
	var rows []FundingHistoryRow
	for _, atSec := range []int64{3600, 7200, 10800, 18000, 21600} {
		rows = append(rows, FundingHistoryRow{
			SettledAtMs: atSec * MsPerSecond, RateFrac: 0.0001, RawRateField: "r",
		})
	}
	entries, err := FinishFundingHistory("test", symbol, FundingDiscrete, rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range entries {
		if entry.IntervalSec != 3600 {
			t.Errorf("entry %d: IntervalSec = %d, want the modal 3600", i, entry.IntervalSec)
		}
	}
	if entries[3].GapPrevSec != 7200 {
		t.Errorf("the row after the missed settlement records GapPrevSec = %d, want 7200",
			entries[3].GapPrevSec)
	}
	report := FundingGaps(entries)
	if report.ModalGapSec != 3600 || report.Counts[7200] != 1 {
		t.Errorf("gap report = %+v, want modal 3600 with one 7200", report)
	}
}

func TestCadenceLooksMixedSeparatesOutagesFromACadenceChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts map[int64]int
		modal  int64
		want   bool
	}{
		{
			// Kraken's real year: six double-gaps and one triple in 8,771.
			name:   "a handful of missed settlements is not a second cadence",
			counts: map[int64]int{3600: 8764, 7200: 6, 10800: 1},
			modal:  3600,
			want:   false,
		},
		{
			// A symbol Binance moved from 8h to 4h part-way through the year.
			name:   "two eras with real weight are",
			counts: map[int64]int{14400: 1200, 28800: 400},
			modal:  14400,
			want:   true,
		},
		{
			name:   "one spacing is never mixed",
			counts: map[int64]int{28800: 1095},
			modal:  28800,
			want:   false,
		},
		{
			name:   "an empty series says nothing",
			counts: map[int64]int{},
			modal:  0,
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := FundingGapReport{ModalGapSec: tc.modal, Counts: tc.counts}
			if got := report.CadenceLooksMixed(); got != tc.want {
				t.Errorf("CadenceLooksMixed() = %v, want %v for %v", got, tc.want, tc.counts)
			}
		})
	}
}

func TestFetchFundingHistoryPageRetriesOnlyWhatIsWorthRetrying(t *testing.T) {
	transient := errors.New("HTTP 502")

	t.Run("a blip is retried and the page is kept", func(t *testing.T) {
		// The alternative is losing a series: a Paradex fetch is 2,000 requests
		// and nine minutes, and aborting it over one 502 discards all of it.
		calls := 0
		err := FetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("err = %v after %d calls; want success on the third", err, calls)
		}
	})

	t.Run("a persistent failure gives up and reports the last error", func(t *testing.T) {
		calls := 0
		err := FetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			return transient
		})
		if !errors.Is(err, transient) || calls != fundingHistoryPageAttempts {
			t.Fatalf("err = %v after %d calls; want the venue's error after %d",
				err, calls, fundingHistoryPageAttempts)
		}
	})

	t.Run("a market the venue does not list is not retried", func(t *testing.T) {
		// Not transient: retrying it spends two more requests and one more
		// second per page to be told the same thing.
		calls := 0
		err := FetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			return fmt.Errorf("wrapped: %w", ErrNotListed)
		})
		if !errors.Is(err, ErrNotListed) || calls != 1 {
			t.Fatalf("err = %v after %d calls; want one call", err, calls)
		}
	})

	t.Run("a cancelled fetch stops immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		if err := FetchFundingHistoryPage(ctx, 0, func() error {
			calls++
			return transient
		}); err == nil {
			t.Fatal("a cancelled fetch reported success")
		}
		if calls != 1 {
			t.Fatalf("%d calls after cancellation; want one", calls)
		}
	})

	t.Run("a rate limit is retried like any other transient failure", func(t *testing.T) {
		// It is transient by definition - the budget refills - and the venue
		// that produced it (Hyperliquid, 2026-09-04) cost two whole series.
		calls := 0
		err := FetchFundingHistoryPage(context.Background(), 0, func() error {
			calls++
			if calls < 2 {
				return &rateLimitError{URL: "https://api.hyperliquid.xyz/info", Body: "null"}
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("err = %v after %d calls; want success on the second", err, calls)
		}
	})
}

func TestRetryDelayWaitsLongerForARateLimitThanForABlip(t *testing.T) {
	const pageDelay = 200 * time.Millisecond

	t.Run("an ordinary failure waits one page delay", func(t *testing.T) {
		if got := retryDelay(errors.New("HTTP 502"), pageDelay, 1); got != pageDelay {
			t.Fatalf("retryDelay = %s, want the page delay %s", got, pageDelay)
		}
	})

	t.Run("a rate limit waits seconds, and longer the second time", func(t *testing.T) {
		// The whole point: a budget measured over a minute is not cleared by
		// coming back 200ms later, which is how three attempts were spent in
		// 600ms and a 12-month series was lost.
		limited := fmt.Errorf("wrapped: %w", &rateLimitError{URL: "u", Body: "null"})
		first, second := retryDelay(limited, pageDelay, 1), retryDelay(limited, pageDelay, 2)
		if first < time.Second {
			t.Fatalf("first rate-limit retry waits %s; that is inside the same exhausted window", first)
		}
		if second <= first {
			t.Fatalf("second retry waits %s, not more than the first %s", second, first)
		}
	})

	t.Run("the venue's own Retry-After wins when it is longer", func(t *testing.T) {
		// It knows its window; our backoff is a guess at it.
		limited := &rateLimitError{URL: "u", RetryAfter: 90 * time.Second}
		if got := retryDelay(limited, pageDelay, 1); got != 90*time.Second {
			t.Fatalf("retryDelay = %s, want the venue's 90s", got)
		}
	})

	t.Run("a Retry-After shorter than the backoff does not shorten it", func(t *testing.T) {
		limited := &rateLimitError{URL: "u", RetryAfter: time.Second}
		if got := retryDelay(limited, pageDelay, 2); got != rateLimitBackoff(2) {
			t.Fatalf("retryDelay = %s, want the backoff %s", got, rateLimitBackoff(2))
		}
	})
}

func TestRateLimitErrorIsRecognisedThroughWrapping(t *testing.T) {
	err := fmt.Errorf("hyperliquid funding history BTC: %w",
		&rateLimitError{URL: "https://api.hyperliquid.xyz/info", Body: "null"})
	if !errors.Is(err, errRateLimited) {
		t.Fatal("a wrapped rate limit is not recognised; every retry decision reads it through wrapping")
	}
	if errors.Is(err, ErrNotListed) {
		t.Fatal("a rate limit read as 'market not listed' would end the series silently and report no rows")
	}
}

func TestParseRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"30", 30 * time.Second},
		{" 5 ", 5 * time.Second},
		{"", 0},
		{"0", 0},
		{"-1", 0},
		// The HTTP-date form is deliberately not parsed: comparing it against
		// our clock measures venue clock skew, which this project has already
		// measured at 80ms on Binance (CLAUDE.md rule 13).
		{"Fri, 04 Sep 2026 12:00:00 GMT", 0},
	} {
		if got := parseRetryAfter(tc.header); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.header, got, tc.want)
		}
	}
}
