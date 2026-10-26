package tui

import (
	"math"
	"testing"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

func TestFmtCost(t *testing.T) {
	for _, c := range []struct {
		billed, indicative float64
		want               string
	}{
		{1.5, 0, "$1.50"},
		{0, 2.1, "~$2.10"},
		{0.4, 2.1, "~$2.50"},
		{0, 0, "$0.00"},
	} {
		if got := fmtCost(c.billed, c.indicative); got != c.want {
			t.Errorf("fmtCost(%v, %v) = %q, want %q", c.billed, c.indicative, got, c.want)
		}
	}
}

func TestCostLabel(t *testing.T) {
	for _, c := range []struct {
		u    *v1.Usage
		want string
	}{
		{nil, "$0.00"},
		{&v1.Usage{CostUsd: 1.5}, "$1.50"},
		{&v1.Usage{IndicativeCostUsd: 2.1}, "~$2.10"},
		{&v1.Usage{CostUsd: 0.4, IndicativeCostUsd: 2.1}, "~$2.50"},
	} {
		if got := CostLabel(c.u); got != c.want {
			t.Errorf("CostLabel(%v) = %q, want %q", c.u, got, c.want)
		}
	}
}

func TestTotals(t *testing.T) {
	tokens, billed, indicative := Totals([]*v1.Task{
		{Usage: &v1.Usage{InputTokens: 100, OutputTokens: 10, CostUsd: 0.5}},
		{Usage: &v1.Usage{CacheReadTokens: 1000, CacheWriteTokens: 200, IndicativeCostUsd: 1.25}},
		{Usage: &v1.Usage{InputTokens: 5, CostUsd: 0.25, IndicativeCostUsd: 0.75}},
		{},
	})
	if tokens != 1315 || math.Abs(billed-0.75) > 1e-9 || math.Abs(indicative-2) > 1e-9 {
		t.Fatalf("Totals = %d, %v, %v; want 1315, 0.75, 2", tokens, billed, indicative)
	}
}
