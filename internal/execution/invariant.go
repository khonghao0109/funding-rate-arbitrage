package execution

import (
	"fmt"
	"math"
)

// The invariant, as one function, so that planEntry, Open and the property test
// all ask exactly the same question.
//
// Writing it three times is how three slightly different invariants come to
// exist, and the one that matters — the one Open actually enforces — is then
// not the one the tests check.

// pairInvariant is the "both open" arm: what it takes for two filled
// quantities to count as a hedged pair.
type pairInvariant struct {
	// ToleranceQtyCoin is the COARSER of the two venues' step sizes. It is the
	// closest two orders can possibly be, so it is the tightest tolerance that
	// is achievable rather than merely desirable.
	ToleranceQtyCoin float64

	SpotPriceQuote float64
	PerpPriceQuote float64

	SpotMinNotionalQuote float64
	PerpMinNotionalQuote float64
}

// check reports whether two filled quantities form a hedged pair, and says
// precisely why not when they do not.
//
// TWO conditions, and they are independent:
//
//  1. the quantities differ by no more than the coarser step — the grids
//     cannot do better than that;
//  2. the residual is worth LESS than the minimum notional on BOTH venues —
//     i.e. it is too small for either venue to let us trade it away even if we
//     wanted to.
//
// The second is not implied by the first. One perp step of BTC is 0.0001, which
// at $60,000 is $6 — under the perp's own $50 minimum but OVER the spot
// venue's $5 one. A residual can therefore sit inside the step tolerance and
// still be a position somebody could close, which means it is a position that
// is really there. In practice both hold trivially, because
// instruments.SizeDeltaNeutral refuses incommensurable grids outright and the
// residual comes out at exactly zero; this function exists for the case where
// that stops being true.
func (p pairInvariant) check(spotQtyCoin, perpQtyCoin float64) error {
	residual := math.Abs(spotQtyCoin - perpQtyCoin)
	if residual > p.ToleranceQtyCoin+gridEpsilon {
		return fmt.Errorf("hai chân lệch %.10g coin, quá dung sai %.10g (bước thô hơn của hai sàn)",
			residual, p.ToleranceQtyCoin)
	}
	if residual == 0 {
		return nil
	}
	if v := residual * p.SpotPriceQuote; p.SpotMinNotionalQuote > 0 && v >= p.SpotMinNotionalQuote {
		return fmt.Errorf("phần dư %.10g coin trị giá %.4f trên sàn spot, không dưới mức tối thiểu %.4f — đó là một vị thế đóng được, tức là một vị thế có thật",
			residual, v, p.SpotMinNotionalQuote)
	}
	if v := residual * p.PerpPriceQuote; p.PerpMinNotionalQuote > 0 && v >= p.PerpMinNotionalQuote {
		return fmt.Errorf("phần dư %.10g coin trị giá %.4f trên sàn perp, không dưới mức tối thiểu %.4f",
			residual, v, p.PerpMinNotionalQuote)
	}
	return nil
}

// flat reports whether both legs hold nothing.
//
// Exact zero, deliberately. A "close enough to zero" threshold here would be a
// second, softer invariant hiding behind the first, and the quantity a venue
// reports is a decimal string it chose — not the result of arithmetic that
// could leave dust.
func flat(spotQtyCoin, perpQtyCoin float64) bool {
	return spotQtyCoin == 0 && perpQtyCoin == 0
}

// classify turns two filled quantities into the outcome, or into an error when
// they are neither hedged nor flat — which is the state this package exists to
// make impossible.
//
// "Both open" requires BOTH legs to hold something. That sounds too obvious to
// write down, and it is exactly what the property test caught missing: an
// earlier version asked only whether the two quantities were CLOSE, so a pair
// of (0, 0.00007) — one leg flat and the other holding dust — was reported as
// hedged, because 0.00007 is indeed within one step of 0. It is not hedged. It
// is a small naked position, and small naked positions are how an account
// accumulates a drawer full of them.
func (p pairInvariant) classify(spotQtyCoin, perpQtyCoin float64) (Outcome, error) {
	if flat(spotQtyCoin, perpQtyCoin) {
		return OutcomeBothFlat, nil
	}
	if spotQtyCoin <= 0 || perpQtyCoin <= 0 {
		return "", fmt.Errorf("%w: spot %.10g, perp %.10g — một chân bằng 0 còn chân kia thì không, đó là vị thế trần chứ không phải phòng hộ",
			ErrUnwindIncomplete, spotQtyCoin, perpQtyCoin)
	}
	if err := p.check(spotQtyCoin, perpQtyCoin); err != nil {
		return "", fmt.Errorf("%w: spot %.10g, perp %.10g: %s", ErrUnwindIncomplete, spotQtyCoin, perpQtyCoin, err.Error())
	}
	return OutcomeBothOpen, nil
}
