// Command execportal is a web page for opening, watching and closing ONE kind of
// position — Strategy 1's spot long + perp short — on Binance TESTNET, or on
// Bybit's testnet or demo service with -broker=bybit (PLAN 4.5j).
//
//	go run ./cmd/execportal                 # http://127.0.0.1:8087
//	go run ./cmd/execportal -port 8088 -symbols BTCUSDT
//	go run ./cmd/execportal -broker bybit -port 8088   # BYBIT_API_KEY/SECRET + BYBIT_MODE
//
// On Bybit both legs trade on ONE Unified Trading Account: one key, one host,
// one request budget, one wallet. Its intent files live in .paper/exec-bybit,
// apart from Binance's, and cmd/execcheck does not read them.
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
	"futures-arbitrage-scanner/internal/coordinator"
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

// defaultSymbolsFlag is the shipped -symbols allow-list, named so the test that
// pins it reads the same string the binary ships.
const defaultSymbolsFlag = "BTCUSDT,ETHUSDT,SOLUSDT,BNBUSDT,XRPUSDT,DOGEUSDT,LTCUSDT,SUIUSDT,LINKUSDT,UNIUSDT,NEARUSDT,AAVEUSDT"

func main() {
	var (
		port = flag.String("port", "8087", "HTTP port (never 8082, 8085 or 8086 — those belong to the scanner runs and the paper ledger)")
		bind = flag.String("bind", "127.0.0.1", "loopback IP to listen on — 127.0.0.1 or ::1; anything else is refused, because this page places orders")
		// This flag is the portal's ALLOW-LIST — what the page and the bot MAY
		// trade — and not what a run trades. A run enters the subset the
		// operator ticks on the page, and the buffered-slot sizing then divides
		// the account across exactly those N pairs (4.5g), so a wider
		// allow-list costs nothing until a box is ticked.
		//
		// Restored to twelve on 2026-09-16 by the operator's decision, after
		// being screened to seven the same day. The screen's finding stands and
		// is now advice rather than a default: on the 3-year corpus SOLUSDT
		// funds negative most of the year, XRPUSDT funds at ≈ 0 and cannot pay a
		// round trip, NEARUSDT flips sign 188 times a year, and BNBUSDT and
		// DOGEUSDT sit at the bottom of the funding ranking (CLAUDE.md,
		// regularity 3). Ticking one of those spends a slot the others do not
		// get — which the operator can now see on the radar and decide pair by
		// pair.
		symbols  = flag.String("symbols", defaultSymbolsFlag, "comma-separated symbols the page and the auto-trader may trade, each the same string on both markets")
		marginFr = flag.Float64("margin-frac", 0.50, "collateral posted on the perp leg as a fraction of notional — a DECISION, not a venue fact")
		slipBps  = flag.Float64("max-slippage-bps", execution.DefaultMaxSlippageBps, "how far past the touch a leg's marketable limit may sit, in basis points")
		legTmo   = flag.Duration("leg-timeout", execution.DefaultLegTimeout, "how long one leg may work before its remainder is cancelled")
		actTmo   = flag.Duration("action-timeout", 3*time.Minute, "overall deadline for one open, close or reconcile")
		scanAddr = flag.String("scanner-addr", "127.0.0.1:8085", "loopback host:port of the running cmd/scanner whose /ws and funding history the Scanner tab relays READ-ONLY; empty turns the tab off")
		papAddr  = flag.String("paper-addr", "127.0.0.1:8086", "loopback host:port of cmd/paperledger whose /api/ledger the Paper tab relays READ-ONLY; empty turns the tab off")
		autoOn   = flag.Bool("autotrade", true, "switch the TESTNET auto-trader on at launch, with its shipped parameters on every -symbols entry (PLAN Q18); on by default — pass -autotrade=false to launch in paused state")
		brokerFl = flag.String("broker", string(venueBinance), "the venue BOTH legs trade on: binance (testnet) or bybit (testnet or demo, chosen by BYBIT_MODE) — never a leg on each")
		btJSON   = flag.String("backtest-json", "docs/reports/backtest-3y-latest.json", "the three-year backtest report the Backtest tab draws, built OUTSIDE this process by tools/report/bt3y.py; it is read from disk and cached by modification time, and the tab says so when the file is absent")

		// Engine 2 (PLAN 4.5k step 4). Off unless asked for, because it dials a
		// SECOND venue's credential and builds the lock that Engine 1 then has
		// to consult: a portal that did that by default would change what every
		// existing invocation does.
		crossOn = flag.Bool("crossperp", false, "wire Engine 2 — the cross-venue perp–perp engine, the exclusive symbol lock and the dual margin guard (needs BOTH Binance USDⓈ-M and Bybit linear credentials)")
		// The pilot DECIDES either way; this flag only lets it SEND. Off is the
		// shipped default and the state every acceptance so far has run in.
		crossPilotOn  = flag.Bool("crossperp-pilot", false, "let Engine 2's funding-spread pilot place and close orders by itself on the testnets; off leaves it advisory — it still measures and still shows every signal")
		crossNotional = flag.Float64("crossperp-notional", 100, "notional in quote on ONE leg of a pair the pilot opens")
		crossLocks    = flag.String("crossperp-locks", coordinator.DefaultLocksPath, "where the exclusive symbol lock table is written; it is a CACHE and the venues are the evidence (rule 7)")
		// Two portals sharing an intent directory could send the same derived
		// ClientOrderID twice, so the second one refuses to start (lockStateDir).
		// An Engine-2 portal beside a running Engine-1 one needs its own.
		stateDirFl = flag.String("state-dir", "", "override the intent directory; empty uses the venue's own (.paper/exec for Binance, .paper/exec-bybit for Bybit). A portal started beside another one MUST name a different directory")

		// Master Command Center: unified gateway for Scanner + Engine 1 + Engine 2 + Paper Ledger
		masterOn = flag.Bool("master", false, "run as the unified Master Command Center (default port 8080): enables Engine 1, Engine 2 (crossperp), coordinator, dual margin guard, and relays to scanner and paperledger")
	)
	flag.Parse()
	_ = godotenv.Load()

	if *masterOn {
		if !flagWasSet("port") {
			*port = "8080"
		}
		if !flagWasSet("state-dir") {
			*stateDirFl = filepath.Join(".paper", "exec-master")
		}
		*crossOn = true
	}

	bindIP, err := checkBind(*bind)
	if err != nil {
		log.Fatalf("execportal: %v", err)
	}
	if err := checkPort(*port); err != nil {
		log.Fatalf("execportal: %v", err)
	}
	kind, err := parseVenueKind(*brokerFl)
	if err != nil {
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

	m := dialMarketsFor(kind)
	if dir := strings.TrimSpace(*stateDirFl); dir != "" {
		m.profile.StateDir = dir
	}
	p := newPortal(m, symbolList, bindIP, *port, execSettings{
		MarginFrac: *marginFr, MaxSlippageBps: *slipBps, LegTimeout: *legTmo, ActionTimeout: *actTmo,
		BacktestJSONPath: *btJSON,
	}, time.Now)
	p.feeds = feeds.New(scannerAddr, paperAddr, time.Now)

	if *crossOn {
		crossSet := defaultCrossSettings()
		crossSet.MaxSlippageBps, crossSet.LegTimeout, crossSet.ActionTimeout = *slipBps, *legTmo, *actTmo
		crossSet.LocksPath, crossSet.PerpMarginFrac = *crossLocks, *marginFr
		pilotCfg := defaultCrossPilotConfig()
		pilotCfg.Enabled, pilotCfg.NotionalQuote, pilotCfg.PerpMarginFrac = *crossPilotOn, *crossNotional, *marginFr
		desk, err := newCrossDesk(dialCrossVenues(), symbolList, crossSet, pilotCfg, time.Now)
		if err != nil {
			p.crossOffVI = "Động cơ 2 KHÔNG dựng được: " + err.Error()
			log.Printf("execportal: -crossperp BỊ TỪ CHỐI — %v", err)
		} else {
			p.attachCross(desk)
			log.Printf("execportal: ĐỘNG CƠ 2 BẬT trên %s ⟷ %s — bảng khóa %s; phi công %s",
				crossVenueBinance, crossVenueBybit, *crossLocks, pilotModeVI(*crossPilotOn))
		}
	}

	log.Printf("execportal: sàn %s — CHỈ host phi-mainnet: %s", m.profile.LabelVI, strings.Join(m.profile.AllowedHosts, ", "))
	if m.profile.UnifiedWallet {
		log.Printf("execportal: spot và perp dùng CHUNG MỘT ví (Unified Trading Account) — số dư hai ô là MỘT khoản, không cộng")
	}
	logMarket("spot", m.spot != nil, m.spotSourceVI, m.spotErr)
	logMarket("futures", m.perp != nil, m.perpSourceVI, m.perpErr)
	log.Printf("execportal: nguồn CHỈ ĐỌC — scanner %s (relay /ws, chỉ khi tab Scanner mở, tối đa %d phiên) · sổ giấy %s",
		orOff(scannerAddr), feeds.MaxScannerRelays, orOff(paperAddr))
	if abs, err := filepath.Abs(p.stateDir); err == nil {
		shared := "chung với cmd/execcheck"
		if m.profile.Kind != venueBinance {
			shared = "RIÊNG của " + m.profile.LabelVI + ", cmd/execcheck không đọc"
		}
		log.Printf("execportal: file ý định (CACHE, %s): %s", shared, abs)
	}
	// .env and the intent cache are both resolved from the working directory,
	// exactly as cmd/execcheck resolves them. Started anywhere else, the portal
	// would read another .env and a different — probably empty — cache, and a
	// position opened from the repo root would not be tracked.
	if _, err := os.Stat("go.mod"); err != nil {
		wd, _ := os.Getwd()
		log.Printf("execportal: CẢNH BÁO — %s không phải gốc repo (không thấy go.mod): .env và %s được đọc từ ĐÂY; hãy chạy từ gốc repo", wd, p.stateDir)
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
	// Engine 2's own loops: the 5-second dual margin read, the pilot's scan, and
	// a retry of any lock a release could not give back. They start here, after
	// the listener exists and Ctrl-C is caught, for the same reason the bot does.
	if p.cross != nil {
		startCross(botCtx, p.cross, *actTmo)
	}

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
	// -autotrade defaults to true, and that default is right for the portal it
	// was written for: one venue, one strategy. It is WRONG for an Engine-2
	// portal, which is normally started beside a running Engine-1 one — the two
	// bots would then trade Strategy 1 on the same account from two processes.
	// So with -crossperp the bot starts only when -autotrade was passed BY NAME.
	autoStart := *autoOn
	if *crossOn && !flagWasSet("autotrade") {
		autoStart = false
		log.Printf("execportal: auto-trader Động cơ 1 KHÔNG tự bật vì -crossperp đang bật — nêu -autotrade tường minh nếu thật sự muốn CẢ HAI động cơ chạy từ tiến trình này")
	}
	if autoStart {
		if why := p.markets.profile.OrdersBlockedVI; why != "" {
			log.Printf("execportal: -autotrade BỊ TỪ CHỐI — %s", why)
		} else if why := p.markets.profile.AutotradeBlockedVI; why != "" {
			log.Printf("execportal: -autotrade BỊ TỪ CHỐI — %s", why)
		} else if err := m.both(); err != nil {
			log.Printf("execportal: -autotrade BỊ TỪ CHỐI — thiếu credential: %v", err)
		} else if _, err := p.autotrade.Start(autotrade.DefaultPortfolioConfig(symbolList)); err != nil {
			log.Printf("execportal: -autotrade BỊ TỪ CHỐI: %v", err)
		} else {
			log.Printf("execportal: AUTO-TRADER BẬT từ lúc khởi động (-autotrade) trên %s — %s, tự đặt lệnh (PLAN Q18)", strings.Join(symbolList, ", "), m.profile.LabelVI)
		}
	}
	go func() {
		if *masterOn {
			log.Printf("========================================================================")
			log.Printf("⚡ ARBITRAGE MASTER COMMAND CENTER · UNIFIED OPERATOR PORTAL")
			log.Printf("   Dashboard URL: http://%s", listener.Addr())
			log.Printf("   Engine 1: Cash & Carry (Spot + Perp)")
			log.Printf("   Engine 2: Cross-Venue Perp–Perp (Binance USD-M ⟷ Bybit Linear)")
			log.Printf("   Exclusive Symbol Lock: Coordinator Active (13 Symbols)")
			log.Printf("   Dual Margin Guard: 50%% (Yellow) | 60%% (Orange) | 65%% (Emergency Red)")
			log.Printf("========================================================================")
		} else {
			log.Printf("execportal: http://%s", listener.Addr())
		}
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

// flagWasSet reports whether a flag was named on the command line, as opposed
// to carrying its default. It is how -crossperp can change what -autotrade's
// DEFAULT means without changing what an explicit -autotrade means.
func flagWasSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func pilotModeVI(enabled bool) string {
	if enabled {
		return "ĐƯỢC PHÉP GỬI LỆNH (-crossperp-pilot)"
	}
	return "chỉ TƯ VẤN — đo và hiển thị, không gửi lệnh nào"
}

// startCross brings Engine 2 up: reconcile the lock table against both venues,
// adopt whatever it says this engine still holds, then run the guard, the pilot
// and the release retry until the context ends.
//
// A failed reconcile is logged and NOT fatal: the coordinator grants nothing
// until it has succeeded, so the failure mode is "Engine 2 cannot open", which
// is the safe one. Everything else on the page keeps working.
func startCross(ctx context.Context, desk *crossDesk, actionTimeout time.Duration) {
	boot, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	if err := desk.reconcileLocks(boot); err != nil {
		log.Printf("execportal/crossperp: ĐỐI SOÁT KHỞI ĐỘNG HỎNG (%v) — Động cơ 2 sẽ TỪ CHỐI mọi lệnh mở cho tới khi đối soát được; bấm ĐỐI SOÁT LẠI trên trang", err)
	} else {
		desk.adoptPairs(boot)
	}

	go desk.runMarginGuard(ctx)
	go desk.pilot.Run(ctx)
	go func() {
		ticker := time.NewTicker(crossReleaseRetryEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				desk.retryReleases(ctx)
			}
		}
	}()
}

// crossReleaseRetryEvery is how often a lock held only because its release
// failed is offered back to the coordinator again.
const crossReleaseRetryEvery = 60 * time.Second
