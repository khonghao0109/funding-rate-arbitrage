package exchanges

import "testing"

func TestSmallerPositiveCap(t *testing.T) {
	cases := []struct{ a, b, want float64 }{
		{0, 0, 0},     // neither stated → stays "not stated", never invented
		{0, 120, 120}, // 0 must not win over a real cap
		{1000, 0, 1000},
		{1000, 120, 120}, // the tighter ceiling governs
		{120, 1000, 120},
	}
	for _, c := range cases {
		if got := SmallerPositiveCap(c.a, c.b); got != c.want {
			t.Errorf("SmallerPositiveCap(%g, %g) = %g, want %g", c.a, c.b, got, c.want)
		}
	}
}
