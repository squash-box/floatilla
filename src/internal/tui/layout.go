package tui

// Layout maths for the fleet screen (SPEC §6), ported from
// fleet-handoff/reference/layout.ts and generalised to any number of lanes.

const (
	margin          = 2
	laneGap         = 3
	tileGap         = 2
	rowGap          = 1
	minTileCols     = 30
	railCols        = 14
	fullTileRows    = 11
	compactTileRows = 9
	filterRowMin    = 40
	minCols         = 80
	minRows         = 24
	gridMinCols     = 100
	// maxTileCols caps tile columns on very wide terminals (SPEC §11.6); the
	// grid is centred instead of stretching tiles past maxTileWidth.
	maxTileCols  = 6
	maxTileWidth = 48
)

type Mode uint8

const (
	ModeTooSmall Mode = iota
	ModeList
	ModeEmpty
	ModeGrid
)

type TileSize uint8

const (
	TileFull TileSize = iota
	TileCompact
)

type LaneIn struct {
	Name string
	// Count of agents holding the lane open (incl. done tiles not aged out).
	Count int
	// EmptyLongEnough: empty for ≥30 s (the caller owns the debounce).
	EmptyLongEnough bool
}

type LaneLayout struct {
	Name      string
	Collapsed bool
	TileCols  int
	Capacity  int
	Overflow  int
}

type Layout struct {
	Mode          Mode
	Tile          TileSize
	Rows          int
	ShowFilterRow bool
	TotalTileCols int
	Lanes         []LaneLayout
}

// ComputeLayout decides the fleet screen mode and lane allocation.
func ComputeLayout(cols, rows int, lanes []LaneIn, pinOpen bool) Layout {
	if cols < minCols || rows < minRows {
		return Layout{Mode: ModeTooSmall}
	}
	collapsed := make([]bool, len(lanes))
	nCollapsed := 0
	for i, l := range lanes {
		if !pinOpen && l.Count == 0 && l.EmptyLongEnough {
			collapsed[i] = true
			nCollapsed++
		}
	}
	if nCollapsed == len(lanes) {
		return Layout{Mode: ModeEmpty}
	}
	if cols < gridMinCols {
		return Layout{Mode: ModeList}
	}

	showFilter := rows >= filterRowMin
	bodyRows := rows - 2 - 2 - 3
	if showFilter {
		bodyRows -= 2
	}
	fullRows := (bodyRows + rowGap) / (fullTileRows + rowGap)
	compactRows := (bodyRows + rowGap) / (compactTileRows + rowGap)
	var tile TileSize
	var tileRows int
	switch {
	case fullRows >= 3:
		tile, tileRows = TileFull, fullRows
	case compactRows >= 2:
		tile, tileRows = TileCompact, compactRows
	default:
		return Layout{Mode: ModeList}
	}

	var open []int
	for i := range lanes {
		if !collapsed[i] {
			open = append(open, i)
		}
	}
	available := cols - 2*margin - (len(lanes)-1)*laneGap - nCollapsed*railCols
	T := max(len(open), (available+tileGap)/(minTileCols+tileGap))
	T = max(len(open), min(T, maxTileCols))

	ideal := make([]float64, len(open))
	alloc := make([]int, len(open))
	sum := 0
	for _, i := range open {
		sum += max(1, lanes[i].Count)
	}
	total := 0
	for k, i := range open {
		ideal[k] = float64(T) * float64(max(1, lanes[i].Count)) / float64(sum)
		alloc[k] = max(1, int(ideal[k]))
		total += alloc[k]
	}
	for total < T { // largest remainder gets the next column
		best := 0
		for k := 1; k < len(alloc); k++ {
			if ideal[k]-float64(alloc[k]) > ideal[best]-float64(alloc[best]) {
				best = k
			}
		}
		alloc[best]++
		total++
	}
	for total > T {
		worst := -1
		for k := range alloc {
			if alloc[k] <= 1 {
				continue
			}
			if worst < 0 || ideal[k]-float64(alloc[k]) < ideal[worst]-float64(alloc[worst]) {
				worst = k
			}
		}
		if worst < 0 {
			break
		}
		alloc[worst]--
		total--
	}

	out := Layout{Mode: ModeGrid, Tile: tile, Rows: tileRows, ShowFilterRow: showFilter, TotalTileCols: T}
	k := 0
	for i, l := range lanes {
		if collapsed[i] {
			out.Lanes = append(out.Lanes, LaneLayout{Name: l.Name, Collapsed: true})
			continue
		}
		capacity := alloc[k] * tileRows
		out.Lanes = append(out.Lanes, LaneLayout{
			Name: l.Name, TileCols: alloc[k], Capacity: capacity, Overflow: max(0, l.Count-capacity),
		})
		k++
	}
	return out
}

// laneRect is the horizontal extent of one lane on screen.
type laneRect struct {
	X, W int
	// TileX/TileW per tile column (open lanes only).
	TileX, TileW []int
}

// placeLanes turns a grid layout into x positions. Tile widths share the
// available space evenly; past maxTileWidth the grid is centred.
func placeLanes(cols int, l Layout) []laneRect {
	nCollapsed, open := 0, 0
	for _, ln := range l.Lanes {
		if ln.Collapsed {
			nCollapsed++
		} else {
			open++
		}
	}
	T := l.TotalTileCols
	tileSpace := cols - 2*margin - (len(l.Lanes)-1)*laneGap - nCollapsed*railCols - (T-open)*tileGap
	x := margin
	if tileSpace > T*maxTileWidth {
		x += (tileSpace - T*maxTileWidth) / 2
		tileSpace = T * maxTileWidth
	}
	rects := make([]laneRect, len(l.Lanes))
	tile := 0
	for i, ln := range l.Lanes {
		if i > 0 {
			x += laneGap
		}
		if ln.Collapsed {
			rects[i] = laneRect{X: x, W: railCols}
			x += railCols
			continue
		}
		r := laneRect{X: x}
		for c := 0; c < ln.TileCols; c++ {
			if c > 0 {
				x += tileGap
			}
			w := (tile+1)*tileSpace/T - tile*tileSpace/T
			r.TileX = append(r.TileX, x)
			r.TileW = append(r.TileW, w)
			x += w
			tile++
		}
		r.W = x - r.X
		rects[i] = r
	}
	return rects
}
