package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/config"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

const defaultConfigFile = ".tuxgo.yaml"

// runExtract implements `tuxgo extract <file|dir>` — the Phase 2 IR
// extraction + marking flow (deterministic, zero LLM calls). The config is
// loaded and its model profile routed so every run exercises the same
// config seam the generation phases will use; extraction itself never
// touches the LLM.
func runExtract(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	outPath := fs.String("out", "", "Write the IR JSON to this path (single-file mode defaults to stdout)")
	configPath := fs.String("config", "", "Path to .tuxgo.yaml (default: ./.tuxgo.yaml when present, else defaults)")
	fragment := fs.Bool("fragment", false, "Force fragment mode on a single file (PF-3.1: detection is otherwise automatic)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("must provide a .pc/.pcf file or a directory to extract")
	}
	target := positional[0]

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)
	irOpts := irOptions(cfg, *fragment)

	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("cannot access target path %s: %w", target, err)
	}

	if fi.IsDir() {
		files, err := ir.ExtractDirOpts(target, irOpts)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			fmt.Fprintf(os.Stderr, "no .pc or .pcf files found in %s\n", target)
			return nil
		}
		return writeDirIR(ctx, cfg, files)
	}

	file, err := ir.ExtractFileOpts(target, irOpts)
	if err != nil {
		return err
	}
	logFileIR(ctx, file)
	if err := archiveIR(ctx, file, "ir-"+strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path))+".json"); err != nil {
		log.Warn("audit archive unavailable", "error", err)
	}

	data, err := marshalIR(file)
	if err != nil {
		return err
	}
	if *outPath == "" {
		fmt.Println(string(data))
	} else {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			return fmt.Errorf("failed writing IR to %s: %w", *outPath, err)
		}
		log.Info("ir written", "path", *outPath, "queries", len(file.Queries), "conditions", len(file.Conditions))
	}
	log.Info("extraction complete", "file", file.Path, "queries", len(file.Queries), "unique_queries", len(file.UniqueQueries()), "conditions", len(file.Conditions))
	return nil
}

// irOptions maps the run config onto extraction options (PF-4.1 buffer
// registry, PF-3.1 forced fragment mode).
func irOptions(cfg *config.Config, forceFragment bool) ir.Options {
	return ir.Options{BufferRoles: cfg.Buffers.Roles, ForceFragment: forceFragment}
}

// loadRunConfig resolves the run configuration: explicit -config path, else
// ./.tuxgo.yaml when present, else the stock defaults.
func loadRunConfig(explicit string) (*config.Config, string, error) {
	path := explicit
	if path == "" {
		if _, err := os.Stat(defaultConfigFile); err == nil {
			path = defaultConfigFile
		}
	}
	if path == "" {
		return config.Default(), "defaults", nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, path, nil
}

// resolveInput picks the conversion target: the CLI positional wins, else
// convert.input from the yaml, else an error naming both sources.
func resolveInput(positional []string, cfg *config.Config) (string, error) {
	if len(positional) > 0 {
		return positional[0], nil
	}
	if cfg.Convert.Input != "" {
		return cfg.Convert.Input, nil
	}
	return "", fmt.Errorf("no input target: pass a .pc/.pcf file or directory, or set convert.input in .tuxgo.yaml")
}

// resolveBatchInput picks the batchpy conversion target: the CLI positional
// wins, else batchpy.input from the yaml, else an error naming both sources.
func resolveBatchInput(positional []string, cfg *config.Config) (string, error) {
	if len(positional) > 0 {
		return positional[0], nil
	}
	if cfg.Batchpy.Input != "" {
		return cfg.Batchpy.Input, nil
	}
	return "", fmt.Errorf("no input target: pass a .pc/.pcf file or directory, or set batchpy.input in .tuxgo.yaml")
}

// resolveMapping picks the endpoint mapping: the -mapping flag wins, else
// convert.mapping from the yaml, else an error naming both sources.
func resolveMapping(flagValue string, cfg *config.Config) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if cfg.Convert.Mapping != "" {
		return cfg.Convert.Mapping, nil
	}
	return "", fmt.Errorf("must provide -mapping <yaml> or set convert.mapping in .tuxgo.yaml — endpoints are user-specified (PRD §4.2.8)")
}

// logConfigRouting proves the yaml routing seam end to end: which profile
// the run routes to, its endpoint, and where the key came from — never the
// key value itself.
func logConfigRouting(ctx context.Context, cfg *config.Config, source string) {
	log := telemetry.Log(ctx)
	m, err := cfg.Route("")
	if err != nil {
		log.Warn("config routing failed", "error", err, "config", source)
		return
	}
	_, keySource, keyErr := m.ResolveKey()
	if keyErr != nil {
		keySource = "unresolved"
	}
	log.Info("config routed",
		"config", source,
		"profile", m.Name,
		"model", m.Model,
		"api_base", m.APIBase,
		"key_source", keySource,
	)
}

func writeDirIR(ctx context.Context, cfg *config.Config, files []*ir.File) error {
	log := telemetry.Log(ctx)
	stateDir := cfg.Paths.State
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed creating IR state dir %s: %w", stateDir, err)
	}

	for _, file := range files {
		logFileIR(ctx, file)
		base := strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path))
		path := filepath.Join(stateDir, base+".ir.json")
		data, err := marshalIR(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("failed writing IR to %s: %w", path, err)
		}
		if err := archiveIR(ctx, file, "ir-"+base+".json"); err != nil {
			log.Warn("audit archive unavailable", "error", err)
		}

		unresolved := 0
		for _, ext := range file.ExternalFns {
			if !ext.Resolved {
				unresolved++
			}
		}
		fmt.Printf("%s: entry=%s conditions=%d queries=%d unique=%d external_fns=%d unresolved=%d unbalanced=%d → %s\n",
			filepath.Base(file.Path), orDash(file.Entry), len(file.Conditions),
			len(file.Queries), len(file.UniqueQueries()), len(file.ExternalFns), unresolved, len(file.Unbalanced), path)
	}
	log.Info("extraction complete", "files", len(files), "state_dir", stateDir)
	return nil
}

// logFileIR emits the per-unit extraction trace (architecture.md §4 schema).
func logFileIR(ctx context.Context, file *ir.File) {
	log := telemetry.Log(ctx)
	for _, q := range file.Queries {
		log.Info("extracted query unit",
			"file", file.Path,
			"query_id", q.ID,
			"query_type", string(q.Type),
			"template_id", q.TemplateID,
			"table", strings.Join(q.Tables, ","),
			"binds", len(q.Binds),
			"cursor_flattened", q.CursorFlattened,
			"duplicate_of", q.DuplicateOf,
			"lines", fmt.Sprintf("%d-%d", q.StartLine, q.EndLine),
		)
	}
	for _, ext := range file.ExternalFns {
		if ext.Resolved {
			log.Info("external fn resolved", "file", file.Path, "fn", ext.Name, "defined_in", ext.DefinedIn, "queries", strings.Join(ext.QueryIDs, ","))
		} else {
			log.Warn("external fn unresolved — generation blocks pending its defining file", "file", file.Path, "fn", ext.Name)
		}
	}
	for _, u := range file.Unbalanced {
		log.Warn("unbalanced region — the parse continues leniently past it, so downstream facts may be truncated",
			"file", file.Path, "kind", u.Kind, "line", u.Line, "col", u.Col)
	}
	log.Info("file extracted",
		"file", file.Path,
		"entry", orDash(file.Entry),
		"conditions", len(file.Conditions),
		"queries", len(file.Queries),
		"host_vars", len(file.HostVars),
		"unbalanced", len(file.Unbalanced),
	)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func marshalIR(file *ir.File) ([]byte, error) {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed serializing IR: %w", err)
	}
	return data, nil
}

// archiveIR persists one file's IR into the run's audit folder (§4.7).
func archiveIR(ctx context.Context, file *ir.File, name string) error {
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		return err
	}
	path, err := rec.Write(name, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(file)
	})
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("ir archived", "path", path)
	return nil
}
