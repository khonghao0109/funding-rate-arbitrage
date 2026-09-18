package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"futures-arbitrage-scanner/internal/broker"
)

// This command places real orders on a real exchange from a web page. The only
// things between it and somebody's money are that the exchange is a testnet and
// that the page answers nobody but this machine, so the tests here are about
// those — and about staying away from the step-3.5 gate — and nothing else.

// clearCredentialEnv makes the test independent of whatever the shell exports.
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, pairs := range [][]broker.EnvPair{futuresEnv, spotEnv, bybitCredentialEnv} {
		for _, p := range pairs {
			t.Setenv(p.KeyVar, "")
			t.Setenv(p.SecretVar, "")
		}
	}
}

// Both markets resolve to a host on the documented testnet allow-list, through
// the SAME function main uses to build them.
func TestExecportal_BothMarketsResolveToATestnetHost(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("BINANCE_FUTURES_TESTNET_API_KEY", "k")
	t.Setenv("BINANCE_FUTURES_TESTNET_API_SECRET", "s")
	t.Setenv("BINANCE_SPOT_TESTNET_API_KEY", "k")
	t.Setenv("BINANCE_SPOT_TESTNET_API_SECRET", "s")

	allowed := map[string]bool{}
	for _, h := range broker.TestnetHosts() {
		allowed[h] = true
	}
	if len(allowed) == 0 {
		t.Fatal("the allow-list is empty, so this test would pass on anything")
	}
	m := dialMarkets()
	if err := m.both(); err != nil {
		t.Fatalf("dialMarkets with both pairs set: %v", err)
	}
	for name, base := range map[string]string{"spot": m.spot.HTTP().BaseURL(), "futures": m.perp.HTTP().BaseURL()} {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !allowed[u.Hostname()] {
			t.Errorf("%s resolves to %q, which is not on the testnet allow-list %v", name, u.Hostname(), broker.TestnetHosts())
		}
		if u.Scheme != "https" {
			t.Errorf("%s uses scheme %q", name, u.Scheme)
		}
	}
	if !strings.Contains(m.perpSourceVI, "BINANCE_FUTURES_TESTNET_API_KEY") || strings.Contains(m.perpSourceVI+m.spotSourceVI, "\"k\"") {
		t.Errorf("credential source should name the variables and never the values: %q / %q", m.perpSourceVI, m.spotSourceVI)
	}
}

// -broker=bybit (PLAN 4.5j): both markets on the Bybit scheme's own allow-list,
// over ONE signed transport, into their own intent directory — for both
// non-production services, and never for a mode nobody set.
func TestExecportal_BybitMarketsResolveToANonProductionHost(t *testing.T) {
	allowed := map[string]bool{}
	for _, h := range broker.TestnetHostsFor(broker.SchemeBybitV5Header) {
		allowed[h] = true
	}
	if len(allowed) == 0 || allowed["api.bybit.com"] {
		t.Fatalf("the Bybit allow-list is empty or holds mainnet: %v", allowed)
	}
	for mode, wantHost := range map[string]string{"testnet": "api-testnet.bybit.com", "demo": "api-demo.bybit.com"} {
		clearCredentialEnv(t)
		t.Setenv("BYBIT_TESTNET", "")
		t.Setenv("BYBIT_API_KEY", "k")
		t.Setenv("BYBIT_API_SECRET", "s")
		t.Setenv("BYBIT_MODE", mode)
		m := dialMarketsFor(venueBybit)
		if err := m.both(); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		for name, c := range map[string]venue{"spot": m.spot, "perp": m.perp} {
			u, err := url.Parse(c.HTTP().BaseURL())
			if err != nil || u.Scheme != "https" || u.Hostname() != wantHost || !allowed[u.Hostname()] {
				t.Errorf("%s %s resolves to %v (%v)", mode, name, u, err)
			}
		}
		if m.spot.HTTP() != m.perp.HTTP() {
			t.Errorf("%s: two transports — each would believe it owns the venue's whole request budget", mode)
		}
		if m.spot.Market() != broker.MarketSpot || m.perp.Market() != broker.MarketFuturesUSDM {
			t.Errorf("%s: markets %s / %s", mode, m.spot.Market(), m.perp.Market())
		}
		if !m.profile.UnifiedWallet || m.profile.StateDir == stateDir || strings.Contains(m.perpSourceVI, "\"k\"") {
			t.Errorf("%s: profile %+v, source %q", mode, m.profile, m.perpSourceVI)
		}
	}
	clearCredentialEnv(t)
	t.Setenv("BYBIT_API_KEY", "k")
	t.Setenv("BYBIT_API_SECRET", "s")
	t.Setenv("BYBIT_MODE", "")
	t.Setenv("BYBIT_TESTNET", "")
	if m := dialMarketsFor(venueBybit); m.both() == nil {
		t.Error("no BYBIT_MODE, and a host was chosen for the key anyway")
	}
	if _, err := parseVenueKind("okx"); err == nil {
		t.Error("-broker okx was accepted")
	}
	if k, err := parseVenueKind(" Bybit "); err != nil || k != venueBybit {
		t.Errorf("%q %v", k, err)
	}
}

// A missing credential is a market with no client and a reason that names the
// variables — the page still comes up and says so.
func TestExecportal_MissingCredentialIsNamedNotFatal(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("BINANCE_SPOT_TESTNET_API_KEY", "k")
	t.Setenv("BINANCE_SPOT_TESTNET_API_SECRET", "s")

	m := dialMarkets()
	if m.spot == nil {
		t.Fatalf("spot had a credential and got no client: %v", m.spotErr)
	}
	if m.perp != nil {
		t.Fatal("futures had no credential and still got a client")
	}
	err := m.both()
	if err == nil {
		t.Fatal("both() says an order can be placed with one market missing")
	}
	if !strings.Contains(err.Error(), "BINANCE_FUTURES_TESTNET_API_KEY") {
		t.Errorf("the refusal does not name the variable to set: %v", err)
	}
}

// broker.NewClient is the guard, and it is not the portal's to widen: a
// mainnet URL is refused whichever command asks.
func TestExecportal_TheGuardItReliesOnRefusesMainnet(t *testing.T) {
	creds := broker.Credentials{APIKey: broker.NewSecret("k"), APISecret: broker.NewSecret("s")}
	for _, base := range []string{"https://fapi.binance.com", "https://api.binance.com", "http://demo-fapi.binance.com"} {
		if _, err := broker.NewClient(broker.Config{BaseURL: base, Credentials: creds,
			TimePath: broker.BinanceFuturesTimePath, WeightLimitPerMin: broker.BinanceFuturesWeightPerMin}); err == nil {
			t.Errorf("broker.NewClient accepted %q", base)
		}
	}
}

// No flag may move the host, and no mainnet host is named anywhere in the
// command. A -base-url would route around broker.NewClient's guard without
// touching that package.
func TestExecportal_HasNoFlagOrHostThatCouldReachMainnet(t *testing.T) {
	src := readOwnSource(t, false)
	for _, bad := range []string{
		`flag.String("base-url"`, `flag.String("host"`, `flag.String("url"`, `flag.String("api"`,
		`flag.Bool("mainnet"`, `flag.Bool("live"`, `flag.Bool("real"`, `flag.Bool("prod"`,
	} {
		if strings.Contains(src, bad) {
			t.Errorf("this command declares %s — the testnet guard can be routed around", bad)
		}
	}
	// Written with what precedes a production host in a URL, so the testnet's
	// own demo-fapi.binance.com does not match its mainnet sibling.
	for _, host := range []string{"//fapi.binance.com", "\"fapi.binance.com", "//api.binance.com", "\"api.binance.com",
		"api1.binance.com", "api-gcp.binance.com", "testnet.binancefuture.com"} {
		if strings.Contains(src, host) {
			t.Errorf("this command names %q", host)
		}
	}
}

// Q15 limit 2, and limit 5 as Q18 narrowed it, by machine: no import of the
// scanner, the store, the journal or the strategy's DECISIONS, and no call to
// EvaluateEntry or EvaluateExit anywhere in the command. execution links
// internal/strategy for EstimateFill, so the binary contains the package; what
// is forbidden is THIS command reaching for the gate's decision. Q18's one
// exception is package autotrade importing strategy for its ARITHMETIC —
// TestAutotrade_DecidesOnTheTestnetAndTradesOnlyThroughThePortal names exactly
// which of its identifiers.
func TestExecportal_ImportsNoScannerStoreOrSignal(t *testing.T) {
	forbiddenImports := []string{
		"futures-arbitrage-scanner/cmd/",
		"futures-arbitrage-scanner/internal/scanner",
		"futures-arbitrage-scanner/internal/store",
		"futures-arbitrage-scanner/internal/strategy",
		"futures-arbitrage-scanner/internal/config",
		"futures-arbitrage-scanner/internal/history",
		"futures-arbitrage-scanner/internal/paper",
		"futures-arbitrage-scanner/internal/notify",
		// The phase-6 signal core and the replay engine produce decisions; the
		// Crowding tab shows a static research snapshot and needs neither.
		"futures-arbitrage-scanner/internal/crowding",
		"futures-arbitrage-scanner/internal/backtest",
		"database/sql",
		"modernc.org/sqlite",
	}
	forbiddenNames := map[string]bool{"EvaluateEntry": true, "EvaluateExit": true}

	fset := token.NewFileSet()
	files := 0
	for _, path := range ownGoFiles(t, false) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		inAutotrade := strings.HasPrefix(path, "autotrade"+string(filepath.Separator))
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if imported == feedsImportPath || imported == autotradeImportPath {
				continue // the two sub-packages the command may import; each has its own test below
			}
			if inAutotrade && imported == strategyImportPath {
				continue // Q18: the arithmetic only, held by the autotrade test
			}
			for _, bad := range forbiddenImports {
				if strings.HasPrefix(imported, bad) {
					t.Errorf("%s imports %s", path, imported)
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if forbiddenNames[x.Name] {
					t.Errorf("%s refers to %s — there is no path from a signal to an order before 3.5 and 3.4 (PLAN Q15)", fset.Position(x.Pos()), x.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && strings.Contains(x.Value, "signal_journal") {
					t.Errorf("%s names the signal journal", fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
	if files < 8 {
		t.Fatalf("only %d files parsed — the test is not seeing the command and its sub-packages", files)
	}
}

const (
	feedsImportPath     = "futures-arbitrage-scanner/cmd/execportal/feeds"
	autotradeImportPath = "futures-arbitrage-scanner/cmd/execportal/autotrade"
	strategyImportPath  = "futures-arbitrage-scanner/internal/strategy"
)

// PLAN Q18: package autotrade may switch orders on and off by itself, on the
// testnet. What keeps that from becoming a second order path, or a path from
// the step-3.5 gate to an order:
//
//   - autotrade DECIDES and never executes: its imports are an allow-list —
//     the standard library it needs, exchanges' types, depth, fees, and
//     strategy — so no broker, no execution machine, no network, no file, and
//     (through the link check) nothing of the scanner, the store or the feeds;
//   - of strategy it names only the cost and APR arithmetic, never a decision;
//   - in the main package only the wiring names it (api.go builds it, main.go
//     runs it and its PnL sampler, server.go routes to handlers in
//     autotrade.go, pnl.go reads its status views); that its orders
//     reach the venue only through openAs and close is held for the WHOLE
//     package by TestExecportal_EveryOrderPathHasFixedCallers.
func TestAutotrade_DecidesOnTheTestnetAndTradesOnlyThroughThePortal(t *testing.T) {
	allowedImports := map[string]bool{
		"context": true, "errors": true, "fmt": true, "math": true, "sort": true, "strings": true, "sync": true, "time": true,
		"futures-arbitrage-scanner/exchanges":      true,
		"futures-arbitrage-scanner/internal/depth": true,
		"futures-arbitrage-scanner/internal/fees":  true,
		strategyImportPath:                         true,
	}
	// The cost and APR arithmetic, and nothing else. EstimateFill and its two
	// sides are the same family as RoundTripCost — which calls EstimateFill
	// itself — and a held pair needs them per leg: pricing its exit through
	// RoundTripCost would also price the two ENTRY fills and refuse the whole
	// figure when the side the exit never takes cannot be filled. What stays
	// banned is every function that DECIDES: EvaluateEntry, EvaluateExit,
	// Params, Candidate — the gate's rules stay the gate's (PLAN Q18).
	allowedStrategy := map[string]bool{
		"NetAPR": true, "NetAPRInput": true, "RoundTripCost": true, "RoundTripInput": true, "RoundTrip": true,
		"EstimateFill": true, "FillEstimate": true, "Side": true, "SideBuy": true, "SideSell": true,
	}
	fset := token.NewFileSet()
	files := 0
	for _, path := range ownGoFiles(t, false) {
		if !strings.HasPrefix(path, "autotrade"+string(filepath.Separator)) {
			continue
		}
		files++
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		strategyName := ""
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if !allowedImports[imported] {
				t.Errorf("%s imports %s — package autotrade decides and reads nothing but what it is handed (PLAN Q18)", path, imported)
			}
			if imported == strategyImportPath {
				strategyName = "strategy"
				if spec.Name != nil {
					t.Errorf("%s imports strategy as %q — renaming it hides it from this test", path, spec.Name.Name)
				}
			}
		}
		if strategyName == "" {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == strategyName && !allowedStrategy[sel.Sel.Name] {
					t.Errorf("%s uses strategy.%s — autotrade borrows the cost and APR arithmetic, never a decision (PLAN Q18)", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	if files < 3 {
		t.Fatalf("read %d files of package autotrade — the test is not seeing it", files)
	}

	// In the main package: who may name the package, and what the adapter may
	// call to reach a venue.
	mayImport := map[string]bool{"api.go": true, "autotrade.go": true, "main.go": true, "pnl.go": true}
	for _, path := range topLevelGoFiles(t, false) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); imported == autotradeImportPath {
				if !mayImport[path] {
					t.Errorf("%s imports package autotrade — only its wiring may (PLAN Q18)", path)
				}
				if spec.Name != nil {
					t.Errorf("%s imports package autotrade as %q", path, spec.Name.Name)
				}
			}
		}
	}

	if _, err := exec.LookPath("go"); err == nil {
		cmd := exec.Command("go", "list", "-deps", "./cmd/execportal/autotrade")
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps ./cmd/execportal/autotrade: %v", err)
		}
		if !strings.Contains(string(out), strategyImportPath) {
			t.Fatal("the dependency list lacks internal/strategy — the test is not reading the real build")
		}
		for _, bad := range []string{"futures-arbitrage-scanner/internal/broker", "futures-arbitrage-scanner/internal/execution",
			"futures-arbitrage-scanner/cmd/", "futures-arbitrage-scanner/internal/scanner", "futures-arbitrage-scanner/internal/store",
			"futures-arbitrage-scanner/internal/config", "futures-arbitrage-scanner/internal/paper", "database/sql"} {
			for _, line := range strings.Split(string(out), "\n") {
				if dep := strings.TrimSpace(line); strings.HasPrefix(dep, bad) && dep != autotradeImportPath {
					t.Errorf("package autotrade links %s", dep)
				}
			}
		}
	}
}

// PLAN Q17: the scanner and paper feeds live in this binary, so "no path from
// a signal to an order" has to hold INSIDE it. The boundary is a package, so
// the compiler holds most of it; these tests hold the rest:
//
//   - package feeds exports exactly three handlers, a health view, a shutdown
//     hook, a constructor and an address check — adding a getter that returns
//     feed data fails here and reopens Q17;
//   - it links no internal/ package and decodes nothing;
//   - in the main package only the HTTP wiring names it, only through those
//     selectors and never under another name, and no file holds an HTTP client
//     or records a handler's answer.
//
// This pins the exported surface and closes the obvious routes; it is not a
// proof. A determined change can still write its own ResponseWriter — which
// is why Q17 names the join as the operator's eyes and nothing else.
func TestExecportal_NoPathFromAFeedToAnOrder(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "feeds", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := pkgs["feeds"]
	if !ok || len(pkg.Files) < 2 {
		t.Fatalf("package feeds not found or not whole: %v", pkgs)
	}
	allowedExports := map[string]bool{
		"Feeds": true, "New": true, "CheckAddr": true, "MaxScannerRelays": true,
		"View": true, "FeedView": true, "ScannerView": true,
		"Feeds.ScannerSocket": true, "Feeds.ScannerHistory": true, "Feeds.PaperLedger": true,
		"Feeds.ScannerCrossRadar": true, "Feeds.ScannerCrossEvents": true,
		"Feeds.View": true, "Feeds.CloseAll": true,
	}
	for name, file := range pkg.Files {
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if strings.HasPrefix(imported, "futures-arbitrage-scanner/") {
				t.Errorf("%s imports %s — the feeds meet nothing of this repository", name, imported)
			}
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				key := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					recv := d.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					if id, ok := recv.(*ast.Ident); ok {
						key = id.Name + "." + key
					}
				}
				if !allowedExports[key] {
					t.Errorf("%s exports %s — package feeds may expose handlers, health and shutdown only (PLAN Q17)", name, key)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						if sp.Name.IsExported() && !allowedExports[sp.Name.Name] {
							t.Errorf("%s exports type %s", name, sp.Name.Name)
						}
						// A view carries health: numbers, flags and messages. A
						// slice, a map or a byte field is where feed data would go.
						if st, ok := sp.Type.(*ast.StructType); ok && sp.Name.IsExported() && sp.Name.Name != "Feeds" {
							for _, field := range st.Fields.List {
								typ, _ := field.Type.(*ast.Ident)
								if typ == nil || !map[string]bool{"string": true, "bool": true, "int": true, "int64": true, "FeedView": true, "ScannerView": true}[typ.Name] {
									t.Errorf("%s: %s has a field of type %s — views carry health, not data", name, sp.Name.Name, types(field.Type))
								}
							}
						}
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							if n.IsExported() && !allowedExports[n.Name] {
								t.Errorf("%s exports %s", name, n.Name)
							}
						}
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Unmarshal", "NewDecoder", "Decode", "ReadJSON", "Valid":
					t.Errorf("%s: package feeds calls %s — a feed is relayed as bytes, never read", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}

	// In the main package: who may import feeds, under which name, and what
	// they may do with it. Every use of p.feeds or of the package must be a
	// selector on the allowlist — a bare "h := p.feeds" is refused too.
	allowedUse := map[string]map[string]bool{
		"server.go": {"ScannerSocket": true, "ScannerHistory": true, "PaperLedger": true, "ScannerCrossRadar": true, "ScannerCrossEvents": true},
		"api.go":    {"New": true, "View": true, "Feeds": true},
		"main.go":   {"New": true, "CheckAddr": true, "MaxScannerRelays": true, "CloseAll": true},
	}
	for _, path := range topLevelGoFiles(t, false) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		imports := false
		for _, spec := range file.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); imported == feedsImportPath {
				imports = true
				if spec.Name != nil {
					t.Errorf("%s imports package feeds as %q — renaming it hides it from this test", path, spec.Name.Name)
				}
			}
		}
		allowed, wiring := allowedUse[path]
		if imports && !wiring {
			t.Errorf("%s imports package feeds — only the HTTP wiring may (PLAN Q17)", path)
		}
		// Walk with parents, so "feeds" is accepted only as the X of an
		// allowed selector.
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			var parent ast.Node
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			stack = append(stack, n)
			isFeeds := false
			switch x := n.(type) {
			case *ast.Ident:
				isFeeds = x.Name == "feeds"
				if sel, ok := parent.(*ast.SelectorExpr); ok && sel.Sel == x {
					isFeeds = false // a field name: handled as the selector below
				}
				if kv, ok := parent.(*ast.KeyValueExpr); ok && kv.Key == x {
					isFeeds = false // the composite-literal key in newPortal
				}
				if field, ok := parent.(*ast.Field); ok {
					for _, name := range field.Names {
						if name == x {
							isFeeds = false // the struct field declaration
						}
					}
				}
			case *ast.SelectorExpr:
				isFeeds = x.Sel.Name == "feeds"
			}
			if !isFeeds {
				return true
			}
			if !wiring {
				t.Errorf("%s names feeds — no order path may (PLAN Q17)", fset.Position(n.Pos()))
				return true
			}
			if assign, ok := parent.(*ast.AssignStmt); ok && path == "main.go" && len(assign.Lhs) == 1 && assign.Lhs[0] == n {
				return true // main installs the feeds: p.feeds = feeds.New(…)
			}
			outer, ok := parent.(*ast.SelectorExpr)
			if !ok || outer.X != n {
				t.Errorf("%s: feeds used on its own — only an allowlisted selector on it is permitted (PLAN Q17)", fset.Position(n.Pos()))
				return true
			}
			if !allowed[outer.Sel.Name] {
				t.Errorf("%s uses feeds.%s, which %s may not (PLAN Q17)", fset.Position(outer.Pos()), outer.Sel.Name, path)
			}
			if path == "server.go" {
				// A handler may only be registered: inside handler(), within a
				// statement that IS a route registration — get(...) or
				// mux.Handle(...) — and never assigned, returned or stored.
				inHandler, registered, stored := false, false, false
				for i := len(stack) - 1; i >= 0; i-- {
					switch anc := stack[i].(type) {
					case *ast.AssignStmt, *ast.ReturnStmt, *ast.ValueSpec, *ast.CompositeLit, *ast.KeyValueExpr, *ast.FuncLit:
						stored = true
					case *ast.ExprStmt:
						if !registered {
							if call, ok := anc.X.(*ast.CallExpr); ok {
								switch fun := call.Fun.(type) {
								case *ast.Ident:
									registered = fun.Name == "get"
								case *ast.SelectorExpr:
									if x, ok := fun.X.(*ast.Ident); ok {
										registered = x.Name == "mux" && fun.Sel.Name == "Handle"
									}
								}
							}
							if !registered {
								stored = true
							}
						}
					case *ast.FuncDecl:
						inHandler = anc.Name.Name == "handler"
						i = -1
					}
					if registered || stored {
						for j := i - 1; j >= 0; j-- {
							if fn, ok := stack[j].(*ast.FuncDecl); ok {
								inHandler = fn.Name.Name == "handler"
							}
						}
						break
					}
				}
				if !inHandler || !registered || stored {
					t.Errorf("%s: feeds.%s must be passed straight into a get(...) or mux.Handle(...) registration inside handler()", fset.Position(outer.Pos()), outer.Sel.Name)
				}
			}
			return true
		})
		// No file makes an HTTP request: the selectors of net/http that build a
		// client or send a request are refused under whatever name the file
		// gives the package, and renaming it is refused too.
		httpName := ""
		for _, spec := range file.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); imported == "net/http" {
				httpName = "http"
				if spec.Name != nil {
					t.Errorf("%s imports net/http as %q — renaming it hides it from this test", path, spec.Name.Name)
					httpName = spec.Name.Name
				}
			}
		}
		if httpName != "" {
			clientSide := map[string]bool{"Client": true, "Transport": true, "DefaultClient": true, "DefaultTransport": true,
				"Get": true, "Head": true, "Post": true, "PostForm": true, "NewRequest": true, "NewRequestWithContext": true, "ReadResponse": true}
			ast.Inspect(file, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == httpName && clientSide[sel.Sel.Name] {
						t.Errorf("%s uses %s.%s — the command makes no HTTP request of its own (PLAN Q17)", fset.Position(sel.Pos()), httpName, sel.Sel.Name)
					}
				}
				return true
			})
		}
		// Reading a feed from Go without package feeds would take an HTTP
		// client or a recorded handler. The command makes no request of its
		// own (the broker and the feeds do), and only server.go serves.
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"net/http/httptest", "gorilla/websocket", "net.Dial", "net/rpc"} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s contains %s — the command reads no feed itself (PLAN Q17)", path, bad)
			}
		}
		if path != "server.go" && strings.Contains(string(src), "ServeHTTP") {
			t.Errorf("%s calls ServeHTTP — only server.go serves, and nothing else may record a handler's answer", path)
		}
	}
	// No other sub-package may import feeds either.
	for _, path := range ownGoFiles(t, false) {
		if !strings.Contains(path, string(filepath.Separator)) || strings.HasPrefix(path, "feeds"+string(filepath.Separator)) {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); imported == feedsImportPath {
				t.Errorf("%s imports package feeds", path)
			}
		}
	}

	if _, err := exec.LookPath("go"); err == nil {
		cmd := exec.Command("go", "list", "-deps", "./cmd/execportal/feeds")
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps ./cmd/execportal/feeds: %v", err)
		}
		if !strings.Contains(string(out), "github.com/gorilla/websocket") {
			t.Fatal("the dependency list lacks gorilla/websocket — the test is not reading the real build")
		}
		for _, line := range strings.Split(string(out), "\n") {
			if dep := strings.TrimSpace(line); strings.HasPrefix(dep, "futures-arbitrage-scanner/") && dep != feedsImportPath {
				t.Errorf("package feeds links %s", dep)
			}
		}
	}
}

// Every function in the main package that sends, or builds something that
// sends, an order has a FIXED set of callers, and the signed transport is named
// nowhere. This is what makes "the bot trades only through openAs and close"
// true of the package rather than of one file: a helper in a new file that
// reconciles, a method value stored for later, or a signed POST through
// p.markets.perp.HTTP() all go red here (review of Q18, 2026-09-15).
//
// Callers are FuncDecls ("portal.reconcile", "portalTrader.Close"); a use
// outside any function — a package-level variable holding a closure — has no
// caller and is refused. A selector counts whether or not it is called, so
// taking a method value is a use.
func TestExecportal_EveryOrderPathHasFixedCallers(t *testing.T) {
	allowedCallers := map[string]map[string]bool{
		"PlaceOrder":  {"portal.sendSquare": true},
		"CancelOrder": {},
		"NewOpener":   {"portal.openAs": true, "portal.close": true},
		"sendSquare":  {"portal.reconcile": true},
		"reconcile":   {"portal.handleReconcile": true},
		"open":        {"portal.handleOpen": true},
		"openAs":      {"portal.open": true, "portalTrader.Open": true},
		"close":       {"portal.handleClose": true, "portalTrader.Close": true},
		// The signed and raw transport: an order can be sent through it without
		// any of the functions above.
		"PostSigned": {}, "DeleteSigned": {}, "GetSigned": {}, "GetPublic": {},
		// The write handlers wrap the functions above and skip every header wall
		// when called directly: they may only be registered as routes.
		"handleOpen": {"portal.handler": true}, "handleClose": {"portal.handler": true}, "handleReconcile": {"portal.handler": true},
		"handleAutotradeStart": {"portal.handler": true}, "handleAutotradeStop": {"portal.handler": true}, "handleAutotradeKill": {"portal.handler": true},
		"handleAutotradeClosePair": {"portal.handler": true}, "handleAutotradePair": {"portal.handler": true},
		"handleAutotradeAckAll": {"portal.handler": true},
		// Engine 2 (PLAN 4.5k step 4). Its order paths are pinned exactly like
		// Engine 1's: two functions reach crossperp.Engine, and only a route
		// handler or the pilot's own scan step may call them. The margin guard's
		// emergency close is deliberately NOT in this list — it calls the engine
		// from inside internal/risk, where its callers are pinned by that
		// package's own tests, because an emergency must not wait behind a page.
		"openPair":        {"portal.handleCrossOpen": true, "crossPilot.enterOne": true},
		"closePair":       {"portal.handleCrossClose": true, "crossPilot.exitOne": true},
		"handleCrossOpen": {"portal.handler": true}, "handleCrossClose": {"portal.handler": true},
		"handleCrossReconcile": {"portal.handler": true}, "handleCrossUnblock": {"portal.handler": true},
		"handleCrossPilot": {"portal.handler": true}, "handleCrossAckMargin": {"portal.handler": true},
		// The lock itself: Engine 1 asks in exactly two places, and the raw
		// coordinator calls that grant or mark a lock are named in exactly one.
		"acquireEngine1": {"portal.openAs": true},
		"releaseEngine1": {"portal.close": true},
		"TryAcquire":     {"portal.acquireEngine1": true},
		"MarkOrdersSent": {"portal.acquireEngine1": true},
		"Withdraw":       {"portal.acquireEngine1": true},
		// The desk and the engine behind it. A read handler that could also
		// reach Open or Close would be an order path no dialog guards.
		"cross": {"portal.crossOff": true, "portal.attachCross": true, "main": true,
			"portal.handleCrossStatus": true, "portal.handleCoordinatorLocks": true, "portal.handleRiskMargin": true,
			"portal.handleCrossOpen": true, "portal.handleCrossClose": true, "portal.handleCrossReconcile": true,
			"portal.handleCrossUnblock": true, "portal.handleCrossPilot": true, "portal.handleCrossAckMargin": true,
			"portal.acquireEngine1": true, "portal.releaseEngine1": true,
			"portal.buildMasterOverview": true, "portal.buildMasterPositions": true},
		"engine2": {"crossDesk.adoptPairs": true, "crossDesk.openPair": true, "crossDesk.closePair": true,
			"crossDesk.statusView": true, "crossDesk.unblockClose": true, "crossDesk.confirmOrders": true,
			"crossDesk.retryReleases": true, "newCrossDesk": true,
			"crossPilot.stopReasonVI": true, "crossPilot.exitOne": true, "crossPilot.enterOne": true},
		"marginGuard": {"newCrossDesk": true, "crossDesk.marginView": true, "crossDesk.acknowledgeMargin": true,
			"crossDesk.runMarginGuard": true, "crossPilot.stressedVenue": true},
		// The bot's Trader and Market wrap openAs and close; the one engine gets
		// the one pair, built in newAutotrade (review of Q18, round 2). The PnL
		// page reads its status; main's sampler reads it too.
		"autotrade": {"portal.handleAutotradeStatus": true, "portal.handleAutotradeStart": true, "portal.handleAutotradeStop": true, "portal.handleAutotradeKill": true,
			"portal.handleAutotradeClosePair": true, "portal.handleAutotradePair": true, "portal.handleAutotradePnL": true,
			"portal.handleAutotradeAckAll": true, "newPortal": true, "main": true,
			"portal.buildMasterOverview": true, "portal.buildMasterPositions": true},
	}
	// Types a value of which is an order path, and the only functions that may
	// name them outside their own methods.
	wrapperTypes := map[string]map[string]bool{
		"portalTrader": {"newAutotrade": true},
		"portalMarket": {"newAutotrade": true},
	}
	seen := map[string]int{}
	fset := token.NewFileSet()
	for _, path := range topLevelGoFiles(t, false) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			caller := "<outside any function>"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				caller = fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) == 1 {
					recv := fn.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					if id, ok := recv.(*ast.Ident); ok {
						caller = id.Name + "." + caller
					}
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if allowed, watched := wrapperTypes[id.Name]; watched {
						own := strings.HasPrefix(caller, id.Name+".")
						if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
							// Only the type's OWN name in its own declaration. An
							// alias (type x = portalTrader), a defined type over it or
							// a struct embedding it is a second way to hold the order
							// path under a name this test would not watch.
							for _, spec := range gd.Specs {
								if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name == id {
									own = true
								}
							}
						}
						if !own && !allowed[caller] {
							t.Errorf("%s: %s names %s — the bot's order path is built once, in newAutotrade (PLAN Q18)", fset.Position(id.Pos()), caller, id.Name)
						}
					}
					return true
				}
				// A request built by hand is how a handler gets called past the
				// walls; the main package serves requests, it never makes one.
				if lit, ok := n.(*ast.CompositeLit); ok {
					if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Request" {
						if x, ok := sel.X.(*ast.Ident); ok && x.Name == "http" {
							t.Errorf("%s: %s builds an http.Request literal", fset.Position(lit.Pos()), caller)
						}
					}
				}
				if call, ok := n.(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "new" && len(call.Args) == 1 {
						if sel, ok := call.Args[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Request" {
							t.Errorf("%s: %s allocates an http.Request", fset.Position(call.Pos()), caller)
						}
					}
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				allowed, watched := allowedCallers[sel.Sel.Name]
				if !watched {
					return true
				}
				seen[sel.Sel.Name]++
				if !allowed[caller] {
					t.Errorf("%s: %s uses .%s — only %v may; an order reaches the venue through a fixed set of functions (PLAN Q16, Q18)",
						fset.Position(sel.Pos()), caller, sel.Sel.Name, keys(allowed))
				}
				return true
			})
		}
	}
	for _, name := range []string{"PlaceOrder", "NewOpener", "sendSquare", "reconcile", "open", "openAs", "close",
		"openPair", "closePair", "acquireEngine1", "releaseEngine1", "TryAcquire", "MarkOrdersSent", "cross", "engine2", "marginGuard"} {
		if seen[name] == 0 {
			t.Errorf("no use of .%s found — the test is not reading the package it guards", name)
		}
	}
}

// declCaller names a declaration the way the order-path guards do:
// "portalTrader.Close", "main", or "<outside any function>".
func declCaller(decl ast.Decl) string {
	fn, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "<outside any function>"
	}
	caller := fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if id, ok := recv.(*ast.Ident); ok {
			caller = id.Name + "." + caller
		}
	}
	return caller
}

// The multi-pair engine's surface in the main package is pinned METHOD BY
// METHOD, not only by who names p.autotrade (review of the multi-pair change,
// 2026-09-15): a read handler that could also call PairControl or Kill, or a
// sampler that built a second engine, would be a path to an order no dialog
// guards. So:
//
//   - every use of the .autotrade field is the receiver of a method call, and
//     each caller may call only its own methods — a read handler only Status;
//   - the one exception is newPortal assigning it;
//   - newAutotrade is called by newPortal only, and autotrade.New by
//     newAutotrade only: one engine, one Trader, one Market;
//   - pnl.go may name the package's two status TYPES and nothing else.
func TestAutotrade_EachCallerReachesOnlyItsOwnEngineMethods(t *testing.T) {
	allowedMethods := map[string]map[string]bool{
		"portal.handleAutotradeStatus":    {"Status": true},
		"portal.handleAutotradePnL":       {"Status": true},
		"portal.buildMasterOverview":      {"Status": true},
		"portal.buildMasterPositions":     {"Status": true},
		"portal.handleAutotradeStart":     {"Start": true},
		"portal.handleAutotradeStop":      {"Stop": true},
		"portal.handleAutotradeKill":      {"Kill": true},
		"portal.handleAutotradeClosePair": {"ClosePair": true},
		"portal.handleAutotradePair":      {"PairControl": true},
		"portal.handleAutotradeAckAll":    {"AckAll": true},
		"main":                            {"Run": true, "Start": true, "Status": true},
	}
	pnlMayName := map[string]bool{"StatusView": true, "PositionView": true}
	fset := token.NewFileSet()
	methods, builds := 0, 0
	for _, path := range topLevelGoFiles(t, false) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			caller := declCaller(decl)
			qualified := map[*ast.SelectorExpr]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "autotrade" {
					qualified[inner] = true
					methods++
					if !allowedMethods[caller][sel.Sel.Name] {
						t.Errorf("%s: %s calls .autotrade.%s — it may call only %v", fset.Position(sel.Pos()), caller, sel.Sel.Name, keys(allowedMethods[caller]))
					}
				}
				return true
			})
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == "autotrade" && !qualified[x] && caller != "newPortal" {
						t.Errorf("%s: %s holds .autotrade without calling a pinned method on it", fset.Position(x.Pos()), caller)
					}
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "autotrade" {
						if x.Sel.Name == "New" {
							builds++
							if caller != "newAutotrade" {
								t.Errorf("%s: %s builds an engine — only newAutotrade may", fset.Position(x.Pos()), caller)
							}
						}
						if filepath.Base(path) == "pnl.go" && !pnlMayName[x.Sel.Name] {
							t.Errorf("%s: pnl.go names autotrade.%s — it reads status types only", fset.Position(x.Pos()), x.Sel.Name)
						}
					}
				case *ast.Ident:
					if x.Name == "newAutotrade" && caller != "newPortal" && !(fn != nil && fn.Name == x) {
						t.Errorf("%s: %s names newAutotrade — only newPortal builds the engine", fset.Position(x.Pos()), caller)
					}
				}
				return true
			})
		}
	}
	if methods < 8 || builds != 1 {
		t.Fatalf("saw %d engine method calls and %d engine builds — the test is not reading the package it guards", methods, builds)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The whole-binary check the AST test cannot make: what the LINKER puts in.
func TestExecportal_BinaryLinksNoScannerAndNoDatabase(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "./cmd/execportal")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/execportal: %v", err)
	}
	deps := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		deps[strings.TrimSpace(line)] = true
	}
	if !deps["futures-arbitrage-scanner/internal/execution"] {
		t.Fatal("the dependency list does not even contain internal/execution — the test is not reading the real build")
	}
	for _, bad := range []string{
		"futures-arbitrage-scanner/cmd/scanner",
		"futures-arbitrage-scanner/internal/scanner",
		"futures-arbitrage-scanner/internal/store",
		"futures-arbitrage-scanner/internal/history",
		"futures-arbitrage-scanner/internal/paper",
		"futures-arbitrage-scanner/internal/config",
		"futures-arbitrage-scanner/internal/crowding",
		"futures-arbitrage-scanner/internal/backtest",
		"modernc.org/sqlite",
		"database/sql",
	} {
		if deps[bad] {
			t.Errorf("cmd/execportal links %s", bad)
		}
	}
}

// The intent files are SHARED with cmd/execcheck, so the two structs must be
// the same shape — names, JSON tags and types. execcheck is a main package and
// cannot be imported, so its source is read.
func TestExecportal_IntentStateHasExeccheckShape(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "execcheck", "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	theirs := map[string]string{}
	theirStateDir := ""
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.TypeSpec:
			if x.Name.Name != "state" {
				return true
			}
			st, ok := x.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, f := range st.Fields.List {
				tag := ""
				if f.Tag != nil {
					raw, _ := strconv.Unquote(f.Tag.Value)
					tag = reflect.StructTag(raw).Get("json")
				}
				for _, name := range f.Names {
					theirs[name.Name] = tag + " " + types(f.Type)
				}
			}
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if name.Name == "stateDir" && i < len(x.Values) {
					if lit, ok := x.Values[i].(*ast.BasicLit); ok {
						theirStateDir, _ = strconv.Unquote(lit.Value)
					}
				}
			}
		}
		return true
	})
	if len(theirs) < 20 {
		t.Fatalf("read only %d fields of execcheck's state — the test is not seeing the struct", len(theirs))
	}

	ours := map[string]string{}
	rt := reflect.TypeOf(intentState{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		ours[f.Name] = f.Tag.Get("json") + " " + f.Type.String()
	}
	for name, want := range theirs {
		if got, ok := ours[name]; !ok {
			t.Errorf("execcheck's state has %s (%s); intentState does not — execcheck would lose it on the next write", name, want)
		} else if got != want {
			t.Errorf("%s: intentState is %q, execcheck's state is %q", name, got, want)
		}
	}
	for name, got := range ours {
		if _, ok := theirs[name]; !ok {
			t.Errorf("intentState has %s (%s) that execcheck's state does not — execcheck would drop it when it re-saves the file", name, got)
		}
	}
	if theirStateDir != stateDir {
		t.Errorf("execcheck writes intents to %q, the portal to %q — the two tools would not see each other", theirStateDir, stateDir)
	}
	if !strings.HasPrefix(stateDir, ".paper/") {
		t.Errorf("stateDir is %q, want somewhere under the gitignored .paper/", stateDir)
	}
}

func types(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

// The cache is JSON files and must never become a schema: the gate has
// data/scanner.db open for writing and no migration may move under it.
func TestExecportal_WritesNoDatabase(t *testing.T) {
	src := readOwnSource(t, false)
	for _, bad := range []string{"CREATE TABLE", "ALTER TABLE", "PRAGMA ", "data/scanner.db\""} {
		if strings.Contains(src, bad) {
			t.Errorf("this command contains %q; it must write nothing but JSON files under %s", bad, stateDir)
		}
	}
}

// It listens on loopback and nowhere else.
func TestCheckBind_OnlyLoopbackLiterals(t *testing.T) {
	for _, ok := range []string{"127.0.0.1", "::1", "127.0.0.2", " 127.0.0.1 "} {
		if _, err := checkBind(ok); err != nil {
			t.Errorf("checkBind(%q) refused a loopback address: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0", "::", "", "localhost", "192.168.1.10", "10.0.0.1", "8.8.8.8", "fe80::1", "example.com"} {
		if _, err := checkBind(bad); err == nil {
			t.Errorf("checkBind(%q) accepted a non-loopback bind", bad)
		}
	}
}

func TestCheckPort_RefusesTheGateAndNonsense(t *testing.T) {
	if err := checkPort("8087"); err != nil {
		t.Errorf("8087 refused: %v", err)
	}
	for _, bad := range []string{"8085", "8082", "8086", "80", "70000", "abc", ""} {
		if err := checkPort(bad); err == nil {
			t.Errorf("checkPort(%q) accepted", bad)
		}
	}
}

// Package static is linked into this binary but lives outside cmd/execportal,
// so none of the per-file walks above read it. It is held to less than the
// command: embedded files and one accessor. No import beyond embed and io/fs,
// no other function (an init included), no sub-package — a helper that grew
// there would be a path into the order binary no guard here looks at.
func TestStatic_IsEmbeddedFilesAndNothingElse(t *testing.T) {
	allowedImports := map[string]bool{"embed": true, "io/fs": true}
	var goFiles []string
	err := filepath.WalkDir(staticDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") {
			goFiles = append(goFiles, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(goFiles) == 0 {
		t.Fatal("no Go file under static/ — the test is not reading the package")
	}
	// The go tool also compiles and links assembly, C and prebuilt objects
	// (.s, .c, .syso …) found beside a package's Go files, and a .s body for FS
	// would pass every check below. So the package directory holds an allow-list
	// of files, not a deny-list of extensions; names starting with "." or "_"
	// are ignored by the go tool and by go:embed alike.
	top, err := os.ReadDir(staticDir)
	if err != nil {
		t.Fatal(err)
	}
	allowedTopFiles := map[string]bool{"embed.go": true, "index.html": true}
	for _, entry := range top {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		if !allowedTopFiles[name] {
			t.Errorf("static/%s: the package directory holds embed.go and index.html only — anything else beside them may be compiled into the order binary", name)
		}
	}
	for _, path := range goFiles {
		if filepath.Dir(path) != filepath.Clean(staticDir) {
			t.Errorf("%s: static/ holds no sub-package", path)
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); !allowedImports[imported] {
				t.Errorf("%s imports %q — package static embeds files and imports nothing else", path, imported)
			}
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name != "FS" || d.Recv != nil {
					t.Errorf("%s declares func %s — package static has one accessor, FS", path, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					var names []*ast.Ident
					switch s := spec.(type) {
					case *ast.ValueSpec:
						names = s.Names
					case *ast.TypeSpec:
						names = []*ast.Ident{s.Name}
					}
					for _, name := range names {
						if name.IsExported() {
							t.Errorf("%s exports %s — package static exports FS only", path, name.Name)
						}
					}
				}
			}
		}
	}
}

// ownGoFiles is every Go file of the command AND its sub-packages, so a new
// package under cmd/execportal/ is read by the same tests as the command.
func ownGoFiles(t *testing.T, withTests bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != "." && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || (!withTests && strings.HasSuffix(path, "_test.go")) {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// topLevelGoFiles is the main package only.
func topLevelGoFiles(t *testing.T, withTests bool) []string {
	t.Helper()
	var out []string
	for _, path := range ownGoFiles(t, withTests) {
		if !strings.Contains(path, string(filepath.Separator)) {
			out = append(out, path)
		}
	}
	return out
}

func readOwnSource(t *testing.T, withTests bool) string {
	t.Helper()
	var all strings.Builder
	for _, name := range ownGoFiles(t, withTests) {
		blob, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(blob)
	}
	if all.Len() == 0 {
		t.Fatal("no source was read, so this test would pass on an empty package")
	}
	return all.String()
}
