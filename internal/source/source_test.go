package source

import "testing"

func TestParsePosition(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
		ok    bool
	}{
		{"3", 3, true}, {" 0.5 ", 0.5, true}, {"0", 0, true},
		{"1e1", 10, true}, {"-1", -1, true},
		{"", 0, false}, {"1-3", 0, false}, {"Book 2", 0, false},
		{"1e999", 0, false},
	} {
		if got, ok := ParsePosition(tc.value); got != tc.want || ok != tc.ok {
			t.Errorf("ParsePosition(%q) = (%v, %t), want (%v, %t)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}
