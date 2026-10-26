package supervisor

import (
	"math"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

func TestDecide(t *testing.T) {
	p := &v1.PermissionPolicy{
		Allow: []string{"read_file", "bash"},
		Ask:   []string{"bash", "write_file"},
		Deny:  []string{"bash"},
	}
	cases := map[string][2]Decision{ // {normal, yolo}
		"read_file":  {Allow, Allow},
		"write_file": {Ask, Allow},
		"bash":       {Deny, Deny}, // deny wins, yolo or not
		"grep":       {Deny, Deny}, // unlisted
	}
	for tool, want := range cases {
		for i, yolo := range []bool{false, true} {
			if got := Decide(p, tool, yolo); got != want[i] {
				t.Errorf("Decide(%s, yolo=%v) = %v, want %v", tool, yolo, got, want[i])
			}
		}
	}
}

func TestCost(t *testing.T) {
	u := &v1.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 2_000_000}
	// 4 + 2 + 0.4
	if got := Cost("claude-opus-5-5", u); math.Abs(got-6.4) > 1e-9 {
		t.Fatalf("cost = %v", got)
	}
	// Claude Code reports long-context and dated variants of the model ID.
	for _, m := range []string{"claude-opus-5-5[1m]", "claude-opus-5-5-20261001", "claude-opus-5-5-20261001[1m]"} {
		if got := Cost(m, u); math.Abs(got-6.4) > 1e-9 {
			t.Errorf("Cost(%s) = %v, want 6.4", m, got)
		}
	}
	for _, m := range []string{"scripted", "claude-opus-5-5 (subscription)"} {
		if got := Cost(m, u); got != 0 {
			t.Errorf("Cost(%s) = %v, unknown model should cost 0", m, got)
		}
	}
}

func TestPrice(t *testing.T) {
	usage := func() *v1.Usage {
		return &v1.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 2_000_000}
	}
	withCost := usage()
	withCost.CostUsd, withCost.IndicativeCostUsd = 99, 42
	cases := []struct {
		name                string
		in                  *v1.UsageDelta
		wantCost, wantIndic float64
	}{
		{"billed", &v1.UsageDelta{Model: "claude-opus-5-5", Usage: usage()}, 6.4, 0},
		{"subscription", &v1.UsageDelta{Model: "claude-opus-5-5", Usage: usage(), Subscription: true}, 0, 6.4},
		{"billed ignores agent cost", &v1.UsageDelta{Model: "claude-opus-5-5", Usage: withCost}, 6.4, 0},
		{"subscription ignores agent cost", &v1.UsageDelta{Model: "claude-opus-5-5", Usage: withCost, Subscription: true}, 0, 6.4},
		{"nil usage", &v1.UsageDelta{Model: "claude-opus-5-5", Subscription: true}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := proto.Clone(c.in).(*v1.UsageDelta)
			got := Price(c.in)
			if !proto.Equal(c.in, before) {
				t.Fatalf("input mutated: %v, was %v", c.in, before)
			}
			u := got.GetUsage()
			if math.Abs(u.GetCostUsd()-c.wantCost) > 1e-9 || math.Abs(u.GetIndicativeCostUsd()-c.wantIndic) > 1e-9 {
				t.Fatalf("cost = %v, indicative = %v; want %v, %v", u.GetCostUsd(), u.GetIndicativeCostUsd(), c.wantCost, c.wantIndic)
			}
			if got.GetModel() != c.in.GetModel() || got.GetSubscription() != c.in.GetSubscription() {
				t.Fatalf("delta = %v, want model %q subscription %v", got, c.in.GetModel(), c.in.GetSubscription())
			}
			in := c.in.GetUsage()
			if u.GetInputTokens() != in.GetInputTokens() || u.GetOutputTokens() != in.GetOutputTokens() ||
				u.GetCacheReadTokens() != in.GetCacheReadTokens() || u.GetCacheWriteTokens() != in.GetCacheWriteTokens() {
				t.Fatalf("tokens = %v, want %v", u, in)
			}
		})
	}
}
