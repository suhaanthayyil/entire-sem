# Entire Graph documentation

These are the current project documents, organized by what you are trying to
do. `entire graph help` lists the public command surface and documented flags;
the command parsers define what the binary accepts, including deliberately
help-hidden compatibility and tuning flags. Generated capability data is
authoritative in `entire graph capabilities --json`.

## Using Entire Graph

| Document | Purpose |
| --- | --- |
| [Root README](../README.md) | What Entire Graph is, installation, activation, and the first agent task |
| [Agent activation](agents.md) | `init-agents` file effects, rerun and marker behavior, client notes, verification, and recovery |
| [Command reference](commands.md) | Task-grouped manual and automation surface, with the defaults that matter |
| [Search results and ranking](search.md) | What `search` returns and how to read it |
| [Operations](operations.md) | Installation channels, cache locations and keys, prewarming, reports, and release archives |
| [Trust and security](trust-and-security.md) | What the tool reads, writes, executes, and sends over the network |
| [Language support](language-support.md) | Current semantic and inventory-only language matrix |
| [Captured session evidence](evidence/2026-08-16-mux-agent-session.md) | Point-in-time record backing the root README's transcript: capture conditions, events, and verbatim outputs |

## Integrating and contributing

| Document | Purpose |
| --- | --- |
| [Semantic provider requirements](semantic-provider-requirements.md) | Provider responsibilities, ownership boundary with Entire Brain, profiles, relations, warnings, and limits |
| [Snapshot format](snapshot-format.md) | Streaming NDJSON contract, compact artifact, and schema compatibility rules |
| [Entire Brain and Entire Graph boundaries](brain-and-graph-boundaries.md) | Ownership decisions and explicit non-goals |
| [Benchmarks](benchmarks.md) | Quantitative methodology, results, corrections, and caveats; `bench/README.md` documents the harness layout and flags |
| [ADR 0001](adr/0001-ga-schema-contract.md) | Accepted `1.x` schema compatibility contract |
| [ADR 0002](adr/0002-committed-tree-cache-key.md) | Accepted committed-tree cache-key decision; authoritative for committed-tree cache identity |
| [ADR 0003](adr/0003-working-tree-search-snapshot-cache.md) | Superseded clean-tree search-snapshot reuse decision |
| [ADR 0004](adr/0004-working-tree-cache-security-boundary.md) | Accepted working-tree cache bypass security decision; authoritative for working-tree eligibility |
| [RFD 0005](adr/0005-converge-code-search.md) | Proposed convergence of code search on one engine; not yet decided, open for comment |

Completed plans, superseded references, branch diaries, and point-in-time proof
logs are listed in the [archive](archive/README.md). Archived documents are kept
for provenance and are not normative. One internal process document remains
active until its work completes: the
[README improvement plan](readme-plan.md), which tracks the root README
revision and will move to the archive when done.

## Sources of truth

| Subject | Source |
| --- | --- |
| Public commands and documented flags | `entire graph help`, `entire graph <command> --help`, and `internal/cli/help.go` |
| Accepted arguments, including help-hidden flags | Command parsers under `internal/cli/` |
| Languages, profiles, and relation types | `entire graph capabilities --json` |
| Provider schema | [ADR 0001](adr/0001-ga-schema-contract.md), [snapshot format](snapshot-format.md), and `internal/sem/provider.go` |
| Committed-tree cache identity | [ADR 0002](adr/0002-committed-tree-cache-key.md) and the cache implementations under `internal/sem/` |
| Working-tree cache eligibility | [ADR 0004](adr/0004-working-tree-cache-security-boundary.md) and `internal/sem/search_cache.go` |
| Agent activation behavior | [Agent activation](agents.md), verified against the installed release |
| Benchmark behavior | [Benchmarks](benchmarks.md), `bench/README.md`, and `cmd/graph-bench` |
