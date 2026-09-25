package app

import (
	"context"
	"os"
	"time"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/telemetry"
)

// NodeView describes the node.
type NodeView struct {
	Version          string             `json:"version"`
	Runtime          string             `json:"runtime"`
	Hostname         string             `json:"hostname"`
	CPUCount         int                `json:"cpu_count"`
	MemTotal         uint64             `json:"mem_total"`
	DefaultInterface string             `json:"default_interface"`
	DefaultGateway   string             `json:"default_gateway"`
	ParentInterface  string             `json:"parent_interface"`
	Interfaces       []netctl.Interface `json:"interfaces"`
	Cameras          CameraCounts       `json:"cameras"`
	StartedAt        time.Time          `json:"started_at"`
}

// CameraCounts summarizes cameras by state.
type CameraCounts struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Error   int `json:"error"`
}

// NodeMetrics is the live usage of the node and its cameras.
type NodeMetrics struct {
	At             time.Time              `json:"at"`
	CPUPercent     float64                `json:"cpu_percent"`
	CPUSustained   float64                `json:"cpu_sustained_percent"`
	MemTotal       uint64                 `json:"mem_total"`
	MemUsed        uint64                 `json:"mem_used"`
	Cameras        map[string]MetricsView `json:"cameras"`
	CameraCounts   CameraCounts           `json:"camera_counts"`
	DB             store.Stats            `json:"db"`
	AdmissionLimit Settings               `json:"limits"`
}

var processStart = time.Now()

// Node returns node information.
func (s *Service) Node(ctx context.Context) (*NodeView, error) {
	info, err := nodeInfo()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	v := &NodeView{
		Version: buildinfo.Version, Runtime: s.rt.Kind(), Hostname: host, CPUCount: s.node.CPUCount(),
		MemTotal: s.node.Latest().MemTotal, DefaultInterface: info.DefaultInterface, DefaultGateway: info.DefaultGateway,
		ParentInterface: s.defaultParent(ctx), Interfaces: info.Interfaces, StartedAt: processStart,
	}
	v.Cameras = s.cameraCounts(ctx)
	return v, nil
}

func (s *Service) cameraCounts(ctx context.Context) CameraCounts {
	var c CameraCounts
	cams, err := s.store.R().ListCameras(ctx)
	if err != nil {
		return c
	}
	c.Total = len(cams)
	statuses, _ := s.store.R().ListCameraStatuses(ctx)
	for _, st := range statuses {
		switch domain.CameraState(st.ActualState) {
		case domain.StateRunning, domain.StateDegraded:
			c.Running++
		case domain.StateError:
			c.Error++
		}
	}
	return c
}

// Metrics returns the live usage of the node.
func (s *Service) Metrics(ctx context.Context) NodeMetrics {
	latest := s.node.Latest()
	m := NodeMetrics{
		At: time.Now(), CPUPercent: latest.CPUPercent, CPUSustained: s.node.SustainedCPU(time.Minute),
		MemTotal: latest.MemTotal, MemUsed: latest.MemUsed, Cameras: map[string]MetricsView{},
		DB: s.store.Stats(), AdmissionLimit: s.Settings(ctx),
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id, ss := range s.sessions {
		if ss.active() {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		if sample, ok := s.metrics.Latest(id); ok {
			m.Cameras[id] = *metricsView(sample)
		}
	}
	m.CameraCounts = s.cameraCounts(ctx)
	return m
}

// CameraMetrics returns the recent samples of a camera.
func (s *Service) CameraMetrics(ctx context.Context, id string, since time.Time) ([]telemetry.CameraSample, error) {
	if _, err := s.store.R().GetCamera(ctx, id); err != nil {
		return nil, store.NotFound(err)
	}
	out := s.metrics.History(id, since)
	if out == nil {
		out = []telemetry.CameraSample{}
	}
	return out, nil
}

func (s *Service) publishNodeMetrics(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pub.Publish("node", "metrics", s.Metrics(ctx))
		}
	}
}
