// Command execportal is a web page for opening, watching and closing ONE kind of
// position — Strategy 1's spot long + perp short — on Binance TESTNET.
//
//	go run ./cmd/execportal                 # http://127.0.0.1:8087
//	go run ./cmd/execportal -port 8088 -symbols BTCUSDT
//
// It is cmd/execcheck with a page in front of it: the same execution machine,
// the same derived ClientOrderIDs, the same intent files under .paper/exec, so
// a position opened in the browser can be read with `execcheck -status` and the
// other way round. Nothing it shows about a position comes from those files;
// balances, positions, orders and funding are read back from the venue.
//
// # What it may and may not do (PLAN §7.1 Q15, extended by Q16, Q17 and Q18)
//
// It may place orders on TESTNET, because a person pressed a button and then
// confirmed a dialog — or because a person switched the auto-trader on (Q18,
// package autotrade), which then opens and closes Strategy 1's pair on its own,
// through the same open and close a button runs. It may not reach a mainnet
// host — broker.NewClient refuses anything outside the documented testnet list
// and this command takes no flag that could move the host. It may not listen
// anywhere but loopback. And no signal of the step-3.5 gate reaches an order:
// the bot reads the testnet's own funding, books and fees, the command imports
// neither cmd/scanner nor the journal nor strategy's decisions, and
// guard_test.go reads its source to keep it that way. Wiring the gate's signal
// to real capital still waits for the step-3.5 verdict, step 3.4 and 4.6.
//
// No database, no schema, no migration: the step-3.5 gate has data/scanner.db
// open for writing on port 8085 and nothing here goes near either.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"futures-arbitrage-scanner/cmd/execportal/autotrade"
	"futures-arbitrage-scanner/cmd/execportal/feeds"
	"futures-arbitrage-scanner/internal/broker"
	"futures-arbitrage-scanner/internal/execution"

	"github.com/joho/godotenv"
)

// reservedPorts belong to other processes of this project, and one of them is a
// fortnight-long unattended run that must not find its port taken.
var reservedPorts = map[string]string{
	"8082": "phase-1 soak scanner",
	"8085": "step-3.5 gate scanner",
	"8086": "cmd/paperledger",
}

func main() {
	var (
		port     = flag.String("port", "8087", "HTTP port (never 8082, 8085 or 8086 — those belong to the scanner runs and the paper ledger)")
		bind     = flag.String("bind", "127.0.0.1", "loopback IP to listen on — 127.0.0.1 or ::1; anything else is refused, because this page places orders")
		symbols  = flag.String("symbols", "BTCUSDT,ETHUSDT,SOLUSDT,BNBUSDT,XRPUSDT,DOGEUSDT,LTCUSDT,SUIUSDT,LINKUSDT,UNIUSDT,NEARUSDT,AAVEUSDT", "comma-separated symbols the page and the auto-trader may trade, each the same string on both markets")
		marginFr = flag.Float64("margin-frac", 0.50, "collateral posted on the perp leg as a fraction of notional — a DECISION, not a venue fact")
		slipBps  = flag.Float64("max-slippage-bps", execution.DefaultMaxSlippageBps, "how far past the touch a leg's marketable limit may sit, in basis points")
		legTmo   = flag.Duration("leg-timeout", execution.DefaultLegTimeout, "how long one leg may work before its remainder is cancelled")
		actTmo   = flag.Duration("action-timeout", 3*time.Minute, "overall deadline for one open, close or reconcile")
		scanAddr = flag.String("scanner-addr", "127.0.0.1:8085", "loopback host:port of the running cmd/scanner whose /ws and funding history the Scanner tab relays READ-ONLY; empty turns the tab off")
		papAddr  = flag.String("paper-addr", "127.0.0.1:8086", "loopback host:port of cmd/paperledger whose /api/ledger the Paper tab relays READ-ONLY; empty turns the tab off")
		autoOn   = flag.Bool("autotrade", true, "switch the TESTNET auto-trader on at launch, with its shipped parameters on every -symbols entry (PLAN Q18); on by default — pass -autotrade=false to launch in paused state")
	)
	flag.Parse()
	_ = godotenv.Load()

	bindIP, err := checkBind(*bind)
	if err != nil {
		log.Fatalf("execportal: %v", err)
	}
	if err := checkPort(*port); err != nil {
		log.Fatalf("execportal: %v", err)
	}
	symbolList, err := parseSymbols(*symbols)
	if err != nil {
		log.Fatalf("execportal: %v", err)
	}
	if *marginFr <= 0 || *marginFr > 1 {
		log.Fatalf("execportal: -margin-frac %v must be in (0, 1]", *marginFr)
	}
	if *slipBps < 0 {
		log.Fatalf("execportal: -max-slippage-bps %v is negative; it would price a buy below the touch", *slipBps)
	}
	if *legTmo <= 0 || *actTmo <= *legTmo {
		log.Fatalf("execportal: -leg-timeout %s must be positive and shorter than -action-timeout %s", *legTmo, *actTmo)
	}

	scannerAddr, err := feeds.CheckAddr("scanner-addr", *scanAddr, *port)
	if err != nil {
		log.Fatalf("execportal: %v", err)
	}
	paperAddr, err := feeds.CheckAddr("paper-addr", *papAddr, *port)
	if err != nil {
		log.Fatalf("execportal: %v", err)
	}

	m := dialMarkets()
	p := newPortal(m, symbolList, bindIP, *port, execSettings{
		MarginFrac: *marginFr, MaxSlippageBps: *slipBps, LegTimeout: *legTmo, ActionTimeout: *actTmo,
	}, time.Now)
	p.feeds = feeds.New(scannerAddr, paperAddr, time.Now)

	log.Printf("execportal: CHỈ TESTNET — host cho phép: %s", strings.Join(broker.TestnetHosts(), ", "))
	logMarket("spot", m.spot != nil, m.spotSourceVI, m.spotErr)
	logMarket("futures", m.perp != nil, m.perpSourceVI, m.perpErr)
	log.Printf("execportal: nguồn CHỈ ĐỌC — scanner %s (relay /ws, chỉ khi tab Scanner mở, tối đa %d phiên) · sổ giấy %s",
		orOff(scannerAddr), feeds.MaxScannerRelays, orOff(paperAddr))
	if abs, err := filepath.Abs(p.stateDir); err == nil {
		log.Printf("execportal: file ý định (CACHE, chung với cmd/execcheck): %s", abs)
	}
	// .env and the intent cache are both resolved from the working directory,
	// exactly as cmd/execcheck resolves them. Started anywhere else, the portal
	// would read another .env and a different — probably empty — cache, and a
	// position opened from the repo root would not be tracked.
	if _, err := os.Stat("go.mod"); err != nil {
		wd, _ := os.Getwd()
		log.Printf("execportal: CẢNH BÁO — %s không phải gốc repo (không thấy go.mod): .env và %s được đọc từ ĐÂY; hãy chạy từ gốc repo", wd, stateDir)
	}

	unlock, err := lockStateDir(p.stateDir)
	if err != nil {
		log.Fatalf("execportal: %v", err)
	}
	defer unlock()

	listener, err := net.Listen("tcp", net.JoinHostPort(bindIP, *port))
	if err != nil {
		log.Fatalf("execportal: listen: %v", err)
	}
	srv := &http.Server{
		Handler:           p.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// A write holds its response until the venue has answered both legs.
		// Its worst case is the action deadline PLUS execution's own detached
		// close-out (UnwindTimeout) and read-backs, so the deadline for writing
		// the answer is generous; the order work never depends on it.
		WriteTimeout: *actTmo + 2*time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
	// Shutdown does not wait for hijacked connections; the relays are ended
	// explicitly so the scanner sees its clients leave.
	srv.RegisterOnShutdown(p.feeds.CloseAll)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The auto-trader's loop runs for the life of the process and trades
	// nothing until it is switched on. It starts only now — after the listener
	// exists (a failed listen exits the process) and after Ctrl-C is caught —
	// so nothing it opens can be cut short by a start-up failure. Its context
	// ends at shutdown, which drops any decision not yet sent; an open or close
	// already sent runs to its own deadline, and main waits for it below.
	botCtx, stopBot := context.WithCancel(context.Background())
	botDone := make(chan struct{})
	go func() {
		p.autotrade.Run(botCtx)
		close(botDone)
	}()
	// The equity series of the PnL page: one sample a minute while the bot runs
	// or holds a pair (pnl.go). It reads, and never trades.
	go func() {
		ticker := time.NewTicker(pnlSampleEvery)
		defer ticker.Stop()
		for {
			select {
			case <-botCtx.Done():
				return
			case <-ticker.C:
				p.samplePnL(botCtx, p.autotrade.Status())
			}
		}
	}()
	if *autoOn {
		if err := m.both(); err != nil {
			log.Printf("execportal: -autotrade BỊ TỪ CHỐI — thiếu credential: %v", err)
		} else if _, err := p.autotrade.Start(autotrade.DefaultPortfolioConfig(symbolList)); err != nil {
			log.Printf("execportal: -autotrade BỊ TỪ CHỐI: %v", err)
		} else {
			log.Printf("execportal: AUTO-TRADER BẬT từ lúc khởi động (-autotrade) trên %s — TESTNET, tự đặt lệnh (PLAN Q18)", strings.Join(symbolList, ", "))
		}
	}
	go func() {
		log.Printf("execportal: http://%s", listener.Addr())
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("execportal: HTTP server stopped: %v", err)
			stop()
		}
	}()
	<-ctx.Done()
	// From here a second Ctrl-C is Go's default: the process dies at once.
	stop()
	stopBot()

	// Shutdown stops accepting and then WAITS for handlers in flight, with no
	// deadline of its own. An open half-way through its second leg must finish
	// — the invariant is only a promise about calls that return — and its
	// worst case is not a number this function knows. writeMu is the last
	// word: it is released only when the action has returned.
	p.busyMu.Lock()
	inFlight := p.busyAction
	p.busyMu.Unlock()
	if inFlight != "" {
		log.Printf("execportal: đang tắt — chờ thao tác %q đang chạy hoàn tất; Ctrl-C lần nữa để thoát NGAY (có thể để lại một chân trần — kiểm bằng execcheck -status)", inFlight)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Printf("execportal: shutdown: %v", err)
	}
	select {
	case <-botDone:
	default:
		log.Printf("execportal: đang tắt — chờ auto-trader xong lượt đang chạy (một lệnh mở/đóng đã gửi thì chạy tới hết hạn của nó); Ctrl-C lần nữa để thoát NGAY (có thể để lại một chân trần — kiểm bằng execcheck -status)")
		<-botDone
	}
	p.writeMu.Lock()
	log.Printf("execportal: đã tắt")
}

func orOff(addr string) string {
	if addr == "" {
		return "TẮT"
	}
	return addr
}

func logMarket(name string, configured bool, sourceVI string, err error) {
	if configured {
		log.Printf("execportal: %s — credential từ %s", name, sourceVI)
		return
	}
	log.Printf("execportal: %s — CHƯA CẤU HÌNH (%v); trang vẫn chạy, không đặt được lệnh", name, err)
}

// checkBind accepts only a loopback IP LITERAL. "localhost" is refused too: it
// is resolved by the machine's resolver, which the operator does not control
// from here, and a name that resolves to a LAN address would publish an order
// button to the network.
func checkBind(raw string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return "", fmt.Errorf("-bind %q is not an IP literal; use 127.0.0.1 or ::1", raw)
	}
	if !ip.IsLoopback() {
		return "", fmt.Errorf("-bind %q is not a loopback address — this page places orders and must not be reachable from another machine", raw)
	}
	return ip.String(), nil
}

func checkPort(raw string) error {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1024 || n > 65535 {
		return fmt.Errorf("-port %q must be a number in 1024..65535", raw)
	}
	if owner, ok := reservedPorts[strconv.Itoa(n)]; ok {
		return fmt.Errorf("-port %d belongs to the %s — pick another", n, owner)
	}
	return nil
}

func parseSymbols(raw string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		s := strings.ToUpper(strings.TrimSpace(part))
		if s == "" {
			continue
		}
		if !symbolPattern.MatchString(s) {
			return nil, fmt.Errorf("-symbols: %q is not a symbol", part)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("-symbols names no symbol")
	}
	return out, nil
}
