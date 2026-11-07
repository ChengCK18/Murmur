package alert

import "testing"

func TestContains(t *testing.T) {
	square := []Point{{0, 0}, {0, 1}, {1, 1}, {1, 0}}
	notch := []Point{{0, 0}, {0, 2}, {2, 2}, {2, 0}, {1, 1}} // the bottom edge dented in
	cases := []struct {
		poly []Point
		p    Point
		in   bool
	}{
		{square, Point{0.5, 0.5}, true},
		{square, Point{1.5, 0.5}, false},
		{square, Point{-0.1, 0.5}, false},
		{notch, Point{1.5, 1}, true},
		{notch, Point{1, 1.5}, true},
		{notch, Point{0.5, 1}, true},
		{notch, Point{1, 0.5}, false}, // in the dent
		{[]Point{{-35.37, 149.16}, {-35.36, 149.16}, {-35.36, 149.17}, {-35.37, 149.17}}, Point{-35.365, 149.165}, true},
	}
	for _, c := range cases {
		if got := Contains(c.poly, c.p); got != c.in {
			t.Errorf("%v in %v: %v, want %v", c.p, c.poly, got, c.in)
		}
	}
}
