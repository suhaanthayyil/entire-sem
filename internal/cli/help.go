package cli

import (
	"fmt"
	"io"
	"strings"
)

// This file is the single source of truth for `entire graph` help. The CLI is a
// hand-rolled dispatcher (see root.go), so instead of a framework we keep a small
// registry of commandDoc values and render both the grouped root listing and the
// per-command detail view from it. Adding a command here (and to the Run switch)
// is what makes it show up in help and answer `--help`.

// cmdGroup buckets commands in the root listing by what the user is trying to do.
type cmdGroup int

const (
	groupSetup   cmdGroup = iota // set up the graph with a coding agent
	groupInspect                 // query the graph to locate/understand code
	groupAnalyze                 // change/risk analysis and reporting
	groupMeta                    // help, diagnostics, and version info
)

// groupTitles are the section headers, rendered in this order.
var groupOrder = []cmdGroup{groupSetup, groupInspect, groupAnalyze, groupMeta}

var groupTitles = map[cmdGroup]string{
	groupSetup:   "Set up your agent",
	groupInspect: "Inspect the graph",
	groupAnalyze: "Analyze changes & more",
	groupMeta:    "Help & diagnostics",
}

type flagDoc struct {
	name string // e.g. "--top-k"
	arg  string // value placeholder, empty for boolean flags, e.g. "n"
	def  string // default value shown as "(default: X)", empty to omit
	desc string
}

type argDoc struct {
	name string // e.g. "<checkpoint-id>"
	desc string
}

type commandDoc struct {
	name     string
	group    cmdGroup
	summary  string   // one line, shown in the root listing
	usage    []string // one or more usage lines
	long     string   // detail paragraph(s) shown under Usage
	args     []argDoc
	flags    []flagDoc
	examples []string
	aliasOf  string // if set, this command renders the target's help
	hidden   bool   // aliases are not listed in the root help
}

// intro is the one-line description printed above the root command listing.
const intro = "entire graph adds a deterministic, no-egress code graph to Entire checkpoints."

// commandDocs is the ordered registry. Every command dispatched in root.go's Run
// switch has an entry here (TestRegistryMatchesDispatch enforces the parity).
var commandDocs = []commandDoc{
	// ── Set up your agent ────────────────────────────────────────────────
	{
		name:    "init-agents",
		group:   groupSetup,
		summary: "Install the coding-agent guide into AGENTS.md/CLAUDE.md",
		usage:   []string{"entire graph init-agents [--repo path]"},
		long:    "Writes the operating guide into a project's AGENTS.md and CLAUDE.md so any coding agent working in the repo knows to locate code with the graph before broad grep/read exploration.",
		flags: []flagDoc{
			{name: "--repo", arg: "path", desc: "Repository to install into (default: current repo)"},
		},
		examples: []string{"entire graph init-agents --repo ."},
	},
	{
		name:     "agent-guide",
		group:    groupSetup,
		summary:  "Print the coding-agent operating guide",
		usage:    []string{"entire graph agent-guide"},
		long:     "Prints the resolution-first guide (graph retrieval, focused source inspection, verification) to stdout without writing any files. Use init-agents to install it into a project instead.",
		examples: []string{"entire graph agent-guide"},
	},
	{
		name:    "index",
		group:   groupSetup,
		summary: "Build/warm a committed-tree cache variant for matching queries",
		usage:   []string{"entire graph index --repo . [--head] [--force] [--profile syntax-only|fast|full] [--cache-dir path] [--report GRAPH_REPORT.md] [--format text|json|auto]"},
		long: "Prebuilds a durable, complete committed-tree snapshot. Later --head searches/neighbors can reuse it when caching is enabled and they resolve the same cache directory, profile, and ordered ignore/include inputs, with unchanged input-file contents and .graphignore. index defaults to full while search defaults to fast, so a default index does not warm a default search --head. Re-running index refreshes that cache variant: an unchanged tree hits, while a changed tree rebuilds. Pass --force to rebuild and overwrite the entry even when the tree is unchanged.\n\n" +
			"At a terminal it draws a live progress bar on stderr (only when it actually builds — a cache hit returns instantly) and prints a readable summary; piped or with --format json it emits the schema-versioned JSON summary that agents and CI consume. --report writes a human-readable GRAPH_REPORT.md rendered from the snapshot, so the same tree always renders the same bytes. The cache defaults to the platform per-user cache dir (macOS ~/Library/Caches/entire-graph; XDG_CACHE_HOME or ~/.cache elsewhere); --cache-dir and ENTIRE_PLUGIN_DATA_DIR override it.\n\n" +
			"A repo-root .graphignore (gitignore syntax) is honored by every graph command, on top of .gitignore. Use it for tracked-but-vendored/generated sources — e.g. tree-sitter parser.c blobs — that otherwise surface as E_FILE_TOO_LARGE/E_PARSE_ERROR partial failures and a \"degraded\" completeness. Oversized/minified skips also no longer count toward \"degraded\" on their own.",
		flags: []flagDoc{
			{name: "--repo", arg: "path", desc: "Repository to index (default: current repo)"},
			{name: "--head", desc: "Accepted for symmetry with the query commands; index is always HEAD-only, so this is a no-op"},
			{name: "--force", desc: "Rebuild from scratch and overwrite the cache even if the tree is unchanged"},
			{name: "--profile", arg: "syntax-only|fast|full", def: "full", desc: "Parsing depth; full favors call-graph correctness"},
			{name: "--cache-dir", arg: "path", desc: "Override the committed-tree cache directory"},
			{name: "--report", arg: "path", desc: "Also write a human-readable GRAPH_REPORT.md"},
			{name: "--format", arg: "text|json|auto", def: "auto", desc: "Summary format; auto = text at a terminal, json when piped"},
			{name: "--ignore-file", arg: "path", desc: "Extra gitignore-style rules to exclude paths (repeatable)"},
			{name: "--include-file", arg: "path", desc: "Re-include ignored paths (gitignore-style; not an allowlist, repeatable)"},
		},
		examples: []string{
			"entire graph index --repo . --head",
			"entire graph index --repo . --head --format json",
		},
	},
	{
		name:    "capabilities",
		group:   groupSetup,
		summary: "List parsed languages, relation types, and features",
		usage:   []string{"entire graph capabilities --json"},
		long:    "Feature-detection surface: which languages are semantically parsed vs inventory-only (file records but no relations), the relation types supported, and enabled features. Check this before assuming a language has semantic relations.",
		flags: []flagDoc{
			{name: "--json", desc: "Required; emits the capabilities report as JSON"},
		},
		examples: []string{"entire graph capabilities --json"},
	},

	// ── Inspect the graph ────────────────────────────────────────────────
	{
		name:    "search",
		group:   groupInspect,
		summary: "Find the code for a task from a plain-language query (start here)",
		usage:   []string{`entire graph search --query "issue or concept" --repo . [--top-k 10] [--format text|json|ndjson|agent] [--head] [--profile fast|full] [--deep]`},
		long: "Ranked source regions for a plain-language description, with source and file:line inline, budgeted to drop straight into context. This is the first move for almost every locate task.\n\n" +
			"By default search returns: ranked candidate fix sites (top hits as full function bodies), RELATED SITES, the COVERING TEST plus other tests over the same code (ALSO COVERING), SAME-CONCEPT LITERAL (every place the concept is named, tagged EDIT/CONSUMER/DOC), a VERIFY line (the narrowest test command for the file), and a CLOSED-SET WARNING when a switch over a sealed set would fail at runtime. The three reference blocks (container map, signature types, declaration card) are OFF by default because they cost turns in agent sessions; --reference-blocks all turns them on for interactive reading.\n\n" +
			"--top-k only changes how many results come back; --deep additionally runs the exhaustive sparse (BM25) pass and fuses it with the semantic ranking (slower, reads every eligible file).\n\n" +
			"Ranking returns one region per unit, which is right for code and wrong for whole prose documents: one markdown document can hold the answer across several distant regions. So for prose the unit is the SECTION, not the file — a document's headed sections are ranked against every other section on their own scores, exactly as independent files would be (--document-resolution ranks a document as one unit instead). Separately, and on prose of any shape including headed documents, when fewer distinct units match than --top-k asked for, the spare slots are spent returning finer regions of the same document as results of their own (multi-resolution retrieval) — strictly additive, so it never displaces a unit and never breaches --max-context-bytes. --single-resolution turns that off. The two stack: a headed document can be ranked by section AND have spare slots filled with promoted passages, so a prose payload may carry both.",
		flags: []flagDoc{
			{name: "--query", arg: "text", desc: "The task or bug in one plain sentence (required)"},
			{name: "--repo", arg: "path", desc: "Repository to search (default: current repo)"},
			{name: "--top-k", arg: "n", def: "10", desc: "Number of results to return"},
			{name: "--format", arg: "text|json|ndjson|agent", def: "json", desc: "Output format; text is tiered for reading, agent is compact"},
			{name: "--head", desc: "Search the committed tree (cached) instead of the working tree"},
			{name: "--profile", arg: "syntax-only|fast|full", def: "fast", desc: "Parsing depth; use full for bug-fix/locate (call-graph active)"},
			{name: "--deep", desc: "Also run the exhaustive BM25 pass and fuse it (slower)"},
			{name: "--single-resolution", desc: "One result per ranked unit; do not spend spare slots on finer regions of a prose document"},
			{name: "--document-resolution", desc: "Rank a prose document as one unit; do not rank its sections separately"},
			{name: "--max-context-bytes", arg: "n", def: "24576", desc: "Output byte budget; 0 = unbounded"},
			{name: "--reference-blocks", arg: "all|container-map,signature-types,type-card", desc: "Turn reference blocks back on (off by default)"},
			{name: "--max-indexed-files", arg: "n", desc: "Bound cold-search parsing to N files"},
			{name: "--index-all-files", desc: "Widen cold-search parsing to every file"},
			{name: "--cache-dir", arg: "path", desc: "Override the committed-tree cache directory"},
			{name: "--no-cache", desc: "Disable the committed-tree cache"},
		},
		examples: []string{
			`entire graph search --repo . --query "token refresh returns 401" --format text --top-k 8`,
			`entire graph search --repo . --query "csv export ordering" --profile full`,
		},
	},
	{
		name:    "neighbors",
		group:   groupInspect,
		summary: "Who calls this / what does it call (targeted relations)",
		usage:   []string{"entire graph neighbors --symbol NAME|<file>:<line> --repo . [--relation CALLS] [--direction both|in|out] [--depth 1|2] [--limit 20]"},
		long: "Direct incoming/outgoing relations for one symbol, with definition locations, plus bounded two-hop paths at --depth 2. Use it when you want one specific relation/direction; for the full blast radius of a change, prefer impact.\n\n" +
			"Select a definition with --symbol <file>:<line>, or --symbol NAME plus --file/--line/--kind when a name is ambiguous. The ambiguity error prints the exact selector for each definition it found.",
		flags: []flagDoc{
			{name: "--symbol", arg: "NAME|<file>:<line>", desc: "Symbol to inspect (required)"},
			{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
			{name: "--file", arg: "path", desc: "Disambiguate an ambiguous name by file"},
			{name: "--line", arg: "n", desc: "Disambiguate by line"},
			{name: "--kind", arg: "kind", desc: "Disambiguate by symbol kind"},
			{name: "--relation", arg: "CALLS", def: "CALLS", desc: "Relation family to follow"},
			{name: "--direction", arg: "both|in|out", def: "both", desc: "in = callers, out = callees"},
			{name: "--depth", arg: "1|2", def: "1", desc: "Hops to traverse"},
			{name: "--limit", arg: "n", def: "20", desc: "Max neighbors per direction"},
			{name: "--format", arg: "json|text|agent", desc: "Output format"},
			{name: "--max-context-bytes", arg: "n", def: "16384", desc: "Output byte budget for agent format"},
			{name: "--internal-only", desc: "Drop unresolved external endpoints"},
			{name: "--exclude-tests", desc: "Drop test-only neighbors"},
			{name: "--head", desc: "Query the committed tree (cached)"},
			{name: "--profile", arg: "syntax-only|fast|full", def: "full", desc: "Parsing depth"},
		},
		examples: []string{
			"entire graph neighbors --repo . --symbol validateToken --relation CALLS --direction in",
			"entire graph neighbors --repo . --symbol validateToken --direction out --depth 2",
		},
	},
	{
		name:    "impact",
		group:   groupInspect,
		summary: "One-shot blast radius for changing a symbol",
		usage:   []string{"entire graph impact --symbol NAME|<file>:<line> --repo . [--depth 1|2] [--limit 15] [--format text|json]"},
		long: "Everything the graph knows about changing one symbol in a single bounded explanation: direct + transitive callers (depth <=2), callees, type consumers (USES_TYPE/PARAM_TYPE/RETURNS_TYPE), data flows, files that historically co-change with the symbol's file, and same-container siblings. Run this before changing a function/type's behavior.\n\n" +
			"Ambiguous names return the definition list; rerun with --file/--line/--kind (or --symbol <file>:<line>) to pick one.",
		flags: []flagDoc{
			{name: "--symbol", arg: "NAME|<file>:<line>", desc: "Symbol to analyze (required)"},
			{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
			{name: "--file", arg: "path", desc: "Disambiguate an ambiguous name by file"},
			{name: "--line", arg: "n", desc: "Disambiguate by line"},
			{name: "--kind", arg: "kind", desc: "Disambiguate by symbol kind"},
			{name: "--depth", arg: "1|2", def: "2", desc: "Caller-traversal depth"},
			{name: "--limit", arg: "n", def: "15", desc: "Max entries per section"},
			{name: "--format", arg: "text|json", desc: "Output format"},
			{name: "--max-context-bytes", arg: "n", def: "4096", desc: "Total text budget"},
			{name: "--exclude-tests", desc: "Drop test-only entries"},
			{name: "--head", desc: "Query the committed tree (cached)"},
			{name: "--profile", arg: "syntax-only|fast|full", def: "full", desc: "Parsing depth"},
		},
		examples: []string{"entire graph impact --repo . --symbol WriteText"},
	},
	{
		name:    "def",
		group:   groupInspect,
		summary: "What a name IS: declaration, fields, and method surface",
		usage:   []string{"entire graph def NAME|<file>:<line> --repo . [--members 15] [--format text|json]"},
		long:    "Structural declaration lookup: what a name is and, for a type, its fields and associated-function/method surface, drawn from the graph's own membership edges (inherent impl blocks, receiver methods, extension members, partial parts, one hop of trait/module/base acquisition). It reports a method's owning type, and for a trait/interface it reports who implements it. Membership is never inferred from a name resembling another name.",
		args: []argDoc{
			{name: "NAME|<file>:<line>", desc: "Symbol name, or a file:line selector"},
		},
		flags: []flagDoc{
			{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
			{name: "--file", arg: "path", desc: "Disambiguate an ambiguous name by file"},
			{name: "--line", arg: "n", desc: "Disambiguate by line"},
			{name: "--kind", arg: "kind", desc: "Disambiguate by symbol kind"},
			{name: "--members", arg: "n", def: "15", desc: "Max members listed per group"},
			{name: "--format", arg: "text|json", desc: "Output format"},
			{name: "--max-context-bytes", arg: "n", def: "4096", desc: "Total text budget"},
			{name: "--head", desc: "Query the committed tree (cached)"},
		},
		examples: []string{"entire graph def Result --repo ."},
	},
	{
		name:    "explain",
		group:   groupInspect,
		summary: "Resolve symbols named by a failing build or test",
		usage: []string{
			"entire graph explain --repo . [--profile full] [--format text|json|agent] [--max-symbols 8] [--max-context-bytes 2048]",
		},
		long: "Reads failing build or test output from stdin, passes that output through by default, and appends declarations for the symbols named by the failure. It queries the working tree by default so the declarations match the code that just failed. The command is local and deterministic; it does not call a model or network service.",
		flags: []flagDoc{
			{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
			{name: "--profile", arg: "syntax-only|fast|full", def: "full", desc: "Parsing depth"},
			{name: "--cache-dir", arg: "path", desc: "Override the committed-tree cache directory"},
			{name: "--format", arg: "text|json|agent", def: "text", desc: "Output format for resolved declarations"},
			{name: "--head", desc: "Resolve declarations from HEAD instead of the working tree"},
			{name: "--worktree", desc: "Resolve declarations from the working tree (default)"},
			{name: "--no-cache", desc: "Disable the committed-tree cache"},
			{name: "--no-echo", desc: "Do not pass the input build output through before declarations"},
			{name: "--max-symbols", arg: "n", def: "8", desc: "Maximum distinct symbol names to resolve"},
			{name: "--max-context-bytes", arg: "n", def: "2048", desc: "Byte budget for the declaration block"},
			{name: "--ignore-file", arg: "path", desc: "Extra gitignore-style exclude rules (repeatable)"},
			{name: "--include-file", arg: "path", desc: "Re-include ignored paths (gitignore-style; not an allowlist)"},
		},
		examples: []string{
			`( o=$(go test ./internal/configs -run '^TestX$' 2>&1); r=$?; printf '%s\n' "$o" | entire graph explain --repo .; exit $r )`,
		},
	},
	{
		name:     "symbols",
		group:    groupInspect,
		summary:  "Stream every symbol definition (bulk NDJSON)",
		usage:    []string{"entire graph symbols --repo . --format ndjson [--worktree]"},
		long:     "A bulk NDJSON stream of every symbol record (stable compound-v1 ID, kind, qualified name, source range, signature, language, container). There is no name argument — grep the stream client-side, or prefer search/neighbors for a single lookup. The trailing summary record carries aggregate stats and completeness.",
		flags:    providerFlagDocs,
		examples: []string{"entire graph symbols --repo . --format ndjson"},
	},
	{
		name:    "edges",
		group:   groupInspect,
		summary: "Stream every relation (bulk NDJSON)",
		usage:   []string{"entire graph edges --repo . --format ndjson [--worktree] [--to ID|NAME] [--from ID|NAME] [--relation TYPE[,TYPE...]]"},
		long:    "A bulk NDJSON stream of every relation record across all relation types (CALLS, IMPORTS, EXTENDS, HANDLES_ROUTE, ...), each tagged with resolution and confidence. For one symbol's callers/callees use neighbors instead. The trailing summary record carries aggregate stats and completeness.",
		flags: append([]flagDoc{
			{name: "--to", arg: "ID|NAME", desc: "Keep relations whose destination matches a stable ID or trailing symbol name"},
			{name: "--from", arg: "ID|NAME", desc: "Keep relations whose source matches a stable ID or trailing symbol name"},
			{name: "--relation", arg: "TYPE[,TYPE...]", desc: "Keep one or more relation types (case-insensitive)"},
		}, providerFlagDocs...),
		examples: []string{
			"entire graph edges --repo . --format ndjson",
			"entire graph edges --repo . --to validateToken --relation CALLS",
		},
	},
	{
		name:    "snapshot",
		group:   groupInspect,
		summary: "Export the whole graph: files, symbols, and relations",
		usage:   []string{"entire graph snapshot --repo . --format ndjson|compact-ndjson|scip [--worktree]"},
		long:    "One header record, then file, external-endpoint, symbol, and relation records. Native and compact NDJSON stream so memory stays bounded; the experimental scip format assembles one complete protobuf Index in memory. The snapshot is a superset of symbols + edges + files for ingestion into agent memory or a store such as Entire Brain. compact-ndjson can be read by snapshot-query. scip reserves stderr for one machine-readable omission note and cannot be combined with --progress.",
		flags:   snapshotFlagDocs,
		examples: []string{
			"entire graph snapshot --repo . --format ndjson",
			"entire graph snapshot --repo . --format compact-ndjson > graph.compact.ndjson",
			"entire graph snapshot --repo . --format scip > index.scip",
		},
	},
	{
		name:    "snapshot-query",
		group:   groupInspect,
		summary: "Query a compact snapshot without rebuilding the graph",
		usage: []string{
			"entire graph snapshot-query --input graph.compact.ndjson --symbol NAME [--format ndjson]",
			"entire graph snapshot-query --input graph.compact.ndjson --from STABLE_ID [--relation TYPE] [--format ndjson]",
		},
		long: "Loads a compact-ndjson v1 snapshot from disk and emits deterministically ordered native NDJSON symbol or relation records. At least one of --symbol or --from is required; --relation narrows a --from query and cannot be used alone.",
		flags: []flagDoc{
			{name: "--input", arg: "path", desc: "Compact snapshot file to query (required)"},
			{name: "--symbol", arg: "name", desc: "Return symbols matching a stable ID, qualified name, or name"},
			{name: "--from", arg: "stable-id", desc: "Return relations whose source has this stable ID"},
			{name: "--relation", arg: "type", desc: "Restrict a --from query to one relation type"},
			{name: "--format", arg: "ndjson", def: "ndjson", desc: "Required output format"},
		},
		examples: []string{
			"entire graph snapshot-query --input graph.compact.ndjson --symbol Cache.Refresh --format ndjson",
			"entire graph snapshot-query --input graph.compact.ndjson --from '<stable-id>' --relation CALLS --format ndjson",
		},
	},

	// ── Analyze changes & more ───────────────────────────────────────────
	{
		name:    "commit",
		group:   groupAnalyze,
		summary: "Entity-level change list for a commit vs its first parent",
		usage:   []string{"entire graph commit [rev] [--json] [--progress] [--max-seconds n] [--repo path]"},
		long:    "Entity-level change list (added / removed / renamed / signature-changed / body-changed) with a heuristic dependent count, so a signature change with many dependents stands out. Stops cleanly after --max-seconds and emits the partial result with W_ANALYSIS_BUDGET_EXCEEDED warnings listing what was skipped.",
		args: []argDoc{
			{name: "rev", desc: "Commit to analyze (default: HEAD)"},
		},
		flags:    commonFlagDocs,
		examples: []string{"entire graph commit HEAD --json"},
	},
	{
		name:    "diff",
		group:   groupAnalyze,
		summary: "Entity-level change list between two refs",
		usage:   []string{"entire graph diff --base <rev> --head <rev> [--json] [--progress] [--max-seconds n] [--repo path] [-- path...]"},
		long:    "Entity-level change list between two refs, with heuristic dependent counts. High dependent counts on a signature change mean run tests first. `analyze` is an alias for this command. Stops cleanly after --max-seconds and emits the partial result with W_ANALYSIS_BUDGET_EXCEEDED warnings.",
		flags: append([]flagDoc{
			{name: "--base", arg: "rev", def: "HEAD~1", desc: "Base revision"},
			{name: "--head", arg: "rev", def: "HEAD", desc: "Head revision"},
		}, commonFlagDocs...),
		examples: []string{"entire graph diff --base main --head HEAD --json"},
	},
	{
		name:    "analyze",
		group:   groupAnalyze,
		aliasOf: "diff",
		hidden:  true,
	},
	{
		name:    "checkpoint",
		group:   groupAnalyze,
		summary: "Analyze the commit behind an Entire-Checkpoint trailer",
		usage:   []string{"entire graph checkpoint <checkpoint-id> [--json] [--repo path]"},
		long:    "Runs the entity-level change analysis for the commit referenced by an Entire-Checkpoint trailer. Useful when reviewing whether a checkpoint is safe to keep, revert, or continue.",
		args: []argDoc{
			{name: "<checkpoint-id>", desc: "The checkpoint ID to analyze"},
		},
		flags: []flagDoc{
			{name: "--json", desc: "Emit the result as JSON"},
			{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
		},
		examples: []string{"entire graph checkpoint abc123 --json"},
	},
	{
		name:    "risk",
		group:   groupAnalyze,
		summary: "Classify a changeset's graph-derived risk and recommend tests",
		usage:   []string{"entire graph risk [--base <rev> --head <rev> | --checkpoint <id>] [--format text|json] [--max-entities n] [--repo path]"},
		long:    "Composes semantic diff and per-entity impact analysis into a bounded changeset risk report. Graph evidence (callers, type consumers, and locations) is printed separately from the deterministic risk inference. Test recommendations come only from graph caller and co-change paths and must be verified by running them.",
		flags: []flagDoc{
			{name: "--base", arg: "rev", def: "HEAD~1", desc: "Base revision"},
			{name: "--head", arg: "rev", def: "HEAD", desc: "Head revision"},
			{name: "--checkpoint", arg: "id", desc: "Analyze the semantic diff associated with an Entire checkpoint"},
			{name: "--format", arg: "text|json", def: "text", desc: "Output format"},
			{name: "--max-entities", arg: "n", def: "5", desc: "Maximum changed entities to impact-analyze"},
			{name: "--profile", arg: "full|fast|syntax-only", def: "full", desc: "Graph snapshot profile"},
			{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
		},
		examples: []string{"entire graph risk --repo . --base HEAD~1 --head HEAD", "entire graph risk --checkpoint abc123 --format json"},
	},
	{
		name:    "verify",
		group:   groupAnalyze,
		summary: "Run a test command and return an adjudicated verdict, not test output",
		usage:   []string{`entire graph verify --test "<cmd>" --repo . [--setup "<cmd>"] [--record-baseline path | --pre-edit-baseline path] [--test-failure-exit-code n] [--max-bytes 2048]`},
		long: "verify runs your test command and reports WHICH TESTS CHANGED rather than what the runner printed: which newly pass, which newly fail, and which were ALREADY failing before the edit (labelled PRE-EXISTING). Raw runner output is never forwarded — ids are, text is not — and id lists cap at 20 with a count.\n\n" +
			"Record a baseline on the pristine tree first (--record-baseline), then pass that file as --pre-edit-baseline after editing. Without a baseline the verdict is a state rather than a delta, so a failure that predates the change cannot be labelled as one.\n\n" +
			"Parsers: pytest, jest/vitest, cargo test, go test, phpunit, rspec, minitest, maven/gradle surefire, ctest. An unrecognised format degrades to an exit-code-only verdict and says so.\n\n" +
			"For configured runner failure statuses (for example Jest testFailureExitCode or RSpec failure_exit_code), pass --test-failure-exit-code on both baseline recording and comparison. This declares the status; it does not configure the runner. Parsed failures must still be present, and missing tests or unbuilt targets still make verification incomplete. A shell-reported signal can share the declared number, so the declaration cannot distinguish those cases.",
		flags: []flagDoc{
			{name: "--test", arg: "cmd", desc: "The test command to run (required)"},
			{name: "--repo", arg: "path", desc: "Repository to run in (default: current repo)"},
			{name: "--setup", arg: "cmd", desc: "Command run before the tests; its output never contributes test ids"},
			{name: "--record-baseline", arg: "path", desc: "Write the pristine-tree result to this file instead of adjudicating"},
			{name: "--pre-edit-baseline", arg: "path", desc: "Diff this run against a previously recorded baseline"},
			{name: "--test-failure-exit-code", arg: "n", desc: "Declare the runner's failure status (1-255), overriding automatic exit-code rules; use the same value when recording and comparing"},
			{name: "--max-bytes", arg: "n", def: "2048", desc: "Cap the rendered verdict; the verdict clause always survives"},
		},
		examples: []string{
			`entire graph verify --repo . --test "pytest tests/test_parser.py" --record-baseline /tmp/base.json`,
			`entire graph verify --repo . --test "pytest tests/test_parser.py" --pre-edit-baseline /tmp/base.json`,
		},
	},
	{
		name:    "stats",
		group:   groupAnalyze,
		summary: "Estimated tokens the graph saved (one line; --verbose for the full report)",
		usage:   []string{"entire graph stats [--repo .] [--since 30d|7d|all] [--verbose] [--format text|json] [--sessions-dir path|--transcript path]"},
		long: "A local, read-only report for humans (agents should not run it as part of a task). It reads the coding-agent session transcripts already on disk (~/.claude/projects/<path-slug>/*.jsonl).\n\n" +
			"By DEFAULT it prints one line: the ESTIMATED tokens saved, marked with ~ because it is a model, not a measurement. --verbose restores the full report — graph calls per verb vs exploration calls, bytes each pulled into context, billed tokens, a graph-first rate, the measured per-call costs, and the model's assumption printed with the number.\n\n" +
			"The estimate credits each graph locate call (search/neighbors/impact) with the ONE exploration call it displaced, priced from that session's own measured bytes per graph call and bytes per exploration call. A session whose graph calls returned more per call than the exploration they displaced correctly contributes nothing.\n\n" +
			"--transcript narrows the whole report to ONE session (that transcript plus its subagent transcripts) instead of a whole project directory. Summaries of unchanged transcripts are memoised under the cache directory, keyed on file identity; --no-cache turns that off.",
		flags: []flagDoc{
			{name: "--repo", arg: "path", desc: "Repository whose sessions to report on (default: current repo)"},
			{name: "--since", arg: "30d|7d|all", def: "30d", desc: "Lookback window (<n>h|<n>d|<n>w, or all)"},
			{name: "--verbose", desc: "Print the full report instead of the single savings line"},
			{name: "--format", arg: "text|json", def: "text", desc: "Output format (json is unaffected by --verbose)"},
			{name: "--sessions-dir", arg: "path", desc: "Override the transcript lookup directory"},
			{name: "--transcript", arg: "path", desc: "Report on a single session transcript"},
			{name: "--cache-dir", arg: "path", desc: "Where to memoise per-transcript summaries"},
			{name: "--no-cache", desc: "Re-parse every transcript instead of reusing the memo"},
		},
		examples: []string{"entire graph stats --repo .", "entire graph stats --repo . --since 7d --verbose"},
	},

	// ── Help & diagnostics ───────────────────────────────────────────────
	{
		name:     "help",
		group:    groupMeta,
		summary:  "Show this command listing",
		usage:    []string{"entire graph help"},
		long:     "Prints the grouped command listing. Run `entire graph <command> --help` for details on any single command.",
		examples: []string{"entire graph help"},
	},
	{
		name:    "doctor",
		group:   groupMeta,
		summary: "Diagnose the environment and confirm no-egress",
		usage:   []string{`entire graph doctor [--json] [--assert "<command line>"]`},
		long: "Reports the resolved repo, the Entire environment variables, plugin-data-dir writability, and confirms no_egress=true (no remote fetches, hosted APIs, telemetry, or grammar downloads).\n\n" +
			"--assert is the preflight: it parses a command line against this binary and exits non-zero if this binary would reject it, WITHOUT running anything — no repo read, no index build, no writes. Run it once before a batch or a benchmark cell so a flag set built for a different build fails at startup instead of failing the first call of every session and leaving an agent to explore by hand. Repeatable. It runs each command's real parser, so a command that requires a flag will say so; assert the command line you actually intend to run.",
		flags: []flagDoc{
			{name: "--json", desc: "Emit the report as JSON"},
			{name: "--assert", arg: "cmdline", desc: "Verify this binary accepts a command line, without running it (repeatable)"},
		},
		examples: []string{
			"entire graph doctor --json",
			`entire graph doctor --assert "search --profile full --top-k 10 --format text"`,
		},
	},
	{
		name:    "version",
		group:   groupMeta,
		summary: "Print the plugin version (and provider name with --json)",
		usage:   []string{"entire graph version [--json]"},
		long:    "Prints the plugin version. With --json it also reports the provider name.",
		flags: []flagDoc{
			{name: "--json", desc: "Emit provider name and version as JSON"},
		},
		examples: []string{"entire graph version --json"},
	},
}

// providerFlagDocs are shared by the snapshot/symbols/edges bulk-stream commands.
var providerFlagDocs = []flagDoc{
	{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
	{name: "--format", arg: "ndjson", def: "ndjson", desc: "Required output format"},
	{name: "--worktree", desc: "Stream the working tree instead of HEAD (never cached)"},
	{name: "--progress", desc: "Emit progress events to stderr (not available with scip)"},
	{name: "--profile", arg: "syntax-only|fast|full", def: "full", desc: "Parsing depth"},
	{name: "--ignore-file", arg: "path", desc: "Extra gitignore-style exclude rules (repeatable)"},
	{name: "--include-file", arg: "path", desc: "Re-include ignored paths (gitignore-style; not an allowlist)"},
}

// snapshotFlagDocs extend the bulk provider flags with complete-snapshot formats.
var snapshotFlagDocs = []flagDoc{
	{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
	{name: "--format", arg: "ndjson|compact-ndjson|scip", def: "ndjson", desc: "Output format; compact and scip require a complete snapshot"},
	{name: "--worktree", desc: "Stream the working tree instead of HEAD (never cached)"},
	{name: "--progress", desc: "Emit progress events to stderr"},
	{name: "--profile", arg: "syntax-only|fast|full", def: "full", desc: "Parsing depth"},
	{name: "--ignore-file", arg: "path", desc: "Extra gitignore-style exclude rules (repeatable)"},
	{name: "--include-file", arg: "path", desc: "Re-include ignored paths (gitignore-style; not an allowlist)"},
}

// commonFlagDocs are shared by the commit/diff/analyze change-analysis commands.
var commonFlagDocs = []flagDoc{
	{name: "--json", desc: "Emit the result as JSON"},
	{name: "--progress", desc: "Emit progress events to stderr"},
	{name: "--max-seconds", arg: "n", def: "120", desc: "Analysis budget; 0 = unlimited"},
	{name: "--repo", arg: "path", desc: "Repository (default: current repo)"},
}

// unexpectedArgumentsError explains an argument the parser did not recognise, and says the one
// thing that is usually true when the argument is flag-shaped: this binary is older than whatever
// wrote the command line.
//
// The old message ("search received unexpected arguments: --callee-hop") describes the symptom and
// not the cause, and the difference matters because of WHO reads it. A harness driving a flag set
// built for a newer binary gets an exit 1 and an empty payload on every single call, for the whole
// run — the agent's first mandated action fails, it falls back to exploring by hand, and the cell
// silently measures a graph arm that never reached the graph. Naming the version turns a run that
// looks broken into a run that reads as version skew, at the first call rather than the last.
//
// A positional argument keeps the plain wording: `search foo` is a typo, not a stale deploy, and
// telling its author about binary versions would be noise.
func unexpectedArgumentsError(command, version string, rest []string) error {
	var flags []string
	for _, arg := range rest {
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
		}
	}
	if len(flags) == 0 {
		return fmt.Errorf("%s received unexpected arguments: %s", command, strings.Join(rest, " "))
	}
	return fmt.Errorf(
		"%s does not accept %s in entire-graph %s: this binary may be older than the caller that built the command line; run \"entire graph %s --help\" for the flags it does accept (unexpected: %s)",
		command, strings.Join(flags, " "), versionOrUnknown(version), command, strings.Join(rest, " "))
}

// versionOrUnknown keeps the message honest when the binary was built without a version stamped in:
// printing an empty string beside "entire-graph" reads as a version rather than the absence of one.
func versionOrUnknown(version string) string {
	if strings.TrimSpace(version) == "" {
		return "(unknown version)"
	}
	return version
}

// findCommandDoc returns the doc for a command name, resolving aliases.
func findCommandDoc(name string) (commandDoc, bool) {
	for _, d := range commandDocs {
		if d.name == name {
			if d.aliasOf != "" {
				return findCommandDoc(d.aliasOf)
			}
			return d, true
		}
	}
	return commandDoc{}, false
}

// wantsHelp reports whether the args request help for the command documented by doc.
//
// It reads the args the way the command's own parser will, because a flat scan for the two spellings
// cannot tell a request for help from DATA that happens to be spelled like one. Two ways it got that
// wrong: `search --query --help` is a search for the literal text "--help" and printed help instead,
// and `diff -- --help` addresses a path named `--help` — after the separator every remaining argument
// is positional by definition, so nothing there can be a flag at all.
//
// The value-taking flags come from the doc registry rather than a second list, because that registry
// is already the thing this file renders and the command parsers are already checked against it
// (agentguide_test.go). A hand-copied list here would be a third spelling of the same fact, and the
// one nothing would notice going stale.
func wantsHelp(doc commandDoc, args []string) bool {
	valued := make(map[string]bool, len(doc.flags))
	for _, flag := range doc.flags {
		if flag.arg != "" {
			valued[flag.name] = true
		}
	}
	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == "--":
			return false
		case valued[args[index]]:
			// The next argument is this flag's VALUE, whatever it is spelled like.
			index++
		case args[index] == "--help" || args[index] == "-h":
			return true
		}
	}
	return false
}

// renderRootHelp prints the grouped, two-column command listing.
func renderRootHelp(w io.Writer) {
	fmt.Fprintln(w, intro)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  entire graph <command> [flags]")

	// Pad command names to a shared width for aligned summaries.
	width := 0
	for _, d := range commandDocs {
		if d.hidden {
			continue
		}
		if len(d.name) > width {
			width = len(d.name)
		}
	}

	for _, g := range groupOrder {
		fmt.Fprintf(w, "\n%s:\n", groupTitles[g])
		for _, d := range commandDocs {
			if d.hidden || d.group != g {
				continue
			}
			fmt.Fprintf(w, "  %-*s   %s\n", width, d.name, d.summary)
		}
	}

	fmt.Fprintln(w, "\nUse \"entire graph <command> --help\" for details on a command.")
	fmt.Fprintln(w, "For coding agents: run \"entire graph agent-guide\" or \"entire graph init-agents\".")
}

// renderCommandHelp prints the detail view for one command.
func renderCommandHelp(w io.Writer, name string) {
	doc, ok := findCommandDoc(name)
	if !ok {
		renderRootHelp(w)
		return
	}

	fmt.Fprintf(w, "%s\n", doc.summary)

	if len(doc.usage) > 0 {
		fmt.Fprintln(w, "\nUsage:")
		for _, u := range doc.usage {
			fmt.Fprintf(w, "  %s\n", u)
		}
	}

	if doc.long != "" {
		fmt.Fprintf(w, "\n%s\n", doc.long)
	}

	if len(doc.args) > 0 {
		fmt.Fprintln(w, "\nArguments:")
		width := columnWidth(argLabels(doc.args))
		for _, a := range doc.args {
			writeTwoColumn(w, a.name, a.desc, width)
		}
	}

	if len(doc.flags) > 0 {
		fmt.Fprintln(w, "\nFlags:")
		labels := flagLabels(doc.flags)
		width := columnWidth(labels)
		for i, f := range doc.flags {
			desc := f.desc
			if f.def != "" {
				desc = fmt.Sprintf("%s (default: %s)", desc, f.def)
			}
			writeTwoColumn(w, labels[i], desc, width)
		}
	}

	if len(doc.examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, e := range doc.examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
}

// maxLabelWidth caps the flag/argument column so one long placeholder does not
// push every description far to the right. Labels wider than this get their
// description on the next line (see writeTwoColumn).
const maxLabelWidth = 26

// flagLabel renders the left column for a flag: "--name <arg>", or "--name arg"
// when the placeholder already carries its own delimiters (e.g. NAME|<file>:<line>),
// or just "--name" for a boolean flag.
func flagLabel(f flagDoc) string {
	switch {
	case f.arg == "":
		return f.name
	case strings.ContainsAny(f.arg, "<>|"):
		return f.name + " " + f.arg
	default:
		return f.name + " <" + f.arg + ">"
	}
}

// writeTwoColumn prints "  label   desc", padding label to width. When the label
// is wider than width (an over-long placeholder), the description drops to the
// next line, indented to the column, so the description column stays aligned.
func writeTwoColumn(w io.Writer, label, desc string, width int) {
	if len(label) > width {
		fmt.Fprintf(w, "  %s\n  %-*s   %s\n", label, width, "", desc)
		return
	}
	fmt.Fprintf(w, "  %-*s   %s\n", width, label, desc)
}

func flagLabels(flags []flagDoc) []string {
	labels := make([]string, len(flags))
	for i, f := range flags {
		labels[i] = flagLabel(f)
	}
	return labels
}

func argLabels(args []argDoc) []string {
	labels := make([]string, len(args))
	for i, a := range args {
		labels[i] = a.name
	}
	return labels
}

// columnWidth returns the label column width: the longest label, capped at
// maxLabelWidth so a single outlier does not widen the whole column.
func columnWidth(labels []string) int {
	width := 0
	for _, l := range labels {
		if n := len(l); n > width && n <= maxLabelWidth {
			width = n
		}
	}
	if width > maxLabelWidth {
		width = maxLabelWidth
	}
	return width
}

// commandNames returns the names of all listed (non-hidden) commands. Kept for
// use by tests that assert the root listing is complete.
func commandNames() []string {
	var names []string
	for _, d := range commandDocs {
		if !d.hidden {
			names = append(names, d.name)
		}
	}
	return names
}
