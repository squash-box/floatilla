package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

var testNow = time.Date(2026, 10, 24, 14, 16, 0, 0, time.Local)

// demoApp is an App loaded with the sample fleet from Main.dc.html, with
// flaky-tests selected as in the mockup.
func demoApp(t *testing.T, w, h int, tasks []*v1.Task) *App {
	t.Helper()
	a := New(Options{Source: NewDemoSource(), Level: LevelTrue, Palette: &Dark, Now: func() time.Time { return testNow }})
	a.w, a.h = w, h
	if tasks == nil {
		tasks = DemoTasks(testNow)
	}
	a.Update(snapshotMsg{types: DemoTypes(), tasks: tasks})
	for _, tk := range tasks {
		if tk.Name == "flaky-tests" {
			a.sel = selection{lane: tk.ProcessType, id: tk.Id}
		}
	}
	return a
}

func dump(t *testing.T, c *Canvas) {
	if os.Getenv("TUI_DUMP") != "" {
		t.Logf("\n%s", c.Plain())
	}
}

func press(a *App, keys ...string) {
	for _, k := range keys {
		a.key(k)
	}
}

func TestDumpScreens(t *testing.T) {
	if os.Getenv("TUI_DUMP") == "" {
		t.Skip("set TUI_DUMP=1 to print screens")
	}
	for _, sz := range [][2]int{{160, 44}, {120, 36}, {80, 24}, {60, 16}} {
		a := demoApp(t, sz[0], sz[1], nil)
		dump(t, a.Render())
	}
	a := demoApp(t, 160, 44, nil)
	press(a, "enter")
	a.Update(tea.KeyPressMsg{})
	dump(t, a.Render())
}

func row(c *Canvas, substr string) int {
	for y := 0; y < c.H; y++ {
		if strings.Contains(c.Row(y), substr) {
			return y
		}
	}
	return -1
}

// §12: at 160×44 the grid matches Main.dc.html.
func TestAcceptanceMainGrid(t *testing.T) {
	a := demoApp(t, 160, 44, nil)
	lay := a.fleetLayout()
	if lay.Mode != ModeGrid || lay.Tile != TileFull || lay.Rows != 3 || !eq(tileCols(lay), []int{1, 2, 1}) {
		t.Fatalf("layout %+v", lay)
	}
	c := a.Render()
	dump(t, c)
	y := row(c, "╭─ flaky-tests")
	if y < 0 {
		t.Fatal("flaky-tests is not drawn selected (rounded border)")
	}
	if st := c.StyleAt(colOf(c, y, "╭"), y); st.FG != Accent {
		t.Fatalf("selected border style = %+v, want accent", st)
	}
	for _, want := range []string{"lanes 1 : 2 : 1 · 3 rows", "PLANNING · 2", "IMPLEMENTATION · 6", "DEPLOY · 2", "+  new plan agent", "◆ 2 waiting", "$15.16"} {
		if row(c, want) < 0 {
			t.Errorf("missing %q", want)
		}
	}
	if row(c, "y  approve") < 0 {
		t.Error("waiting tiles have no action chips")
	}
}

func colOf(c *Canvas, y int, s string) int {
	runes := []rune(c.Row(y))
	target := []rune(s)
	for i := range runes {
		if i+len(target) <= len(runes) && string(runes[i:i+len(target)]) == s {
			return i
		}
	}
	return -1
}

// §12: at 120×36 three lanes, one column each, compact tiles, ↓3 on impl.
func TestAcceptanceGrid120(t *testing.T) {
	a := demoApp(t, 120, 36, nil)
	lay := a.fleetLayout()
	if lay.Tile != TileCompact || !eq(tileCols(lay), []int{1, 1, 1}) {
		t.Fatalf("layout %+v", lay)
	}
	c := a.Render()
	dump(t, c)
	y := row(c, "IMPLEMENTATION · 6")
	if y < 0 || !strings.Contains(c.Row(y), "↓3") {
		t.Fatalf("impl header lacks ↓3: %q", c.Row(max(0, y)))
	}
}

// §12: at 80×24 a list grouped by type with a preview pane; below 80×24
// the too-small message.
func TestAcceptanceListAndTooSmall(t *testing.T) {
	c := demoApp(t, 80, 24, nil).Render()
	dump(t, c)
	for _, want := range []string{"AGENTS · 10", "PLAN · 2", "IMPL · 6", "DEPLOY · 2", "FLAKY-TESTS · IMPL · TESTING 3/7", "◆ approve Bash(pnpm test --runInBand)?"} {
		if row(c, want) < 0 {
			t.Errorf("80×24 missing %q", want)
		}
	}
	if y := row(c, "flaky-tests"); y < 0 || !strings.HasPrefix(strings.TrimLeft(c.Row(y), " │"), "▌") && !strings.Contains(c.Row(y), "▌") {
		t.Error("cursor marker not on the selected row")
	}

	c = demoApp(t, 60, 16, nil).Render()
	if row(c, "terminal too small · 60×16 · need 80×24") < 0 || row(c, "fleet keeps running · ◆ 2 waiting") < 0 {
		t.Fatalf("too small:\n%s", c.Plain())
	}
}

// §12: resizing keeps the selection (scrolling its lane) and the log pin.
func TestAcceptanceResizeKeepsSelection(t *testing.T) {
	a := demoApp(t, 160, 44, nil)
	for _, tk := range a.fleet.All(testNow, "") {
		if tk.Name == "docs-sweep" { // last in impl's sort order
			a.sel = selection{lane: "impl", id: tk.Id}
		}
	}
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	c := a.Render()
	if row(c, "╭─ docs-sweep") < 0 {
		t.Fatalf("selected agent hidden after resize:\n%s", c.Plain())
	}
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if a.selectedTask().GetName() != "docs-sweep" {
		t.Fatal("selection lost in list mode")
	}

	press(a, "enter")
	if !a.detail.follow {
		t.Fatal("log should follow by default")
	}
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 44})
	a.Render()
	if !a.detail.follow || a.screen != screenDetail {
		t.Fatal("resize dropped the log pin or the detail screen")
	}
}

// §12: emptying deploy collapses it after 30 s and impl takes 3 columns;
// a new deploy agent re-expands it at once.
func TestAcceptanceLaneCollapse(t *testing.T) {
	now := testNow
	a := demoApp(t, 160, 44, nil)
	a.now = func() time.Time { return now }

	tasks := DemoTasks(testNow)
	var deploy []*v1.Task
	for _, tk := range tasks {
		if tk.ProcessType == "deploy" {
			deploy = append(deploy, tk)
		}
	}
	// Both deploy agents finish and age out.
	for _, tk := range deploy {
		tk.Status = v1.TaskStatus_TASK_STATUS_SUCCEEDED
		tk.FinishedAt = timestampAt(now.Add(-time.Hour))
		tk.UpdatedAt = timestampAt(now)
		a.Update(taskMsg{tk})
	}
	// Two more impl agents so the freed width has somewhere to go (2 / 8).
	for i := 0; i < 2; i++ {
		extra := DemoTasks(testNow)[4]
		extra.Id = "extra" + string(rune('a'+i))
		a.Update(taskMsg{extra})
	}
	a.Update(tickMsg(now))
	if l := a.fleetLayout(); l.Lanes[2].Collapsed {
		t.Fatal("collapsed before the 30 s debounce")
	}
	now = now.Add(31 * time.Second)
	a.Update(tickMsg(now))
	l := a.fleetLayout()
	if !l.Lanes[2].Collapsed || l.Lanes[1].TileCols != 3 {
		t.Fatalf("after 30 s: %+v", l)
	}
	c := a.Render()
	if row(c, "⏎ expand") < 0 || row(c, "last run") < 0 {
		t.Fatalf("rail or recent count missing:\n%s", c.Plain())
	}

	fresh := DemoTasks(testNow)[8]
	fresh.Id = "fresh"
	fresh.FinishedAt = nil
	fresh.Status = v1.TaskStatus_TASK_STATUS_RUNNING
	a.Update(taskMsg{fresh})
	if a.fleetLayout().Lanes[2].Collapsed {
		t.Fatal("a new deploy agent should re-expand the lane immediately")
	}
}

// §12: zero agents shows the idle screen with the recent list.
func TestAcceptanceEmptyFleet(t *testing.T) {
	tasks := DemoTasks(testNow)
	var finished []*v1.Task
	for _, tk := range tasks {
		tk.Status = v1.TaskStatus_TASK_STATUS_SUCCEEDED
		tk.FinishedAt = timestampAt(testNow.Add(-2 * time.Hour))
		tk.Prompt = nil
		finished = append(finished, tk)
	}
	a := demoApp(t, 160, 44, finished)
	c := a.Render()
	dump(t, c)
	for _, want := range []string{"no agents running", "RECENT · TODAY", "[n] new agent", "[p] plan", "plan 0  impl 0  deploy 0 │ idle", " IDLE "} {
		if row(c, want) < 0 {
			t.Errorf("idle screen missing %q", want)
		}
	}
}

// §12: statuses read with NO_COLOR and on 16-colour terminals.
func TestAcceptanceColorLadder(t *testing.T) {
	a := demoApp(t, 160, 44, nil)
	a.theme = NewTheme(LevelNone, Dark)
	none := a.Render().Render(a.theme)
	if strings.Contains(none, "38;") || strings.Contains(none, "48;") || strings.Contains(none, "[3") {
		t.Fatal("NO_COLOR output contains colour codes")
	}
	for _, glyph := range []string{"◆ waiting", "✗ failed", "✓ done", "‖ paused", "▓"} {
		if !strings.Contains(none, glyph) {
			t.Errorf("NO_COLOR output missing %q", glyph)
		}
	}
	if !strings.Contains(none, "\x1b[7m") {
		t.Error("NO_COLOR should use inverse video for chips")
	}

	a.theme = NewTheme(Level16, Dark)
	c16 := a.Render().Render(a.theme)
	if strings.Contains(c16, "38;2;") || strings.Contains(c16, "38;5;") {
		t.Fatal("16-colour output uses extended colours")
	}
	if !strings.Contains(c16, "\x1b[1;35m◆") {
		t.Error("waiting should be bold magenta on 16 colours")
	}
}

type recordingSource struct {
	*DemoSource
	answers []*v1.PromptAnswer
	stops   []string
}

func (r *recordingSource) Answer(_ context.Context, id string, ans *v1.PromptAnswer) error {
	r.answers = append(r.answers, ans)
	return nil
}

func (r *recordingSource) Stop(_ context.Context, id, _ string) error {
	r.stops = append(r.stops, id)
	return nil
}

func run(a *App, k string) {
	if cmd := a.key(k); cmd != nil {
		a.Update(cmd())
	}
}

// §12: a waiting agent is answerable with y / n from grid, list and detail;
// x always asks to confirm.
func TestAcceptanceAnswerEverywhere(t *testing.T) {
	src := &recordingSource{DemoSource: NewDemoSource()}
	a := demoApp(t, 160, 44, nil)
	a.src = src

	run(a, "y") // grid
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	run(a, "n") // list
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 44})
	press(a, "enter")
	run(a, "y") // detail
	if len(src.answers) != 3 || !src.answers[0].Approved || src.answers[1].Approved || src.answers[2].PromptId != "p-bash" {
		t.Fatalf("answers = %v", src.answers)
	}

	run(a, "x")
	if a.in.kind != inputConfirmStop || len(src.stops) != 0 {
		t.Fatal("x should ask to confirm first")
	}
	run(a, "y")
	if len(src.stops) != 1 {
		t.Fatal("confirmed stop not sent")
	}
}

// §12: the detail log follows, unpins on k, re-pins on f.
func TestAcceptanceDetailLog(t *testing.T) {
	a := demoApp(t, 160, 44, nil)
	press(a, "enter")
	d := a.detail
	for i := 0; i < 100; i++ {
		a.Update(logMsg{gen: d.gen, ev: logEvent(d.id, testNow, "edit", fmt.Sprintf("src/file%d.ts", i), 0)})
	}
	c := a.Render()
	if row(c, "LOG · FOLLOWING") < 0 || row(c, "src/file99.ts") < 0 {
		t.Fatalf("not following:\n%s", c.Plain())
	}
	press(a, "k", "k")
	c = a.Render()
	if row(c, "LOG · SCROLLED") < 0 || row(c, "src/file99.ts") >= 0 {
		t.Fatalf("k didn't unpin:\n%s", c.Plain())
	}
	a.Update(logMsg{gen: d.gen, ev: logEvent(d.id, testNow, "bash", "make test", 0)})
	if c = a.Render(); row(c, "make test") >= 0 {
		t.Fatal("a scrolled log should keep its position")
	}
	press(a, "f")
	if c = a.Render(); row(c, "make test") < 0 {
		t.Fatal("f should re-pin to the bottom")
	}
	press(a, "3")
	if c = a.Render(); row(c, "TESTS · ✓118 ✗0") < 0 {
		t.Fatalf("tests artefact tab:\n%s", c.Plain())
	}
	press(a, "esc")
	if a.screen != screenFleet || a.detail != nil {
		t.Fatal("esc should return to the fleet")
	}
}

func timestampAt(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
