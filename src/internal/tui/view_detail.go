package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

const railWidth = 36

func (a *App) drawDetail(c *Canvas) {
	now := a.now()
	d := a.detail
	t := a.fleet.Task(d.id)
	if t == nil {
		a.drawBar(c, 0, []Span{{" floatilla › ", stBold}, {"loading…", stMuted}}, nil)
		a.statusBar(c, "DETAIL", []hint{{"esc", "fleet"}}, "", nil)
		return
	}
	spec := a.fleet.Type(t.ProcessType)
	tabs := a.artefactTabs(t)
	d.artefact = min(d.artefact, len(tabs)-1)
	kind := tabs[d.artefact]
	l := statusLook(t.Status, a.frame+phase(t.Id))
	wide := a.w >= gridMinCols

	if wide {
		left := []Span{
			{" floatilla › ", stBold}, {"[" + spec.GetTag() + "] ", stMuted}, {t.Name, stBold},
			{"   " + l.Glyph + " " + l.Label, l.style()}, {" · " + fmtDuration(elapsed(t, now)), stMuted},
			{fmt.Sprintf("   stage %d/%d ", t.StageIndex+1, len(spec.GetStages())), stMuted}, {stageName(spec, t.StageIndex), stInk},
		}
		if t.Branch != "" {
			left = append(left, Span{"   ⎇ ", stMuted}, Span{t.Branch, stMuted})
		}
		right := []Span{{fmtTokens(taskTokens(t)) + " tok · " + fmtCost(t.GetUsage().GetCostUsd()), stMuted}, {fmt.Sprintf("  %d×%d ", a.w, a.h), stMuted}}
		a.drawBar(c, 0, left, right)

		paneH := a.h - 3
		a.drawAgentRail(c, t, spec, tabs, 2, 2, railWidth, paneH, now)
		px := 2 + railWidth + 1
		pw := a.w - px - 2
		focused := !d.railFocus
		border, box, titleSt := stLine, BoxLight, stMuted
		if focused {
			border, box, titleSt = stAccent, BoxRounded, Style{FG: Accent, Bold: true}
		}
		c.Box(px, 2, pw, paneH, box, border)
		c.TextEllipsis(px+2, 2, " "+a.paneTitle(t, kind)+" ", titleSt, pw-4)
		ix, iw := px+2, pw-4
		a.drawTabs(c, t, tabs, ix, 3, iw, false)
		c.HLine(px+1, 4, pw-2, '─', stLine)
		c.HLine(px+1, 2+paneH-3, pw-2, '─', stLine)
		bodyY, bodyH := 5, paneH-6
		footer := a.drawArtefact(c, t, spec, kind, ix, bodyY, iw, bodyH, now)
		a.drawFooter(c, t, footer, ix, 2+paneH-2, iw)
	} else {
		left := []Span{{" ‹ ", stMuted}, {t.Name, stBold}, {"  [" + spec.GetTag() + "]  ", stMuted},
			{fmt.Sprintf("%s %d/%d ", l.Glyph, t.StageIndex+1, len(spec.GetStages())), l.style()}, {stageName(spec, t.StageIndex), stInk}}
		right := []Span{{fmtCost(t.GetUsage().GetCostUsd()), stMuted}, {fmt.Sprintf("  %d×%d ", a.w, a.h), stMuted}}
		a.drawBar(c, 0, left, right)
		a.drawTabs(c, t, tabs, 3, 1, a.w-4, true)
		c.HLine(1, 2, a.w-2, '─', stLine)
		bodyH := a.h - 4
		if t.Status == v1.TaskStatus_TASK_STATUS_WAITING && t.Prompt != nil {
			bodyH--
			a.drawFooter(c, t, "", 2, a.h-2, a.w-4)
		}
		a.drawArtefact(c, t, spec, kind, 2, 3, a.w-4, bodyH, now)
	}

	if a.inputBar(c) {
		return
	}
	hints := []hint{{fmt.Sprintf("1-%d", len(tabs)), "artefact"}, {"j/k", "scroll"}, {"f", "follow"}}
	if t.Status == v1.TaskStatus_TASK_STATUS_WAITING {
		hints = append(hints, hint{"y/n", "answer"})
	}
	if wide {
		hints = append(hints, hint{"tab", "pane"})
	}
	hints = append(hints, hint{"x", "stop"})
	a.statusBar(c, strings.ToUpper(kind), hints, "y/n", []Span{{"esc", stInk}, {" fleet  ", stMuted}, {"?", stInk}, {" help ", stMuted}})
}

func (a *App) paneTitle(t *v1.Task, kind string) string {
	if kind == "log" {
		if a.detail.follow {
			return "LOG · FOLLOWING"
		}
		return "LOG · SCROLLED"
	}
	for _, ar := range t.Artefacts {
		if ar.Kind == kind && ar.Summary != "" {
			return strings.ToUpper(kind) + " · " + strings.ToUpper(ar.Summary)
		}
	}
	return strings.ToUpper(kind)
}

func artefactOf(t *v1.Task, kind string) *v1.ArtefactRef {
	for _, ar := range t.Artefacts {
		if ar.Kind == kind {
			return ar
		}
	}
	return nil
}

// drawTabs draws "[1 log]  2 diff  3 tests ✗2" plus right-aligned info.
func (a *App) drawTabs(c *Canvas, t *v1.Task, tabs []string, x, y, w int, narrow bool) {
	var spans []Span
	for i, k := range tabs {
		if i > 0 {
			spans = append(spans, Span{"  ", Style{}})
		}
		label := fmt.Sprintf("%d %s", i+1, k)
		if i == a.detail.artefact {
			spans = append(spans, Span{" " + label + " ", Style{FG: AccentInk, BG: Accent, Bold: true, Chip: true}})
		} else {
			spans = append(spans, Span{fmt.Sprint(i + 1), stInk}, Span{" " + k, stMuted})
		}
		if ar := artefactOf(t, k); ar != nil && narrow {
			for _, tok := range strings.Fields(ar.Summary) {
				if strings.HasPrefix(tok, "✗") && tok != "✗0" {
					spans = append(spans, Span{" " + tok, Style{FG: StatusFailed}})
				}
			}
		}
	}
	info := ""
	if tabs[a.detail.artefact] == "log" {
		n := len(a.detail.logs)
		if n > 0 && a.detail.logs[0].At != nil {
			info = a.detail.logs[0].At.AsTime().Local().Format("15:04") + " → now · "
		}
		info += fmt.Sprintf("%d lines", n)
	} else if ar := artefactOf(t, tabs[a.detail.artefact]); ar != nil && ar.Uri != "" {
		info = ar.Uri
	}
	iw := Width(info)
	used := c.Spans(x, y, spans, w)
	if used+iw+2 <= w {
		c.Text(x+w-iw, y, info, stMuted, iw)
	}
}

// drawArtefact draws the active artefact; returns footer text.
func (a *App) drawArtefact(c *Canvas, t *v1.Task, spec *v1.ProcessTypeSpec, kind string, x, y, w, h int, now time.Time) string {
	if kind == "log" {
		return a.drawLog(c, t, x, y, w, h, now)
	}
	ar := artefactOf(t, kind)
	if ar == nil {
		c.Text(x, y, "no "+kind+" yet", stMuted, w)
		c.TextEllipsis(x, y+2, fmt.Sprintf("%s agents publish a %s artefact when they get to it.", spec.GetName(), kind), stFaint, w)
		return ""
	}
	title := ar.Title
	if title == "" {
		title = kind
	}
	c.TextEllipsis(x, y, title, stBold, w)
	c.Spans(x, y+1, factSpans(kind, ar.Summary), w)
	if ar.Uri != "" {
		c.Spans(x, y+2, []Span{{"at ", stMuted}, {ar.Uri, stInk}}, w)
	}
	if ar.UpdatedAt != nil {
		c.Text(x, y+3, "updated "+fmtDuration(now.Sub(ar.UpdatedAt.AsTime()))+" ago", stFaint, w)
	}
	c.TextEllipsis(x, y+5, "Only the summary is published today; the full "+kind+" view needs artefact content from the backend.", stFaint, w)
	return ""
}

func logKindStyle(kind string) Style {
	switch kind {
	case "bash":
		return Style{FG: StatusRunning, Bold: true}
	case "ask":
		return Style{FG: StatusWaiting, Bold: true}
	case "read", "grep", "think":
		return Style{FG: InkMuted, Bold: true}
	default:
		return stBold
	}
}

func toneStyle(tone v1.LogTone, fallback Style) Style {
	switch tone {
	case v1.LogTone_LOG_TONE_FAILED:
		return Style{FG: StatusFailed}
	case v1.LogTone_LOG_TONE_SUCCESS:
		return Style{FG: StatusSuccess}
	case v1.LogTone_LOG_TONE_MUTED:
		return stMuted
	}
	return fallback
}

// drawLog renders the live log with follow mode (SPEC §5.1). Returns the
// footer text.
func (a *App) drawLog(c *Canvas, t *v1.Task, x, y, w, h int, now time.Time) string {
	d := a.detail
	inflight := t.Status == v1.TaskStatus_TASK_STATUS_RUNNING
	total := len(d.logs)
	if inflight {
		total++
	}
	maxTop := max(0, total-h)
	if d.follow {
		d.top = maxTop
	} else if d.top >= maxTop {
		d.top = maxTop
		if total > h {
			d.follow = true // scrolled back to the bottom: re-pin
		}
	}
	if total == 0 {
		c.Text(x, y, "no log lines yet", stFaint, w)
	}
	for i := 0; i < h && d.top+i < total; i++ {
		idx := d.top + i
		ry := y + i
		if idx == len(d.logs) { // the in-flight line
			c.FillBG(x-1, ry, w+2, 1, Selection)
			c.Text(x-1, ry, "▌", stAccent, 1)
			l := statusLook(t.Status, a.frame+phase(t.Id))
			c.Text(x, ry, now.Format("15:04:05"), stFaint, 8)
			c.Text(x+10, ry, l.Glyph, l.style(), 1)
			c.TextEllipsis(x+12, ry, t.Activity, stInk, w-12)
			continue
		}
		ev := d.logs[idx]
		ln := ev.GetLog()
		if ln.GetKind() == "" {
			c.Text(x+17, ry, "│ ", stFaint, 2)
			c.TextEllipsis(x+19, ry, ln.GetText(), toneStyle(ln.GetTone(), stMuted), w-19)
			continue
		}
		if ev.At != nil && !ev.At.AsTime().IsZero() && ev.At.AsTime().Unix() > 0 {
			c.Text(x, ry, ev.At.AsTime().Local().Format("15:04:05"), stFaint, 8)
		}
		c.Text(x+10, ry, ln.GetKind(), logKindStyle(ln.GetKind()), 5)
		textSt := stInk
		if ln.GetKind() == "think" || ln.GetKind() == "read" || ln.GetKind() == "grep" {
			textSt = stMuted
		}
		c.TextEllipsis(x+17, ry, ln.GetText(), toneStyle(ln.GetTone(), textSt), w-17)
	}
	if d.follow {
		return "following · pinned to bottom · k scroll up to unpin"
	}
	return fmt.Sprintf("scrolled · line %d of %d · f follow", d.top+1, total)
}

// drawFooter shows the pending prompt if any, else the footer text.
func (a *App) drawFooter(c *Canvas, t *v1.Task, text string, x, y, w int) {
	if t.Status == v1.TaskStatus_TASK_STATUS_WAITING && t.Prompt != nil {
		chips := chipSpans(t, false)
		cw := spansWidth(chips)
		c.TextEllipsis(x, y, "◆ "+t.Prompt.Text, Style{FG: StatusWaiting, Bold: true}, w-cw-2)
		c.Spans(x+w-cw, y, chips, cw)
		return
	}
	c.TextEllipsis(x, y, text, stMuted, w)
}

func (a *App) drawAgentRail(c *Canvas, t *v1.Task, spec *v1.ProcessTypeSpec, tabs []string, x, y, w, h int, now time.Time) {
	d := a.detail
	border, box, titleSt := stLine, BoxLight, stMuted
	if d.railFocus {
		border, box, titleSt = stAccent, BoxRounded, Style{FG: Accent, Bold: true}
	}
	c.Box(x, y, w, h, box, border)
	c.Text(x+2, y, " AGENT ", titleSt, w-4)
	ix, iw := x+2, w-4
	row := y + 1
	c.TextEllipsis(ix, row, t.Title, stBold, iw)
	row++
	started := "queued"
	if t.StartedAt != nil {
		started = "started " + t.StartedAt.AsTime().Local().Format("15:04")
	}
	c.TextEllipsis(ix, row, fmt.Sprintf("%s · %d log lines", started, len(d.logs)), stMuted, iw)
	row += 2
	c.Spans(ix, row, trackSpans(t, spec, segWidth(iw, len(spec.GetStages()), 10), a.theme.Level), iw)
	row += 2

	c.TextEllipsis(ix, row, spec.GetTitle()+" STAGES", Style{FG: InkMuted, Bold: true}, iw)
	row++
	records := map[int32]*v1.StageRecord{}
	for _, r := range t.Stages {
		records[r.Index] = r
	}
	stages := spec.GetStages()
	bottomRows := 4 + len(tabs) + 3 // artefacts block + footer
	for i, s := range stages {
		if row >= y+h-bottomRows {
			break
		}
		branch := "├─ "
		if i == len(stages)-1 {
			branch = "└─ "
		}
		rec := records[int32(i)]
		mark, markSt := "○", stFaint
		dur := ""
		switch {
		case rec != nil && rec.FinishedAt != nil:
			mark, markSt = "✓", Style{FG: StatusSuccess}
			if rec.StartedAt != nil {
				dur = fmtDuration(rec.FinishedAt.AsTime().Sub(rec.StartedAt.AsTime()))
			}
		case int32(i) == t.StageIndex:
			l := statusLook(t.Status, a.frame+phase(t.Id))
			mark, markSt = l.Glyph, l.style()
			if rec != nil && rec.StartedAt != nil {
				dur = fmtDuration(now.Sub(rec.StartedAt.AsTime()))
			}
		}
		c.Text(ix, row, branch, stFaint, 3)
		c.Text(ix+3, row, mark, markSt, 1)
		nameSt := stMuted
		if int32(i) == t.StageIndex {
			nameSt = stInk
		}
		c.TextEllipsis(ix+5, row, s, nameSt, iw-5-Width(dur)-1)
		c.Text(ix+iw-Width(dur), row, dur, stMuted, Width(dur))
		row++
		if int32(i) == t.StageIndex && t.Activity != "" && row < y+h-bottomRows {
			c.Text(ix, row, "│", stFaint, 1)
			c.TextEllipsis(ix+5, row, t.Activity, stMuted, iw-5)
			row++
		}
	}

	row = max(row+1, y+h-bottomRows)
	c.Text(ix, row, "ARTEFACTS", Style{FG: InkMuted, Bold: true}, iw)
	row++
	for i, k := range tabs {
		if row >= y+h-4 {
			break
		}
		desc := "—"
		switch {
		case k == "log":
			desc = fmt.Sprintf("%d lines", len(d.logs))
			if !fsmTerminal(t.Status) {
				desc += " · live"
			}
		case artefactOf(t, k) != nil:
			desc = artefactOf(t, k).Summary
		default:
			desc = "not yet"
		}
		if i == d.artefact {
			c.FillBG(ix-1, row, iw+2, 1, PaneRaised)
		}
		st := stInk
		if i == d.artefact {
			st = stBold
		}
		c.Text(ix, row, fmt.Sprintf("%d %-6s", i+1, k), st, 9)
		c.TextEllipsis(ix+9, row, desc, stMuted, iw-9)
		row++
	}
	c.HLine(ix, y+h-4, iw, '─', stLine)
	pct := int(math.Round(t.ContextUsed * 100))
	filled := min(10, int(math.Round(t.ContextUsed*10)))
	c.Spans(ix, y+h-3, []Span{{"context ", stMuted}, {strings.Repeat("█", filled), stMuted}, {strings.Repeat("░", 10-filled), stFaint}, {fmt.Sprintf(" %d%%", pct), stMuted}}, iw)
	c.Spans(ix, y+h-2, []Span{{"a", stInk}, {" attach to session", stMuted}}, iw)
}

func fsmTerminal(s v1.TaskStatus) bool {
	return s == v1.TaskStatus_TASK_STATUS_SUCCEEDED || s == v1.TaskStatus_TASK_STATUS_FAILED
}
