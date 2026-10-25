package supervisor

import (
	"slices"

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

// Cost returns the USD cost of u on model; unknown models cost 0.
func Cost(model string, u *v1.Usage) float64 {
	p, ok := prices[model]
	if !ok {
		return 0
	}
	return (float64(u.GetInputTokens())*p.input +
		float64(u.GetOutputTokens())*p.output +
		float64(u.GetCacheReadTokens())*p.cacheRead +
		float64(u.GetCacheWriteTokens())*p.cacheWrite) / 1e6
}
