# Entire Graph Risk Intelligence

## One-sentence summary

Entire Graph Risk Intelligence adds deterministic, changeset-level risk analysis to a large open-source developer tool, then optionally makes the resulting evidence queryable in Databricks Delta tables.

## Problem, intended user and why it matters

Developers reviewing a change in a large repository can see a text diff but cannot reliably see its downstream blast radius or the tests most likely to matter. The intended user is a developer or reviewer who needs a bounded, evidence-backed risk assessment before shipping a change.

## Selected Entire track and why Entire is essential

**Selected track: Large Open Source Repository (Track 2).**

This is a contribution to [Entire Graph](https://github.com/entireio/entire-graph), an open-source Entire CLI plugin. Entire is essential rather than incidental: the new `entire graph risk` command composes Entire Graph's semantic Git diff, persistent code graph, impact analysis, and checkpoint-aware workflow. A plain Git diff cannot supply the direct callers, type consumers, affected tests, or explicit graph-evidence limitations reported by the command.

## Repository, branch, and submission commits

- GitHub fork: <https://github.com/Yashwanthk06/entire-graph>
- Upstream pull request: <https://github.com/entireio/entire-graph/pull/235>
- Submission branch: `track2-risk`
- Risk-analysis feature commit: [`d760c2929bd409a63a320b76e0a7de09b97bd4d9`](https://github.com/Yashwanthk06/entire-graph/commit/d760c2929bd409a63a320b76e0a7de09b97bd4d9)
- Databricks integration commit: [`15e7e1782c79958ebd2e27a7d730d3ee8db1fb99`](https://github.com/Yashwanthk06/entire-graph/commit/15e7e1782c79958ebd2e27a7d730d3ee8db1fb99)
- Entire mirror/project URL: **not available from this local checkout.** Add the exact shared Entire project or mirror URL here before submission if the form requires one.

## Architecture and main workflow

1. `entire graph risk` semantically compares `--base` and `--head`, or reads an Entire checkpoint.
2. It builds/loads the current committed-tree graph and resolves bounded dependency impact for changed entities.
3. It emits deterministic `high`, `medium`, or `low` risk, graph evidence, affected-test paths, and limitations in text or JSON.
4. The optional Databricks job ingests the JSON report into a Unity Catalog Delta table, one row per changed entity, for team-wide querying and trend analysis.

The main implementation is [`internal/cli/risk.go`](internal/cli/risk.go), with behavior covered by [`internal/cli/risk_test.go`](internal/cli/risk_test.go). The Databricks integration is isolated under [`databricks/`](databricks/) and therefore does not alter local CLI behavior or send source code to a service.

## Entire Graph findings and verification

The key graph finding is that risk should be determined from semantic relationships, not text-file churn. For each changed entity, the command resolves direct callers, transitive callers, type consumers, callees, and graph-related test paths. A signature change with resolved callers/type consumers is classified conservatively above an isolated body-only change. When the requested revision is not the current graph snapshot, the command deliberately omits misleading graph evidence and reports a limitation.

Verification is represented in the risk-command tests: they cover flag validation, deterministic risk classification, JSON output, bounded entity analysis, affected test selection, and degraded/non-current graph evidence. The command's output keeps graph facts separate from the tool's risk inference.

## Noon Curveball: what changed and how we adapted

The curveball was the need to demonstrate Databricks value without compromising Entire Graph's local-only and no-egress runtime contract. We adapted by keeping the primary analysis entirely local and adding an **optional, separately deployed** Databricks bundle that consumes only the explicit JSON report chosen by the user. This preserves the core command's deterministic, offline behavior while enabling governed analytics after a report is intentionally exported.

The isolation is demonstrated by the separate bundle files and by the existing Go tests for `entire graph risk`; no Go production path imports a Databricks SDK or contacts a workspace.

## Checkpoint links and what each checkpoint proves

**Submission blocker: no Entire checkpoints are present on the current local submission branch.** `entire checkpoint list` reported `0` checkpoints in this checkout. Do not submit placeholder links.

Before submitting, create or recover the required checkpoints from the tracked agent session and replace this section with their real Entire URLs:

| Checkpoint | URL | What it proves |
| --- | --- | --- |
| Risk-analysis implementation | `ADD REAL ENTIRE CHECKPOINT URL` | The semantic-diff, graph-impact, risk classification, and tests were created on the tracked branch. |
| Databricks integration | `ADD REAL ENTIRE CHECKPOINT URL` | The optional bundle, notebook, documentation, and review of the no-egress boundary were completed. |

Use `entire checkpoint list` and `entire checkpoint explain <id>` from the tracked submission branch to obtain and verify the records. If the checkpoints live on another existing branch/session, switch to it and use the URLs returned there rather than creating fictitious replacements.

## Setup, run and test instructions

Prerequisites: Git 2.36+, Go 1.26 with CGO enabled, the Entire CLI, and the Entire Graph plugin build dependencies. `mise` is optional but pins the repository toolchain.

```sh
git clone https://github.com/Yashwanthk06/entire-graph.git
cd entire-graph
git checkout track2-risk

# Build and install the plugin locally.
go build -o entire-graph ./cmd/entire-graph
entire plugin install ./entire-graph --force
entire graph version

# Run the product against the current submission change.
entire graph risk --repo . --base origin/main --head HEAD --format text
entire graph risk --repo . --base origin/main --head HEAD --format json > risk-report.json

# Run the repository test suite (or `mise run test`).
go test -timeout 30m ./...
```

For a focused check of this feature, run `go test ./internal/cli`. The CI environment must include the repository's generated Tree-sitter grammar sources; without them Go can fail during package setup before risk tests execute.

## Databricks use, data sources and limitations

**Capabilities used:** Databricks Declarative Automation Bundles (formerly Asset Bundles), a Lakeflow Job, a Python notebook task, Spark DataFrames, Unity Catalog schema/table creation, and Delta Lake append writes.

**Why it is essential:** Databricks is optional for the individual CLI decision, but essential for the team-scale analytics workflow: it turns multiple deterministic risk reports into a governed, queryable history for release/risk trends without changing or externalizing the repository analysis runtime.

**Relevant paths:**

- [`databricks/databricks.yml`](databricks/databricks.yml)
- [`databricks/resources/risk_ingestion.job.yml`](databricks/resources/risk_ingestion.job.yml)
- [`databricks/src/ingest_risk_reports.py`](databricks/src/ingest_risk_reports.py)
- [`docs/databricks-risk-ingestion.md`](docs/databricks-risk-ingestion.md)

**Data provenance:** The input is JSON explicitly generated by `entire graph risk --format json` for a named Git base/head revision or checkpoint. The job writes entity-level rows containing revision metadata, risk classifications, graph evidence, recommended-test paths, limitations, source-report path, and ingestion timestamp. No third-party or scraped data is used.

**Reproduction:**

```sh
entire graph risk --repo . --base origin/main --head HEAD --format json > risk-report.json
# Upload risk-report.json to a Unity Catalog Volume, for example:
# /Volumes/main/entire_graph/risk_reports/risk-report.json
cd databricks
databricks bundle validate -t dev
databricks bundle deploy -t dev
databricks bundle run entire_graph_risk_ingestion -t dev \
  --params risk_report_path=/Volumes/main/entire_graph/risk_reports/risk-report.json
```

**Workspace/app/endpoint/demo URL:** **not deployed yet.** Add the Databricks Job URL and one successful run URL here only after the reproduction steps complete. The Databricks CLI was not installed in the development checkout, so bundle validation and a workspace run were not performed locally.

## Known limitations and next steps

- Graph evidence is intentionally bounded and can be incomplete for ambiguous symbols, partial parses, or a requested head that is not the current checkout.
- Risk categories are deterministic heuristics, not a claim of test coverage or production safety.
- Databricks ingestion is append-only; downstream queries should deduplicate by revision/checkpoint and source-report path when needed.
- Required Entire checkpoint links, an Entire mirror/project URL if mandated, and a successful Databricks job run URL remain to be added before final submission.

## Demo readiness

**Opening sentence:** “Entire Graph Risk Intelligence helps code reviewers see which semantic code changes are most likely to break dependent components, and what to test next.”

Show this critical path, not slides alone:

1. Run `entire graph risk --repo . --base origin/main --head HEAD --format text` and point out one changed entity, its caller/type-consumer evidence, risk level, and recommended tests.
2. Show the corresponding JSON output and the focused test command/result.
3. Explain that Entire Graph supplies the semantic diff and dependency evidence that a Git text diff cannot.
4. Open a real Entire checkpoint and state what implementation decision it records. Do this only after the links above have been added.
5. Explain the curveball: analysis stays no-egress/local; only a user-exported JSON report enters the Databricks workflow.
6. If opting into Databricks, run the bundle and show the resulting Delta table query plus the successful job run URL.
7. Close with the known limitations and the next production step: scheduled report ingestion, deduplication, and a release-risk dashboard.

If a live Databricks workspace is unavailable, record a reliable screen capture of steps 1–3 and the pre-recorded successful Databricks job/table result. Do not present an unrun bundle as a live demo.
