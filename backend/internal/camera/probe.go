package camera

import (
	"context"
	"encoding/json"
	"time"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/engines/probe"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
)

// probeTimeout bounds a connection test: a login and a few commands.
const probeTimeout = 8 * time.Second

// probeTarget tests a target from the camera's network, the way its
// deliveries reach it (audit B7).
func (r *Runtime) probeTarget(ctx context.Context, t engine.Target) ipc.TargetTestResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"test": true, "source": "MockVision", "camera": r.identity.Name, "target": t.Name, "at": time.Now().UTC()})
	start := time.Now()
	status, err := probe.Target(ctx, t, probe.Info{Body: body, UserAgent: buildinfo.UserAgent(), ClientID: "mockvision-test-" + r.identity.Serial, IP: r.identity.IP})
	res := ipc.TargetTestResult{HTTPStatus: status, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}
