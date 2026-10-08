package profile_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

func baseParent(t *testing.T) *profile.Parent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-base.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := profile.Validate(profile.Input{Data: data}, engines.Builtin())
	if !res.OK() {
		t.Fatalf("base: %+v", res.Problems)
	}
	return &profile.Parent{Ref: "milesight/base@0.1.0", Resolved: res.Resolved}
}

const childHead = `schema: 1
profile:
  id: milesight/c2965
  version: 1.0.0
  name: Milesight C2965
  vendor: Milesight
  model: MS-C2965-PB
  extends: milesight/base@^0
`

// Mappings merge key by key, lists of items with ids merge by id, and
// remove drops what the parent had (D17).
func TestInheritMerges(t *testing.T) {
	parent := baseParent(t)
	var base map[string]any
	_ = json.Unmarshal(parent.Resolved, &base)
	routes := base["engines"].(map[string]any)["http"].(map[string]any)["routes"].([]any)
	first := routes[0].(map[string]any)["id"].(string)
	second := routes[1].(map[string]any)["id"].(string)

	child := childHead + `identity:
  device:
    model: MS-C2965-PB
engines:
  http:
    routes:
      - id: ` + first + `
        match: { method: GET, path: /cgi-bin/changed.cgi }
      - id: brand-new
        match: { method: GET, path: /cgi-bin/new.cgi }
        action: { status: 204 }
remove: ["/engines/http/routes/` + second + `"]
`
	res := profile.Validate(profile.Input{Data: []byte(child), Parent: parent}, engines.Builtin())
	if !res.OK() {
		t.Fatalf("problems: %+v", res.Problems)
	}
	var got map[string]any
	_ = json.Unmarshal(res.Resolved, &got)
	gotRoutes := got["engines"].(map[string]any)["http"].(map[string]any)["routes"].([]any)
	if len(gotRoutes) != len(routes) {
		t.Fatalf("%d routes, want %d: one dropped, one added", len(gotRoutes), len(routes))
	}
	r0 := gotRoutes[0].(map[string]any)
	if r0["id"] != first || r0["match"].(map[string]any)["path"] != "/cgi-bin/changed.cgi" || r0["action"] == nil {
		t.Fatalf("the first route merges with the parent's: %v", r0)
	}
	for _, r := range gotRoutes {
		if r.(map[string]any)["id"] == second {
			t.Fatal("the removed route is still there")
		}
	}
	if last := gotRoutes[len(gotRoutes)-1].(map[string]any); last["id"] != "brand-new" {
		t.Fatalf("a new route goes last: %v", last)
	}
	meta := got["profile"].(map[string]any)
	if meta["extends"] != "milesight/base@0.1.0" || meta["model"] != "MS-C2965-PB" || meta["id"] != "milesight/c2965" {
		t.Fatalf("profile: %v", meta)
	}
	if lin := meta["lineage"].([]any); len(lin) != 1 || lin[0] != "milesight/base@0.1.0" {
		t.Fatalf("lineage %v", lin)
	}
	if got["identity"].(map[string]any)["factory"] == nil {
		t.Fatal("the identity merges with the parent's: its factory values are gone")
	}
	if _, has := got["remove"]; has {
		t.Fatal("remove stays out of the resolved profile")
	}
}

func TestInheritRefusals(t *testing.T) {
	parent := baseParent(t)
	deep := *parent
	deep.Resolved = []byte(strings.Replace(string(parent.Resolved), `"profile":{`, `"profile":{"lineage":["a/b@1.0.0","c/d@1.0.0","e/f@1.0.0"],`, 1))
	self := *parent
	self.Resolved = []byte(strings.Replace(string(parent.Resolved), `"profile":{`, `"profile":{"lineage":["milesight/c2965@0.9.0"],`, 1))
	for name, c := range map[string]struct {
		doc    string
		parent *profile.Parent
		want   string
	}{
		"not installed":     {childHead, nil, "the parent is not installed"},
		"another parent":    {childHead, &profile.Parent{Ref: "acme/other@1.0.0", Resolved: parent.Resolved}, "was given as its parent"},
		"too deep":          {childHead, &deep, "levels of inheritance"},
		"its own ancestor":  {childHead, &self, "cannot inherit from itself"},
		"remove nothing":    {childHead + "remove: [\"/state/Nope\"]\n", parent, "the parent has nothing there"},
		"remove no parent":  {strings.Replace(childHead, "  extends: milesight/base@^0\n", "", 1) + "remove: [\"/state/X\"]\n", nil, "extends nothing"},
		"bad reference":     {strings.Replace(childHead, "@^0", "@not a range", 1), parent, "is not a version range"},
		"remove the schema": {childHead + "remove: [\"/profile/name\"]\n", parent, "cannot be removed"},
	} {
		t.Run(name, func(t *testing.T) {
			res := profile.Validate(profile.Input{Data: []byte(c.doc), Parent: c.parent}, engines.Builtin())
			for _, p := range res.Problems {
				if p.Step == profile.StepInheritance && strings.Contains(p.Message, c.want) {
					return
				}
			}
			t.Fatalf("want an inheritance problem with %q, got %+v", c.want, res.Problems)
		})
	}
}
