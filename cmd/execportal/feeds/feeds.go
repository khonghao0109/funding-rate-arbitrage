// Package feeds carries the read-only feeds of the project's two other
// processes into the operator portal, so one page can show the scanner and the
// paper ledger beside the order controls (PLAN Q17).
//
// It is a package of its own so that the boundary is the compiler's, not a
// convention: cmd/execportal can reach nothing here but three http handlers, a
// health view and a shutdown hook. No exported function or method returns a
// price, a rate or a paper decision, so there is no call by which a feed could
// reach an order — the operator's eyes are the only join. guard_test.go pins
// the exported surface, and that this package links no internal/ package and
// decodes nothing.
//
// Three properties make it acceptable to run inside the one binary that holds
// credentials, each held by a test:
//
//   - Bytes are relayed and never decoded.
//   - The relay never makes the scanner wait. cmd/scanner writes each
//     broadcast synchronously on its ingestion path, holding one mutex across
//     every client with a 2-second deadline apiece (internal/scanner
//     writeToClients), so a client that stops reading costs the step-3.5 gate
//     up to two seconds PER BROADCAST. The upstream socket is read by a
//     goroutine that never blocks on the browser: when the browser falls
//     behind, the relay is dropped rather than the scanner slowed.
//   - Upstreams are loopback IP literals and every upstream path is fixed. The
//     portal cannot be pointed at another machine, and a browser cannot choose
//     what is fetched upstream — only a validated symbol and one of the page's
//     own day counts are forwarded.
//
// What the handlers do NOT check is who is asking: Host, Origin and
// Sec-Fetch-Site are the portal's walls (cmd/execportal server.go), applied
// before any handler here runs.
package feeds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	scannerSocketPath  = "/ws"
	scannerHistoryPath = "/api/funding/history"
	paperLedgerPath    = "/api/ledger"
	// The cross-venue funding radar (PLAN 4.5i): public market data the scanner
	// already holds, and its episode log.
	scannerCrossRadarPath  = "/api/cross-radar"
	scannerCrossEventsPath = "/api/cross-radar/events"

	// MaxScannerRelays bounds how many scanner clients the portal can add to
	// the gate process, whatever a page or a reload loop does. Every one of
	// them makes the scanner build and send its spread matrices.
	MaxScannerRelays = 3

	// A relay may be opened at most this many times a minute in total: each
	// session makes the gate write meta, funding and depth under its broadcast
	// mutex, and a page stuck in a reconnect loop must not turn that into load.
	maxRelaySessionsPerMin = 20

	// relayQueueFrames is how far a browser may fall behind before its relay is
	// dropped. Measured 2026-09-14 on 13 pairs: 2,994 frames in 41 s, ~73 a
	// second, so 1,024 frames is about fourteen seconds of a stalled tab.
	relayQueueFrames = 1024
	relayQueueBytes  = 32 << 20

	maxUpstreamFrameBytes = 16 << 20
	// A browser has nothing to say to the scanner; anything past a control
	// frame's size ends the relay.
	maxBrowserFrameBytes = 1 << 10
	browserWriteTimeout  = 10 * time.Second
	upstreamDialTimeout  = 5 * time.Second

	feedFetchTimeout = 20 * time.Second
	// A year of settled history across seven venues is ~4 MiB of JSON.
	maxFeedBodyBytes = 16 << 20
	// The history lives in the gate's SQLite file; a chart re-drawn by several
	// tabs asks it once a minute, not once per tab.
	historyTTL = time.Minute
	// The radar page polls every five seconds; several tabs share one answer.
	crossRadarTTL = 3 * time.Second
	// The episode log changes when an episode opens or closes.
	crossEventsTTL = 30 * time.Second
	// cmd/paperledger rebuilds every five minutes by default.
	paperTTL = 30 * time.Second
	// A failed or 5xx answer is shared only briefly, so a recovered upstream is
	// seen at once.
	errorTTL = 5 * time.Second
)

// historyDays are the windows the page offers. Anything else is refused, so
// the cache — and the gate's database — sees at most this many keys a symbol.
var historyDays = map[string]bool{"7": true, "30": true, "90": true, "180": true, "365": true}

// crossEventDays are the event-log windows the page offers.
var crossEventDays = map[string]bool{"1": true, "7": true, "30": true}

var symbolPattern = regexp.MustCompile(`^[A-Z0-9]{5,20}$`)

// Feeds holds the upstream addresses and what the portal has seen of them.
type Feeds struct {
	scannerAddr string
	paperAddr   string
	now         func() time.Time

	dialer      *websocket.Dialer
	client      *http.Client
	maxRelays   int
	queueFrames int
	perMin      int

	mu      sync.Mutex
	relays  map[*relaySession]struct{}
	opened  []time.Time
	closing bool
	scanner health
	paper   health

	history     *cache
	ledger      *cache
	crossRadar  *cache
	crossEvents *cache
}

type health struct {
	sessions      int64
	lastOKAtMs    int64
	lastErrorVI   string
	lastErrorAtMs int64
}

// body is an upstream answer kept as bytes — never decoded.
type body struct {
	status int
	bytes  []byte
}

// relaySession is one browser ↔ scanner pipe.
type relaySession struct {
	cancel context.CancelFunc
	stop   func(code int, reasonVI string)
}

// New builds the feeds. An empty address turns that feed off.
func New(scannerAddr, paperAddr string, now func() time.Time) *Feeds {
	if now == nil {
		now = time.Now
	}
	return &Feeds{
		scannerAddr: scannerAddr,
		paperAddr:   paperAddr,
		now:         now,
		dialer: &websocket.Dialer{
			HandshakeTimeout: upstreamDialTimeout,
			ReadBufferSize:   64 << 10,
			WriteBufferSize:  1 << 10,
			// Loopback only, never through a proxy an environment variable names.
			Proxy: nil,
		},
		client: &http.Client{
			Timeout:   feedFetchTimeout,
			Transport: &http.Transport{Proxy: nil, MaxIdleConns: 4, IdleConnTimeout: time.Minute},
			// A redirect would let an upstream send the portal somewhere else;
			// the broker's own client still has that debt (PLAN 4.5b), this one
			// does not start with it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		maxRelays:   MaxScannerRelays,
		queueFrames: relayQueueFrames,
		perMin:      maxRelaySessionsPerMin,
		relays:      map[*relaySession]struct{}{},
		history:     newCache(now),
		ledger:      newCache(now),
		crossRadar:  newCache(now),
		crossEvents: newCache(now),
	}
}

// CheckAddr accepts host:port with a loopback IP literal, or "" for off.
func CheckAddr(flagName, raw, ownPort string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("-%s %q is not host:port: %v", flagName, raw, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("-%s %q: the host must be a loopback IP literal — the portal reads feeds from this machine only", flagName, raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("-%s %q: port must be a number in 1..65535", flagName, raw)
	}
	if strconv.Itoa(n) == ownPort {
		return "", fmt.Errorf("-%s %q is the portal's own port", flagName, raw)
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(n)), nil
}

// ------------------------------------------------------------------ status

// FeedView is what the portal's status endpoint says about one feed: health,
// never data.
type FeedView struct {
	Addr          string `json:"addr"`
	Enabled       bool   `json:"enabled"`
	SessionsTotal int64  `json:"sessions_total"`
	LastOKAtMs    int64  `json:"last_ok_at_ms"`
	LastErrorVI   string `json:"last_error_vi"`
	LastErrorAtMs int64  `json:"last_error_at_ms"`
	NoteVI        string `json:"note_vi"`
}

// ScannerView adds the relay slots.
type ScannerView struct {
	FeedView
	RelaysActive int `json:"relays_active"`
	RelaysMax    int `json:"relays_max"`
}

// View is both feeds' health.
type View struct {
	Scanner ScannerView `json:"scanner"`
	Paper   FeedView    `json:"paper"`
}

// View reports the feeds' health.
func (f *Feeds) View() View {
	f.mu.Lock()
	defer f.mu.Unlock()
	feed := func(addr string, h health, note string) FeedView {
		return FeedView{Addr: addr, Enabled: addr != "", SessionsTotal: h.sessions, LastOKAtMs: h.lastOKAtMs,
			LastErrorVI: h.lastErrorVI, LastErrorAtMs: h.lastErrorAtMs, NoteVI: note}
	}
	return View{
		Scanner: ScannerView{
			FeedView: feed(f.scannerAddr, f.scanner,
				"chỉ đọc: relay nguyên văn từng frame WS của cmd/scanner, không giải mã, không gửi gì ngược lại; chỉ nối khi tab Scanner đang mở"),
			RelaysActive: len(f.relays),
			RelaysMax:    f.maxRelays,
		},
		Paper: feed(f.paperAddr, f.paper,
			"chỉ đọc: /api/ledger của cmd/paperledger, chuyển nguyên văn; portal không mở database nào"),
	}
}

func (f *Feeds) note(h *health, errVI string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nowMs := f.now().UnixMilli()
	if errVI == "" {
		h.lastOKAtMs = nowMs
		return
	}
	h.lastErrorVI, h.lastErrorAtMs = errVI, nowMs
}

// ------------------------------------------------------------ scanner relay

// admit reserves a relay slot, or says why it cannot.
func (f *Feeds) admit() (*relaySession, context.Context, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing {
		return nil, nil, "portal đang tắt"
	}
	if len(f.relays) >= f.maxRelays {
		return nil, nil, fmt.Sprintf("đã đủ %d phiên relay tới scanner — đóng bớt tab, mỗi phiên là một client thêm vào tiến trình cổng 3.5", f.maxRelays)
	}
	now := f.now()
	recent := f.opened[:0]
	for _, at := range f.opened {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	f.opened = recent
	if len(f.opened) >= f.perMin {
		return nil, nil, fmt.Sprintf("đã mở %d phiên relay trong một phút — chờ rồi thử lại, để một trang nối lại liên tục không thành tải cho scanner", f.perMin)
	}
	f.opened = append(f.opened, now)
	ctx, cancel := context.WithCancel(context.Background())
	s := &relaySession{cancel: cancel}
	f.relays[s] = struct{}{}
	f.scanner.sessions++
	return s, ctx, ""
}

func (f *Feeds) release(s *relaySession) {
	f.mu.Lock()
	delete(f.relays, s)
	f.mu.Unlock()
	s.cancel()
}

// armStop installs the session's stop, or runs it at once if shutdown began
// while the session was still dialing.
func (f *Feeds) armStop(s *relaySession, stop func(int, string)) {
	f.mu.Lock()
	closing := f.closing
	if !closing {
		s.stop = stop
	}
	f.mu.Unlock()
	if closing {
		stop(websocket.CloseGoingAway, "portal đang tắt")
	}
}

// CloseAll ends every relay and refuses new ones. http.Server.Shutdown does not
// wait for hijacked connections, so the portal registers this with
// RegisterOnShutdown.
func (f *Feeds) CloseAll() {
	f.mu.Lock()
	f.closing = true
	sessions := make([]*relaySession, 0, len(f.relays))
	for s := range f.relays {
		sessions = append(sessions, s)
	}
	f.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
		f.mu.Lock()
		stop := s.stop
		f.mu.Unlock()
		if stop != nil {
			stop(websocket.CloseGoingAway, "portal đang tắt")
		}
	}
}

// ScannerSocket upgrades the browser and relays cmd/scanner's /ws to it, one
// way.
func (f *Feeds) ScannerSocket(w http.ResponseWriter, r *http.Request) {
	if f.scannerAddr == "" {
		writeError(w, http.StatusServiceUnavailable, "feed_disabled", "portal chạy với -scanner-addr rỗng — không có nguồn scanner")
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, "websocket_required", "endpoint này chỉ nhận kết nối WebSocket")
		return
	}
	session, ctx, refusedVI := f.admit()
	if refusedVI != "" {
		// A browser cannot read an HTTP body from a refused handshake — it sees
		// close 1006 — so the reason goes where the page can ask for it.
		f.note(&f.scanner, "portal từ chối mở relay: "+refusedVI)
		writeError(w, http.StatusServiceUnavailable, "relay_limit", refusedVI)
		return
	}
	defer f.release(session)

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		ReadBufferSize:   maxBrowserFrameBytes,
		WriteBufferSize:  64 << 10,
		// Origin and Sec-Fetch-Site are the portal's walls, checked before
		// this handler runs; the upgrader's own check would only repeat them.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	browser, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has answered the browser
	}
	defer browser.Close()

	dialCtx, cancelDial := context.WithTimeout(ctx, upstreamDialTimeout)
	upstream, resp, err := f.dialer.DialContext(dialCtx, "ws://"+f.scannerAddr+scannerSocketPath,
		http.Header{"User-Agent": {"execportal-relay (read-only)"}})
	cancelDial()
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		msg := "scanner " + f.scannerAddr + " không nối được: " + err.Error()
		f.note(&f.scanner, msg)
		closeSocket(browser, websocket.CloseTryAgainLater, msg)
		return
	}
	f.note(&f.scanner, "")

	startedAt := f.now()
	frames, reason := f.pump(session, browser, upstream)
	log.Printf("execportal: relay scanner đóng sau %s, %d frame — %s",
		f.now().Sub(startedAt).Round(time.Second), frames, reason)
}

// pump runs one relay until either side ends. It returns how many frames went
// to the browser and why it stopped.
func (f *Feeds) pump(session *relaySession, browser, upstream *websocket.Conn) (int64, string) {
	queue := make(chan []byte, f.queueFrames)
	var queuedBytes, frames atomic.Int64
	stopped := make(chan struct{})
	var once sync.Once
	var why string
	stop := func(code int, reasonVI string) {
		once.Do(func() {
			why = reasonVI
			close(stopped)
			// The scanner first, and at once: telling the browser why can wait
			// up to a second behind a write the stalled browser is not
			// reading, and for that second nothing would drain the scanner.
			upstream.Close()
			closeSocket(browser, code, reasonVI)
			browser.Close()
		})
	}
	f.armStop(session, stop)

	// upstream → queue. The one rule of this function: this loop never waits
	// on the browser. A full queue ends the relay instead.
	go func() {
		upstream.SetReadLimit(maxUpstreamFrameBytes)
		const overrunVI = "trình duyệt đọc không kịp — relay đã ngắt để không làm chậm scanner"
		for {
			kind, data, err := upstream.ReadMessage()
			if err != nil {
				stop(websocket.CloseInternalServerErr, "scanner đã đóng kết nối")
				return
			}
			if kind != websocket.TextMessage {
				continue
			}
			if queuedBytes.Load()+int64(len(data)) > relayQueueBytes {
				stop(websocket.CloseTryAgainLater, overrunVI)
				return
			}
			select {
			case queue <- data:
				queuedBytes.Add(int64(len(data)))
			default:
				stop(websocket.CloseTryAgainLater, overrunVI)
				return
			}
		}
	}()

	// queue → browser.
	go func() {
		for {
			select {
			case <-stopped:
				return
			case data := <-queue:
				queuedBytes.Add(-int64(len(data)))
				_ = browser.SetWriteDeadline(time.Now().Add(browserWriteTimeout))
				if err := browser.WriteMessage(websocket.TextMessage, data); err != nil {
					stop(websocket.CloseInternalServerErr, "ghi sang trình duyệt lỗi")
					return
				}
				frames.Add(1)
			}
		}
	}()

	// The browser's side is read only for its control frames; a data frame is
	// discarded, never forwarded — nothing reaches the scanner from here.
	browser.SetReadLimit(maxBrowserFrameBytes)
	for {
		if _, _, err := browser.ReadMessage(); err != nil {
			stop(websocket.CloseNormalClosure, "trình duyệt đóng")
			break
		}
	}
	<-stopped
	return frames.Load(), why
}

// closeSocket sends a close frame whose reason fits the protocol's 123 bytes,
// cut on a rune boundary so Vietnamese text stays valid UTF-8.
func closeSocket(c *websocket.Conn, code int, reasonVI string) {
	const maxReasonBytes = 120
	if len(reasonVI) > maxReasonBytes {
		cut := maxReasonBytes
		for cut > 0 && !utf8.RuneStart(reasonVI[cut]) {
			cut--
		}
		reasonVI = reasonVI[:cut]
	}
	_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reasonVI), time.Now().Add(time.Second))
}

// ------------------------------------------------------------- HTTP feeds

var errNotPaperLedger = errors.New("tiến trình trả lời không mang X-Execution-Mode: paper — không phải cmd/paperledger")

// fetch GETs a fixed upstream URL and keeps the answer as bytes.
func (f *Feeds) fetch(rawURL string, requirePaper bool) (body, error) {
	ctx, cancel := context.WithTimeout(context.Background(), feedFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return body{}, err
	}
	req.Header.Set("User-Agent", "execportal-feed (read-only)")
	resp, err := f.client.Do(req)
	if err != nil {
		return body{}, err
	}
	defer resp.Body.Close()
	blob, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBodyBytes+1))
	if err != nil {
		return body{}, fmt.Errorf("đọc phản hồi: %w", err)
	}
	if len(blob) > maxFeedBodyBytes {
		return body{}, fmt.Errorf("phản hồi vượt %d MiB", maxFeedBodyBytes>>20)
	}
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); !strings.HasPrefix(ct, "application/json") {
		return body{}, fmt.Errorf("HTTP %d, Content-Type %q — không phải JSON", resp.StatusCode, ct)
	}
	if resp.StatusCode >= 500 {
		return body{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if requirePaper {
		if mode := resp.Header.Get("X-Execution-Mode"); mode != "paper" {
			return body{}, errNotPaperLedger
		}
		if resp.StatusCode != http.StatusOK {
			return body{}, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	return body{status: resp.StatusCode, bytes: blob}, nil
}

// writeFeed serves an upstream answer verbatim, labelled with its own mode: the
// portal's testnet label does not belong on a PAPER body or on public market
// data.
func writeFeed(w http.ResponseWriter, mode string, b body, readAt time.Time) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Execution-Mode", mode)
	h.Set("X-Execportal-Fetched-At-Ms", strconv.FormatInt(readAt.UnixMilli(), 10))
	w.WriteHeader(b.status)
	_, _ = w.Write(b.bytes)
}

// ScannerHistory relays cmd/scanner's settled funding history for one symbol.
func (f *Feeds) ScannerHistory(w http.ResponseWriter, r *http.Request) {
	if f.scannerAddr == "" {
		writeError(w, http.StatusServiceUnavailable, "feed_disabled", "portal chạy với -scanner-addr rỗng — không có nguồn scanner")
		return
	}
	query := r.URL.Query()
	symbol := query.Get("symbol")
	if !symbolPattern.MatchString(symbol) {
		writeError(w, http.StatusBadRequest, "bad_symbol", "symbol "+quote(symbol)+" không hợp lệ")
		return
	}
	days := query.Get("days")
	if days == "" {
		days = "30"
	}
	if !historyDays[days] {
		writeError(w, http.StatusBadRequest, "bad_days", "days phải là 7, 30, 90, 180 hoặc 365 — nhận "+quote(days))
		return
	}
	upstream := url.URL{Scheme: "http", Host: f.scannerAddr, Path: scannerHistoryPath,
		RawQuery: url.Values{"symbol": {symbol}, "days": {days}}.Encode()}
	b, readAt, err := f.history.get(symbol+"|"+days, historyTTL, func() (body, error) {
		return f.fetch(upstream.String(), false)
	})
	if err != nil {
		msg := "không đọc được lịch sử funding từ scanner " + f.scannerAddr + ": " + err.Error()
		f.note(&f.scanner, msg)
		writeError(w, http.StatusBadGateway, "feed_unreachable", msg)
		return
	}
	writeFeed(w, "scanner-feed", b, readAt)
}

// ScannerCrossRadar relays cmd/scanner's cross-venue funding radar (PLAN 4.5i)
// verbatim. It takes no parameter, so the upstream URL is fully fixed.
func (f *Feeds) ScannerCrossRadar(w http.ResponseWriter, r *http.Request) {
	if f.scannerAddr == "" {
		writeError(w, http.StatusServiceUnavailable, "feed_disabled", "portal chạy với -scanner-addr rỗng — không có nguồn scanner")
		return
	}
	upstream := url.URL{Scheme: "http", Host: f.scannerAddr, Path: scannerCrossRadarPath}
	b, readAt, err := f.crossRadar.get("radar", crossRadarTTL, func() (body, error) {
		return f.fetch(upstream.String(), false)
	})
	if err != nil {
		msg := "không đọc được radar chéo sàn từ scanner " + f.scannerAddr + ": " + err.Error()
		f.note(&f.scanner, msg)
		writeError(w, http.StatusBadGateway, "feed_unreachable", msg)
		return
	}
	writeFeed(w, "scanner-feed", b, readAt)
}

// ScannerCrossEvents relays the radar's episode log for one of the offered
// windows; anything else is refused before the scanner is asked.
func (f *Feeds) ScannerCrossEvents(w http.ResponseWriter, r *http.Request) {
	if f.scannerAddr == "" {
		writeError(w, http.StatusServiceUnavailable, "feed_disabled", "portal chạy với -scanner-addr rỗng — không có nguồn scanner")
		return
	}
	days := r.URL.Query().Get("days")
	if days == "" {
		days = "7"
	}
	if !crossEventDays[days] {
		writeError(w, http.StatusBadRequest, "bad_days", "days phải là 1, 7 hoặc 30 — nhận "+quote(days))
		return
	}
	upstream := url.URL{Scheme: "http", Host: f.scannerAddr, Path: scannerCrossEventsPath,
		RawQuery: url.Values{"days": {days}}.Encode()}
	b, readAt, err := f.crossEvents.get(days, crossEventsTTL, func() (body, error) {
		return f.fetch(upstream.String(), false)
	})
	if err != nil {
		msg := "không đọc được nhật ký đợt chênh từ scanner " + f.scannerAddr + ": " + err.Error()
		f.note(&f.scanner, msg)
		writeError(w, http.StatusBadGateway, "feed_unreachable", msg)
		return
	}
	writeFeed(w, "scanner-feed", b, readAt)
}

// PaperLedger relays cmd/paperledger's /api/ledger.
func (f *Feeds) PaperLedger(w http.ResponseWriter, r *http.Request) {
	if f.paperAddr == "" {
		writeError(w, http.StatusServiceUnavailable, "feed_disabled", "portal chạy với -paper-addr rỗng — không có nguồn sổ giấy")
		return
	}
	upstream := url.URL{Scheme: "http", Host: f.paperAddr, Path: paperLedgerPath}
	b, readAt, err := f.ledger.get("ledger", paperTTL, func() (body, error) {
		f.mu.Lock()
		f.paper.sessions++
		f.mu.Unlock()
		return f.fetch(upstream.String(), true)
	})
	if err != nil {
		code := "feed_unreachable"
		if errors.Is(err, errNotPaperLedger) {
			code = "not_paper_ledger"
		}
		msg := "không đọc được sổ giấy ở " + f.paperAddr + ": " + err.Error() +
			" — chạy `go run ./cmd/paperledger` (đọc SQLite mode=ro; trỏ -db vào một bản sao nếu muốn chắc chắn)"
		f.note(&f.paper, msg)
		writeError(w, http.StatusBadGateway, code, msg)
		return
	}
	f.note(&f.paper, "")
	writeFeed(w, "paper", b, readAt)
}
