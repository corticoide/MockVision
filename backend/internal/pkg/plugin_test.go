package pkg

import (
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

// pluginPackage packs a plugin package; the program is not run here.
func pluginPackage(t *testing.T, manifest string, files map[string][]byte) []byte {
	t.Helper()
	man := map[string]any{}
	if err := yaml.Unmarshal([]byte(manifest), &man); err != nil {
		t.Fatal(err)
	}
	data, err := pack(man, files)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const helloManifest = `format: 1
kind: plugin
id: examples/hello
version: 1.0.0
requires: { contract: 1 }
plugin:
  engine: hello
  executable: hello
  permissions: [state.write, net.listen, events.emit]
`

func TestPluginPackage(t *testing.T) {
	bin := map[string][]byte{"bin/linux-" + runtime.GOARCH + "/hello": []byte("\x7fELF"), "bin/linux-riscv64/hello": []byte("\x7fELF"), "README.md": []byte("hi")}
	res := Inspect(pluginPackage(t, helloManifest, bin), "hello.mvpkg", engines.Builtin(), Options{})
	if !res.Report.OK() || res.Plugin == nil {
		t.Fatalf("problems: %+v", res.Report.Problems)
	}
	p := res.Plugin
	// Permissions come back in the panel's order.
	if p.Engine != "hello" || p.Version != "1.0.0" || p.Executable != "hello" || strings.Join(p.Permissions, ",") != "net.listen,state.write,events.emit" ||
		len(p.Arches) != 2 || res.Report.Kind != "plugin" || res.Report.Level != "" {
		t.Fatalf("plugin = %+v, report = %+v", p, res.Report)
	}

	for _, tc := range []struct {
		name, from, to, step, want string
		files                      map[string][]byte
	}{
		{"built-in name", "engine: hello", "engine: rtsp", StepPlugin, "built into MockVision", nil},
		{"bad name", "engine: hello", "engine: Hello!", profile.StepSchema, "lowercase", nil},
		{"no contract", "requires: { contract: 1 }\n", "", profile.StepSchema, "requires.contract", nil},
		{"other contract", "contract: 1", "contract: 9", profile.StepCompatibility, "contract 9", nil},
		{"unknown permission", "events.emit]", "events.emit, root]", profile.StepSchema, `unknown permission "root"`, nil},
		{"twice", "events.emit]", "events.emit, net.listen]", profile.StepSchema, "listed twice", nil},
		{"path executable", "executable: hello", "executable: ../hello", profile.StepSchema, "plain file name", nil},
		{"no program here", "", "", profile.StepCompatibility, "no program for this machine", map[string][]byte{"bin/linux-riscv64/hello": []byte("x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := bin
			if tc.files != nil {
				files = tc.files
			}
			man := strings.Replace(helloManifest, tc.from, tc.to, 1)
			res := Inspect(pluginPackage(t, man, files), "hello.mvpkg", engines.Builtin(), Options{})
			if res.Report.OK() || !hasProblem(res.Report, tc.step, tc.want) || res.Plugin != nil {
				t.Fatalf("problems: %+v", res.Report.Problems)
			}
		})
	}
	if res := Inspect(pluginPackage(t, strings.Replace(helloManifest, "plugin:\n", "x-plugin:\n", 1), bin), "", engines.Builtin(), Options{}); res.Report.OK() {
		t.Fatal("a plugin package without its section was accepted")
	}
}

// A profile may use an enabled plugin's engine, known by its descriptor.
func TestProfileWithAPluginEngine(t *testing.T) {
	prof := strings.Replace(string(demo(t)), "\nengines:\n", "\nengines:\n  hello:\n    engine: hello@^1\n    port: 7000\n", 1)
	if res := Inspect([]byte(prof), "p.yaml", engines.Builtin(), Options{}); res.Report.OK() {
		t.Fatal("an unknown engine was accepted")
	}
	d := engine.Descriptor{Name: "hello", Version: "1.0.0", Contract: engine.Contract, Role: engine.RoleServer,
		Sockets: []engine.SocketSpec{{Name: "tcp", Network: "tcp", DefaultPort: 7000}}}
	if res := Inspect([]byte(prof), "p.yaml", engines.Builtin(), Options{Plugins: []engine.Descriptor{d}}); !res.Report.OK() {
		t.Fatalf("problems: %+v", res.Report.Problems)
	}
}
