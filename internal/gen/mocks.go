package gen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// MockTarget names one interface to regenerate with mockgen.
type MockTarget struct {
	Source string // interface file path
	Dest   string // mock output path
	Name   string // interface name
}

// MockTargetsFor derives the standard db/controller mock targets (A4.3) —
// the one table for the artifact conventions (mock_store.go /
// mock_controller.go, `<Service>Store` / `<Service>Controller`). Paths that
// do not exist are omitted (gentest best-effort semantics); convert passes
// existence through to RunMocks, so the filter is harmless there.
func MockTargetsFor(serviceDir, service string) []MockTarget {
	return []MockTarget{
		{
			Source: filepath.Join(serviceDir, "db", "interface.go"),
			Dest:   filepath.Join(serviceDir, "db", "mock_store.go"),
			Name:   service + "Store",
		},
		{
			Source: filepath.Join(serviceDir, "controller", "interface.go"),
			Dest:   filepath.Join(serviceDir, "controller", "mock_controller.go"),
			Name:   service + "Controller",
		},
	}
}

// RunMocks regenerates the uber-go/mock doubles for targets when the
// mockgen binary is available; otherwise it is a WARN + skip — never a
// run failure (plan-conversion §4.7). Shared by `convert` and `gentest`
// (PRD-2026-09-09 GT-D7); missing sources are skipped silently.
func RunMocks(ctx context.Context, targets []MockTarget) {
	log := telemetry.Log(ctx)
	mockgen, err := exec.LookPath("mockgen")
	if err != nil {
		log.Warn("mockgen not found — mocks skipped (uber-go/mock requires docs/dependencies.md onboarding before wiring)")
		return
	}
	for _, t := range targets {
		if _, err := os.Stat(t.Source); err != nil {
			continue
		}
		cmd := exec.Command(mockgen, "-source", t.Source, "-destination", t.Dest, "-package", filepath.Base(filepath.Dir(t.Dest)), t.Name)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Warn("mockgen failed (best-effort)", "interface", t.Name, "output", string(out))
		} else {
			log.Info("mocks generated", "interface", t.Name, "path", t.Dest)
		}
	}
}
