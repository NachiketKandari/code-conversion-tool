package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/common"
	"github.com/Public/convert-tux-to-go/internal/convert"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/ledger"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// Dir-mode convert fan-out (PRD-2026-09-06 v0.8.7): a target directory that
// holds several Tuxedo entry files converts one worker per service
// end-to-end — plan → generate → validate → write — bounded by
// concurrency.workers (the batchpy per-file seam's pattern). Every service
// writes into its own output subtree (baseRoot/<service>) so ledgers, the
// staged-collision guard, and Tier B scope per service; workers=1 keeps
// results byte-identical to sequential single-service runs. Results print in
// extraction order after all workers land, and the first per-service error
// (in that order) becomes the run's exit error.

// serviceOutcome is one worker's per-service result, indexed by input
// position so the ordered summary phase mirrors the input order.
type serviceOutcome struct {
	service string
	base    string
	res     *convert.Result
	led     *ledger.Ledger
	err     error
}

// runConvertFanout converts every entry file as its own service in parallel.
// mappingPath must be a directory of per-service mapping yamls (each
// declaring source: <entry file>); baseRoot is the run's output base and
// every service writes under baseRoot/<service>. excluded carries the entry
// basenames convert.fileFilter dropped — mappings declaring those sources
// are deliberate skips (WARN), not orphan drift.
func runConvertFanout(ctx context.Context, w *convertWiring, target string, mains []*ir.File, files []*ir.File, excluded []string, mappingPath, baseRoot, degrade string) error {
	log := telemetry.Log(ctx)
	if fi, err := os.Stat(mappingPath); err != nil || !fi.IsDir() {
		return fmt.Errorf("convert: %s holds %d service(s) (fileFilter may exclude entries) — pass a mapping directory (one yaml per service, each with source: <entry file>), not %q",
			target, len(mains), mappingPath)
	}
	bySource, err := loadMappingDir(mappingPath)
	if err != nil {
		return err
	}
	mappings, err := matchMappings(ctx, target, mains, bySource, excluded)
	if err != nil {
		return err
	}

	workers := w.cfg.Concurrency.Workers
	if workers < 1 {
		workers = 1
	}
	log.Info("convert dir fan-out", "target", target, "services", len(mains),
		"workers", workers, "mapping_dir", mappingPath)
	results := make([]serviceOutcome, len(mains))
	common.RunIndexed(len(mains), workers, func(i int) {
		main := mains[i]
		mapping := mappings[i]
		// Per-service output isolation: its own subtree keeps the
		// staged-collision guard, ledger, and Tier B scope natural.
		// The inner DB-render pool stays at 1 — the outer pool already
		// bounds the run's goroutines (nested pools would multiply).
		base := filepath.Join(baseRoot, mapping.Service)
		res, led, err := convertOneService(ctx, w, main, files, mapping, base, 1)
		results[i] = serviceOutcome{service: mapping.Service, base: base, res: res, led: led, err: err}
	})

	ok, failed := 0, 0
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			log.Error("convert service failed", "service", r.service, "error", r.err)
			fmt.Printf("%s: failed — %v\n", r.service, r.err)
			if firstErr == nil {
				firstErr = r.err
			}
			failed++
			continue
		}
		ok++
		printServiceSummary(r.service, r.res, r.led, r.base, degrade)
	}
	fmt.Printf("convert: %d service(s) from %s — %d ok, %d failed\n", len(mains), target, ok, failed)
	return firstErr
}

// mappingSource keys a mapping by the entry file it declares (lowercased
// basename) alongside the yaml it came from, for deterministic errors.
type mappingSource struct {
	file    string
	mapping *plan.Mapping
}

// loadMappingDir loads every mapping yaml in dir, keyed by the entry file
// each declares via source:. os.ReadDir returns sorted names, so parse
// errors report deterministically.
func loadMappingDir(dir string) (map[string]mappingSource, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("convert: read mapping dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("convert: no mapping yamls in %s", dir)
	}
	bySource := make(map[string]mappingSource, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		m, err := plan.LoadMapping(path)
		if err != nil {
			return nil, err
		}
		if m.Source == "" {
			return nil, fmt.Errorf("convert: mapping %s: missing source — dir mode requires source: <entry .pc file>", path)
		}
		key := strings.ToLower(filepath.Base(m.Source))
		if prev, dup := bySource[key]; dup {
			return nil, fmt.Errorf("convert: mappings %s and %s both declare source %s", prev.file, path, m.Source)
		}
		bySource[key] = mappingSource{file: path, mapping: m}
	}
	return bySource, nil
}

// matchMappings pairs every entry file with its mapping. Both directions
// are strict: an entry without a mapping is an error (endpoints are
// user-specified, never invented), and a mapping whose source matches no
// entry is drift to surface, not skip — except sources excluded by
// convert.fileFilter, which are deliberate skips (WARN).
func matchMappings(ctx context.Context, target string, mains []*ir.File, bySource map[string]mappingSource, excluded []string) ([]*plan.Mapping, error) {
	log := telemetry.Log(ctx)
	excludedSet := make(map[string]bool, len(excluded))
	for _, e := range excluded {
		excludedSet[e] = true
	}
	matched := make([]*plan.Mapping, 0, len(mains))
	used := make(map[string]bool, len(mains))
	for _, main := range mains {
		key := strings.ToLower(filepath.Base(main.Path))
		ms, ok := bySource[key]
		if !ok {
			return nil, fmt.Errorf("convert: no mapping for entry %s in %s (expected a mapping yaml with source: %s)",
				filepath.Base(main.Path), target, filepath.Base(main.Path))
		}
		used[key] = true
		matched = append(matched, ms.mapping)
	}
	var orphans []string
	for key, ms := range bySource {
		if used[key] {
			continue
		}
		if excludedSet[key] {
			log.Warn("mapping skipped — its source entry is excluded by convert.fileFilter",
				"mapping", filepath.Base(ms.file), "source", key)
			continue
		}
		orphans = append(orphans, filepath.Base(ms.file))
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return nil, fmt.Errorf("convert: mapping(s) %s declare sources matching no entry file in %s", strings.Join(orphans, ", "), target)
	}
	return matched, nil
}
