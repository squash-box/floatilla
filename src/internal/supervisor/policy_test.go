package supervisor

import (
	"math"
	"testing"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

func TestDecide(t *testing.T) {
	p := &v1.PermissionPolicy{
		Allow: []string{"read_file", "bash"},
		Ask:   []string{"bash", "write_file"},
		Deny:  []string{"bash"},
	}
	cases := map[string]Decision{
		"read_file":  Allow,
		"write_file": Ask,
		"bash":       Deny, // deny wins
		"grep":       Deny, // unlisted
	}
	for tool, want := range cases {
		if got := Decide(p, tool); got != want {
			t.Errorf("Decide(%s) = %v, want %v", tool, got, want)
		}
	}
}

func TestCost(t *testing.T) {
	u := &v1.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 2_000_000}
	// 4 + 2 + 0.4
	if got := Cost("claude-opus-5-5", u); math.Abs(got-6.4) > 1e-9 {
		t.Fatalf("cost = %v", got)
	}
	if Cost("scripted", u) != 0 {
		t.Fatal("unknown model should cost 0")
	}
}
