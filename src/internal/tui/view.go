package tui

import (
	"fmt"
	"strings"
	"time"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

// Render draws the current screen onto a fresh canvas.
func (a *App) Render() *Canvas {
	c := NewCanvas(a.w, a.h)
	if a.w < minCols || a.h < minRows {
		a.drawTooSmall(c)
		return c
	}
	if !a.loaded {
		a.drawConnecting(c)
		return c
	}
	if a.screen == screenDetail && a.detail != nil {
		a.drawDetail(c)
	} else {
		a.drawFleet(c)
	}
	if a.help {
		a.drawHelp(c)
	}
	return c
}

func (a *App) drawConnecting(c *Canvas) {
	msg := "connecting to the control plane…"
	if a.connErr != "" {
		msg = "can't reach the control plane: " + a.connErr
	}
	c.TextEllipsis(max(0, (a.w-Width(msg))/2), a.h/2, msg, stMuted, a.w)
	hint := "retrying · ctrl+c quits"
	c.Text(max(0, (a.w-Width(hint))/2), a.h/2+1, hint, stFaint, a.w)
}

func (a *App) drawTooSmall(c *Canvas) {
	waiting := 0
	for _, t := range a.fleet.All(a.now(), "") {
		if t.Status == v1.TaskStatus_TASK_STATUS_WAITING {
			waiting++
		}
	}
	l1 := fmt.Sprintf("terminal too small · %d×%d · need %d×%d", a.w, a.h, minCols, minRows)
	l2 := []Span{{"fleet keeps running · ", stMuted}, {fmt.Sprintf("◆ %d waiting", waiting), Style{FG: StatusWaiting, Bold: true}}}
	y := max(0, a.h/2-1)
	c.TextEllipsis(max(0, (a.w-Width(l1))/2), y, l1, stInk, a.w)
	c.Spans(max(0, (a.w-spansWidth(l2))/2), y+1, l2, a.w)
}

// statusCounts counts tasks per status in header order.
func statusCounts(ts []*v1.Task) map[v1.TaskStatus]int {
	n := map[v1.TaskStatus]int{}
	for _, t := range ts {
		n[t.Status]++
	}
	return n
}

// countSpans renders "⠹ 5 running  ◆ 2 waiting …" (or glyph + count only).
func (a *App) countSpans(n map[v1.TaskStatus]int, labels bool, sep string) []Span {
	var out []Span
	for _, s := range countOrder {
		if n[s] == 0 {
			continue
		}
		l := statusLook(s, a.frame)
		if len(out) > 0 {
			out = append(out, Span{sep, Style{}})
		}
		text := fmt.Sprintf("%s %d", l.Glyph, n[s])
		if labels {
			text += " " + l.Label
		}
		out = append(out, Span{text, l.style()})
	}
	return out
}

// drawBar fills a bar row with pane-raised and draws left/right spans.
func (a *App) drawBar(c *Canvas, y int, left, right []Span) {
	c.Fill(0, y, a.w, 1, Style{BG: PaneRaised})
	rw := spansWidth(right)
	c.Spans(0, y, left, a.w-rw-1)
	c.Spans(a.w-rw, y, right, rw)
}

func (a *App) fleetHeader(c *Canvas, visible []*v1.Task, idle bool) {
	now := a.now()
	counts := statusCounts(visible)
	byType := map[string]int{}
	for _, t := range visible {
		byType[t.ProcessType]++
	}
	var types []Span
	for i, s := range a.fleet.Types {
		if i > 0 {
			types = append(types, Span{"  ", Style{}})
		}
		types = append(types, Span{s.Name + " ", stMuted}, Span{fmt.Sprint(byType[s.Name]), stInk})
	}
	totalsOf := visible
	if idle {
		totalsOf = a.fleet.Recent(now, false)
	}
	tokens, cost := Totals(totalsOf)
	size := fmt.Sprintf("%d×%d", a.w, a.h)
	conn := []Span{}
	if a.connErr != "" {
		conn = []Span{{"⚠ reconnecting", Style{FG: StatusFailed}}, {"  ", Style{}}}
	}

	build := func(labels, withTypes, withTokens bool) ([]Span, []Span) {
		left := []Span{{" floatilla", stBold}}
		if withTypes {
			left = append(left, Span{"  ", Style{}})
			left = append(left, types...)
			left = append(left, Span{" │ ", stLine})
		} else {
			left = append(left, Span{"  ", Style{}})
		}
		if idle {
			left = append(left, Span{"idle", stMuted})
		} else {
			left = append(left, a.countSpans(counts, labels, "  ")...)
		}
		money := fmtCost(cost)
		if withTokens {
			money = fmtTokens(tokens) + " tok · " + money
		}
		right := append(append([]Span{}, conn...), Span{money, stMuted}, Span{"  " + size + " ", stMuted})
		return left, right
	}
	// Degrade at narrow widths (SPEC §3): labels → type counts → tokens.
	variants := [][3]bool{{true, true, true}, {false, true, true}, {false, false, true}, {false, false, false}}
	var left, right []Span
	for _, v := range variants {
		left, right = build(v[0], v[1], v[2])
		if spansWidth(left)+spansWidth(right)+2 <= a.w {
			break
		}
	}
	a.drawBar(c, 0, left, right)
}

type hint struct{ key, label string }

// statusBar draws the mode chip, key hints and help / flash on the right.
func (a *App) statusBar(c *Canvas, mode string, hints []hint, primary string, right []Span) {
	y := a.h - 1
	left := []Span{{" " + mode + " ", Style{FG: AccentInk, BG: Accent, Bold: true, Chip: true}}}
	if right == nil {
		right = []Span{{"?", stInk}, {" help ", stMuted}}
	}
	if a.flash != "" {
		st := stMuted
		if a.flashErr {
			st = Style{FG: StatusFailed}
		}
		right = []Span{{Ellipsis(a.flash, a.w/2) + " ", st}}
	}
	avail := a.w - spansWidth(right) - 2
	for _, h := range hints {
		keySt := stInk
		if h.key == primary {
			keySt = stAccent
		}
		item := []Span{{"  ", Style{}}, {h.key, keySt}, {" " + h.label, stMuted}}
		if spansWidth(left)+spansWidth(item) > avail {
			break
		}
		left = append(left, item...)
	}
	a.drawBar(c, y, left, right)
}

// inputBar replaces the status bar while typing.
func (a *App) inputBar(c *Canvas) bool {
	if a.in.kind == inputNone {
		return false
	}
	var mode, prompt, help string
	switch a.in.kind {
	case inputFilter:
		mode, prompt, help = "FILTER", "name, type or status: ", "⏎ keep · esc clear"
	case inputAnswer:
		mode, prompt, help = "ANSWER", a.taskName(a.in.taskID)+": ", "⏎ send · esc cancel"
	case inputNewAgent:
		mode, prompt, help = "NEW", "new "+a.in.lane+" agent · title: ", "⏎ submit · esc cancel"
	case inputConfirmStop:
		left := []Span{{" STOP ", Style{FG: AccentInk, BG: Accent, Bold: true, Chip: true}},
			{"  stop " + a.taskName(a.in.taskID) + "? ", stInk}, {"y", stAccent}, {" stop  ", stMuted}, {"n", stInk}, {" keep it running", stMuted}}
		a.drawBar(c, a.h-1, left, nil)
		return true
	}
	cursor := " "
	if a.frame/5%2 == 0 {
		cursor = "▏"
	}
	left := []Span{{" " + mode + " ", Style{FG: AccentInk, BG: Accent, Bold: true, Chip: true}},
		{"  " + prompt, stMuted}, {a.in.text, stInk}, {cursor, stAccent}}
	a.drawBar(c, a.h-1, left, []Span{{help + " ", stMuted}})
	return true
}

func (a *App) drawHelp(c *Canvas) {
	type row struct{ k, v string }
	rows := []row{
		{"h j k l / arrows", "move (crosses lanes at the edges)"},
		{"[ ]", "previous / next lane"},
		{"⏎", "open agent detail · expand a collapsed lane"},
		{"y / n", "answer the selected agent's prompt"},
		{"+", "new agent in the selected lane"},
		{"x", "stop (asks first)"},
		{"P / r", "pause / resume"},
		{"/", "filter by name, type or status"},
		{"v", "toggle grid / list"},
		{"g", "toggle grouping by type"},
		{"z", "pin all lanes open"},
		{"1–9", "detail: switch artefact"},
		{"j k · f", "detail: scroll · follow the log"},
		{"tab", "detail: switch pane"},
		{"esc", "back to the fleet · clear filter"},
		{"q", "quit"},
	}
	w := min(a.w-4, 64)
	h := min(a.h-2, len(rows)+4)
	x, y := (a.w-w)/2, (a.h-h)/2
	c.Fill(x, y, w, h, Style{BG: Pane})
	c.Box(x, y, w, h, BoxRounded, stAccent)
	c.Text(x+2, y, " KEYS ", Style{FG: Accent, Bold: true}, w-4)
	for i, r := range rows {
		if i+2 >= h-1 {
			break
		}
		c.Text(x+3, y+2+i, r.k, stInk, 20)
		c.TextEllipsis(x+24, y+2+i, r.v, stMuted, w-27)
	}
	c.Text(x+w-14, y+h-1, " ? closes ", stMuted, 12)
}

func (a *App) drawFleet(c *Canvas) {
	now := a.now()
	lay := a.fleetLayout()
	visible := a.fleet.All(now, "")
	idle := lay.Mode == ModeEmpty && !a.listView
	a.fleetHeader(c, visible, idle)

	switch {
	case idle:
		a.drawEmpty(c)
	case a.isList(lay):
		a.drawList(c, now)
	default:
		a.drawGrid(c, lay, now)
	}

	if a.inputBar(c) {
		return
	}
	switch {
	case idle:
		a.statusBar(c, "IDLE", []hint{{"n", "new agent"}, {"r", "rerun"}, {"⏎", "open"}, {"h", "history"}, {"q", "quit"}}, "n", nil)
	case a.isList(lay):
		hints := []hint{{"j/k", "move"}, {"[ ]", "group"}, {"⏎", "open"}, {"y/n", "answer"}, {"+", "new agent"}, {"x", "stop"}, {"/", "filter"}}
		if lay.Mode == ModeGrid || a.listView {
			hints = append(hints, hint{"v", "grid view"})
		}
		a.statusBar(c, "LIST", hints, "⏎", nil)
	default:
		a.statusBar(c, "GRID", []hint{{"hjkl", "move"}, {"[ ]", "lane"}, {"⏎", "open"}, {"y/n", "answer"},
			{"+", "new agent"}, {"x", "stop"}, {"/", "filter"}, {"g", "ungroup"}, {"v", "list view"}}, "⏎", nil)
	}
}

func (a *App) drawEmpty(c *Canvas) {
	now := a.now()
	recent := a.fleet.Recent(now, a.historyAll)
	w := min(a.w-8, 76)
	x := (a.w - w) / 2
	shown := min(len(recent), max(0, a.h-14))
	blockH := 6 + max(1, shown) + 1
	y := max(2, (a.h-blockH)/2)

	title := "no agents running"
	c.Text((a.w-Width(title))/2, y, title, stInk, w)
	keys := []Span{}
	chip := func(k, label string) {
		if len(keys) > 0 {
			keys = append(keys, Span{"   ", Style{}})
		}
		keys = append(keys, Span{"[" + k + "]", stAccent}, Span{" " + label, stMuted})
	}
	chip("n", "new agent")
	for _, s := range a.fleet.Types {
		chip(s.Name[:1], s.Name)
	}
	c.Spans((a.w-spansWidth(keys))/2, y+2, keys, w)

	label := "RECENT · TODAY "
	if a.historyAll {
		label = "RECENT · ALL "
	}
	c.Text(x, y+4, label, Style{FG: InkMuted, Bold: true}, w)
	c.HLine(x+Width(label), y+4, w-Width(label), '─', stLine)
	if len(recent) == 0 {
		c.Text(x+2, y+5, "nothing finished yet", stFaint, w)
	}
	for i := 0; i < shown; i++ {
		t := recent[i]
		ry := y + 5 + i
		if i == a.emptyCursor {
			c.FillBG(x, ry, w, 1, Selection)
			c.Text(x, ry, "▌", stAccent, 1)
		}
		l := statusLook(t.Status, 0)
		c.Text(x+2, ry, l.Glyph, l.style(), 1)
		c.Text(x+4, ry, t.ProcessType, stMuted, 8)
		c.TextEllipsis(x+13, ry, t.Name, stInk, 30)
		desc := recentDesc(t, a.fleet.Type(t.ProcessType)) + " · " + fmtDuration(now.Sub(t.FinishedAt.AsTime()))
		desc = Ellipsis(desc, w-46)
		c.Text(x+w-Width(desc), ry, desc, stMuted, w-46)
	}
	foot := []Span{{"r", stInk}, {" rerun selected · ", stMuted}, {"⏎", stAccent}, {" open artefacts · ", stMuted}, {"h", stInk}, {" full history", stMuted}}
	if a.historyAll {
		foot[len(foot)-1].Text = " today only"
	}
	c.Spans(x, y+6+max(1, shown), foot, w)
}

func recentDesc(t *v1.Task, spec *v1.ProcessTypeSpec) string {
	stage := stageName(spec, t.StageIndex)
	if t.Status == v1.TaskStatus_TASK_STATUS_FAILED {
		if t.Error != "" {
			return "failed in " + stage + ": " + t.Error
		}
		return "failed in " + stage
	}
	parts := []string{stage}
	for _, ar := range t.Artefacts {
		if ar.Kind == "pr" || ar.Kind == "deploy" {
			parts = append(parts, ar.Summary)
		}
	}
	return strings.Join(parts, " · ")
}

func (a *App) laneCountSpans(ts []*v1.Task) []Span {
	n := statusCounts(ts)
	var out []Span
	for _, s := range countOrder {
		if n[s] == 0 || s == v1.TaskStatus_TASK_STATUS_QUEUED {
			continue
		}
		l := statusLook(s, a.frame)
		if len(out) > 0 {
			out = append(out, Span{" ", Style{}})
		}
		out = append(out, Span{fmt.Sprintf("%s%d", l.Glyph, n[s]), l.style()})
	}
	return out
}

func (a *App) drawGrid(c *Canvas, lay Layout, now time.Time) {
	lanes := a.fleet.Lanes(now, a.filter)
	y := 2
	if lay.ShowFilterRow {
		a.drawFilterRow(c, lay, lanes)
		y = 4
	}
	tileH := fullTileRows
	if lay.Tile == TileCompact {
		tileH = compactTileRows
	}
	tilesTop := y + 3
	bottom := tilesTop + lay.Rows*(tileH+rowGap) - rowGap
	rects := placeLanes(a.w, lay)

	for i, ln := range lay.Lanes {
		r := rects[i]
		spec := a.fleet.Type(ln.Name)
		laneSelected := a.sel.lane == ln.Name
		if ln.Collapsed {
			a.drawLaneRail(c, r.X, y, r.W, bottom-y, spec, laneSelected && a.sel.id == "")
			continue
		}
		ts := lanes[ln.Name]
		// Scroll the lane so the selected agent stays visible (SPEC §6.4).
		scroll := 0
		for k, t := range ts {
			if t.Id == a.sel.id && k/ln.TileCols >= lay.Rows {
				scroll = (k/ln.TileCols - lay.Rows + 1) * ln.TileCols
			}
		}
		hidden := max(0, len(ts)-scroll-ln.Capacity)
		a.drawLaneHeader(c, r, y, spec, ts, hidden, now)

		for row := 0; row < lay.Rows; row++ {
			for col := 0; col < ln.TileCols; col++ {
				k := scroll + row*ln.TileCols + col
				tx, ty, tw := r.TileX[col], tilesTop+row*(tileH+rowGap), r.TileW[col]
				if k < len(ts) {
					a.drawTile(c, tx, ty, tw, tileH, ts[k], spec, ts[k].Id == a.sel.id, lay.Tile == TileCompact, now)
				} else {
					a.drawEmptySlot(c, tx, ty, tw, tileH, spec, laneSelected && a.sel.id == "" && k == len(ts))
				}
			}
		}
	}
}

func (a *App) drawFilterRow(c *Canvas, lay Layout, lanes map[string][]*v1.Task) {
	total := 0
	for _, ts := range lanes {
		total += len(ts)
	}
	filter := "none"
	if a.filter != "" {
		filter = a.filter
	}
	group := "by type"
	if a.ungrouped {
		group = "none"
	}
	left := []Span{
		{fmt.Sprintf("AGENTS · %d", total), Style{FG: Accent, Bold: true}},
		{"  group ", stMuted}, {group, stInk},
		{"  sort ", stMuted}, {"needs you first", stInk},
		{"  filter ", stMuted}, {filter, stInk},
	}
	var alloc []string
	for _, ln := range lay.Lanes {
		if ln.Collapsed {
			alloc = append(alloc, "–")
		} else {
			alloc = append(alloc, fmt.Sprint(ln.TileCols))
		}
	}
	right := fmt.Sprintf("lanes %s · %d rows", strings.Join(alloc, " : "), lay.Rows)
	if a.settings.PinLanes {
		right = "pinned · " + right
	}
	c.Spans(margin, 2, left, a.w-2*margin-Width(right)-2)
	c.Text(a.w-margin-Width(right), 2, right, stMuted, Width(right))
}

func (a *App) drawLaneHeader(c *Canvas, r laneRect, y int, spec *v1.ProcessTypeSpec, ts []*v1.Task, hidden int, now time.Time) {
	title := fmt.Sprintf("%s · %d", spec.GetTitle(), len(ts))
	right := a.laneCountSpans(ts)
	if hidden > 0 {
		more := Span{fmt.Sprintf("  ↓%d more", hidden), stMuted}
		if Width(title)+spansWidth(right)+Width(more.Text)+1 > r.W {
			more.Text = fmt.Sprintf(" ↓%d", hidden)
		}
		right = append(right, more)
	}
	rw := spansWidth(right)
	c.Spans(r.X+r.W-rw, y, right, rw)
	c.TextEllipsis(r.X, y, title, stBold, r.W-rw-1)

	recent := a.fleet.RecentDone(spec.GetName(), now)
	rec := ""
	if recent > 0 {
		rec = fmt.Sprintf("✓%d recent", recent)
		c.Text(r.X+r.W-Width(rec), y+1, rec, Style{FG: StatusSuccess}, Width(rec))
	}
	avail := r.W - Width(rec) - 1
	legend := strings.Join(spec.GetStages(), " › ")
	if Width(legend) > avail && spec.GetLegendShort() != "" {
		legend = spec.GetLegendShort()
	}
	c.TextEllipsis(r.X, y+1, legend, stMuted, avail)
	c.HLine(r.X, y+2, r.W, '─', stLine)
}

func (a *App) drawEmptySlot(c *Canvas, x, y, w, h int, spec *v1.ProcessTypeSpec, selected bool) {
	st := stLine
	if selected {
		st = stAccent
	}
	c.Box(x, y, w, h, BoxDashed, st)
	text := "+  new " + spec.GetName() + " agent"
	c.TextEllipsis(x+max(1, (w-Width(text))/2), y+h/2, text, stMuted, w-2)
}

func (a *App) drawLaneRail(c *Canvas, x, y, w, h int, spec *v1.ProcessTypeSpec, selected bool) {
	st := stLine
	if selected {
		st = stAccent
	}
	c.Box(x, y, w, h, BoxDashed, st)
	ix, iw := x+1, w-2
	c.TextEllipsis(ix, y+1, spec.GetTitle(), stBold, iw)
	c.Text(ix, y+2, "0 agents", stMuted, iw)
	row := y + 4
	c.Text(ix, row, "stages", stFaint, iw)
	for _, s := range spec.GetStages() {
		row++
		if row >= y+h-6 {
			c.Text(ix, row, "…", stFaint, iw)
			break
		}
		c.TextEllipsis(ix, row, "○ "+s, stMuted, iw)
	}
	row += 2
	if row < y+h-4 {
		c.Text(ix, row, "last run", stFaint, iw)
		if last := a.fleet.LastRun(spec.GetName(), a.now()); last != nil {
			l := statusLook(last.Status, 0)
			c.TextEllipsis(ix, row+1, l.Glyph+" "+fmtDuration(a.now().Sub(last.FinishedAt.AsTime()))+" ago", l.style(), iw)
		} else {
			c.Text(ix, row+1, "—", stMuted, iw)
		}
	}
	c.Spans(ix, y+h-3, []Span{{"+", stInk}, {" new", stMuted}}, iw)
	c.Spans(ix, y+h-2, []Span{{"⏎", stAccent}, {" expand", stMuted}}, iw)
}

// chipSpans renders prompt action chips for a waiting task.
func chipSpans(t *v1.Task, withOpen bool) []Span {
	chip := func(k string, st Style) Span { return Span{" " + k + " ", st} }
	key := Style{FG: Ink, BG: PaneRaised, Chip: true}
	hot := Style{FG: AccentInk, BG: StatusWaiting, Bold: true, Chip: true}
	var out []Span
	if t.GetPrompt().GetKind() == v1.PromptKind_PROMPT_KIND_QUESTION {
		out = []Span{chip("y", hot), {" reply", stMuted}}
	} else {
		out = []Span{chip("y", hot), {" approve  ", stMuted}, chip("n", key), {" deny", stMuted}}
	}
	if withOpen {
		out = append(out, Span{"  ", Style{}}, chip("⏎", key), Span{" open", stMuted})
	}
	return out
}

func (a *App) drawTile(c *Canvas, x, y, w, h int, t *v1.Task, spec *v1.ProcessTypeSpec, selected, compact bool, now time.Time) {
	c.Fill(x+1, y+1, w-2, h-2, Style{BG: Pane})
	border, kind, titleSt := stLine, BoxLight, stBold
	if selected {
		border, kind, titleSt = stAccent, BoxRounded, Style{FG: Accent, Bold: true}
	}
	c.Box(x, y, w, h, kind, border)

	el := " " + fmtDuration(elapsed(t, now)) + " "
	ex := x + w - 2 - Width(el)
	c.Text(ex, y, el, stMuted, Width(el))
	c.TextEllipsis(x+2, y, " "+t.Name+" ", titleSt, ex-x-3)

	ix, iw := x+2, w-4
	l := statusLook(t.Status, a.frame+phase(t.Id))
	spend := fmtTokens(taskTokens(t)) + " · " + fmtCost(t.GetUsage().GetCostUsd())
	left := l.Glyph + " " + l.Label
	if Width(left)+Width(spend)+2 > iw {
		spend = fmtCost(t.GetUsage().GetCostUsd())
	}
	c.Text(ix, y+1, left, l.style(), iw)
	if Width(left)+Width(spend)+2 <= iw {
		c.Text(ix+iw-Width(spend), y+1, spend, stMuted, Width(spend))
	}
	c.TextEllipsis(ix, y+2, t.Title, stInk, iw)
	c.Spans(ix, y+3, trackSpans(t, spec, segWidth(iw, len(spec.GetStages()), 8), a.theme.Level), iw)

	waiting := t.Status == v1.TaskStatus_TASK_STATUS_WAITING && t.Prompt != nil
	if compact && waiting {
		// Nine rows leave no room for both the stage line and the action
		// row; the track already shows the stage, so the prompt takes its
		// line and the chips take the activity line.
		c.TextEllipsis(ix, y+4, t.Prompt.Text, Style{FG: StatusWaiting}, iw)
		c.Spans(ix, y+5, chipSpans(t, false), iw)
		c.HLine(ix, y+h-3, iw, '─', stLine)
		c.Spans(ix, y+h-2, facts(t, spec), iw)
		return
	}

	n := len(spec.GetStages())
	stage := []Span{{fmt.Sprintf("%d/%d ", t.StageIndex+1, n), stMuted}, {stageName(spec, t.StageIndex), stBold}}
	if t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED {
		stage = append(stage, Span{" ✓", Style{FG: StatusSuccess}})
	}
	c.Spans(ix, y+4, stage, iw)

	activity := t.Activity
	actSt := Style{FG: l.Tok}
	if t.Status == v1.TaskStatus_TASK_STATUS_RUNNING {
		actSt = stInk
	}
	if waiting {
		activity = t.Prompt.Text
	}
	c.TextEllipsis(ix, y+5, activity, actSt, iw)

	row := y + 6
	tailN := 2
	switch {
	case compact:
		tailN = 0
	case waiting:
		tailN = 1
	}
	tail := t.Tail
	if len(tail) > tailN {
		tail = tail[len(tail)-tailN:]
	}
	for _, line := range tail {
		c.Text(ix, row, "│ ", stFaint, 2)
		c.TextEllipsis(ix+2, row, line, stMuted, iw-2)
		row++
	}
	if waiting {
		c.Spans(ix, row, chipSpans(t, !compact), iw)
	}
	c.HLine(ix, y+h-3, iw, '─', stLine)
	c.Spans(ix, y+h-2, facts(t, spec), iw)
}

func (a *App) drawList(c *Canvas, now time.Time) {
	ts := a.listTasks(now)
	previewH := 6
	if a.h < 24 {
		previewH = 0
	}
	boxY, boxH := 1, a.h-2-previewH
	x, w := 1, a.w-2
	c.Box(x, boxY, w, boxH, BoxRounded, stAccent)
	c.Text(x+2, boxY, fmt.Sprintf(" AGENTS · %d ", len(ts)), Style{FG: Accent, Bold: true}, w-4)
	if a.filter != "" {
		f := " filter " + a.filter + " "
		c.TextEllipsis(x+w-2-Width(f), boxY, f, stMuted, w/2)
	}

	type listRow struct {
		heading *v1.ProcessTypeSpec
		count   int
		task    *v1.Task
	}
	var rows []listRow
	if a.ungrouped {
		for _, t := range ts {
			rows = append(rows, listRow{task: t})
		}
	} else {
		lanes := a.fleet.Lanes(now, a.filter)
		for _, s := range a.fleet.Types {
			rows = append(rows, listRow{heading: s, count: len(lanes[s.Name])})
			for _, t := range lanes[s.Name] {
				rows = append(rows, listRow{task: t})
			}
		}
	}
	inner := boxH - 2
	selIdx := 0
	for i, r := range rows {
		if r.task != nil && r.task.Id == a.sel.id {
			selIdx = i
		}
	}
	top := max(0, selIdx-inner+1)
	ix, iw := x+1, w-2
	nameW, stageW := 18, 14
	for i := 0; i < inner && top+i < len(rows); i++ {
		r := rows[top+i]
		ry := boxY + 1 + i
		if r.heading != nil {
			s := r.heading
			if r.count == 0 {
				c.Spans(ix+2, ry, []Span{{fmt.Sprintf("%s · 0 · ", s.Tag), Style{FG: InkMuted, Bold: true}}, {"+", stInk}, {" new", stMuted}}, iw-2)
				continue
			}
			legend := s.LegendShort
			if legend == "" {
				legend = strings.Join(s.Stages, " › ")
			}
			c.Spans(ix+2, ry, []Span{{fmt.Sprintf("%s · %d", s.Tag, r.count), stBold}, {"  " + legend, stMuted}}, iw-2)
			continue
		}
		t := r.task
		spec := a.fleet.Type(t.ProcessType)
		if t.Id == a.sel.id {
			c.FillBG(ix, ry, iw, 1, Selection)
			c.Text(ix, ry, "▌", stAccent, 1)
		}
		l := statusLook(t.Status, a.frame+phase(t.Id))
		cx := ix + 2
		c.Text(cx, ry, l.Glyph, l.style(), 1)
		cx += 2
		c.TextEllipsis(cx, ry, t.Name, stInk, nameW)
		cx += nameW + 1
		dotsW := max(7, len(spec.GetStages()))
		c.Spans(cx, ry, dotSpans(t, spec), dotsW)
		cx += dotsW + 2
		name := stageName(spec, t.StageIndex)
		if t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED {
			name += " ✓"
		}
		c.TextEllipsis(cx, ry, name, stMuted, stageW)
		cx += stageW + 2
		activity, st := t.Activity, stInk
		if t.Status == v1.TaskStatus_TASK_STATUS_WAITING && t.Prompt != nil {
			activity, st = t.Prompt.Text, Style{FG: StatusWaiting}
		} else if t.Status != v1.TaskStatus_TASK_STATUS_RUNNING {
			st = Style{FG: l.Tok}
		}
		c.TextEllipsis(cx, ry, activity, st, ix+iw-cx-1)
	}
	if previewH > 0 {
		a.drawPreview(c, x, boxY+boxH, w, previewH)
	}
}

func dotSpans(t *v1.Task, spec *v1.ProcessTypeSpec) []Span {
	st := statusLook(t.Status, 0)
	var out []Span
	for i := range spec.GetStages() {
		switch {
		case t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED:
			out = append(out, Span{"■", Style{FG: StatusSuccess}})
		case int32(i) < t.StageIndex:
			out = append(out, Span{"■", stMuted})
		case int32(i) == t.StageIndex:
			out = append(out, Span{"■", Style{FG: st.Tok}})
		default:
			out = append(out, Span{"□", stFaint})
		}
	}
	return out
}

func (a *App) drawPreview(c *Canvas, x, y, w, h int) {
	c.Box(x, y, w, h, BoxLight, stLine)
	t := a.selectedTask()
	if t == nil {
		c.Text(x+2, y, " NO AGENT SELECTED ", stMuted, w-4)
		return
	}
	spec := a.fleet.Type(t.ProcessType)
	title := fmt.Sprintf(" %s · %s · %s %d/%d ", strings.ToUpper(t.Name), spec.GetTag(), strings.ToUpper(stageName(spec, t.StageIndex)), t.StageIndex+1, len(spec.GetStages()))
	c.TextEllipsis(x+2, y, title, Style{FG: InkMuted, Bold: true}, w-4)
	ix, iw := x+2, w-4
	row := y + 1
	l := statusLook(t.Status, a.frame+phase(t.Id))
	if t.Status == v1.TaskStatus_TASK_STATUS_WAITING && t.Prompt != nil {
		c.TextEllipsis(ix, row, "◆ "+t.Prompt.Text, Style{FG: StatusWaiting, Bold: true}, iw)
		c.Spans(ix, row+1, chipSpans(t, true), iw)
		row += 2
	} else {
		c.TextEllipsis(ix, row, l.Glyph+" "+t.Activity, Style{FG: l.Tok}, iw)
		row++
	}
	for _, line := range t.Tail {
		if row >= y+h-1 {
			break
		}
		c.Text(ix, row, "│ ", stFaint, 2)
		c.TextEllipsis(ix+2, row, line, stMuted, iw-2)
		row++
	}
}
