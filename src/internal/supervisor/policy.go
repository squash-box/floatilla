package supervisor

import (
	"regexp"
	"slices"

	"google.golang.org/protobuf/proto"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

type Decision int

const (
	Deny Decision = iota
	Allow
	Ask
)

// Decide applies a permission policy to a tool. Deny wins over allow and
// ask; tools the policy doesn't mention are denied. In yolo mode tools the
// policy would ask about are allowed without asking; denials still hold.
func Decide(p *v1.PermissionPolicy, tool string, yolo bool) Decision {
	switch {
	case slices.Contains(p.GetDeny(), tool):
		return Deny
	case slices.Contains(p.GetAllow(), tool):
		return Allow
	case slices.Contains(p.GetAsk(), tool) && yolo:
		return Allow
	case slices.Contains(p.GetAsk(), tool):
		return Ask
	default:
		return Deny
	}
}

// price is USD per million tokens.
type price struct{ input, output, cacheRead, cacheWrite float64 }

// prices for cost metering. Cache writes are billed at 1.25× input (5-minute
// TTL), cache reads at the published rate.
var prices = map[string]price{
	"claude-fable-5-1":  {10, 50, 1, 12.5},
	"claude-opus-5-5":   {4, 20, 0.20, 5},
	"claude-sonnet-5-5": {2, 10, 0.20, 2.5},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01, 0.125},
}

var (
	variantSuffix = regexp.MustCompile(`\[[^\]]*\]$`) // claude-opus-5-5[1m]
	dateSuffix    = regexp.MustCompile(`-\d{8}$`)     // claude-opus-5-5-20261001
)

// canonical maps a model ID variant onto its price-table key by stripping a
// trailing "[...]" and then a trailing "-YYYYMMDD".
func canonical(model string) string {
	return dateSuffix.ReplaceAllString(variantSuffix.ReplaceAllString(model, ""), "")
}

// Cost returns the USD cost of u on model; unknown models cost 0.
func Cost(model string, u *v1.Usage) float64 {
	p, ok := prices[canonical(model)]
	if !ok {
		return 0
	}
	return (float64(u.GetInputTokens())*p.input +
		float64(u.GetOutputTokens())*p.output +
		float64(u.GetCacheReadTokens())*p.cacheRead +
		float64(u.GetCacheWriteTokens())*p.cacheWrite) / 1e6
}

// Price meters a usage delta from the agent. Subscription tokens aren't
// billed per token, so they're priced at list rates as indicative cost.
// Whatever cost the agent sent is ignored.
func Price(d *v1.UsageDelta) *v1.UsageDelta {
	u := &v1.Usage{}
	if d.GetUsage() != nil {
		u = proto.Clone(d.GetUsage()).(*v1.Usage)
	}
	u.CostUsd, u.IndicativeCostUsd = 0, 0
	c := Cost(d.GetModel(), u)
	if d.GetSubscription() {
		u.IndicativeCostUsd = c
	} else {
		u.CostUsd = c
	}
	return &v1.UsageDelta{Model: d.GetModel(), Usage: u, Subscription: d.GetSubscription()}
}
