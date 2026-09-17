package broker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The boundary between public market data and credentials, checked by machine
// rather than by review.
//
// CLAUDE.md states it twice — "exchanges/ must NOT import any internal/
// package" and "internal/broker/ must NOT be reachable from the data ingestion
// path" — and internal/backtest already shows the house style for enforcing a
// rule like this: two AST tests, one requiring what must be there and one
// forbidding what must not. The same reasoning applies with more at stake. The
// failure this guards against is silent: someone adds a signed call to a venue
// package "just to read the leverage bracket", and from then on the scanner
// binary that runs unattended for a fortnight on port 8085 links a package
// holding API keys.

// (a) Nothing under exchanges/ may import internal/broker — or, as CLAUDE.md
// puts it more broadly, any internal/ package at all. The broader rule is
// asserted because it is the rule actually in force and it holds today
// (measured: zero files), and a test asserting a subset of a rule is a test
// that will one day pass while the rule is broken.
func TestExchangesImportsNoInternalPackageAndCertainlyNotThisOne(t *testing.T) {
	root := filepath.Join("..", "..", "exchanges")
	fset := token.NewFileSet()

	var offences []string
	walked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		walked++
		// _test.go files are included on purpose: a test that imports the
		// broker links it into a binary too, and is how the import arrives in
		// production a week later.
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			if imported := strings.Trim(spec.Path.Value, `"`); strings.HasPrefix(imported, "futures-arbitrage-scanner/internal/") {
				offences = append(offences, path+" imports "+imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if walked < 50 {
		t.Fatalf("only %d files were parsed under %s — the walk is not seeing the tree, so a clean result proves nothing", walked, root)
	}
	for _, o := range offences {
		t.Errorf("%s — exchanges/ is public market data and must not reach into internal/; if it is internal/broker, it has just put credentials on the ingestion path", o)
	}
}

// (b) The whole-binary check, which the AST test cannot make: an import three
// packages deep is still an import, and the question that matters is what the
// LINKER put into the binary the step-3.5 gate runs unattended.
//
// go list -deps answers exactly that, transitively, for the real build.
func TestNoCommandBinaryLinksTheBroker(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	root := filepath.Join("..", "..")

	// The command list is READ FROM DISK rather than typed out, so a new
	// binary cannot escape this test by nobody remembering to add it. The
	// exceptions are named, and naming them is the point: they are the only
	// binaries allowed to hold a credential or to place an order.
	allowed := map[string]string{
		"brokercheck": "step 4.1/4.2 diagnostic — the one command that reads a balance and places a test order",
		"execcheck":   "step 4.4b/4.5 diagnostic — opens and closes a position from the terminal (PLAN Q15)",
		"execportal":  "strategy 1 execution portal — web portal for testnet demo execution (PLAN Q16)",
		"bybitcheck":  "step 4.5i diagnostic — reads clock, wallet, one position and key permissions on Bybit testnet/demo; places no order",
	}
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatalf("read cmd/: %v", err)
	}
	// Both packages, not just the broker: internal/execution places orders
	// through it, so a binary linking execution reaches the venue even if it
	// never names the broker itself.
	forbidden := []string{
		"futures-arbitrage-scanner/internal/broker",
		"futures-arbitrage-scanner/internal/execution",
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if reason, ok := allowed[e.Name()]; ok {
			t.Logf("cmd/%s is exempt: %s", e.Name(), reason)
			continue
		}
		checked++
		t.Run(e.Name(), func(t *testing.T) {
			cmd := exec.Command("go", "list", "-deps", "./cmd/"+e.Name())
			cmd.Dir = root
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list -deps ./cmd/%s: %v", e.Name(), err)
			}
			for _, line := range strings.Split(string(out), "\n") {
				for _, bad := range forbidden {
					if strings.TrimSpace(line) == bad {
						t.Fatalf("cmd/%s links %s — the binary now reaches a venue with a credential, "+
							"and step 3.5's gate process runs unattended for a fortnight", e.Name(), bad)
					}
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no command was checked; the exemption list has swallowed the whole cmd/ tree")
	}
}

// (c) The testnet guard, both directions. A guard tested only on what it lets
// through is a guard nobody has tested.
func TestNewClient_AcceptsOnlyTheDocumentedTestnetHosts(t *testing.T) {
	accept := []string{
		BinanceFuturesTestnetBaseURL,
		BinanceSpotTestnetBaseURL,
		"https://demo-fapi.binance.com/",     // trailing slash
		"https://testnet.binance.vision:443", // explicit port
		"  https://demo-fapi.binance.com  ",  // pasted with whitespace
	}
	refuse := map[string]string{
		"https://fapi.binance.com":                    "the PRODUCTION futures host",
		"https://api.binance.com":                     "the PRODUCTION spot host",
		"https://demo-fapi.binance.com.evil.example":  "a suffix that only looks like the host",
		"https://notdemo-fapi.binance.com":            "a prefix that only looks like the host",
		"https://testnet.binance.vision.evil.example": "the spot host with something appended",
		"http://demo-fapi.binance.com":                "plaintext — a signed request over http publishes the credential",
		"https://testnet.binancefuture.com":           "a host no reachable official page documents (rule 5)",
		"":                                            "nothing at all",
		"://broken":                                   "an unparseable URL",
		"https://u:p@demo-fapi.binance.com":           "userinfo — a credential inside the URL",
		"https://u:p@demo-fapi.binance.com/%zz":       "userinfo in a URL that does not parse",
		"https://demo-fapi.binance.com:8443":          "a port the testnet does not serve",
		"https://demo-fapi.binance.com/?x=1":          "a query, which the request pin would drop",
		"https://demo-fapi.binance.com/#f":            "a fragment",
	}
	creds := Credentials{APIKey: NewSecret("k"), APISecret: NewSecret("s")}
	build := func(base string) error {
		_, err := NewClient(Config{
			BaseURL: base, Credentials: creds,
			TimePath: BinanceFuturesTimePath, WeightLimitPerMin: BinanceFuturesWeightPerMin,
		})
		return err
	}

	for _, base := range accept {
		if err := build(base); err != nil {
			t.Errorf("NewClient refused the documented testnet URL %q: %v", base, err)
		}
	}
	for base, why := range refuse {
		err := build(base)
		if err == nil {
			t.Errorf("NewClient ACCEPTED %q (%s) — at step 4.1 there is no host but testnet", base, why)
		} else if strings.Contains(err.Error(), "u:p") {
			t.Errorf("the refusal of %q echoes its userinfo: %v", base, err)
		}
	}
}

// There is no flag, field or environment variable that widens the guard.
//
// Checked on the package's own source, crudely and on purpose: the way this
// rule dies is not someone deleting checkTestnetBaseURL, it is someone adding
// `AllowMainnet bool` for one afternoon of debugging and leaving it. A
// substring check fires on a field, a constant, a flag name, or a comment
// proposing one.
func TestPackage_HasNoEscapeHatchForMainnet(t *testing.T) {
	forbidden := []string{"allowmainnet", "allowproduction", "skiphostcheck", "skiptestnet",
		"insecurehost", "unsafehost", "allowlive", "disableguard"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		// Only the package's real sources: this test file names the forbidden
		// strings itself.
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		lowered := strings.ToLower(string(raw))
		for _, bad := range forbidden {
			if strings.Contains(lowered, bad) {
				t.Errorf("%s mentions %q — step 4.6 introduces a mainnet host with the operator's decision and real capital behind it; 4.1 does not get a switch", name, bad)
			}
		}
	}
}

// Config.TestTransport runs BELOW the host pin, so whatever it does with a
// signed request — send it elsewhere, over plaintext, with TLS checks off — no
// wall in this package sees. NewClient refuses it outside a test binary; this
// makes the same rule visible in source, over EVERY non-test Go file in the
// module (no directory skipped: a main package under testdata/ still builds
// when named). It fails on:
//
//   - an identifier named TestTransport, except in the two files of this
//     package that define and install it and the refusal probe;
//   - a string literal containing it (reflect's FieldByName takes a string);
//   - a //go:linkname into this module (it can write an unexported variable);
//   - http.DefaultTransport or http.DefaultClient named by this package's own
//     code — process-wide variables anything linked in can rewire.
//
// Unkeyed Config literals cannot reach the field either: Config carries a
// blank field, so a literal from another package must use names.
func TestTestTransport_IsNamedByNoProductionFile(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		filepath.Join(root, "internal", "broker", "client.go"):                           true,
		filepath.Join(root, "internal", "broker", "redirect.go"):                         true,
		filepath.Join(root, "internal", "broker", "testdata", "refusalprobe", "main.go"): true,
	}
	brokerDir := filepath.Join(root, "internal", "broker") + string(filepath.Separator)
	var scanned int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			// A nested checkout (a worktree, an agent's copy) is another module
			// with its own internal/broker; it is not this module's source.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, "//go:linkname") && strings.Contains(c.Text, "futures-arbitrage-scanner/") {
					t.Errorf("%s: %s — a linkname into this module can rewrite what NewClient checks", rel, c.Text)
				}
			}
		}
		inBroker := strings.HasPrefix(path, brokerDir)
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "TestTransport" && !allowed[path] {
					t.Errorf("%s names TestTransport — a transport below the host pin belongs in a _test.go file only", rel)
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && strings.Contains(x.Value, "TestTransport") && !allowed[path] {
					t.Errorf("%s spells TestTransport in a string — reflect can set a field by name", rel)
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && inBroker && pkg.Name == "http" && (x.Sel.Name == "DefaultTransport" || x.Sel.Name == "DefaultClient") {
					t.Errorf("%s uses http.%s — the broker's transport is its own, never the process-wide one", rel, x.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 150 {
		t.Fatalf("scanned %d production files — the walk is not seeing the module", scanned)
	}
}
