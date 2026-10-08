# hello, an example MockVision plugin

An engine that answers a line protocol on its port: a client gets a
greeting with the camera's serial and a parameter's value; `set <value>`
writes the parameter, `event` raises a motion event, anything else is a
gap the camera reports. It shows what a plugin reaches through its camera,
as its permissions allow.

## Build and package

```sh
mkdir -p pkg/bin/linux-amd64
CGO_ENABLED=0 GOARCH=amd64 go build -o pkg/bin/linux-amd64/hello ./examples/plugins/hello
cat > pkg/manifest.yaml <<'YAML'
format: 1
kind: plugin
id: examples/hello
version: 1.0.0
requires: { contract: 1 }
plugin: { engine: hello, executable: hello, permissions: [net.listen, state.read, state.write, events.emit] }
YAML
mockvision pkg build pkg -o hello.mvpkg -k acme.key
```

Import `hello.mvpkg` in **Plugins** and enable it. A profile then uses it
like a built-in engine:

```yaml
engines:
  hello:
    engine: hello@^1
    port: 7000
    greeting: HELLO
    param: System.DeviceName
events:
  motion: { vendor_name: Motion }
```

```sh
$ nc 192.168.5.190 7000
HELLO 6C0E85E15042 Network Camera
event
EMITTED 01J…
```

## Writing one

Implement `engine.Engine` (`sdk/engine`) and call `plugin.Serve` from
`main`. The camera starts the program with the sockets of its ports and
two connections, one for each direction of `sdk/proto/.../engine.proto`:
the camera calls the engine's `Start`, `Reload`, `Health` and `Stop`; the
engine reaches the camera through `in.Host`, which `plugin.Serve` turns
into gRPC calls. Run without a camera, the program prints what it is and
exits.
