package app

import (
	"context"
	"net"
	"net/netip"
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
	Panel            PanelAccess        `json:"panel"`
	StartedAt        time.Time          `json:"started_at"`
}

// PanelAccess says where the panel and the API listen (D63): every
// interface or a management IP, always behind a login.
type PanelAccess struct {
	Listen        string   `json:"listen"`
	AllInterfaces bool     `json:"all_interfaces"`
	URLs          []string `json:"urls"`
}

// CameraCounts summarizes cameras by state. Running includes degraded
// cameras, which still answer.
type CameraCounts struct {
	Total   int            `json:"total"`
	Running int            `json:"running"`
	Error   int            `json:"error"`
	ByState map[string]int `json:"by_state"`
}

// NodeMetrics is the live usage of the node and its cameras.
type NodeMetrics struct {
	At             time.Time              `json:"at"`
	CPUPercent     float64                `json:"cpu_percent"`
	CPUSustained   float64                `json:"cpu_sustained_percent"`
	MemTotal       uint64                 `json:"mem_total"`
	MemUsed        uint64                 `json:"mem_used"`
	NetInterface   string                 `json:"net_interface"`
	NetRxBps       float64                `json:"net_rx_bps"`
	NetTxBps       float64                `json:"net_tx_bps"`
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
	v.Panel = panelAccess(s.opts.Listen, info.Interfaces)
	return v, nil
}

// panelAccess lists the URLs the panel answers on: the listen address, or
// every address of the node when it listens on all interfaces.
func panelAccess(listen string, ifaces []netctl.Interface) PanelAccess {
	p := PanelAccess{Listen: listen, URLs: []string{}}
	if listen == "" {
		return p
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return p
	}
	ip := net.ParseIP(host)
	if host != "" && (ip == nil || !ip.IsUnspecified()) {
		p.URLs = append(p.URLs, "http://"+net.JoinHostPort(host, port))
		return p
	}
	p.AllInterfaces = true
	for _, i := range ifaces {
		if i.Loopback || !i.Up {
			continue
		}
		for _, a := range i.Addrs {
			pfx, err := netip.ParsePrefix(a)
			if err != nil || !pfx.Addr().Is4() {
				continue
			}
			p.URLs = append(p.URLs, "http://"+net.JoinHostPort(pfx.Addr().String(), port))
		}
	}
	return p
}

func (s *Service) cameraCounts(ctx context.Context) CameraCounts {
	c := CameraCounts{ByState: map[string]int{}}
	cams, err := s.store.R().ListCameras(ctx)
	if err != nil {
		return c
	}
	c.Total = len(cams)
	states := make(map[string]string, len(cams))
	statuses, _ := s.store.R().ListCameraStatuses(ctx)
	for _, st := range statuses {
		states[st.CameraID] = st.ActualState
	}
	for _, cam := range cams {
		st, ok := states[cam.ID]
		if !ok {
			st = string(domain.StateStopped)
		}
		c.ByState[st]++
		switch domain.CameraState(st) {
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
		MemTotal: latest.MemTotal, MemUsed: latest.MemUsed, NetInterface: latest.NetInterface,
		NetRxBps: latest.NetRxBps, NetTxBps: latest.NetTxBps, Cameras: map[string]MetricsView{},
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

// NodeHistory returns the node samples newer than since, at most the last
// ten minutes: enough for the dashboard's charts.
func (s *Service) NodeHistory(since time.Time) []telemetry.NodeSample {
	out := s.node.History(since)
	if out == nil {
		out = []telemetry.NodeSample{}
	}
	return out
}

// measureInterface points the node's traffic sampler at the interface the
// cameras hang from; in local mode they answer on the loopback.
func (s *Service) measureInterface(ctx context.Context) {
	iface := "lo"
	if s.rt.Kind() != "local" {
		iface = s.defaultParent(ctx)
	}
	s.node.SetInterface(iface)
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
