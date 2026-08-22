package snapshot

import "testing"

func TestIsDegenerate(t *testing.T) {
	cases := []struct {
		n, last int
		want    bool
	}{
		{1, 35, true}, {11, 35, true}, {12, 35, false}, {35, 35, false},
		{1, 4, false}, // last not rich
		{0, 5, true}, {5, 5, false},
	}
	for _, c := range cases {
		if got := IsDegenerate(c.n, c.last, 5, 3); got != c.want {
			t.Errorf("IsDegenerate(%d,%d)=%v want %v", c.n, c.last, got, c.want)
		}
	}
}
