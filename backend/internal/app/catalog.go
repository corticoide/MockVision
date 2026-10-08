package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// installCatalog installs the official catalog shipped in the binary: the
// profiles, or the versions of them, this node does not have yet (D81).
// They go through the same pipeline as any package. One that cannot be
// installed is logged and left out; the node starts anyway.
func (s *Service) installCatalog(ctx context.Context) {
	if s.opts.Catalog == nil {
		return
	}
	pkgs, err := pkg.BuildCatalog(s.opts.Catalog)
	if err != nil {
		s.log.Error("cannot read the official catalog", "error", err)
		return
	}
	actor := Actor{Type: "system", Name: "catalog"}
	for _, p := range pkgs {
		sum := sha256.Sum256(p.Data)
		existing, err := s.store.R().GetPackageByKey(ctx, db.GetPackageByKeyParams{Kind: "profile", PkgID: p.ID, Version: p.Version})
		if err == nil {
			if existing.Sha256 != hex.EncodeToString(sum[:]) {
				s.log.Warn("this build's catalog profile differs from the one installed under its version; catalog changes need a new version",
					"profile", p.ID, "version", p.Version)
			}
			continue
		} else if !notFound(err) {
			s.log.Error("cannot read the installed profiles", "error", err)
			return
		}
		res, err := s.inspectWithParent(ctx, p.Name, p.Data)
		if err != nil {
			s.log.Error("cannot validate a catalog profile", "profile", p.ID, "version", p.Version, "error", err)
			continue
		}
		if res.Report.OK() && len(res.Fixtures) > 0 {
			srep, err := s.runSelfTest(ctx, res, func(string) {})
			applySelfTest(res, srep, err)
		}
		out, err := s.installPackage(ctx, actor, p.Data, res, sourceCatalog)
		if err != nil {
			s.log.Error("cannot install a catalog profile", "profile", p.ID, "version", p.Version, "error", err, "problems", res.Report.Problems)
			continue
		}
		s.log.Info("catalog profile installed", "profile", p.ID, "version", p.Version, "level", out.Profile.Level, "signature", out.Profile.SignatureStatus)
	}
}
