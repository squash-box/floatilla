package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

// frameInterval drives the spinner and caps redraws at 10 fps (SPEC §10).
const frameInterval = 100 * time.Millisecond

type screen uint8

const (
	screenFleet screen = iota
	screenDetail
)

type inputKind uint8

const (
	inputNone inputKind = iota
	inputFilter
	inputAnswer
	inputNewAgent
	inputConfirmStop
)

type input struct {
	kind   inputKind
	text   string
	taskID string
	lane   string
}

// selection is a lane plus, optionally, a task in it. An empty task id
// selects the lane itself (an empty lane or a collapsed rail).
type selection struct {
	lane string
	id   string
}

type detailState struct {
	id        string
	gen       int
	cancel    context.CancelFunc
	logs      []*v1.AgentEvent
	artefact  int
	follow    bool
	top       int
	railFocus bool
}

// Options configure the app.
type Options struct {
	Source Source
	Level  ColorLevel
	// Palette, when set, fixes the theme; otherwise it follows the
	// terminal background (dark unless it reports light).
	Palette  *Palette
	Settings *Settings
	Now      func() time.Time
}

// App is the Bubble Tea model.
type App struct {
	src        Source
	theme      *Theme
	themeFixed bool
	settings   *Settings
	now        func() time.Time

	w, h  int
	frame int
	fleet *Fleet

	loaded    bool
	connErr   string
	screen    screen
	listView  bool
	ungrouped bool
	sel       selection
	filter    string
	in        input
	help      bool
	flash     string
	flashErr  bool
	flashAt   time.Time

	emptyCursor int
	historyAll  bool

	detail *detailState
	gen    int

	ctx    context.Context
	cancel context.CancelFunc
	events chan tea.Msg
}

func New(o Options) *App {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Settings == nil {
		o.Settings = &Settings{}
	}
	p, fixed := Dark, false
	if o.Palette != nil {
		p, fixed = *o.Palette, true
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		src: o.Source, theme: NewTheme(o.Level, p), themeFixed: fixed, settings: o.Settings, now: o.Now,
		w: 160, h: 44, fleet: NewFleet(), ctx: ctx, cancel: cancel, events: make(chan tea.Msg, 4096),
	}
}

// Messages.

type snapshotMsg struct {
	types []*v1.ProcessTypeSpec
	tasks []*v1.Task
}
type taskMsg struct{ task *v1.Task }
type connMsg struct{ err error }
type detailTaskMsg struct {
	gen  int
	task *v1.Task
}
type logMsg struct {
	gen int
	ev  *v1.AgentEvent
}
type tickMsg time.Time
type doneMsg struct {
	ok  string
	err error
}

func (a *App) Init() tea.Cmd {
	go a.runFleet()
	cmds := []tea.Cmd{a.wait(), a.tick()}
	if !a.themeFixed && a.theme.Level >= Level256 {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

func (a *App) wait() tea.Cmd {
	return func() tea.Msg {
		select {
		case m := <-a.events:
			return m
		case <-a.ctx.Done():
			return nil
		}
	}
}

func (a *App) tick() tea.Cmd {
	return tea.Tick(frameInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (a *App) send(m tea.Msg) {
	select {
	case a.events <- m:
	case <-a.ctx.Done():
	}
}

// runFleet keeps a snapshot + watch going, reconnecting with backoff.
func (a *App) runFleet() {
	backoff := time.Second
	for a.ctx.Err() == nil {
		types, tasks, err := a.src.Snapshot(a.ctx)
		if err == nil {
			backoff = time.Second
			a.send(snapshotMsg{types, tasks})
			err = a.src.WatchFleet(a.ctx, func(t *v1.Task) { a.send(taskMsg{t}) })
		}
		if a.ctx.Err() != nil {
			return
		}
		a.send(connMsg{err})
		select {
		case <-time.After(backoff):
		case <-a.ctx.Done():
			return
		}
		backoff = min(2*backoff, 15*time.Second)
	}
}

func (a *App) openDetail(id string) {
	a.closeDetail()
	a.gen++
	ctx, cancel := context.WithCancel(a.ctx)
	d := &detailState{id: id, gen: a.gen, cancel: cancel, follow: true}
	a.detail = d
	a.screen = screenDetail
	go func() {
		for ctx.Err() == nil {
			err := a.src.WatchTask(ctx, id,
				func(t *v1.Task) { a.send(detailTaskMsg{d.gen, t}) },
				func(ev *v1.AgentEvent) { a.send(logMsg{d.gen, ev}) })
			if ctx.Err() != nil {
				return
			}
			a.send(connMsg{err})
			// A reconnect replays the log from the start.
			a.send(logMsg{d.gen, nil})
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
		}
	}()
}

func (a *App) closeDetail() {
	if a.detail != nil {
		a.detail.cancel()
		a.detail = nil
	}
	a.screen = screenFleet
}

func (a *App) setFlash(msg string, isErr bool) {
	a.flash, a.flashErr, a.flashAt = msg, isErr, a.now()
}

// action runs a source call off the UI goroutine.
func (a *App) action(ok string, fn func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		defer cancel()
		return doneMsg{ok: ok, err: fn(ctx)}
	}
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = m.Width, m.Height
		return a, nil
	case tea.BackgroundColorMsg:
		if !m.IsDark() {
			a.theme = NewTheme(a.theme.Level, Light)
		}
		return a, nil
	case tickMsg:
		a.frame++
		a.fleet.Tick(a.now())
		if a.flash != "" && a.now().Sub(a.flashAt) > 5*time.Second {
			a.flash = ""
		}
		return a, a.tick()
	case snapshotMsg:
		a.fleet.Load(m.types, m.tasks, a.now())
		a.loaded, a.connErr = true, ""
		a.fixSelection()
		return a, a.wait()
	case taskMsg:
		a.fleet.Upsert(m.task)
		a.connErr = ""
		a.fleet.Tick(a.now())
		a.fixSelection()
		return a, a.wait()
	case connMsg:
		if m.err != nil {
			a.connErr = m.err.Error()
		}
		return a, a.wait()
	case detailTaskMsg:
		if a.detail != nil && m.gen == a.detail.gen {
			a.fleet.Upsert(m.task)
		}
		return a, a.wait()
	case logMsg:
		if a.detail != nil && m.gen == a.detail.gen {
			if m.ev == nil {
				a.detail.logs = nil
			} else {
				a.detail.logs = append(a.detail.logs, m.ev)
			}
		}
		return a, a.wait()
	case doneMsg:
		if m.err != nil {
			a.setFlash(m.err.Error(), true)
		} else if m.ok != "" {
			a.setFlash(m.ok, false)
		}
		return a, nil
	case tea.KeyPressMsg:
		return a, a.key(m.String())
	}
	return a, nil
}

// Quit stops background work; call after the program exits.
func (a *App) Quit() { a.cancel() }

func (a *App) key(k string) tea.Cmd {
	if k == "ctrl+c" {
		a.cancel()
		return tea.Quit
	}
	if a.in.kind != inputNone {
		return a.inputKey(k)
	}
	if a.help {
		if k == "?" || k == "esc" || k == "q" {
			a.help = false
		}
		return nil
	}
	if k == "?" {
		a.help = true
		return nil
	}
	if a.screen == screenDetail {
		return a.detailKey(k)
	}
	return a.fleetKey(k)
}

func (a *App) inputKey(k string) tea.Cmd {
	in := &a.in
	if in.kind == inputConfirmStop {
		id := in.taskID // read before the reset: in points at a.in
		a.in = input{}
		if k == "y" || k == "enter" {
			name := a.taskName(id)
			return a.action("stopped "+name, func(ctx context.Context) error { return a.src.Stop(ctx, id, "stopped from the fleet manager") })
		}
		return nil
	}
	switch k {
	case "esc":
		if in.kind == inputFilter {
			a.filter = ""
		}
		a.in = input{}
		return nil
	case "enter":
		text := strings.TrimSpace(in.text)
		cur := *in
		a.in = input{}
		switch cur.kind {
		case inputFilter:
			a.filter = text
		case inputAnswer:
			if text == "" {
				return nil
			}
			return a.answer(cur.taskID, &v1.PromptAnswer{Approved: true, Text: text})
		case inputNewAgent:
			if text == "" {
				return nil
			}
			name := slug(text)
			if name == "" {
				a.setFlash("title needs some letters or digits", true)
				return nil
			}
			req := &v1.SubmitTaskRequest{ProcessType: cur.lane, Name: name, Title: text}
			return a.action(fmt.Sprintf("submitted %s agent %s", cur.lane, name), func(ctx context.Context) error {
				_, err := a.src.Submit(ctx, req)
				return err
			})
		}
		return nil
	case "backspace":
		if r := []rune(in.text); len(r) > 0 {
			in.text = string(r[:len(r)-1])
		}
	case "ctrl+u":
		in.text = ""
	case "space":
		in.text += " "
	default:
		if len([]rune(k)) == 1 {
			in.text += k
		}
	}
	if in.kind == inputFilter {
		a.filter = in.text
		a.fixSelection()
	}
	return nil
}

func (a *App) taskName(id string) string {
	if t := a.fleet.Task(id); t != nil {
		return t.Name
	}
	return id
}

// answer handles y / n on a waiting task.
func (a *App) answer(id string, ans *v1.PromptAnswer) tea.Cmd {
	t := a.fleet.Task(id)
	if t == nil || t.Status != v1.TaskStatus_TASK_STATUS_WAITING || t.Prompt == nil {
		return nil
	}
	ans.PromptId = t.Prompt.Id
	verb := "denied"
	switch {
	case ans.Text != "":
		verb = "answered"
	case ans.Approved:
		verb = "approved"
	}
	return a.action(verb+" "+t.Name, func(ctx context.Context) error { return a.src.Answer(ctx, id, ans) })
}

// promptKey handles y/n for the selected task in any screen.
func (a *App) promptKey(k string, t *v1.Task) (tea.Cmd, bool) {
	if t == nil || t.Status != v1.TaskStatus_TASK_STATUS_WAITING || t.Prompt == nil {
		return nil, false
	}
	question := t.Prompt.Kind == v1.PromptKind_PROMPT_KIND_QUESTION
	switch {
	case k == "y" && question:
		a.in = input{kind: inputAnswer, taskID: t.Id}
		return nil, true
	case k == "y":
		return a.answer(t.Id, &v1.PromptAnswer{Approved: true}), true
	case k == "n":
		return a.answer(t.Id, &v1.PromptAnswer{Approved: false}), true
	}
	return nil, false
}

// taskKey handles actions on a task shared by fleet and detail screens.
func (a *App) taskKey(k string, t *v1.Task) (tea.Cmd, bool) {
	if t == nil {
		return nil, false
	}
	if cmd, ok := a.promptKey(k, t); ok {
		return cmd, true
	}
	id, name := t.Id, t.Name
	switch k {
	case "x":
		if t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED || t.Status == v1.TaskStatus_TASK_STATUS_FAILED {
			a.setFlash(name+" has already finished", false)
			return nil, true
		}
		a.in = input{kind: inputConfirmStop, taskID: id}
		return nil, true
	case "P":
		return a.action("paused "+name, func(ctx context.Context) error { return a.src.Pause(ctx, id) }), true
	case "r":
		if t.Status != v1.TaskStatus_TASK_STATUS_PAUSED {
			a.setFlash(name+" is not paused", false)
			return nil, true
		}
		return a.action("resumed "+name, func(ctx context.Context) error { return a.src.Resume(ctx, id) }), true
	case "a":
		a.setFlash("attaching to a live session isn't available yet", false)
		return nil, true
	}
	return nil, false
}

func (a *App) selectedTask() *v1.Task {
	if a.sel.id == "" {
		return nil
	}
	return a.fleet.Task(a.sel.id)
}

func (a *App) newAgent(lane string) {
	if a.fleet.Type(lane) == nil && len(a.fleet.Types) > 0 {
		lane = a.fleet.Types[0].Name
	}
	a.in = input{kind: inputNewAgent, lane: lane}
}

func (a *App) fleetKey(k string) tea.Cmd {
	now := a.now()
	lay := a.fleetLayout()
	if lay.Mode == ModeEmpty && !a.listView {
		return a.emptyKey(k)
	}
	switch k {
	case "q":
		a.cancel()
		return tea.Quit
	case "/":
		a.in = input{kind: inputFilter, text: a.filter}
		return nil
	case "esc":
		a.filter = ""
		return nil
	case "v":
		a.listView = !a.listView
		return nil
	case "g":
		a.ungrouped = !a.ungrouped
		return nil
	case "z":
		a.settings.PinLanes = !a.settings.PinLanes
		if err := a.settings.Save(); err != nil {
			a.setFlash("couldn't save settings: "+err.Error(), true)
		} else if a.settings.PinLanes {
			a.setFlash("lanes pinned open", false)
		} else {
			a.setFlash("lanes collapse when empty", false)
		}
		return nil
	case "+", "N":
		a.newAgent(a.sel.lane)
		return nil
	case "enter":
		if t := a.selectedTask(); t != nil {
			a.openDetail(t.Id)
			return nil
		}
		if a.sel.lane != "" {
			a.fleet.Expand(a.sel.lane)
		}
		return nil
	}
	if cmd, ok := a.taskKey(k, a.selectedTask()); ok {
		return cmd
	}
	if a.isList(lay) {
		a.moveList(k, now)
		return nil
	}
	a.moveGrid(k, lay, now)
	return nil
}

func (a *App) emptyKey(k string) tea.Cmd {
	recent := a.fleet.Recent(a.now(), a.historyAll)
	switch k {
	case "q":
		a.cancel()
		return tea.Quit
	case "n", "+", "N":
		a.newAgent("")
		return nil
	case "j", "down":
		a.emptyCursor = min(a.emptyCursor+1, max(0, len(recent)-1))
		return nil
	case "k", "up":
		a.emptyCursor = max(0, a.emptyCursor-1)
		return nil
	case "h":
		a.historyAll = !a.historyAll
		a.emptyCursor = 0
		return nil
	case "enter":
		if a.emptyCursor < len(recent) {
			a.openDetail(recent[a.emptyCursor].Id)
		}
		return nil
	case "r":
		if a.emptyCursor < len(recent) {
			t := recent[a.emptyCursor]
			req := &v1.SubmitTaskRequest{ProcessType: t.ProcessType, Workflow: t.Workflow, Name: t.Name, Title: t.Title, Instructions: t.Instructions, Inputs: t.Inputs}
			return a.action("rerunning "+t.Name, func(ctx context.Context) error {
				_, err := a.src.Submit(ctx, req)
				return err
			})
		}
		return nil
	}
	// p / i / d …: new agent of the type with that initial.
	for _, s := range a.fleet.Types {
		if strings.HasPrefix(s.Name, k) && len(k) == 1 {
			a.newAgent(s.Name)
			return nil
		}
	}
	return nil
}

func (a *App) isList(lay Layout) bool {
	return a.listView || a.ungrouped || lay.Mode == ModeList
}

func (a *App) laneIndex(name string) int {
	for i, s := range a.fleet.Types {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// fixSelection keeps the selection on something that exists.
func (a *App) fixSelection() {
	if len(a.fleet.Types) == 0 {
		return
	}
	now := a.now()
	lanes := a.fleet.Lanes(now, a.filter)
	if t := a.fleet.Task(a.sel.id); t != nil && Visible(t, now) && matches(t, a.filter) {
		a.sel.lane = t.ProcessType
		return
	}
	if a.sel.id != "" || a.laneIndex(a.sel.lane) < 0 {
		// Prefer the first agent that needs a human, then any agent.
		for _, t := range a.fleet.All(now, a.filter) {
			a.sel = selection{lane: t.ProcessType, id: t.Id}
			return
		}
		a.sel = selection{lane: a.fleet.Types[0].Name}
		return
	}
	if ts := lanes[a.sel.lane]; len(ts) > 0 {
		a.sel.id = ts[0].Id
	}
}

func (a *App) moveGrid(k string, lay Layout, now time.Time) {
	if lay.Mode != ModeGrid {
		return
	}
	lanes := a.fleet.Lanes(now, a.filter)
	li := a.laneIndex(a.sel.lane)
	if li < 0 {
		li = 0
	}
	ln := lay.Lanes[li]
	ts := lanes[a.sel.lane]
	idx := -1
	for i, t := range ts {
		if t.Id == a.sel.id {
			idx = i
		}
	}
	cols := max(1, ln.TileCols)
	selectLane := func(i int, fromRow int, atEnd bool) {
		i = (i + len(lay.Lanes)) % len(lay.Lanes)
		name := lay.Lanes[i].Name
		a.sel = selection{lane: name}
		nts := lanes[name]
		if len(nts) == 0 {
			return
		}
		ncols := max(1, lay.Lanes[i].TileCols)
		col := 0
		if atEnd {
			col = ncols - 1
		}
		j := min(len(nts)-1, fromRow*ncols+col)
		a.sel.id = nts[j].Id
	}
	row := 0
	if idx >= 0 {
		row = idx / cols
	}
	switch k {
	case "l", "right":
		if idx >= 0 && idx%cols < cols-1 && idx+1 < len(ts) {
			a.sel.id = ts[idx+1].Id
		} else {
			selectLane(li+1, row, false)
		}
	case "h", "left":
		if idx >= 0 && idx%cols > 0 {
			a.sel.id = ts[idx-1].Id
		} else {
			selectLane(li-1, row, true)
		}
	case "j", "down":
		if idx >= 0 && idx+cols < len(ts) {
			a.sel.id = ts[idx+cols].Id
		}
	case "k", "up":
		if idx >= cols {
			a.sel.id = ts[idx-cols].Id
		}
	case "]":
		selectLane(li+1, 0, false)
	case "[":
		selectLane(li-1, 0, false)
	}
}

// listRows is the order of rows in list mode.
func (a *App) listTasks(now time.Time) []*v1.Task {
	if a.ungrouped {
		return a.fleet.All(now, a.filter)
	}
	lanes := a.fleet.Lanes(now, a.filter)
	var out []*v1.Task
	for _, s := range a.fleet.Types {
		out = append(out, lanes[s.Name]...)
	}
	return out
}

func (a *App) moveList(k string, now time.Time) {
	ts := a.listTasks(now)
	if len(ts) == 0 {
		return
	}
	idx := 0
	for i, t := range ts {
		if t.Id == a.sel.id {
			idx = i
		}
	}
	switch k {
	case "j", "down":
		idx = min(idx+1, len(ts)-1)
	case "k", "up":
		idx = max(idx-1, 0)
	case "]", "[":
		// jump to the next / previous group
		cur := ts[idx].ProcessType
		step := 1
		if k == "[" {
			step = -1
		}
		for i := idx + step; i >= 0 && i < len(ts); i += step {
			if ts[i].ProcessType != cur {
				idx = i
				break
			}
		}
	default:
		return
	}
	a.sel = selection{lane: ts[idx].ProcessType, id: ts[idx].Id}
}

func (a *App) detailKey(k string) tea.Cmd {
	d := a.detail
	t := a.fleet.Task(d.id)
	tabs := a.artefactTabs(t)
	switch k {
	case "esc", "q":
		a.closeDetail()
		return nil
	case "tab", "shift+tab":
		if a.w >= gridMinCols {
			d.railFocus = !d.railFocus
		}
		return nil
	case "f", "G":
		d.follow = true
		return nil
	case "j", "down":
		if d.railFocus {
			d.artefact = min(d.artefact+1, len(tabs)-1)
		} else {
			d.top++
		}
		return nil
	case "k", "up":
		if d.railFocus {
			d.artefact = max(d.artefact-1, 0)
		} else {
			d.follow = false
			d.top = max(0, d.top-1)
		}
		return nil
	case "pgdown", "ctrl+d", "space":
		d.top += max(1, a.h/2)
		return nil
	case "pgup", "ctrl+u":
		d.follow = false
		d.top = max(0, d.top-max(1, a.h/2))
		return nil
	case "g", "home":
		d.follow = false
		d.top = 0
		return nil
	case "]", "[":
		a.setFlash("file navigation needs diff content, which the backend doesn't publish yet", false)
		return nil
	}
	if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
		if i := int(k[0] - '1'); i < len(tabs) {
			d.artefact = i
		}
		return nil
	}
	cmd, _ := a.taskKey(k, t)
	return cmd
}

// artefactTabs: the log, then the type's declared artefacts.
func (a *App) artefactTabs(t *v1.Task) []string {
	tabs := []string{"log"}
	if t == nil {
		return tabs
	}
	if spec := a.fleet.Type(t.ProcessType); spec != nil {
		for _, k := range spec.Artefacts {
			if k != "log" && !contains(tabs, k) {
				tabs = append(tabs, k)
			}
		}
	}
	for _, ar := range t.Artefacts {
		if !contains(tabs, ar.Kind) {
			tabs = append(tabs, ar.Kind)
		}
	}
	if len(tabs) > 9 {
		tabs = tabs[:9]
	}
	return tabs
}

func (a *App) fleetLayout() Layout {
	now := a.now()
	lanes := a.fleet.Lanes(now, a.filter)
	return ComputeLayout(a.w, a.h, a.fleet.LaneInputs(now, lanes), a.settings.PinLanes)
}

func (a *App) View() tea.View {
	v := tea.NewView(a.Render().Render(a.theme))
	v.AltScreen = true
	v.WindowTitle = "floatilla"
	return v
}
