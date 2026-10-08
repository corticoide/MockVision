package pkg

import (
	"fmt"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
	"github.com/corticoide/mockvision/sdk/plugin"
)

// StepPlugin checks what a plugin package provides and asks for.
const StepPlugin = "plugin"

// PluginSpec is the plugin section of a plugin package's manifest: the
// engine it provides, its program under bin/linux-<arch>/ and the
// permissions it asks for (D85, D86). The package's version is the
// engine's.
type PluginSpec struct {
	Engine      string   `json:"engine"`
	Executable  string   `json:"executable"`
	Permissions []string `json:"permissions"`
}

// PluginInfo is a plugin package that passed the pipeline.
type PluginInfo struct {
	Engine      string   `json:"engine"`
	Version     string   `json:"version"`
	Executable  string   `json:"executable"`
	Permissions []string `json:"permissions"`
	// Arches are the machines the package has a program for.
	Arches []string `json:"arches"`
}

var (
	pluginEngineName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	pluginExeName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// PluginBinaryPath is where a plugin package keeps its program for arch.
func PluginBinaryPath(arch, exe string) string {
	return path.Join("bin", "linux-"+arch, exe)
}

// inspectPlugin checks a plugin package: its engine's name, its program
// for this machine and its permissions. What the program says it is, the
// node checks when it installs it, by running it confined.
func (in *inspector) inspectPlugin(m *Manifest, files map[string][]byte) {
	sp := m.Plugin
	if sp == nil {
		in.problem(profile.StepSchema, "manifest.yaml", 0, "a plugin package needs a plugin section: engine, executable and permissions")
		in.step(StepPlugin, "failed", "")
		return
	}
	n := len(in.res.Report.Problems)
	switch {
	case !pluginEngineName.MatchString(sp.Engine):
		in.problem(profile.StepSchema, "manifest.yaml", 0, "plugin.engine %q must be 2 to 32 lowercase letters, digits or dashes", sp.Engine)
	case in.builtin != nil && builtinHas(in.builtin, sp.Engine):
		in.problem(StepPlugin, "manifest.yaml", 0, "engine %s is built into MockVision; a plugin cannot replace it", sp.Engine)
	}
	switch c := m.Requires.Contract; {
	case c == 0:
		in.problem(profile.StepSchema, "manifest.yaml", 0, "a plugin package names the engine contract it implements in requires.contract")
	case c != engine.Contract:
		in.problem(profile.StepCompatibility, "manifest.yaml", 0, "the plugin implements engine contract %d; this MockVision speaks %d", c, engine.Contract)
	}
	seen := map[string]bool{}
	for _, p := range sp.Permissions {
		switch {
		case !plugin.ValidPermission(p):
			in.problem(profile.StepSchema, "manifest.yaml", 0, "unknown permission %q (known: %s)", p, strings.Join(plugin.Permissions, ", "))
		case seen[p]:
			in.problem(profile.StepSchema, "manifest.yaml", 0, "permission %s is listed twice", p)
		}
		seen[p] = true
	}
	var arches []string
	if !pluginExeName.MatchString(sp.Executable) {
		in.problem(profile.StepSchema, "manifest.yaml", 0, "plugin.executable %q must be a plain file name", sp.Executable)
	} else {
		for name := range files {
			dir, file := path.Split(name)
			if file == sp.Executable && strings.HasPrefix(dir, "bin/linux-") && strings.Count(dir, "/") == 2 {
				arches = append(arches, strings.TrimSuffix(strings.TrimPrefix(dir, "bin/linux-"), "/"))
			}
		}
		sort.Strings(arches)
		if _, ok := files[PluginBinaryPath(runtime.GOARCH, sp.Executable)]; !ok {
			have := "none"
			if len(arches) > 0 {
				have = "linux-" + strings.Join(arches, ", linux-")
			}
			in.problem(profile.StepCompatibility, "manifest.yaml", 0, "the package has no program for this machine, %s (it has: %s)",
				PluginBinaryPath(runtime.GOARCH, sp.Executable), have)
		}
	}
	if len(in.res.Report.Problems) > n {
		in.step(StepPlugin, "failed", "")
		return
	}
	perms := make([]string, 0, len(sp.Permissions))
	for _, p := range plugin.Permissions {
		if seen[p] {
			perms = append(perms, p)
		}
	}
	in.res.Plugin = &PluginInfo{Engine: sp.Engine, Version: m.Version, Executable: sp.Executable, Permissions: perms, Arches: arches}
	in.step(StepPlugin, "passed", fmt.Sprintf("engine %s, for linux-%s", sp.Engine, strings.Join(arches, ", linux-")))
}

// builtinHas says whether the catalog has an engine of that name, of any
// version.
func builtinHas(c profile.EngineCatalog, name string) bool {
	_, err := c.Resolve(name, "*")
	return err == nil || !strings.Contains(err.Error(), "unknown engine")
}

// withPlugins is the catalog that also resolves the plugins' engines, by
// their descriptors: profiles validate against them, only cameras run them.
func withPlugins(c profile.EngineCatalog, plugins []engine.Descriptor) profile.EngineCatalog {
	cat, ok := c.(*engines.Catalog)
	if !ok || len(plugins) == 0 {
		return c
	}
	extra := map[string]engine.Factory{}
	for _, d := range plugins {
		extra[d.Name] = engines.Described(d)
	}
	return cat.With(extra)
}
