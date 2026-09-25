package engine

import "time"

// Event is the canonical event. The core always produces this structure;
// each profile decides how the event is named and serialized for its vendor.
type Event struct {
	ID   string    `json:"id"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	// Trigger is what produced the event: manual, random, schedule, script
	// or external.
	Trigger   string         `json:"trigger,omitempty"`
	Rule      *Rule          `json:"rule,omitempty"`
	Direction string         `json:"direction,omitempty"`
	Object    *Object        `json:"object,omitempty"`
	Plate     *Plate         `json:"plate,omitempty"`
	Speed     *Speed         `json:"speed,omitempty"`
	Media     *EventMedia    `json:"media,omitempty"`
	Custom    map[string]any `json:"custom,omitempty"`
}

// Rule is the VCA rule that produced an event.
type Rule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Box is a bounding box in normalized 0-1 coordinates.
type Box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Object is the detected object.
type Object struct {
	Class      string  `json:"class"`
	Color      string  `json:"color,omitempty"`
	Box        *Box    `json:"box,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// Plate is license plate data.
type Plate struct {
	Text       string  `json:"text"`
	Format     string  `json:"format,omitempty"`
	Color      string  `json:"color,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Crop       string  `json:"crop,omitempty"`
}

// Speed is speed data for radar profiles.
type Speed struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	Limit float64 `json:"limit,omitempty"`
}

// EventMedia references the snapshot and clip of an event.
type EventMedia struct {
	Snapshot string `json:"snapshot,omitempty"`
	Clip     string `json:"clip,omitempty"`
}
