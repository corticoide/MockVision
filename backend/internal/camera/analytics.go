package camera

import (
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
)

// The heat grid the camera counts objects on. Heat maps of other sizes are
// summed from it, so a map is exact at any size up to this one.
const (
	heatCols = 128
	heatRows = 72
)

// analytics is what the camera's analytics count from the events it emits,
// as real cameras do for people counting, occupancy and heat maps: the
// crossings of each line by direction and object class, the entries, exits
// and occupancy of each region, the events of each type, and the centers of
// the objects seen, on a grid over the picture. Reports feed nothing. The
// counts live in the camera process and start again when it starts.
type analytics struct {
	mu      sync.Mutex
	since   time.Time
	lines   map[string]*ipc.LineCount
	regions map[string]*ipc.RegionCount
	events  map[string]int
	heat    []int
}

func newAnalytics() *analytics {
	a := &analytics{}
	a.reset()
	return a
}

// reset starts every count again from zero.
func (a *analytics) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.since = time.Now().UTC()
	a.lines = map[string]*ipc.LineCount{}
	a.regions = map[string]*ipc.RegionCount{}
	a.events = map[string]int{}
	a.heat = make([]int, heatCols*heatRows)
}

// record counts an event the camera emitted.
func (a *analytics) record(e engine.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events[e.Type]++
	class := ""
	if e.Object != nil {
		class = e.Object.Class
	}
	if r := e.Rule; r != nil {
		switch domain.RuleType(r.Type) {
		case domain.RuleLine:
			lc := a.lines[r.ID]
			if lc == nil {
				lc = &ipc.LineCount{RuleID: r.ID, Classes: map[string]ipc.DirectionPair{}}
				a.lines[r.ID] = lc
			}
			pair := lc.Classes[class]
			switch e.Direction {
			case domain.CrossAB:
				lc.AToB++
				pair.AToB++
			case domain.CrossBA:
				lc.BToA++
				pair.BToA++
			}
			if class != "" && pair != (ipc.DirectionPair{}) {
				lc.Classes[class] = pair
			}
		case domain.RuleRegion:
			rc := a.regions[r.ID]
			if rc == nil {
				rc = &ipc.RegionCount{RuleID: r.ID, Events: map[string]int{}}
				a.regions[r.ID] = rc
			}
			rc.Events[e.Type]++
			switch domain.EventType(e.Type) {
			case domain.EventRegionEntrance:
				rc.Entries++
				rc.Occupancy++
			case domain.EventRegionExit:
				rc.Exits++
				rc.Occupancy = max(rc.Occupancy-1, 0)
			}
		}
	}
	if e.Object != nil && e.Object.Box != nil {
		b := e.Object.Box
		col := min(max(int((b.X+b.W/2)*heatCols), 0), heatCols-1)
		row := min(max(int((b.Y+b.H/2)*heatRows), 0), heatRows-1)
		a.heat[row*heatCols+col]++
	}
}

// snapshot returns the counts of the camera's rules, in their order and
// with their current names, and the heat map at cols by rows cells (none
// when cols or rows is 0; sizes above the camera's grid take its size).
func (a *analytics) snapshot(rules []domain.Rule, cols, rows int) ipc.Analytics {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := ipc.Analytics{Since: a.since, Lines: []ipc.LineCount{}, Regions: []ipc.RegionCount{}, Events: make(map[string]int, len(a.events))}
	for k, v := range a.events {
		out.Events[k] = v
	}
	for _, r := range rules {
		switch r.Type {
		case domain.RuleLine:
			lc := ipc.LineCount{RuleID: r.ID, Classes: map[string]ipc.DirectionPair{}}
			if c := a.lines[r.ID]; c != nil {
				lc.AToB, lc.BToA = c.AToB, c.BToA
				for k, v := range c.Classes {
					lc.Classes[k] = v
				}
			}
			lc.Name = r.Name
			out.Lines = append(out.Lines, lc)
		case domain.RuleRegion:
			rc := ipc.RegionCount{RuleID: r.ID, Events: map[string]int{}}
			if c := a.regions[r.ID]; c != nil {
				rc.Entries, rc.Exits, rc.Occupancy = c.Entries, c.Exits, c.Occupancy
				for k, v := range c.Events {
					rc.Events[k] = v
				}
			}
			rc.Name = r.Name
			out.Regions = append(out.Regions, rc)
		}
	}
	if cols > 0 && rows > 0 {
		out.Heat = a.heatAt(cols, rows)
	}
	return out
}

// heatAt sums the heat grid into cols by rows cells. The lock must be held.
func (a *analytics) heatAt(cols, rows int) *ipc.Heat {
	cols, rows = min(cols, heatCols), min(rows, heatRows)
	h := &ipc.Heat{Cols: cols, Rows: rows, Cells: make([]int, cols*rows)}
	for y := range heatRows {
		r := y * rows / heatRows
		for x := range heatCols {
			if n := a.heat[y*heatCols+x]; n > 0 {
				h.Cells[r*cols+x*cols/heatCols] += n
			}
		}
	}
	return h
}

// lineCount is the crossings of the line named or identified rule, one way
// (A->B or B->A) or both (any other direction).
func (a *analytics) lineCount(rules []domain.Rule, rule, direction string) int {
	for _, l := range a.snapshot(rules, 0, 0).Lines {
		if l.RuleID == rule || l.Name == rule {
			switch direction {
			case domain.CrossAB:
				return l.AToB
			case domain.CrossBA:
				return l.BToA
			}
			return l.AToB + l.BToA
		}
	}
	return 0
}

// occupancy is the objects inside the region named or identified rule.
func (a *analytics) occupancy(rules []domain.Rule, rule string) int {
	for _, r := range a.snapshot(rules, 0, 0).Regions {
		if r.RuleID == rule || r.Name == rule {
			return r.Occupancy
		}
	}
	return 0
}
