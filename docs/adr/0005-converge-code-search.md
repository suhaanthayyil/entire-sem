# RFD 0005 - Converge code search on one engine, deprecate duplicates

Status: Proposed (RFC, open for comment). P2.M1, COR-786
Date: 2026-08-21

## TLDR

We maintain two independent tree-sitter extractor stacks and have no single front door for code
search. This RFD proposes **Option A**: entire-graph becomes the precision symbol **producer**,
emitting a SCIP index per `repo@commit` that peregrine ingests into its already-reserved
`SymbolSource::Scip` / `SymbolPrecision::Semantic` slots. peregrine stays the find-text-fast
**serving** engine and the single front door. entire-graph stays the understand-structure engine
(relations, impact, brain). Each retires its duplicate half: peregrine stops owning symbol
precision, entire-graph stops building a search engine.

Embedding backbone consolidation is **not** in scope. That is COR-788 and gets its own RFD.

## Context

### The honest inventory

| System | Owner | Lang | Role today | Backbone | Deployed? |
|---|---|---|---|---|---|
| **peregrine** | Evis | Rust | Lexical code search: trigram index, regex, symbol/fuzzy search, cross-file go-to-def | Trigram postings (Roaring), zstd content. No vectors | Staging only, `eks-staging-us-west-2`, ~114 repos, single replica |
| **entire-graph** | Us | Go | Structural code graph: typed symbols + 30 relation types, ranked search, impact, diff | Tree-sitter snapshot, tree-hash-keyed cache. No vectors | One-shot local binary, plus brain ingestion |
| **entire-search** | Alisha | - | Product-entity search over transcript chunks, commits, PRs. Deliberately strips code | Cohere embed-v4, 1536-dim, Turbopuffer | Yes, serves entire.io |
| **librarian** | Alex | - | Comms and docs knowledge. No code | nomic-768, pgvector | Yes |

entire-search and librarian are prior verified context. Neither is code search, neither overlaps
here, and neither is touched by this RFD.

### Duplicate 1: two tree-sitter extractor stacks

peregrine ships 15 per-language extractors plus import resolvers (`src/symbols/mod.rs:8-22`,
`:25-39`). entire-graph ships a semantic path for **36** languages (`docs/language-support.md`),
a strict superset: all 15 of peregrine's tier-1 languages appear in it. Two teams pay grammar
maintenance and upgrade churn for an overlapping set, and the smaller stack is the one users hit.

The duplication already costs coverage, not just effort. **peregrine's Kotlin support does not
work.** `src/symbols/parser.rs:15-34` wires only 14 grammars; Kotlin is absent with the comment
"tree-sitter-kotlin 0.3.x links against tree-sitter 0.20 (incompatible with 0.26)" (`:27`).
Unmapped languages return `None` (`:6-11`) and `extract_symbols` returns empty
(`extract.rs:93-96`), so `lang_kotlin.rs` is unreachable and every Kotlin file yields zero
symbols, despite the README advertising Kotlin as tier 1. entire-graph extracts Kotlin today.

Precision is the larger gap, and peregrine's source says so. It has **two** resolvers:

- **Index-time, persisted, crude.** `batch_resolve_references` (`src/symbols/resolve.rs:122-179`)
  populates `resolved_to` in every shipped index (called from `src/index/writer.rs:170`, `:416`,
  `src/index/incremental.rs:1121`). Its own comment: "ponytail: uses simple name-matching +
  same-directory preference heuristic" (`resolve.rs:118`). Repo-wide `name -> defs` map, prefer a
  same-directory candidate, else **pick the first arbitrarily** (`:174-176`). No import parsing,
  no qualifier or type awareness.
- **Query-time, not persisted, better.** `resolve_file_references` (`:185-380`) parses imports,
  filters external packages and disambiguates by qualifier, but runs one file per request behind
  `POST /api/resolve` (`src/server/http.rs:1296`), recomputed every call.

`README.md` describes only the second and claims resolution is "query-time, not index-time,"
which is misleading about what ships in the index. The honest statement: peregrine's persisted
resolution is name-based and heuristic with arbitrary tiebreaks. Its wanted upgrade is documented
as expensive at build time (`resolve.rs:119-121`), and is exactly the work entire-graph already
does elsewhere.

### Duplicate 2: entire-graph is starting to build a search engine

Branch `codex/eg-graphify-lexical-sidecar` (commit `5442ed87`, not on main, divergent) adds a real
**trigram-postings on-disk lexical index**: `internal/sem/search_lexical_index.go` (984 lines)
plus `search_source_pack.go` (455 lines), magic headers `EGLEXI1` / `EGPOST1` (`:35-36`),
artifacts `manifest.json` / `lexicon.bin` / `postings.bin` (`:26-28`). Confirmed dormant, zero
callers in the query path. That is a second trigram engine, in a second language, inside the repo
whose own boundary doc says serving is not its job. This RFD is the decision that closes it.

### Duplicate 3: no single code-search front door

peregrine is reachable through the entire-api gateway as upstream `"peregrine"`
(`entire-api/internal/server/api.go:399`), spec-merged under a `/search` prefix (`:451-457`), and
surfaced to agents as `search_entire_code` (`mcp/internal/entiremcp/curatedtools.go:619-666`).
entire-graph reaches agents by a disjoint route: a shell-out binary and brain ingestion.
`grep -rni "entire-graph"` across the MCP repo returns **zero hits**. Callers pick a door by
accident, and the two doors disagree about what a symbol is.

### The seam already exists, and was designed on purpose

peregrine reserved an external precision producer from the start: `src/symbols/types.rs:111-115`
`enum SymbolSource { TreeSitter = 0, Scip = 1 }`, `:118-122`
`enum SymbolPrecision { Syntax = 0, Semantic = 1 }`, `:137-138` both carried on every `Symbol`,
`:265-276` `deserialize` already mapping byte `1` to `Scip` and `Semantic`. Deliberately, per its
own spec: "source and precision fields exist to support future SCIP overlay without schema
changes" and "Resolution ... is explicitly out of scope for v1 (requires import analysis or SCIP)"
(`docs/superpowers/specs/2026-03-31-symbolic-search-design.md:45,48`), with `docs/design.md:27`
recording "SCIP evaluated later." Nothing constructs `SymbolSource::Scip` today. It is a reserved
slot waiting for a producer.

The producer exists and is **merged**: PR #97 (`ec/candidate-scip-export-v2`) landed on main on
2026-08-27, adding `snapshot --format scip` emitting a real `scippb.Index` via
`github.com/scip-code/scip/bindings/go/scip v0.9.0`, deterministically marshalled
(`internal/sem/compact_snapshot.go:454,462`), with unsupported relations reported in a JSON stderr
omission note. The half of step 1 that was a merge is therefore done; what remains there is
promoting the format out of experimental and freezing the note's compatibility policy.

## Options considered

### Option A: peregrine absorbs entire-graph's extraction via a SCIP feed

One extractor, two engines. entire-graph produces SCIP per `repo@commit`; peregrine ingests it
into `.pg` section 6 as `Scip` / `Semantic`, keeping its native layer as fallback. Cons:

- Adds a cross-repo build dependency and a new failure mode, a stale or absent feed. Needs an
  explicit freshness contract and fallback or quality silently regresses.
- **SCIP is a lossy projection.** Only 15 of entire-graph's 30 relation types map to SCIP
  references (`compact_snapshot.go:540-547`); the rest drop into `UnsupportedRelationCounts`
  (`:427-429`). Only `Import`, `WriteAccess`, `ReadAccess` get explicit roles (`:549-560`); CALLS,
  EXTENDS, IMPLEMENTS emit role 0. entire-graph cannot retire its native snapshot format.
- **The monikers are not portable.** The exporter hardcodes scheme `"entire-graph"`, package
  manager `"."`, package name = repo key (`:660-664`), and embeds `shortDigest(record.ID)`, an
  internal ID, in the descriptor (`:613-636`). Syntactically valid SCIP carrying
  entire-graph-specific identity, not monikers another indexer resolves to the same symbol.
- PR #97 "materializes one complete SCIP index in memory before writing it," an unvalidated scale
  risk on a monorepo. It also couples release cadences: peregrine's build waits on an
  entire-graph run.

### Option B: entire-graph becomes the search engine too

Delete peregrine, serve everything from entire-graph. Cons, close to disqualifying:

- It contradicts a boundary already ratified in this repo. `docs/brain-and-graph-boundaries.md`
  declares the MCP server surface "Brain's job" and multi-repo / cross-project graph "Brain's
  job," because "there is no equivalent identity for 'nine repositories as of roughly now'." Code
  search is inherently multi-repo fan-out.
- entire-graph is a one-shot, no-egress binary, not a serving system. Caches key on the git
  **tree** hash, "deliberately tree-only, not commit-keyed"
  (`internal/sem/search_cache.go:485-489`), monolithic, with **no incremental indexing**: a
  one-character change alters the tree hash and forces a full rebuild. Per-file invalidation is
  "unbuilt, not declined" per the boundary doc.
- It discards a working, benchmarked engine. peregrine indexes are roughly 4.9x smaller than
  Zoekt's with about 4x faster exact search (README), it does `git diff-tree` incremental indexing
  with surgical section splices (`src/index/patch.rs:1-6`), and it is already wired through the
  gateway and MCP. Rebuilding trigram search, regex literal pruning, mmap serving and an anon
  gate in Go is quarters of work to reach parity we already have.

### Option C: status quo plus contracts

Keep both extractors, write a compatibility contract, let each engine serve its own door. Cons:

- It removes nothing, so it does not satisfy COR-786. Maintenance cost is unchanged and grows with
  every grammar bump. Kotlin stays broken in peregrine.
- The stacks keep drifting on symbol identity, and a contract with no shared producer has no
  enforcement point. Callers still have to know which door to knock on.
- Its single honest advantage is that it carries no migration risk.

## Decision: Option A

**entire-graph becomes the precision symbol producer. peregrine remains the serving engine and the
single front door.** This is the only option where each system keeps the half it is demonstrably
good at and drops the half it duplicates:

- peregrine is a serving system with a deployed footprint, incremental indexing, and an in-source
  admission that its persisted resolution is an arbitrary-tiebreak heuristic. It should stop
  owning symbol precision.
- entire-graph is a deterministic producer with 36 semantic languages, a frozen `1.x` schema
  contract (`docs/adr/0001-ga-schema-contract.md`), and an explicit written boundary against
  becoming a server. It should stop growing a serving surface and sell its precision to the engine
  that already serves.

Neither engine is deleted. The **duplicate halves** are.

### The interface

**Artifact.** One SCIP protobuf index per `repo@commit` from `entire-graph snapshot --format scip`
(PR #97), plus its JSON omission note as a required sidecar. The note is contract, not debug
output: it declares which relations and languages were skipped, which is what lets peregrine
decide per-language whether to trust the feed.

Being contract, it is versioned like one. The note already carries a version field
(`"version": "entire-graph-scip-omissions/v1"`, `internal/sem/compact_snapshot.go:299`), so
"freeze the schema" in step 1 means freezing a compatibility **policy**, not a field list. ADR
0001's `1.x` rules (`docs/adr/0001-ga-schema-contract.md`) extend to the note unchanged: the
major is the compatibility boundary and a consumer refuses an unknown one; minors are additive
only, never removing a field, changing a field's meaning, or making an optional field required;
readers ignore unknown fields and warn rather than fail on a newer minor within a supported
major. A bare `v1` does leave a tolerant reader with nothing to warn on and no way to tell
compatible evolution from a break -- but the fix is not to rewrite the tag. `v1` is already
shipped: `TestSCIPOmissionNoteVersionIsPinned` asserts the exact string, deliberately, so that a
contract change cannot be incidental, and consumers matching it exactly would be rejected by a
rename. Retagging to `/v1.0` would therefore be a breaking change performed in the name of an
additive freeze, and it would fail the very test that guards the contract.

The minor is carried **beside** the tag instead, as a new optional integer field that a tolerant
reader may ignore -- which is exactly the additive move `1.x` allows. The tag stays
`entire-graph-scip-omissions/v1`, exact-match consumers keep working, the pinning test keeps
passing, and a reader that wants to distinguish compatible evolution from a break has a field to
read. A breaking change is still `/v2` with a migration note, and is out of scope for the feed's
`v1` line.

**Identity, and an addressing mismatch to resolve.** peregrine's `.pg` header carries `org_id`,
`repo_name`, `repo_id` and `indexed_commit: [u8; 20]` (`src/index/format.rs:33-44`), so the join
key exists consumer-side. But the **S3 object is repo-scoped, not commit-scoped**: key
`{repo_id[0..2]}/{repo_id}.pg` (`src/server/s3_store.rs:14-24`), overwritten on every rebuild
(`src/server/index_manager.rs:704-716`). There is no commit-addressed index history, and
entire-graph dedupes on tree hash rather than commit. The contract: the feed is addressed by
`(repo_id, commit_sha)`, the producer may dedupe by tree hash underneath, and the consumer
validates the feed against the `indexed_commit` it is currently building. The feed store is
commit-versioned even though the `.pg` store is not.

**Object-format scope: SHA-1 only, for now.** `indexed_commit` is `[u8; 20]`, which holds a
SHA-1 object id and nothing else, while entire-graph reads SHA-256 repositories
(`internal/gitutil/git.go:1529-1530` sizes tree metadata for 64-hex object ids;
`internal/gitutil/git_test.go:1582` branches on `rev-parse --show-object-format`). A 32-byte
commit id cannot be carried in that header, and the exact-commit join above is the whole
freshness contract, so this is scoped rather than fudged: **the feed is defined for SHA-1
repositories only.** The publisher must refuse to publish a feed for a SHA-256 repository rather
than truncate or re-derive an id, and the consumer treats an absent feed there as feed-off and
serves from its native layer -- the same path as any other missing feed, so no new failure mode.
Widening `indexed_commit` to a length-tagged object id is a `.pg` header change and the one part
of this design that would need a format bump; it is deferred, not assumed away. Step 6's corpus
must record each repository's object format, so the scope note is confirmed rather than trusted.

**Delivery, and the no-egress boundary.** entire-graph does not write to the object store.
The provider is no-egress by ratified contract -- "never add remote fetches, hosted API calls,
telemetry, or runtime grammar downloads" (`AGENTS.md:224`) -- and this RFD's own case against
Option B rests on it being "a one-shot, no-egress binary". A producer that had to hold
object-store credentials and reach the network would spend exactly the property that makes its
output trustworthy, and would change that boundary by implication in an implementation review
rather than on purpose here. So the write is split three ways:

- **entire-graph** runs `snapshot --format scip`, which writes the SCIP protobuf to stdout and
  the omission note to stderr (`internal/cli/root.go:271,445`). It has no file sink, no network
  client and no credentials, and needs none: both halves of the artifact leave over pipes.
- **A publisher wrapper**, deployment-side and not the graph binary, captures those two streams
  and uploads them to the same object store as the `.pg` index, under the `(repo_id, commit_sha)`
  key above. It owns the credentials, retries, retention and the paging for a failed publish.
- **The indexer tier** (`deploy/fleet/apps/peregrine/base/index-deployment.yaml`) reads the
  artifact at build time.

Whether that wrapper is a standalone job or a step inside peregrine's indexer tier is open
question 6. That it is *not* entire-graph is decided here.

Once the artifact is in the store, peregrine writes `SectionType::SymbolTable = 6`
(`src/index/format.rs:24-31`) from the feed instead of from its own extractors. **No `.pg`
version bump is required to ingest it**: `VERSION`
stays `1` (`:6`), the enum values are already defined and parsed, and TOC sections are
independently addressable and CRC32-checksummed (`:52-58`), so section 6 can be rewritten alone.
That holds for the symbol table only; widening `indexed_commit` for SHA-256 repositories, above,
is a header change and would need one.

**Freshness contract.**
1. A feed is valid only for the exact `indexed_commit` being built. Any other commit is ignored,
   never approximated. Staleness is a hard miss.
2. Per-language trust is driven by the omission note, which must carry more than aggregate
   counts to do that job. A count says something was skipped; only the records say which file and
   why, and a consumer cannot fall back for one language on a number. The note carries
   `language_tiers` (each language as `semantic` or `inventory-only`) and `partial_failures` (the
   failure records, not just `partial_failure_count`), alongside `commit` and `tree` for the
   revision. A language reported skipped or degraded falls back for that language only, not for
   the whole index.

   **A failure record carries no language, and the fallback is a join, not a field read.** The
   record is `code`, `severity`, `file_path`, `effect_on_semantic_completeness`, `detail`
   (`internal/sem/provider.go:255-261`) -- there is no `language`. That is deliberate rather than
   an omission to correct: `PartialFailure` is one shared record type across every entire-graph
   surface, and several of its producers have no language in hand at all (an ignored subtree that
   could not be read, a git listing that failed). A language field on it would be blank often
   enough that a consumer routing per-language fallback on it would fall back for nothing, which
   is worse than an explicit join. So the consumer resolves the language by joining
   `partial_failures[].file_path` to the SCIP `Document` with the same `relative_path`; the export
   emits a Document for every discovered file, including one whose parse was skipped, and sets its
   `language` from the same file record the tier was derived from
   (`internal/sem/compact_snapshot.go:451-464`).

   One wrinkle the ingestion step must be told rather than left to discover: `Document.language` is
   the SCIP enum name while `language_tiers` is keyed by entire-graph's own language name, and the
   two differ for a fixed, closed set (`C++`/`CPP`, `C#`/`CSharp`, `F#`/`FSharp`,
   `Objective-C`/`Objective_C`, `Protocol Buffers`/`Protobuf`, `Starlark`/`Skylark`, `Bash`/
   `ShellScript`, and the rest, enumerated in full at
   `internal/sem/compact_snapshot.go:852-892`). The mapping is total and one-way, so it belongs in
   the written contract of open question 2 next to the moniker scheme, not re-derived in each
   consumer.
3. Relation loss is expected and bounded: the feed supplies **symbols and resolution**, not
   relations. entire-graph's native snapshot stays the source of truth for the graph.
4. The feed is advisory to availability *while the native extractors remain*. peregrine must build
   a complete, servable index with the feed entirely absent.

   **This conflicts with step 9 and the RFD does not yet resolve it.** Step 9 removes peregrine's
   extractors for every language at feed-on parity. Once they are gone, a feed outage or a rollback
   by clearing the flag cannot produce a complete index for those languages -- it produces one
   missing their symbols. "Feed-off is permanently supported" and "the duplicate extractors are
   removed" cannot both hold for the same language. See open question 9.

**Precision must follow the tier, not the transport.** The feed carries symbols for
inventory-only languages too -- files that were discovered and listed but never parsed for symbols
or relations -- and SCIP has no way to mark them, since every discovered file becomes a `Document`.
Importing the symbol table wholesale as `Scip` / `Semantic` would advertise those as semantically
parsed. The ingestion step must partition by the note's `language_tiers`: only `semantic` languages
may be written as `Scip` / `Semantic`; an `inventory-only` language either keeps the native layer or
is written with `Syntax` precision. Mixed-precision indexes are already legal here, so this costs
nothing but the filter.

**Fallback.** peregrine's native tree-sitter layer is retained and stays the default when no valid
feed is present, emitting `TreeSitter` / `Syntax` exactly as today. `precision` becomes a queryable
field, so callers needing semantic guarantees can filter on it, and mixed-precision indexes are
legal and honest rather than hidden.

### Migration steps and owners

| # | Step | Owner | Gate |
|---|---|---|---|
| 1 | Promote `--format scip` from experimental and freeze the omission-note compatibility policy | Us | Byte-identical export on repeat runs, shown in PR #97. The merge half is done: #97 is on main as of 2026-08-27, and the note reports `unidentified_records`, `unlocated_symbols`, `language_tiers`, `partial_failures`, `commit` and `tree`. Freezing means adopting the additive-only policy in *The interface* for that shape -- the tag stays `entire-graph-scip-omissions/v1` and the minor rides beside it -- not freezing it against all further additions |
| 2 | Publish per-language SCIP coverage matrix vs peregrine's tier-1 15 | Us | Reviewed by Evis |
| 3 | Decide the moniker scheme (open question 2) | Us + Evis | Written into the contract |
| 4 | Build the parity harness | TBD (open question 5) | Reproducible by both sides |
| 5 | Add SCIP ingestion to peregrine's index writer, per-repo flag, default off | Evis | Feed-off path byte-identical to today |
| 6 | Run parity on N repos, feed-on vs feed-off | Joint | Recall no worse than feed-off *and* above the per-language absolute floor, precision better, budgets held |
| 7 | Flip default to feed-on per language as each clears parity | Evis | Per-language, reversible |
| 8 | Wire the converged surface into the single MCP front door | Us + Alisha | Follows the `brain_*` plane pattern, P2.M3 |
| 9 | Remove peregrine's duplicate extractors at feed-on parity | Evis | Two release cycles feed-on, no rollback |

### What "deprecate duplicates" concretely removes, and when

Nothing is removed before step 7 has held for two release cycles. Then:

- **peregrine**: the per-language extractors and import resolvers (`src/symbols/lang_*.rs`,
  `src/symbols/imports_*.rs`) for every language at feed-on parity, plus the index-time heuristic
  `batch_resolve_references` (`resolve.rs:122`) once no shipped language depends on it. Up to 15
  extractor modules and 11 import-resolver modules, including `lang_kotlin.rs`, dead today.
  tree-sitter stays only for languages still on the fallback path. `resolve_file_references` stays
  as the query-time path for repos with no feed.
- **entire-graph**: branches `codex/eg-graphify-lexical-sidecar` and
  `codex/eg-graphify-query-service` are closed rather than merged.
  `docs/brain-and-graph-boundaries.md` gains a verdict entry recording that full-text and
  multi-repo code search is peregrine's job. entire-graph's own `search` stays, scoped to
  single-repo structural ranking, as `docs/search.md` already describes.
- **Front door**: `search_entire_code` becomes the one code-search tool. Its dropped `usages` mode
  should be reinstated: the MCP ADR recorded "peregrine has no such endpoint today"
  (`docs/adrs/20260721-mcp-curated-product-surface.md:145`), now stale, since `/api/usages` exists
  at `peregrine/src/server/http.rs:1688` and entire-api allowlists `/search/api/usages` (`:458`).

### Test and rollout gates

- **Corpus**: at least 10 repos spanning all peregrine tier-1 languages, including one monorepo
  large enough to test PR #97's in-memory materialization. Each repository's git object format is
  recorded with its result, so the SHA-1-only scope above is confirmed on the corpus rather than
  assumed.
- **Primary metric**: symbol recall and precision of the SCIP-fed symbol table against peregrine's
  native layer, per language, definitions and references scored separately. Cross-file
  `resolved_to` correctness scored against hand-labelled ground truth, since that is the field the
  heuristic is weakest on and the one a semantic producer should most improve.
- **Non-regression**: recall must not drop in any language. A precision gain that costs recall is
  a fail, because users notice a missing hit more than a spurious one.
- **Absolute floor, because the baseline is not trustworthy everywhere.** Non-regression against
  peregrine's native layer is a floor of zero wherever that layer is empty or broken. Kotlin
  yields no symbols at all today (`parser.rs:27`, `extract.rs:93-96`), so a feed that emitted
  nothing for Kotlin would pass a zero-to-zero comparison and could reach feed-on without ever
  fixing the coverage gap this RFD cites as a motivation. Every language therefore also has to
  clear an **absolute recall floor against hand-labelled ground truth** on a sampled subset of
  the corpus, not only a delta against the native layer. A language whose native baseline is
  empty or known-degraded -- Kotlin at minimum -- may not go feed-on on the delta comparison at
  all; the ground-truth measurement is the only evidence that counts for it. The floor per
  language is agreed with Evis before the run, like the budgets.
- **Budgets**: `.pg` size delta, index build wall-clock delta, p50/p99 query latency, each with a
  ceiling agreed with Evis before the run, not after.
- **Rollout**: per-repo flag, then per-language default, then global, each stage reversible by
  clearing the flag and falling back to the native layer with no reindex. Feed-off stays a
  supported configuration for as long as that language still has a native extractor -- which step 9
  ends. See open question 9.

### Non-goals

- **Embedding backbone convergence.** Cohere embed-v4/Turbopuffer versus nomic-768/pgvector is
  COR-788, separate RFD. Nothing here adds vectors; both engines in scope are non-vector and stay
  that way.
- **Prose and knowledge search.** entire-search and librarian keep their corpora and owners.
- **Not an entire-graph deprecation.** Its relation graph, impact analysis, diff and brain feed are
  unaffected and remain the reason it exists.
- **Not an MCP surface design.** The single surface is P2.M3; this RFD only commits to what sits
  behind it.

## Open questions

1. **SCIP coverage per language.** 36 semantic languages against peregrine's 15, but the
   projection is lossier than the native snapshot and its per-language fidelity is unmeasured.
   Step 2 answers this and could shrink the set that ever reaches feed-on. (An earlier
   revision of this RFD also flagged doc drift here, `docs/language-support.md` reporting
   185 supported / 36 semantic against a live `capabilities --json` of 179 / 35. That drift
   has since been reconciled on main: `capabilities --json` now reports 185 supported / 36
   semantic / 149 inventory-only and 30 relation types, matching the doc. Only the
   per-language SCIP fidelity question remains open.)
2. **Symbol identity.** Partly resolved; the remainder is a real decision.

   **Resolved.** The package version used to be the commit, so an unchanged symbol got a new
   identity on every commit: two indexes of the same repository one unrelated commit apart
   shared no symbol string at all, which would have made a per-`repo@commit` feed unjoinable
   across commits and defeated the cross-index linking the field exists for. The version is now
   the project's own declared version from its root manifest (`package.json`, `Cargo.toml`,
   `pyproject.toml`), falling back to `0` -- the common case for Go, whose `go.mod` carries a
   module path and not a version. A regression test pins it, and commit provenance moved to
   `Metadata` and the omission note. Consumers must now take `(repo, commit)` from the
   envelope, never from the symbol.

   **Still open.** Four things that must move together, because each rewrites every symbol's
   identity:
   - The scheme is `entire-graph` with package manager `.`, and the descriptor embeds
     `shortDigest(record.ID)`, an internal compound-v1 hash. SCIP parses that into the standard
     `disambiguator` field, so it is valid and stable, but it is not a moniker another indexer
     resolves to the same symbol. Real package-manager monikers, or internal IDs and revisit?
   - Package identity is per-repository: the package name is the repo key, so a monorepo whose
     sub-packages carry different versions cannot express that without also moving the name.
     Per-directory package identity is the same decision viewed from the other side.
   - **External symbols are minted in the indexed repository's own package and version**, not in
     the defining dependency's (`compact_snapshot.go`, `scipExternalSymbol`). SCIP links a
     reference to a separately indexed dependency by symbol identity, so an id that names *our*
     package can never match the id that dependency's own index exports. Cross-repository
     go-to-definition therefore cannot work at all, however the scheme question above is settled.
     This is the sharpest form of the identity decision, because it is the one the feed's stated
     purpose depends on.
   - **Descriptors omit the container ancestry.** A symbol carries a file descriptor and a leaf
     descriptor and nothing between, so a nested type or a method does not spell out
     namespace -> class -> member. SCIP expects non-local descriptors to form the fully qualified
     hierarchy and reserves `enclosing_symbol` mainly for locals, so a standard consumer cannot
     reconstruct the structure. `enclosing_symbol` is populated today and carries the parent, but
     relying on it is a private convention, not the interchange contract.

   Decide all four before step 5. They are one decision wearing four faces: each rewrites every
   symbol's identity, so settling them separately churns consumers once per answer.
3. **Index size and latency budgets.** Semantic symbols carry more data per symbol. peregrine's
   headline is 4.9x smaller than Zoekt; decide how much of that lead is for sale before spending it.
4. **In-memory materialization.** PR #97 builds the full SCIP index in memory, which a SCIP
   `Index` cannot avoid: it is one protobuf message and cannot be streamed. Now measured on
   entire-graph itself (about 28 MB of native NDJSON, 9,770 definitions, 25,476 references):
   peak RSS about 170 MB against about 115 MB for `ndjson` and about 119 MB for
   `compact-ndjson`, so roughly **1.5x** the streaming formats rather than the order of
   magnitude this question assumed. A multiplier measured on a mid-size repository does not
   carry to a monorepo, where the absolute figure is what decides, so the corpus in the test
   gates must still include one monorepo large enough to settle it -- but this is now a
   sizing question, not an unknown.
5. **Who owns the parity harness?** It must be trusted by both sides, which argues against either
   side owning it alone. Unassigned, and the biggest scheduling risk here.
6. **Publisher runtime placement.** The producer itself is settled -- entire-graph emits over
   pipes and never uploads (see *Delivery*). Open is where the publisher wrapper that captures
   and uploads those streams runs: a step inside peregrine's indexer tier, or a separate job.
   Affects failure isolation and paging.
7. **Feed store addressing.** The `.pg` store is repo-keyed and overwritten in place while the feed
   is commit-keyed. Commit-versioned feed store with its own retention, or repo-keyed and accept
   that a rebuild races the feed?
8. **Staging to production.** peregrine is staging-only, single replica, no live autoscaling per
   `docs/go-live-plan.md`, no production cluster overlay in the repo. Convergence assumes it
   reaches production. If that slips, COR-786 lands on an engine no user can reach.

9. **Does feed-off survive step 9, and if not, which promise goes?** The freshness contract says
   peregrine must build a complete servable index with the feed absent, and the rollout section
   calls feed-off permanently supported. Step 9 removes the native extractors for every language at
   feed-on parity. Those cannot all be true at once: after removal, a feed outage degrades those
   languages rather than falling back cleanly. Three ways out, and this RFD should pick one before
   step 5 rather than discovering it at step 9:
   - **Keep the extractors.** Feed-off stays real, but "deprecate duplicates" is not satisfied for
     the languages that matter most, which is what COR-786 asked for.
   - **Retire the feed-off contract explicitly.** Accept that a feed outage degrades a feed-on
     language, and say so in the contract, with whatever availability commitment that implies for
     the producer.
   - **Scope it.** Keep the extractors only for languages where a degraded index is unacceptable,
     and remove the rest, making the duplicate-removal per-language rather than global.

   Deciding this late is the expensive option: step 9 is the irreversible one, and it is the step
   that discovers the answer.

## Consequences

- One tree-sitter extraction stack of record, maintained by one team, covering a superset of
  today's languages, and fixing Kotlin as a side effect.
- Symbol precision becomes an explicit queryable property rather than an undocumented heuristic.
- peregrine gains a cross-repo build dependency it did not have, mitigated by a mandatory
  always-supported fallback.
- entire-graph gains a second consumer contract alongside brain. ADR 0001's additive-only `1.x`
  rules extend to the SCIP projection and its omission note, versioned as set out in
  *The interface*.
- COR-786 closes when steps 1 through 7 land and step 9 removes the first duplicate extractor.
  Step 8 continues into P2.M3.

## Appendix: pointers

**peregrine** (`entirehq/peregrine`, Rust, owner evisdren, latest `b55bfce` / PR #180)

- **Format**: `src/index/format.rs:5-6` `MAGIC = b"PRGN"`, `VERSION: u32 = 1`; `:24-31`
  `SectionType` (SymbolTable = 6; its `// Future` comment is stale, it is live per
  `writer.rs:577,991`, `reader.rs:485-491`); `:33-44` `Header` incl. `indexed_commit: [u8; 20]`;
  `:52-58` `TocEntry`, 24 bytes, CRC32 per section
- **Storage**: `src/server/s3_store.rs:14-24` key `{repo_id[0..2]}/{repo_id}.pg`;
  `index_manager.rs:704-716` rebuild overwrites it, so the index is repo-addressed, not
  commit-addressed; `:563-566` S3 cold-start seed. `reader.rs:152-156` mmap, `registry.rs:41-51`
  readers stay resident; `incremental.rs:2-4` git diff-tree incremental, `patch.rs:1-6` splices
- **Reserved seam**: `src/symbols/types.rs:111-115` `SymbolSource`, `:118-122` `SymbolPrecision`,
  `:137-138` per-symbol fields, `:265-276` deserialize maps byte 1 to `Scip` / `Semantic`,
  `:232-241` older indexes end after the precision byte. Deliberate per
  `docs/superpowers/specs/2026-03-31-symbolic-search-design.md:45,48` and `docs/design.md:27`.
- **Extractors**: `src/symbols/mod.rs:8-22` 15 `lang_*.rs`, `:25-39` import resolvers;
  `parser.rs:15-34` only 14 grammars wired, `:27` Kotlin excluded, `:6-11` unmapped returns
  `None`, `extract.rs:93-96` yields no symbols
- **Resolvers**: `resolve.rs:118` "ponytail" admission; `:122-179` `batch_resolve_references`,
  `:174-176` arbitrary tiebreak, called from `writer.rs:170,416`, `incremental.rs:1121`;
  `:185-380` `resolve_file_references`, query-time only, sole caller `http.rs:1296`.
  `serialize.rs:220-227` SymbolTable header 7x u32 with `OLD_HEADER_SIZE=20` /
  `V2_HEADER_SIZE=24` still supported: the sub-format evolved three times under one unchanged
  top-level `VERSION=1`.
- **Serving**: `src/server/http.rs:1679-1692` routes (`/api/symbols` :1687, `/api/usages` :1688,
  `/api/resolve` :1689, `/api/fuzzy` :1690); `proto/peregrine/v1/peregrine.proto:4-11` gRPC has 7
  RPCs and does **not** expose symbols, usages or resolve, so HTTP is a superset.
  `src/search/ranking.rs:181-219` already uses symbols: +5.0 definition boost.
- **Deploy**: `deploy/fleet/apps/peregrine/base/` index-deployment, query-statefulset, index-hpa,
  query-hpa; only overlay `deploy/fleet/clusters/nonprod/eks-staging-us-west-2/peregrine.yaml`.
  `docs/go-live-plan.md:1-30` staging behind envoy, ~114 repos, single replica; gaps: no
  multi-replica or sharding, no load testing, no HPA. Re-index is NATS-driven from entiredb.

**entire-graph** (`entireio/entire-graph`, Go)

- `internal/sem/provider.go:34` `SchemaVersion = "1.1"`; `:50-81` the 30 relation types.
  `docs/adr/0001-ga-schema-contract.md` `1.x` frozen GA, additive-only minors, tolerant readers.
- `docs/adr/0002-committed-tree-cache-key.md` and `internal/sem/search_cache.go:485-489`
  "deliberately tree-only, not commit-keyed"; `records_cache.go:89-90` cache paths. No
  incremental reuse.
- `docs/language-support.md` 185 supported / 36 semantic / 149 inventory-only, covering all 15 of
  peregrine's tier-1 set; live `capabilities --json` now agrees (185 / 36 / 149, 30 relation
  types), so the drift noted in open question 1 is reconciled.
  `docs/search.md` search is hybrid lexical plus graph expansion, single `--repo`, byte-budgeted.
- `docs/brain-and-graph-boundaries.md` MCP surface and multi-repo graph are "Brain's job";
  per-file incremental invalidation is "unbuilt, not declined".
- PR #97 `ec/candidate-scip-export-v2`, MERGED 2026-08-27, so the following are on main.
  `internal/cli/root.go:255`
  `--format scip`, `:262-269` snapshot-mode only. `internal/sem/compact_snapshot.go:454,462`
  deterministic marshal; `:345-365` Documents; `:384-404` SymbolInformation and definition
  occurrences; `:407-420` external symbols; `:540-547` only 15 of 30 relations map; `:549-560`
  only Import/WriteAccess/ReadAccess get roles; `:427-429` `UnsupportedRelationCounts`;
  `:613-636,660-664` entire-graph-specific monikers. `go.mod:6` `scip-code/scip/bindings/go/scip
  v0.9.0`.
- Branch `codex/eg-graphify-lexical-sidecar` commit `5442ed87`, not on main:
  `internal/sem/search_lexical_index.go:35-36` `EGLEXI1`/`EGPOST1`, `:26-28` postings artifacts,
  `:657,751` trigram API. Zero callers in the query path.
- SCIP is on main since #97; the note's version tag is `entire-graph-scip-omissions/v1`
  (`internal/sem/compact_snapshot.go:299`), and `--format scip` writes the protobuf to stdout and
  the note to stderr (`internal/cli/root.go:271,445`), with no file or network sink.
  Current CLI is `entire graph ...`; `entire sem ...` is the deprecated old name.

**entire-api / mcp** (the front door, and where the P2.M3 surface lands)

- `entire-api/internal/server/api.go:399` `AnonGate{ Upstream: "peregrine", ... }`; `:451-457`
  peregrine's OpenAPI merged under a `/search` prefix, failing closed on rename; `:458`
  `anonSearchRoutes = {"/search/api/symbols", "/search/api/usages"}`
- `mcp/internal/entiremcp/curatedtools.go:619-666` `search_entire_code`: `content` to
  `/search/api/search`, `files`/`symbols` to `/search/api/fuzzy`, resolving to the storage ULIDs
  peregrine keys on; `:55` `codeSearchUnavailable`, peregrine "staging-only today".
  `docs/adrs/20260721-mcp-curated-product-surface.md:100-105` decision 10; `:145` `usages` dropped
  as unavailable, now stale.
- Surface pattern to follow, PR #41 brain tools: `braintools.go:123-133` `brainPlane`, `:393-420`
  `withBrainRepo` adapter, `:299-369` `registerBrainTools`, registered once at `server.go:293`;
  shared routing and auth via `entireAPIPlane` at `gitplane.go:41-61`.
- `grep -rni "entire-graph"` across the MCP repo returns zero hits.
