package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	for _, pairs := range [][]broker.EnvPair{futuresEnv, spotEnv} {
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

// Q15 limit 2 and limit 5, by machine: no import of the scanner, the store, the
// journal or the strategy's decisions, and no call to EvaluateEntry or
// EvaluateExit. execution links internal/strategy for EstimateFill, so the
// binary contains the package; what is forbidden is THIS command reaching for
// a decision.
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
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
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
	if files < 5 {
		t.Fatalf("only %d files parsed — the test is not seeing the package", files)
	}
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

func ownGoFiles(t *testing.T, withTests bool) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !withTests && strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
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
