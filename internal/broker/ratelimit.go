package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The per-minute IP request-weight budgets, MEASURED from each venue's own
// exchangeInfo on 2026-09-13 during the step-4.1 acceptance run.
//
// Both venues answer the same figure, and the documented way to learn it is to
// ask: the USDⓈ-M general-info page states no number at all and says instead
// that "the /fapi/v1/exchangeInfo rateLimits array contains objects related to
// the exchange's RAW_REQUEST, REQUEST_WEIGHT, and ORDER rate limits".
// https://developers.binance.com/docs/derivatives/usds-margined-futures/general-info
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/general-endpoints
//
// What was actually read, HTTP 200 both:
//
//	GET https://demo-fapi.binance.com/fapi/v1/exchangeInfo   (739 symbols)
//	  REQUEST_WEIGHT MINUTE 1 -> 6000
//	  ORDERS MINUTE 1 -> 1200 · ORDERS SECOND 10 -> 300
//	GET https://testnet.binance.vision/api/v3/exchangeInfo   (1363 symbols)
//	  REQUEST_WEIGHT MINUTE 1 -> 6000
//	  ORDERS SECOND 10 -> 50 · ORDERS DAY 1 -> 160000
//	  RAW_REQUESTS MINUTE 5 -> 300000
//
// This CORRECTS the 2400 that shipped on 2026-09-12 as an unverified
// conservative default: the real ceiling on the host this client is restricted
// to is 6000, and 2400 was throttling it to 40% of its budget. Too low is the
// safe direction, which is why shipping it was tolerable; too high is not — it
// is a 429, then an automatic IP ban that "scale[s] in duration for repeat
// offenders, from 2 minutes to 3 days".
//
// THESE ARE TESTNET'S NUMBERS. The client cannot reach any other host (hosts.go),
// so they are the right ones today, but a venue is free to give mainnet a
// different ceiling and step 4.6 must re-read both before it points anywhere
// else. The ORDERS limits above are recorded for step 4.2, which is the first
// step that spends them; nothing enforces them yet.
const (
	BinanceFuturesWeightPerMin = 6000
	BinanceSpotWeightPerMin    = 6000
)

// usedWeightHeaderPrefix is what the venue stamps every answer with:
// "X-MBX-USED-WEIGHT-(intervalNum)(intervalLetter)".
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/general-api-information
const usedWeightHeaderPrefix = "X-Mbx-Used-Weight-"

// orderCountHeaderPrefix is its sibling, "X-MBX-ORDER-COUNT-(intervalNum)
// (intervalLetter)". Nothing here places an order — that is step 4.2 — so it is
// only READ and reported, never budgeted against. It is named now so that the
// step which does place orders finds the header already parsed rather than
// inventing a second convention for it.
const orderCountHeaderPrefix = "X-Mbx-Order-Count-"

// ErrIPBanned is HTTP 418: the venue has auto-banned this IP for continuing to
// send after 429s.
//
// It is terminal on purpose and nothing here clears it. A ban lengthens for
// repeat offenders, so a process that "waits it out" and resumes is the exact
// behaviour that turns two minutes into three days. The operator is told, the
// process stops using this client, and a human decides when to come back.
var ErrIPBanned = errors.New("broker: HTTP 418 — this IP is banned by the venue; do not retry")

// WeightBudget is one IP's request-weight budget over a one-minute window.
//
// Two sources feed it. Our own tally is a PRE-FLIGHT guess — it is what lets a
// call block before it is made — and the venue's X-MBX-USED-WEIGHT header is
// the authority, because it also counts whatever else is using this IP and any
// weight we declared wrongly. The header can only ever raise the figure within
// a window: a lower reading is a stale answer overtaking a newer one, and
// lowering the guard on it is how a budget gets spent twice.
type WeightBudget struct {
	mu sync.Mutex

	limitPerMin int
	now         func() time.Time
	// wait is injectable so the tests exercise a one-minute window without
	// taking a minute.
	wait func(ctx context.Context, d time.Duration) error

	windowStart  time.Time
	usedInWindow int

	// blockedUntil is a 429's backoff; bannedAt/bannedUntil is a 418.
	blockedUntil time.Time

	banned      bool
	bannedUntil time.Time

	// lastUsedHeader and lastOrderCountHeader are for the diagnostic report:
	// the venue's own words about what this IP has spent.
	lastUsedHeader       string
	lastOrderCountHeader string
}

// NewWeightBudget builds a budget. limitPerMin must be positive: see
// NewClient's refusal.
func NewWeightBudget(limitPerMin int, now func() time.Time) *WeightBudget {
	if now == nil {
		now = time.Now
	}
	return &WeightBudget{
		limitPerMin: limitPerMin,
		now:         now,
		wait:        sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Reserve blocks until `weight` fits in the current window, then charges it.
//
// It returns before the request is made, which is the whole point: a limiter
// that notices afterwards has already earned the 429.
func (b *WeightBudget) Reserve(ctx context.Context, weight int) error {
	if weight <= 0 {
		weight = 1
	}
	if weight > b.limitPerMin {
		return fmt.Errorf("broker: a call costing %d weight cannot be made against a budget of %d per minute — no window will ever have room for it",
			weight, b.limitPerMin)
	}
	for {
		b.mu.Lock()
		if b.banned {
			until := b.bannedUntil
			b.mu.Unlock()
			return fmt.Errorf("%w (the venue named an expiry of %s; a human decides whether to come back, because retrying extends the ban)",
				ErrIPBanned, stampOrUnknown(until))
		}
		now := b.now()
		if wait := b.blockedUntil.Sub(now); wait > 0 {
			b.mu.Unlock()
			if err := b.wait(ctx, wait); err != nil {
				return err
			}
			continue
		}
		b.rollWindowLocked(now)
		if b.usedInWindow+weight <= b.limitPerMin {
			b.usedInWindow += weight
			b.mu.Unlock()
			return nil
		}
		// No room left in this minute. The venue's counter resets on the
		// window boundary, so that is what to wait for — not a guessed delay.
		wait := b.windowStart.Add(time.Minute).Sub(now)
		b.mu.Unlock()
		if err := b.wait(ctx, wait); err != nil {
			return err
		}
	}
}

// rollWindowLocked resets the tally when the wall-clock minute changes.
//
// Aligned to the minute because the venue's own counter is: the header is
// "X-MBX-USED-WEIGHT-(intervalNum)(intervalLetter)" over that interval. If the
// alignment were ever wrong, Observe corrects it on the first answer — which is
// why the header is the authority and this is only the guess.
func (b *WeightBudget) rollWindowLocked(now time.Time) {
	window := now.Truncate(time.Minute)
	if !window.Equal(b.windowStart) {
		b.windowStart, b.usedInWindow = window, 0
	}
}

// Observe adopts what the venue said this IP has spent.
func (b *WeightBudget) Observe(h http.Header) {
	used, usedHeader := largestPrefixedValue(h, usedWeightHeaderPrefix)
	_, orderHeader := largestPrefixedValue(h, orderCountHeaderPrefix)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollWindowLocked(b.now())
	if usedHeader != "" {
		b.lastUsedHeader = usedHeader
		if used > b.usedInWindow {
			b.usedInWindow = used
		}
	}
	if orderHeader != "" {
		b.lastOrderCountHeader = orderHeader
	}
}

// NoteStatus records what a non-2xx answer means for the budget.
//
//   - 429: "used when breaking a request rate limit". Back off for the delay
//     the venue named, and when it named none, for the REST OF THE MINUTE its
//     counter is measured over. Never a sub-second retry: the trap is already
//     recorded in CLAUDE.md against Hyperliquid — a rate limit is a budget over
//     a minute, so retrying inside the same window just spends the attempts.
//   - 418: "used when an IP has been auto-banned for continuing to send
//     requests after receiving 429 codes". Terminal.
//
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/limits
func (b *WeightBudget) NoteStatus(statusCode int, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch statusCode {
	case http.StatusTeapot:
		b.banned = true
		if retryAfter > 0 {
			b.bannedUntil = now.Add(retryAfter)
		}
	case http.StatusTooManyRequests:
		floor := now.Truncate(time.Minute).Add(time.Minute) // the rest of this minute
		until := now.Add(retryAfter)
		if until.Before(floor) {
			until = floor
		}
		if until.After(b.blockedUntil) {
			b.blockedUntil = until
		}
	}
}

// ErrIPCoolingDown is a venue's instruction to stop sending from this IP for a
// stated time, enforced locally: Reserve refuses rather than waits.
var ErrIPCoolingDown = errors.New("broker: this IP is in a venue-ordered cool-down; nothing is sent until it ends")

// ipCooldown is a venue-ordered stop for the whole IP: calls are REFUSED until
// it ends, not queued, because every call made inside it extends the block.
type ipCooldown struct {
	mu    sync.Mutex
	now   func() time.Time
	until time.Time
}

// bybitIPCooldown is shared by every Bybit client in the process.
//
// Bybit: HTTP 403 "access too frequent" means "terminate all HTTP sessions and
// wait for at least 10 minutes" (https://bybit-exchange.github.io/docs/v5/rate-limit).
// A resolve loop polling an order inside that window extends the block, and the
// unwind that follows it runs into the same wall.
var bybitIPCooldown = &ipCooldown{now: time.Now}

func (c *ipCooldown) NoteForbidden(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until := c.now().Add(d); until.After(c.until) {
		c.until = until
	}
}

func (c *ipCooldown) Refused() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now().Before(c.until) {
		return fmt.Errorf("%w until %s", ErrIPCoolingDown, stampOrUnknown(c.until))
	}
	return nil
}

// UsedThisWindow is the current tally, venue header included.
func (b *WeightBudget) UsedThisWindow() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollWindowLocked(b.now())
	return b.usedInWindow
}

// LimitPerMin is the budget this client was built with.
func (b *WeightBudget) LimitPerMin() int { return b.limitPerMin }

// Banned reports the 418 state and the expiry the venue named, if any.
func (b *WeightBudget) Banned() (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.banned, b.bannedUntil
}

// ReportVI is the one line the diagnostic command prints: our tally, the
// budget, and the venue's own header so the two can be compared.
func (b *WeightBudget) ReportVI() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollWindowLocked(b.now())
	out := fmt.Sprintf("%d/%d weight trong phút này", b.usedInWindow, b.limitPerMin)
	if b.lastUsedHeader != "" {
		out += " (sàn báo " + b.lastUsedHeader + ")"
	}
	if b.lastOrderCountHeader != "" {
		out += " · order count " + b.lastOrderCountHeader
	}
	return out
}

// largestPrefixedValue reads every header sharing a prefix and returns the
// largest value with a rendering of the header it came from.
//
// The interval is part of the header NAME, so a venue may stamp several at once
// (1M, 1D). Taking the largest is the conservative reading; the name is kept
// so a report can say which window the number describes rather than implying
// they are all minutes.
func largestPrefixedValue(h http.Header, prefix string) (int, string) {
	best, bestName := -1, ""
	for name, values := range h {
		if !strings.HasPrefix(name, prefix) || len(values) == 0 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(values[0]))
		if err != nil {
			continue
		}
		if n > best {
			best, bestName = n, strings.ToUpper(name)+"="+values[0]
		}
	}
	if best < 0 {
		return 0, ""
	}
	return best, bestName
}

func stampOrUnknown(t time.Time) string {
	if t.IsZero() {
		return "không nêu"
	}
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}
