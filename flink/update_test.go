package main

import "testing"

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{{"v1.1.1", "1.1.0", true}, {"1.2.0", "1.1.9", true}, {"v2.0.0", "1.9.9", true}, {"1.1.0", "1.1.0", false}, {"1.0.9", "1.1.0", false}, {"v1.10.0", "1.9.0", true}}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q,%q)=%v", c.a, c.b, got)
		}
	}
}
