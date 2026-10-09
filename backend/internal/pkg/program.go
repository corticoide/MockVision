package pkg

import (
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/scraper"
)

// ProgramInfo is a capture program package that passed the pipeline.
type ProgramInfo struct {
	ProgramID  string   `json:"program_id"`
	Version    string   `json:"version"`
	Name       string   `json:"name"`
	Vendors    []string `json:"vendors,omitempty"`
	Steps      int      `json:"steps"`
	ProgramRaw []byte   `json:"-"`
}

// inspectProgram checks a program package: a program.yaml whose steps are
// all read-only (RN-17). The id in the manifest must match the program's.
func (in *inspector) inspectProgram(m *Manifest, files map[string][]byte) {
	raw, ok := files["program.yaml"]
	if !ok {
		in.problem(profile.StepIntegrity, "manifest.yaml", 0, "a program package needs program.yaml")
		in.step(StepProgram, "failed", "")
		return
	}
	prog, err := scraper.ParseProgram(raw)
	if err != nil {
		in.problem(StepProgram, "program.yaml", 0, "%v", err)
		in.step(StepProgram, "failed", "")
		return
	}
	if m.ID != "" && prog.ID != "" && m.ID != prog.ID {
		in.problem(profile.StepCompatibility, "program.yaml", 0, "program.id %s does not match the manifest id %s", prog.ID, m.ID)
		in.step(StepProgram, "failed", "")
		return
	}
	in.res.Program = &ProgramInfo{ProgramID: prog.ID, Version: prog.Version, Name: prog.Name, Vendors: prog.Compatible.Vendors,
		Steps: len(prog.Steps), ProgramRaw: raw}
	in.step(StepProgram, "passed", prog.Name)
}

// StepProgram checks a capture program package.
const StepProgram = "program"
