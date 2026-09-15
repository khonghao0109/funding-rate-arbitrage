package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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

	// WeightLimitPerMin is this venue's documented REQUEST_WEIGHT budget —
	// BinanceFuturesWeightPerMin or BinanceSpotWeightPerMin. There is no
	// default: a budget nobody looked up is not an unlimited one, and the cost
	// of assuming otherwise is a 429 followed by an IP ban that lengthens for
	// repeat offenders.
	WeightLimitPerMin int

	// HTTPClient is injectable so tests run against httptest and never open a
	// socket to a venue. Nil gets a client with a bounded timeout. Either way
	// the Client works on a COPY (the caller's is not modified) whose
	// CheckRedirect refuses every redirect and whose transport checks every
	// request against BaseURL's host (redirect.go). What a copy cannot take away:
	// the caller's own Transport runs BELOW that check and could route a request
	// anywhere, so injecting one is trusting it with the credential. Today only
	// tests and cmd/brokercheck's CaptureTransport (over http.DefaultTransport)
	// do; narrowing this hook is recorded as a 4.6 prerequisite (PLAN 4.5b).
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
	budget       *WeightBudget

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
	if cfg.WeightLimitPerMin <= 0 {
		return nil, fmt.Errorf("broker: weight_limit_per_min must be the venue's documented REQUEST_WEIGHT budget (futures %d, spot %d) — a budget nobody looked up is not an unlimited one",
			BinanceFuturesWeightPerMin, BinanceSpotWeightPerMin)
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		// checkTestnetBaseURL parsed the same string a moment ago.
		return nil, errors.New("broker: base URL does not parse — not quoted, since it may carry userinfo")
	}
	httpClient := guardedHTTPClient(cfg.HTTPClient, base.Host)
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
		budget:         NewWeightBudget(cfg.WeightLimitPerMin, now),
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

// Budget exposes the weight budget for a report. It is the same instance every
// call reserves against.
func (c *Client) Budget() *WeightBudget { return c.budget }

// GetPublic calls an unsigned endpoint. It sends no credential — not even the
// API key header — because an endpoint that does not need identifying should
// not be handed an identity.
func (c *Client) GetPublic(ctx context.Context, ep Endpoint, params []Param, into any) error {
	return c.do(ctx, http.MethodGet, ep, QueryString(params), paramsInQuery, false, into)
}

// GetSigned calls a SIGNED (USER_DATA) endpoint.
//
// timestamp and recvWindow are appended here, last before the signature, so a
// caller cannot forget them and cannot set them to something the client did not
// measure.
func (c *Client) GetSigned(ctx context.Context, ep Endpoint, params []Param, into any) error {
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
	return c.do(ctx, http.MethodGet, ep, SignedQuery(c.creds.APISecret, signed), paramsInQuery, true, into)
}

// PostSigned and DeleteSigned are the step-4.2 write verbs.
//
// WHERE the parameters travel is not a style choice — it is what each endpoint
// documents, and Binance is not uniform about it:
//
//	POST   /fapi/v1/order   "Parameter Location: Request body"
//	POST   /api/v3/order    "Parameter Location: Request Body"
//	DELETE /api/v3/order    "Parameter Location: Query String"
//	DELETE /fapi/v1/order   query string, as for the GET
//
// The signature covers the same bytes either way: "totalParams is defined as
// the query string concatenated with the request body", so when every parameter
// is in the body, totalParams IS the body, and the signature is appended to
// whichever of the two carries them.
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/endpoint-security-type
func (c *Client) PostSigned(ctx context.Context, ep Endpoint, params []Param, into any) error {
	return c.writeSigned(ctx, http.MethodPost, ep, params, paramsInBody, into)
}

// DeleteSigned sends the parameters on the query string, as both venues'
// cancel endpoints document.
func (c *Client) DeleteSigned(ctx context.Context, ep Endpoint, params []Param, into any) error {
	return c.writeSigned(ctx, http.MethodDelete, ep, params, paramsInQuery, into)
}

func (c *Client) writeSigned(ctx context.Context, method string, ep Endpoint, params []Param, where paramPlacement, into any) error {
	if err := c.ensureClock(ctx); err != nil {
		return err
	}
	signed := make([]Param, 0, len(params)+2)
	signed = append(signed, params...)
	signed = append(signed,
		Param{"recvWindow", fmt.Sprintf("%d", c.recvWindowMs)},
		Param{"timestamp", fmt.Sprintf("%d", c.timestampMs())},
	)
	return c.do(ctx, method, ep, SignedQuery(c.creds.APISecret, signed), where, true, into)
}

// paramPlacement is which half of the request carries the parameters — and so
// which half carries the signature.
type paramPlacement int

const (
	paramsInQuery paramPlacement = iota
	paramsInBody
)

// How much of an answer is read, and how much of a failed one is quoted.
//
// These are two different questions and were one constant until the step-4.1
// acceptance run of 2026-09-13 found it. A SUCCESS body is parsed, so its
// ceiling only has to be larger than any answer the venue legitimately sends —
// a spot /api/v3/account lists hundreds of assets and an exchangeInfo is
// megabytes, both far past the 4 KiB that was in force. An ERROR body is
// venue-controlled text that lands in an error message, a log and a report, so
// its ceiling stays small however large the success one grows.
const (
	maxResponseBytes  = 16 << 20 // 16 MiB: past any documented payload, still bounded
	maxErrorBodyBytes = 4096
)

func (c *Client) do(ctx context.Context, method string, ep Endpoint, query string, where paramPlacement, signed bool, into any) error {
	// Reserved BEFORE the request. A limiter that notices afterwards has
	// already earned the 429 it exists to avoid.
	if err := c.budget.Reserve(ctx, ep.WeightIP); err != nil {
		return fmt.Errorf("%s: %w", ep.Path, err)
	}
	full := c.baseURL + ep.Path
	var reqBody io.Reader
	if where == paramsInBody {
		// The signed parameters travel in the body; the URL keeps none of
		// them, which incidentally puts the signature out of reach of every
		// URL-printing error path in Go.
		reqBody = strings.NewReader(query)
	} else if query != "" {
		full += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reqBody)
	if err != nil {
		// NewRequest embeds the URL in its error; redact before it escapes.
		return fmt.Errorf("%s: build request: %s", redactURL(full), c.scrub(err.Error()))
	}
	if where == paramsInBody {
		// The content type Binance documents for body parameters.
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		// The two walls of redirect.go keep their sentinels, and nothing else
		// of the *url.Error: its URL is the whole request, or the venue's
		// Location, and either may carry the signature.
		if errors.Is(err, ErrRedirectAttempted) {
			if resp != nil {
				c.budget.Observe(resp.Header)
			}
			return c.redirectRefused(full, resp, false)
		}
		if errors.Is(err, ErrHostNotPinned) {
			return fmt.Errorf("%w: %s is not %s — nothing was sent", ErrHostNotPinned, redactURL(full), c.baseURL)
		}
		// *url.Error prints the WHOLE URL, signature included. This is the
		// error path the leak test exists for.
		return fmt.Errorf("%s: %s", redactURL(full), c.scrub(errWithoutURL(err, full)))
	}
	defer resp.Body.Close()

	// The venue's own account of what this IP has spent, read on EVERY answer
	// including the failures — a 429 is exactly when the number matters.
	c.budget.Observe(resp.Header)

	// Read one byte past the ceiling so an oversized answer can be NAMED
	// rather than handed to the decoder as truncated JSON.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if resp.StatusCode != http.StatusOK {
		retryAfter := parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
		c.budget.NoteStatus(resp.StatusCode, retryAfter)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			// Every 3xx arrives here: hostPinTransport took its Location away,
			// so http.Client had nothing to follow. It is not the API's answer,
			// and its body is not the venue's verdict either — a Binance-shaped
			// JSON body on a 302 must not read as "order not found".
			return c.redirectRefused(full, resp, true)
		}
		// The ERROR body stays short: it is venue-controlled text on its way
		// into an error message, a log and a report. scrubTo cuts it on the
		// body's own positions, so a key straddling the cut is hidden whole
		// and a long run that scrubs short cannot pull anything across.
		shown := c.scrubTo(strings.TrimSpace(string(body)), maxErrorBodyBytes)
		err := &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        redactURL(full),
			Body:       shown,
			RetryAfter: retryAfter,
		}
		if resp.StatusCode == http.StatusTeapot {
			// Surfaced as ErrIPBanned so a caller stops on errors.Is rather
			// than on a status code it had to remember means "teapot".
			return fmt.Errorf("%w: %s", ErrIPBanned, err)
		}
		return err
	}
	if into == nil {
		return nil
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("%s: câu trả lời quá lớn (hơn %d byte) — không giải mã, vì cắt ngắn JSON rồi báo lỗi giải mã sẽ đổ tội cho sàn",
			redactURL(full), maxResponseBytes)
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
	return scrubCut(s, len(s), c.creds.APISecret.Expose(), c.creds.APIKey.Expose())
}

// scrubTo is scrub for venue-controlled text that is also cut short: at most
// maxBytes of s are shown, measured on s itself.
func (c *Client) scrubTo(s string, maxBytes int) string {
	return scrubCut(s, maxBytes, c.creds.APISecret.Expose(), c.creds.APIKey.Expose())
}

// scrubHexSignature removes the value of any `signature=` parameter that
// survived into a string. The signature is derived from the secret and a
// timestamp; it is not reusable, but it identifies the account and belongs in
// no log.
func scrubHexSignature(s string) string {
	return scrubCut(s, len(s))
}

const signatureMarker = "signature="

// scrubCut shows at most the first maxBytes of s, with every occurrence of each
// needle and the hex value of every `signature=` parameter replaced by Redacted.
//
// Two properties, both found missing by review on 2026-09-15 and both cheap to
// lose again:
//
//   - The cut is measured on the ORIGINAL string. Replacing first and cutting
//     after lets a long replaced run shrink and pull whatever followed it —
//     a key — across the cut; cutting first and replacing after leaves the
//     prefix of a key that straddled the cut, which no longer matches. Here a
//     span that starts before the cut is hidden whole, and nothing that starts
//     after it is looked at.
//   - It is LINEAR in maxBytes plus the longest needle, whatever s holds. The
//     text is venue-controlled and can be megabytes; a scrub that rescans after
//     each replacement held a request for seconds past its deadline on a body
//     of repeated `signature=`.
func scrubCut(s string, maxBytes int, needles ...string) string {
	if maxBytes < 0 || maxBytes > len(s) {
		maxBytes = len(s)
	}
	// Look past the cut by the longest thing that could straddle it, so a span
	// starting before the cut is found whole.
	margin := len(signatureMarker)
	for _, n := range needles {
		margin = max(margin, len(n))
	}
	window := s[:min(len(s), maxBytes+margin)]

	type span struct{ start, end int }
	var spans []span
	for _, needle := range needles {
		if needle == "" {
			continue
		}
		for from := 0; from < len(window); {
			i := strings.Index(window[from:], needle)
			if i < 0 {
				break
			}
			start := from + i
			spans = append(spans, span{start, start + len(needle)})
			// Resume one byte on, not past the match: a needle whose first
			// byte recurs inside it can be echoed overlapping itself, and the
			// second copy must be found too.
			from = start + 1
		}
	}
	for from := 0; from < len(window); {
		i := strings.Index(window[from:], signatureMarker)
		if i < 0 {
			break
		}
		start := from + i + len(signatureMarker)
		end := start
		for end < len(window) && isHexDigit(window[end]) {
			end++
		}
		if end > start {
			spans = append(spans, span{start, end})
		}
		from = end
	}
	if len(spans) == 0 {
		return s[:maxBytes]
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := make([]span, 0, len(spans))
	for _, sp := range spans {
		if n := len(merged); n > 0 && sp.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, sp.end)
			continue
		}
		merged = append(merged, sp)
	}

	var b strings.Builder
	b.Grow(maxBytes)
	pos := 0
	for _, sp := range merged {
		if sp.start >= maxBytes {
			break
		}
		b.WriteString(s[pos:sp.start])
		b.WriteString(Redacted)
		pos = sp.end
	}
	if pos < maxBytes {
		b.WriteString(s[pos:maxBytes])
	}
	return b.String()
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
