package tui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

type cell struct {
	r rune
	s Style
	// cont marks the second column of a wide rune.
	cont bool
}

// Canvas is a grid of styled cells. Everything the UI draws goes through it,
// so layout is exact to the cell and clipped at the edges.
type Canvas struct {
	W, H  int
	cells []cell
}

func NewCanvas(w, h int) *Canvas {
	c := &Canvas{W: w, H: h, cells: make([]cell, w*h)}
	for i := range c.cells {
		c.cells[i].r = ' '
	}
	return c
}

func (c *Canvas) at(x, y int) *cell {
	if x < 0 || y < 0 || x >= c.W || y >= c.H {
		return nil
	}
	return &c.cells[y*c.W+x]
}

// merge applies s over the cell's style; a None background keeps the cell's.
func merge(under, s Style) Style {
	if s.BG == None {
		s.BG = under.BG
	}
	return s
}

func (c *Canvas) Set(x, y int, r rune, s Style) {
	if p := c.at(x, y); p != nil {
		*p = cell{r: r, s: merge(p.s, s)}
	}
}

// Text draws s at (x, y), clipped to maxW columns (and the canvas). It
// returns the number of columns drawn.
func (c *Canvas) Text(x, y int, s string, st Style, maxW int) int {
	if maxW <= 0 {
		return 0
	}
	used := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if w == 0 {
			continue
		}
		if used+w > maxW {
			break
		}
		c.Set(x+used, y, r, st)
		if w == 2 {
			if p := c.at(x+used+1, y); p != nil {
				*p = cell{r: ' ', s: merge(p.s, st), cont: true}
			}
		}
		used += w
	}
	return used
}

// TextEllipsis draws s, ending in "…" if it doesn't fit in maxW columns.
func (c *Canvas) TextEllipsis(x, y int, s string, st Style, maxW int) int {
	return c.Text(x, y, Ellipsis(s, maxW), st, maxW)
}

// Fill paints a rectangle's background (and clears its runes).
func (c *Canvas) Fill(x, y, w, h int, st Style) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			if p := c.at(i, j); p != nil {
				*p = cell{r: ' ', s: st}
			}
		}
	}
}

// FillBG sets the background of a rectangle, keeping its content.
func (c *Canvas) FillBG(x, y, w, h int, bg Token) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			if p := c.at(i, j); p != nil {
				p.s.BG = bg
			}
		}
	}
}

func (c *Canvas) HLine(x, y, w int, r rune, st Style) {
	for i := 0; i < w; i++ {
		c.Set(x+i, y, r, st)
	}
}

func (c *Canvas) VLine(x, y, h int, r rune, st Style) {
	for j := 0; j < h; j++ {
		c.Set(x, y+j, r, st)
	}
}

type BoxKind uint8

const (
	BoxLight BoxKind = iota
	BoxRounded
	BoxDashed
)

var boxRunes = map[BoxKind][6]rune{
	BoxLight:   {'┌', '┐', '└', '┘', '─', '│'},
	BoxRounded: {'╭', '╮', '╰', '╯', '─', '│'},
	BoxDashed:  {'┄', '┄', '┄', '┄', '┄', '┆'},
}

// Box draws a border around the rectangle (x, y, w, h).
func (c *Canvas) Box(x, y, w, h int, kind BoxKind, st Style) {
	if w < 2 || h < 2 {
		return
	}
	b := boxRunes[kind]
	c.Set(x, y, b[0], st)
	c.Set(x+w-1, y, b[1], st)
	c.Set(x, y+h-1, b[2], st)
	c.Set(x+w-1, y+h-1, b[3], st)
	c.HLine(x+1, y, w-2, b[4], st)
	c.HLine(x+1, y+h-1, w-2, b[4], st)
	c.VLine(x, y+1, h-2, b[5], st)
	c.VLine(x+w-1, y+1, h-2, b[5], st)
}

// Render serialises the canvas to a string of rows with SGR styling.
func (c *Canvas) Render(t *Theme) string {
	var sb strings.Builder
	sb.Grow(c.W * c.H * 2)
	for y := 0; y < c.H; y++ {
		cur := ""
		for x := 0; x < c.W; x++ {
			p := c.cells[y*c.W+x]
			if p.cont {
				continue
			}
			if seq := t.SGR(p.s); seq != cur {
				if cur != "" {
					sb.WriteString("\x1b[0m")
				}
				sb.WriteString(seq)
				cur = seq
			}
			sb.WriteRune(p.r)
		}
		if cur != "" {
			sb.WriteString("\x1b[0m")
		}
		if y < c.H-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// Row returns row y as plain text (tests and debugging).
func (c *Canvas) Row(y int) string {
	var sb strings.Builder
	for x := 0; x < c.W; x++ {
		if p := c.cells[y*c.W+x]; !p.cont {
			sb.WriteRune(p.r)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

// Plain returns the whole canvas as plain text.
func (c *Canvas) Plain() string {
	rows := make([]string, c.H)
	for y := range rows {
		rows[y] = c.Row(y)
	}
	return strings.Join(rows, "\n")
}

// StyleAt returns the style of a cell (tests).
func (c *Canvas) StyleAt(x, y int) Style {
	if p := c.at(x, y); p != nil {
		return p.s
	}
	return Style{}
}

// Width is the display width of s in columns.
func Width(s string) int { return runewidth.StringWidth(s) }

// Ellipsis shortens s to at most w columns, ending in "…" when cut.
func Ellipsis(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= w {
		return s
	}
	return runewidth.Truncate(s, w, "…")
}

// Span is a run of text in one style; rows are built from spans.
type Span struct {
	Text  string
	Style Style
}

// Spans draws spans left to right, clipped to maxW. Returns columns used.
func (c *Canvas) Spans(x, y int, spans []Span, maxW int) int {
	used := 0
	for _, sp := range spans {
		if used >= maxW {
			break
		}
		used += c.Text(x+used, y, sp.Text, sp.Style, maxW-used)
	}
	return used
}

func spansWidth(spans []Span) int {
	w := 0
	for _, sp := range spans {
		w += Width(sp.Text)
	}
	return w
}
