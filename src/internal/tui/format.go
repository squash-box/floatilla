package tui

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var (
	stInk    = Style{FG: Ink}
	stBold   = Style{FG: Ink, Bold: true}
	stMuted  = Style{FG: InkMuted}
	stFaint  = Style{FG: InkFaint}
	stLine   = Style{FG: Line}
	stAccent = Style{FG: Accent}
)

// look is how a status reads: glyph first, colour second (SPEC §1.2).
type look struct {
	Glyph string
	Tok   Token
	Label string
	Bold  bool
}

func statusLook(s v1.TaskStatus, frame int) look {
	switch s {
	case v1.TaskStatus_TASK_STATUS_RUNNING:
		return look{spinner[frame%len(spinner)], StatusRunning, "running", false}
	case v1.TaskStatus_TASK_STATUS_WAITING:
		return look{"◆", StatusWaiting, "waiting", true}
	case v1.TaskStatus_TASK_STATUS_FAILED:
		return look{"✗", StatusFailed, "failed", false}
	case v1.TaskStatus_TASK_STATUS_SUCCEEDED:
		return look{"✓", StatusSuccess, "done", false}
	case v1.TaskStatus_TASK_STATUS_PAUSED:
		return look{"‖", StatusPaused, "paused", false}
	case v1.TaskStatus_TASK_STATUS_PARKED:
		return look{"◇", StatusPaused, "parked", false}
	default:
		return look{"○", StatusPaused, "queued", false}
	}
}

func (l look) style() Style { return Style{FG: l.Tok, Bold: l.Bold} }

func statusLabel(s v1.TaskStatus) string { return statusLook(s, 0).Label }

// phase offsets each agent's spinner so they don't tick together (SPEC §10).
func phase(id string) int {
	h := 0
	for _, r := range id {
		h = h*31 + int(r)
	}
	return (h & 0x7fffffff) % len(spinner)
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func fmtCost(c float64) string { return fmt.Sprintf("$%.2f", c) }

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(max(0, d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// elapsed is how long a task has been (or was) running.
func elapsed(t *v1.Task, now time.Time) time.Duration {
	start := t.GetStartedAt()
	if start == nil {
		start = t.GetCreatedAt()
	}
	if start == nil {
		return 0
	}
	end := now
	if t.FinishedAt != nil {
		end = t.FinishedAt.AsTime()
	}
	return end.Sub(start.AsTime())
}

func taskTokens(t *v1.Task) int64 {
	u := t.GetUsage()
	return u.GetInputTokens() + u.GetOutputTokens() + u.GetCacheReadTokens() + u.GetCacheWriteTokens()
}

func stageName(spec *v1.ProcessTypeSpec, i int32) string {
	if spec == nil || int(i) >= len(spec.Stages) || i < 0 {
		return fmt.Sprintf("stage %d", i+1)
	}
	return spec.Stages[i]
}

// trackSpans draws the stage track: one segment per stage (SPEC §4.4).
func trackSpans(t *v1.Task, spec *v1.ProcessTypeSpec, segW int, level ColorLevel) []Span {
	n := 1
	if spec != nil {
		n = len(spec.Stages)
	}
	st := statusLook(t.Status, 0)
	done := t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED
	var spans []Span
	for i := 0; i < n; i++ {
		if i > 0 {
			spans = append(spans, Span{" ", Style{}})
		}
		switch {
		case done:
			spans = append(spans, Span{strings.Repeat("█", segW), Style{FG: StatusSuccess}})
		case int32(i) < t.StageIndex:
			spans = append(spans, Span{strings.Repeat("█", segW), stMuted})
		case int32(i) == t.StageIndex:
			fill := 1
			if t.StageProgress != nil {
				fill = max(1, int(math.Round(float64(*t.StageProgress)*float64(segW))))
			}
			fill = min(fill, segW)
			glyph := "█"
			if level == LevelNone {
				glyph = "▓"
			}
			spans = append(spans,
				Span{strings.Repeat(glyph, fill), Style{FG: st.Tok}},
				Span{strings.Repeat("░", segW-fill), stFaint})
		default:
			spans = append(spans, Span{strings.Repeat("░", segW), stFaint})
		}
	}
	return spans
}

// segWidth fits n segments with 1-col gaps into w columns, capped.
func segWidth(w, n, maxW int) int {
	if n <= 0 {
		return 1
	}
	return max(1, min(maxW, (w+1)/n-1))
}

var digit = regexp.MustCompile(`\d`)

// valueSpans colours fact values: +n / −n diff, ✓n / ✗n tests, ●n comments.
func valueSpans(v string) []Span {
	var out []Span
	for i, tok := range strings.Split(v, " ") {
		if i > 0 {
			out = append(out, Span{" ", stInk})
		}
		st := stInk
		switch {
		case strings.HasPrefix(tok, "+") && digit.MatchString(tok):
			st = Style{FG: DiffAddInk}
		case (strings.HasPrefix(tok, "−") || strings.HasPrefix(tok, "-")) && digit.MatchString(tok):
			st = Style{FG: DiffDelInk}
		case strings.HasPrefix(tok, "✓") && digit.MatchString(tok):
			st = Style{FG: StatusSuccess}
		case tok == "✗0":
			st = stMuted
		case strings.HasPrefix(tok, "✗") && digit.MatchString(tok):
			st = Style{FG: StatusFailed}
		case strings.HasPrefix(tok, "●") && digit.MatchString(tok):
			st = Style{FG: StatusWaiting}
		}
		out = append(out, Span{tok, st})
	}
	return out
}

// factSpans renders one artefact summary. "label value  label value"
// summaries carry their own labels; a bare value is labelled by its kind.
func factSpans(kind, summary string) []Span {
	if !strings.Contains(summary, "  ") {
		return append([]Span{{kind + " ", stMuted}}, valueSpans(summary)...)
	}
	var out []Span
	for i, pair := range strings.Split(summary, "  ") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if i > 0 {
			out = append(out, Span{"  ", stInk})
		}
		words := strings.Split(pair, " ")
		cut := len(words)
		for k, w := range words {
			if digit.MatchString(w) {
				cut = k
				break
			}
		}
		if cut == len(words) || cut == 0 {
			cut = 1
		}
		out = append(out, Span{strings.Join(words[:cut], " ") + " ", stMuted})
		out = append(out, valueSpans(strings.Join(words[cut:], " "))...)
	}
	return out
}

// facts is the per-type summary line at the foot of a tile (SPEC §2.4),
// built from the task's artefacts in the type's declared order.
func facts(t *v1.Task, spec *v1.ProcessTypeSpec) []Span {
	byKind := map[string]*v1.ArtefactRef{}
	for _, a := range t.Artefacts {
		byKind[a.Kind] = a
	}
	order := []string{}
	if spec != nil {
		order = append(order, spec.Artefacts...)
	}
	for _, a := range t.Artefacts { // undeclared kinds go last
		if !contains(order, a.Kind) {
			order = append(order, a.Kind)
		}
	}
	var out []Span
	for _, kind := range order {
		a, ok := byKind[kind]
		if !ok || kind == "log" || a.Summary == "" {
			continue
		}
		if len(out) > 0 {
			out = append(out, Span{"  ", stInk})
		}
		out = append(out, factSpans(kind, a.Summary)...)
	}
	if len(out) == 0 {
		return []Span{{"no artefacts yet", stFaint}}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// slug turns a title into a task name.
func slug(title string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			dash = false
		} else if !dash && sb.Len() > 0 {
			sb.WriteByte('-')
			dash = true
		}
		if sb.Len() >= 32 {
			break
		}
	}
	return strings.Trim(sb.String(), "-")
}
