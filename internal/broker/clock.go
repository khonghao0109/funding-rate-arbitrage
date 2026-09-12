package broker

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The server-time endpoints, quoted from the documentation.
//
//   - USDⓈ-M futures: GET /fapi/v1/time, weight 1, {"serverTime": …}
//     https://developers.binance.com/docs/derivatives/usds-margined-futures/market-data/rest-api/Check-Server-Time
//   - Spot: GET /api/v3/time, IP weight 1, {"serverTime": …}
//     https://developers.binance.com/docs/binance-spot-api-docs/rest-api/general-endpoints
const (
	BinanceFuturesTimePath = "/fapi/v1/time"
	BinanceSpotTimePath    = "/api/v3/time"

	// Weights for those two calls, from the same pages.
	BinanceTimeWeight = 1
)

// DefaultClockSyncEvery is how often the skew is re-measured.
//
// Not once at start-up: this process is expected to run for weeks on a laptop
// that suspends, and a suspend returns with the wall clock moved and the skew
// measurement not. Thirty minutes costs one weight-1 call per half hour against
// a 2400/minute budget — nothing — and bounds how far a drifting clock can get
// before it is noticed.
const DefaultClockSyncEvery = 30 * time.Minute

// ErrClockSkew means the local clock is further from the venue's than the
// recvWindow this client declares.
//
// Why refuse rather than lean on the correction: the correction is a
// measurement with an age. When |skew| is small the correction is a nicety and
// a stale one costs nothing; when |skew| exceeds the whole window, the
// correction is the ONLY thing making the request acceptable, and the first
// second of drift after it turns every signed call into the venue's -1021
// ("Timestamp for this request is outside of the recvWindow"), which names our
// clock nowhere. A local refusal that prints both numbers in milliseconds is a
// diagnosis; -1021 at 3am is a mystery.
// https://developers.binance.com/docs/binance-spot-api-docs/errors
var ErrClockSkew = errors.New("broker: local clock too far from the venue's")

type serverTimeResponse struct {
	ServerTimeMs int64 `json:"serverTime"`
}

// SyncClock measures the offset between the venue's clock and ours and stores
// it. It is safe to call concurrently and costs one weight-1 request.
//
// The measurement is taken against the MIDPOINT of our own request window
// rather than against the instant the answer was decoded: the venue stamped its
// reply somewhere inside the round trip, so charging the whole trip to the
// venue's clock would report a skew that is really our own latency. On a
// 200 ms round trip that is a 100 ms error in a number compared against a
// 5000 ms window — small, but it is the kind of error that is only ever
// discovered when the window is tightened.
func (c *Client) SyncClock(ctx context.Context) (int64, error) {
	if c.timePath == "" {
		return 0, errors.New("broker: no server-time endpoint configured — set Config.TimePath")
	}
	ep := Endpoint{Path: c.timePath, WeightIP: BinanceTimeWeight}
	var resp serverTimeResponse
	sentAt := c.now()
	if err := c.GetPublic(ctx, ep, nil, &resp); err != nil {
		return 0, fmt.Errorf("broker: could not read the venue clock at %s: %w", c.timePath, err)
	}
	answeredAt := c.now()
	if resp.ServerTimeMs <= 0 {
		return 0, fmt.Errorf("broker: %s answered with no serverTime", c.timePath)
	}

	midpointMs := sentAt.UnixMilli() + (answeredAt.UnixMilli()-sentAt.UnixMilli())/2
	skewMs := resp.ServerTimeMs - midpointMs

	c.clockMu.Lock()
	c.clockSkewMs, c.clockMeasuredAt = skewMs, answeredAt
	c.clockMu.Unlock()
	return skewMs, nil
}

// ClockSkewMs is the last measured offset, venue minus local, in milliseconds.
// Positive means the venue's clock is ahead of ours.
func (c *Client) ClockSkewMs() int64 {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	return c.clockSkewMs
}

// ClockMeasuredAt is when the offset above was taken; the zero time means never.
func (c *Client) ClockMeasuredAt() time.Time {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	return c.clockMeasuredAt
}

// SetClockSyncEvery changes the re-measurement interval. A non-positive value
// restores the default.
func (c *Client) SetClockSyncEvery(every time.Duration) {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	if every <= 0 {
		every = DefaultClockSyncEvery
	}
	c.clockSyncEvery = every
}

// ensureClock measures the skew if it has never been measured or has gone
// stale, then refuses the call outright if the result is wider than the window
// this client declares.
func (c *Client) ensureClock(ctx context.Context) error {
	c.clockMu.Lock()
	measuredAt, every := c.clockMeasuredAt, c.clockSyncEvery
	c.clockMu.Unlock()

	if measuredAt.IsZero() || c.now().Sub(measuredAt) >= every {
		if _, err := c.SyncClock(ctx); err != nil {
			return err
		}
	}

	skewMs := c.ClockSkewMs()
	if abs64(skewMs) >= c.recvWindowMs {
		return fmt.Errorf("%w: measured %d ms against a recv_window_ms of %d — fix the machine clock (or raise recv_window_ms, max %d) rather than let the venue answer -1021",
			ErrClockSkew, skewMs, c.recvWindowMs, MaxRecvWindowMs)
	}
	return nil
}

// timestampMs is the value sent as `timestamp`: our clock, moved into the
// venue's frame by the last measurement.
func (c *Client) timestampMs() int64 {
	return c.now().UnixMilli() + c.ClockSkewMs()
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
