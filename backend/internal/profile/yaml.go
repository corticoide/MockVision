package profile

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Limits bound what the YAML reader accepts. Profiles come from users and
// the community, so size, nesting and aliases are capped before anything
// else looks at the document.
type Limits struct {
	MaxBytes   int
	MaxDepth   int
	MaxAliases int
	MaxNodes   int
}

// DefaultLimits are the limits used by the importer.
var DefaultLimits = Limits{MaxBytes: 1 << 20, MaxDepth: 48, MaxAliases: 64, MaxNodes: 100_000}

// Position is where a value starts in its YAML file.
type Position struct {
	Line int
	// Block is true for literal or folded scalars, whose content starts on
	// the line after Line.
	Block bool
}

// Positions maps JSON pointers ("/engines/http/routes/0") to positions.
type Positions map[string]Position

// Line returns the line of pointer, or of its closest ancestor.
func (p Positions) Line(pointer string) int {
	for {
		if pos, ok := p[pointer]; ok {
			return pos.Line
		}
		i := strings.LastIndex(pointer, "/")
		if i < 0 {
			return 0
		}
		pointer = pointer[:i]
	}
}

// YAMLError is a problem found while reading YAML, with its line.
type YAMLError struct {
	Line    int
	Message string
}

func (e *YAMLError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

// ParseYAML decodes a YAML document into JSON-compatible values
// (map[string]any, []any, string, json.Number, bool, nil) and records the
// line of every value.
func ParseYAML(data []byte, lim Limits) (any, Positions, error) {
	if len(data) > lim.MaxBytes {
		return nil, nil, &YAMLError{Message: fmt.Sprintf("file is %d bytes, the limit is %d", len(data), lim.MaxBytes)}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, yamlSyntaxError(err)
	}
	if doc.Kind == 0 {
		return nil, nil, &YAMLError{Message: "the document is empty"}
	}
	c := &converter{lim: lim, pos: Positions{}}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) != 1 {
			return nil, nil, &YAMLError{Message: "expected a single YAML document"}
		}
		root = doc.Content[0]
	}
	v, err := c.convert(root, "", 0)
	if err != nil {
		return nil, nil, err
	}
	return v, c.pos, nil
}

func yamlSyntaxError(err error) error {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	// yaml.v3 reports "line N: message".
	if rest, ok := strings.CutPrefix(msg, "line "); ok {
		if i := strings.Index(rest, ":"); i > 0 {
			if n, convErr := strconv.Atoi(rest[:i]); convErr == nil {
				return &YAMLError{Line: n, Message: strings.TrimSpace(rest[i+1:])}
			}
		}
	}
	return &YAMLError{Message: msg}
}

type converter struct {
	lim     Limits
	pos     Positions
	aliases int
	nodes   int
}

func (c *converter) convert(n *yaml.Node, ptr string, depth int) (any, error) {
	c.nodes++
	if c.nodes > c.lim.MaxNodes {
		return nil, &YAMLError{Line: n.Line, Message: "document has too many nodes"}
	}
	if depth > c.lim.MaxDepth {
		return nil, &YAMLError{Line: n.Line, Message: fmt.Sprintf("nesting deeper than %d levels", c.lim.MaxDepth)}
	}
	if _, seen := c.pos[ptr]; !seen {
		c.pos[ptr] = Position{Line: n.Line, Block: n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0}
	}
	switch n.Kind {
	case yaml.AliasNode:
		c.aliases++
		if c.aliases > c.lim.MaxAliases {
			return nil, &YAMLError{Line: n.Line, Message: fmt.Sprintf("more than %d aliases", c.lim.MaxAliases)}
		}
		return c.convert(n.Alias, ptr, depth+1)
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				return nil, &YAMLError{Line: k.Line, Message: "mapping keys must be plain values"}
			}
			if k.ShortTag() == "!!merge" {
				if err := c.merge(out, v, ptr, depth); err != nil {
					return nil, err
				}
				continue
			}
			key := k.Value
			if _, dup := out[key]; dup {
				return nil, &YAMLError{Line: k.Line, Message: fmt.Sprintf("duplicated key %q", key)}
			}
			child, err := c.convert(v, ptr+"/"+escapePointer(key), depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = child
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, item := range n.Content {
			child, err := c.convert(item, ptr+"/"+strconv.Itoa(i), depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, child)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalar(n)
	}
	return nil, &YAMLError{Line: n.Line, Message: "unsupported YAML node"}
}

// merge applies a "<<" merge key: keys already present win.
func (c *converter) merge(out map[string]any, v *yaml.Node, ptr string, depth int) error {
	merged, err := c.convert(v, ptr, depth+1)
	if err != nil {
		return err
	}
	var sources []any
	switch m := merged.(type) {
	case map[string]any:
		sources = []any{m}
	case []any:
		sources = m
	default:
		return &YAMLError{Line: v.Line, Message: "merge key needs a mapping or a list of mappings"}
	}
	for _, s := range sources {
		m, ok := s.(map[string]any)
		if !ok {
			return &YAMLError{Line: v.Line, Message: "merge key needs a mapping or a list of mappings"}
		}
		for k, val := range m {
			if _, exists := out[k]; !exists {
				out[k] = val
			}
		}
	}
	return nil
}

func scalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, &YAMLError{Line: n.Line, Message: err.Error()}
		}
		return b, nil
	case "!!int":
		var i int64
		if err := n.Decode(&i); err != nil {
			return nil, &YAMLError{Line: n.Line, Message: "integer out of range"}
		}
		return json.Number(strconv.FormatInt(i, 10)), nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, &YAMLError{Line: n.Line, Message: err.Error()}
		}
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if strings.ContainsAny(s, "IN") { // Inf or NaN
			return nil, &YAMLError{Line: n.Line, Message: "infinite and NaN numbers are not allowed"}
		}
		return json.Number(s), nil
	default:
		// Strings, timestamps and binary values are kept as written.
		return n.Value, nil
	}
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
