package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// Settings are node-wide settings stored in the database (D60: only the
// data path, port and interface come from the environment).
type Settings struct {
	MaxCameras          int     `json:"max_cameras"`
	MaxRAMPercent       float64 `json:"max_ram_percent"`
	MaxCPUPercent       float64 `json:"max_cpu_percent"`
	ParentInterface     string  `json:"parent_interface"`
	EventsRetentionDays int     `json:"events_retention_days"`
}

// SettingsPatch changes some settings.
type SettingsPatch struct {
	MaxCameras          *int     `json:"max_cameras,omitempty"`
	MaxRAMPercent       *float64 `json:"max_ram_percent,omitempty"`
	MaxCPUPercent       *float64 `json:"max_cpu_percent,omitempty"`
	ParentInterface     *string  `json:"parent_interface,omitempty"`
	EventsRetentionDays *int     `json:"events_retention_days,omitempty"`
}

const settingsKey = "node"

func defaultSettings() Settings {
	l := domain.DefaultAdmissionLimits()
	return Settings{MaxCameras: l.MaxCameras, MaxRAMPercent: l.MaxRAMPercent, MaxCPUPercent: l.MaxCPUPercent, EventsRetentionDays: 7}
}

// Settings returns the current settings.
func (s *Service) Settings(ctx context.Context) Settings {
	set := defaultSettings()
	raw, err := s.store.R().GetSetting(ctx, settingsKey)
	if err == nil {
		_ = json.Unmarshal([]byte(raw), &set)
	}
	return set
}

// UpdateSettings validates and saves a patch.
func (s *Service) UpdateSettings(ctx context.Context, actor Actor, p SettingsPatch) (Settings, error) {
	set := s.Settings(ctx)
	before := set
	v := &domain.ValidationError{}
	if p.MaxCameras != nil {
		if *p.MaxCameras < 1 || *p.MaxCameras > 1000 {
			v.Add("max_cameras", "must be between 1 and 1000")
		}
		set.MaxCameras = *p.MaxCameras
	}
	if p.MaxRAMPercent != nil {
		if *p.MaxRAMPercent < 10 || *p.MaxRAMPercent > 99 {
			v.Add("max_ram_percent", "must be between 10 and 99")
		}
		set.MaxRAMPercent = *p.MaxRAMPercent
	}
	if p.MaxCPUPercent != nil {
		if *p.MaxCPUPercent < 10 || *p.MaxCPUPercent > 100 {
			v.Add("max_cpu_percent", "must be between 10 and 100")
		}
		set.MaxCPUPercent = *p.MaxCPUPercent
	}
	if p.ParentInterface != nil {
		if *p.ParentInterface != "" && s.rt.Kind() == "netns" {
			info, _ := nodeInfo()
			if _, ok := info.Lookup(*p.ParentInterface); !ok {
				v.Add("parent_interface", "interface %s does not exist on this node", *p.ParentInterface)
			}
		}
		set.ParentInterface = *p.ParentInterface
	}
	if p.EventsRetentionDays != nil {
		if *p.EventsRetentionDays < 1 || *p.EventsRetentionDays > 365 {
			v.Add("events_retention_days", "must be between 1 and 365")
		}
		set.EventsRetentionDays = *p.EventsRetentionDays
	}
	if err := v.Err(); err != nil {
		return before, err
	}
	raw, _ := json.Marshal(set)
	if err := s.store.W().UpsertSetting(ctx, db.UpsertSettingParams{Key: settingsKey, ValueJson: string(raw)}); err != nil {
		return before, err
	}
	s.audit(ctx, actor, "settings.update", "settings", settingsKey, map[string]any{"before": before, "after": set})
	return set, nil
}

func (set Settings) limits() domain.AdmissionLimits {
	return domain.AdmissionLimits{MaxCameras: set.MaxCameras, MaxRAMPercent: set.MaxRAMPercent, MaxCPUPercent: set.MaxCPUPercent}
}

// cameraCost estimates what one more camera costs, corrected with what the
// running cameras really use (D91).
func (s *Service) cameraCost() domain.CameraCost {
	cost := domain.CameraCost{RAM: 25 << 20, CPUPercent: 1}
	if rss, cpu, n := s.metrics.Averages(); n > 0 {
		cost.RAM = rss + rss/5 // 20% margin
		if cpu > cost.CPUPercent {
			cost.CPUPercent = cpu
		}
	}
	return cost
}

func (s *Service) nodeUsage(ctx context.Context) (domain.NodeUsage, error) {
	n, err := s.store.R().CountCameras(ctx)
	if err != nil {
		return domain.NodeUsage{}, err
	}
	latest := s.node.Latest()
	return domain.NodeUsage{
		Cameras:    int(n),
		MemTotal:   latest.MemTotal,
		MemUsed:    latest.MemUsed,
		CPUPercent: s.node.SustainedCPU(time.Minute),
		CPUCount:   s.node.CPUCount(),
	}, nil
}

// admitCreate applies RN-16 and D91 before creating a camera.
func (s *Service) admitCreate(ctx context.Context) error {
	usage, err := s.nodeUsage(ctx)
	if err != nil {
		return err
	}
	return domain.AdmitCreate(s.Settings(ctx).limits(), usage, s.cameraCost())
}

// admitStart applies D91 before starting a camera.
func (s *Service) admitStart(ctx context.Context) error {
	usage, err := s.nodeUsage(ctx)
	if err != nil {
		return err
	}
	return domain.AdmitStart(s.Settings(ctx).limits(), usage, s.cameraCost())
}

// errorIs is a small helper for store lookups.
func notFound(err error) bool {
	return errors.Is(store.NotFound(err), domain.ErrNotFound)
}
