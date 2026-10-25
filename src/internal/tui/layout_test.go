package tui

import "testing"

func lanes(plan, impl, deploy int, emptyLong ...string) []LaneIn {
	long := map[string]bool{}
	for _, n := range emptyLong {
		long[n] = true
	}
	return []LaneIn{
		{Name: "plan", Count: plan, EmptyLongEnough: long["plan"]},
		{Name: "impl", Count: impl, EmptyLongEnough: long["impl"]},
		{Name: "deploy", Count: deploy, EmptyLongEnough: long["deploy"]},
	}
}

func tileCols(l Layout) []int {
	var out []int
	for _, ln := range l.Lanes {
		out = append(out, ln.TileCols)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The expected results at the bottom of fleet-handoff/reference/layout.ts.
func TestComputeLayoutMatchesMockups(t *testing.T) {
	// Main.dc.html
	l := ComputeLayout(160, 44, lanes(2, 6, 2), false)
	if l.Mode != ModeGrid || l.Tile != TileFull || l.Rows != 3 || !eq(tileCols(l), []int{1, 2, 1}) || !l.ShowFilterRow {
		t.Fatalf("160×44: %+v", l)
	}
	for _, ln := range l.Lanes {
		if ln.Overflow != 0 {
			t.Fatalf("160×44 overflow: %+v", ln)
		}
	}

	// Grid-120.dc.html
	l = ComputeLayout(120, 36, lanes(2, 6, 2), false)
	if l.Mode != ModeGrid || l.Tile != TileCompact || l.Rows != 3 || !eq(tileCols(l), []int{1, 1, 1}) || l.Lanes[1].Overflow != 3 {
		t.Fatalf("120×36: %+v", l)
	}

	// Lanes-Collapsed.dc.html
	l = ComputeLayout(160, 44, lanes(2, 8, 0, "deploy"), false)
	if l.Mode != ModeGrid || l.Rows != 3 || !l.Lanes[2].Collapsed || l.Lanes[0].TileCols != 1 || l.Lanes[1].TileCols != 3 {
		t.Fatalf("collapsed: %+v", l)
	}

	// Grid-80, TooSmall, Fleet-Empty
	if l := ComputeLayout(80, 24, lanes(2, 6, 2), false); l.Mode != ModeList {
		t.Fatalf("80×24: %+v", l)
	}
	if l := ComputeLayout(60, 16, lanes(2, 6, 2), false); l.Mode != ModeTooSmall {
		t.Fatalf("60×16: %+v", l)
	}
	if l := ComputeLayout(160, 44, lanes(0, 0, 0, "plan", "impl", "deploy"), false); l.Mode != ModeEmpty {
		t.Fatalf("empty: %+v", l)
	}
}

func TestComputeLayoutEdges(t *testing.T) {
	// 100 cols: T = max(3, 2) = 3; 90 usable cols over three single-tile
	// lanes gives 30-col tiles (SPEC §6.2 estimates "about 28").
	l := ComputeLayout(100, 44, lanes(2, 6, 2), false)
	if !eq(tileCols(l), []int{1, 1, 1}) {
		t.Fatalf("100 cols: %+v", l)
	}
	rects := placeLanes(100, l)
	if w := rects[0].TileW[0]; w != 30 {
		t.Fatalf("100-col tile width = %d", w)
	}

	// Pinned lanes never collapse.
	if l := ComputeLayout(160, 44, lanes(0, 0, 0, "plan", "impl", "deploy"), true); l.Mode != ModeGrid {
		t.Fatalf("pinned: %+v", l)
	}

	// Very wide: capped at 6 tile columns and centred.
	l = ComputeLayout(400, 44, lanes(2, 6, 2), false)
	if l.TotalTileCols != maxTileCols {
		t.Fatalf("400 cols: T = %d", l.TotalTileCols)
	}
	rects = placeLanes(400, l)
	if rects[0].X <= margin || rects[0].TileW[0] > maxTileWidth {
		t.Fatalf("400 cols not centred: %+v", rects)
	}

	// Too few rows for two compact rows → list.
	if l := ComputeLayout(160, 24, lanes(2, 6, 2), false); l.Mode != ModeList {
		t.Fatalf("160×24: %+v", l)
	}
}

func TestPlaceLanesFillsWidth(t *testing.T) {
	l := ComputeLayout(160, 44, lanes(2, 6, 2), false)
	rects := placeLanes(160, l)
	last := rects[len(rects)-1]
	if end := last.X + last.W; end != 160-margin {
		t.Fatalf("lanes end at %d, want %d", end, 160-margin)
	}
}
