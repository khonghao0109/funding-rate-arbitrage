// Package bybit implements broker.Broker against Bybit V5 LINEAR perpetuals on a
// NON-PRODUCTION host: the testnet or the demo-trading service (PLAN 4.5i).
//
// It is built on broker.Client with broker.SchemeBybitV5Header, so it inherits
// every wall that package already proves: the per-scheme host allow-list (no
// production host, no flag), https only, no redirect followed, every request
// pinned to its own host, the Secret that no fmt verb prints, the clock refusal,
// and the scrubbed error text. Nothing here opens its own HTTP client.
//
// # One market: USDT-settled linear perpetuals
//
// broker.MarketFuturesUSDM is read here as "category=linear, settled in USDT" —
// Bybit's name for the same instrument class. USDC-settled perps and dated
// futures are refused when their rules are read (FetchInstrument, market.go);
// PlaceOrder does not re-check the settle coin, so a caller must size from
// FetchInstrument's rules, as internal/execution does. Spot is refused
// outright: the pillar this serves is perp against perp.
//
// # What a Bybit answer does NOT say, and what that costs
//
//   - An order-create answer is an ACKNOWLEDGEMENT with two ids and no status:
//     "The acknowledgement of an place order request indicates that the request
//     was sucessfully accepted. This request is asynchronous". PlaceOrder
//     therefore reports status NEW with nothing filled, exactly like a Binance
//     futures MARKET ack, and the fill must be read back (GetOrder) — the
//     4.4b defect of believing an ack must not be repeated here.
//   - Cancel is asynchronous too, so CancelOrder polls the order until it is in
//     a terminal state, re-sends the cancel to an order it can see is still
//     live, and otherwise returns ErrCancelNotConfirmed — never a live order as
//     if it were final.
//   - Two empty order lists are ErrOrderNotVisible, AMBIGUOUS — never
//     broker.ErrOrderNotFound, which callers read as "nothing filled" and "safe
//     to resend".
//   - Venue refusals arrive as HTTP 200 with a non-zero retCode, not as a 4xx.
//     internal/execution's definiteRejection counts only a 4xx as a definite
//     refusal, so today every Bybit refusal reads as AMBIGUOUS there and is
//     resolved by asking for the order. That is the safe direction and it is a
//     NAMED DEBT for the cross-venue execution step, not an oversight.
//
// Every path, parameter and code is quoted in endpoints.go / errors.go with the
// page it came from, read 2026-09-17 from the docs repository's master branch
// (https://github.com/bybit-exchange/docs). Its stale `main` branch differs
// materially (ban duration, the meaning of 10009, deprecations) and must not be
// used as a source.
package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"futures-arbitrage-scanner/internal/broker"
)

// Mode is which non-production Bybit service a key belongs to. A key works on
// exactly one: "Check whether the key and domain are matched, there are 4 env:
// mainnet, testnet, mainnet-demo, testnet-demo" (error 10003).
type Mode string

const (
	ModeTestnet Mode = "testnet"
	ModeDemo    Mode = "demo"
)

// ParseMode reads BYBIT_MODE. Anything else is refused, never defaulted:
// defaulting a typo to testnet sends a demo key to the wrong host and reports
// 10003, which names neither the typo nor the variable.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeTestnet:
		return ModeTestnet, nil
	case ModeDemo:
		return ModeDemo, nil
	}
	return "", fmt.Errorf("bybit: mode %q is neither %q nor %q", s, ModeTestnet, ModeDemo)
}

// BaseURL is the mode's host.
func (m Mode) BaseURL() string {
	if m == ModeDemo {
		return broker.BybitDemoBaseURL
	}
	return broker.BybitTestnetBaseURL
}

// SourceID names this client's books and rules. Distinct from the scanner's
// "bybit_futures", which is PUBLIC MAINNET data: a testnet book is another
// matching engine, and a demo book is the venue's simulation of mainnet, so
// neither may be joined with the corpus under the mainnet id.
func (m Mode) SourceID() string { return "bybit_linear_" + string(m) }

// DefaultConfig fills in host, scheme, clock path and request budget, so a
// caller supplies only the mode and the credential.
func DefaultConfig(mode Mode, creds broker.Credentials) (broker.Config, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return broker.Config{}, err
	}
	return broker.Config{
		BaseURL:           mode.BaseURL(),
		Scheme:            broker.SchemeBybitV5Header,
		Credentials:       creds,
		TimePath:          broker.BybitTimePath,
		WeightLimitPerMin: broker.BybitRequestsPerMin,
	}, nil
}

// Client is broker.Broker for Bybit V5 linear perpetuals.
type Client struct {
	http *broker.Client
	mode Mode
}

var _ broker.Broker = (*Client)(nil)

// New builds a Client. cfg must carry the Bybit scheme; broker.NewClient
// refuses any host outside that scheme's allow-list.
func New(mode Mode, cfg broker.Config) (*Client, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return nil, err
	}
	if cfg.Scheme != broker.SchemeBybitV5Header {
		return nil, fmt.Errorf("bybit: the config does not carry the Bybit V5 signing scheme")
	}
	c, err := broker.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{http: c, mode: mode}, nil
}

// Mode is the service this client talks to.
func (c *Client) Mode() Mode { return c.mode }

// HTTP exposes the signed client, for the clock and budget a diagnostic prints.
func (c *Client) HTTP() *broker.Client { return c.http }

// envelope is the common V5 answer: "retCode … retMsg … result … retExtInfo …
// time Current timestamp (ms)". https://bybit-exchange.github.io/docs/v5/guide
type envelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
	TimeMs  int64           `json:"time"`
}

// callKind tells the error table which call a code arrived on: a code such as
// 110008 means "gone" to a cancel and something else anywhere else.
type callKind int

const (
	callRead callKind = iota
	callPlace
	callCancel
)

func (c *Client) getPublic(ctx context.Context, ep broker.Endpoint, params []broker.Param, into any) error {
	var env envelope
	if err := c.http.GetPublic(ctx, ep, params, &env); err != nil {
		return classifyTransport(err)
	}
	return c.unwrapEnvelope(env, callRead, into)
}

func (c *Client) getSigned(ctx context.Context, ep broker.Endpoint, params []broker.Param, into any) error {
	var env envelope
	if err := c.http.GetSignedV5(ctx, ep, params, &env); err != nil {
		return classifyTransport(err)
	}
	return c.unwrapEnvelope(env, callRead, into)
}

func (c *Client) postSigned(ctx context.Context, ep broker.Endpoint, kind callKind, body any, into any) error {
	var env envelope
	if err := c.http.PostSignedV5JSON(ctx, ep, body, &env); err != nil {
		return classifyTransport(err)
	}
	return c.unwrapEnvelope(env, kind, into)
}

func (c *Client) unwrapEnvelope(env envelope, kind callKind, into any) error {
	if env.RetCode != 0 {
		// retMsg on an HTTP 200 never passed through broker.Client's error-body
		// scrub, so it is scrubbed here before it can reach a message.
		return venueError(env.RetCode, c.http.ScrubVenueText(env.RetMsg), kind)
	}
	if into == nil {
		return nil
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return fmt.Errorf("bybit: retCode 0 with no result")
	}
	if err := json.Unmarshal(env.Result, into); err != nil {
		// Not quoted: the decoder may repeat venue text.
		return fmt.Errorf("bybit: the result does not have the documented shape")
	}
	return nil
}

// parseNumber reads the venue's decimal strings. "" is 0 — the venue's own
// spelling of "no value" for avgPrice and liqPrice — and anything else that
// does not parse is an ERROR, never a zero, because a zero quantity reads as
// "flat" and a zero price as "nothing filled".
func parseNumber(field, s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bybit: field %s is not a number", field)
	}
	return v, nil
}

// parseRequiredNumber is parseNumber for a field whose blank would read as a
// fact — a size of 0 is "flat", a cumExecQty of 0 is "nothing filled". Blank or
// absent (a renamed field decodes as "") is an error.
func parseRequiredNumber(field, s string) (float64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, fmt.Errorf("bybit: field %s is blank or absent — refused rather than read as 0", field)
	}
	return parseNumber(field, s)
}

func parseMs(field, s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bybit: field %s is not a millisecond stamp", field)
	}
	return v, nil
}

// formatNumber renders a float for the wire without exponent form. Callers
// pass the value broker.RoundOrder produced; 12 decimals, then trimmed, so a
// float residue such as 0.30000000000000004 goes out as 0.3 rather than as a
// quantity off the venue's grid. No Bybit step or tick is finer than 1e-12.
func formatNumber(v float64) string {
	s := strconv.FormatFloat(v, 'f', 12, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
