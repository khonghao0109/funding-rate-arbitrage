package broker

import (
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
		if err := build(base); err == nil {
			t.Errorf("NewClient ACCEPTED %q (%s) — at step 4.1 there is no host but testnet", base, why)
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
