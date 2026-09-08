package testscan

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const dbInterfaceSrc = `package db

import "context"

type NavStore interface {
	GetA(ctx context.Context) ([]int, error)
	GetB(ctx context.Context) (int, error)
}

func NewNavStore() NavStore { return nil }
`

const dbStoreSrc = `package db

import "context"

type store struct{}

func (s *store) GetA(ctx context.Context) ([]int, error) { return nil, nil }

func (s *store) GetB(ctx context.Context) (int, error) { return 0, nil }

func (s *store) getHidden(ctx context.Context) error { return nil }

func Util() int { return 42 }
`

const dbTestSrc = `package db

import "testing"

type NavStoreSuite struct{ s *store }

func TestNavStoreSuite(t *testing.T) {}

func (x *NavStoreSuite) TestGetA() {}

func (x *NavStoreSuite) TestGetBCallsIt() { x.s.GetB(nil) }

func TestOther(t *testing.T) { _ = Util() }

func (x *NavStoreSuite) helperNotTest() { x.s.getHidden(nil) }
`

const dbMockSrc = `package db

import "context"

type MockNavStore struct{}

func (m *MockNavStore) GetA(ctx context.Context) ([]int, error) { return nil, nil }
`

const controllerSrc = `package controller

type Controller struct{}

func (c *Controller) List() error { return nil }
`

const controllerTestSrc = `package controller

import "testing"

func TestList(t *testing.T) {}
`

const handlerSrc = `package handler

type Handler struct{}

func (h *Handler) Serve() {}
`

const modelsSrc = `package models

type Req struct{}
`

func buildServiceTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	svc := filepath.Join(root, "svcnav")
	writeFile(t, filepath.Join(svc, "db", "interface.go"), dbInterfaceSrc)
	writeFile(t, filepath.Join(svc, "db", "store.go"), dbStoreSrc)
	writeFile(t, filepath.Join(svc, "db", "store_test.go"), dbTestSrc)
	writeFile(t, filepath.Join(svc, "db", "mock_store.go"), dbMockSrc)
	writeFile(t, filepath.Join(svc, "controller", "controller.go"), controllerSrc)
	writeFile(t, filepath.Join(svc, "controller", "controller_test.go"), controllerTestSrc)
	writeFile(t, filepath.Join(svc, "handler", "handler.go"), handlerSrc)
	writeFile(t, filepath.Join(svc, "models", "models.go"), modelsSrc)
	return root
}

func findFunc(t *testing.T, r *Report, layer Layer, name string) Func {
	t.Helper()
	for _, s := range r.Services {
		for _, l := range s.Layers {
			if l.Layer != layer {
				continue
			}
			for _, f := range l.Funcs {
				if f.Name == name {
					return f
				}
			}
		}
	}
	t.Fatalf("function %s not found in layer %s", name, layer)
	return Func{}
}

func TestResolveService(t *testing.T) {
	root := buildServiceTree(t)
	svc := filepath.Join(root, "svcnav")
	tgt, err := Resolve(svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Mode != ModeService || tgt.Root != svc || tgt.LayerDirs[0].Service != "svcnav" {
		t.Fatalf("service resolve wrong: %+v", tgt)
	}
	if len(tgt.LayerDirs) != 3 {
		t.Fatalf("want 3 layer dirs, got %d: %+v", len(tgt.LayerDirs), tgt.LayerDirs)
	}
	for i, want := range []Layer{LayerDB, LayerController, LayerHandler} {
		if tgt.LayerDirs[i].Layer != want {
			t.Fatalf("layer dir %d: got %s, want %s", i, tgt.LayerDirs[i].Layer, want)
		}
	}
}

func TestResolveLayerDirAndFile(t *testing.T) {
	root := buildServiceTree(t)
	dbDir := filepath.Join(root, "svcnav", "db")
	tgt, err := Resolve(dbDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Mode != ModeLayer || tgt.LayerDirs[0].Dir != dbDir || tgt.LayerDirs[0].Layer != LayerDB {
		t.Fatalf("layer resolve wrong: %+v", tgt)
	}

	tgt, err = Resolve(filepath.Join(dbDir, "store.go"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Mode != ModeFile || tgt.LayerDirs[0].Layer != LayerDB || tgt.LayerDirs[0].Service != "svcnav" {
		t.Fatalf("file resolve wrong: %+v", tgt)
	}
}

func TestResolveOtherFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "x", "misc.go"), "package x\n\nfunc M() {}\n")
	tgt, err := Resolve(filepath.Join(root, "x", "misc.go"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Mode != ModeFile || tgt.LayerDirs[0].Layer != LayerOther || tgt.LayerDirs[0].Service != "" {
		t.Fatalf("other-file resolve wrong: %+v", tgt)
	}
}

func TestResolveRootAndDescent(t *testing.T) {
	root := buildServiceTree(t)
	svc2 := filepath.Join(root, "svc2")
	writeFile(t, filepath.Join(svc2, "handler", "h.go"), handlerSrc)
	tgt, err := Resolve(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Mode != ModeRoot || len(tgt.LayerDirs) != 4 {
		t.Fatalf("root resolve wrong: mode=%s dirs=%+v", tgt.Mode, tgt.LayerDirs)
	}

	nested := t.TempDir()
	writeFile(t, filepath.Join(nested, "pkg", "services", "svcx", "db", "x.go"), dbInterfaceSrc)
	tgt, err = Resolve(nested, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Mode != ModeRoot || tgt.Root != filepath.Join(nested, "pkg", "services") || len(tgt.LayerDirs) != 1 {
		t.Fatalf("pkg/services descent wrong: %+v", tgt)
	}
}

func TestResolveErrors(t *testing.T) {
	root := buildServiceTree(t)
	if _, err := Resolve(filepath.Join(root, "svcnav", "models", "models.go"), nil); err == nil || !strings.Contains(err.Error(), "models") {
		t.Fatalf("models target must error, got %v", err)
	}
	if _, err := Resolve(filepath.Join(root, "svcnav", "db", "store.go"), []Layer{LayerHandler}); err == nil || !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("filtered layer must error, got %v", err)
	}
	empty := filepath.Join(root, "empty")
	writeFile(t, filepath.Join(empty, "readme.txt"), "x")
	if _, err := Resolve(empty, nil); err == nil || !strings.Contains(err.Error(), "does not look like") {
		t.Fatalf("random dir must error, got %v", err)
	}
	if _, err := Resolve(filepath.Join(root, "svcnav"), []Layer{"models"}); err == nil || !strings.Contains(err.Error(), "not a testable layer") {
		t.Fatalf("models filter must error, got %v", err)
	}
}

func TestScanInventoryAndDetection(t *testing.T) {
	root := buildServiceTree(t)
	tgt, err := Resolve(filepath.Join(root, "svcnav"), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := tgt.Scan()
	if err != nil {
		t.Fatal(err)
	}

	getA := findFunc(t, rep, LayerDB, "GetA")
	if !getA.HasTest || len(getA.Evidence) != 1 || getA.Evidence[0].Kind != "name" || getA.Evidence[0].Test != "TestGetA" {
		t.Fatalf("GetA evidence wrong: %+v", getA)
	}
	if getA.Evidence[0].File != "store_test.go" || getA.Evidence[0].Line <= 0 {
		t.Fatalf("GetA evidence location wrong: %+v", getA.Evidence[0])
	}
	if getA.Recv != "store" || !getA.Exported {
		t.Fatalf("GetA inventory wrong: %+v", getA)
	}

	getB := findFunc(t, rep, LayerDB, "GetB")
	if !getB.HasTest || getB.Evidence[0].Kind != "call" || getB.Evidence[0].Test != "TestGetBCallsIt" {
		t.Fatalf("GetB evidence wrong: %+v", getB)
	}

	util := findFunc(t, rep, LayerDB, "Util")
	if !util.HasTest || util.Evidence[0].Kind != "call" || util.Evidence[0].Test != "TestOther" {
		t.Fatalf("Util evidence wrong: %+v", util)
	}

	if f := findFunc(t, rep, LayerDB, "NewNavStore"); f.HasTest {
		t.Fatalf("NewNavStore must be a gap: %+v", f)
	}
	if !findFunc(t, rep, LayerDB, "NewNavStore").Ctor {
		t.Fatalf("NewNavStore must be flagged ctor")
	}
	if f := findFunc(t, rep, LayerDB, "getHidden"); f.HasTest || f.Exported {
		t.Fatalf("getHidden must stay a gap (helper-only call site): %+v", f)
	}
	for _, s := range rep.Services {
		for _, l := range s.Layers {
			for _, f := range l.Funcs {
				if f.Name == "MockNavStore" || f.Recv == "MockNavStore" {
					t.Fatalf("mock file leaked into inventory: %+v", f)
				}
			}
		}
	}

	list := findFunc(t, rep, LayerController, "List")
	if !list.HasTest || list.Evidence[0].Kind != "name" {
		t.Fatalf("List evidence wrong: %+v", list)
	}
	if f := findFunc(t, rep, LayerHandler, "Serve"); f.HasTest {
		t.Fatalf("Serve must be a gap: %+v", f)
	}

	total, tested, gaps := rep.Counts()
	if total != 7 || tested != 4 || gaps != 3 {
		t.Fatalf("counts wrong: total=%d tested=%d gaps=%d", total, tested, gaps)
	}
	if len(rep.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", rep.Warnings)
	}
}

func TestScanWarningsOnBrokenFile(t *testing.T) {
	root := buildServiceTree(t)
	writeFile(t, filepath.Join(root, "svcnav", "db", "broken.go"), "not go at all")
	tgt, err := Resolve(filepath.Join(root, "svcnav"), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := tgt.Scan()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, wn := range rep.Warnings {
		if strings.Contains(wn, "skipped") && strings.Contains(wn, "broken.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("broken file must warn, got %v", rep.Warnings)
	}
	if f := findFunc(t, rep, LayerDB, "GetA"); !f.HasTest {
		t.Fatalf("scan must continue past broken file: %+v", f)
	}
}

func TestReportText(t *testing.T) {
	root := buildServiceTree(t)
	tgt, err := Resolve(filepath.Join(root, "svcnav"), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := tgt.Scan()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"gentest gap report",
		"service svcnav",
		"gap NewNavStore",
		"ok  ",
		"TestGetA (name) store_test.go:",
		"gentest: 7 functions, 4 tested, 3 gaps",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("text report missing %q:\n%s", want, out)
		}
	}
}

func TestReportJSON(t *testing.T) {
	root := buildServiceTree(t)
	tgt, err := Resolve(filepath.Join(root, "svcnav"), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := tgt.Scan()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	bTotal, bTested, bGaps := back.Counts()
	if bTotal != 7 || bTested != 4 || bGaps != 3 {
		t.Fatalf("json round-trip lost counts: %d/%d/%d", bTotal, bTested, bGaps)
	}
}
