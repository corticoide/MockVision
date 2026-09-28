package domain

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Video analytics (VCA): the rules drawn on a camera's picture say where
// events happen, and its triggers make them happen (D39, D40). The profile
// says which kinds of rule and which object classes the device has, which
// events it can deliver and which kind of rule each comes from; the camera
// decides where its rules are and what fires them. Nothing here assumes an
// analytic a profile does not declare.

// RuleType is the geometry of a VCA rule.
type RuleType string

const (
	// RuleLine is a virtual line: objects crossing it raise its events,
	// line_crossing and whatever line events the profile declares.
	RuleLine RuleType = "line"
	// RuleRegion is a polygon: objects entering, leaving, loitering in or
	// intruding into it raise its events.
	RuleRegion RuleType = "region"
)

// Crossings a line rule reports. The sides of a line are those of the
// segment from its first point to its second: A on the left, B on the
// right, as a real camera draws them.
const (
	CrossAB   = "A->B"
	CrossBA   = "B->A"
	CrossBoth = "both"
)

// RegionEvents are the canonical events of regions: an object enters,
// leaves, stays (loitering) or intrudes.
var RegionEvents = []EventType{EventRegionEntrance, EventRegionExit, EventLoitering, EventIntrusion}

// Limits of a camera's analytics.
const (
	MaxRules          = 16
	MaxTriggers       = 16
	MaxRegionPoints   = 20
	MaxVCANameLength  = 32
	MaxObjectClasses  = 16
	MaxPlates         = 100
	MaxPlateLength    = 16
	MaxPlateMasks     = 10
	MinTriggerSeconds = 1
	MaxTriggerSeconds = 24 * 60 * 60
	MaxSpeed          = 500
)

// A line shorter than this, a region side shorter than minRegionSide or a
// region smaller than this share of the picture is too small to draw.
const (
	minLineLength = 0.02
	minRegionSide = 0.005
	minRegionArea = 0.0004
)

// Point is a position on the picture, from 0 to 1 from its top left corner:
// rules stay in place when the resolution changes.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Rule is a VCA rule of a camera (D39).
type Rule struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Type   RuleType `json:"type"`
	Points []Point  `json:"points"`
	// Direction is which crossings a line reports: A->B, B->A or both.
	// Regions have none.
	Direction string `json:"direction"`
	// Events are what the rule reports: events the profile raises from its
	// kind of rule, such as line_crossing for lines and region_entrance,
	// region_exit, loitering or intrusion for regions.
	Events []string `json:"events"`
	// ObjectClasses are the objects the rule detects; empty means every
	// class of the profile.
	ObjectClasses []string `json:"object_classes"`
	Enabled       bool     `json:"enabled"`
}

// EventTypes lists the events the rule reports. Lines stored before they
// listed their events report line_crossing.
func (r Rule) EventTypes() []string {
	if r.Type == RuleLine && len(r.Events) == 0 {
		return []string{string(EventLineCrossing)}
	}
	return r.Events
}

// Normalized fills what a rule may leave out: a line reports both ways and,
// listing no event, line_crossing; empty lists are not nil.
func (r Rule) Normalized() Rule {
	if r.Type == RuleLine {
		if r.Direction == "" {
			r.Direction = CrossBoth
		}
		r.Events = r.EventTypes()
	}
	if r.Events == nil {
		r.Events = []string{}
	}
	if r.ObjectClasses == nil {
		r.ObjectClasses = []string{}
	}
	return r
}

// Reports tells whether the rule reports events of a type.
func (r Rule) Reports(eventType string) bool {
	return slices.Contains(r.EventTypes(), eventType)
}

// CanonicalRuleType is the kind of rule a canonical event comes from when
// its profile does not say: lines for line_crossing, regions for the region
// events, none for the rest, such as lpr or tamper, and for custom events.
func CanonicalRuleType(eventType string) RuleType {
	switch EventType(eventType) {
	case EventLineCrossing:
		return RuleLine
	case EventRegionEntrance, EventRegionExit, EventLoitering, EventIntrusion:
		return RuleRegion
	}
	return ""
}

// TriggerType is what makes events happen (D40): manual and random in v1;
// schedule, script and external come in v1.1.
type TriggerType string

const (
	// TriggerManual is an event fired from the panel or the API.
	TriggerManual TriggerType = "manual"
	// TriggerRandom emits events at random moments while the camera runs.
	TriggerRandom TriggerType = "random"
)

// Trigger is a stored trigger of a camera. A random one emits an event of
// its type at a random moment between MinSeconds and MaxSeconds after the
// previous one, while it is enabled and the camera runs.
type Trigger struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Type      TriggerType `json:"type"`
	EventType string      `json:"event_type"`
	// RuleID is the rule its events happen on; empty picks, each time,
	// one of the camera's enabled rules that report the type.
	RuleID     string `json:"rule_id"`
	MinSeconds int    `json:"min_seconds"`
	MaxSeconds int    `json:"max_seconds"`
	// Plates are license plates to pick from, and PlateMasks generate
	// them (D41). An event of a trigger with either carries a plate; lpr
	// events always do.
	Plates     []string `json:"plates"`
	PlateMasks []string `json:"plate_masks"`
	// Speed, when set, gives every event a speed in its range.
	Speed   *SpeedRange `json:"speed"`
	Enabled bool        `json:"enabled"`
}

// SpeedRange is where generated speeds fall, for radar profiles (D41).
// Limit is the speed limit the events report; 0 means none.
type SpeedRange struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Limit float64 `json:"limit"`
	Unit  string  `json:"unit"`
}

// SpeedUnits are the units a speed can be in.
var SpeedUnits = []string{"km/h", "mph"}

// VCACaps is what a camera's profile allows: the kinds of rule and the
// object classes of its analytics, the events it can deliver with the
// shortest interval between two of a type, the kind of rule each event
// comes from and which events are reports.
type VCACaps struct {
	RuleTypes []RuleType
	// ObjectClasses the analytics recognize; empty accepts any class.
	ObjectClasses []string
	Events        map[string]time.Duration
	// EventRules maps an event type to the kind of rule it comes from;
	// events missing here come from no rule.
	EventRules map[string]RuleType
	// Reports are events that carry what the camera counted, not an object
	// it saw: they come from no rule and feed no count.
	Reports map[string]bool
}

// Delivers tells whether the camera can send events of a type.
func (c VCACaps) Delivers(eventType string) bool {
	_, ok := c.Events[eventType]
	return ok
}

// RuleTypeFor returns the kind of rule an event type comes from, or "" for
// the events no rule raises.
func (c VCACaps) RuleTypeFor(eventType string) RuleType {
	return c.EventRules[eventType]
}

// RuleEvents lists the deliverable events of a kind of rule, sorted.
func (c VCACaps) RuleEvents(kind RuleType) []string {
	var out []string
	for typ := range c.Events {
		if c.EventRules[typ] == kind {
			out = append(out, typ)
		}
	}
	slices.Sort(out)
	return out
}

// ValidateRules checks a camera's rules against its profile.
func ValidateRules(rules []Rule, caps VCACaps) error {
	v := &ValidationError{}
	if len(rules) > MaxRules {
		v.Add("rules", "a camera can have at most %d rules", MaxRules)
	}
	names := map[string]bool{}
	ids := map[string]bool{}
	for i, r := range rules {
		f := "rules[" + strconv.Itoa(i) + "]"
		if r.ID != "" {
			if ids[r.ID] {
				v.Add(f+".id", "is duplicated")
			}
			ids[r.ID] = true
		}
		validateVCAName(v, f+".name", r.Name, names)
		switch r.Type {
		case RuleLine, RuleRegion:
			if !slices.Contains(caps.RuleTypes, r.Type) {
				v.Add(f+".type", "the camera's profile has no %s rules", r.Type)
			}
		default:
			v.Add(f+".type", "must be line or region")
			continue
		}
		validateGeometry(v, f+".points", r.Type, r.Points)
		if r.Type == RuleLine {
			if r.Direction != CrossAB && r.Direction != CrossBA && r.Direction != CrossBoth {
				v.Add(f+".direction", "must be A->B, B->A or both")
			}
		} else if r.Direction != "" {
			v.Add(f+".direction", "only lines have a direction")
		}
		validateRuleEvents(v, f+".events", r.Type, r.EventTypes(), caps)
		validateClasses(v, f+".object_classes", r.ObjectClasses, caps)
	}
	return v.Err()
}

// ValidateTriggers checks a camera's stored triggers against its rules and
// its profile.
func ValidateTriggers(triggers []Trigger, rules []Rule, caps VCACaps) error {
	v := &ValidationError{}
	if len(triggers) > MaxTriggers {
		v.Add("triggers", "a camera can have at most %d triggers", MaxTriggers)
	}
	byID := make(map[string]Rule, len(rules))
	for _, r := range rules {
		byID[r.ID] = r
	}
	names := map[string]bool{}
	ids := map[string]bool{}
	for i, t := range triggers {
		f := "triggers[" + strconv.Itoa(i) + "]"
		if t.ID != "" {
			if ids[t.ID] {
				v.Add(f+".id", "is duplicated")
			}
			ids[t.ID] = true
		}
		validateVCAName(v, f+".name", t.Name, names)
		if t.Type != TriggerRandom {
			v.Add(f+".type", "must be random")
		}
		minInterval, delivered := caps.Events[t.EventType]
		switch {
		case !ValidEventType(t.EventType):
			v.Add(f+".event_type", "%q is not a canonical event type", t.EventType)
		case !delivered:
			v.Add(f+".event_type", "the camera's profile does not deliver %s events", t.EventType)
		}
		if caps.Reports[t.EventType] && (len(t.Plates)+len(t.PlateMasks) > 0 || t.Speed != nil) {
			v.Add(f+".event_type", "%s is a report: it carries counts, not plates or speeds", t.EventType)
		}
		if t.RuleID != "" {
			r, ok := byID[t.RuleID]
			switch {
			case caps.RuleTypeFor(t.EventType) == "":
				v.Add(f+".rule_id", "%s events do not come from a rule", t.EventType)
			case !ok:
				v.Add(f+".rule_id", "is not a rule of the camera")
			case !r.Reports(t.EventType):
				v.Add(f+".rule_id", "rule %s does not report %s events", r.Name, t.EventType)
			}
		}
		validSeconds := func(n int) bool { return n >= MinTriggerSeconds && n <= MaxTriggerSeconds }
		if !validSeconds(t.MinSeconds) {
			v.Add(f+".min_seconds", "must be between %d and %d seconds", MinTriggerSeconds, MaxTriggerSeconds)
		} else if wait := time.Duration(t.MinSeconds) * time.Second; wait < minInterval {
			v.Add(f+".min_seconds", "must be at least %d s: the profile allows one %s event every %s",
				int(math.Ceil(minInterval.Seconds())), t.EventType, minInterval)
		}
		if !validSeconds(t.MaxSeconds) {
			v.Add(f+".max_seconds", "must be between %d and %d seconds", MinTriggerSeconds, MaxTriggerSeconds)
		} else if t.MaxSeconds < t.MinSeconds {
			v.Add(f+".max_seconds", "must not be less than the minimum")
		}
		validatePlates(v, f, t.Plates, t.PlateMasks)
		if s := t.Speed; s != nil {
			if !(s.Min >= 0 && s.Max <= MaxSpeed && s.Min <= s.Max) {
				v.Add(f+".speed", "must go from 0 to %d, the minimum first", MaxSpeed)
			}
			if !(s.Limit >= 0 && s.Limit <= MaxSpeed) {
				v.Add(f+".speed.limit", "must be between 0 (none) and %d", MaxSpeed)
			}
			if !slices.Contains(SpeedUnits, s.Unit) {
				v.Add(f+".speed.unit", "must be km/h or mph")
			}
		}
	}
	return v.Err()
}

// validateVCAName checks the name of a rule or a trigger, unique among
// seen regardless of case.
func validateVCAName(v *ValidationError, field, name string, seen map[string]bool) {
	switch {
	case strings.TrimSpace(name) == "":
		v.Add(field, "is required")
		return
	case strings.TrimSpace(name) != name:
		v.Add(field, "must not start or end with spaces")
	case utf8.RuneCountInString(name) > MaxVCANameLength:
		v.Add(field, "must be at most %d characters", MaxVCANameLength)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		v.Add(field, "must not contain control characters")
	}
	key := strings.ToLower(name)
	if seen[key] {
		v.Add(field, "is duplicated")
	}
	seen[key] = true
}

func validateGeometry(v *ValidationError, field string, typ RuleType, pts []Point) {
	for _, p := range pts {
		// Written so NaN fails too.
		if !(p.X >= 0 && p.X <= 1 && p.Y >= 0 && p.Y <= 1) {
			v.Add(field, "coordinates must be between 0 and 1")
			return
		}
	}
	if typ == RuleLine {
		switch {
		case len(pts) != 2:
			v.Add(field, "a line has two points")
		case distance(pts[0], pts[1]) < minLineLength:
			v.Add(field, "the line is too short")
		}
		return
	}
	if len(pts) < 3 || len(pts) > MaxRegionPoints {
		v.Add(field, "a region has from 3 to %d points", MaxRegionPoints)
		return
	}
	for i := range pts {
		if distance(pts[i], pts[(i+1)%len(pts)]) < minRegionSide {
			v.Add(field, "two consecutive points are almost the same")
			return
		}
	}
	switch {
	case selfIntersects(pts):
		v.Add(field, "the sides of the region cross each other")
	case polygonArea(pts) < minRegionArea:
		v.Add(field, "the region is too small")
	}
}

// validateRuleEvents checks the events of a rule: at least one, each
// delivered by the profile and coming from the rule's kind.
func validateRuleEvents(v *ValidationError, field string, kind RuleType, events []string, caps VCACaps) {
	choices := caps.RuleEvents(kind)
	if len(events) == 0 {
		if len(choices) == 0 {
			v.Add(field, "the camera's profile raises no event from %s rules", kind)
		} else {
			v.Add(field, "choose at least one of %s", strings.Join(choices, ", "))
		}
		return
	}
	seen := map[string]bool{}
	for _, e := range events {
		switch {
		case seen[e]:
			v.Add(field, "%s is duplicated", e)
		case !caps.Delivers(e):
			v.Add(field, "the camera's profile does not deliver %s events", e)
		case caps.RuleTypeFor(e) != kind:
			v.Add(field, "%s events do not come from %s rules", e, kind)
		}
		seen[e] = true
	}
}

func validateClasses(v *ValidationError, field string, classes []string, caps VCACaps) {
	if len(classes) > MaxObjectClasses {
		v.Add(field, "at most %d classes", MaxObjectClasses)
		return
	}
	seen := map[string]bool{}
	for _, c := range classes {
		switch {
		case c == "" || utf8.RuneCountInString(c) > 32 || strings.IndexFunc(c, unicode.IsControl) >= 0:
			v.Add(field, "a class is 1 to 32 characters")
		case len(caps.ObjectClasses) > 0 && !slices.Contains(caps.ObjectClasses, c):
			v.Add(field, "the camera's profile does not detect %q", c)
		case seen[c]:
			v.Add(field, "%s is duplicated", c)
		}
		seen[c] = true
	}
}

func validatePlates(v *ValidationError, f string, plates, masks []string) {
	if len(plates) > MaxPlates {
		v.Add(f+".plates", "at most %d plates", MaxPlates)
	}
	for _, p := range plates {
		if p == "" || strings.TrimSpace(p) != p || utf8.RuneCountInString(p) > MaxPlateLength || strings.IndexFunc(p, unicode.IsControl) >= 0 {
			v.Add(f+".plates", "%q must be 1 to %d characters, without spaces at the ends", p, MaxPlateLength)
			break
		}
	}
	if len(masks) > MaxPlateMasks {
		v.Add(f+".plate_masks", "at most %d formats", MaxPlateMasks)
	}
	for _, m := range masks {
		if err := ValidateMask(m); err != nil {
			v.Add(f+".plate_masks", "%q: %v", m, err)
			break
		}
		if n := utf8.RuneCountInString(ExpandMask(m, func(int) int { return 0 })); n > MaxPlateLength {
			v.Add(f+".plate_masks", "%q makes plates longer than %d characters", m, MaxPlateLength)
			break
		}
	}
}

// Side tells on which side of the line from a to b a point lies: negative
// on side A (on the left, walking from a to b), positive on side B, zero
// on the line. Coordinates grow rightwards and downwards, as on a picture.
func Side(a, b, p Point) float64 {
	return (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X)
}

// Contains reports whether p lies inside the polygon.
func Contains(poly []Point, p Point) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.Y > p.Y) != (b.Y > p.Y) && p.X < (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y)+a.X {
			in = !in
		}
	}
	return in
}

func distance(a, b Point) float64 {
	return math.Hypot(b.X-a.X, b.Y-a.Y)
}

// polygonArea is the shoelace formula.
func polygonArea(poly []Point) float64 {
	s := 0.0
	for i, a := range poly {
		b := poly[(i+1)%len(poly)]
		s += a.X*b.Y - b.X*a.Y
	}
	return math.Abs(s) / 2
}

// selfIntersects reports whether two sides of a polygon that do not share
// a corner touch or cross.
func selfIntersects(poly []Point) bool {
	n := len(poly)
	for i := 0; i < n; i++ {
		for j := i + 2; j < n; j++ {
			if i == 0 && j == n-1 {
				continue // the closing side shares the first corner
			}
			if segmentsTouch(poly[i], poly[(i+1)%n], poly[j], poly[(j+1)%n]) {
				return true
			}
		}
	}
	return false
}

func segmentsTouch(p1, p2, q1, q2 Point) bool {
	d1, d2 := Side(q1, q2, p1), Side(q1, q2, p2)
	d3, d4 := Side(p1, p2, q1), Side(p1, p2, q2)
	if (d1 > 0 && d2 < 0 || d1 < 0 && d2 > 0) && (d3 > 0 && d4 < 0 || d3 < 0 && d4 > 0) {
		return true
	}
	// A point of one segment lying on the other.
	on := func(a, b, p Point, d float64) bool {
		return d == 0 && math.Min(a.X, b.X) <= p.X && p.X <= math.Max(a.X, b.X) && math.Min(a.Y, b.Y) <= p.Y && p.Y <= math.Max(a.Y, b.Y)
	}
	return on(q1, q2, p1, d1) || on(q1, q2, p2, d2) || on(p1, p2, q1, d3) || on(p1, p2, q2, d4)
}
