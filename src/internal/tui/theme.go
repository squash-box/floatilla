// Package tui is the fleet manager terminal UI (fleet-handoff/SPEC.md). It
// draws every screen onto a cell canvas; Bubble Tea handles input, resizes
// and diffing the frames onto the terminal.
package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Token is a Flotilla colour token (fleet-handoff/design/tokens.json).
type Token uint8

const (
	None Token = iota // terminal default
	Ground
	Pane
	PaneRaised
	Selection
	Line
	Ink
	InkMuted
	InkFaint
	Accent
	AccentInk
	StatusRunning
	StatusSuccess
	StatusFailed
	StatusWaiting
	StatusPaused
	DiffAddBg
	DiffAddInk
	DiffDelBg
	DiffDelInk
	tokenCount
)

type Palette [tokenCount]string

var Dark = Palette{
	Ground: "#0f1214", Pane: "#171b1f", PaneRaised: "#21272c", Selection: "#2e2618", Line: "#343b42",
	Ink: "#e6e2d8", InkMuted: "#9aa1a8", InkFaint: "#5d656d", Accent: "#f2a93b", AccentInk: "#0f1214",
	StatusRunning: "#6cb6ff", StatusSuccess: "#7bd88f", StatusFailed: "#f2777a", StatusWaiting: "#c99bf0", StatusPaused: "#9aa1a8",
	DiffAddBg: "#15291b", DiffAddInk: "#a8e6b4", DiffDelBg: "#33191b", DiffDelInk: "#ffb3ad",
}

var Light = Palette{
	Ground: "#f6f4ef", Pane: "#ffffff", PaneRaised: "#ece8df", Selection: "#f8e9cf", Line: "#d3cec3",
	Ink: "#1c1f22", InkMuted: "#5c636a", InkFaint: "#a19b90", Accent: "#a35f00", AccentInk: "#ffffff",
	StatusRunning: "#1f6fb2", StatusSuccess: "#2b7a3b", StatusFailed: "#b3261e", StatusWaiting: "#7d3aa8", StatusPaused: "#5c636a",
	DiffAddBg: "#e3f4e6", DiffAddInk: "#1d5e2b", DiffDelBg: "#fbe4e2", DiffDelInk: "#8f1d16",
}

// ColorLevel is the capability ladder of SPEC §9.
type ColorLevel uint8

const (
	LevelNone ColorLevel = iota
	Level16
	Level256
	LevelTrue
)

func DetectColorLevel(getenv func(string) string) ColorLevel {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch {
	case getenv("NO_COLOR") != "":
		return LevelNone
	case getenv("COLORTERM") == "truecolor" || getenv("COLORTERM") == "24bit":
		return LevelTrue
	case strings.Contains(getenv("TERM"), "256"):
		return Level256
	default:
		return Level16
	}
}

// ansi16 holds the 16-colour fallbacks: foreground SGR codes. Tokens not
// listed use the terminal default.
var ansi16FG = map[Token]int{
	StatusRunning: 34, StatusWaiting: 35, StatusSuccess: 32, StatusFailed: 31, StatusPaused: 90,
	InkMuted: 90, InkFaint: 90, Line: 90, Accent: 33, AccentInk: 30, DiffAddInk: 32, DiffDelInk: 31,
}

// ansi16BG: only chips get a background on 16-colour terminals.
var ansi16BG = map[Token]int{Accent: 43, StatusWaiting: 45}

// Style is a cell style. BG None means "keep whatever is underneath".
type Style struct {
	FG, BG  Token
	Bold    bool
	Inverse bool
	// Chip marks accent/status fills that must stay visible without colour
	// (rendered inverse under NO_COLOR).
	Chip bool
}

// Theme renders styles to SGR sequences for one palette and colour level.
type Theme struct {
	Level   ColorLevel
	Palette Palette
	// PaintGround fills unstyled cells with Ground (only when owning the
	// whole screen, and only with real colours).
	PaintGround bool
	cache       map[Style]string
}

func NewTheme(level ColorLevel, p Palette) *Theme {
	return &Theme{Level: level, Palette: p, PaintGround: level >= Level256, cache: map[Style]string{}}
}

// SGR returns the escape sequence that sets s from a reset state.
func (t *Theme) SGR(s Style) string {
	if seq, ok := t.cache[s]; ok {
		return seq
	}
	var codes []string
	if s.Bold {
		codes = append(codes, "1")
	}
	switch t.Level {
	case LevelNone:
		if s.Inverse || s.Chip || s.BG == Selection {
			codes = append(codes, "7")
		}
	case Level16:
		if s.Inverse {
			codes = append(codes, "7")
		}
		if c, ok := ansi16FG[s.FG]; ok {
			codes = append(codes, strconv.Itoa(c))
		}
		if c, ok := ansi16BG[s.BG]; ok {
			codes = append(codes, strconv.Itoa(c))
		} else if s.Chip {
			codes = append(codes, "7")
		}
	default:
		if s.Inverse {
			codes = append(codes, "7")
		}
		if s.FG != None {
			codes = append(codes, t.color(s.FG, 38))
		}
		bg := s.BG
		if bg == None && t.PaintGround {
			bg = Ground
		}
		if bg != None {
			codes = append(codes, t.color(bg, 48))
		}
	}
	seq := ""
	if len(codes) > 0 {
		seq = "\x1b[" + strings.Join(codes, ";") + "m"
	}
	t.cache[s] = seq
	return seq
}

func (t *Theme) color(tok Token, base int) string {
	r, g, b := hexRGB(t.Palette[tok])
	if t.Level == LevelTrue {
		return fmt.Sprintf("%d;2;%d;%d;%d", base, r, g, b)
	}
	return fmt.Sprintf("%d;5;%d", base, xterm256(r, g, b))
}

func hexRGB(h string) (r, g, b int) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(h, "#"), 16, 32)
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)
}

// xterm256 quantises to the nearest colour of the 6×6×6 cube or grey ramp.
func xterm256(r, g, b int) int {
	levels := []int{0, 95, 135, 175, 215, 255}
	nearest := func(v int) int {
		best := 0
		for i, l := range levels {
			if abs(v-l) < abs(v-levels[best]) {
				best = i
			}
		}
		return best
	}
	ri, gi, bi := nearest(r), nearest(g), nearest(b)
	cube := 16 + 36*ri + 6*gi + bi
	cubeDist := sq(r-levels[ri]) + sq(g-levels[gi]) + sq(b-levels[bi])

	grey := (r + g + b) / 3
	gIdx := max(0, min(23, (grey-8+5)/10))
	gv := 8 + 10*gIdx
	greyDist := sq(r-gv) + sq(g-gv) + sq(b-gv)
	if greyDist < cubeDist {
		return 232 + gIdx
	}
	return cube
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sq(v int) int { return v * v }
