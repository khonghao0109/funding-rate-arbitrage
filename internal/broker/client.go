package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config describes one venue account's signed REST access.
//
// Every field that carries a unit says so in its name (CLAUDE.md rule 4).
type Config struct {
	// BaseURL must be a TESTNET host — see hosts.go. There is no flag to
	// widen it at this step.
	BaseURL string

	Credentials Credentials

	// RecvWindowMs is how long after `timestamp` the venue will still accept
	// the request. Binance defaults it to 5000 ms when absent and caps it at
	// 60000; this package always SENDS it rather than relying on the default,
	// so the journal of a request says what window it was judged under.
	// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/endpoint-security-type
	RecvWindowMs int64

	// TimePath is the venue's server-time endpoint, used to measure the clock
	// skew every signed request is corrected by — BinanceFuturesTimePath or
	// BinanceSpotTimePath. Empty means no signed call can be made, because a
	// timestamp nobody checked is how -1021 arrives.
	TimePath string

	// ClockSyncEvery is how often the skew is re-measured; 0 takes
	// DefaultClockSyncEvery.
	ClockSyncEvery time.Duration

	// HTTPClient is injectable so tests run against httptest and never open a
	// socket to a venue. Nil gets a client with a bounded timeout.
	HTTPClient *http.Client

	// Now is the local clock, injectable so the skew tests can move it. Nil
	// takes time.Now.
	Now func() time.Time

	// UserAgentVI identifies this tool in the venue's logs. Optional.
	UserAgentVI string
}

// RecvWindow bounds, from the documentation cited on Config.RecvWindowMs.
const (
	DefaultRecvWindowMs int64 = 5000
	MaxRecvWindowMs     int64 = 60000
)

// Client is signed REST access to ONE venue base URL.
//
// It places no orders. Step 4.1 is the transport and nothing else: the order
// interface is 4.2, and every method here is a GET.
type Client struct {
	baseURL string
	creds   Credentials

	recvWindowMs int64
	http         *http.Client
	userAgent    string
	timePath     string
	now          func() time.Time

	// The clock measurement, guarded because one client is shared by every
	// caller and the skew is read on every signed request.
	clockMu         sync.Mutex
	clockSkewMs     int64
	clockMeasuredAt time.Time
	clockSyncEvery  time.Duration
}

// NewClient validates the configuration and refuses anything it cannot make
// safe. It opens no connection.
func NewClient(cfg Config) (*Client, error) {
	if err := checkTestnetBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.Credentials.APIKey.Empty() || cfg.Credentials.APISecret.Empty() {
		return nil, fmt.Errorf("%w: NewClient needs both halves of the pair", ErrNoCredentials)
	}
	recvWindowMs := cfg.RecvWindowMs
	if recvWindowMs == 0 {
		recvWindowMs = DefaultRecvWindowMs
	}
	if recvWindowMs < 0 || recvWindowMs > MaxRecvWindowMs {
		return nil, fmt.Errorf("broker: recv_window_ms %d is outside the documented range (1..%d)", recvWindowMs, MaxRecvWindowMs)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	syncEvery := cfg.ClockSyncEvery
	if syncEvery <= 0 {
		syncEvery = DefaultClockSyncEvery
	}
	return &Client{
		baseURL:        strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		creds:          cfg.Credentials,
		recvWindowMs:   recvWindowMs,
		http:           httpClient,
		userAgent:      cfg.UserAgentVI,
		timePath:       cfg.TimePath,
		now:            now,
		clockSyncEvery: syncEvery,
	}, nil
}

// BaseURL is the host this client is pinned to, for a banner. It carries no
// query and therefore no signature.
func (c *Client) BaseURL() string { return c.baseURL }

// RecvWindowMs is what every signed request declares.
func (c *Client) RecvWindowMs() int64 { return c.recvWindowMs }

// HTTPError is a non-2xx answer from the venue.
//
// It carries the status, the venue's own message and the PATH — never the
// query, because the signature is a query parameter. Body is scrubbed of both
// the secret and the signature before it is stored, so that even a venue that
// echoed the request back could not put them here.
type HTTPError struct {
	StatusCode int
	URL        string // scheme://host/path, redacted
	Body       string // scrubbed
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s: HTTP %d (retry after %s): %s", e.URL, e.StatusCode, e.RetryAfter, e.Body)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.URL, e.StatusCode, e.Body)
}

// GetPublic calls an unsigned endpoint. It sends no credential — not even the
// API key header — because an endpoint that does not need identifying should
// not be handed an identity.
func (c *Client) GetPublic(ctx context.Context, path string, params []Param, into any) error {
	return c.do(ctx, path, QueryString(params), false, into)
}

// GetSigned calls a SIGNED (USER_DATA) endpoint.
//
// timestamp and recvWindow are appended here, last before the signature, so a
// caller cannot forget them and cannot set them to something the client did not
// measure.
func (c *Client) GetSigned(ctx context.Context, path string, params []Param, into any) error {
	// The clock first, always: a signed request built on an unmeasured or
	// stale skew is one the venue answers with -1021, and that answer names
	// our clock nowhere.
	if err := c.ensureClock(ctx); err != nil {
		return err
	}
	signed := make([]Param, 0, len(params)+2)
	signed = append(signed, params...)
	signed = append(signed,
		Param{"recvWindow", fmt.Sprintf("%d", c.recvWindowMs)},
		Param{"timestamp", fmt.Sprintf("%d", c.timestampMs())},
	)
	return c.do(ctx, path, SignedQuery(c.creds.APISecret, signed), true, into)
}

func (c *Client) do(ctx context.Context, path, query string, signed bool, into any) error {
	full := c.baseURL + path
	if query != "" {
		full += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		// NewRequest embeds the URL in its error; redact before it escapes.
		return fmt.Errorf("%s: build request: %s", redactURL(full), c.scrub(err.Error()))
	}
	if signed {
		// "API-keys are passed into the Rest API via the X-MBX-APIKEY header."
		// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/endpoint-security-type
		req.Header.Set("X-MBX-APIKEY", c.creds.APIKey.Expose())
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error prints the WHOLE URL, signature included. This is the
		// error path the leak test exists for.
		return fmt.Errorf("%s: %s", redactURL(full), c.scrub(errWithoutURL(err, full)))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        redactURL(full),
			Body:       c.scrub(strings.TrimSpace(string(body))),
			RetryAfter: parseRetryAfterSeconds(resp.Header.Get("Retry-After")),
		}
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s: decode: %s", redactURL(full), c.scrub(err.Error()))
	}
	return nil
}

// scrub removes anything that must never appear in a message, whatever produced
// it: the secret itself, the API key, and any signature this client generated.
//
// It is belt AND braces on purpose. redactURL already strips the query, but
// scrub covers the cases redactURL cannot see — a venue that echoed a parameter
// back, a decoder quoting the input, a wrapped error built somewhere else.
func (c *Client) scrub(s string) string {
	for _, v := range []string{c.creds.APISecret.Expose(), c.creds.APIKey.Expose()} {
		if v != "" {
			s = strings.ReplaceAll(s, v, Redacted)
		}
	}
	return scrubHexSignature(s)
}

// scrubHexSignature removes the value of any `signature=` parameter that
// survived into a string. The signature is derived from the secret and a
// timestamp; it is not reusable, but it identifies the account and belongs in
// no log.
func scrubHexSignature(s string) string {
	const marker = "signature="
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			return s
		}
		j := i + len(marker)
		for j < len(s) && isHexDigit(s[j]) {
			j++
		}
		if j == i+len(marker) {
			// "signature=" with nothing after it; leave it and stop, or this
			// loop never advances.
			return s[:j] + Redacted + scrubHexSignature(s[j:])
		}
		s = s[:i+len(marker)] + Redacted + s[j:]
	}
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// errWithoutURL strips the URL Go's http stack embeds in a transport error, so
// the caller's own redacted URL is the only one in the message.
func errWithoutURL(err error, full string) string {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, full, redactURL(full))
	// *url.Error renders as `Get "<url>": <cause>`; the quoted form is the one
	// that survives the replacement above only if the URL matched exactly.
	return msg
}

// parseRetryAfterSeconds reads the header's delay-seconds form.
//
// "A Retry-After header is sent with a 418 or 429 responses and will give the
// number of seconds required to wait, in the case of a 429, to prevent a ban."
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/limits
//
// The HTTP-date form is deliberately not parsed, for the same reason
// exchanges/instruments.go gives: comparing it needs the venue's clock, and
// this project has measured venue clocks running ahead of ours.
func parseRetryAfterSeconds(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	var seconds int
	if _, err := fmt.Sscanf(header, "%d", &seconds); err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
