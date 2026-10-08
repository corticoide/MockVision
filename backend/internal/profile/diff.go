package profile

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Change is one difference between two versions of a profile: something
// added, removed or changed in a section, with what changed in it.
type Change struct {
	// Section is state, streams, engines, routes, events, identity, vca or
	// storage.
	Section string   `json:"section"`
	Key     string   `json:"key"`
	Kind    string   `json:"kind"` // added, removed or changed
	Details []string `json:"details,omitempty"`
}

// Kinds of change.
const (
	ChangeAdded   = "added"
	ChangeRemoved = "removed"
	ChangeChanged = "changed"
)

// maxDetails bounds what a change lists.
const maxDetails = 12

// Diff compares two resolved profiles, what a camera would see change if it
// moved from one to the other (D05): parameters, streams, engines and their
// routes, events, identity, analytics and storage.
func Diff(from, to []byte) ([]Change, error) {
	var a, b map[string]any
	if err := json.Unmarshal(from, &a); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(to, &b); err != nil {
		return nil, err
	}
	var out []Change
	out = append(out, diffMap("state", obj(a["state"]), obj(b["state"]))...)
	out = append(out, diffMap("streams", obj(obj(a["media"])["streams"]), obj(obj(b["media"])["streams"]))...)
	ea, eb := obj(a["engines"]), obj(b["engines"])
	out = append(out, diffMap("engines", withoutRoutes(ea), withoutRoutes(eb))...)
	out = append(out, diffMap("routes", routesOf(ea), routesOf(eb))...)
	out = append(out, diffMap("events", obj(a["events"]), obj(b["events"]))...)
	for _, sec := range []string{"identity", "vca", "storage"} {
		if c, ok := diffWhole(sec, a[sec], b[sec]); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func withoutRoutes(engines map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range engines {
		m := map[string]any{}
		for kk, vv := range obj(v) {
			if kk != "routes" {
				m[kk] = vv
			}
		}
		out[k] = m
	}
	return out
}

// routesOf keys every route by instance/id.
func routesOf(engines map[string]any) map[string]any {
	out := map[string]any{}
	for inst, v := range engines {
		routes, _ := obj(v)["routes"].([]any)
		for i, r := range routes {
			id, _ := obj(r)["id"].(string)
			if id == "" {
				id = fmt.Sprintf("#%d", i+1)
			}
			out[inst+"/"+id] = r
		}
	}
	return out
}

func diffMap(section string, a, b map[string]any) []Change {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []Change
	for _, k := range sorted {
		av, inA := a[k]
		bv, inB := b[k]
		switch {
		case !inA:
			out = append(out, Change{Section: section, Key: k, Kind: ChangeAdded})
		case !inB:
			out = append(out, Change{Section: section, Key: k, Kind: ChangeRemoved})
		case !reflect.DeepEqual(av, bv):
			out = append(out, Change{Section: section, Key: k, Kind: ChangeChanged, Details: details(av, bv)})
		}
	}
	return out
}

func diffWhole(section string, a, b any) (Change, bool) {
	switch {
	case reflect.DeepEqual(a, b):
		return Change{}, false
	case a == nil:
		return Change{Section: section, Key: section, Kind: ChangeAdded}, true
	case b == nil:
		return Change{Section: section, Key: section, Kind: ChangeRemoved}, true
	}
	return Change{Section: section, Key: section, Kind: ChangeChanged, Details: details(a, b)}, true
}

// details lists the leaves that differ, as "path: before → after".
func details(a, b any) []string {
	var out []string
	var walk func(path string, a, b any)
	walk = func(path string, a, b any) {
		if len(out) >= maxDetails || reflect.DeepEqual(a, b) {
			return
		}
		am, aok := a.(map[string]any)
		bm, bok := b.(map[string]any)
		if aok && bok {
			keys := map[string]bool{}
			for k := range am {
				keys[k] = true
			}
			for k := range bm {
				keys[k] = true
			}
			sorted := make([]string, 0, len(keys))
			for k := range keys {
				sorted = append(sorted, k)
			}
			sort.Strings(sorted)
			for _, k := range sorted {
				walk(strings.TrimPrefix(path+"."+k, "."), am[k], bm[k])
			}
			return
		}
		if path == "" {
			path = "value"
		}
		out = append(out, path+": "+brief(a)+" → "+brief(b))
	}
	walk("", a, b)
	return out
}

func brief(v any) string {
	if v == nil {
		return "none"
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}
