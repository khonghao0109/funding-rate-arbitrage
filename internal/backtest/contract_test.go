package backtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// noTestFiles keeps the parse to the package's real sources: a rule declared in
// a _test.go file is not one the engine runs.
func noTestFiles(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }

// PLAN.md step 3.3 makes this an acceptance criterion in so many words: "engine
// dùng đúng hàm tín hiệu mà production sẽ dùng (kiểm bằng cách đọc import)".
//
// This is a TRIPWIRE on names, not a proof of behaviour: an engine could keep
// all four calls and still run a private pre-filter before them. The proof is
// TestRun_MatchesAStepByStepReplayThroughTheProductionRules in engine_test.go;
// this test exists so the cheap, obvious edit fails loudly too.
//
// It is checked mechanically rather than by reading, because the failure it
// guards against is silent and slow: someone inlines "just this one condition"
// to avoid a parameter, the two sides drift, and step 3.5 then reports a
// mismatch that no longer distinguishes a wrong strategy from two
// implementations that grew apart (PLAN Q8, §7.1).
func TestEngine_CallsTheProductionSignalFunctions(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", noTestFiles, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	imported, called := false, map[string]bool{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, spec := range file.Imports {
				if spec.Path.Value == `"futures-arbitrage-scanner/internal/strategy"` {
					imported = true
				}
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "strategy" {
					return true
				}
				called[sel.Sel.Name] = true
				return true
			})
			_ = path
		}
	}

	if !imported {
		t.Fatal("internal/backtest must import internal/strategy — PLAN Q8 forbids a second copy of the rules")
	}
	for _, fn := range []string{"EvaluateEntry", "EvaluateExit", "RoundTripCost", "UsableSettled"} {
		if !called[fn] {
			t.Errorf("engine never calls strategy.%s — the replay must run the production rule, not its own", fn)
		}
	}
}

// The other half of the same guarantee: the engine must not have grown its own
// copy of a rule it is supposed to be borrowing.
func TestEngine_DoesNotReimplementTheSignalRules(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", noTestFiles, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	// Names that would mean a condition had been re-grown here rather than
	// asked for. Deliberately the vocabulary of the signal layer.
	forbidden := []string{"checkPersistence", "checkNetAPR", "checkRateThreshold",
		"evaluateEntry", "evaluateExit", "checkLiquidity", "checkHedgeLeg"}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				for _, name := range forbidden {
					if strings.EqualFold(fn.Name.Name, name) {
						t.Errorf("internal/backtest declares %s — that rule belongs to internal/strategy, "+
							"and a second copy is exactly what step 3.5 cannot diagnose around", fn.Name.Name)
					}
				}
			}
		}
	}
}
