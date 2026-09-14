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
// # What it may and may not do (PLAN §7.1 Q15, extended by Q16 on 2026-09-14)
//
// It may place orders on TESTNET, because a person pressed a button and then
// confirmed a dialog. It may not reach a mainnet host — broker.NewClient
// refuses anything outside the documented testnet list and this command takes
// no flag that could move the host. It may not listen anywhere but loopback.
// And there is NO path from a live signal to an order: it imports neither
// internal/strategy's decisions nor the journal, and guard_test.go reads its
// source to keep it that way. Wiring a signal to an order waits for both the
// step-3.5 verdict and step 3.4.
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
		symbols  = flag.String("symbols", "BTCUSDT,ETHUSDT", "comma-separated symbols the page may trade, each the same string on both markets")
		marginFr = flag.Float64("margin-frac", 0.50, "collateral posted on the perp leg as a fraction of notional — a DECISION, not a venue fact")
		slipBps  = flag.Float64("max-slippage-bps", execution.DefaultMaxSlippageBps, "how far past the touch a leg's marketable limit may sit, in basis points")
		legTmo   = flag.Duration("leg-timeout", execution.DefaultLegTimeout, "how long one leg may work before its remainder is cancelled")
		actTmo   = flag.Duration("action-timeout", 3*time.Minute, "overall deadline for one open, close or reconcile")
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

	m := dialMarkets()
	p := newPortal(m, symbolList, bindIP, *port, execSettings{
		MarginFrac: *marginFr, MaxSlippageBps: *slipBps, LegTimeout: *legTmo, ActionTimeout: *actTmo,
	}, time.Now)

	log.Printf("execportal: CHỈ TESTNET — host cho phép: %s", strings.Join(broker.TestnetHosts(), ", "))
	logMarket("spot", m.spot != nil, m.spotSourceVI, m.spotErr)
	logMarket("futures", m.perp != nil, m.perpSourceVI, m.perpErr)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	p.writeMu.Lock()
	log.Printf("execportal: đã tắt")
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
