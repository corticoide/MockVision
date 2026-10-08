package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

// MaxInheritanceDepth is how many ancestors a profile may have (D17).
const MaxInheritanceDepth = 3

// Parent is the installed profile a document extends: its resolved
// document, which holds what its own ancestors gave it, and its lineage.
type Parent struct {
	// Ref is the parent pinned to its version, vendor/model@1.2.0.
	Ref      string          `json:"ref"`
	Resolved json.RawMessage `json:"resolved"`
}

// Lineage returns the ancestors a resolved document records, nearest
// first; the importer writes them, so they hold whatever a parent was
// resolved with.
func Lineage(resolved []byte) []string {
	var doc struct {
		Profile struct {
			Lineage []string `json:"lineage"`
		} `json:"profile"`
	}
	_ = json.Unmarshal(resolved, &doc)
	return doc.Profile.Lineage
}

// ExtendsRef splits an extends reference, vendor/model@range, into the
// profile ID and the version range (any version when it has none).
func ExtendsRef(ref string) (id, rng string, err error) {
	id, rng, _ = strings.Cut(strings.TrimSpace(ref), "@")
	if rng == "" {
		rng = "*"
	}
	if !domain.ValidProfileID(id) {
		return "", "", fmt.Errorf("extends %q: the parent must be a profile ID such as milesight/base@^1", ref)
	}
	if _, err := semver.NewConstraint(rng); err != nil {
		return "", "", fmt.Errorf("extends %q: %q is not a version range", ref, rng)
	}
	return id, rng, nil
}

// Extends returns what a profile document extends, read with the bounded
// YAML reader; "" when it extends nothing or cannot be read (validation
// reports why).
func Extends(data []byte) string {
	generic, _, err := ParseYAML(data, DefaultLimits)
	if err != nil {
		return ""
	}
	root, _ := generic.(map[string]any)
	meta, _ := root["profile"].(map[string]any)
	ref, _ := meta["extends"].(string)
	return strings.TrimSpace(ref)
}

// inherit merges the document over its parent (D17): mappings merge key by
// key, lists whose items all have an id merge item by item, anything else
// is replaced. remove lists what the parent had that the child drops, as
// pointers whose list segments are item ids. The result records the
// parent pinned to its version and the whole lineage.
func (v *validator) inherit(root map[string]any) (map[string]any, bool) {
	meta, _ := root["profile"].(map[string]any)
	ref, _ := meta["extends"].(string)
	if ref == "" {
		if _, has := root["remove"]; has {
			v.errorf(StepInheritance, "/remove", "remove drops what a parent gave; this profile extends nothing")
			return nil, false
		}
		return root, true
	}
	id, _, err := ExtendsRef(ref)
	if err != nil {
		v.errorf(StepInheritance, "/profile/extends", "%v", err)
		return nil, false
	}
	p := v.in.Parent
	if p == nil {
		v.errorf(StepInheritance, "/profile/extends", "extends %s: the parent is not installed", ref)
		return nil, false
	}
	parentID, _, _ := strings.Cut(p.Ref, "@")
	if parentID != id {
		v.errorf(StepInheritance, "/profile/extends", "extends %s, but %s was given as its parent", ref, p.Ref)
		return nil, false
	}
	lineage := append([]string{p.Ref}, Lineage(p.Resolved)...)
	if len(lineage) > MaxInheritanceDepth {
		v.errorf(StepInheritance, "/profile/extends", "extends %s, which makes %d levels of inheritance; at most %d are allowed", ref, len(lineage), MaxInheritanceDepth)
		return nil, false
	}
	if own, _ := meta["id"].(string); own != "" {
		for _, anc := range lineage {
			if a, _, _ := strings.Cut(anc, "@"); a == own {
				v.errorf(StepInheritance, "/profile/extends", "%s cannot inherit from itself: %s is among its ancestors", own, anc)
				return nil, false
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(p.Resolved))
	dec.UseNumber()
	var parent map[string]any
	if err := dec.Decode(&parent); err != nil {
		v.errorf(StepInheritance, "/profile/extends", "the parent %s cannot be read: %v", p.Ref, err)
		return nil, false
	}
	// What a parent's identity and version say is not the child's.
	if pm, ok := parent["profile"].(map[string]any); ok {
		for _, k := range []string{"id", "version", "lineage", "extends"} {
			delete(pm, k)
		}
	}
	delete(parent, "coverage")
	ok := true
	if raw, has := root["remove"]; has {
		list, _ := raw.([]any)
		for i, item := range list {
			ptr, _ := item.(string)
			if err := removeAt(parent, ptr); err != nil {
				v.errorf(StepInheritance, fmt.Sprintf("/remove/%d", i), "remove %s: %v", ptr, err)
				ok = false
			}
		}
	}
	if !ok {
		return nil, false
	}
	child := make(map[string]any, len(root))
	for k, val := range root {
		if k != "remove" {
			child[k] = val
		}
	}
	merged, _ := mergeValues(parent, child).(map[string]any)
	mm, _ := merged["profile"].(map[string]any)
	mm["extends"] = p.Ref
	lin := make([]any, len(lineage))
	for i, l := range lineage {
		lin[i] = l
	}
	mm["lineage"] = lin
	return merged, true
}

// mergeValues merges child over parent.
func mergeValues(parent, child any) any {
	switch c := child.(type) {
	case map[string]any:
		p, ok := parent.(map[string]any)
		if !ok {
			return c
		}
		out := make(map[string]any, len(p)+len(c))
		for k, val := range p {
			out[k] = val
		}
		for k, val := range c {
			if pv, has := out[k]; has {
				out[k] = mergeValues(pv, val)
			} else {
				out[k] = val
			}
		}
		return out
	case []any:
		p, ok := parent.([]any)
		if !ok || !allHaveIDs(p) || !allHaveIDs(c) {
			return c
		}
		out := append([]any{}, p...)
		index := map[string]int{}
		for i, item := range out {
			index[itemID(item)] = i
		}
		for _, item := range c {
			if i, has := index[itemID(item)]; has {
				out[i] = mergeValues(out[i], item)
				continue
			}
			index[itemID(item)] = len(out)
			out = append(out, item)
		}
		return out
	}
	return child
}

func itemID(item any) string {
	m, _ := item.(map[string]any)
	id, _ := m["id"].(string)
	return id
}

func allHaveIDs(list []any) bool {
	if len(list) == 0 {
		return false
	}
	for _, item := range list {
		if itemID(item) == "" {
			return false
		}
	}
	return true
}

// removeAt deletes what a pointer names in a document; a list segment is
// the id of one of its items.
func removeAt(doc map[string]any, ptr string) error {
	if !strings.HasPrefix(ptr, "/") || ptr == "/" {
		return errors.New("a pointer such as /state/Encode.Third.Resolution is needed")
	}
	segs := strings.Split(ptr[1:], "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	if segs[0] == "schema" || segs[0] == "profile" {
		return fmt.Errorf("%s cannot be removed", segs[0])
	}
	var node any = doc
	for i, seg := range segs {
		last := i == len(segs)-1
		switch n := node.(type) {
		case map[string]any:
			next, ok := n[seg]
			if !ok {
				return errors.New("the parent has nothing there")
			}
			if last {
				delete(n, seg)
				return nil
			}
			node = next
		case []any:
			at := -1
			for j, item := range n {
				if itemID(item) == seg {
					at = j
				}
			}
			if at < 0 {
				return fmt.Errorf("the parent's list has no item with id %s", seg)
			}
			if last {
				rest := append(n[:at:at], n[at+1:]...)
				return replaceList(doc, segs[:i], rest)
			}
			node = n[at]
		default:
			return errors.New("the parent has nothing there")
		}
	}
	return nil
}

// replaceList stores a shortened list back where it was.
func replaceList(doc map[string]any, path []string, list []any) error {
	var node any = doc
	for i, seg := range path {
		last := i == len(path)-1
		switch n := node.(type) {
		case map[string]any:
			if last {
				n[seg] = list
				return nil
			}
			node = n[seg]
		case []any:
			for j, item := range n {
				if itemID(item) == seg {
					if last {
						n[j] = list
						return nil
					}
					node = item
				}
			}
		}
	}
	return errors.New("the parent has nothing there")
}
