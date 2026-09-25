// Package engines is the catalog of built-in engines. Each engine lives in
// its own package and implements the same contract as external plugins
// (sdk/engine); profiles request them by name and version range.
package engines

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/corticoide/mockvision/backend/internal/engines/httpapi"
	"github.com/corticoide/mockvision/backend/internal/engines/httppush"
	"github.com/corticoide/mockvision/backend/internal/engines/rtsp"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Catalog resolves engine references such as http-api@^1.
type Catalog struct {
	factories map[string]engine.Factory
}

// Builtin returns the engines compiled into MockVision.
func Builtin() *Catalog {
	return &Catalog{factories: map[string]engine.Factory{
		rtsp.Name:     rtsp.New,
		httpapi.Name:  httpapi.New,
		httppush.Name: httppush.New,
	}}
}

// Resolve returns a new instance of the engine named name whose version
// satisfies rng.
func (c *Catalog) Resolve(name, rng string) (engine.Engine, error) {
	f, ok := c.factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown engine %q (available: %s)", name, strings.Join(c.Names(), ", "))
	}
	e := f()
	d := e.Describe()
	if d.Contract != engine.Contract {
		return nil, fmt.Errorf("engine %s implements contract %d, this MockVision speaks %d", name, d.Contract, engine.Contract)
	}
	constraint, err := semver.NewConstraint(rng)
	if err != nil {
		return nil, fmt.Errorf("invalid version range %q for engine %s", rng, name)
	}
	v, err := semver.NewVersion(d.Version)
	if err != nil {
		return nil, fmt.Errorf("engine %s has an invalid version %q", name, d.Version)
	}
	if !constraint.Check(v) {
		return nil, fmt.Errorf("engine %s %s does not satisfy %s", name, d.Version, rng)
	}
	return e, nil
}

// Names lists the engines in the catalog.
func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.factories))
	for n := range c.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Descriptors describes every engine.
func (c *Catalog) Descriptors() []engine.Descriptor {
	out := make([]engine.Descriptor, 0, len(c.factories))
	for _, n := range c.Names() {
		out = append(out, c.factories[n]().Describe())
	}
	return out
}
