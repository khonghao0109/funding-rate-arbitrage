package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testBudget is a budget on a clock the test owns. Waiting ADVANCES that clock
// instead of sleeping, so a test about a one-minute window runs in microseconds
// and asserts the wait really happened rather than that it was skipped.
func testBudget(t *testing.T, limitPerMin int, startAt time.Time) (*WeightBudget, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	nowMs, sleptMs := &atomic.Int64{}, &atomic.Int64{}
	nowMs.Store(startAt.UnixMilli())
	b := NewWeightBudget(limitPerMin, func() time.Time { return time.UnixMilli(nowMs.Load()) })
	b.wait = func(ctx context.Context, d time.Duration) error {
		if d <= 0 {
			return nil
		}
		sleptMs.Add(d.Milliseconds())
		nowMs.Add(d.Milliseconds())
		return nil
	}
	return b, nowMs, sleptMs
}

// The budget blocks BEFORE the call that would exceed it, and releases when the
// venue's minute rolls over. Blocking after the fact is what earns the 429 the
// whole mechanism exists to avoid.
func TestWeightBudget_BlocksBeforeExceedingTheMinuteAndReleasesOnTheNextWindow(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 20, 0, time.UTC) // 20 s into the minute
	b, _, slept := testBudget(t, 100, start)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := b.Reserve(ctx, 20); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	if got := b.UsedThisWindow(); got != 100 {
		t.Fatalf("used = %d, want the full 100 reserved without waiting", got)
	}
	if slept.Load() != 0 {
		t.Fatalf("waited %d ms before the budget was spent", slept.Load())
	}

	// The 101st unit of weight has to wait for the window to roll: 40 s from
	// 10:00:20 to 10:01:00, not a fixed guess.
	if err := b.Reserve(ctx, 20); err != nil {
		t.Fatalf("reserve past the limit: %v", err)
	}
	if got := slept.Load(); got != 40_000 {
		t.Errorf("waited %d ms, want 40000 — to the next wall-clock minute, where the venue's counter resets", got)
	}
	if got := b.UsedThisWindow(); got != 20 {
		t.Errorf("used = %d after the window rolled, want 20 (the new window holds only this call)", got)
	}
}

// The venue's own header is the authority. Our local tally is a pre-flight
// guess — it cannot see another process on the same IP, and it cannot see a
// weight we mis-declared — so a header that reports MORE wins.
func TestWeightBudget_AdoptsTheVenuesUsedWeightHeader(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	b, _, slept := testBudget(t, 100, start)
	ctx := context.Background()

	if err := b.Reserve(ctx, 5); err != nil {
		t.Fatal(err)
	}
	b.Observe(http.Header{"X-Mbx-Used-Weight-1m": []string{"95"}})
	if got := b.UsedThisWindow(); got != 95 {
		t.Fatalf("used = %d, want the venue's 95 — something else is spending this IP's budget", got)
	}
	// With 95 of 100 gone, a weight-20 call must wait for the next window.
	if err := b.Reserve(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if slept.Load() == 0 {
		t.Error("the header was read and then ignored: the next call did not wait")
	}

	// A header reporting LESS than we counted does not lower the guard: it is
	// a stale reading of a window we have already spent into.
	b.Observe(http.Header{"X-Mbx-Used-Weight-1m": []string{"1"}})
	if got := b.UsedThisWindow(); got != 20 {
		t.Errorf("used = %d, want 20 — a smaller header reading must not erase what we have already spent", got)
	}
}

// 429 backs off in SECONDS, and the delay is the LONGER of the venue's
// Retry-After and the rest of the minute its weight counter is measured over.
//
// The trap is recorded in CLAUDE.md against Hyperliquid and the same rule is
// already in exchanges/funding_history.go: a rate limit is a budget measured
// over a MINUTE, so retrying a few hundred milliseconds later only spends what
// is left of it. Taking the maximum rather than the header alone matters at a
// 429 early in a minute — coming back while the venue's counter is still high
// earns the second 429, and it is the second one that gets an IP banned.
func TestWeightBudget_A429BacksOffBySecondsAndNeverInsideTheSameMinute(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 10, 0, time.UTC) // 50 s of the minute left
	ctx := context.Background()

	cases := []struct {
		name       string
		retryAfter time.Duration
		wantMs     int64
	}{
		{"Retry-After shorter than the window that is still running", 30 * time.Second, 50_000},
		{"Retry-After longer than the window — the venue knows more than we do", 90 * time.Second, 90_000},
		{"no Retry-After at all — the rest of the minute, never a sub-second retry", 0, 50_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, slept := testBudget(t, 10_000, start)
			b.NoteStatus(http.StatusTooManyRequests, tc.retryAfter)
			if err := b.Reserve(ctx, 1); err != nil {
				t.Fatalf("reserve after a 429: %v", err)
			}
			if got := slept.Load(); got != tc.wantMs {
				t.Errorf("waited %d ms, want %d", got, tc.wantMs)
			}
		})
	}
}

// 418 is an IP ban, "from 2 minutes to 3 days" and scaling for repeat
// offenders. It is not a backoff: continuing to send is what lengthens it, so
// the budget stops for good and says so, and nothing retries automatically.
func TestWeightBudget_A418StopsForGoodAndNeverRetries(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	b, _, slept := testBudget(t, 10_000, start)

	b.NoteStatus(http.StatusTeapot, 120*time.Second)

	for i := 0; i < 3; i++ {
		err := b.Reserve(context.Background(), 1)
		if !errors.Is(err, ErrIPBanned) {
			t.Fatalf("reserve %d after a 418 returned %v, want ErrIPBanned", i, err)
		}
	}
	if slept.Load() != 0 {
		t.Errorf("the budget waited %d ms after a 418; an IP ban is not a backoff and waiting it out silently is how it gets extended", slept.Load())
	}
	banned, until := b.Banned()
	if !banned || !until.Equal(start.Add(120*time.Second)) {
		t.Errorf("Banned() = %v, %s; want true and the documented expiry so the operator can be told", banned, until)
	}
}

// A call that costs more than the whole minute can never be made, and waiting
// for a window that will never have room is a hang, not a retry.
func TestWeightBudget_RefusesACallBiggerThanTheWholeBudget(t *testing.T) {
	b, _, _ := testBudget(t, 10, time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC))
	err := b.Reserve(context.Background(), 40)
	if err == nil {
		t.Fatal("a 40-weight call against a 10/minute budget must be refused, not waited on forever")
	}
	if errors.Is(err, ErrIPBanned) {
		t.Errorf("wrong error: %v", err)
	}
}

// A budget nobody looked up is not an unlimited one. This is the same refusal
// internal/fees makes for an unverified fee schedule, and for the same reason.
func TestNewClient_RefusesAWeightBudgetNobodyLookedUp(t *testing.T) {
	_, err := NewClient(Config{
		BaseURL:           BinanceFuturesTestnetBaseURL,
		Credentials:       Credentials{APIKey: NewSecret("k"), APISecret: NewSecret("s")},
		TimePath:          BinanceFuturesTimePath,
		WeightLimitPerMin: 0,
	})
	if err == nil {
		t.Fatal("NewClient accepted a zero weight budget")
	}
}

// The two budgets, pinned so a typo is a test failure rather than a 418. Their
// PROVENANCE differs and the constants' comment says so: spot's 6000 is quoted
// from the docs, futures' 2400 is an unverified conservative default because
// the official page gives no number and points at exchangeInfo instead.
func TestWeightLimits_AreThePinnedFigures(t *testing.T) {
	if BinanceFuturesWeightPerMin != 2400 {
		t.Errorf("futures REQUEST_WEIGHT = %d, want the pinned conservative 2400", BinanceFuturesWeightPerMin)
	}
	if BinanceSpotWeightPerMin != 6000 {
		t.Errorf("spot REQUEST_WEIGHT = %d, the docs show 6000/minute in the exchangeInfo example", BinanceSpotWeightPerMin)
	}
}

// The wiring, against a fake venue: the client must read the weight header off
// every answer INCLUDING the failures, treat 418 as terminal, and surface it as
// ErrIPBanned so a caller stops on errors.Is rather than on remembering that
// 418 means teapot.
func TestClient_ReadsTheWeightHeaderAndStopsDeadOnA418(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == BinanceFuturesTimePath {
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
			return
		}
		requests.Add(1)
		w.Header().Set("X-MBX-USED-WEIGHT-1M", "137")
		w.Header().Set("X-MBX-ORDER-COUNT-10S", "0")
		code := int(status.Load())
		if code == http.StatusTeapot {
			w.Header().Set("Retry-After", "120")
		}
		w.WriteHeader(code)
		fmt.Fprint(w, `[{"asset":"USDT","balance":"1000"}]`)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := leakTestClient(t, &pinnedTransport{to: u, serverTimeMs: time.Now().UnixMilli()})

	var into []map[string]any
	if err := client.GetSigned(context.Background(), FuturesAccountBalance, nil, &into); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := client.Budget().UsedThisWindow(); got != 137 {
		t.Errorf("used weight = %d, want the venue's 137 from X-MBX-USED-WEIGHT-1M", got)
	}
	if report := client.Budget().ReportVI(); !strings.Contains(report, "137") {
		t.Errorf("the report must quote the venue's own header: %q", report)
	}

	status.Store(http.StatusTeapot)
	err = client.GetSigned(context.Background(), FuturesAccountBalance, nil, &into)
	if !errors.Is(err, ErrIPBanned) {
		t.Fatalf("a 418 returned %v, want ErrIPBanned", err)
	}
	banned := requests.Load()

	// Every later call must be refused WITHOUT reaching the venue: continuing
	// to send is what lengthens the ban.
	for i := 0; i < 3; i++ {
		if err := client.GetSigned(context.Background(), FuturesAccountBalance, nil, &into); !errors.Is(err, ErrIPBanned) {
			t.Fatalf("call %d after the ban returned %v, want ErrIPBanned", i, err)
		}
	}
	if got := requests.Load(); got != banned {
		t.Errorf("%d more request(s) went to the venue after the 418; a banned client must not send at all", got-banned)
	}
}
