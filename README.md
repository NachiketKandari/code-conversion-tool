# tuxgo — Pro*C/Tuxedo to idiomatic Go

`tuxgo` converts Pro*C/Tuxedo services into idiomatic Go that plugs into an existing service layout (`models/`, `db/`, `controller/`, `handler/`). The core idea: a small LLM cannot convert a monolithic `.pc` file in one call, so the tool does all structure discovery deterministically — decomposition, query classification, template selection, token budgeting — and the model only fills small, template-shaped gaps.

**Status:** the triage analyzer (`tuxgo analyze`), the config + LLM routing seam (Phase 1), the deterministic IR extraction + marking (`tuxgo extract`, Phase 2), interface accumulation (Phase 3), the token budgeter + query-replaced views (Phase 4), and the conversion pipeline (`tuxgo plan` / `tuxgo convert`, Phase 5) ship. Tier-B compilation and the live API runs happen on the workspace laptop where the target service exists; the roadmap below is the short version — the full design docs (`docs/`, `prd/`) are local to the working repo, not shipped.

## Requirements

- Go 1.26+

## Build & run

```sh
go build -o bin/tuxgo ./cmd/tuxgo   # bin/ is gitignored
# or without building:
go run ./cmd/tuxgo <command> [flags] <path>
```

## `analyze` — complexity triage

Scans `.pc`/`.pcf` files and ranks them by conversion complexity. A single file prints one report; a directory walks recursively and produces one CSV row per file:

```sh
go run ./cmd/tuxgo analyze testdata/nav
go run ./cmd/tuxgo analyze path/to/pc-files -csv report.csv
```

Flags may appear before or after the target path:

| Flag | Meaning |
|---|---|
| `-csv <path>` | write the CSV to a file (default: stdout) |
| `-weights <csv>` | re-score using per-fn weights edited into a previously generated CSV |
| `-verbose` | debug-level console logging (the log file always captures debug) |
| `-log-dir <dir>` | structured log directory (default `conversion_logs/logs`) |

### Scoring rubric

+1 per DB query (`EXEC SQL` block — SELECT, INSERT, UPDATE, DELETE, MERGE), +10 per **complex** external fn (defining body has SQL/`tpcall`, or unknown), +5 per **simple** external fn (pure logic/conversion), +20 per `tpcall`, and +1 per unit of **branching factor** — every if/else-if header contributes +1 × 2^(number of enclosing if/else-if blocks), so `if` inside `if` inside `if` contributes 1+2+4. `else` headers never contribute, an `else` body does not nest its chain, an `else if` is a sibling of its chain (+1 flat), and loops (`while`/`for`) are not nesting contexts; unbraced parents create no nesting level. External fns are project-convention symbols (`fn_*`/`chk_*` prefixes) only — C stdlib, POSIX, Tuxedo/ATMI, and FML buffer calls are dropped constructs and never score. Code inside standard C comments is ignored; banner-delimited version regions ("Ver 1.5 added here for IBM" …) are live code and count.

### CSV schema

Four layers — a marks comment line, a marks cells row, the header, then one row per file:

```text
# tuxgo marks: query=1 simple=5 complex=10 tpcall=20 branch=1
,,1,1,,,20,,,,,,,,,,,,            ← marks cells row: each scored column's mark sits over that column
file, num_lines, num_queries, branching_factor, branch_count, has_tpcall,
tpcall_count, fn_local_count, fn_external_count, external_fns,
external_weight, select_count, insert_count, update_count, delete_count,
merge_count, complexity_score, complexity, reasons
```

The `external_fns` cell names every external call as `name:class:weight` (semicolon-separated), so each row is self-contained: `score = num_queries*query + branching_factor*branch + external_weight + tpcall_count*tpcall`. The query-type counts sum to `num_queries` (every unit counts, dedup-inclusive; SELECT covers direct reads and flattened cursors alike). `num_lines` is the file's raw line count (a triage context fact — it does not score). `complexity_score` and `complexity` are **spreadsheet formulas** — `=C$2*C4+D$2*D4+G$2*G4+K4` over the marks cells row (absolute `$2` references) and the row's own factor cells, and an `IF` ladder over the score cell for the LOW/MEDIUM/HIGH tier — so a spreadsheet recalculates the moment you edit a marks cell or a weight. Programmatic consumers must recompute the score from the row (the formula string is not a number); the tool itself never reads those cells back. Tools consuming the table can skip the marks line (`csv.Reader.Comment = '#'` in Go, `comment='#'` in pandas; Excel shows it as an extra first row).

### Re-scoring from the CSV

The CSV is the single tuning surface — both the rubric marks and the per-fn weights are set there, and no scoring change ever requires a code change:

```sh
# 1. rubric marks: edit the marks cells row (CSV line 2 — the cell under
#    each factor column); the cells win over the line-1 comment. Here the
#    num_queries mark (column 3) becomes 2.
awk -F, 'NR==2 {$3=2} {print}' OFS=, report.csv > edited.csv
# 2. per-fn weights: edit the external_fns cell (e.g. drop the session check)
sed -e 's/chk_session:complex:10/chk_session:complex:0/' report.csv > edited.csv
# 3. re-run
go run ./cmd/tuxgo analyze path/to/pc-files -weights edited.csv
```

Precedence when scoring an external fn: **per-fn weight > tier mark > conversion-name fallback**. Precedence for the rubric marks: **marks cells row > `# tuxgo marks:` comment** — the cells under `num_queries`, `branching_factor`, and `tpcall_count` are the editing surface for those marks (`simple`/`complex` still live on the comment line, where they tier the external fns). Because a generated CSV pins an explicit weight for every fn it saw, editing `simple=`/`complex=` only affects fns whose weight you cleared (`fn_x:simple:` — falls back to the tier mark). Typos in either the comment line or a marks cell are errors, never silent defaults. Counts stay factual (`fn_external_count` still lists the call); only weights change. In the nav fixture, dropping the session check moves the score 174 → 164 (still HIGH).

One mental model point: the score/tier cells are spreadsheet formulas over the editable inputs (marks cells row + per-fn weight cells) — edit an input in your spreadsheet and the row recalculates live; edit it in the file and re-run with `-weights` and the tool recomputes from the same inputs. The formulas exist for humans and spreadsheets; the tool is still the calculation engine of record and never parses them back. Why CSV and not xlsx: it stays diffable, tool-able with the stdlib, and dependency-free — an xlsx writer would add a third-party dependency for no scoring benefit (R4).

## `extract` — deterministic IR extraction + marking

Parses `.pc`/`.pcf` files into the conversion IR — 100% deterministic tool work, zero LLM calls (§4: the LLM never discovers structure):

- **query units** — every live `EXEC SQL` block classified and **marked**
  with a `QueryType` (SELECT_SINGLE / SELECT_MULTI / INSERT / UPDATE / DELETE / MERGE) and its generation template ID; cursor groups flatten to one multi-row unit (extent = DECLARE → last CLOSE, row shape = FETCH-INTO list); descriptors carry tables, binds + arity, order-by; identical SQL across branches links via `dedup_key`/`duplicate_of` (the plan level collapses to one DB method)
- **condition inventory** — the entry function's (`SVC_*`) multi-branch
  top-level if/elseif/else chains: expression, flag vars, source range, FML ops (`Fget32` = inputs, `Fadd32` = outputs, FNOTPRES ⇒ optional, session/ error fields marked dropped), embedded query refs — *inventory only*; the user's mapping decides endpoint promotion (Phase 5). Each condition also carries a parsed **predicate tree** (`||`/`&&`/`!`/comparisons/calls — C boolean grammar, anything else degrades to `Raw`) alongside the raw text
- **comment inventory** — every comment span (block / line / banner) is a
  recorded fact with line/col extents (`InComment` answers "is this position code?"); banner comments (`Ver X.Y added here` … `Ver X.Y comment ends`) delimit *live* regions and never hide code; unterminated comments or braces are loud `unbalanced` facts carried on the IR (`unbalanced` in extract output/JSON) and surfaced as WARNs by convert; mapping against a truncated file errors naming the unbalanced region, never a bare count. (A missing semicolon after EXEC SQL is *not* a fact — the scanner closes the block at the next `;` leniently; documented residual of the severity analysis.)
- **buffer roles** — FML buffer variables are recorded with their convention
  role: `Ibuffer` → input, `Obuffer` → output, `Sbuffer`/`Rbuffer` → send/ recv (matching is case-insensitive on the name or its last `_`-segment, so `ptr_fml_Ibuffer` resolves); the registry is extensible via `buffers.roles` in `.tuxgo.yaml`, and unknown names surface as a visible `unknown-role` fact; every FML op carries the buffer it targeted
- **tpcall sites** — each `tpcall` call site correlates into a `TPCall`
  fact: the outbound service name plus its FML contract — `Fadd32` ops into the send buffer before the call, `Fget32` ops from the receive buffer after (the enclosing block is the window); sites whose buffers can't be correlated degrade to a visible `ambiguous` fact
- **host vars** — typed from scanned declarations, header vars flagged
  `from_header`
- **external fns** — called-but-undefined `fn_*`/`chk_*`; dir mode resolves
  them corpus-wide (`fn_is_demo_active` → `fn_demo_lib.pc`) with QueryID cross-references; unresolved ones are flagged in the IR — `convert` turns them into panicking stubs (see `plan` below)

**Fragment mode:** a file holding a lone code block (typically one branch of an entry, in `.pc` or `.txt`) is detected automatically — no `SVC_*` entry and no function definitions — and converted under the same rubric: the fragment wraps as the pseudo-function `__fragment`, the top-level if-chain builds the condition inventory (a single-branch chain is enough; a block with no chain at all becomes one endpoint covering the whole fragment), and queries/FML ops extract identically. Use `-fragment` only to force the mode when detection is ambiguous. Helper files with `fn_*` definitions keep their full-file semantics.

```sh
go run ./cmd/tuxgo extract testdata/nav/SVC_DEMO_LIST.pc   # IR JSON to stdout
go run ./cmd/tuxgo extract testdata/nav -out ir.json          # single file → path
go run ./cmd/tuxgo extract path/to/pc-files                   # dir → conversion_logs/state/<file>.ir.json
```

Flags: `-out <path>` (file-mode output), `-config <path>` (default `./.tuxgo.yaml` when present), `-fragment` (force fragment mode on a single-file input). Every run loads and routes the config seam (logs `profile`, `model`, `api_base`, `key_source` — never the key), logs the per-unit trace (`extracted query unit`), and archives the IR into the run's audit folder. Golden tests pin the nav fixture: 4 conditions, 7 query units, the F/I COUNT dedup link, and the external-fn resolution.

## `plan` — deterministic decomposition

Builds the conversion plan from the IR + **your** endpoint mapping — the tool never invents endpoints or their names (§4.2.8). The mapping is a small YAML file you write — a fully commented, runnable template ships at [`configs/nav.mapping.example.yaml`](configs/nav.mapping.example.yaml) (its demo values map the local `testdata/nav` fixture):

```yaml
service: nav
module: mutual-fund-be/pkg/services/nav
readDBs: [EBATEST, MF]          # wiring args for the store constructor
routeGroup: /nav
endpoints:                      # condition index → endpoint name + route
  - { condition: 1, name: NavHistory, route: /mfnavhistory }
  - { condition: 2, name: SipFreedem, route: /mf_sipfreedem_schemes }
  - { condition: 3, name: NavList,    route: /mfnavschemelist }
dbMethods:                      # optional pins: reference-quality DB signatures
  cur_mf_nav_list: { name: GetNavDetails, params: [compCd:string] }
  q3: { name: GetCount, params: [matchAccount:string] }
  fn_is_demo_active:q1: { name: IsDemoActive }   # external fn → its own DB method
```

Unpinned names get deterministic fallbacks (cursor/table-derived). The plan collapses duplicate queries to one DB method, records `chk_*` as dropped constructs, and converts unresolved external fns as visible **panicking stubs** (`controller/fnstubs.go` — the calling endpoints generate against the stub and carry on; the stub's doc comment and `panic` mark every call site until the fn is implemented, never silent). Every tpcall site inside a mapped endpoint becomes its own **`tpcall` plan unit** carrying the site's full send/recv FML contract; sites outside every mapped condition are recorded skips. A site whose buffers are identified but whose block carries **zero FML ops** is flagged `ambiguous` so the placeholder states the empty contract instead of understating the gap.

```sh
go run ./cmd/tuxgo plan testdata/nav -mapping configs/nav.mapping.example.yaml
# → conversion_logs/ledger/nav.plan.json (+ .md twin + audit copy)
```

Same inputs → byte-identical plan (golden-tested). Conditions not mapped are left unconverted — their exclusive queries are recorded as skipped.

## `convert` — execute the plan

Generates the target code unit by unit. Models, DB methods, interfaces, handler glue and the router render **deterministically** (zero LLM calls); each mapped endpoint's controller body is the one LLM gap, filled from the **query-replaced branch view** + DB signatures — the prompt never contains raw SQL. The controller prompt pins the fixed method signature (named returns `data`/`err`), verbatim request/response/row structs, and exact store arities; START/END logging is template-owned, so bodies never duplicate it. Import headers are best-effort — unused/missing imports are left to the IDE's save-time import fixes (user directive, 2026-09-07). DB units render through the `concurrency.workers` pool (bytes identical to a workers=1 run, race-tested); interface appends stay serial. Dir mode with several entry files fans out one worker per service (same byte-identity guarantee). Every unit is validated (Tier A: parse + gofmt) with bounded retries, recorded in the ledger (resumable), and audited.

**DML services** (INSERT/UPDATE/DELETE in mapped endpoints) render through the tx-variant templates — `tx *sqlx.Tx` second parameter per the decision-27 convention, `ExecContext` + `RowsAffected` bodies, sqlx import derived per method kind; MERGE keeps the plain `db_method_merge` contract. Fn-file query indexing is collision-guarded: raw query IDs belong to the main file alone — referenced fn files namespace as `fn:<name>:<id>` (the pin namespace), every other `.pc` in a dir-mode target is scoped under its own base name, and two definitions sharing one ID with different SQL hard-error naming both files (never a walk-order silent overwrite).

```sh
go run ./cmd/tuxgo convert testdata/nav -mapping configs/nav.mapping.example.yaml
```

**No mapping? Draft, then stop.** With no `-mapping`, no `convert.mapping`, and no `mappings/` drafts, convert runs the endpoint-discovery scan (see the `discover` section below): editable mapping drafts land in `mappings/` — AI-named when the model is reachable, deterministic names otherwise — and the run exits **before generating anything**, so reviewing them and re-running the same command is free (no tree cleanup, no half-converted state). On the re-run the mapping resolves from the `mappings/` convention (a directory of per-service drafts, or a single-entry pick by `source:`/file-name match). Explicit `-mapping` still wins and stays strict.

**Multi-service directories fan out one worker per service.** When the target directory holds several Tuxedo entry files (`SVC_*` functions), `convert` partitions it into services and converts each end-to-end on its own goroutine, bounded by `concurrency.workers`. Each service writes into its own output subtree (`baseRoot/<service>`) with its own ledger, so runs stay isolated and resumable; the staged-collision guard and Tier B scope per service. `-mapping` then points at a **mapping directory** — one yaml per service declaring `source: <entry .pc file>`. The default `mappings/` convention is **lenient**: drafts for other targets (or old-format files with no `source:`) are skipped, and full validation is deferred to the draft that actually matches an entry — so a foreign or half-filled draft never poisons an unrelated run, while a matching-but-untagged draft surfaces as an error naming its file (fill its `name:`/`route:` fields, or delete it and re-run to regenerate). An explicit `-mapping <dir>` is strict: every yaml must load and orphan sources are errors; an entry without any mapping is always a hard error (endpoints are user data). When `-mapping` is absent, the `mappings/` drafts are used — entries still missing a draft get one written and the run drafts-and-stops. `convert.fileFilter` scopes the run to your entries: `fileFilter: "mf_"` converts only `SVC_MF_*.pc` — with mappings present for the excluded services, they are skipped with a warning. Results print in extraction order after all workers land (first per-service error = exit error); workers=1 output is byte-identical to sequential single-service runs. Single-entry directories and single files keep the plain behavior above (`-mapping <file>`).

**Convert Python and Go in parallel** — two invocations, one per target, share nothing: `batchpy` writes Python modules under `-out` (default `python_out/`), `convert` writes Go under the target/staged tree, and each run gets its own log/audit IDs, so they compose trivially:

```sh
go run ./cmd/tuxgo batchpy tux/ -out python_out &
go run ./cmd/tuxgo convert tux/ -mapping nav.mapping.yaml &
wait
```

With `concurrency.workers > 1` both fan out internally (LLM endpoint rate limits are the ceiling). Never run two `convert` processes against the same output base at once — ledgers and staged trees are per-run, not cross-process-locked.

Repeat runs need no CLI arguments: set `convert.input` (the `.pc`/`.pcf` target) and `convert.mapping` in `.tuxgo.yaml` and run `tuxgo convert` bare — explicit CLI arguments always override the yaml, and an untouched workspace falls back to the `mappings/` drafts. `tuxgo plan` resolves its inputs the same way.

**Deterministic-only mode:** `run.llm: false` in the yaml (or `-no-llm` for one run) is the single AI knob shared by every command. For convert it generates every deterministic artifact — models, DB methods, interfaces, handler glue, router — with zero LLM calls; pending controller bodies are marked **skipped** (never failed) in the ledger, and a later LLM-enabled run resumes exactly those. The same knob drives batchpy's service bodies, gentest's gap-fill, and discovery naming (deterministic names).

**Every LLM exchange is archived.** Each AI call — convert controller bodies, batchpy service bodies, gentest gap-fill, discovery naming — writes its full trace (unit, template, assembled prompt, raw response, gate errors, outcome) into the run's audit folder (`conversion_logs/audit/<run-id>/`): `unit-<id>-attempt<n>.json` for convert, `<seam>-<name>-attempt<n>.json` for the rest. "What did the model see and say" is answerable for every seam after the fact.

**External interactions render as placeholders:** the target Go service has no outbound-call convention today, so each `tpcall` unit compiles as a stub in `controller/tpcall_placeholders.go` — a `// tuxgo:TODO tp:<SVC> — <source>:<lines>` marker carrying the full `send:`/`recv:` FML contract and reason, a body returning zero values + `errPlaceholder`. The build never breaks (R8: no invented scaffolding), and grepping `tuxgo:TODO` enumerates every external gap. The ledger records them under a `placeholder` status (alongside `blocked`/`skipped`) and the run summary reports them as a first-class count. When an outbound convention exists, a `tpcalls:` mapping pin upgrades a placeholder to a real call — no pipeline change (documented in `docs/plan-conversion.md`).

**SQL fidelity is checked, flag-only:** after generation, every db method's embedded SQL is compared against the source Tux SQL (`internal/sqlchk`) — same columns, tables, and conditions; alias renames and bind-style changes (`:1` ↔ `:sql_x`) pass, anything else flags as a typed deviation (`columns|tables|where|set|values|order|binds`). Deviations flip the unit's ledger status to `deviated` and the run summary reports `N sql deviations`; the run never fails, the reviewer decides. Controller bodies additionally pass a **required-call gate** — every store call the branch view shows must appear in the body (a dropped sub-flow is fed back through the bounded retries, never silently accepted) — and controller/handler artifacts are checked SQL-free (a SQL keyword leak in a string literal flags as `sql-leak`); a db method with no extractable SQL literal reports `unverifiable` — never a silent pass.

**Staged trees are per-run:** a fresh run (empty ledger) over an existing staged/target tree hard-errors instead of silently mixing generations — controller/db files accumulate, so a leftover tree from a previous run must be cleared (or resumed via its ledger). `-base <dir>` overrides where output lands for one run (convert and gentest) — useful for throwaway verification runs that must not touch the staged tree or the real target.

Where output lands depends on the two-laptop constraint:

- **`paths.mainGo` set and resolvable** (the workspace laptop, where the
  target service lives) → files write into the real service tree, Tier B (`go build`/`go vet`/`go test`, plus `go run .` when `validate.run: true`) runs batched after conversion, and `mockgen` regenerates the doubles when the binary is present.
- **`paths.mainGo` empty/absent** (this laptop) → generated code stages under
  `paths.staged` (`conversion_logs/_staged/`), Tier B is skipped with a recorded reason, and the run still succeeds — never an environment failure.

Re-runs resume from the ledger: appended units are skipped (zero extra LLM calls); pending ones retry; `tuxgo plan` + `convert` over the same mapping regenerate the deterministic units byte-identically.

**Reading a convert summary line** — `maintux: 12 files written under … — units: 17 appended, 0 failed, 0 blocked, 0 skipped, 0 placeholders, 1 stubbed fns, 0 sql deviations, 7 llm calls`:

| Field | Meaning |
|---|---|
| `appended` | units generated and written (ledger status: appended/validated) |
| `failed` | units whose gate retries were exhausted (names print below; resume retries them) |
| `blocked` | always 0 today — kept for legacy ledgers |
| `skipped` | deterministic-only mode: controller bodies left for an LLM-enabled resume |
| `placeholders` | tpcall sites rendered as contract-stubs (`controller/tpcall_placeholders.go`) |
| `stubbed fns` | unresolved external fns converted as panicking stubs, with the endpoints that call them |
| `sql deviations` | db methods whose SQL drifted from the source (flag-only — the reviewer decides) |
| `llm calls` | chat calls consumed this run (every one archived in the audit folder) |

## `batchpy` — Tux batch → Python

Converts Tuxedo **batch** programs (`main()`-entry, no `SVC_*`) into Python service modules on the `core.db_router` wrapper conventions (PRD 2026-09-08): module-level `*_QUERY` SQL constants, a repository/DAL layer, and a service class whose `process_daily_batch(workers: int = 1)` is the scheduler entrypoint. The parse stack is shared with the Go target (scanner → IR, cursor flattening); the shape rubric picks the target shape by complexity:

- **simple** (cursor + keyed-UPDATE batches): cursor loops dissolve into
  `fetchall` + chunked `executemany` — pure cursor-function DAL + phase methods, fully deterministic, retention 100%.
- **stateful** (staging-table walks, accumulators): repository class with
  every query as a method + service shell; the service orchestration body is the **one LLM gap**, filled from the **CodeView** (entry body with SQL regions replaced by repo-call placeholders — the prompt never carries raw SQL). `-no-llm` leaves a `# tuxgo:TODO service body` placeholder.

Transactions are **wrapper-owned**: generated code opens `get_connection(mode=DbMode.READ|WRITE)` context managers and never calls `commit`/`rollback`. `-dml-loop batch|rowbyrow` chooses between chunked `executemany` (default) and per-row `cur.execute`; `-shape repo` forces the repository shape. Every module passes the **pychk syntax gate** (structural
+ `python3` `ast.parse` when an interpreter is on PATH) and the **SQL
fidelity gate** (same `sqlchk.Compare` projection as the Go target; Pro*C `INTO :host` lists and `: name` spacing are documented tolerance). The LLM seam consumes an **orchestration contract** — the CodeView sliced into per-block phases, each listing the repository methods that must be called under its condition — and a contract gate rejects bodies that drop a required call, feeding the omission back through bounded retries (live eval: retention 69% → 100% on the stateful reference). Each run reports **logic retention per mode** (constants matched, phases emitted, CodeView stubs represented in the service body, log-site parity).

```sh
go run ./cmd/tuxgo batchpy batchExamples/batchTux1.pc -no-llm
# → python_out/bat_mf_ti_rjct.py (+ audit artifacts + retention report)
```

Directory mode converts every `.pc` file through the full per-file pipeline (scan → flow → plan → generate → gates → write) on its own goroutine, bounded by `concurrency.workers` (default 1 = serial; workers>1 parallelizes whole-file conversion including LLM calls — endpoint rate limits are the ceiling). `batchpy.fileFilter` scopes the directory to your batches: `fileFilter: "bat_mf_"` converts only those file names (case-insensitive substring); empty (default) = every batch. Summaries always print in input order, and every job leaves `batch file started` / `batch module written` (with `duration_ms`) records in the run log. Module names come from the batch's `c_ServiceName` literal: two files declaring the same service in one run hard-error **before anything is written** (convert them individually if intentional). A **wrong-pipeline guard** skips Tuxedo service files (`SVC_*` entries) with a visible per-file skip line — never a plausible-looking empty module — and SQL-less batches WARN while still producing their (degenerate) module.

Conventions live in the `batchpy:` config section (`wrapperModule`, `routerClass`, `readMode`/`writeMode`, `loggerPrefix`, `entrypoint`, `shape`, `dmlLoop`, `chunkSize`, `outDir`); CLI flags override. The input target is yaml-configurable too — `batchpy.input` (a `.pc`/`.pcf file or directory) is used when the CLI passes no positional, mirroring `convert.input` for the Go path; both commands' output dirs are yaml-driven as well (`batchpy.outDir` for Python, `paths.staged` / `paths.mainGo` for Go).

## `gentest` — post-conversion Go test generation

The post-conversion pipeline (PRD 2026-09-09): point it at anything inside a converted service tree — a single `.go` file, a layer dir (`nav/handler`), a service dir (`nav` → db + controller + handler tests), or a services root (one worker per service) — and it scans which functions already have tests (by `Test<Fn>`/suite-method naming **or** any call-site invocation in existing test bodies), then generates the missing tests from the in-house shapes: db = testify suite + `go-sqlmock` table cases; controller = gomock store suite (`SetupTest`/`TearDownTest` mock lifecycle, guarded `gomock.Any()` EXPECTs, per-case request fields); handler = gomock controller + gin test context with Error/204/Success cases. Existing test funcs are never clobbered; mocks regenerate via the shared `mockgen` runner. Fixture values come from a `FixtureSource` seam — v0 synthesizes deterministic placeholders from struct tags; parsing real runtime logs is the last phase, on logs provided later.

```sh
go run ./cmd/tuxgo gentest examples/nav -check-only   # gap report: functions without tests
go run ./cmd/tuxgo gentest <converted service tree> [-layers db,controller,handler] [-no-llm] [-base dir]
```

Output is staged-first (`paths.staged` / `-base`) — the target service tree is never written implicitly; `-check-only` prints the full function gap report (per function: `ok` with its name/call-site evidence or `gap`) and archives it as `gentest_gap_report.json` in the run's audit folder.

**Generation is live (PRD 2026-09-09 v0.3):** one function = one unit = one table-driven test block, rendered on the `concurrency.workers` pool (results merge in input order — workers=1 is byte-identical). The deterministic templates fill **db** (sqlmock suite: query-table regex, fixture rows/expected structs from the models' `db:` tags, SQLError/Success [-NoRows] cases) and **handler** (gin-context suite: Error 500 / Failure 204 / Success 200 over the mocked controller) and **passthrough controllers**; **field-mapping controllers** are the one LLM gap — the seam sits inside the unit worker (function source + suite contract + dependency EXPECT order + assumed fixtures, never db SQL), parse/shape-gated with bounded retries, and `-no-llm` records `llm-required` notes instead. Covered functions (by the scan's two-pass detection) are never regenerated; when a layer already has test files the output is the `<svc>_gentest_test.go` twin suite so existing tests are untouched. Constructors, unexported helpers, and tx/DML db shapes are recorded skips. Output composition passes a parse gate + gofmt; when the output tree is a module, a degrade-safe `go vet` compile gate runs per package (recorded, never fatal); mocks regenerate via the shared runner. Fixture values come from the `FixtureSource` seam (v0 = deterministic placeholders from struct tags; the runtime-log backend lands last, on logs provided later).

```sh
go run ./cmd/tuxgo gentest <converted service tree> -check-only        # gap report only
go run ./cmd/tuxgo gentest <converted service tree> -no-llm -base out/ # deterministic test generation
```

Goldens pin the `-no-llm workers=1` output for the synthetic demo service (`testdata/gentest/` → `expected/`, all three layers + the gap report; the fixture tree is local-only — gitignored and purged from history, no example bytes in the repo); regenerate only with a deliberate template/scan change (`GT_UPDATE_GOLDENS=1`, or the gentest command documented above the golden test).

## `flow` — core-logic flow IR + parser accuracy *(PRD 2026-09-10)*

`tuxgo flow <file|dir>` builds the statement-level flow tree (PRD-2026-09-10) from the scanner facts — branches, loops (for/while/do), SQL spans, returns, C declarations, and residual statements nested by block extents — and reports, per function:

- **coverage**: how many of the function's live code lines the deterministic parse classified vs residue (with the residue line numbers — the parser-accuracy instrument);
- **idiom hints**: fetch-then-iterate (cursor `while(1)`+FETCH → one multi-row store call + `range` over rows), request-guard (Fget32 + error add + return → collapses into the request struct), response-fanout (per-row Fadd32 → response struct mapping), err-op-check-loop (droppable), debug-only-if, occurrence-decode, do-while;
- **`-go` draft**: the deterministic transpilation draft — guards collapse, fetch loops become `rows, err := <store call>` + `for _, row := range rows`, Tuxedo buffer management/logging elides with a count, and everything untranspilable becomes an explicit `// TODO(line N):` marker (a correct skeleton, never a guess);
- **`-out` JSON**: the machine twin (trees + coverage + hints).

```sh
go run ./cmd/tuxgo flow testdata/nav/SVC_DEMO_LIST.pc        # coverage + hints
go run ./cmd/tuxgo flow tuxExamples/mainTux.pc -go           # + transpilation draft
go run ./cmd/tuxgo flow <dir> -out flow.json                 # machine twin
```

Fault tolerance is contractual: malformed input (unbalanced braces, parenless headers, unterminated comments, garbage bytes) degrades to loud residue — never a panic, never silently dropped constructs. `convert` reuses the same trees: with `convert.flowDraft` (default on) the controller prompt carries the endpoint's flow draft as a verified base the LLM enhances — the REQUIRED-CALLS gate is unchanged and `-no-llm` runs stay byte-identical.

## `discover` — endpoint scan-then-tag *(PRD 2026-09-10)*

`convert` is the day-to-day entry: **with no mapping anywhere it runs this engine itself** — scans the target, writes the drafts, and stops (draft, then re-run). `tuxgo discover <file|dir>` stays for drafting ahead of time (same scan, same drafts, plus `-out`/`-stdout`).

The scan keeps the human decision (§4.2.8 — the tool never invents endpoints): it finds the **API candidates** — conditions/blocks enclosing `Fget32` request reads **and** non-error `Fadd32` response writes (error emissions — `FML_ERR_MSG` into the input buffer, "fadd err = returning error" — never count; an if enclosing only error emission is a guard, not an API), including qualifying nested ifs (keyed `c<n>.<k>`; subsets of their parent are marked redundant), and emits an **editable mapping draft** per entry: endpoints with pre-filled `name`/`route` (always editable), per-candidate read/write/query census comments, non-candidates kept commented-out for control, `service` prefilled, and a `dbMethods:` block (AI-proposed pins, or the commented skeleton listing the entry's query IDs). `module:` defaults to the service name at load, so a draft is **loadable as-is** — reviewing the suggested names is the only step, and even that is optional.

**Naming** — one knob, shared with every command (`run.llm` in the yaml, `-no-llm` per run): when the model is reachable, one LLM call per candidate (branch source + its query SQL; params stay deterministic from the IR binds) proposes the endpoint name/route and db-method/row pins, marked `# ai-suggested — edit freely`; otherwise names derive from the strongest semantic token source (cursor name `cur_mf_nav_hist` → `GetMfNavHist`; else the first response field → `GetMfNavDate`; else `Endpoint<n>`), marked `# deterministic — edit freely`. Unknown query ids and non-identifier proposals are dropped; per-candidate failures fall back to deterministic names. Every naming attempt (prompt + raw response) lands in the run's audit folder (`discover-c<k>-attempt<n>.json`).

Drafts land as `mappings/<entry>.mapping.yaml` (`-out` overrides, `-stdout` prints); a re-run never overwrites an existing draft — the fresh one lands alongside as `<entry> (1).mapping.yaml`. The bare command falls back to `convert.input`. Every command's wall-clock duration is recorded in the run log (`run completed … duration_ms`).

```sh
go run ./cmd/tuxgo convert tuxExamples/mainTux.pc          # no mapping → drafts + stop; re-run to convert
go run ./cmd/tuxgo discover tuxExamples/mainTux.pc         # same drafts, ahead of time
go run ./cmd/tuxgo discover tuxExamples/mainTux.pc -stdout # print instead of writing
go run ./cmd/tuxgo discover <dir>                          # one draft per entry in mappings/
go run ./cmd/tuxgo discover                                # uses convert.input from .tuxgo.yaml
```

## Glossary

The recurring vocabulary across commands, summaries, and logs:

- **corpus** — the set of real `.pc` sources a run can see (e.g. `tuxExamples/`, `stuff-i-want-checked/`, `tux/`). "Resolved corpus-wide" = external fns are looked up across **every file in the target directory**, which is why a dir-mode convert resolves helpers a single-file run cannot. Corpus homes are gitignored, local-only.
- **IR (intermediate representation)** — the deterministic, LLM-free parse of a `.pc` file: query units, conditions, FML ops, external fns. Everything downstream (plan/convert/batchpy) reads it; `tuxgo extract` writes it.
- **golden** — a byte-pinned expected output guarded by a test (`testdata/batch/expected/`, `testdata/gentest/expected/`, the analyzer score pins). A refactor must reproduce goldens byte-for-byte or the test fails — the proof that consolidation changed nothing. Regenerate only with a deliberate rubric/template change.
- **draft** — an editable mapping YAML written by convert's no-mapping fallback / `discover` (`mappings/<entry>.mapping.yaml`), with names/routes pre-filled (AI-suggested or deterministic). Review (or don't) and re-run — the tool proposes, your re-run decides.
- **ledger** — the per-service unit-status database (`conversion_logs/ledger/`) that makes runs resumable: appended units skip on re-runs, pending ones retry.
- **seam** — a single well-defined extension point shared by commands (`llm.RunSeam` owns every retry/budget/gate/audit skeleton; `profile` owns target-shape conventions).
- **Tier A / Tier B** — Tier A (parse + gofmt) always runs where you are; Tier B (build/vet/test/smoke) runs only where the target service lives (`paths.mainGo` set). On a laptop without the target, Tier B is skipped with a recorded reason — compile-class residuals in LLM bodies surface there, by design.
- **placeholder** — a tpcall contract-stub: the build works, the FML contract is in the comment, an outbound convention upgrades it later.
- **stub** — a panicking package-level fn for an unresolved external fn (`controller/fnstubs.go`): the endpoint's logic generates against it and carries on; the `panic` makes every call loud until the fn is implemented.
- **kitchen fixture** — `testdata/stripped/SVC_MIN_KITCHEN.pc`, the kitchen-sink regression target: one file with a bit of every construct (cursors, INSERTs, tpcall sites) so one run exercises every path.
- **fn pool** — the full set of files ingested for a dir target; helper libs never need to match filters or mappings, they only feed fn resolution.

## Working-directory layout

The agreed runtime layout (where the built binary lives):

```text
<workdir>/
├── tuxgo                       # the built binary (go build -o tuxgo ./cmd/tuxgo)
├── .tuxgo.yaml                 # run config — copy of configs/.tuxgo.example.yaml, edited
├── mappings/                   # mapping drafts (gitignored) — written by convert's
                                 # no-mapping fallback / discover; review names, then
                                 # just re-run convert (the convention resolves automatically)
├── tux/                        # the .pc corpus to convert (default input dir)
└── conversion_logs/            # everything the tool writes (gitignored)
    ├── logs/
    │   ├── run-<DDMMYYYY_HHMMSS>.jsonl  # machine copy (jq/script friendly, full timestamps)
    │   └── run-<DDMMYYYY_HHMMSS>.log    # human-readable twin (clock-time lines)
    └── audit/
        └── <DDMMYYYY_HHMMSS>/
            ├── triage_report.csv           # archived copy of every analyze run
            ├── unit-<id>-attempt<N>.json   # per-unit LLM attempt records (convert)
            ├── controller-<fn>-attempt<N>.json  # gentest gap-fill exchanges
            ├── batchpy-<module>-attempt<N>.json # batchpy service-body exchanges
            ├── discover-c<k>-attempt<N>.json    # AI naming exchanges
            ├── <service>_ledger.json       # ledger snapshot (convert)
            ├── sql-fidelity.json           # per-method SQL fidelity results (PF-6)
            └── sql-free.json               # SQL-free artifact leak results (PF-6)
```

`state/` (IR cache) is live under `conversion_logs/` (written by `extract` dir mode); `ledger/` (unit status + conversion map) joins in Phase 5. `tux/`, `conversion_logs/`, the root binary, and your local `.tuxgo.yaml` are gitignored.

### Configuration (`.tuxgo.yaml`)

The loader ships (Phase 1) — copy `configs/.tuxgo.example.yaml` to the working-directory root and edit; every command loads it when present and falls back to defaults otherwise. Unknown keys are errors. Schema:

- `run` — profile selection, pinned temperature, token budgets (16k window,
  12k prompt / 4k output) and the `charsPerToken` estimator ratio (default 4, tunable per model without a code change); `run.llm: false` (or `-no-llm` on any command) is THE AI knob — deterministic-only everywhere: convert skips controller bodies (resumable later), batchpy/gentest degrade to placeholders/notes, discovery naming goes deterministic
- `models[]` — OpenAI-compatible profiles on one client seam: `isec-vllm`
  (production, on-prem endpoint) + `local-dev-openrouter` (local dev, external API); keys resolve via `apiKeyEnv` (env first) with a gitignored-file `apiKey` literal as the local-dev fallback — never commit a literal key (R6). `tuxgo` routes to the profile named by `run.profile` (or an explicit override) and logs the routing decision per run. Egress note: live LLM calls send **derived** content only (query-replaced views, struct definitions — never raw SQL, never raw source files); for zero external egress of derived logic too, route `run.profile: isec-vllm` (on-prem) or run `-no-llm`
- `retrieval` — disabled in MVP (V2 BM25 / V3 vector plug in behind the seam)
- `elision`, `concurrency`, `validate` — safe-mode elision, opt-in worker
  count, bounded validation retries; `validate.compile` (`auto|always|never`) and `validate.run` gate the Tier-B compile/vet/test/smoke checks
- `db` — generated DB-layer shape: `withGorm: false` (default) is the plain
  sqlx-only store (`NewXStore(db *sqlx.DB)`); `true` renders the nav-example variant where the store also carries the legacy `*gorm.DB` handle
- `convert` — default `input` (`.pc`/`.pcf` target) and `mapping` (endpoint
  YAML or mapping directory) so `tuxgo plan`/`tuxgo convert` run bare; CLI flags override, and with both empty convert falls back to the `mappings/` drafts — none present, it drafts and stops (see `discover`). `fileFilter` scopes **directory** targets to entries whose file name contains the substring (case-insensitive) — e.g. `fileFilter: "mf_"` converts only your `SVC_MF_*.pc` services; empty (default) = every entry. Helper/fn libs never need to match (the full directory set stays in the fn-resolution pool), an explicitly passed file bypasses the filter, and a dir-mode mapping for a filter-excluded entry is skipped with a warning instead of the orphan error. `flowDraft` (default on) feeds the PRD-2026-09-10 deterministic flow-tree draft into the controller prompt for the LLM to enhance — set it `false` for prompt A/B runs; the REQUIRED-CALLS gate is unchanged either way
- `batchpy` — batch→Python conventions (PRD 2026-09-08): `wrapperModule`
  (`core.db_router`), `routerClass`, `readMode`/`writeMode`, `loggerPrefix` (`app.`), `entrypoint` (`process_daily_batch`), `shape` (`auto|repo`), `dmlLoop` (`batch|rowbyrow`), `chunkSize`, `outDir` (`python_out`), `fileFilter` (same name-substring scoping as `convert.fileFilter`, for batch files); CLI flags override
- `paths` — `target` (the existing Go service), `mainGo` (the Tier-B anchor,
  enables in-place generation when set), `ledger`/`state`/`staged` (the `conversion_logs/` sub-roots); `paths.staged` receives generated code when the target service does not exist. Logs and audit are **not** configurable — `conversion_logs/logs/` and `conversion_logs/audit/` are fixed conventions (move logs with the global `-log-dir` flag)

## Development

```sh
go build ./...        # build
go test ./...         # unit + golden-fixture tests
go vet ./...          # vet
gofmt -l .            # formatting check (fix with gofmt -w)
```

`internal/archtest` additionally machine-checks the layering law (dependency direction cmd → orchestration → pipelines → parse stack → kernel; forbidden arrows fail the test run).

Fixtures live in `testdata/nav/` (`SVC_DEMO_LIST.pc`, `fn_demo_lib.pc`) and `testdata/merge/` (`SVC_DEMO_MERGE.pc` — MERGE rubric + nesting math); the converted reference targets live in `examples/nav/`. Golden tests pin the fixture scores (nav 174 = 7 queries + 25 external + 142 branching factor; fn_demo_lib 2 = 1 query + 1 branch) and the merge fixture's 3 branch headers / factor 4 / score 5.

## Layout

```text
cmd/tuxgo/            CLI (analyze, extract, plan, convert, batchpy, gentest, flow, discover)
internal/
  audit/              per-run audit trail recorder (conversion_logs/audit/<run-id>/)
  budget/             token ceilings (run.* yaml keys) + query-replacement view —
                      SQL regions → one resolved DB call line (Phase 4)
  config/             .tuxgo.yaml schema, loader, validation, profile routing (Phase 1)
  convert/            conversion orchestrator — ledger-resumable plan execution,
                      LLM controller bodies via the query-replaced view (Phase 5),
                      flow-draft AI-enhance seam (PRD 2026-09-10)
  cproc/analyzer/     complexity scoring + CSV export + weights re-scoring
  cproc/flow/         statement-level flow tree + coverage metric + idiom hints +
                      deterministic Go transpilation renderer (PRD 2026-09-10)
  cproc/ir/           deterministic IR extraction + QueryType/template marking,
                      condition inventory, external-fn resolution (Phase 2)
  cproc/scanner/      Pro*C statement-level scanner (EXEC SQL, calls, defs, directives,
                      branches, var decls, fn bodies)
  gen/                deterministic generation core — models, DB methods, interfaces,
                      handler glue, router (zero LLM) (Phase 5)
  goast/              go/ast interface accumulation (offset-splice append, signature
                      inspection, import merge) (Phase 3)
  ledger/             resumable unit status + source→target conversion map (Phase 5)
  llm/                OpenAI-compatible client (chat + SSE stream, retries, token
                      accounting) + httptest fake (Phase 1)
  plan/               decomposition plan builder + user endpoint mapping (Phase 5)
  templates/          versioned, embedded code-shape templates (model/db/controller/handler)
  telemetry/          unified slog logging (console + JSONL, run-id correlation)
  validate/           two-tier validator — syntax always; build/vet/test anchored at
                      paths.mainGo when the target service exists (Phase 5)
testdata/nav/,          .pc fixtures (nav golden + merge rubric) — local-only,
testdata/merge/         gitignored + purged from history (no example bytes in the repo)
examples/             golden reference conversion (nav service) — local-only
docs/                 architecture, pipeline design, conversion-phase plan,
                      dependency justifications — local-only
prd/                  product requirements (source of truth + roadmap) — local-only
configs/              example yamls: the run config schema and a commented
                      endpoint-mapping input for plan/convert (shipped)
```

## Roadmap

- [x] Phase 0c — analyzer: detection, two-tier scoring, CSV + re-scoring
- [x] Templates package — versioned `v1` code-shape set behind a `Provider` seam
- [x] Unified logging + audit trail seed
- [x] Phase 1 — config (YAML multi-profile) + OpenAI-compatible LLM client + audit per-unit traces *(live OpenRouter gate verified; isec on-net check pending)*
- [x] Phase 2 — Pro*C IR extraction (models, query units/descriptors, cursor flattening, endpoint inventory, FML ops) *(post-functioning pass 2026-09-07: comment inventory, predicate trees, fragment mode, buffer roles, correlated tpcall sites + placeholders; SQL fidelity gate flag-only in convert; TPBEGIN/TPCOMMIT boundaries, mtime re-index remain)*
- [x] Phase 3 — Go AST engine: interface accumulation (`internal/goast` — offset-splice appends preserving formatting/comments, structural idempotency + `ErrSignatureConflict`, `InspectInterface`, gofmt-grouped `AddImports`); body edits stay text-append+gofmt in Phase 5 `gen`
- [x] Phase 4 — token budgeter + query-replacement view (`internal/budget` — ceilings from `run.maxPromptTokens/maxOutputTokens/charsPerToken` yaml keys, typed over-budget errors; `ReplaceQueries` rewrites every SQL region to one resolved call line, missing call = error; nav gate: branch view −80.5%, SQL −98.5%)
- [x] Phase 5 — conversion orchestrator: `tuxgo plan` (user endpoint mapping → deterministic units, orphan/blocker check) + `tuxgo convert` (models/db/interfaces/handler/router with zero LLM, controller bodies via the LLM seam over the query-replaced view, ledger resume, per-unit audit, two-tier validation). Live Tier-B compile + API runs happen on the workspace laptop via `paths.mainGo` *(remaining: TPBEGIN/TPCOMMIT crux-flow tx wrapping, eval diff vs `examples/nav/` on the target laptop)*
- [x] batchpy — Tux batch → Python (`tuxgo batchpy`: shape rubric, repository/DAL layering, orchestration-contract LLM seam, pychk + fidelity gates, retention report, per-file worker fan-out)
- [ ] Phase 6 — test generation (PRD-2026-09-09: `tuxgo gentest` — scan, templates, per-function generation engine, in-worker LLM seam + gates, target matrix/staging, byte-pinned goldens: all landed with a full in-repo E2E; runtime-log FixtureSource pending on user logs) + Phase 7/8 — eval harness, Q&A CLI
- [x] convert dir fan-out — one worker per service file end-to-end (`tuxgo convert <dir>` with several Tuxedo entry files: per-service ledgers, per-service output subtrees `base/<service>`, mapping directory with `source:` entries; `concurrency.workers` bounds the pool, workers=1 byte-identical, race-tested)
- [x] UX consolidation (2026-09-10) — convert is the front door: no mapping → discovery drafts land in `mappings/` and the run stops (re-run converts, `mappings/` convention resolves automatically); drafts load with zero edits (`module` defaults to the service, dead `routeGroup` dropped); the AI knob is one (`run.llm` + `-no-llm` everywhere — `discover.mode`/`-mode`/`-ai` deleted); every LLM seam archives its full prompt+response trace (`audit.Exchange`); shared `resolveLLMClient`/`runIndexed` seams
- [ ] TBD (parked) — dir-mode fan-out for `analyze`/`extract`; take up when corpus scale demands it

See `prd/PRD-2026-09-06.md` (§6 work plan) and `docs/architecture.md` — both local to the working repo, not shipped.
