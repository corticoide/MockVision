package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/camera"
	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/sdk/engine"
	"github.com/corticoide/mockvision/sdk/plugin"
)

// describeTimeout bounds the run of a plugin's program at install.
const describeTimeout = 20 * time.Second

// engineSet is what resolves engines: the built-in ones and the enabled
// plugins', with what a camera needs to run each plugin.
type engineSet struct {
	catalog *engines.Catalog
	plugins map[string]ipc.Plugin
}

// engineCatalog resolves the engines profiles may use.
func (s *Service) engineCatalog() *engines.Catalog { return s.engs.Load().catalog }

// PluginView is an installed plugin: the engine its package provides, what
// it may do and whether it runs.
type PluginView struct {
	ID              string         `json:"id"`
	PackageID       string         `json:"package_id"`
	Package         string         `json:"package"`
	Engine          string         `json:"engine"`
	Version         string         `json:"version"`
	Role            string         `json:"role"`
	Permissions     []string       `json:"permissions"`
	Sockets         []PluginSocket `json:"sockets"`
	Emits           []string       `json:"emits"`
	SignatureStatus string         `json:"signature_status"`
	Signer          string         `json:"signer,omitempty"`
	SHA256          string         `json:"sha256"`
	Enabled         bool           `json:"enabled"`
	// ApprovedBy is who enabled it last, approving its permissions.
	ApprovedBy  string     `json:"approved_by,omitempty"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	InstalledAt time.Time  `json:"installed_at"`
}

// PluginSocket is a port the plugin's engine listens on.
type PluginSocket struct {
	Name        string `json:"name"`
	Network     string `json:"network"`
	DefaultPort int    `json:"default_port"`
}

func pluginView(p db.Plugin, pkgID, signature, signer, sha string) PluginView {
	var perms []string
	_ = json.Unmarshal([]byte(p.PermissionsJson), &perms)
	var d engine.Descriptor
	_ = json.Unmarshal([]byte(p.DescriptorJson), &d)
	v := PluginView{ID: p.ID, PackageID: p.PackageID, Package: pkgID, Engine: p.Engine, Version: p.EngineVersion, Role: string(d.Role),
		Permissions: nonNil(perms), Sockets: []PluginSocket{}, Emits: nonNil(d.Emits), SignatureStatus: signature, Signer: signer, SHA256: sha,
		Enabled: store.Bool(p.Enabled), ApprovedBy: p.ApprovedBy, InstalledAt: store.Time(p.InstalledAt)}
	for _, so := range d.Sockets {
		v.Sockets = append(v.Sockets, PluginSocket{Name: so.Name, Network: so.Network, DefaultPort: so.DefaultPort})
	}
	if p.ApprovedAt.Valid {
		t := store.Time(p.ApprovedAt.Int64)
		v.ApprovedAt = &t
	}
	return v
}

func pluginRowView(r db.GetPluginRow) PluginView {
	return pluginView(db.Plugin{ID: r.ID, PackageID: r.PackageID, Engine: r.Engine, EngineVersion: r.EngineVersion, Executable: r.Executable,
		Dir: r.Dir, PermissionsJson: r.PermissionsJson, DescriptorJson: r.DescriptorJson, Enabled: r.Enabled, ApprovedBy: r.ApprovedBy,
		ApprovedAt: r.ApprovedAt, InstalledAt: r.InstalledAt}, r.PkgID, r.SignatureStatus, r.Signer, r.Sha256)
}

// ListPlugins returns the installed plugins.
func (s *Service) ListPlugins(ctx context.Context) ([]PluginView, error) {
	rows, err := s.store.R().ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PluginView, 0, len(rows))
	for _, r := range rows {
		out = append(out, pluginRowView(db.GetPluginRow(r)))
	}
	return out, nil
}

// GetPlugin returns an installed plugin.
func (s *Service) GetPlugin(ctx context.Context, id string) (*PluginView, error) {
	r, err := s.store.R().GetPlugin(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	v := pluginRowView(r)
	return &v, nil
}

// installPlugin installs a validated plugin package: its files unpacked
// where cameras read them, and its descriptor as its program gives it,
// run confined and unable to connect anywhere. It is installed disabled:
// an admin approves its permissions by enabling it (D86).
func (s *Service) installPlugin(ctx context.Context, actor Actor, data []byte, res *pkg.Result, source string) (*ImportResult, error) {
	rep, pl := res.Report, res.Plugin
	if pl == nil {
		return nil, errors.New("the plugin package was validated without its plugin")
	}
	existing, err := s.store.R().GetPackageByKey(ctx, db.GetPackageByKeyParams{Kind: rep.Kind, PkgID: rep.ID, Version: rep.Version})
	if err == nil {
		if existing.Sha256 != rep.SHA256 {
			return nil, domain.Conflict("version", "%s@%s is already installed with different content; publish a new version", rep.ID, rep.Version)
		}
		row, err := s.store.R().GetPluginByPackage(ctx, existing.ID)
		if err != nil {
			return nil, err
		}
		v, err := s.GetPlugin(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		return &ImportResult{Plugin: v, Report: rep}, nil
	} else if !notFound(err) {
		return nil, err
	}
	if s.opts.Exe == "" {
		return nil, domain.Invalid("file", "plugins run confined by the MockVision binary, which this service was not given")
	}

	dir := filepath.Join(s.opts.DataDir, "plugins", rep.SHA256)
	if err := unpackPlugin(data, dir); err != nil {
		return nil, err
	}
	reject := func(format string, args ...any) (*ImportResult, error) {
		_ = os.RemoveAll(dir)
		rep.Problems = append(rep.Problems, profile.Problem{Step: pkg.StepPlugin, Severity: profile.SeverityError, File: pkg.PluginBinaryPath("<arch>", pl.Executable),
			Message: fmt.Sprintf(format, args...)})
		rep.Steps = append(rep.Steps, pkg.Step{Name: "describe", Status: "failed"})
		return nil, &ImportError{Report: rep}
	}
	spec := ipc.Plugin{Engine: pl.Engine, Version: pl.Version, Dir: dir, Executable: pl.Executable, Permissions: pl.Permissions}
	dctx, cancel := context.WithTimeout(ctx, describeTimeout)
	desc, err := camera.DescribePlugin(dctx, s.opts.Exe, spec)
	cancel()
	if err != nil {
		return reject("%v", err)
	}
	if msg := checkDescriptor(desc, pl); msg != "" {
		return reject("%s", msg)
	}
	rep.Steps = append(rep.Steps, pkg.Step{Name: "describe", Status: "passed", Note: fmt.Sprintf("%s %s, %s", desc.Name, desc.Version, desc.Role)})

	manifest, _ := json.Marshal(res.Manifest)
	report, _ := json.Marshal(rep)
	perms, _ := json.Marshal(nonNil(pl.Permissions))
	descJSON, _ := json.Marshal(desc)
	now := time.Now().UnixMilli()
	pkgID, plugID := ulid.Make().String(), ulid.Make().String()
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.InsertPackage(ctx, db.InsertPackageParams{
			ID: pkgID, Kind: rep.Kind, PkgID: rep.ID, Version: rep.Version, Sha256: rep.SHA256, SignatureStatus: rep.Signature,
			Signer: rep.Signer, ManifestJson: string(manifest), ReportJson: string(report), Enabled: 0, InstalledAt: now, Source: source,
		}); err != nil {
			return err
		}
		return q.InsertPlugin(ctx, db.InsertPluginParams{ID: plugID, PackageID: pkgID, Engine: pl.Engine, EngineVersion: pl.Version,
			Executable: pl.Executable, Dir: dir, PermissionsJson: string(perms), DescriptorJson: string(descJSON), InstalledAt: now})
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		if store.IsUnique(err) {
			return nil, domain.Conflict("version", "engine %s %s is already installed", pl.Engine, pl.Version)
		}
		return nil, err
	}
	s.audit(ctx, actor, "package.import", "plugin", plugID, map[string]any{"id": rep.ID, "version": rep.Version, "engine": pl.Engine,
		"permissions": pl.Permissions, "signature": rep.Signature, "sha256": rep.SHA256})
	v, err := s.GetPlugin(ctx, plugID)
	if err != nil {
		return nil, err
	}
	s.pub.Publish("plugins", "installed", v)
	return &ImportResult{Plugin: v, Report: rep, Created: true}, nil
}

// checkDescriptor compares what a plugin's program says it is with its
// manifest; it returns what does not match.
func checkDescriptor(d engine.Descriptor, pl *pkg.PluginInfo) string {
	switch {
	case d.Name != pl.Engine:
		return fmt.Sprintf("the program provides engine %q, the manifest says %q", d.Name, pl.Engine)
	case d.Version != pl.Version:
		return fmt.Sprintf("the program is version %q, the package %q", d.Version, pl.Version)
	case d.Contract != engine.Contract:
		return fmt.Sprintf("the program implements engine contract %d; this MockVision speaks %d", d.Contract, engine.Contract)
	case d.Role != engine.RoleServer && d.Role != engine.RoleClient:
		return fmt.Sprintf("the program's role %q is not one an engine has", d.Role)
	}
	names := map[string]bool{}
	for _, so := range d.Sockets {
		if so.Network != "tcp" && so.Network != "udp" {
			return fmt.Sprintf("socket %s: network %q is neither tcp nor udp", so.Name, so.Network)
		}
		if names[so.Name] || so.Name == "" {
			return fmt.Sprintf("socket %q is unnamed or named twice", so.Name)
		}
		names[so.Name] = true
	}
	if len(d.Sockets) > 0 && !hasString(pl.Permissions, plugin.PermNetListen) {
		return fmt.Sprintf("the engine listens on %d sockets but the package does not ask for %s", len(d.Sockets), plugin.PermNetListen)
	}
	if err := profile.CheckEngineSchema(d); err != nil {
		return fmt.Sprintf("the engine's configuration schema is invalid: %v", err)
	}
	return ""
}

func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// unpackPlugin writes a package's files under dir: programs executable,
// everything readable by the cameras, nothing writable but by the service.
func unpackPlugin(data []byte, dir string) error {
	files, err := pkg.Files(data)
	if err != nil {
		return err
	}
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	// Modes are set past the service's umask: the cameras' user reads it.
	if err := mkdirAll(tmp); err != nil {
		return err
	}
	for name, b := range files {
		if name == "manifest.sig" {
			continue
		}
		p := filepath.Join(tmp, filepath.FromSlash(name))
		if !strings.HasPrefix(p, tmp+string(filepath.Separator)) {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("unsafe path %q", name)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(name, "bin/") {
			mode = 0o755
		}
		if err := mkdirAll(filepath.Dir(p)); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		if err := os.WriteFile(p, b, mode); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
		if err := os.Chmod(p, mode); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	_ = os.RemoveAll(dir)
	return os.Rename(tmp, dir)
}

// mkdirAll creates dir and its missing parents with mode 0755.
func mkdirAll(dir string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	if err := mkdirAll(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// SetPluginEnabled enables a plugin, which approves its permissions and
// makes its engine available to profiles and cameras, or disables it. One
// version of an engine is enabled at a time; an unsigned plugin is enabled
// only where the settings allow unsigned plugins (D84). Running cameras
// keep the engine they started with until they restart.
func (s *Service) SetPluginEnabled(ctx context.Context, actor Actor, id string, enabled bool) (*PluginView, error) {
	row, err := s.store.R().GetPlugin(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	if enabled {
		switch row.SignatureStatus {
		case pkg.SignatureOfficial, pkg.SignatureTrusted:
		case pkg.SignatureUnsigned:
			if !s.Settings(ctx).AllowUnsignedPlugins {
				return nil, domain.Conflict("enabled", "plugin %s is not signed by a key this node trusts; allow unsigned plugins in the settings to enable it", row.PkgID)
			}
		default:
			return nil, domain.Conflict("enabled", "plugin %s has an invalid signature", row.PkgID)
		}
		err = s.store.Tx(ctx, func(q *db.Queries) error {
			if err := q.DisableEngine(ctx, row.Engine); err != nil {
				return err
			}
			return q.EnablePlugin(ctx, db.EnablePluginParams{ID: id, ApprovedBy: actorLabel(actor), ApprovedAt: sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}})
		})
	} else {
		_, err = s.store.W().DisablePlugin(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	action := "plugin.disable"
	if enabled {
		action = "plugin.enable"
	}
	s.audit(ctx, actor, action, "plugin", id, map[string]any{"engine": row.Engine, "version": row.EngineVersion, "permissions": json.RawMessage(row.PermissionsJson)})
	if err := s.loadPlugins(ctx); err != nil {
		return nil, err
	}
	v, err := s.GetPlugin(ctx, id)
	if err != nil {
		return nil, err
	}
	s.pub.Publish("plugins", "changed", v)
	return v, nil
}

// disableUnsignedPlugins disables the unsigned plugins, once the settings
// stop allowing them.
func (s *Service) disableUnsignedPlugins(ctx context.Context, actor Actor) {
	rows, err := s.store.R().ListPlugins(ctx)
	if err != nil {
		s.log.Error("cannot list the plugins", "error", err)
		return
	}
	for _, r := range rows {
		if store.Bool(r.Enabled) && r.SignatureStatus != pkg.SignatureOfficial && r.SignatureStatus != pkg.SignatureTrusted {
			if _, err := s.SetPluginEnabled(ctx, actor, r.ID, false); err != nil {
				s.log.Error("cannot disable an unsigned plugin", "plugin", r.PkgID, "error", err)
			}
		}
	}
}

// loadPlugins makes the enabled plugins' engines resolvable.
func (s *Service) loadPlugins(ctx context.Context) error {
	rows, err := s.store.R().ListEnabledPlugins(ctx)
	if err != nil {
		s.log.Error("cannot load the plugins", "error", err)
		return err
	}
	set := &engineSet{plugins: map[string]ipc.Plugin{}}
	extra := map[string]engine.Factory{}
	for _, r := range rows {
		var d engine.Descriptor
		var perms []string
		if err := errors.Join(json.Unmarshal([]byte(r.DescriptorJson), &d), json.Unmarshal([]byte(r.PermissionsJson), &perms)); err != nil {
			s.log.Error("a plugin's record is unreadable", "engine", r.Engine, "error", err)
			continue
		}
		extra[r.Engine] = engines.Described(d)
		set.plugins[r.Engine] = ipc.Plugin{Engine: r.Engine, Version: r.EngineVersion, Dir: r.Dir, Executable: r.Executable,
			Permissions: perms, Descriptor: d}
	}
	set.catalog = engines.Builtin().With(extra)
	s.engs.Store(set)
	return nil
}

// pluginDescriptors describes the enabled plugins' engines, for the import
// pipeline.
func (s *Service) pluginDescriptors() []engine.Descriptor {
	var out []engine.Descriptor
	for _, p := range s.engs.Load().plugins {
		out = append(out, p.Descriptor)
	}
	return out
}

// pluginsFor lists the plugins a camera's profile uses.
func (s *Service) pluginsFor(doc *profile.Document) ([]ipc.Plugin, error) {
	set := s.engs.Load()
	var out []ipc.Plugin
	seen := map[string]bool{}
	for _, inst := range profile.SortedKeys(doc.Engines) {
		name, _, err := profile.EngineName(doc.Engines[inst])
		if err != nil {
			return nil, err
		}
		if p, ok := set.plugins[name]; ok && !seen[name] {
			seen[name] = true
			out = append(out, p)
		}
	}
	return out, nil
}
