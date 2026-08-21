# Memory Optimization Pass — gotpl-analyzer

Baseline: commit `b87130c` ("Optimize memory usage"). All numbers measured on this
machine (i7-10510U, 8 threads, Go 1.27) with `go test -benchmem -benchtime=3x`
against the real project `/home/nabiizy/Code/go/eclinichmsgo` (412 render calls,
269 output render calls, 70 funcMaps, 97 named blocks) and the synthetic
30-template workspace from `daemon_benchmark_test.go`.

---

## 0. Regression repaired first (prerequisite)

The baseline commit had broken two of its own tests
(`TestExternalMethodDocs`, `TestHoverDocForExternalMethod`) and silently dropped
data: `extractMethodFields` skipped **all** stdlib types and the method-doc
fallback (`funcDocAt`) was removed, so methods of external types
(`time.Time.Format`, …) vanished from field trees and hover.

**Fix** (`ast/field_extractor.go`): restored external method extraction with doc
resolution via a new `resolveExternalMethodDocs` pass that runs *after*
`addMethodDocs`, preserving structIndex precedence for project types (raw doc
text with trailing newline) and using `funcDocAt` only for types absent from the
project index. Verified against a build of the last correct commit `c458348`.

A second silent regression in the same commit — removing recursive return-type
expansion in `extractSignatureInfoWithFields` / `extractSignatureFromType` —
was found by diffing full CLI JSON output against the `c458348` reference:
**36 registry types** reachable only through method returns
(`labreports.*`, `cbc.*`, `enums.*`, `http.Header`, `url.Values`, …) were missing,
producing dead ends for consumers resolving `.X.Method().Field` chains through
`Types`. Restored the bounded expansion (MaxFieldDepth + seen-maps + fieldCache).
Post-fix registry diff vs reference: 0 missing, +3 stdlib additions
(`time.Location/Month/Weekday`) from restored external-method extraction.
`renderCalls` and `validationErrors` are set-identical to the reference.

---

## 1. Findings and fixes (ordered by measured impact)

### F1. Daemon analyze response carried inline field trees — 86% payload bloat
- **Where**: `daemon.go` `buildSnapshotFromResult`, merge of propagated vars into
  `outputRenderCalls`.
- **Why it hurts**: propagated variables arrive with full inline field trees and
  were merged *after* `Flatten()`. Measured 770 fields across 6 vars on the tiny
  synthetic workspace; response 303,541 B → 42,149 B (**-86%**) after stripping.
  JSON encoding of the old payload dominated daemon-cycle allocations (~2.2 GB
  per cycle on the real project, `encoding/json/v2.makeStructArshaler` +
  `reflect.mapassign_faststr0` in `-alloc_space` profile).
- **Fix**: `stripInlineTrees` on the output copies only; the snapshot's
  `renderVarsByTemplate` keeps trees for hover/validation. This *aligns* the
  response with the documented contract ("Variable field trees are stripped;
  consumers resolve types via Types").
- **Risk**: low-medium (output shape change toward documented contract; extension
  already resolves via `Types`). Verified: renderCalls/validationErrors
  set-identical to reference CLI output; all tests pass.

### F2. Template store loaded twice per daemon analyze
- **Where**: `validator.ValidateTemplates` loads `TemplateStore` internally;
  `buildSnapshotFromResult` loaded it again for `BuildPropagatedRenderVarIndex`.
- **Why it hurts**: full disk walk + transiently two copies of every template's
  content per cycle.
- **Fix**: additive exported `ValidateTemplatesWithStore(..., store)`; existing
  `ValidateTemplates` delegates to it. Daemon loads once, shares.
- **Risk**: low (pure addition; CLI path unchanged).

### F3. Defensive deep clones per expression-inference call
- **Where**: `validator/expression_infer.go` `InferExpressionType`,
  `ValidateFunctionCallArgTypes` cloned `vars`, `scopeStack` (+ every frame's
  `Locals`), `blockLocals`, `funcMaps`, `typeRegistry` on **every call**.
- **Measured**: `maps.clone` = 45.55 MB (6.1%) + 222K objects (4.3%) of cold-run
  allocations; 80% `map[string]FuncMapInfo` driven by
  `ValidateFunctionCallArgTypes`.
- **Fix**: removed clones; audited every access — all five fields are read-only
  inside the inferencer; concurrent callers only read shared maps, so sharing is
  race-free (confirmed by `-race`). Read-only contract documented on both
  exported functions. Helpers `cloneTemplateVarMap`/`cloneScopeStack` deleted.
- **Risk**: low.

### F4. Expression parse-tree cache (stub FuncMap rebuilt per action)
- **Where**: `parseExpressionTree` built a fresh `template.FuncMap` of stub
  closures + re-parsed identical expressions per call: 44.2 MB in
  `text/template.addFuncs/addValueFuncs` (6%) plus parse-tree churn.
- **Fix**: package-level `exprTreeCache` keyed by `expr + localVarNames`
  (sorted at source in `collectLocalVarNames` — map iteration order previously
  made keys unstable). Entries store the sorted registry key set; reuse requires
  exact set match (binary search), since the parser rejects undefined functions.
  Bounded at 4096 entries with clear-on-full eviction — pure derived data, so
  eviction can never change results. Shared stateless `noopFunc` stub.
- **Risk**: medium-low (cache correctness argued above; hit-rate verified by
  profile delta).

### F5. Redundant `BuildTypeRegistry()` before `Flatten()` in the daemon
- **Where**: `buildSnapshotFromResult` step 1; `Flatten()` re-builds internally.
- **Fix**: explicit call removed; comment explains why. One full registry tree
  walk saved per analyze cycle.
- **Risk**: low (nothing between the two calls consumes `rawResult.Types`).

### F6. `strings.Fields` on hot per-action scanning paths
- **Where**: 6 sites in `validator/utils.go`, `content_validator.go`,
  `scope_at_position.go`, `scope_tracker.go` allocated a full `[]string` per
  action but consumed ≤2 words (115K objects in cold profile).
- **Fix**: allocation-free `nextWord`/`firstTwoWords` helpers with exactly
  `strings.Fields` space semantics (`unicode.IsSpace`).
- **Risk**: low (word-boundary semantics preserved deliberately).

### F7. Worker pools fanned out for tiny inputs
- **Where**: `processNodesConcurrently`, `buildStructIndex`,
  `attachMethodDocsConcurrent` (ast), `runWorkers`,
  `processTemplateFilesConcurrently` (validator) — always `max(NumCPU(),1)`
  workers regardless of input size.
- **Fix**: collapse to 1 worker below `minParallelWorkUnits`/`minParallelWorkItems`
  (= 32). No goroutine/channel/WaitGroup overhead for small runs.
- **Risk**: low.

### F8. Root scope rebuilt per expression node
- **Where**: `buildRootScope(i.vars)` called from `currentDotType` /
  `resolveVariablePath` per node though `vars` is fixed per inferencer
  (12 MB in cold profile).
- **Fix**: lazy `rootScopeFields()` memoized on the inferencer; whole
  19-method receiver chain converted to pointer receivers.
- **Risk**: low.

### F9. extFileCache lifecycle decision (measured, then reverted)
Per-run invalidation of `extFileCache` (alongside `ClearTypeCache()`) was tried
and **rejected on evidence**: it forced re-parsing stdlib sources every cycle
(+200K objects/cycle) while the cache is naturally bounded by the distinct
external files referenced by analyzed types. Left warm across cycles; retention
is small and flat (visible in the +5 MB plateau below).

### F10. Goroutine-capture audit — closed, no findings
All production `go func` sites (`runWorkers`, `attachMethodDocsConcurrent`,
`processFuncScopesConcurrently`) pass chunk data as parameters or delegate to
extracted worker functions; closures capture only channels/WaitGroups/shared
read-only maps. No large-scope captures.

### F11. Capacity-hint sweep — closed
Audited all `make([]T)` / `make(map[K]V)` sites in non-test code. Slice makes
all carry capacity; remaining unhinted maps have unknowable size upfront.
Added `len(...)` hints to the two named-block registry builders whose source
size is known (`parseAllNamedTemplatesFromStore`,
`processTemplateFilesConcurrently`). Marginal win; hygiene.

---

## 2. Final numbers

| Metric | Baseline b87130c | Final | Δ |
|---|---|---|---|
| ColdStart B/op | 270.0 MB | 220–230 MB | **−15%** |
| ColdStart allocs/op | 2.03 M | 1.76–1.93 M | −5…13% |
| WarmStart B/op | 264.6 MB | 220.6 MB | **−16.6%** |
| WarmStart allocs/op | 1.905 M | 1.761 M | −7.5% |
| DaemonAnalyze (synthetic) | 442 ms / 7.31 MB | 425 ms / 7.48 MB | ~flat* |
| Daemon TotalAlloc / real-project cycle | ~5.5 GB | ~278 MB | **−95%** |
| Daemon cycle wall time (real project) | ~45 s | ~3.1 s | **14× faster** |
| Analyze response size (synthetic) | 303.5 KB | 42.1 KB | **−86%** |
| Retained heap after cycles (real project) | 44–45 MB, flat | 49–52 MB, flat | +5 MB (bounded caches: exprTreeCache, warm extFileCache, restored external method info) |
| TestDaemonMemoryLeak | PASS, flat | PASS, flat | ✓ |

\* synthetic workspace too small to exercise payload encoding; the real-project
daemon path is where the win lands.

Correctness verification:
- `go test ./...` and `go test -race ./...` fully green (incl. the two tests the
  baseline failed).
- Full CLI JSON output vs `c458348` reference binary: `renderCalls` and
  `validationErrors` set-identical; `types` registry equal except +3 stdlib
  entries (restored external-method extraction); `namedBlocks` identical.
- Output nondeterminism observed in *both* baseline and final binaries
  (concurrent worker ordering) predates this work.

---

## 3. Identified but not fixed

1. **`mergeTypeInfo` duplicates all app-package type-info maps** (~58 MB, 8.3%
   of cold allocs): unifying `Types/Defs/Uses` into one `types.Info` copies what
   `packages.Load` already holds. Avoiding the copy needs per-file
   `*types.Info` routing through the extraction pipeline — an architectural
   change touching every extractor signature. Flagged for a dedicated pass.
2. **Stale `templateOverlays`**: overlays live until `clearTemplate` arrives; a
   tab closed without that RPC retains its buffer forever. Fixing requires an
   extension-side contract (close notification or TTL), i.e. cross-repo change.
3. **`globalPkgCache` is dead code**: declared + mutex + `InvalidateCache()` in
   `ast/analyzer.go`, never populated. No memory impact; removal would drop an
   exported symbol (contract constraint), left as-is.
4. **Streaming JSON in `main.go encodeJSON`**: `json.Encoder` already streams to
   stdout; the materialized `ValidationOutput` tree is required by validation
   ordering (flatten-after-validate). Benefit would appear only for very large
   outputs; deferred.
5. **Baseline nondeterminism** (worker arrival order in aggregated slices)
   makes byte-diff comparisons noisy; a deterministic-mode flag would help
   future regression checks but changes no memory behavior.
