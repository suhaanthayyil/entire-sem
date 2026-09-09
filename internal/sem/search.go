package sem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/termsafe"
)

const (
	// Keep default search responses compact enough for agent context. Larger top-k and snippet
	// defaults produced ~15KB responses that are repeatedly carried through later turns; this
	// output-budget choice is not evidence of end-to-end task savings.
	defaultSearchTopK              = 10
	defaultSearchContextLines      = 8
	defaultSearchMaxRegionLines    = 80
	defaultSearchMaxSnippetLines   = 6
	defaultSearchMaxRegionsPerFile = 3
	defaultSearchMaxIndexedFiles   = 96
	deepSearchMaxIndexedFiles      = 256
	searchIndexedFilesPerResult    = 3
	maxSearchQueryTerms            = 48
	minGitGrepPreselectionFiles    = 10_000
	sparseSearchChunkStrideLines   = 60
	sparseSearchRRFConstant        = 60
	sparseSearchFileRankWeight     = 2
	deepSearchSparseNumerator      = 5
	deepSearchSparseDenominator    = 8
	deepSearchSemanticHeadDivisor  = 32
	maxDeepSearchSemanticHead      = 10
	maxSparseSearchQueryTerms      = 64
	maxSparseSearchFileBytes       = 2 * 1024 * 1024
	maxSearchContentCacheBytes     = 32 * 1024 * 1024
	maxSearchContentCacheFileBytes = 2 * 1024 * 1024
	searchDiversityRelevanceRatio  = 0.75
	maxSearchGitGrepPatterns       = 4096
)

// SearchOptions controls local issue-to-code retrieval. Search reads the same
// HEAD/worktree view and ignore rules as provider snapshots.
type SearchOptions struct {
	Worktree          bool
	IgnoreFiles       []string
	IncludeFiles      []string
	Profile           Profile
	TopK              int
	ContextLines      int
	MaxRegionLines    int
	MaxSnippetLines   int
	MaxRegionsPerFile int
	MaxParseBytes     int
	CacheDir          string
	DisableCache      bool
	MaxIndexedFiles   int
	IndexAllFiles     bool
	MaxContextBytes   int
	// OmitsRepoIgnoreDisclosureFloor says this caller's renderer never prints the
	// repo-ignore disclosure floor, so the ranking must not be charged for it.
	//
	// The floor is funded from INSIDE MaxContextBytes (see the reservation in
	// SearchRepository), and exactly one renderer emits it: `--format text`, which
	// has no warning channel of its own. The structured formats carry the same
	// disclosure as data — the W_REPO_IGNORED_SOURCE warning and the whole
	// repo_ignored report — whether or not the ranking pays for a floor, so an
	// unconditional reservation only deleted ranked source from them. Measured on a
	// fixture with one excluded file: at --max-context-bytes 600 the reservation cost
	// a JSON payload its ONLY result (result_bytes 589 -> 0) and bought that payload
	// nothing, because JSON never renders the floor those bytes were held for.
	//
	// The zero value is the conservative one: a caller that does not say what it
	// renders is assumed to render the floor, so the disclosure can never end up
	// unfunded by omission.
	OmitsRepoIgnoreDisclosureFloor bool
	// progressivePreselection is internal policy, not a caller override. The
	// standard cold default may widen adaptively; explicit and TopK-adaptive
	// MaxIndexedFiles values remain exact compatibility limits.
	progressivePreselection bool
	// afterCachePolicyCapture is a deterministic test seam for policy-file
	// mutation between transaction capture and the first cache lookup. It is
	// deliberately unexported and nil in production.
	afterCachePolicyCapture func()
	// afterPreindexLoad is a deterministic test seam for repository-identity
	// mutation between a complete cache lookup and source preselection. It is
	// deliberately unexported and nil in production.
	afterPreindexLoad func()
	// BodyHeadRanks caps how deep the COMPLETE-BODY upgrade reaches, independently of the
	// locator head. 0 means the built-in depth (searchEnclosureHeadRanks). It may only narrow
	// the head, never widen it, so the growth allowance stays sized for the bodies it funds.
	BodyHeadRanks int
	// MEASURED SESSION RESULT — READ BEFORE ENABLING. On haiku 30 with the prompt and the baseline
	// block held constant, turning this on together with --top-k 20 cost +18.9 pt of total_tokens,
	// +17.7 pt of turns and +14.6 pt of billed_new against the same cell with shipped defaults
	// (-23.5% -> -4.6% total_tokens). eg's turns rose 32.10 -> 35.37 against an unchanged baseline.
	// A separate resolution-anchored evaluation found the same combination cost CORRECTNESS:
	// the terser edit-from-payload path stops short of a complete fix on harder tasks.
	//
	// The $0 payload screen that cleared it (gold recall 22->26/30, code-less gold 8->6, no instance
	// regressing) was measuring the PAYLOAD, not the SESSION. A payload-shape win is not evidence of a
	// session win -- that has now been measured four times, and this is the first time it actively hurt.
	//
	// It stays here, opt-in and default 0, because the mechanism is sound and a narrower use may pay.
	// Do not enable it in a measured cell without re-running the attribution.
	// HeadWindowLines makes a HEAD rank that has no enclosable callable come back as a bounded
	// read window of this many lines instead of a two-line locator. 0 leaves ordinary code
	// search off and lets native prose-parent retrieval use its measured default.
	//
	// Measured over 79 agent sessions: the gold file was in the payload as a LOCATOR ONLY in 20 of
	// 74 sessions (24 of them at rank <= 3), and 61 post-search Reads targeted a ranked file that
	// carried no code at all. Those reads cost a turn (~17,000 tokens) to recover what ~2 kB of
	// window would have carried.
	HeadWindowLines int
	// IncludeFileOutline emits the FILE OUTLINE block: the other symbols in the files the payload
	// already named, as an index with no source. Off by default.
	//
	// Measured over 79 agent sessions: 77 targeted greps landed on a file the payload had already
	// named, 35 of them immediately followed by a Read of that same file, and the reads that follow a
	// payload file land a median 109 lines from anything printed — 47 of 53 inside another symbol the
	// graph already indexes in that file.
	// VerifyExplainCommand, when set, is appended to the emitted VERIFY line as
	// `<test cmd> 2>&1 | <this>`, so an agent that runs VERIFY verbatim also gets the declarations
	// for whatever the failure names. Measured: 0 of 27 agents typed that pipe when the PROMPT asked
	// them to, while 79 VERIFY-style commands were run.
	VerifyExplainCommand string
	IncludeFileOutline   bool
	// FullUnitTop makes the first N ranks come back as their COMPLETE enclosing unit — function,
	// method, or type/container declaration — bypassing every condition the opportunistic body
	// upgrade applies (see the editability comment in search_enclosure.go). 0 = off, and off is
	// byte-for-byte today's payload.
	//
	// It is the editability lever, and editability is a different measurement from recall: on the
	// R30PUB benchmark payloads the share of needed edits whose replaced text appears VERBATIM in
	// the payload is ~11%, while the gold FILE is usually ranked. The misses are the three
	// suppression conditions (no enclosable callable, the 160-line cap, and no demotable tail at
	// small --top-k), not the ranking.
	//
	// N >= 2 reaches rank 2 only when rank 2's score is still within searchFullUnitGapRatio of
	// rank 1's: one forced unit per genuinely ambiguous answer, not N bodies per search.
	FullUnitTop int
	// VerifyPrefix is a decorator baked into the emitted VERIFY command, after any `cd <dir> &&` the
	// derivation added, so a harness can grep its own token out of a transcript.
	VerifyPrefix string
	// VerifyPreFixStatus is one caller-computed line rendered verbatim as `PRE-FIX:` under the emitted
	// command, capped at searchVerifyPreFixStatusMaxBytes.
	VerifyPreFixStatus string
	// CalleeHop admits the top hit's OUTGOING CALLS targets as candidate fix sites — up to three,
	// same-repo and resolved only. Off by default.
	//
	// It is the answer to what --full-unit-top could not reach. Measured over eight R30PUB instances
	// with the unit levers on, 8 of 12 code-gold files were ranked and 0 carried the gold hunk
	// verbatim, and in every ranked case the printed unit and the gold unit were different callables
	// of the same file — usually the ranked hit being a thin entry point and the gold being the
	// helper it calls (redis__redis-10095: lpopCommand ranked, gold inside popGenericCommand, one
	// CALLS edge away in the same file). search_related.go excludes outgoing CALLS by design; see
	// search_callee.go for why its stated substitute (co-members of the same unit) cannot cover this.
	CalleeHop bool
	// EditSiteBodies attaches source to the EDIT-role sites of the SAME-CONCEPT LITERAL block: the
	// enclosing unit when one is resolvable, else a bounded window around the site. CONSUMER and DOC
	// sites stay file:line-only — a consumer is listed precisely so an agent does NOT open it.
	//
	// Off by default because it is an additive block and the default block is sized at 560 B. When
	// it is on, the block's cap rises to searchLiteralEditBodyClusterMaxBytes and its cost is
	// reported in stats.literal_cluster_bytes like every other block's.
	EditSiteBodies bool
	// EnclosureContextLines pads the rank-1 complete body with this many source lines on each
	// side. 0 means no padding (the body's exact symbol bounds).
	//
	// Measured on 65 sessions: of the reads that re-open a file whose complete body the payload
	// ALREADY printed, only 3.8% ask for lines inside that body — 80% ask for lines outside it
	// (file head 15.6%, within 40 lines 41.4%), median gap 42 lines, median window 50 lines. The
	// body is the right unit to EDIT and the wrong unit to UNDERSTAND, so a bounded margin buys
	// back the follow-up read that the unpadded body provokes.
	EnclosureContextLines int
	// The three REFERENCE blocks below are OFF by default, and the default is the measurement's
	// verdict rather than a taste call. Six session-level runs on real agents added the container
	// map (1119 B), the signature-type block (197 B) and the declaration card (300 B) to the first
	// search response and made sessions MORE expensive at no gain: turns up (23.1 vs 21.0 on one
	// model, 30.1 vs 26.8 on another), cost up 14-19%, resolve rate flat to worse. A leaner
	// competing tool answering the same prompt finished in 18.5 turns for less money.
	//
	// The discriminator that separated the blocks that paid from the blocks that did not was NOT
	// how informative they read. It was whether a block REPLACES A TOOL CALL the agent would
	// otherwise have made. These three answer questions an agent was not about to ask, so their
	// bytes are re-read on every later turn for nothing.
	//
	// They are kept, and kept tested, because that measurement is about AGENT sessions. A human
	// reading one search interactively pays for the bytes once and may well want the map.
	// See search_blocks.go for the default payload and why each other block is in it.
	IncludeContainerMap   bool
	IncludeSignatureTypes bool
	IncludeTypeCard       bool
	// Deep enables the exhaustive sparse (BM25 chunk) retrieval pass and its
	// reciprocal-rank fusion with the semantic ranking.
	//
	// It used to be switched on IMPLICITLY by `TopK > 10`, which made every observable
	// property of a search discontinuous at that boundary: the git-tree-grep preselector
	// was disabled, every file in the repo was read, the indexed-file pool shrank, region
	// deduplication stopped and the reported score stopped being a relevance score. Asking
	// for one more result is not a request to change retrieval strategy, so the strategy is
	// now an explicit choice and top-k only changes how many rows come back.
	Deep bool
	// SingleResolution turns OFF multi-resolution prose retrieval: the finer regions of a prose
	// document stay in each result's `passages` field instead of being promoted to results of their
	// own. See search_multi_resolution.go — the expansion is strictly additive (it only fills result
	// slots the distinct-file ranking left empty, and never breaches MaxContextBytes), so this
	// switch exists for callers that do not want passages promoted, not because the expansion can
	// cost them a file.
	//
	// It does NOT on its own restore the pre-section output shape. Promotion and section ranking are
	// independent: with only this flag set, a prose document is still ranked by section, so one file
	// can still supply many results. Reproducing the previous "one result per file" payload requires
	// SingleResolution AND DocumentResolution together.
	SingleResolution bool
	// DocumentResolution turns OFF section-resolution prose ranking, restoring the previous
	// behaviour: one prose document is one retrievable unit, so a corpus of N documents can never
	// return more than N ranked regions however large --top-k is. With it unset (the default), a
	// prose document's SECTIONS are the ranked units — each competes on its own score exactly as
	// an independent file would — which is what every comparable document retriever does and what
	// a reader asking a question about a long document actually wants. See search_prose_parent.go.
	DocumentResolution bool
}

// SearchPassage is an additional, non-overlapping source region from the same file as its
// parent result. It lets prose retrieval return two distant answer-bearing turns without
// pretending the unrelated text between them was one contiguous snippet.
type SearchPassage struct {
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	FocusLine int    `json:"focus_line"`
	Snippet   string `json:"snippet"`
}

// SearchResult is a ranked source region suitable for direct agent context.
type SearchResult struct {
	Rank             int     `json:"rank"`
	Score            float64 `json:"score"`
	FilePath         string  `json:"file_path"`
	StartLine        int     `json:"start_line"`
	EndLine          int     `json:"end_line"`
	FocusLine        int     `json:"focus_line"`
	SnippetStartLine int     `json:"snippet_start_line"`
	SnippetEndLine   int     `json:"snippet_end_line"`
	// SymbolStartLine/SymbolEndLine are the ENCLOSING symbol's true bounds, which differ from the
	// snippet bounds whenever the body was elided (too long for the enclosure cap, or a bounded
	// window). Without them a payload can say `main.c:578-583 symbol=umain` while `umain` actually
	// spans 270-725, and the agent cannot tell it is holding a 6-line shard of a 456-line function.
	// Measured on jqlang/jq-2235: that omission produced three sweeping reads (26,873 B, covering
	// main.c lines 1-697) where the no-tool baseline's `grep -n` gave exact anchors and read 17,536 B.
	SymbolStartLine int      `json:"symbol_start_line,omitempty"`
	SymbolEndLine   int      `json:"symbol_end_line,omitempty"`
	Language        string   `json:"language,omitempty"`
	Kind            string   `json:"kind,omitempty"`
	SymbolID        string   `json:"symbol_id,omitempty"`
	SymbolName      string   `json:"symbol_name,omitempty"`
	QualifiedName   string   `json:"qualified_name,omitempty"`
	Signature       string   `json:"signature,omitempty"`
	Signals         []string `json:"signals"`
	// Section groups a result for presentation. Empty (omitted) means the primary list of
	// candidate fix sites; see search_section.go for the other values and why the grouping is
	// a label rather than a filter.
	Section string `json:"section,omitempty"`
	Snippet string `json:"snippet"`
	// Passages are additional source slices from FilePath. The primary snippet stays unchanged
	// for backward-compatible consumers; clients that understand this additive field can avoid
	// a follow-up read when relevant prose is split across a long session.
	Passages []SearchPassage `json:"passages,omitempty"`
	// MergedRanks lists the PRE-merge ranks whose spans this one result now covers, set when
	// several near hits in one file were folded into a single contiguous region
	// (search_span_merge.go). Present only on a merged span, so its absence is the ordinary
	// case; when it is present the snippet is the verbatim, unelided text of the whole range,
	// which is the fact that stops a reader spending a turn bridging the gap itself.
	MergedRanks []int `json:"merged_ranks,omitempty"`
	// UnitStartLine/UnitEndLine are the TRUE span of the enclosing unit when --full-unit-top asked
	// for that unit whole and searchFullUnitMaxLines clipped it. They are set ONLY on a clipped
	// forced unit — their absence is the ordinary case and means the printed span IS the unit — and
	// they exist so a reader can be told which of the unit's own lines are missing rather than
	// discovering it by opening the file. Schema 1.x additive.
	UnitStartLine int `json:"unit_start_line,omitempty"`
	UnitEndLine   int `json:"unit_end_line,omitempty"`
	// CommentFocusLine is the COMMENT line the query originally matched, recorded only when the
	// payload re-anchored this hit onto the code that comment documents (search_reanchor.go). Its
	// absence is the ordinary case and means FocusLine is where the match itself landed. It is kept
	// because the prose line is still the evidence for why the hit is here, and a reader who is told
	// only the code line cannot tell a re-anchored hit from a direct one. Schema 1.x additive.
	CommentFocusLine int `json:"comment_focus_line,omitempty"`
	// BodyFromReanchor marks a result whose source exists ONLY because the re-anchor moved its
	// anchor onto code: the allocation computed WITHOUT the re-anchored enclosures showed nothing at
	// this rank. It is what lets the renderer fund such a body out of spare slots only, and it is
	// deliberately NOT the same test as `CommentFocusLine > 0` — most re-anchored hits already
	// carried a body, and gating those would evict source the re-anchor never paid for.
	BodyFromReanchor bool `json:"body_from_reanchor,omitempty"`
	// There is deliberately no per-result `Neighbors` list here. "The types this hit is
	// written in terms of" is answered once, by the signature-type block (search_sigtypes.go),
	// and "the other places this change lands" by the related-site block
	// (search_related.go) — both funded by DISPLACING TAIL LOCATORS, so neither can ever
	// shrink a complete head body. A per-result neighbor line folded in before the byte
	// fitter has the opposite property: its bytes compete with the head, and a complete
	// enclosing body is the one thing in the payload measured to remove a whole follow-up
	// turn. Two blocks appending graph neighbors to one payload would also print the same
	// fact twice. See search_blocks.go for the budget policy this follows.
}

type SearchStats struct {
	QueryConstraintsTruncated bool `json:"query_constraints_truncated,omitempty"`
	FilesScanned              int  `json:"files_scanned"`
	// RepoIgnoredFiles counts the files Git listed that the REPOSITORY's own
	// ignore rules removed from this corpus before FilesScanned was taken. It sits
	// beside FilesScanned deliberately: read alone, "scanned N files" invites the
	// reading that N is what the repository contains.
	RepoIgnoredFiles               int     `json:"files_excluded_by_repo_ignore_rules,omitempty"`
	PreselectionBackend            string  `json:"preselection_backend,omitempty"`
	PreselectionPasses             int     `json:"preselection_passes,omitempty"`
	PreselectionFilesExamined      int     `json:"preselection_files_examined,omitempty"`
	PreselectionConfidence         float64 `json:"preselection_confidence,omitempty"`
	PreselectionCoverage           float64 `json:"preselection_coverage,omitempty"`
	PreselectionDiversity          float64 `json:"preselection_diversity,omitempty"`
	PreselectionWidened            bool    `json:"preselection_widened,omitempty"`
	PreselectionBounded            bool    `json:"preselection_bounded,omitempty"`
	UsagePreselectionBackend       string  `json:"identifier_usage_preselection_backend,omitempty"`
	UsagePreselectionPasses        int     `json:"identifier_usage_preselection_passes,omitempty"`
	UsagePreselectionFilesExamined int     `json:"identifier_usage_preselection_files_examined,omitempty"`
	// Content-read counters report blobs hydrated into the Go process. Git's
	// own immutable-tree scans are represented by the backend/pass/examined
	// counters above; their internal byte IO is deliberately not estimated.
	FilesContentRead int   `json:"files_content_read_during_preselection"`
	QueryFilesRead   int   `json:"files_content_read_during_query"`
	QueryBytesRead   int64 `json:"bytes_content_read_during_query"`
	UsageFilesRead   int   `json:"files_content_read_for_identifier_usage,omitempty"`
	UsageBytesRead   int64 `json:"bytes_content_read_for_identifier_usage,omitempty"`
	// ContextBlockFilesRead/BytesRead is the IO the agent-asked blocks added: files the RANKING never
	// asked for, read to locate one literal, to find the switch sites over a variant set, and to
	// identify the build system above the top hit. It is reported separately because the query
	// counters answer a different question — how tightly selective indexing bounded the ranking.
	ContextBlockFilesRead int   `json:"files_content_read_for_context_blocks,omitempty"`
	ContextBlockBytesRead int64 `json:"bytes_content_read_for_context_blocks,omitempty"`
	FilesIndexed          int   `json:"files_indexed"`
	SymbolsConsidered     int   `json:"symbols_considered"`
	LexicalCandidates     int   `json:"lexical_candidates"`
	GraphCandidates       int   `json:"graph_candidates"`
	// CallerBoostedCandidates counts scored candidate regions across the
	// lexical, graph-expanded, and sparse pools that received the call-graph
	// caller-degree signal (the "graph:callers" result signal).
	CallerBoostedCandidates int `json:"caller_boosted_candidates,omitempty"`
	IdentifierUsages        int `json:"identifier_usage_candidates,omitempty"`
	NeighborCandidates      int `json:"same_container_neighbor_candidates,omitempty"`
	BridgeCandidates        int `json:"same_file_bridge_candidates,omitempty"`
	SparseCandidates        int `json:"sparse_candidates"`
	SparseFilesRead         int `json:"sparse_files_content_read"`
	CandidatesSelected      int `json:"candidates_selected"`
	ResultBytes             int `json:"result_bytes"`
	ProsePassages           int `json:"prose_passages,omitempty"`
	ProsePassageBytes       int `json:"prose_passage_bytes,omitempty"`
	ContextBudgetBytes      int `json:"context_budget_bytes,omitempty"`
	ResultsDropped          int `json:"results_dropped_by_budget,omitempty"`
	// SnippetsTruncated counts results carrying LESS source than the ranker produced for
	// them, whether the byte fitter compacted them or the snippet allocator demoted them to
	// a locator. A result grown to a complete symbol body is not truncated.
	SnippetsTruncated int `json:"snippets_truncated_by_budget,omitempty"`
	// CompleteSymbols counts results returned as the complete body of their enclosing
	// symbol; LocatorSnippets counts tail results demoted to a locator window to pay for
	// them. Together they report how the byte budget was allocated (schema 1.x additive).
	CompleteSymbols int `json:"complete_symbol_snippets,omitempty"`
	LocatorSnippets int `json:"locator_snippets,omitempty"`
	// DocReanchored counts ranked hits whose anchor landed in a doc comment and was moved onto the
	// code that comment documents (search_reanchor.go). It is reported like every other rendering
	// decision: a payload that describes a hit at a line the query did not match must say so.
	DocReanchored int `json:"doc_reanchored_hits,omitempty"`
	// MergedSpans counts ranked hits collapsed into a contiguous same-file span
	// (search_span_merge.go). It is reported for the same reason every other allocation
	// decision is: a payload that shows fewer blocks than the ranking produced must say so.
	MergedSpans int `json:"merged_spans,omitempty"`
	// CalleeHopSites counts candidate fix sites admitted by the callee hop (--callee-hop): units the
	// top hit CALLS. It is reported separately from RelatedSites because it is a separate, gated
	// route with a different funding rule — see search_callee.go — and because a payload has to be
	// attributable to the lever that shaped it.
	CalleeHopSites int `json:"callee_hop_sites,omitempty"`
	// RelatedSites counts entries in the related-site block: the other places the top hit's
	// change usually has to land (callers, sibling implementations, near-duplicate bodies).
	// They are funded out of the tail of the ranking, so this count also says how much of the
	// ranking's tail was worth less than a structural neighbour of the top hit.
	RelatedSites int `json:"related_sites,omitempty"`
	// SignatureTypes counts entries in the signature-type block: the types named in the top
	// hit's own signature. Like the related sites it is funded by displacement, so it costs
	// the payload nothing beyond the tail locators it replaced.
	SignatureTypes int `json:"signature_types,omitempty"`
	// CoveringTests counts entries in the covering-test section (0 or 1) and TypeCardEntries the
	// declarations in the type card.
	CoveringTests   int `json:"covering_tests,omitempty"`
	TypeCardEntries int `json:"type_card_entries,omitempty"`
	// The four *Bytes counters below are the per-section cost breakdown. Every one of these
	// blocks lives OUTSIDE `results`, so ResultBytes cannot account for it, and a block that
	// could grow unmeasured is a block that can silently break the byte contract it was built
	// under. They exist so the combined ceiling in Validate can be checked and so the cost of
	// each section is attributable. See search_blocks.go for the one budget policy that spends
	// them and the order in which they yield.
	TypeCardBytes      int `json:"type_card_bytes,omitempty"`
	SignatureTypeBytes int `json:"signature_type_bytes,omitempty"`
	// ContainerMapBytes is what the container map cost, measured — like its cap — on the LARGER
	// of its two wire forms, because a caller pays whichever one it asked for and the JSON form
	// runs about twice the rendered text. It is the ONE deliberately additive block: a payload
	// that spent its budget on complete head bodies must not lose one to buy the map, so the map
	// is capped (searchContainerMapMaxBytes) and its cost is stated rather than hidden. Every
	// other block above is funded from within the ceiling.
	ContainerMapBytes int `json:"container_map_bytes,omitempty"`
	// The three counters below are the agent-asked blocks' cost, measured — like the container
	// map's — on the LARGER of their two wire forms, so a JSON consumer is never told it paid half
	// of what it actually paid. LiteralClusterHits is how many occurrences the literal block
	// listed; the block's own hits_total says how many exist.
	LiteralClusterBytes int `json:"literal_cluster_bytes,omitempty"`
	LiteralClusterHits  int `json:"literal_cluster_hits,omitempty"`
	// FileOutlineBytes is what the FILE OUTLINE block cost, so its price is attributable like every
	// other block's rather than emergent.
	FileOutlineBytes   int `json:"file_outline_bytes,omitempty"`
	FileOutlineRows    int `json:"file_outline_rows,omitempty"`
	VerifyCommandBytes int `json:"verify_command_bytes,omitempty"`
	// VerifyExplainSuffixBytes is the caller-configured `| <explain cmd>` tail appended to the verify
	// command. It is reported separately because it is fixed overhead the caller opted into, so it is
	// added to the block's allowance instead of competing with the command the ranking derived.
	VerifyExplainSuffixBytes int `json:"verify_explain_suffix_bytes,omitempty"`
	// VerifyTier is which rung of the ladder produced the emitted command: narrow, suite, build-check
	// or none. See the tier constants in search_verify.go.
	VerifyTier     string `json:"verify_tier,omitempty"`
	ClosedSetBytes int    `json:"closed_set_bytes,omitempty"`
	// ContextBlockBytes is the sum of every block counter above: the whole cost of
	// everything outside `results`, in one number, so a caller can see the payload's true size
	// without re-deriving it.
	ContextBlockBytes int   `json:"context_block_bytes,omitempty"`
	IndexCacheHit     bool  `json:"index_cache_hit"`
	IndexLatencyMS    int64 `json:"index_latency_ms"`
	QueryLatencyMS    int64 `json:"query_latency_ms"`
	TotalLatencyMS    int64 `json:"total_latency_ms"`
	// SearchLatencyMS is retained as the backwards-compatible name for total
	// retrieval latency. New consumers should use TotalLatencyMS and the
	// separate preselection, index, and query phases.
	SearchLatencyMS    int64 `json:"search_latency_ms"`
	PreselectLatencyMS int64 `json:"preselect_latency_ms"`
}

// SearchFormatVersion versions the search RESPONSE ENVELOPE, the same axis
// impact, def, neighbors, index and stats already emit as format_version. It is
// deliberately not sem.SchemaVersion: this is a query response, not one of the
// persisted 1.x record artifacts ADR 0001 freezes, and conflating the two would
// make every search-payload change look like a change to the ingestion schema.
const SearchFormatVersion = 1

type SearchResponse struct {
	// FormatVersion leads the payload so a consumer can branch on it before
	// reading anything else. Search was the only machine-readable surface with no
	// version discriminator at all: its five sibling query commands emit
	// format_version and the record/snapshot surfaces emit schema_version, but a
	// consumer parsing search JSON had nothing to tell one payload shape from the
	// next.
	FormatVersion int            `json:"format_version"`
	Query         string         `json:"query"`
	RepoRoot      string         `json:"repo_root"`
	Commit        string         `json:"commit,omitempty"`
	Tree          string         `json:"tree,omitempty"`
	Profile       string         `json:"profile"`
	Results       []SearchResult `json:"results"`
	// The three blocks below live outside `results`, and they are declared in the order they
	// are rendered — see the section order documented in search_blocks.go.
	//
	// SignatureTypes holds the declarations of the types named in the top hit's
	// own signature (fields and member signatures, never bodies, never a
	// transitive walk). It is funded by displacing redundant tail locators, so
	// its presence never grows the payload — see search_sigtypes.go.
	SignatureTypes []SearchSignatureType `json:"signature_types,omitempty"`
	// TypeCard carries the declarations behind the names the head body uses — the compact
	// answer to "what is this identifier", which a snippet full of USES never contains. It is
	// not a ranking and holds no fix sites; see search_typecard.go.
	TypeCard []TypeCardEntry `json:"type_card,omitempty"`
	// ContainerMap maps the top hit's enclosing container — the file's extent and the
	// container's members with their line ranges — so a range read can be sized from this one
	// response instead of by reading the whole file. Nil when the payload has no ranked code
	// hit whose container the graph knows. See search_container_map.go.
	ContainerMap *SearchContainerMap `json:"container_map,omitempty"`
	// The three blocks below are the DEFAULT reference material, and each one exists because it
	// replaces a tool call the measurement says an agent makes anyway.
	//
	// LiteralCluster is `grep` folded into `search`: every occurrence of the one distinctive
	// literal that names the queried concept, each annotated with its enclosing symbol and whether
	// that site defines the concept or merely passes it. See search_literals.go.
	LiteralCluster *SearchLiteralCluster `json:"literal_cluster,omitempty"`
	// FileOutlines indexes the other symbols in the files the payload named. An index, never source.
	FileOutlines []SearchFileOutline `json:"file_outlines,omitempty"`
	// VerifyCommand is the narrowest test invocation for the top hit's file, derived from the
	// repository's own build files. Nil whenever it could not be derived with confidence: a wrong
	// command costs more than no command. See search_verify.go.
	VerifyCommand *SearchVerifyCommand `json:"verify_command,omitempty"`
	// ClosedSet warns when the top hit belongs to a closed variant set whose switch/match sites
	// will fail at RUNTIME, not at compile time, if a variant is added without an arm. See
	// search_closedset.go.
	ClosedSet *SearchClosedSet `json:"closed_set,omitempty"`
	// CoverageNote reports how many tests exercise the anchor the covering-test entry is about, and
	// names the ones that entry does not show. It exists because "the fix passed the test I was
	// shown and broke its neighbour" is a measured wrong-fix shape that no single test body can
	// prevent. Nil whenever exactly one test covers the anchor. See search_covertest.go.
	CoverageNote *SearchCoverageNote `json:"coverage_note,omitempty"`
	// RepoIgnored discloses what the REPOSITORY's own ignore rules removed from the
	// corpus this answer was assembled from, and is nil when they removed nothing.
	//
	// Every other field here describes the corpus that survived. That is what made
	// a committed ignore line an attack: one line deleted a tracked source file
	// from the payload, the remaining files were reported as the coverage, and an
	// agent working under this tool's own "search first" doctrine had nothing to
	// tell it that a file it should have read was missing. The graph does not need
	// to index an ignored file. It needs to stop implying that what it indexed is
	// everything the repository holds.
	//
	// Only REPOSITORY-controlled rules are reported (.graphignore, .gitignore,
	// .git/info/exclude, nested .gitignore). Exclusions the caller asked for with
	// --ignore-file are not: telling callers what they themselves excluded is noise
	// that would bury the case that matters.
	RepoIgnored     *RepoIgnoreReport  `json:"repo_ignored,omitempty"`
	Stats           SearchStats        `json:"stats"`
	Warnings        []ProviderWarning  `json:"warnings"`
	PartialFailures []PartialFailure   `json:"partial_failures"`
	Completeness    CompletenessReport `json:"completeness"`
	// replayProvenancePaths is the old mutable provider corpus whose membership
	// and contents could influence preselection, scores, ordering, and rendered
	// confidence. It stays out of schema 1.x and is used only by session replay.
	replayProvenancePaths []string
}

// boundedWorktreeSearchReplayProvenance sorts and deduplicates the already
// materialized provider corpus. One path beyond the persisted replay limit is
// retained deliberately: ValidateSearchReplayPaths then disables replay without
// carrying an unbounded hidden slice in SearchResponse.
func boundedWorktreeSearchReplayProvenance(paths []string) []string {
	ordered := append([]string(nil), paths...)
	sort.Strings(ordered)
	result := make([]string, 0, minInt(len(ordered), SearchReplayMaxPathCount+1))
	for _, filePath := range ordered {
		if len(result) > 0 && result[len(result)-1] == filePath {
			continue
		}
		result = append(result, filePath)
		if len(result) == SearchReplayMaxPathCount+1 {
			break
		}
	}
	return result
}

// repoIgnoreDisclosureCode is the diagnostic code that carries the disclosure on
// the channel every existing consumer already reads. A caller that only parses
// `warnings` still learns that this answer was assembled from a narrowed corpus.
const repoIgnoreDisclosureCode = "W_REPO_IGNORED_SOURCE"

// withRepoIgnoreDisclosure puts the disclosure warning FIRST when the
// repository's own ignore rules narrowed the corpus, and returns warnings
// unchanged otherwise.
func withRepoIgnoreDisclosure(warnings []ProviderWarning, report *RepoIgnoreReport) []ProviderWarning {
	// A count of zero is not "nothing was excluded" whenever the report also says
	// the count is short. An ignored directory that was unreadable before its
	// first descendant was reached removes an unknown number of files and lands
	// here with Files == 0 and CountIncomplete set, and guarding on Files alone
	// silenced this warning in exactly that case — the total shortfall, not the
	// partial one. That is this disclosure's own failure mode reached through its
	// own guard: the field doc promises "a caller that only parses warnings still
	// learns that this answer was assembled from a narrowed corpus", and the text
	// renderer already draws the line here (see RenderRepoIgnoreDisclosure), so
	// the two channels disagreed about the same report.
	if report == nil || (report.Files == 0 && !report.CountIncomplete && !report.GitListingUnavailable) {
		return warnings
	}
	names := make([]string, 0, len(report.Sources))
	for _, source := range report.Sources {
		names = append(names, termsafe.Line(source.File))
	}
	// With no file counted there is also no Sources entry to attribute it to —
	// attribution is recorded per counted path — so the sentence has to name the
	// rules generically rather than render "0 files excluded by " with an empty
	// list where the reader expects a filename.
	detail := fmt.Sprintf("%d file%s excluded by %s", report.Files, pluralS(report.Files), strings.Join(names, ", "))
	if report.Files == 0 {
		detail = "an unknown number of files excluded by the repository's own ignore rules"
		if len(names) > 0 {
			detail = "an unknown number of files excluded by " + strings.Join(names, ", ")
		}
	}
	switch {
	case len(report.Sample) > 0:
		detail += "; including " + termsafe.Line(report.Sample[0].Path)
	case len(report.Unreadable) > 0:
		detail += "; unreadable: " + termsafe.Line(report.Unreadable[0])
	}
	// FilePath carries the first excluded path so the disclosure survives every
	// renderer that prints only a warning's code and file — which is the compact
	// agent payload, i.e. the exact reader this finding is about. "Something is
	// hidden" is a shrug; "internal/auth/auth.go is hidden" is a next action.
	first := ""
	switch {
	case len(report.Sample) > 0:
		first = report.Sample[0].Path
	case len(report.Unreadable) > 0:
		// Nothing was counted, so there is no sample path to name. The path that
		// stopped the count is the only actionable one left, and a renderer that
		// prints just a warning's code and file is the reader this field exists
		// for — leaving it empty there is "something is hidden" with no next step.
		first = report.Unreadable[0]
	}
	effect := "files git lists were excluded from the corpus by the repository's own ignore rules; " +
		"the coverage figures describe what remained, not what the repository contains"
	if report.Files == 0 {
		// Stats.RepoIgnoredFiles is 0 here, so the coverage line and the compact
		// form's X<n> show no exclusion at all. Saying "the coverage figures
		// describe what remained" would invite the reader to subtract a number
		// that was never disclosed; what is true is that the size is unknown.
		effect = "the repository's own ignore rules removed content from the corpus and how much could not be " +
			"determined; the coverage figures describe what remained, not what the repository contains"
	}
	// FIRST, not appended. Every renderer that caps the diagnostics it prints
	// takes them from the head of the list — the agent payload prints three and
	// replaces the rest with a count — so appending left this warning's path behind
	// whatever unrelated diagnostics the snapshot happened to carry, and a reader
	// holding only the count knows something is hidden without knowing what to
	// open. The existing warnings keep their order behind it.
	return append([]ProviderWarning{{
		Code:                 repoIgnoreDisclosureCode,
		Severity:             "warning",
		FilePath:             first,
		EffectOnCompleteness: effect,
		Detail:               detail,
	}}, warnings...)
}

// repoIgnoreIncompleteCode is the partial-failure code for a disclosure whose
// count is short. Partial failures are the channel this provider already uses for
// "the answer is real but something was not read", and the coverage line already
// counts them, so the shortfall reaches every renderer without a new field to
// learn.
const repoIgnoreIncompleteCode = "E_REPO_IGNORE_UNREADABLE"

// repoIgnoreTruncatedCode is the same shortfall for the other reason: the
// excluded tree was larger than the accounting enumerates. It is a separate code
// because the codes are what a consumer routes on, and routing a tree that is
// merely big to the one that says UNREADABLE sends a reader after a permission
// problem that does not exist.
const repoIgnoreTruncatedCode = "E_REPO_IGNORE_COUNT_INCOMPLETE"

// repoIgnoreGitUnavailableCode marks the other way the disclosure falls short of
// exact: the listing was produced without Git's own enumeration of a real
// checkout, so a path a `.gitignore` removed cannot be separated from the
// tracked source Git would still have listed.
const repoIgnoreGitUnavailableCode = "E_REPO_IGNORE_GIT_UNAVAILABLE"

// withRepoIgnorePartialFailures appends the shortfall when the exclusion count
// could not be completed, and returns failures unchanged otherwise.
//
// It is applied at the response sink rather than where the walk runs, so every
// path that builds a SearchResponse from a listing inherits it and none can
// forget it — the same reason the disclosure warning is added here.
func withRepoIgnorePartialFailures(failures []PartialFailure, report *RepoIgnoreReport) []PartialFailure {
	if report == nil {
		return failures
	}
	if report.GitListingUnavailable {
		failures = append(failures, PartialFailure{
			Code:     repoIgnoreGitUnavailableCode,
			Severity: "warning",
			EffectOnCompleteness: "Git could not list this checkout, so the repository's own ignore rules were applied by a " +
				"filesystem walk; what they removed that Git would still have listed — tracked files — is not counted here",
			Detail: "exclusion accounting ran without Git's own listing of a git working tree",
		})
	}
	if !report.CountIncomplete {
		return failures
	}
	first := ""
	if len(report.Unreadable) > 0 {
		first = report.Unreadable[0]
	}
	detail := fmt.Sprintf("%d excluded path%s counted", report.Files, pluralS(report.Files))
	if len(report.Unreadable) > 0 {
		detail += "; unreadable: " + termsafe.Line(strings.Join(report.Unreadable, ", "))
	}
	// Two different shortfalls reach this line and a reader can only act on the
	// one that happened: an unreadable subtree is a broken checkout, a spent walk
	// budget is a tree too large to enumerate. Naming the wrong one sends the
	// reader looking for a permission problem that is not there.
	effect := "the tree the repository's own ignore rules removed is larger than the exclusion accounting " +
		"enumerates, so the disclosed count is a lower bound rather than the exact number, and the paths it " +
		"did enumerate are a filesystem-order sample of that tree rather than its first paths in sorted " +
		"order — two readers on two filesystems can be shown different ones"
	code := repoIgnoreTruncatedCode
	if len(report.Unreadable) > 0 {
		effect = "part of a tree the repository's own ignore rules removed could not be read, so the " +
			"disclosed exclusion count is a lower bound rather than the exact number"
		code = repoIgnoreIncompleteCode
	}
	return append(failures, PartialFailure{
		Code:                 code,
		Severity:             "warning",
		FilePath:             first,
		EffectOnCompleteness: effect,
		Detail:               detail,
	})
}

// maxRenderedRepoExclusions is how many excluded paths the TEXT payload names.
// The header line carries the exact count, so the list only has to be enough to
// act on; a longer one would cost bytes at the head of every payload in a
// repository that vendors legitimately.
const maxRenderedRepoExclusions = 3

// RenderRepoIgnoreDisclosure renders the disclosure for a text payload, or nil
// when there is nothing to disclose.
func RenderRepoIgnoreDisclosure(report *RepoIgnoreReport) []byte {
	if report == nil || (report.Files == 0 && !report.CountIncomplete && !report.GitListingUnavailable) {
		return nil
	}
	var out bytes.Buffer
	// Capped exactly like the sample list below, and for the same reason: Sources
	// carries one entry per DISTINCT .gitignore/.git-info-exclude/.graphignore
	// file that contributed an exclusion, with no upper bound of its own (a
	// project can nest hundreds of vendored .gitignore files, each an arbitrary
	// repository-controlled path). Joining every one of them here used to make
	// this single header line grow without limit, ahead of the ranked results
	// this payload's --max-context-bytes budget already accounts for elsewhere —
	// exactly the kind of unbudgeted, repository-controlled bytes that ceiling
	// exists to rule out.
	sourceNames := report.Sources
	if len(sourceNames) > maxRenderedRepoExclusions {
		sourceNames = sourceNames[:maxRenderedRepoExclusions]
	}
	names := make([]string, 0, len(sourceNames))
	for _, source := range sourceNames {
		names = append(names, termsafe.Line(source.File))
	}
	sourcesLabel := strings.Join(names, ", ")
	if remaining := len(report.Sources) - len(sourceNames); remaining > 0 {
		sourcesLabel += fmt.Sprintf(", +%d more", remaining)
	}
	// A count of zero is not "nothing was excluded" whenever the report also
	// says the count could not be completed: an ignored tree that was unreadable
	// before its first descendant was reached excludes an unknown number of
	// files, and the JSON channel says so. Rendering nothing for it told the one
	// reader who only ever sees the text payload that the corpus was whole —
	// which is this disclosure's own failure mode, reached through its renderer.
	if report.Files == 0 {
		out.WriteString("EXCLUDED: the repository's own ignore rules removed content from this corpus; " +
			"how much could not be determined\n")
	} else {
		fmt.Fprintf(&out, "EXCLUDED: %d file%s removed from this corpus by the repository's own ignore rules (%s)\n",
			report.Files, pluralS(report.Files), sourcesLabel)
	}
	shown := report.Sample
	if len(shown) > maxRenderedRepoExclusions {
		shown = shown[:maxRenderedRepoExclusions]
	}
	for _, exclusion := range shown {
		fmt.Fprintf(&out, "- %s\n", termsafe.Line(exclusion.Path))
	}
	if remaining := report.Files - len(shown); remaining > 0 {
		// The pointer has to name what the JSON actually holds. Its sample is
		// capped too, so promising "the full list" there is an instruction that
		// cannot be followed once a repository excludes more than the cap — and a
		// disclosure that misdirects is worse than a shorter honest one.
		if report.SampleTruncated {
			// "the count is exact" is a claim, and it is false whenever the walk
			// behind it stopped short. Printing it beside the LOWER BOUND line two
			// rows down told the reader both things at once.
			exactness := "the count is exact"
			if report.CountIncomplete {
				exactness = "the count is a lower bound"
			}
			fmt.Fprintf(&out, "- ... %d more (--format json: repo_ignored names %d of the %d; %s)\n",
				remaining, len(report.Sample), report.Files, exactness)
		} else {
			fmt.Fprintf(&out, "- ... %d more (full list in --format json: repo_ignored)\n", remaining)
		}
	}
	// Both shortfalls print AFTER the paths that could be named, and both print
	// whatever the count is: a partially counted tree renders an exact-looking
	// number today, and a reader cannot act on a lower bound they were not told
	// was one.
	if report.CountIncomplete {
		if len(report.Unreadable) > 0 {
			// Capped like Sources and Sample above, and for the same reason:
			// Unreadable carries one entry per unreadable repository-controlled
			// directory with no upper bound of its own (the ledger samples up to
			// maxRepoExclusionSample of them). Joining every one of them here
			// made this single line grow with the size of the broken subtree
			// instead of the size of the answer.
			unreadable := report.Unreadable
			if len(unreadable) > maxRenderedRepoExclusions {
				unreadable = unreadable[:maxRenderedRepoExclusions]
			}
			line := termsafe.Line(strings.Join(unreadable, ", "))
			if remaining := len(report.Unreadable) - len(unreadable); remaining > 0 {
				line += fmt.Sprintf(", +%d more", remaining)
			}
			fmt.Fprintf(&out, "- count is a LOWER BOUND: %s could not be read\n", line)
		} else {
			// An empty list rendered "- count is a LOWER BOUND:  could not be read",
			// which reads as a broken checkout to someone whose tree is merely big.
			out.WriteString("- count is a LOWER BOUND: the excluded tree is larger than this accounting " +
				"enumerates, and the paths named are a filesystem-order sample of it, not its first in sorted order\n")
		}
	}
	if report.GitListingUnavailable {
		out.WriteString("- count is PARTIAL: Git could not list this checkout, so tracked files its own " +
			"ignore rules cover are not counted here\n")
	}
	return out.Bytes()
}

// maxRepoIgnoreFloorBytes bounds RenderRepoIgnoreDisclosureFloor.
//
// The bound is the whole point of the floor. The full disclosure's size is set by
// the repository being searched — excluded path names, ignore-file names,
// unreadable directory names — which is why it may only be printed when it fits
// the remaining context headroom. The floor carries none of those: one sentence,
// one integer, and a pointer at the JSON channel that holds the rest. Its size is
// therefore a property of this file, so a caller can state in advance the most a
// payload can exceed --max-context-bytes by in order to stay honest about its own
// corpus. TestRepoIgnoreDisclosureFloorIsBoundedAndRepositoryFree pins it.
const maxRepoIgnoreFloorBytes = 160

// repoIgnoreDisclosureReserveBytes is what a listing with something to disclose
// takes out of the ranking's budget so the floor can be printed from INSIDE the
// caller's ceiling instead of on top of it.
//
// It is the floor THIS report renders, not the most any report could render. The
// count the floor states is settled before this is called — a listing's
// RepoIgnoreReport is produced by preselection and travels to SearchResponse
// unmodified — so the exact length is knowable here, and reserving
// maxRepoIgnoreFloorBytes instead took bytes off every ranking to fund a sentence
// that is shorter than the bound for every report that is not adversarially long.
// The bound still bounds; it is no longer the price.
//
// A listing with nothing to disclose reserves nothing and its payload is
// byte-identical.
func repoIgnoreDisclosureReserveBytes(report *RepoIgnoreReport) int {
	return len(RenderRepoIgnoreDisclosureFloor(report))
}

// searchRepoIgnoreFloorReserveBytes is the reservation as this CALLER sees it:
// the floor's own length when the caller's renderer prints one, and nothing when
// it does not.
func searchRepoIgnoreFloorReserveBytes(options SearchOptions, report *RepoIgnoreReport) int {
	if options.OmitsRepoIgnoreDisclosureFloor {
		return 0
	}
	return repoIgnoreDisclosureReserveBytes(report)
}

// RenderRepoIgnoreDisclosureFloor renders the IRREDUCIBLE form of the disclosure:
// the fact that the repository's own rules removed content, and where the details
// are. It is what a text payload prints when the full block (above) cannot be
// afforded — because the alternative, printing nothing, tells the one reader who
// only ever sees `--format text` that the corpus was whole.
//
// It names no repository-controlled bytes on purpose. That is what lets it be
// admitted against a budget the ranked results have already spent: the overshoot
// it can cause is bounded by maxRepoIgnoreFloorBytes and chosen here, not by the
// repository being searched.
func RenderRepoIgnoreDisclosureFloor(report *RepoIgnoreReport) []byte {
	if report == nil || (report.Files == 0 && !report.CountIncomplete && !report.GitListingUnavailable) {
		return nil
	}
	// A count of zero beside a shortfall is not "nothing was excluded" — same rule
	// as the full block, and for the same reason: an ignored tree that could not be
	// enumerated excludes an unknown amount.
	if report.Files == 0 {
		return []byte("EXCLUDED: repo ignore rules removed content from this corpus, amount unknown" +
			" (--format json: repo_ignored)\n")
	}
	atLeast := ""
	if report.CountIncomplete {
		atLeast = "at least "
	}
	return fmt.Appendf(nil, "EXCLUDED: %s%d file%s removed from this corpus by repo ignore rules"+
		" (--format json: repo_ignored)\n", atLeast, report.Files, pluralS(report.Files))
}

type searchQuery struct {
	raw         string
	rawLower    string
	constraints searchConstraints
	terms       []string
	termSet     map[string]bool
	weights     map[string]float64
	// dottedCallMentions holds lowercased "container.member" pairs the query
	// wrote as explicit calls ("Type.method(...)") — a direct naming of the
	// symbol the query is about.
	dottedCallMentions []string
	// words holds only the terms the caller actually WROTE as words: the maximal
	// alphanumeric runs of the raw query. It is deliberately not the term set —
	// `terms` also contains camelCase/compound fragments and morphological variants
	// mined out of identifiers, which are evidence for matching but not for intent.
	// See searchQuerySupplied.
	words map[string]bool
	// wordSequence is the same words in the order the caller wrote them, duplicates kept.
	// Reconstructing the identifier a caller typed ("update market sizes" ->
	// updateMarketSizes) is order-dependent, so the set above cannot do it.
	wordSequence []string
	// matchableWords is wordSequence without the words that cannot contribute a match to
	// any document in this corpus — words that never became search terms (stop words) and
	// terms no indexed file contains. Scoring reads the query through this sequence so an
	// unmatchable word cannot revoke a bonus the matchable ones earned.
	// Populated by withCorpusPresence once document frequencies are known.
	matchableWords []string
	// identifierTokens holds the compaction of every raw token the caller wrote in
	// identifier shape (mixed case, or containing `_` `.` `/` `:`). Such a token IS the
	// caller naming a symbol, whatever else the query says around it.
	identifierTokens map[string]bool
}

type searchCandidate struct {
	result            SearchResult
	aliases           []string
	termCounts        map[string]int
	docLength         int
	baseScore         float64
	score             float64
	constraintSurface *searchConstraintSurface
	prosePassagePlan  []SearchPassage
	// proseSection identifies the headed section of a prose document this region falls in. Set by
	// attachProseSectionUnits; it is what makes the prose unit of retrieval the SECTION rather than
	// the file. Empty for code, and for prose whose file has no indexed symbols.
	proseSection string
}

type searchContentReadTracker struct {
	read  contentReader
	files int
	bytes int64
}

func (tracker *searchContentReadTracker) Read(path string) (string, bool) {
	content, ok := tracker.read(path)
	if ok {
		tracker.files++
		tracker.bytes += int64(len(content))
	}
	return content, ok
}

func cachedContentReader(read contentReader) contentReader {
	type entry struct {
		content string
		ok      bool
	}
	cache := map[string]entry{}
	cachedBytes := 0
	return func(path string) (string, bool) {
		if cached, exists := cache[path]; exists {
			return cached.content, cached.ok
		}
		content, ok := read(path)
		if len(content) <= maxSearchContentCacheFileBytes && cachedBytes+len(content) <= maxSearchContentCacheBytes {
			cache[path] = entry{content: content, ok: ok}
			cachedBytes += len(content)
		}
		return content, ok
	}
}

var searchWordPattern = regexp.MustCompile(`[[:alnum:]_./:+#-]+`)
var sparseSearchWordPattern = regexp.MustCompile(`[[:alpha:]][[:alnum:]_]*|[[:digit:]]+`)

// searchQueryURLPattern matches an absolute URL written inside a query. The run stops at
// whitespace and at the delimiters that end a URL in prose and Markdown — `<>"'` + backtick,
// and the closing `)]}` of a `[text](url)` link — so a linked URL does not swallow the
// sentence punctuation that follows it.
var searchQueryURLPattern = regexp.MustCompile("(?i)[a-z][a-z0-9+.-]*://[^\\s<>\"'`)\\]}]+")

// stripSearchQueryURLState removes the query string and fragment of every URL in a query,
// leaving the scheme, host and path in place.
//
// Those two components are the web application's own state, never a name in this repository,
// and both mint tokens that outrank the words the caller actually wrote:
//
//   - `?file=/index.js` (the codesandbox/StackBlitz "open this file" parameter) tokenizes to
//     the standalone path token `index.js`, which is identifier-shaped, so it earns the
//     code-like weight 2.5 and then a +6.25 pathSearchScore on EVERY `index.js` in the tree.
//     Measured on preactjs/preact: `hooks/src/index.js` took rank 1 with signal `path` and
//     `karma.conf.js` rank 4, on the strength of a path that exists on codesandbox.io.
//   - a playground permalink encodes the whole reproduction program in its fragment
//     (`https://play.vuejs.org/#eNp9UsFOAjEQ...`). `#` is inside searchWordPattern's class and
//     base64 uses `+` and `/`, so the fragment is one enormous token whose camel-hump and
//     separator variants become dozens of terms — each mixed-case, so each reads as code-like
//     at weight 1.1. Measured on vuejs/core: 40 of 48 terms were base64 debris, and because
//     the term list is sorted by weight and truncated at maxSearchQueryTerms, that debris
//     EVICTED every plain prose word of the issue (`array`, `item`, `object`, `converted`,
//     `incorrectly`) from the query entirely.
//
// The path component is deliberately kept: a "the bug is here" link into the repository's own
// source (`https://github.com/o/r/blob/main/src/options.ts`) is the one part of a URL that does
// name a file, and dropping it would discard real evidence. A `#L42` line anchor or a `#issues`
// heading anchor is not a name in the tree, so removing the fragment costs nothing there.
func stripSearchQueryURLState(query string) string {
	if !strings.Contains(query, "://") {
		return query
	}
	return searchQueryURLPattern.ReplaceAllStringFunc(query, func(url string) string {
		cut := strings.IndexAny(url, "?#")
		if cut < 0 {
			return url
		}
		// A space, not "": the stripped tail must still end the URL token so the word that
		// follows it in the text cannot fuse onto the host/path.
		return url[:cut] + " "
	})
}

var searchStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "be": true, "but": true, "by": true, "can": true, "change": true,
	"code": true, "did": true, "does": true, "for": true, "from": true, "how": true,
	"i": true, "in": true, "into": true, "is": true, "it": true,
	"make": true, "not": true, "of": true, "on": true, "or": true, "our": true,
	// Politeness is addressed to the tool, never to the corpus: a word like these carries
	// no retrieval intent, so indexing it can only add noise. This is a refinement, not the
	// mechanism — see matchesExactSymbolForm and scoreSearchCandidates, which make ANY
	// unmatchable word inert, listed or not.
	"kindly": true, "please": true, "pls": true, "thanks": true,
	"should": true, "support": true, "that": true, "the": true,
	"this": true, "to": true, "use": true, "uses": true, "using": true,
	"we": true, "what": true, "when": true, "where": true, "which": true, "while": true,
	"with": true, "without": true,
}

var sparseSearchStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "be": true, "been": true, "but": true, "by": true,
	"can": true, "could": true, "do": true, "does": true, "for": true,
	"from": true, "had": true, "has": true, "have": true, "how": true,
	"i": true, "if": true, "in": true, "into": true, "is": true,
	"it": true, "its": true, "may": true, "not": true, "of": true,
	"on": true, "or": true, "our": true, "should": true, "that": true,
	"the": true, "their": true, "then": true, "this": true, "to": true,
	"was": true, "we": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "why": true, "will": true, "with": true,
	"would": true, "you": true, "your": true,
}

// SearchRepository performs local hybrid lexical/semantic retrieval. It uses
// no qrels, hosted models, embeddings, or network access.
// SearchRepository stamps the response envelope version at the ONE place a
// content-bearing SearchResponse leaves this package, rather than at each
// construction site. searchRepository has two success returns and error returns
// throughout; stamping per site means a third success path added later silently
// emits format_version 0, which a consumer cannot tell from "field absent".
// This mirrors how AnalyzeGitRangeWithOptions stamps SchemaVersion.
func SearchRepository(ctx context.Context, repo, providerVersion, query string, options SearchOptions) (SearchResponse, error) {
	response, err := searchRepository(ctx, repo, providerVersion, query, options)
	if err != nil {
		return SearchResponse{}, err
	}
	response.FormatVersion = SearchFormatVersion
	return response, nil
}

func searchRepository(ctx context.Context, repo, providerVersion, query string, options SearchOptions) (SearchResponse, error) {
	q := buildSearchQuery(query)
	if len(q.terms) == 0 {
		return SearchResponse{}, errors.New("search query has no meaningful terms")
	}
	if options.TopK <= 0 {
		options.TopK = defaultSearchTopK
	}
	if options.ContextLines < 0 {
		return SearchResponse{}, errors.New("search context lines cannot be negative")
	}
	if options.ContextLines == 0 {
		options.ContextLines = defaultSearchContextLines
	}
	if options.MaxRegionLines <= 0 {
		options.MaxRegionLines = defaultSearchMaxRegionLines
	}
	if options.MaxSnippetLines <= 0 {
		options.MaxSnippetLines = defaultSearchMaxSnippetLines
	}
	if options.MaxRegionsPerFile <= 0 {
		options.MaxRegionsPerFile = defaultSearchMaxRegionsPerFile
	}
	if options.MaxIndexedFiles <= 0 {
		options.MaxIndexedFiles = defaultSearchIndexedFiles(options.TopK)
		options.progressivePreselection = options.MaxIndexedFiles == defaultSearchMaxIndexedFiles
	} else {
		options.progressivePreselection = false
	}
	if options.Profile == "" {
		// Fast, not syntax-only: graph-aware ranking (caller-degree boosts and
		// relation expansion) needs call/construct edges over the indexed
		// subset. Syntax-only emits none, which silently disables the graph
		// half of hybrid search (graph_candidates is always 0).
		options.Profile = ProfileFast
	}
	sparseQuery := buildSparseSearchQuery(query)
	searchStarted := time.Now()
	baseSnapshotOptions := ProviderSnapshotOptions{
		NoNetwork:     true,
		Worktree:      options.Worktree,
		IgnoreFiles:   options.IgnoreFiles,
		IncludeFiles:  options.IncludeFiles,
		MaxParseBytes: options.MaxParseBytes,
		Profile:       options.Profile,
	}
	searchCacheDisabled := options.DisableCache
	if !options.Worktree && !searchCacheDisabled && options.CacheDir != "" {
		capturedOptions, captureErr := CaptureProviderCachePolicy(repo, baseSnapshotOptions)
		if captureErr != nil {
			// Capturing retains every policy input at once and therefore has a
			// stricter aggregate bound than sequential matcher construction. An
			// optional cache must not reject a search that the uncached path can
			// safely execute.
			searchCacheDisabled = true
		} else {
			baseSnapshotOptions = capturedOptions
			// Keep lexical preselection on the same immutable path ordering as
			// provider keying and construction. The captured policy itself travels
			// in baseSnapshotOptions to avoid rereading the files below.
			options.IgnoreFiles = capturedOptions.IgnoreFiles
			options.IncludeFiles = capturedOptions.IncludeFiles
			if options.afterCachePolicyCapture != nil {
				options.afterCachePolicyCapture()
			}
		}
	}
	var preindexedSnapshot ProviderSnapshot
	// preindexBinding keeps the generation this complete snapshot was read under
	// beside the snapshot itself. Deriving a selective view from it later is only
	// sound while that generation is still current, and this is the only place
	// that can observe it before the snapshot is read.
	var preindexBinding preloadedCompleteSnapshot
	preindexCacheHit := false
	indexStarted := time.Now()
	if !options.Worktree && !searchCacheDisabled {
		var err error
		preindexBinding, preindexCacheHit, err = loadCachedCompleteSearchSnapshotBinding(
			ctx, repo, providerVersion, baseSnapshotOptions, options.CacheDir,
		)
		if err != nil {
			return SearchResponse{}, err
		}
		preindexedSnapshot = preindexBinding.snapshot
	}
	if options.afterPreindexLoad != nil {
		options.afterPreindexLoad()
	}
	preindexLoadLatency := time.Since(indexStarted)
	preselectStarted := time.Now()
	selection, err := preselectSearchFiles(
		ctx, repo, q, sparseQuery, options, baseSnapshotOptions, preindexedSnapshot, preindexCacheHit,
	)
	if err != nil {
		return SearchResponse{}, err
	}
	// The repository identity can change without changing HEAD (for example
	// when a remote URL changes). Symbols embed that identity, so a complete
	// preindex loaded before preselection is usable only when both identities
	// still match. Treat drift as a miss and rebuild from the captured source.
	if preindexCacheHit && !searchSnapshotMatchesSelection(preindexedSnapshot, selection) {
		preindexedSnapshot = ProviderSnapshot{}
		preindexBinding = preloadedCompleteSnapshot{}
		preindexCacheHit = false
	}
	preselectLatency := time.Since(preselectStarted)
	// THE DISCLOSURE IS FUNDED FROM INSIDE THE CEILING, and it has to be funded
	// here, before a single byte of ranking is fitted.
	//
	// A `--format text` payload leads with what the repository's own ignore rules
	// removed, and degrades that block to an irreducible floor rather than
	// dropping it — text renders no warnings of its own, so the floor is the only
	// thing that stops a narrowed corpus reading as a whole one. But the fitter
	// spends the ceiling to the last few bytes (measured: 1996 funded of 2000),
	// so by render time there was nothing left to print the floor from and it was
	// admitted ON TOP: a payload that had something to disclose overran
	// --max-context-bytes by the floor's full length, and that number is what a
	// caller sized a context window against.
	//
	// Taking the reservation out of the RANKING rather than out of the answer
	// keeps both halves: the ranking loses at most maxRepoIgnoreFloorBytes, a
	// fixed cost this file picks, and the disclosure lands inside the caller's
	// number. Only a listing that actually has something to disclose pays it.
	//
	// A ceiling too small to fund the allowance at all keeps the behaviour it had:
	// the caller asked for less room than the shortest honest disclosure needs, and
	// the renderer's own last-resort floor is what answers that.
	//
	// options is a value, so narrowing it here narrows every fitter downstream.
	// callerContextBytes is what the response REPORTS, because the ceiling the
	// caller named is the one the renderer measures its headroom against — and
	// because a stat that quietly shrank would make the reservation unauditable.
	//
	// ONLY a renderer that prints the floor pays for it. The reservation buys the
	// text payload its one channel for saying the corpus is narrowed; the structured
	// formats already carry that fact as data (warnings + repo_ignored) and emit it
	// whatever the ranking spends, so charging them too deleted ranked source and
	// funded nothing — at --max-context-bytes 600 it cost a JSON payload its only
	// result. A caller declares that with OmitsRepoIgnoreDisclosureFloor; saying
	// nothing keeps the funded behaviour.
	callerContextBytes := options.MaxContextBytes
	if reserve := searchRepoIgnoreFloorReserveBytes(options, selection.repoIgnored); reserve > 0 &&
		options.MaxContextBytes >= reserve {
		// Zero means unlimited to the fitter. A one-byte budget seats no
		// result, preserving the floor-only case when the reserve uses it all.
		options.MaxContextBytes = max(1, options.MaxContextBytes-reserve)
	}
	selectedFiles := selection.files
	var replayProvenancePaths []string
	if options.Worktree || selection.commit == "" {
		replayProvenancePaths = boundedWorktreeSearchReplayProvenance(selection.allFiles)
	}
	if len(selectedFiles) == 0 {
		// A no-hit query still reports the health of an already-preindexed HEAD
		// graph. Do not build a cold graph merely to return no results, but do
		// preserve cached partial failures/completeness and cache-hit provenance.
		cachedSnapshot, cacheHit := preindexedSnapshot, preindexCacheHit
		// Commit can differ when two commits share a tree, but the tree and
		// repository identity must still match: the latter participates in stable
		// symbol IDs even when source bytes are unchanged.
		if cacheHit && selection.commit != "" && !searchSnapshotMatchesSelection(cachedSnapshot, selection) {
			return SearchResponse{}, errors.New("repository identity or HEAD changed during search; retry against a stable repository")
		}
		totalLatency := time.Since(searchStarted).Milliseconds()
		repoRoot, commit, tree := selection.repoRoot, selection.commit, selection.tree
		warnings := selection.warnings
		partialFailures := []PartialFailure{}
		completeness := CompletenessReport{Languages: map[string]LanguageCompleteness{}, Relations: map[string]int{}}
		symbolsConsidered := 0
		if cacheHit {
			repoRoot = cachedSnapshot.Header.RepoRoot
			commit = cachedSnapshot.Header.Commit
			tree = cachedSnapshot.Header.Tree
			warnings = cachedSnapshot.Header.Warnings
			partialFailures = cachedSnapshot.Header.PartialFailures
			if partialFailures == nil {
				partialFailures = []PartialFailure{}
			}
			completeness = cachedSnapshot.Header.Completeness
			symbolsConsidered = len(cachedSnapshot.Symbols)
		}
		return SearchResponse{
			Query:                 query,
			RepoRoot:              repoRoot,
			Commit:                commit,
			Tree:                  tree,
			Profile:               string(options.Profile),
			Results:               []SearchResult{},
			replayProvenancePaths: replayProvenancePaths,
			Stats: SearchStats{
				QueryConstraintsTruncated: q.constraints.Truncated,
				FilesScanned:              selection.filesScanned,
				RepoIgnoredFiles:          repoIgnoredFileCount(selection.repoIgnored),
				PreselectionBackend:       selection.preselectionBackend,
				PreselectionPasses:        selection.preselectionPasses,
				PreselectionFilesExamined: selection.preselectionFilesExamined,
				PreselectionConfidence:    selection.preselectionConfidence,
				PreselectionCoverage:      selection.preselectionCoverage,
				PreselectionDiversity:     selection.preselectionDiversity,
				PreselectionWidened:       selection.preselectionWidened,
				PreselectionBounded:       selection.preselectionBounded,
				FilesContentRead:          selection.filesContentRead,
				FilesIndexed:              0,
				SymbolsConsidered:         symbolsConsidered,
				ResultBytes:               serializedSearchResultBytes([]SearchResult{}),
				ContextBudgetBytes:        callerContextBytes,
				IndexCacheHit:             cacheHit,
				IndexLatencyMS:            preindexLoadLatency.Milliseconds(),
				PreselectLatencyMS:        preselectLatency.Milliseconds(),
				TotalLatencyMS:            totalLatency,
				SearchLatencyMS:           totalLatency,
			},
			RepoIgnored:     selection.repoIgnored,
			Warnings:        withRepoIgnoreDisclosure(warnings, selection.repoIgnored),
			PartialFailures: withRepoIgnorePartialFailures(partialFailures, selection.repoIgnored),
			Completeness:    completeness,
		}, nil
	}

	// The provider graph and lexical query scope must describe the same files.
	// A complete preindex can supply cached parse output, but search derives the
	// exact selective graph so cache presence cannot change relation expansion.
	onlyFiles := selectedFiles
	if options.IndexAllFiles || len(selectedFiles) == selection.filesScanned {
		// Canonicalize complete snapshots to the query-independent cache key so
		// `graph index` and all-files search share one durable artifact.
		onlyFiles = nil
	}
	snapshotOptions := baseSnapshotOptions
	snapshotOptions.OnlyFiles = onlyFiles
	snapshot, cacheHit := preindexedSnapshot, preindexCacheHit
	indexLatency := preindexLoadLatency
	if cacheHit && len(snapshotOptions.OnlyFiles) > 0 {
		// The preindexed complete snapshot never serves a selective query
		// directly: the loader reuses the per-query selective cache entry, only
		// derives from the complete snapshot on a miss, and falls back to an
		// ordinary build when derivation fails (for example after a HEAD move).
		indexStarted = time.Now()
		snapshot, cacheHit, err = loadOrDeriveSelectiveSearchSnapshot(
			ctx, repo, providerVersion, snapshotOptions, options.CacheDir, searchCacheDisabled, preindexBinding,
		)
		if err != nil {
			return SearchResponse{}, err
		}
		indexLatency += time.Since(indexStarted)
	} else if !cacheHit {
		indexStarted = time.Now()
		snapshot, cacheHit, err = loadOrBuildSearchGraphSnapshot(ctx, repo, providerVersion, snapshotOptions, options.CacheDir, searchCacheDisabled)
		if err != nil {
			return SearchResponse{}, err
		}
		indexLatency += time.Since(indexStarted)
	}
	// See the analogous no-hit guard above. Tree identity pins source bytes;
	// repository identity additionally pins the stable IDs built from them.
	if selection.commit != "" && !searchSnapshotMatchesSelection(snapshot, selection) {
		return SearchResponse{}, errors.New("repository identity or HEAD changed during search; retry against a stable repository")
	}
	queryStarted := time.Now()
	useHead := !options.Worktree && snapshot.Header.Commit != ""
	read, closeSource, err := openSearchContentReader(
		ctx, repo, snapshot.Header.Commit, useHead, options.IgnoreFiles, options.IncludeFiles, options.MaxParseBytes,
	)
	if err != nil {
		return SearchResponse{}, err
	}
	if closeSource != nil {
		defer closeSource()
	}
	queryReads := &searchContentReadTracker{read: read}
	read = cachedContentReader(queryReads.Read)
	// The repository-wide literal lookup two blocks share. It reuses the per-term posting lists
	// preselection already built, so it adds no corpus pass; see search_needle.go for the three
	// sources and why it answers nothing when none of them is exact.
	needleIndex := &searchNeedleIndex{read: read, corpus: selection.allFiles}
	needleIndex.termFiles, needleIndex.termFileTotals = selection.termPostings.snapshot()
	if selection.gitGrepUsable {
		needleIndex.grep = newSearchNeedleGrep(ctx, selection.repoRoot, selection.gitGrepTreeish, selection.ignores)
	}

	symbolsByFile := make(map[string][]SymbolRecord)
	symbolsByID := make(map[string]SymbolRecord, len(snapshot.Symbols))
	for _, symbol := range snapshot.Symbols {
		symbolsByFile[symbol.FilePath] = append(symbolsByFile[symbol.FilePath], symbol)
		symbolsByID[symbol.ID] = symbol
	}
	for filePath := range symbolsByFile {
		sort.Slice(symbolsByFile[filePath], func(i, j int) bool {
			left, right := symbolsByFile[filePath][i], symbolsByFile[filePath][j]
			if left.StartLine != right.StartLine {
				return left.StartLine < right.StartLine
			}
			return left.EndLine < right.EndLine
		})
	}

	fileLanguages := make(map[string]string, len(snapshot.Files))
	for _, file := range snapshot.Files {
		fileLanguages[file.Path] = file.Language
	}

	fileDF := make(map[string]int, len(q.terms))
	sparseDF := selection.sparseDF
	if sparseDF == nil {
		sparseDF = make(map[string]int, len(sparseQuery.terms))
	}
	sparseDocumentCount := selection.sparseDocumentCount
	sparseDocumentLength := selection.sparseDocumentLength
	var candidates []searchCandidate
	sparseCandidates := append([]searchCandidate(nil), selection.sparseCandidates...)
	// Corpus statistics come first, in their own pass. Scoring has to know which query
	// words this repository can match at all before a single candidate is built: the
	// all-or-nothing bonuses (exact-symbol, all-query-terms) are otherwise judged against
	// words nothing contains, which silently penalizes the documents that matched the rest
	// of the query. The content reader is memoized, so the second pass normally re-reads
	// nothing; a selection larger than the content cache can fall back to disk for the
	// overflow, which rereadFiles/rereadBytes below keep out of the read telemetry.
	indexableFiles := make([]string, 0, len(selectedFiles))
	for _, filePath := range selectedFiles {
		if err := ctx.Err(); err != nil {
			return SearchResponse{}, err
		}
		content, ok := read(filePath)
		if !ok || strings.IndexByte(content, 0) >= 0 {
			continue
		}
		indexableFiles = append(indexableFiles, filePath)
		lowerContent := strings.ToLower(content)
		lowerPath := strings.ToLower(filepath.ToSlash(filePath))
		for _, term := range q.terms {
			if strings.Contains(lowerContent, term) || strings.Contains(lowerPath, term) {
				fileDF[term]++
			}
		}
	}
	q = q.withCorpusPresence(fileDF)
	statisticsPassFiles, statisticsPassBytes := queryReads.files, queryReads.bytes
	for _, filePath := range indexableFiles {
		if err := ctx.Err(); err != nil {
			return SearchResponse{}, err
		}
		content, ok := read(filePath)
		if !ok {
			continue
		}
		lines := strings.Split(content, "\n")
		candidates = append(candidates, candidatesForFile(
			q, filePath, fileLanguages[filePath], lines, symbolsByFile[filePath], options,
		)...)
	}
	// Re-reading a file the content cache had to drop pulls in the same file, not another
	// one, so it must not inflate what the query is reported to have read.
	rereadFiles := queryReads.files - statisticsPassFiles
	rereadBytes := queryReads.bytes - statisticsPassBytes
	sparseFilesRead := selection.sparseFilesContentRead
	if options.Deep && len(sparseQuery.terms) > 0 {
		for _, filePath := range selection.sparseFiles {
			if selection.sparsePrecomputedFiles[filePath] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return SearchResponse{}, err
			}
			content, ok := read(filePath)
			sparseFilesRead++
			if !ok || len(content) > maxSparseSearchFileBytes || strings.IndexByte(content, 0) >= 0 {
				continue
			}
			lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
			if len(lines) == 1 && lines[0] == "" {
				continue
			}
			language := ""
			if spec, detected := languageForContent(filePath, content); detected {
				language = spec.language
			}
			fileSparse, documentCount, documentLength := sparseCandidatesForFile(
				sparseQuery, filePath, language, lines, options,
			)
			sparseCandidates = append(sparseCandidates, fileSparse...)
			sparseDocumentCount += documentCount
			sparseDocumentLength += documentLength
			for _, candidate := range fileSparse {
				for term := range candidate.termCounts {
					sparseDF[term]++
				}
			}
		}
		if len(selection.sparseFiles) > 0 && len(selection.sparseFiles) < selection.filesScanned {
			sparseDocumentCount = scaleSearchCorpusValue(
				sparseDocumentCount, selection.filesScanned, len(selection.sparseFiles),
			)
			sparseDocumentLength = scaleSearchCorpusValue(
				sparseDocumentLength, selection.filesScanned, len(selection.sparseFiles),
			)
		}
	}

	stats := SearchStats{
		QueryConstraintsTruncated: q.constraints.Truncated,
		FilesScanned:              selection.filesScanned,
		PreselectionBackend:       selection.preselectionBackend,
		PreselectionPasses:        selection.preselectionPasses,
		PreselectionFilesExamined: selection.preselectionFilesExamined,
		PreselectionConfidence:    selection.preselectionConfidence,
		PreselectionCoverage:      selection.preselectionCoverage,
		PreselectionDiversity:     selection.preselectionDiversity,
		PreselectionWidened:       selection.preselectionWidened,
		PreselectionBounded:       selection.preselectionBounded,
		FilesContentRead:          selection.filesContentRead,
		FilesIndexed:              len(selectedFiles),
		SymbolsConsidered:         len(snapshot.Symbols),
		LexicalCandidates:         len(candidates),
		SparseCandidates:          len(sparseCandidates),
		SparseFilesRead:           sparseFilesRead,
		IndexCacheHit:             cacheHit,
		IndexLatencyMS:            indexLatency.Milliseconds(),
		PreselectLatencyMS:        preselectLatency.Milliseconds(),
	}
	scoreSearchCandidates(candidates, q, fileDF, maxInt(1, len(selectedFiles)))
	callerBoosts := searchGraphCallerBoosts(snapshot.Relations, symbolsByID)
	stats.CallerBoostedCandidates += applySearchCallerBoosts(candidates, callerBoosts)
	sortSearchCandidates(candidates)

	graphCandidates := expandGraphCandidates(candidates, q, snapshot.Relations, symbolsByID, callerBoosts, read, fileLanguages, options)
	stats.GraphCandidates = len(graphCandidates)
	stats.CallerBoostedCandidates += countSearchCandidatesWithSignal(graphCandidates, "graph:callers")
	candidates = append(candidates, graphCandidates...)
	sortSearchCandidates(candidates)
	expansionSeeds := append([]searchCandidate(nil), candidates...)
	usageFilesBefore := queryReads.files
	usageBytesBefore := queryReads.bytes
	usagePreselection := searchScanTelemetry{}
	identifierUsages := expandIdentifierUsageCandidates(
		ctx, expansionSeeds, q, symbolsByID, symbolsByFile, read, fileLanguages, options,
		committedUsageFileSelector(
			repo, selection.commit, useHead, fileLanguages, options.Deep, selection.filesScanned, &usagePreselection,
		),
	)
	usageFilesRead := queryReads.files - usageFilesBefore
	usageBytesRead := queryReads.bytes - usageBytesBefore
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, err
	}
	stats.IdentifierUsages = len(identifierUsages)
	stats.UsagePreselectionBackend = usagePreselection.backend
	stats.UsagePreselectionPasses = usagePreselection.passes
	stats.UsagePreselectionFilesExamined = usagePreselection.filesExamined
	neighbors := expandSameContainerNeighborCandidates(ctx, expansionSeeds, q, symbolsByID, symbolsByFile, read, fileLanguages, options)
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, err
	}
	stats.NeighborCandidates = len(neighbors)
	bridges := expandSameFileBridgeCandidates(ctx, expansionSeeds, q, symbolsByFile, read, fileLanguages, options)
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, err
	}
	stats.BridgeCandidates = len(bridges)
	candidates = append(candidates, identifierUsages...)
	candidates = append(candidates, neighbors...)
	candidates = append(candidates, bridges...)
	candidates = dedupeSemanticMirrorCandidates(candidates, q, symbolsByID)
	// File-class prior then near-duplicate collapse, in that order: the prior
	// decides WHICH copy of a duplicated document survives, the collapse frees
	// the budget the other copies would have spent. Both run before selection so
	// the reclaimed result slots go to genuinely different code.
	applySearchFileClassPrior(candidates, q)
	// The boilerplate prior sits beside the file-class prior for the same reason: both correct a
	// score that term overlap got right about the TEXT and wrong about the FIX SITE. See
	// search_boilerplate.go.
	applySearchBoilerplatePrior(candidates, q)
	sortSearchCandidates(candidates)
	candidates = collapseNearDuplicateCandidates(candidates)
	// The prose unit of retrieval is the SECTION, and which section a region belongs to is decided
	// from the document's headings — not from whether the region happens to carry a symbol of its
	// own, which is true only for regions that matched a heading LINE. See attachProseSectionUnits.
	attachProseSectionUnits(candidates, symbolsByFile)
	semantic := selectSearchCandidates(candidates, q, options.TopK, options.MaxRegionsPerFile, !options.DocumentResolution)
	selected := semantic
	if len(sparseCandidates) > 0 {
		attachSparseCandidateSymbols(sparseCandidates, symbolsByFile)
		scoreSparseCandidates(sparseCandidates, sparseQuery, sparseDF, sparseDocumentCount, sparseDocumentLength)
		applySearchCandidateAgreement(sparseCandidates, q.constraints)
		// Caller boost FIRST, then the priors — the same order the semantic pool uses
		// above (boost at searchGraphCallerBoosts, priors before selectDiverseCandidates),
		// and it is load-bearing rather than incidental. The caller boost is ADDITIVE
		// (`score += boost`) while the file-class and boilerplate priors are MULTIPLICATIVE
		// (`score *= prior`), so `(score + boost) * prior` demotes a prose or vendored hit
		// together with the call-graph evidence it accumulated. Reversing them gives
		// `score * prior + boost`, which hands a well-connected documentation file a full,
		// undemoted boost and breaks the file-class prior's contract that non-source must be
		// clearly more relevant than the best source hit to outrank it.
		stats.CallerBoostedCandidates += applySearchCallerBoosts(sparseCandidates, callerBoosts)
		applySearchFileClassPrior(sparseCandidates, q)
		applySearchBoilerplatePrior(sparseCandidates, q)
		sortSearchCandidates(sparseCandidates)
		sparseCandidates = dedupeSemanticMirrorCandidates(sparseCandidates, q, symbolsByID)
		sortSearchCandidates(sparseCandidates)
		sparseCandidates = collapseNearDuplicateCandidates(sparseCandidates)
		// Fusion decides the ORDER. The reported score stays a relevance score on the
		// semantic scale (sparse rows are rescaled onto it by rescaleFusedCandidateScores),
		// because the score is the only signal a caller has for "is this a real hit or is
		// the repo simply missing what I asked for". Overwriting it with 1/rank — which the
		// deep path used to do — destroyed exactly that signal: every payload came back
		// looking like 1, 0.5, 0.333 whether the top hit was perfect or worthless.
		selected = rescaleFusedCandidateScores(
			selectHybridCandidates(semantic, sparseCandidates, options.TopK),
			semantic, sparseCandidates,
		)
	}
	sparseHydrationReads := hydrateSparseCandidates(selected, read)
	stats.SparseFilesRead += sparseHydrationReads
	// A test file is never a fix site, and rank 1 is the slot an agent reads and edits from. When
	// the flat -12 in searchPathPrior was not enough to sink one, lift the best editable hit over
	// it rather than subtracting harder — subtraction can eject the test from the payload, and the
	// VERIFY deriver reads that test. Runs before ranks are numbered so the byte fitter, the
	// enclosure planner and the VERIFY deriver all see one consistent order.
	// See search_testrank.go.
	selected = promoteFixSiteOverLeadingTest(selected, q)
	// Passage deduplication runs HERE, on the final selection, and not inside selectSearchCandidates
	// which only produced the semantic half of it: hybrid fusion replaces and reorders rows, so a
	// plan deduplicated against the pre-fusion selection loses passages whose claimant fusion
	// dropped and keeps passages that duplicate a sparse row fusion seated. See
	// dropSelectedProsePassages.
	selected = dropSelectedProsePassages(selected)
	results := make([]SearchResult, 0, len(selected))
	prosePassagePlans := make(map[int][]SearchPassage)
	for i := range selected {
		selected[i].result.Rank = i + 1
		selected[i].result.Score = math.Round(selected[i].score*10000) / 10000
		if selected[i].result.Signals == nil {
			selected[i].result.Signals = []string{}
		}
		if len(selected[i].prosePassagePlan) > 0 {
			prosePassagePlans[i+1] = append([]SearchPassage(nil), selected[i].prosePassagePlan...)
		}
		results = append(results, selected[i].result)
	}
	// A hit whose anchor landed in a doc comment is moved onto the code that comment documents
	// BEFORE anything prices or plans it: the byte fitter then charges for the code it will print,
	// the enclosure planner looks for a callable at the code line rather than in the prose above it,
	// and the literal block mines program text. Ranking is untouched — see search_reanchor.go.
	results, stats.DocReanchored = reanchorSearchDocComments(results, symbolsByFile, read, options.MaxSnippetLines)
	// SECTION LABELS ARE PRICED WITH THE RANKING, not after it. `"section":"docs-and-fixtures"` is
	// ~28 bytes on every result it touches, and the authoritative pass below runs after the fitter
	// and the allocator have already spent the ceiling — so on a prose-heavy payload those bytes
	// landed OUTSIDE the budget and SearchResponse.Validate rejected the whole response with
	// `search result context exceeds byte budget`, failing the command for a caller who had asked
	// for a smaller answer. Measured: a 6-document notes corpus at --max-context-bytes 6000 came
	// back 6170 bytes with --document-resolution.
	//
	// Labelling here charges them to the fitter. The pass is idempotent, so the call below still
	// decides the final labels, and the all-docs fallback, on the payload that is going out.
	results = assignSearchSections(results, q)
	ranked := append([]SearchResult(nil), results...)
	results, resultBytes, dropped, _ := fitSearchResultsToBudget(results, q, options.MaxContextBytes)
	// The fitter decides HOW MANY results fit; the allocator decides how the bytes they are
	// allowed are spent. It runs second, on the already-seated results, so a hit whose
	// complete enclosing symbol fits is returned whole — removing the follow-up file read
	// an agent would otherwise pay a whole turn for — funded, when the budget is tight, by
	// demoting the tail of the ranking to locators.
	// How deep the COMPLETE-BODY head reaches is separable from how deep the "never demoted to a
	// locator" head reaches, and on a capable model they want different depths. Measured on sonnet
	// (20 instances, config 9): of the files the payload showed, rank 1 was later read 62% of the
	// time and edited 54%, rank 2 read 27%/edited 20%, but ranks 3/4/5 were read 0%/7%/0% and
	// edited 11%/7%/0% — while still being charged for 21 complete bodies. Those bytes are replayed
	// on every later turn and buy nothing on that tier.
	//
	// So BodyHeadRanks narrows only the body upgrade. The locator head is deliberately left on
	// searchEnclosureHeadRanks: a tail locator is the only mention of its file, and the gold file
	// sits at rank 6-8 on 17% of instances, so shortening the ranking itself loses fix sites (that
	// is why cutting --top-k to 5 was rejected). Bodies are what cost bytes; locators are not.
	bodyHeadRanks := searchEnclosureHeadRanks
	if options.BodyHeadRanks > 0 && options.BodyHeadRanks < bodyHeadRanks {
		bodyHeadRanks = options.BodyHeadRanks
	}
	// HeadWindowLines: a head rank with no enclosable callable falls back to a bounded read
	// window instead of a two-line locator. 0 disables it (previous behaviour exactly).
	// An explicit caller value wins. Prose-parent retrieval otherwise uses its measured native
	// default; ordinary code search keeps the previous disabled behaviour.
	headWindowLines := resolvedSearchHeadWindowLines(results, options.HeadWindowLines)
	// FullUnitTop: the first N ranks come back as their complete enclosing unit whatever the
	// opportunistic conditions say. Resolved against the SEATED ranking, because the gap heuristic
	// that admits rank 2 is a statement about the scores the caller will actually read.
	fullUnitRanks := searchFullUnitForceRanks(results, options.FullUnitTop)
	// BUDGET-DRIVEN RENDERING is enabled by the payload-shape levers, never by default: the shipped
	// payload is what every prior measurement was taken against, and this changes how the whole ceiling
	// is spent. --edit-site-bodies is excluded because it only touches the literal block, so its effect
	// stays isolated and separately measurable.
	budgetDriven := fullUnitRanks > 0 || options.CalleeHop
	enclosures := planSearchEnclosures(
		results, symbolsByID, symbolsByFile, read,
		defaultSearchEnclosureMaxLines, options.EnclosureContextLines, bodyHeadRanks, headWindowLines,
		fullUnitRanks, budgetDriven,
	)
	// The UNFORCED plan for the same inputs. It is what the allocator computes its control allocation
	// from, and that control is the floor a forced unit may not push anything below — see
	// seatForcedSearchUnits for the regression that made this necessary. Planning it twice costs no IO:
	// `read` is the shared content cache, so the second pass re-reads nothing.
	var plainEnclosures []searchEnclosure
	if fullUnitRanks > 0 {
		plainEnclosures = planSearchEnclosures(
			results, symbolsByID, symbolsByFile, read,
			defaultSearchEnclosureMaxLines, options.EnclosureContextLines, bodyHeadRanks, headWindowLines,
			0, budgetDriven,
		)
	}
	tailSnippetLines := minInt(searchEnclosureTailSnippetLines, options.MaxSnippetLines)
	seated := results
	// bodyHeadRanks narrows the BODY head only; the never-demoted head stays at
	// searchEnclosureHeadRanks. Passing one value for both is what made --body-head-ranks=2
	// tersify ranks 3-5 as well, contradicting the paragraph above.
	results, completeSymbols, locators := allocateSearchSnippets(
		seated, enclosures, plainEnclosures, options.MaxContextBytes,
		resolvedSearchSnippetGrowth(seated, options.MaxContextBytes),
		bodyHeadRanks, searchEnclosureHeadRanks, tailSnippetLines,
	)
	// THE RE-ANCHOR FUNDING INVARIANT, the same one seatForcedSearchUnits enforces for forced units: a
	// body a hit gained only because it was re-anchored may be paid for out of free budget, never out of
	// another rank's existing allocation. Re-anchoring moves the ANCHOR; it is not a licence to
	// re-decide who gets source.
	//
	// Measured on vuejs__core-11870: `arrayInstrumentations.ts:10→12` picked up a complete body and the
	// allocator funded it by demoting `runtime-core/src/helpers/renderList.ts:54-107` — 54 lines of the
	// production helper the issue is actually about — to a bare locator. Net: the payload traded a body
	// it had for a body it did not need.
	//
	// So the re-anchored enclosures are planned, priced and then ACCEPTED ONLY IF the resulting
	// allocation still shows every other rank everything the control allocation showed it. When it does
	// not, control stands: the hit keeps its re-anchored `focus=`, which is the larger part of the win
	// and costs nothing.
	if control, exempt, gated := unreanchoredSearchEnclosures(seated, enclosures); gated {
		plan, bodies, demoted := allocateSearchSnippets(
			seated, control, plainEnclosures, options.MaxContextBytes, searchEnclosureGrowthBytes,
			bodyHeadRanks, searchEnclosureHeadRanks, tailSnippetLines,
		)
		if !searchAllocationPreservesSource(plan, results, exempt) {
			results, completeSymbols, locators = plan, bodies, demoted
		} else {
			markSearchReanchorGainedBodies(results, plan, exempt)
		}
	}
	// Rule 1 of budget-driven rendering, applied last of the ranking passes: spend whatever ceiling is
	// still unclaimed on the ranks that are still locators, in rank order. Purely additive.
	if budgetDriven {
		var expanded int
		results, expanded = spendRemainingSearchBudget(results, results, enclosures, options.MaxContextBytes)
		completeSymbols += expanded
	}
	stats.CompleteSymbols = completeSymbols
	stats.LocatorSnippets = locators
	results, _, _ = allocateProseParentPassages(results, prosePassagePlans, options.MaxContextBytes)
	// Truncation is measured against the ranking, by rank, so it has to be read off the
	// allocator's output BEFORE the related-site block renumbers the payload.
	truncated := countBudgetTruncatedResults(ranked, results)
	// The callee hop runs HERE, and the position is load-bearing at both ends. After the truncation
	// counter, so that counter still describes what the ALLOCATOR did (it is read by rank, and the
	// hop only inserts new ranks, so an inserted entry is skipped rather than miscounted). Before the
	// span merge, so a callee in the anchor's own file — the commonest and most useful case — can be
	// folded into one contiguous span with its caller instead of arriving as a second excerpt of the
	// same file with a hole between them.
	if options.CalleeHop {
		hopCache := newSearchRelatedFileCache(read, searchRelatedCandidateLimit)
		sites := selectSearchCalleeHopSites(
			results, q, snapshot.Relations, symbolsByID, symbolsByFile, hopCache,
			searchCalleeHopRanks(results), searchCalleeHopSiteLimit,
		)
		// Bodies follow the other levers rather than inventing a third policy; see
		// searchCalleeHopResult.
		withBody := fullUnitRanks > 0 || options.EditSiteBodies
		entries := make([]SearchResult, 0, len(sites))
		anchorIndexes := make([]int, 0, len(sites))
		for _, site := range sites {
			lines, ok := hopCache.get(site.symbol.FilePath)
			if !ok {
				continue
			}
			entry, ok := searchCalleeHopResult(site, lines, withBody)
			if !ok {
				continue
			}
			entries = append(entries, entry)
			anchorIndexes = append(anchorIndexes, site.anchorIndex)
		}
		results, stats.CalleeHopSites = mergeSearchCalleeHopSites(
			results, entries, anchorIndexes, options.MaxContextBytes,
			minInt(searchEnclosureTailSnippetLines, options.MaxSnippetLines),
			maxInt(fullUnitRanks, 1),
		)
	}
	// Two printed bodies of one file with a small hole between them are one region as far as the
	// reader is concerned, and the hole is what it spends a turn closing: measured over 113 Opus
	// payloads, 40% carry such a pair, and the tool makes +19.6% MORE Read calls than the no-tool
	// baseline while total tool calls fall 14.5%. Folding them into one contiguous, explicitly
	// unelided span runs here — after truncation has been measured against the ranking, so the
	// counter still describes what the allocator did, and before sectioning, so it can only ever
	// touch candidate fix sites. See search_span_merge.go for the bounds that keep it from
	// evicting a hit to pay for a bridge.
	if merged, spans, absorbed := mergeSameFileSearchSpans(results, read, options.MaxContextBytes); spans > 0 {
		results = merged
		stats.MergedSpans = spans
		stats.CompleteSymbols = maxInt(0, stats.CompleteSymbols-absorbed)
	}
	// Sectioning first: it decides which hit anchors the related-site block, because a
	// docs-and-fixtures hit at rank 1 is not a fix site and its neighbourhood is not the
	// neighbourhood of the change.
	results = assignSearchSections(results, q)
	if anchors := searchRelatedAnchors(results, symbolsByID, symbolsByFile, searchEnclosureHeadRanks); len(anchors) > 0 {
		sites := selectSearchRelatedSites(
			results, anchors, q, snapshot.Relations, symbolsByID, symbolsByFile, read, searchRelatedSiteLimit,
		)
		results, stats.RelatedSites = mergeSearchRelatedSites(results, sites, read, options.MaxContextBytes)
	}
	// The three context blocks below share one budget. Their construction order is FIXED and
	// load-bearing — see the policy in search_blocks.go. In short:
	//
	//  1. Contract context (covering test + type card) runs FIRST. It is the only block that
	//     GROWS the payload, so it must claim its bytes against the untouched ceiling; and the
	//     covering test is the block that yields last, so it gets first call on funding.
	//  2. Signature types run SECOND. fundSearchSignatureTypes is strictly byte-neutral — it
	//     seats the block only out of locators it displaces — so running it after step 1
	//     cannot breach the ceiling step 1 just satisfied. Running it BEFORE step 1 would:
	//     step 1's ceiling check does not know about bytes already committed outside
	//     `results`, so it would re-spend them and overshoot by exactly the block's size.
	//  3. The container map runs LAST, on the payload that is actually going out: its anchor
	//     must be the hit the caller will read, after every block above has decided which
	//     result that is.
	var typeCard []TypeCardEntry
	var coverageNote *SearchCoverageNote
	if anchors := searchTypeCardAnchor(results, symbolsByID, symbolsByFile, searchEnclosureHeadRanks); len(anchors) > 0 {
		context := buildSearchContractContext(
			results, anchors, q, snapshot.Relations, symbolsByID, symbolsByFile, read,
			options.IncludeTypeCard,
		)
		results, typeCard, coverageNote, stats.CoveringTests, stats.TypeCardEntries =
			mergeSearchContractContext(results, context, options.MaxContextBytes)
	}
	var signatureTypes []SearchSignatureType
	if options.IncludeSignatureTypes {
		if anchors := searchRelatedAnchors(results, symbolsByID, symbolsByFile, 1); len(anchors) == 1 {
			block := searchSignatureTypeSurface(
				anchors[0], symbolsByID, symbolsByFile, snapshot.Relations, searchSignatureTypeLimit,
			)
			results, signatureTypes = fundSearchSignatureTypes(results, block, searchEnclosureHeadRanks)
		}
	}
	stats.SignatureTypes = len(signatureTypes)
	if len(signatureTypes) > 0 {
		stats.SignatureTypeBytes = serializedSearchResultBytes(signatureTypes)
	}
	// The three agent-asked blocks. Each one replaces a specific tool call an agent measurably
	// makes anyway — a repo-wide grep, a fumbled test invocation, and the read that discovers a
	// switch it forgot to extend — which is the property the reference blocks above lack. They are
	// additive and separately capped; see search_blocks.go.
	verifyEvidence := searchVerifyEvidence{
		read: read, prefix: options.VerifyPrefix, preFixStatus: options.VerifyPreFixStatus,
	}
	// The three agent-asked blocks read files the RANKING never asked for: a bounded set of files
	// containing one literal, a handful of switch sites, and the build manifests above the top hit.
	// Their IO is bracketed here and reported under its own counter rather than folded into the query
	// counters, which document how tightly selective indexing bounded the ranking's own reads. Two
	// different questions, two different numbers.
	blockFilesBefore, blockBytesBefore := queryReads.files, queryReads.bytes
	// The closed-set warning is built BEFORE the literal cluster, and the order is a budget decision
	// rather than a preference: both draw on the needle index's shared read budget, the warning needs
	// only a handful of files, and it is the block whose absence can make a patch wrong. Building it
	// first guarantees it is never starved by a literal lookup that spent the budget and then failed
	// its own distinctiveness test.
	closedSet := buildSearchClosedSet(
		results, symbolsByID, symbolsByFile, needleIndex, searchClosedSetMaxBytes,
	)
	if closedSet != nil {
		stats.ClosedSetBytes = searchClosedSetCost(closedSet)
	}
	literalCluster := buildSearchLiteralCluster(
		results, q, symbolsByFile, needleIndex, searchLiteralClusterMaxBytes, options.EditSiteBodies,
	)
	if literalCluster != nil {
		stats.LiteralClusterBytes = searchLiteralClusterCost(literalCluster)
		stats.LiteralClusterHits = len(literalCluster.Hits)
	}
	var fileOutlines []SearchFileOutline
	if options.IncludeFileOutline {
		fileOutlines = buildSearchFileOutlines(
			results, symbolsByFile,
			searchOutlineMaxFiles, searchOutlineMaxRowsPerFile, searchOutlineMaxBytes,
		)
		if len(fileOutlines) > 0 {
			stats.FileOutlineBytes = SearchFileOutlineCost(fileOutlines)
			for _, outline := range fileOutlines {
				stats.FileOutlineRows += len(outline.Rows)
			}
		}
	}
	verifyCommand := buildSearchVerifyCommand(results, verifyEvidence)
	if verifyCommand != nil && options.VerifyExplainCommand != "" {
		// Compose here, in the emitted string, rather than asking the agent to compose. `explain`
		// passes the build output through before appending declarations, so this stays a superset of
		// what the bare command printed — the agent loses nothing by running the longer line.
		composed, overhead := composeSearchVerifyExplain(
			verifyCommand.Command, options.VerifyExplainCommand)
		verifyCommand.Command = composed
		// The wrapper is caller-configured FIXED overhead, not content the ranking produced, so it is
		// added to the block's allowance rather than charged against it. Without this the composed
		// command overflows the 320-byte cap and search_blocks fails the whole response — measured:
		// "search verify command exceeds its allowance: 373 > 320", which returned ZERO-BYTE payloads
		// on 7 of 17 sessions before it was caught.
		stats.VerifyExplainSuffixBytes = overhead
	}
	if verifyCommand != nil {
		stats.VerifyCommandBytes = searchVerifyCommandCost(verifyCommand)
		// The tier is reported so a harness can bucket sessions by which rung answered them; that is
		// how "the VERIFY command is unrunnable or non-covering in 12/12 sessions" was measured.
		stats.VerifyTier = verifyCommand.Tier
	}
	blockFilesRead := queryReads.files - blockFilesBefore
	blockBytesRead := queryReads.bytes - blockBytesBefore
	stats.ContextBlockFilesRead = blockFilesRead
	stats.ContextBlockBytesRead = blockBytesRead
	var containerMap *SearchContainerMap
	if options.IncludeContainerMap {
		containerMap = fitSearchContainerMap(
			buildSearchContainerMap(results, symbolsByID, symbolsByFile, read),
			searchContainerMapMaxBytes,
		)
	}
	if containerMap != nil {
		// searchContainerMapCost, not the text length: the block is CAPPED on the larger of its
		// two wire forms (the JSON object runs about twice the rendered text), so reporting the
		// text length told a JSON consumer it had paid roughly half what it actually paid.
		// stats.context_block_bytes is only attributable if every counter feeding it measures
		// what the caller was actually charged.
		stats.ContainerMapBytes = searchContainerMapCost(containerMap)
	}
	// Multi-resolution runs LAST, after every block above has decided which result it anchors on,
	// so a promoted passage can never become the anchor for the related-site, contract or
	// container-map blocks — those describe code, and a prose passage is not a definition. It is
	// also the only pass that can raise the result count above the distinct-file count, so running
	// it here keeps every earlier pass reasoning about the ranking it actually produced.
	// Containment is resolved before promotion, not after, so the slots a duplicate was occupying
	// are handed back to the expansion and spent on regions the payload does not already show.
	results = dropContainedProseResults(results)
	if !options.SingleResolution {
		// The reference blocks are funded from the same ceiling the response is validated against,
		// and they were priced before this pass runs, so promotion may only spend what they left.
		reserved := stats.SignatureTypeBytes
		if len(typeCard) > 0 {
			reserved += serializedSearchResultBytes(typeCard)
		}
		results = expandProseResolution(results, options.TopK, options.MaxContextBytes, reserved)
	}
	stats.CandidatesSelected = len(results)
	stats.ProsePassages, stats.ProsePassageBytes = searchPassageStats(results)
	resultBytes = serializedSearchResultBytes(results)
	stats.ResultBytes = resultBytes
	if len(typeCard) > 0 {
		stats.TypeCardBytes = serializedSearchResultBytes(typeCard)
	}
	// One number for everything outside `results`, so the payload's true size never has to be
	// re-derived from three separate counters. See search_blocks.go.
	stats.ContextBlockBytes = searchContextBlockBytes(stats)
	stats.ContextBudgetBytes = callerContextBytes
	stats.ResultsDropped = dropped
	stats.SnippetsTruncated = truncated
	// Query and usage counters are disjoint physical reads. Identifier lookups
	// already satisfied by the shared content cache add zero usage bytes.
	stats.QueryFilesRead = queryReads.files - usageFilesRead - rereadFiles - blockFilesRead
	stats.QueryBytesRead = queryReads.bytes - usageBytesRead - rereadBytes - blockBytesRead
	stats.UsageFilesRead = usageFilesRead
	stats.UsageBytesRead = usageBytesRead
	stats.QueryLatencyMS = time.Since(queryStarted).Milliseconds()
	stats.TotalLatencyMS = time.Since(searchStarted).Milliseconds()
	stats.SearchLatencyMS = stats.TotalLatencyMS
	stats.RepoIgnoredFiles = repoIgnoredFileCount(selection.repoIgnored)
	if results == nil {
		results = []SearchResult{}
	}
	partialFailures := snapshot.Header.PartialFailures
	if partialFailures == nil {
		partialFailures = []PartialFailure{}
	}
	return SearchResponse{
		Query:                 query,
		RepoRoot:              snapshot.Header.RepoRoot,
		Commit:                snapshot.Header.Commit,
		Tree:                  snapshot.Header.Tree,
		Profile:               string(options.Profile),
		Results:               results,
		SignatureTypes:        signatureTypes,
		TypeCard:              typeCard,
		ContainerMap:          containerMap,
		LiteralCluster:        literalCluster,
		FileOutlines:          fileOutlines,
		VerifyCommand:         verifyCommand,
		ClosedSet:             closedSet,
		CoverageNote:          coverageNote,
		RepoIgnored:           selection.repoIgnored,
		Stats:                 stats,
		Warnings:              withRepoIgnoreDisclosure(snapshot.Header.Warnings, selection.repoIgnored),
		PartialFailures:       withRepoIgnorePartialFailures(partialFailures, selection.repoIgnored),
		Completeness:          snapshot.Header.Completeness,
		replayProvenancePaths: replayProvenancePaths,
	}, nil
}

// newSearchNeedleGrep is the needle index's Git route: it lists every file in the tree
// containing a fixed string, case-sensitively. It is the only route into the payload that
// answers over the WHOLE TREE rather than over the listed corpus, so it is the only one that
// can name a path the listing already excluded — a credential store denied by the built-in
// rules in ignore.go, a tree the caller's --ignore-file removed, anything .graphignore names.
//
// Git has already inspected the broader tree to produce these paths. Re-applying the corpus's
// own ignore verdict here is the output boundary: it keeps a denied path away from the needle
// index's content reader and therefore out of search blocks. Using the same matcher rather than
// a second predicate keeps a caller's --include-file meaning the same thing on both routes.
func newSearchNeedleGrep(ctx context.Context, repoRoot, treeish string, ignores ignoreMatcher) func(string) ([]string, bool) {
	return func(pattern string) ([]string, bool) {
		paths, grepErr := gitutil.GrepFixedStringPaths(ctx, repoRoot, treeish, pattern)
		// nil ledger: these paths come from Git's whole-tree grep, not from the
		// corpus listing, so what is dropped here was never disclosed as corpus.
		return filterIgnoredPaths(paths, ignores, nil), grepErr == nil
	}
}

func openSearchContentReader(
	ctx context.Context,
	repo, commit string,
	useHead bool,
	ignoreFiles, includeFiles []string,
	maxParseBytes int,
) (contentReader, func() error, error) {
	// The content reader's cap is a ceiling on what a search RESULT can show,
	// not a mirror of the parser's eligibility cutoff: a file too large to
	// parse into the graph can still surface as a lexical (git-tree-grep)
	// match, and that match still needs its snippet read. So the floor here
	// stays the package default regardless of a caller-LOWERED MaxParseBytes
	// -- only a caller-RAISED MaxParseBytes should raise it further, else a
	// file the raised limit let into the snapshot would be refused right back
	// out during ranking/snippet reads. A negative resolved value means
	// "uncapped" (resolveMaxParseBytes's documented escape hatch) and must
	// stay uncapped rather than be floored back up to the default.
	resolvedMaxParseBytes := resolveMaxParseBytes(maxParseBytes)
	maxBytes := resolvedMaxParseBytes
	if resolvedMaxParseBytes >= 0 {
		maxBytes = max(defaultMaxParseBytes, resolvedMaxParseBytes)
	}
	if useHead {
		treePathPrefix, err := gitutil.RepoPrefix(ctx, repo)
		if err != nil {
			return nil, nil, err
		}
		batch, err := gitutil.NewBatchFileReader(ctx, repo, commit)
		if err != nil {
			return nil, nil, err
		}
		// Snippet and body reads never need a file the indexer refuses to parse,
		// so the reader declines it rather than materializing it twice over. Use
		// the wider of the package default and the caller's resolved search
		// limit: if a caller raised MaxParseBytes, a file the raised limit lets
		// into the snapshot must not then be refused here during ranking/snippet
		// reads.
		batch.SetMaxBytes(int64(maxBytes))
		limited := gitutil.NewLimitedFileReader(ctx, repo, commit, int64(maxBytes))
		read := func(path string) (string, bool) {
			if !batch.IsPathSafe(path) {
				// Same ceiling as the batch reader above. The shared exact-object
				// fallback exists because the batch protocol cannot carry a
				// newline-bearing path, not to exempt that path from the cap:
				// the file's name is chosen by the repository being searched,
				// so an unbounded read here would be a name-shaped hole in the
				// bound. One reader also shares the component traversal allowance
				// and prefix cache across all requested files.
				result, err := limited.ReadFile(treePathPrefix + path)
				return result.Content, err == nil && result.Status == gitutil.LimitedFileContent
			}
			content, ok, err := batch.ReadFile(path)
			return content, ok && err == nil
		}
		closeReaders := func() error {
			return errors.Join(batch.Close(), limited.Close())
		}
		return read, closeReaders, nil
	}
	opened, err := openSource(ctx, repo, "", sourceOptions{
		ignoreFiles:  ignoreFiles,
		includeFiles: includeFiles,
		maxReadBytes: maxBytes,
	})
	if err != nil {
		return nil, nil, err
	}
	return opened.read, opened.close, nil
}

func defaultSearchIndexedFiles(topK int) int {
	// Shallow interactive searches keep the original cold-start bound. Deeper
	// rankings need a wider file pool or preselection becomes the recall limit
	// before TopK and per-file diversity can take effect.
	//
	// Do NOT raise these bounds hoping for recall: MEASURED THE OPPOSITE. On 52
	// SWE-bench Multilingual instances disjoint from the tuning suite, distilled
	// queries, top-k 20 — gold-file recall by pool size:
	//
	//	 96 (default)  76.9%
	//	256            73.1%   (-2 instances)
	//	1024           69.2%   (-4 instances)
	//
	// Preselection is a useful FILTER, not the recall limit. A wider pool adds
	// weakly-matching candidates that compete for the same TopK slots and displace
	// the gold file: at 1024 the losses were PHP/laravel, Java/lucene, Ruby/fastlane,
	// Ruby/fluentd and TS/vue. Rust alone improves (33% -> 50%), so the tradeoff is
	// per-language and negative on aggregate.
	return minInt(
		deepSearchMaxIndexedFiles,
		maxInt(
			defaultSearchMaxIndexedFiles,
			minInt(topK, deepSearchMaxIndexedFiles)*searchIndexedFilesPerResult,
		),
	)
}

func fitSearchResultsToBudget(results []SearchResult, q searchQuery, budget int) ([]SearchResult, int, int, int) {
	originalCount := len(results)
	if budget <= 0 || len(results) == 0 {
		return results, serializedSearchResultBytes(results), 0, 0
	}

	for count := len(results); count > 0; count-- {
		// The per-result share is a CAP as well as a floor: no single result may eat the
		// budget, which is what keeps a huge signature or region from starving the rest of
		// the ranking. Deliberate whole-body snippets are added afterwards, by
		// allocateSearchSnippets, which is bounded separately.
		//
		// The per-result floor is a fixed 256 bytes, not budget/count: below that a result
		// degrades to a single truncated line and stops being usable, so the fitter drops
		// whole results (reported in results_dropped_by_budget) rather than keeping a
		// larger number of unusable ones.
		perResult := maxInt(256, (budget-2-(count-1))/count)
		compacted := make([]SearchResult, count)
		truncated := 0
		for index := range compacted {
			compacted[index], _ = compactSearchResultToBytes(results[index], q, perResult)
			if compacted[index].Snippet != results[index].Snippet || compacted[index].Signature != results[index].Signature {
				truncated++
			}
		}
		resultBytes := serializedSearchResultBytes(compacted)
		if resultBytes <= budget {
			return compacted, resultBytes, originalCount - count, truncated
		}
	}
	return []SearchResult{}, serializedSearchResultBytes([]SearchResult{}), originalCount, 0
}

func compactSearchResultToBytes(result SearchResult, q searchQuery, budget int) (SearchResult, int) {
	if size := serializedSearchResultBytes(result); size <= budget {
		return result, size
	}
	result.Signature = truncateSearchText(result.Signature, 192, q)
	if size := serializedSearchResultBytes(result); size <= budget {
		return result, size
	}

	lines := strings.Split(result.Snippet, "\n")
	focus := result.FocusLine - result.SnippetStartLine
	if focus < 0 || focus >= len(lines) {
		focus = len(lines) / 2
	}
	bestSpan, bestBalance := 0, len(lines)+1
	var best SearchResult
	bestSize := 0
	for left := 0; left <= focus; left++ {
		for right := focus; right < len(lines); right++ {
			candidate := result
			candidate.SnippetStartLine = result.SnippetStartLine + left
			candidate.SnippetEndLine = result.SnippetStartLine + right
			candidate.Snippet = strings.Join(lines[left:right+1], "\n")
			size := serializedSearchResultBytes(candidate)
			if size > budget {
				continue
			}
			span := right - left + 1
			balance := (focus - left) - (right - focus)
			if balance < 0 {
				balance = -balance
			}
			if span > bestSpan || (span == bestSpan && balance < bestBalance) {
				best, bestSize = candidate, size
				bestSpan, bestBalance = span, balance
			}
		}
	}
	if bestSpan > 0 {
		return best, bestSize
	}
	candidate := result
	candidate.SnippetStartLine = result.SnippetStartLine + focus
	candidate.SnippetEndLine = candidate.SnippetStartLine
	candidate.Snippet = truncateSearchText(lines[focus], maxInt(16, budget/3), q)
	return candidate, serializedSearchResultBytes(candidate)
}

func truncateSearchText(value string, maxBytes int, q searchQuery) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	center := len(value) / 2
	lower := strings.ToLower(value)
	for _, term := range q.terms {
		if index := strings.Index(lower, term); index >= 0 {
			center = index + len(term)/2
			break
		}
	}
	window := maxBytes - len("... ") - len(" ...")
	if window <= 0 {
		return ""
	}
	start := maxInt(0, center-window/2)
	end := minInt(len(value), start+window)
	start = maxInt(0, end-window)
	for start < end && !utf8RuneStart(value[start]) {
		start++
	}
	for end > start && end < len(value) && !utf8RuneStart(value[end]) {
		end--
	}
	if start >= end {
		return ""
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "... "
	}
	if end < len(value) {
		suffix = " ..."
	}
	return prefix + value[start:end] + suffix
}

func utf8RuneStart(value byte) bool {
	return value&0xc0 != 0x80
}

func serializedSearchResultBytes(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}

type searchFileCandidate struct {
	path            string
	score           float64
	legacyScore     float64
	legacyEligible  bool
	pathScore       float64
	contentScore    float64
	constraintScore float64
	matchedTerms    []bool
	// matchedWeight is the query weight this file matched, kept so the coverage share of
	// the file score can be normalized AFTER the scan — by then the terms no file matched
	// are known and can be left out of the denominator. See applySearchFileCoverage.
	matchedWeight float64
}

// applySearchFileCoverage adds the query-coverage share of the preselection score once the
// scan is done. The denominator is the weight of the terms at least one file matched, not of
// everything the caller typed: a term the corpus does not contain would otherwise shrink the
// coverage share of every file and re-order them against their path evidence, which is how a
// single nonsense word used to change which files got indexed at all.
func applySearchFileCoverage(files []searchFileCandidate, q searchQuery, matchedTerms []bool) {
	queryWeight := 0.0
	for index, term := range q.terms {
		if index < len(matchedTerms) && matchedTerms[index] {
			queryWeight += q.weights[term]
		}
	}
	if queryWeight <= 0 {
		return
	}
	for index := range files {
		coverageBonus := 4 * files[index].matchedWeight / queryWeight
		files[index].matchedTerms = append([]bool(nil), files[index].matchedTerms...)
		files[index].contentScore += coverageBonus
		files[index].legacyScore += coverageBonus
		files[index].score += coverageBonus
	}
}

type searchFileSelection struct {
	files                     []string
	sparseFiles               []string
	sparseCandidates          []searchCandidate
	sparseDF                  map[string]int
	sparsePrecomputedFiles    map[string]bool
	sparseDocumentCount       int
	sparseDocumentLength      int
	sparseFilesContentRead    int
	repoRoot                  string
	repoKey                   string
	commit                    string
	tree                      string
	warnings                  []ProviderWarning
	filesScanned              int
	filesContentRead          int
	preselectionBackend       string
	preselectionPasses        int
	preselectionFilesExamined int
	preselectionConfidence    float64
	preselectionCoverage      float64
	preselectionDiversity     float64
	preselectionWidened       bool
	preselectionBounded       bool
	// allFiles is every file preselection discovered, kept so a repository-wide literal lookup has
	// a corpus to fall back on when it is small enough to read (search_needle.go). It costs one
	// slice of paths, which the sparse file list already pays for.
	allFiles []string
	// termPostings is the per-query-term path index, filled in ONLY when the content pass saw the
	// whole corpus. When preselection short-circuited through Git it is empty and the lookup uses
	// Git instead — a truncated posting list would let a block state a repository-wide total it
	// cannot know.
	termPostings *searchTermPostings
	// gitGrepUsable records that Git answered a grep for this repository, so the lookup may ask it
	// again for a needle the posting lists do not cover. gitGrepTreeish is the tree that grep must
	// run against — empty means the working tree, which is what a worktree search indexes.
	gitGrepUsable  bool
	gitGrepTreeish string
	// repoIgnored is the listing's disclosure of what the repository's own ignore
	// rules removed, carried through so every response shape can report it.
	repoIgnored *RepoIgnoreReport
	// ignores is the exclude stack the corpus listing was filtered with, kept so the routes that
	// reach OUTSIDE that listing can apply the same verdict. Only one does: the needle index's
	// `git grep`, which Git answers over the whole tree. Carrying the matcher rather than
	// re-deriving a predicate is what keeps a caller's --include-file meaning the same thing on
	// both routes.
	ignores ignoreMatcher
}

type searchScanTelemetry struct {
	backend       string
	passes        int
	filesExamined int
}

func preselectSearchFiles(
	ctx context.Context,
	repo string,
	q, sparseQuery searchQuery,
	options SearchOptions,
	snapshotOptions ProviderSnapshotOptions,
	preindexedSnapshot ProviderSnapshot,
	preindexCacheHit bool,
) (searchFileSelection, error) {
	// This is the one prepareSource caller that reads sourceContext.repoIgnored
	// (into selection.repoIgnored below, then SearchResponse.RepoIgnored); every
	// other caller discards it, so this is where the ledger's accounting cost is
	// worth paying.
	snapshotOptions.trackRepoIgnored = true
	source, err := prepareSource(ctx, repo, snapshotOptions)
	if err != nil {
		return searchFileSelection{}, err
	}
	if source.close != nil {
		defer source.close()
	}
	// Keep the exact matcher that admitted source.paths. Re-reading policy files
	// here could give the whole-tree Git-grep route a different verdict and let
	// it name a path the corpus listing denied, even when caching is bypassed.
	ignores := source.ignores
	selection := searchFileSelection{
		ignores:                   ignores,
		repoRoot:                  source.absRepo,
		repoKey:                   source.key,
		commit:                    source.commit,
		tree:                      source.tree,
		warnings:                  append([]ProviderWarning{}, source.warnings...),
		repoIgnored:               source.repoIgnored,
		filesScanned:              len(source.paths),
		preselectionBackend:       "inventory",
		preselectionPasses:        1,
		preselectionFilesExamined: len(source.paths),
		sparseFiles:               append([]string(nil), source.paths...),
		sparseDF:                  make(map[string]int, len(sparseQuery.terms)),
		sparsePrecomputedFiles:    make(map[string]bool),
		allFiles:                  append([]string(nil), source.paths...),
		termPostings:              newSearchTermPostings(),
		// A resolved commit is what says "this is a Git repository", so a needle the posting lists
		// cannot price can be answered exactly by one `git grep`. An empty treeish means the working
		// tree, which is what a worktree search indexes; a HEAD search must grep the commit itself.
		gitGrepUsable: source.commit != "",
	}
	if !options.Worktree {
		selection.gitGrepTreeish = source.commit
	}
	// Interactive committed-tree search can ask Git's optimized object-store
	// scanner for every eligible content match in one fixed-string batch. Keep
	// every matched/path-evidenced file: MaxIndexedFiles is a cold selective-
	// indexing guard, not a retrieval recall cap once the graph is preindexed.
	// Deeper sparse search retains the exhaustive path until corpus statistics
	// can be persisted with the preindex.
	//
	// Tree identity is what makes the preindexed graph exact for source's
	// current HEAD; the grep below runs against source.commit directly, so a
	// same-tree preindex built at a different commit is still an exact match.
	exactFullPreindex := preindexCacheHit &&
		preindexedSnapshot.Header.Tree == source.tree &&
		preindexedSnapshot.Header.RepoKey == source.key
	grepPatterns, grepSafe := searchGitGrepPatterns(q.terms)
	if exactFullPreindex && !options.Worktree && !options.Deep && grepSafe {
		matches, grepErr := gitutil.GrepTreePaths(ctx, source.absRepo, source.commit, grepPatterns)
		if grepErr == nil {
			// This branch deliberately keeps every matched file rather than
			// honouring MaxIndexedFiles (see above), so the bridge gets its own
			// budget rather than a remainder of a cap this path does not apply.
			selection.files = bridgeRegistrationHandlerFiles(ctx, source, committedSearchFiles(source.paths, matches, q), searchRegistrationBridgeMaxHandlers)
			selection.sparseFiles = append([]string(nil), selection.files...)
			selection.preselectionBackend = "git-tree-grep"
			selection.preselectionPasses = 1
			selection.preselectionFilesExamined = len(source.paths)
			// Git scanned content but returned only paths, so the provider has no posting lists.
			// Git answered once, so it can answer again for a single needle.
			selection.gitGrepUsable = true
			selection.gitGrepTreeish = source.commit
			return selection, nil
		}
	}
	if options.IndexAllFiles || len(source.paths) <= options.MaxIndexedFiles {
		selection.files = append([]string(nil), source.paths...)
		return selection, nil
	}
	// Which terms any file matches is not known until the scan is done, so the coverage
	// share of every file score is added afterwards, over the matched terms only
	// (applySearchFileCoverage). Until then a file carries its matched weight unnormalized.
	matchedAnywhere := make([]bool, len(q.terms))
	matcher := newSearchTermMatcher(q.terms)
	scanPaths := source.paths
	usedGitIndexPreselection := false
	if shouldUseGitGrepPreselection(options.Worktree, len(source.paths)) {
		matches, grepErr := gitutil.GrepIndexMatches(ctx, source.absRepo, searchGitGrepPreselectionPatterns(q), 32)
		tracked, trackedErr := gitutil.ListIndexFiles(ctx, source.absRepo)
		if grepErr == nil && trackedErr == nil {
			usedGitIndexPreselection = true
			allowed := make(map[string]bool, len(source.paths))
			for _, filePath := range source.paths {
				allowed[filePath] = true
			}
			trackedSet := make(map[string]bool, len(tracked))
			for _, filePath := range tracked {
				trackedSet[filePath] = true
			}
			termMatches := make(map[string][]bool)
			for _, match := range matches {
				if !allowed[match.Path] {
					continue
				}
				seen := termMatches[match.Path]
				if seen == nil {
					seen = make([]bool, len(q.terms))
				}
				for index, matched := range matcher.match(match.Text) {
					seen[index] = seen[index] || matched
				}
				termMatches[match.Path] = seen
			}
			provisional := make([]searchFileCandidate, 0, len(termMatches)+16)
			grepMatchedAnywhere := make([]bool, len(q.terms))
			for filePath, seen := range termMatches {
				matchedWeight := 0.0
				for index, matched := range seen {
					if matched {
						matchedWeight += q.weights[q.terms[index]]
						grepMatchedAnywhere[index] = true
					}
				}
				pathScore := pathSearchScore(q, filePath)
				contentScore := matchedWeight
				provisional = append(provisional, searchFileCandidate{
					path:          filePath,
					pathScore:     2 * pathScore,
					contentScore:  contentScore,
					matchedTerms:  append([]bool(nil), seen...),
					score:         2*pathScore + contentScore + searchPathPrior(q, filePath),
					matchedWeight: matchedWeight,
				})
			}
			untracked := make([]string, 0, 16)
			for _, filePath := range source.paths {
				if !trackedSet[filePath] {
					untracked = append(untracked, filePath)
					continue
				}
				if _, exists := termMatches[filePath]; !exists {
					if pathScore := pathSearchScore(q, filePath); pathScore > 0 {
						provisional = append(provisional, searchFileCandidate{
							path:      filePath,
							pathScore: 2 * pathScore,
							score:     2*pathScore + searchPathPrior(q, filePath),
						})
					}
				}
			}
			applySearchFileCoverage(provisional, q, grepMatchedAnywhere)
			sort.Slice(provisional, func(i, j int) bool {
				if provisional[i].score != provisional[j].score {
					return provisional[i].score > provisional[j].score
				}
				return canonicalSearchPathLess(provisional[i].path, provisional[j].path)
			})
			poolLimit := len(provisional)
			if threshold := (len(provisional)-1)/4 + 1; options.MaxIndexedFiles < threshold {
				poolLimit = options.MaxIndexedFiles * 4
			}
			scanPaths = make([]string, 0, poolLimit+len(untracked))
			selection.sparseFiles = make([]string, 0, len(provisional)+len(untracked))
			for _, candidate := range provisional {
				selection.sparseFiles = append(selection.sparseFiles, candidate.path)
			}
			selection.sparseFiles = append(selection.sparseFiles, untracked...)
			for _, candidate := range provisional[:poolLimit] {
				scanPaths = append(scanPaths, candidate.path)
			}
			scanPaths = append(scanPaths, untracked...)
			if len(scanPaths) == 0 {
				scanPaths = source.paths
			}
		}
	}
	var sparseMu sync.Mutex
	var contentReadMu sync.Mutex
	var matchedMu sync.Mutex
	contentReads := 0
	scoreFile := func(filePath string) (searchFileCandidate, bool) {
		if err := ctx.Err(); err != nil {
			return searchFileCandidate{}, false
		}
		content, ok := source.read(filePath)
		if ok {
			contentReadMu.Lock()
			contentReads++
			contentReadMu.Unlock()
		}
		if !ok || strings.IndexByte(content, 0) >= 0 {
			return searchFileCandidate{}, false
		}
		if options.Deep && len(sparseQuery.terms) > 0 && len(content) <= maxSparseSearchFileBytes {
			lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
			if len(lines) > 1 || lines[0] != "" {
				language := ""
				if spec, detected := languageForContent(filePath, content); detected {
					language = spec.language
				}
				fileSparse, documentCount, documentLength := sparseCandidatesForFile(
					sparseQuery, filePath, language, lines, options,
				)
				sparseMu.Lock()
				selection.sparseCandidates = append(selection.sparseCandidates, fileSparse...)
				selection.sparseDocumentCount += documentCount
				selection.sparseDocumentLength += documentLength
				selection.sparseFilesContentRead++
				selection.sparsePrecomputedFiles[filePath] = true
				for _, candidate := range fileSparse {
					for term := range candidate.termCounts {
						selection.sparseDF[term]++
					}
				}
				sparseMu.Unlock()
			}
		}
		pathScore := 2 * pathSearchScore(q, filePath)
		matchedWeight := 0.0
		matched := matcher.match(content)
		for index, hit := range matched {
			if hit {
				matchedWeight += q.weights[q.terms[index]]
			}
		}
		contentScore := matchedWeight
		constraintScore := searchFileConstraintEvidence(filePath+"\n"+content, q.constraints)
		legacyEligible := pathScore > 0 || contentScore > 0
		if !legacyEligible && constraintScore == 0 {
			return searchFileCandidate{}, false
		}
		matchedMu.Lock()
		for index, hit := range matched {
			matchedAnywhere[index] = matchedAnywhere[index] || hit
		}
		matchedMu.Unlock()
		// The posting lists are a by-product of a match this loop already computed. Recording them
		// here is what makes a repository-wide literal lookup free later on.
		selection.termPostings.record(filePath, matched, q.terms)
		legacyScore := pathScore + contentScore + searchPathPrior(q, filePath)
		return searchFileCandidate{
			path:            filePath,
			legacyScore:     legacyScore,
			legacyEligible:  legacyEligible,
			pathScore:       pathScore,
			contentScore:    contentScore,
			constraintScore: constraintScore,
			matchedTerms:    append([]bool(nil), matched...),
			score:           legacyScore + constraintScore,
			matchedWeight:   matchedWeight,
		}, true
	}
	workers := 1
	if options.Worktree {
		workers = minInt(8, maxInt(1, runtime.GOMAXPROCS(0)))
	}
	files := scoreSearchPaths(ctx, scanPaths, workers, scoreFile)
	if err := ctx.Err(); err != nil {
		return searchFileSelection{}, err
	}
	applySearchFileCoverage(files, q, matchedAnywhere)
	legacyFiles := make([]searchFileCandidate, 0, len(files))
	for _, file := range files {
		if file.legacyEligible {
			legacyFiles = append(legacyFiles, file)
		}
	}
	progressive := options.progressivePreselection && len(legacyFiles) > options.MaxIndexedFiles
	if progressive {
		evidence := make([]searchPreselectionEvidence, len(files))
		for index, file := range files {
			evidence[index] = searchPreselectionEvidence{
				Path:            file.path,
				OriginalScore:   file.score,
				PathScore:       file.pathScore,
				ContentScore:    file.contentScore,
				ConstraintScore: file.constraintScore,
				MatchedTerms:    append([]bool(nil), file.matchedTerms...),
			}
		}
		assessment := selectProgressiveSearchFiles(
			fuseSearchPreselectionEvidence(evidence), matchedAnywhere, q, options.MaxIndexedFiles,
		)
		selection.files = assessment.Files
		selection.preselectionPasses = assessment.Passes
		selection.preselectionConfidence = assessment.Confidence
		selection.preselectionCoverage = assessment.Coverage
		selection.preselectionDiversity = assessment.Diversity
		selection.preselectionWidened = assessment.Widened
		selection.preselectionBounded = assessment.Bounded
	} else {
		sort.Slice(legacyFiles, func(i, j int) bool {
			if legacyFiles[i].legacyScore != legacyFiles[j].legacyScore {
				return legacyFiles[i].legacyScore > legacyFiles[j].legacyScore
			}
			return legacyFiles[i].path < legacyFiles[j].path
		})
		if len(legacyFiles) > options.MaxIndexedFiles {
			legacyFiles = legacyFiles[:options.MaxIndexedFiles]
		}
		selection.files = make([]string, len(legacyFiles))
		for index, file := range legacyFiles {
			selection.files[index] = file.path
		}
		selection.preselectionPasses = 1
	}
	// An explicit MaxIndexedFiles is an exact compatibility limit, so the bridge
	// spends only what preselection left unused.
	selection.files = bridgeRegistrationHandlerFiles(ctx, source, selection.files, options.MaxIndexedFiles-len(selection.files))
	selection.filesContentRead = contentReads
	selection.preselectionBackend = "go-content"
	selection.preselectionFilesExamined = len(scanPaths)
	if usedGitIndexPreselection {
		selection.preselectionBackend = "git-index-grep+go-content"
		if !progressive {
			selection.preselectionPasses++
		}
		selection.preselectionFilesExamined += len(source.paths)
		// The content pass ran over a Git-narrowed pool, so the posting lists cover only part of
		// the corpus. Discard them rather than let a block compute a repository-wide total from a
		// subset — Git is here, so the exact answer is one grep away.
		selection.termPostings = newSearchTermPostings()
		selection.gitGrepUsable = true
		selection.gitGrepTreeish = ""
	}
	return selection, nil
}

const (
	// searchRegistrationBridgeMaxHandlers is the ceiling on how many distinct
	// handlers one preselection will chase — and, since each one contributes at
	// most its defining file, on how many files the bridge may add. The caller's
	// unspent MaxIndexedFiles budget narrows it further.
	// searchRegistrationBridgeMaxParses bounds how many candidate files it may
	// parse to tell a definition from a call site, and
	// searchRegistrationBridgeScanLimit how much of the corpus it may read.
	searchRegistrationBridgeMaxHandlers = 8
	searchRegistrationBridgeMaxParses   = 64
	searchRegistrationBridgeScanLimit   = 50_000
)

// bridgeRegistrationHandlerFiles adds the source files that define the handlers
// named by the command tables already in the selection.
//
// A command verb reaches its implementation only through a registration table
// (commands/<verb>.json -> "function": handler); the verb never appears in the
// handler body. The provider indexes the verb as a searchable ALIAS of the
// handler symbol, but it can only do that for a handler it parsed, and on a
// repository above MaxIndexedFiles the selective snapshot is built from these
// preselected files alone. A query for the verb then matches the JSON table,
// which is exactly the file that cannot answer it, and the handler is never
// indexed — so the alias the provider exists to attach can never be attached.
//
// The bridge belongs here rather than in the provider: the provider's OnlyFiles
// scope is the contract that its graph and search's lexical scope describe the
// SAME files, so widening the corpus inside the provider would silently break
// it. Widening the selection keeps both sides derived from one list.
//
// It is deliberately cheap and rare. Nothing runs unless a selected file is a
// commands/*.json carrying a "function" field, it adds at most one file per
// handler and never more than budget files, and it stops as soon as every
// handler has been placed. budget is what the caller's MaxIndexedFiles has left
// unspent: that limit is documented as an exact compatibility ceiling when set
// explicitly, so a caller asking for a strict parsing ceiling must not be handed
// more files than it asked for — even to complete an alias.
//
// A file is added only when it DEFINES the handler, which is checked by parsing
// it. Accepting any file that merely applies the name would let call sites
// consume the budget while the definition sits further down the path order, and
// the handler symbol — the whole point of the bridge — would still be missing.
func bridgeRegistrationHandlerFiles(ctx context.Context, source sourceContext, selected []string, budget int) []string {
	if budget > searchRegistrationBridgeMaxHandlers {
		budget = searchRegistrationBridgeMaxHandlers
	}
	if budget <= 0 || len(selected) == 0 || len(selected) >= len(source.paths) {
		return selected
	}
	aliases := collectRegistrationAliases(selected, source.read)
	if len(aliases) == 0 {
		return selected
	}
	handlers := make([]string, 0, len(aliases))
	for handler := range aliases {
		handlers = append(handlers, handler)
	}
	sort.Strings(handlers)
	chosen := make(map[string]bool, len(selected))
	for _, filePath := range selected {
		chosen[filePath] = true
	}
	// A handler the selection already defines needs no file and must not hold a
	// slot: truncating first let a lexicographically earlier handler that was
	// already covered consume the only budget there was, and the handler that
	// actually needed bridging was never reached.
	handlers = unplacedRegistrationHandlers(handlers, placedRegistrationHandlers(handlers, selected, source.read))
	if len(handlers) > budget {
		handlers = handlers[:budget]
	}
	if len(handlers) == 0 {
		return selected
	}
	placed := make(map[string]bool, len(handlers))
	var added []string
	examined, parsed := 0, 0
	for _, filePath := range source.paths {
		// len(added) cannot exceed len(handlers), which the budget already
		// truncated, so the budget needs no second check here.
		if len(placed) == len(handlers) || examined >= searchRegistrationBridgeScanLimit || parsed >= searchRegistrationBridgeMaxParses {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if chosen[filePath] || !Supported(filePath) {
			continue
		}
		content, ok := source.read(filePath)
		if !ok {
			continue
		}
		examined++
		// The cheap textual screen runs first so the parse budget is spent only
		// on files that could plausibly hold the definition.
		candidate := false
		for _, handler := range handlers {
			if !placed[handler] && declaresAppliedIdentifier(content, handler) {
				candidate = true
				break
			}
		}
		if !candidate {
			continue
		}
		parsed++
		defined := topLevelFunctionNames(filePath, content)
		for _, handler := range handlers {
			if placed[handler] || !defined[handler] {
				continue
			}
			placed[handler] = true
			if !chosen[filePath] {
				chosen[filePath] = true
				added = append(added, filePath)
			}
		}
	}
	if len(added) == 0 {
		return selected
	}
	return append(append(make([]string, 0, len(selected)+len(added)), selected...), added...)
}

// topLevelFunctionNames returns the names of the top-level functions a file
// defines, so the bridge can tell the file that DEFINES a handler from the files
// that merely call it.
//
// It is deliberately narrower than "declares this name anywhere". A registration
// table names a bare identifier that the runtime dispatches to, which is a
// top-level function by construction; a method or a nested closure that happens
// to share the name is a different symbol, and accepting one would mark the
// handler placed, spend a slot on the wrong file, and hide the real definition
// that a later path would have supplied.
func topLevelFunctionNames(path, content string) map[string]bool {
	entities, _ := (TreeSitterParser{}).Parse(path, content)
	names := make(map[string]bool, len(entities))
	for _, entity := range entities {
		if entity.Kind != "function" || entity.Local {
			continue
		}
		names[entity.Name] = true
	}
	return names
}

// placedRegistrationHandlers reports which handlers the already-selected files
// define, so the bridge neither re-adds their file nor spends budget on them.
func placedRegistrationHandlers(handlers, selected []string, read contentReader) map[string]bool {
	placed := make(map[string]bool, len(handlers))
	for _, filePath := range selected {
		if len(placed) == len(handlers) {
			break
		}
		if !Supported(filePath) {
			continue
		}
		content, ok := read(filePath)
		if !ok {
			continue
		}
		candidate := false
		for _, handler := range handlers {
			if !placed[handler] && declaresAppliedIdentifier(content, handler) {
				candidate = true
				break
			}
		}
		if !candidate {
			continue
		}
		defined := topLevelFunctionNames(filePath, content)
		for _, handler := range handlers {
			if defined[handler] {
				placed[handler] = true
			}
		}
	}
	return placed
}

// unplacedRegistrationHandlers keeps the handlers still needing a file, in order.
func unplacedRegistrationHandlers(handlers []string, placed map[string]bool) []string {
	remaining := handlers[:0:0]
	for _, handler := range handlers {
		if !placed[handler] {
			remaining = append(remaining, handler)
		}
	}
	return remaining
}

// declaresAppliedIdentifier reports whether name occurs in content as a whole
// identifier applied to a parameter list, in a position that looks like a
// DECLARATION rather than a call. It is the cheap screen in front of the parse:
// what it admits is decided by topLevelFunctionNames, and what it rejects is
// never parsed at all.
//
// Rejecting calls matters for recall, not just cost. The parse budget is finite,
// and a widely-called handler has far more call sites than definitions; if call
// sites could pass the screen, enough of them lexically ahead of the definition
// would spend the whole budget and the bridge would fail on exactly the large
// repositories it exists for.
//
// The rule is deliberately permissive toward declarations and strict about
// calls: everything on the line before the name must read as a declaration's
// leading tokens — a return type, a qualifier, `func`/`function`/`static` — so
// any operator, paren, dot or bracket rules the occurrence out, as does a
// statement keyword or a line that ends in a semicolon (a call, or a C
// prototype, which is not the definition either).
func declaresAppliedIdentifier(content, name string) bool {
	if name == "" {
		return false
	}
	for offset := 0; offset <= len(content)-len(name); {
		at := strings.Index(content[offset:], name)
		if at < 0 {
			return false
		}
		start := offset + at
		end := start + len(name)
		offset = end
		if start > 0 && isJSIdentifierPart(content[start-1]) {
			continue
		}
		if cursor := skipSpace(content, end); cursor >= len(content) || content[cursor] != '(' {
			continue
		}
		if declarationLeadsIdentifier(content, start) {
			return true
		}
	}
	return false
}

// declarationStatementKeywords are the words that make an applied identifier a
// call however type-shaped the rest of the line looks.
var declarationStatementKeywords = map[string]bool{
	"return": true, "await": true, "yield": true, "new": true, "throw": true,
	"if": true, "while": true, "for": true, "switch": true, "case": true,
	"else": true, "typeof": true, "delete": true, "defer": true, "go": true,
}

// declarationLeadsIdentifier reports whether the text before an occurrence reads
// as the head of a declaration.
func declarationLeadsIdentifier(content string, start int) bool {
	lineStart := strings.LastIndexByte(content[:start], '\n') + 1
	lineEnd := len(content)
	if at := strings.IndexByte(content[start:], '\n'); at >= 0 {
		lineEnd = start + at
	}
	// A line that terminates is a call or a prototype; a definition's line
	// carries on into its body.
	if strings.HasSuffix(strings.TrimSpace(content[lineStart:lineEnd]), ";") {
		return false
	}
	prefix := strings.TrimSpace(content[lineStart:start])
	if prefix == "" {
		// A declaration may put its return type on the line above
		// (`void\ngetrangeCommand(client *c)`), which is ordinary C style. A
		// call at the start of its own line looks identical on this line alone,
		// so the line above decides: a declaration head there, or nothing.
		return declarationHeadText(previousNonEmptyLine(content, lineStart))
	}
	return declarationHeadText(prefix)
}

// previousNonEmptyLine returns the trimmed line above the one starting at
// lineStart, skipping blank lines.
func previousNonEmptyLine(content string, lineStart int) string {
	for lineStart > 0 {
		end := lineStart - 1
		begin := strings.LastIndexByte(content[:end], '\n') + 1
		if text := strings.TrimSpace(content[begin:end]); text != "" {
			return text
		}
		lineStart = begin
	}
	return ""
}

// declarationHeadText reports whether text reads as a declaration's leading
// tokens: a return type, a qualifier, `func`/`function`/`static`. Any operator,
// paren, dot or bracket rules it out, as does a statement keyword.
func declarationHeadText(text string) bool {
	if text == "" {
		return false
	}
	for index := 0; index < len(text); index++ {
		switch character := text[index]; character {
		case ' ', '\t', '*', '&', ':', '<', '>', ',', '~':
		default:
			if !isJSIdentifierPart(character) {
				return false
			}
		}
	}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == '\t' }) {
		if declarationStatementKeywords[word] {
			return false
		}
	}
	return true
}

func searchSnapshotMatchesSelection(snapshot ProviderSnapshot, selection searchFileSelection) bool {
	return snapshot.Header.Tree == selection.tree && snapshot.Header.RepoKey == selection.repoKey
}

func searchTermsSafeForGitGrep(terms []string) bool {
	for _, term := range terms {
		for index := 0; index < len(term); index++ {
			if term[index] < 0x20 || term[index] > 0x7e {
				return false
			}
		}
	}
	return true
}

var (
	searchASCIILowerAlternativesOnce sync.Once
	searchASCIILowerAlternatives     [128][]rune
)

// searchGitGrepPatterns makes Git's byte-oriented fixed-string preselection a
// conservative superset of Go's strings.ToLower matching. Some non-ASCII
// uppercase runes lower to ASCII (for example Turkish dotted İ -> i), while
// Git's -i behavior for those runes varies by platform and locale. Generate
// exact UTF-8 pattern alternatives from this Go runtime's Unicode tables. If
// the cartesian expansion would be excessive, return unsafe so the caller
// falls back to exhaustive content scoring without changing recall.
func searchGitGrepPatterns(terms []string) ([]string, bool) {
	if !searchTermsSafeForGitGrep(terms) {
		return nil, false
	}
	searchASCIILowerAlternativesOnce.Do(func() {
		for value := rune(128); value <= unicode.MaxRune; value++ {
			lower := unicode.ToLower(value)
			if lower >= 0 && lower < 128 {
				searchASCIILowerAlternatives[lower] = append(searchASCIILowerAlternatives[lower], value)
			}
		}
	})
	patterns := make([]string, 0, len(terms))
	seen := make(map[string]bool, len(terms))
	for _, term := range terms {
		variants := []string{""}
		for index := 0; index < len(term); index++ {
			alternatives := searchASCIILowerAlternatives[term[index]]
			if len(alternatives) == 0 {
				for variantIndex := range variants {
					variants[variantIndex] += term[index : index+1]
				}
				continue
			}
			if len(variants)*(len(alternatives)+1) > maxSearchGitGrepPatterns-len(patterns) {
				return nil, false
			}
			expanded := make([]string, 0, len(variants)*(len(alternatives)+1))
			for _, prefix := range variants {
				expanded = append(expanded, prefix+term[index:index+1])
				for _, alternative := range alternatives {
					expanded = append(expanded, prefix+string(alternative))
				}
			}
			variants = expanded
		}
		for _, variant := range variants {
			if seen[variant] {
				continue
			}
			seen[variant] = true
			patterns = append(patterns, variant)
			if len(patterns) > maxSearchGitGrepPatterns {
				return nil, false
			}
		}
	}
	return patterns, true
}

func committedSearchFiles(paths, matches []string, q searchQuery) []string {
	allowed := make(map[string]bool, len(paths))
	for _, filePath := range paths {
		allowed[filePath] = true
	}
	matched := make(map[string]bool, len(matches))
	for _, match := range matches {
		if allowed[match] {
			matched[match] = true
		}
	}
	files := make([]searchFileCandidate, 0, len(matched)+16)
	for _, filePath := range paths {
		pathScore := pathSearchScore(q, filePath)
		if !matched[filePath] && pathScore == 0 {
			continue
		}
		files = append(files, searchFileCandidate{
			path:  filePath,
			score: 2*pathScore + searchPathPrior(q, filePath),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].score != files[j].score {
			return files[i].score > files[j].score
		}
		return canonicalSearchPathLess(files[i].path, files[j].path)
	})
	selected := make([]string, len(files))
	for index, file := range files {
		selected[index] = file.path
	}
	return selected
}

func scoreSearchPaths(
	ctx context.Context,
	paths []string,
	workers int,
	score func(string) (searchFileCandidate, bool),
) []searchFileCandidate {
	initialCapacity := minInt(len(paths), 1024)
	if workers <= 1 {
		files := make([]searchFileCandidate, 0, initialCapacity)
		for _, filePath := range paths {
			if ctx.Err() != nil {
				break
			}
			if candidate, ok := score(filePath); ok {
				files = append(files, candidate)
			}
		}
		return files
	}

	jobs := make(chan string)
	results := make(chan searchFileCandidate)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for filePath := range jobs {
				if ctx.Err() != nil {
					return
				}
				if candidate, ok := score(filePath); ok {
					select {
					case results <- candidate:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, filePath := range paths {
			select {
			case jobs <- filePath:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wait.Wait()
		close(results)
	}()
	files := make([]searchFileCandidate, 0, initialCapacity)
	for candidate := range results {
		files = append(files, candidate)
	}
	return files
}

func shouldUseGitGrepPreselection(worktree bool, fileCount int) bool {
	return worktree && fileCount >= minGitGrepPreselectionFiles
}

func searchPreselectionPatterns(q searchQuery) []string {
	const (
		maxPatterns            = 12
		maxPrimaryPatterns     = 8
		maxInitialCodePatterns = 4
		maxOriginalPatterns    = 4
		maxMorphPatterns       = 2
		maxFragmentTerms       = 2
	)
	patterns := make([]string, 0, maxPatterns)
	seen := make(map[string]bool, maxPatterns)
	appendTier := func(limit int, include func(float64) bool) {
		if limit <= 0 {
			return
		}
		added := 0
		for _, term := range q.terms {
			if seen[term] || !include(q.weights[term]) {
				continue
			}
			patterns = append(patterns, term)
			seen[term] = true
			added++
			if added == limit || len(patterns) == maxPatterns {
				return
			}
		}
	}
	appendTier(maxInitialCodePatterns, func(weight float64) bool {
		return weight >= 1.25
	})
	appendTier(maxOriginalPatterns, func(weight float64) bool {
		return weight == 1
	})
	appendTier(maxPrimaryPatterns-len(patterns), func(weight float64) bool {
		return weight >= 1.25 || weight == 1
	})
	appendTier(maxMorphPatterns, func(weight float64) bool {
		return weight < 1
	})
	appendTier(maxFragmentTerms, func(weight float64) bool {
		return weight > 1 && weight < 1.25
	})
	appendTier(maxPatterns-len(patterns), func(weight float64) bool {
		return weight >= 1.25 || weight == 1
	})
	return patterns
}

func searchGitGrepPreselectionPatterns(q searchQuery) []string {
	patterns := searchPreselectionPatterns(q)
	// Each additional fixed-string pattern makes Git scan and emit more of a
	// large index. Preserve one derived morphological/fragment fallback rather
	// than letting direct terms consume the entire bounded pattern budget.
	if len(patterns) <= 6 {
		return patterns
	}
	direct := make([]string, 0, 6)
	derived := make([]string, 0, 1)
	for _, pattern := range patterns {
		if q.weights[pattern] >= 1 {
			direct = append(direct, pattern)
		} else {
			derived = append(derived, pattern)
		}
	}
	limit := 6
	if len(derived) > 0 {
		limit--
	}
	if len(direct) > limit {
		direct = direct[:limit]
	}
	if len(derived) > 0 {
		direct = append(direct, derived[0])
	}
	return direct
}

func candidatesForFile(q searchQuery, filePath, language string, lines []string, symbols []SymbolRecord, options SearchOptions) []searchCandidate {
	var out []searchCandidate
	covered := make([]bool, len(lines)+1)
	for _, symbol := range symbols {
		start, end := clampRegion(symbol.StartLine, symbol.EndLine, len(lines))
		if start == 0 {
			continue
		}
		searchStart := precedingDocumentationStart(lines, start, options.ContextLines)
		for line := searchStart; line <= end; line++ {
			covered[line] = true
		}
		if end-searchStart+1 <= options.MaxRegionLines {
			if candidate, ok := makeSearchCandidate(q, filePath, language, lines, searchStart, end, symbol, options.MaxSnippetLines); ok {
				out = append(out, candidate)
			}
			continue
		}
		regions := matchingLineRegions(q, lines, searchStart, end, options.ContextLines, options.MaxRegionLines)
		for _, region := range regions {
			attributed := attributeSearchRegion(q, lines, region[0], region[1], symbol, symbols)
			if candidate, ok := makeSearchCandidate(q, filePath, language, lines, region[0], region[1], attributed, options.MaxSnippetLines); ok {
				out = append(out, candidate)
			}
		}
		if len(regions) == 0 {
			focusedEnd := minInt(end, searchStart+options.MaxRegionLines-1)
			if candidate, ok := makeSearchCandidate(q, filePath, language, lines, searchStart, focusedEnd, symbol, options.MaxSnippetLines); ok {
				out = append(out, candidate)
			}
		}
	}

	var uncoveredHits []int
	for index, line := range lines {
		lineNumber := index + 1
		if !covered[lineNumber] && textMatchesSearchQuery(q, line) {
			uncoveredHits = append(uncoveredHits, lineNumber)
		}
	}
	for _, region := range uncoveredRegionsAroundHits(uncoveredHits, covered, len(lines), options.ContextLines, options.MaxRegionLines) {
		if candidate, ok := makeSearchCandidate(q, filePath, language, lines, region[0], region[1], SymbolRecord{}, options.MaxSnippetLines); ok {
			out = append(out, candidate)
		}
	}

	if len(out) == 0 && textMatchesSearchQuery(q, filepath.ToSlash(filePath)) {
		end := minInt(len(lines), maxInt(1, options.ContextLines*2+1))
		// A path can match only through weak derived fragments (compound
		// identifier parts or morphological variants) that pathSearchScore
		// does not treat as evidence. Such files produce no valid candidate
		// and must be skipped, not emitted as zero-value results.
		if candidate, ok := makeSearchCandidate(q, filePath, language, lines, 1, end, SymbolRecord{}, options.MaxSnippetLines); ok {
			candidate.baseScore += pathSearchScore(q, filePath)
			candidate.result.Signals = appendUnique(candidate.result.Signals, "path")
			out = append(out, candidate)
		}
	}
	return out
}

// attributeSearchRegion decides which symbol a matching region INSIDE a larger symbol should
// be credited to.
//
// A symbol too large to return whole is split into the regions that matched the query, and
// every one of those regions inherited the enclosing symbol's identity — including its
// name-match score. For a container that is wrong twice over: a 3,000-line class scores its
// name bonus at every matching line in the file, so an arbitrary region of `class Calculation`
// outranks the small method that actually implements the reported behaviour; and the result
// then reports `class Calculation` as the thing the agent is looking at.
//
// The correction is narrow, and it is a statement about evidence rather than a tuning knob: a
// region that does not even contain the declaration of the symbol it is named after did not
// match that name, so it is credited to the smallest CALLABLE inside the container that
// contains it. Regions that do include the declaration, and containers with no callable at
// that line (field blocks, constant tables), are left exactly as they were.
func attributeSearchRegion(
	q searchQuery, lines []string, start, end int, symbol SymbolRecord, symbols []SymbolRecord,
) SymbolRecord {
	if symbol.ID == "" || searchEnclosableSymbolKind(symbol.Kind) || start <= symbol.StartLine {
		return symbol
	}
	inner, ok := smallestSearchSymbolContainingLineWhere(
		symbols, searchFocusLine(q, lines, start, end),
		func(candidate SymbolRecord) bool {
			return searchEnclosableSymbolKind(candidate.Kind) &&
				candidate.StartLine >= symbol.StartLine && candidate.EndLine <= symbol.EndLine
		},
	)
	if !ok {
		return symbol
	}
	return inner
}

// sparseCandidatesForFile complements syntax-aware regions with fixed-width
// lexical windows. Syntax regions are precise but can hide the relevant part
// of a large symbol or prose-heavy file; overlapping windows preserve those
// locations for deeper rankings without changing the interactive search path.
func sparseCandidatesForFile(
	sparseQuery searchQuery,
	filePath, language string,
	lines []string,
	options SearchOptions,
) ([]searchCandidate, int, int) {
	if len(lines) == 0 {
		return nil, 0, 0
	}
	chunkLines := maxInt(1, options.MaxRegionLines)
	stride := minInt(sparseSearchChunkStrideLines, chunkLines)
	pathCounts, _ := sparseSearchTermCounts(filepath.ToSlash(filePath), sparseQuery.termSet)
	documentCount := 0
	documentLength := 0
	var out []searchCandidate
	for start := 1; start <= len(lines); start += stride {
		end := minInt(len(lines), start+chunkLines-1)
		text := filepath.ToSlash(filePath) + "\n" + strings.Join(lines[start-1:end], "\n")
		counts, length := sparseSearchTermCounts(text, sparseQuery.termSet)
		documentCount++
		documentLength += maxInt(1, length)
		if len(counts) > 0 {
			focus := sparseSearchFocusLine(sparseQuery, lines, start, end)
			snippetStart, snippetEnd := focusedSnippetRegion(start, end, focus, options.MaxSnippetLines)
			signals := []string{"sparse-region"}
			if len(pathCounts) > 0 {
				signals = append(signals, "path")
			}
			out = append(out, searchCandidate{
				result: SearchResult{
					FilePath:         filePath,
					StartLine:        start,
					EndLine:          end,
					FocusLine:        focus,
					SnippetStartLine: snippetStart,
					SnippetEndLine:   snippetEnd,
					Language:         language,
					Signals:          signals,
					Snippet:          "",
				},
				termCounts: counts,
				docLength:  length,
			})
		}
		if end == len(lines) {
			break
		}
	}
	return out, documentCount, documentLength
}

func sparseSearchFocusLine(q searchQuery, lines []string, start, end int) int {
	bestLine := start
	bestMatches := -1
	for line := start; line <= end; line++ {
		counts, _ := sparseSearchTermCounts(lines[line-1], q.termSet)
		matches := 0
		for _, count := range counts {
			matches += count
		}
		if matches > bestMatches {
			bestLine = line
			bestMatches = matches
		}
	}
	return bestLine
}

// attachSparseCandidateSymbols restores exact semantic identity for sparse
// windows whose best matching line is inside a parsed symbol. Sparse regions
// are built during file preselection, before the provider snapshot is
// available, so they otherwise cannot participate in semantic mirror dedup.
func attachSparseCandidateSymbols(candidates []searchCandidate, symbolsByFile map[string][]SymbolRecord) {
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.result.SymbolID != "" {
			continue
		}
		symbol, ok := smallestSearchSymbolContainingLine(
			symbolsByFile[candidate.result.FilePath], candidate.result.FocusLine,
		)
		if !ok {
			continue
		}
		if candidate.result.Language == "" {
			candidate.result.Language = symbol.Language
		}
		candidate.result.Kind = symbol.Kind
		candidate.result.SymbolID = symbol.ID
		candidate.result.SymbolName = symbol.Name
		candidate.result.QualifiedName = symbol.QualifiedName
		candidate.result.Signature = symbol.Signature
		candidate.aliases = append([]string(nil), symbol.Aliases...)
		candidate.result.SymbolStartLine = symbol.StartLine
		candidate.result.SymbolEndLine = symbol.EndLine
	}
}

func smallestSearchSymbolContainingLine(symbols []SymbolRecord, line int) (SymbolRecord, bool) {
	return smallestSearchSymbolContainingLineWhere(symbols, line, nil)
}

// smallestSearchSymbolContainingLineWhere is smallestSearchSymbolContainingLine restricted to
// the symbols a caller accepts (a nil filter accepts every symbol). It lets snippet allocation
// ask for the smallest enclosing CALLABLE while ranking keeps asking for the smallest symbol
// of any kind.
func smallestSearchSymbolContainingLineWhere(
	symbols []SymbolRecord, line int, keep func(SymbolRecord) bool,
) (SymbolRecord, bool) {
	bestSpan := int(^uint(0) >> 1)
	var best SymbolRecord
	found := false
	for _, symbol := range symbols {
		if symbol.ID == "" || line < symbol.StartLine || line > symbol.EndLine {
			continue
		}
		if keep != nil && !keep(symbol) {
			continue
		}
		span := symbol.EndLine - symbol.StartLine
		if !found || span < bestSpan || (span == bestSpan && symbol.StartLine > best.StartLine) {
			best = symbol
			bestSpan = span
			found = true
		}
	}
	return best, found
}

func hydrateSparseCandidates(candidates []searchCandidate, read contentReader) int {
	cache := make(map[string][]string)
	missing := make(map[string]bool)
	reads := 0
	for index := range candidates {
		candidate := &candidates[index]
		isSparse := false
		for _, signal := range candidate.result.Signals {
			if signal == "sparse-region" {
				isSparse = true
				break
			}
		}
		if candidate.result.Snippet != "" || !isSparse {
			continue
		}
		filePath := candidate.result.FilePath
		lines, cached := cache[filePath]
		if !cached && !missing[filePath] {
			content, ok := read(filePath)
			reads++
			if !ok || strings.IndexByte(content, 0) >= 0 {
				missing[filePath] = true
				continue
			}
			lines = strings.Split(strings.TrimSuffix(content, "\n"), "\n")
			cache[filePath] = lines
			if candidate.result.Language == "" {
				if spec, detected := languageForContent(filePath, content); detected {
					candidate.result.Language = spec.language
				}
			}
		}
		if len(lines) == 0 {
			continue
		}
		start, end := clampRegion(candidate.result.SnippetStartLine, candidate.result.SnippetEndLine, len(lines))
		if start > 0 {
			candidate.result.Snippet = strings.Join(lines[start-1:end], "\n")
		}
	}
	return reads
}

func scaleSearchCorpusValue(value, totalFiles, observedFiles int) int {
	if value <= 0 || totalFiles <= observedFiles || observedFiles <= 0 {
		return value
	}
	maxValue := int(^uint(0) >> 1)
	quotient, remainder := value/observedFiles, value%observedFiles
	if quotient > maxValue/totalFiles {
		return maxValue
	}
	scaled := quotient * totalFiles
	if remainder == 0 {
		return scaled
	}
	if remainder > (maxValue-scaled)/totalFiles {
		return maxValue
	}
	product := remainder * totalFiles
	extra := product / observedFiles
	if product%observedFiles != 0 {
		extra++
	}
	return scaled + extra
}

func scoreSparseCandidates(
	candidates []searchCandidate,
	q searchQuery,
	documentFrequency map[string]int,
	documentCount, documentLength int,
) {
	if len(candidates) == 0 || documentCount <= 0 {
		return
	}
	averageLength := float64(maxInt(1, documentLength)) / float64(documentCount)
	for i := range candidates {
		candidate := &candidates[i]
		candidate.score += searchPathPrior(q, candidate.result.FilePath)
		// cmm parity: score a sparse window on its symbol identity (name / qualified-name)
		// and path tokens, not only body-BM25 + path-prior. This is what lets a fix file whose
		// domain nouns live in its path/identifiers surface as a genuine ranked hit instead of
		// relevance-free RRF filler (measured whiffs: fmt core.h on_zero, druid *PostAggregator.java).
		// attachSparseCandidateSymbols now runs before this, so the symbol fields are populated.
		candidate.score += pathSearchScore(q, candidate.result.FilePath)
		if candidate.result.SymbolID != "" {
			nameScore, nameSignals := symbolSearchScore(q, SymbolRecord{
				Name:          candidate.result.SymbolName,
				QualifiedName: candidate.result.QualifiedName,
				Signature:     candidate.result.Signature,
				Kind:          candidate.result.Kind,
			})
			candidate.score += nameScore
			candidate.result.Signals = appendUnique(candidate.result.Signals, nameSignals...)
		}
		for _, term := range q.terms {
			frequency := candidate.termCounts[term]
			if frequency == 0 {
				continue
			}
			df := documentFrequency[term]
			inverse := math.Log(1 + (float64(documentCount-df)+0.5)/(float64(df)+0.5))
			denominator := float64(frequency) + 1.2*(0.25+0.75*float64(maxInt(1, candidate.docLength))/averageLength)
			candidate.score += q.weights[term] * inverse * (float64(frequency) * 2.2 / denominator)
		}
	}
}

func uncoveredRegionsAroundHits(hits []int, covered []bool, lineCount, context, maxLines int) [][2]int {
	baseRegions := regionsAroundHits(hits, 1, lineCount, context, maxLines)
	hitSet := make(map[int]bool, len(hits))
	for _, hit := range hits {
		hitSet[hit] = true
	}
	var out [][2]int
	for _, region := range baseRegions {
		runStart := 0
		runHasHit := false
		flush := func(end int) {
			if runStart > 0 && runHasHit {
				out = append(out, [2]int{runStart, end})
			}
			runStart = 0
			runHasHit = false
		}
		for line := region[0]; line <= region[1]; line++ {
			if line < len(covered) && covered[line] {
				flush(line - 1)
				continue
			}
			if runStart == 0 {
				runStart = line
			}
			runHasHit = runHasHit || hitSet[line]
		}
		flush(region[1])
	}
	return out
}

func precedingDocumentationStart(lines []string, symbolStart, limit int) int {
	start := symbolStart
	if limit <= 0 {
		return start
	}
	sawComment := false
	for line := symbolStart - 1; line >= 1 && symbolStart-line <= limit; line-- {
		trimmed := strings.TrimSpace(lines[line-1])
		isComment := strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "/*") ||
			strings.HasPrefix(trimmed, "*") ||
			strings.HasPrefix(trimmed, "--")
		if isComment {
			sawComment = true
			start = line
			continue
		}
		if trimmed == "" && sawComment {
			start = line
			continue
		}
		break
	}
	return start
}

func makeSearchCandidate(q searchQuery, filePath, language string, lines []string, start, end int, symbol SymbolRecord, maxSnippetLines int) (searchCandidate, bool) {
	start, end = clampRegion(start, end, len(lines))
	if start == 0 {
		return searchCandidate{}, false
	}
	regionText := strings.Join(lines[start-1:end], "\n")
	counts, length := searchTermCounts(regionText, q.termSet)
	focus := searchFocusLine(q, lines, start, end)
	if symbol.ID != "" {
		focus = maxInt(focus, symbol.StartLine)
	}
	// symbol.StartLine can fall outside [start, end] when the caller passed a truncated
	// window (e.g. a same-container-neighbor candidate clipped to a tiny --max-snippet-lines).
	// Clamp so FocusLine always satisfies the SearchResponse.Validate invariant
	// (StartLine <= FocusLine <= EndLine); otherwise the whole response is rejected with
	// "invalid search result at rank N" (reproducible with --max-snippet-lines 1).
	focus = minInt(maxInt(focus, start), end)
	snippetStart, snippetEnd := focusedSnippetRegion(start, end, focus, maxSnippetLines)
	snippet := strings.Join(lines[snippetStart-1:snippetEnd], "\n")
	pathScore := pathSearchScore(q, filePath)
	base := pathScore
	signals := []string{}
	if pathScore > 0 {
		signals = append(signals, "path")
	}
	if len(counts) > 0 {
		signals = append(signals, "body")
	}
	if symbol.ID != "" {
		nameScore, nameSignals := symbolSearchScore(q, symbol)
		base += nameScore
		signals = append(signals, nameSignals...)
	}
	if len(counts) == 0 && pathScore == 0 && len(signals) == 0 {
		return searchCandidate{}, false
	}
	base += searchPathPrior(q, filePath)
	return searchCandidate{
		result: SearchResult{
			FilePath:         filePath,
			StartLine:        start,
			EndLine:          end,
			FocusLine:        focus,
			SnippetStartLine: snippetStart,
			SnippetEndLine:   snippetEnd,
			Language:         language,
			Kind:             symbol.Kind,
			SymbolID:         symbol.ID,
			SymbolName:       symbol.Name,
			QualifiedName:    symbol.QualifiedName,
			Signature:        symbol.Signature,
			SymbolStartLine:  symbol.StartLine,
			SymbolEndLine:    symbol.EndLine,
			Signals:          appendUnique(nil, signals...),
			Snippet:          snippet,
		},
		termCounts: counts,
		docLength:  length,
		baseScore:  base,
		aliases:    append([]string(nil), symbol.Aliases...),
	}, true
}

func scoreSearchCandidates(candidates []searchCandidate, q searchQuery, fileDF map[string]int, fileCount int) {
	if len(candidates) == 0 {
		return
	}
	averageLength := 0.0
	for _, candidate := range candidates {
		averageLength += float64(maxInt(1, candidate.docLength))
	}
	averageLength /= float64(len(candidates))
	queryWeight := 0.0
	idf := make(map[string]float64, len(q.terms))
	for _, term := range q.terms {
		df := fileDF[term]
		weight := math.Log(1+(float64(fileCount-df)+0.5)/(float64(df)+0.5)) * q.weights[term]
		idf[term] = weight
		if df == 0 {
			// No indexed file contains this term, so no candidate can ever match it —
			// and, being maximally rare, it would carry the LARGEST idf of the query.
			// Charging it to the query weight would put full coverage out of reach and
			// tax every document that matched the rest of the query, purely because the
			// caller wrote one word the repository has never heard of. An unmatched term
			// contributes zero (BM25), never a penalty.
			continue
		}
		queryWeight += weight
	}
	for i := range candidates {
		candidate := &candidates[i]
		bm25 := 0.0
		coveredWeight := 0.0
		codeTokenBonus := 0.0
		for _, term := range q.terms {
			frequency := candidate.termCounts[term]
			if frequency == 0 {
				continue
			}
			weight := idf[term]
			coveredWeight += weight
			if q.weights[term] >= 2 {
				codeTokenBonus += 6 * q.weights[term]
			}
			denominator := float64(frequency) + 1.2*(0.25+0.75*float64(maxInt(1, candidate.docLength))/averageLength)
			bm25 += weight * (float64(frequency) * 2.2 / denominator)
		}
		// Data declarations (fields/constants) are near-degenerate BM25 docs: a
		// one-line declaration that repeats the query identifier outscores the
		// method whose body actually misbehaves (a caller's `private final Foo
		// foo;` outranking `Foo.go()`'s error handler). The declaration names
		// the thing; the defect lives in executable code — damp text evidence
		// from data declarations rather than banning them outright. Exception:
		// when the query is ABOUT this declaration — its name covers most of
		// the query's terms (definition-lookup intent) — the declaration is
		// the target, not a bystander.
		if searchKindClass(candidate.result.Kind) == searchKindData &&
			candidate.result.EndLine-candidate.result.StartLine <= 3 &&
			!searchResultHasSignal(candidate.result, "exact-symbol") &&
			!searchNameCoversQuery(candidate.result, q) {
			bm25 *= 0.4
			codeTokenBonus *= 0.5
		}
		coverage := 0.0
		if queryWeight > 0 {
			coverage = coveredWeight / queryWeight
		}
		candidate.score = candidate.baseScore + bm25 + 7*coverage + minFloat64(24, codeTokenBonus)
		candidate.score += searchNameCoverageWeight * searchNameTermCoverage(candidate.result, q, idf)
		// trail 18's kind/subject/dotted-call structural bonuses (orthogonal to the
		// caller-degree boost that trail 10 applies as a separate step). Centrality was
		// dropped in the merge: trail 10's caller-boost is the reviewed successor of the
		// same inbound-degree signal, so keeping both would double-count it.
		candidate.score += searchStructuralAdjustment(candidate, q)
		bonus, signals := searchCandidateAgreementCached(candidate, q.constraints)
		candidate.score += bonus
		candidate.result.Signals = appendUnique(candidate.result.Signals, signals...)
		if codeTokenBonus > 0 {
			candidate.result.Signals = appendUnique(candidate.result.Signals, "exact-code-token")
		}
		if coverage == 1 {
			candidate.score += 2
			candidate.result.Signals = appendUnique(candidate.result.Signals, "all-query-terms")
		}
	}
}

// searchNameCoversQuery reports whether the symbol's own name path matches at
// least half of the query's terms — the signature of a query that is about the
// symbol itself (find/define intent) rather than one that merely mentions it.
// searchNameTermCoverage is the idf-weighted share of the query's terms that the SYMBOL'S OWN NAME
// carries. It is separate from BM25 coverage, which is computed over the whole indexed document and
// therefore treats a word in a 400-line body as equal evidence to the same word in the identifier a
// developer chose.
//
// Measured on gohugoio/hugo#12171 with the query form our own doctrine mandates ("the bug in one
// sentence"): the gold site `pageMap.getPagesInSection` was ABSENT from the top 20, while rank 1-5
// were Pages.Reverse (22.48), Site.Sections (20.76), sortKeys (20.24), pageTree.Sections (19.53) and
// AddLeadingSlash (19.01) — a five-point spread across the entire head, i.e. no usable rank 1. Four
// independent models each abandoned the prose form and re-queried in code vocabulary.
//
// The discriminating fact was already present and unscored: `getPagesInSection` carries TWO of the
// query's domain terms (pages, section) where `Site.Sections` and `Pages.Reverse` carry one. A name
// is a deliberate, dense label; prose in a body is incidental. Rewarding name coverage separates a
// head that BM25 alone collapses.
//
// Plural-tolerant on purpose: the issue says "sections" and the symbol says "Section", and an exact
// substring test scores that as a miss — which is precisely how the gold site lost.
func searchNameTermCoverage(result SearchResult, q searchQuery, _ map[string]float64) float64 {
	name := strings.ToLower(result.QualifiedName)
	if name == "" {
		name = strings.ToLower(result.SymbolName)
	}
	if name == "" || len(q.terms) == 0 {
		return 0
	}
	// Deliberately NOT idf-weighted. Weighting by rarity is right for a document, where a rare word
	// is the discriminating one; it is wrong for an identifier, where the discriminating words are
	// the DOMAIN nouns a developer would put in a name. On the hugo issue sentence the rare terms are
	// "similarly", "adjacent", "incorrect" — none of which any identifier contains — so idf weighting
	// pushed the denominator onto words the name could never carry and scored `getPagesInSection`
	// (pages + section) barely above `Pages.Reverse` (pages). Counting DISTINCT matched terms is the
	// signal: two domain terms in one name is strong evidence, three is decisive, and beyond that the
	// extra matches say little, so it saturates.
	matched := 0
	for _, term := range q.terms {
		if searchNameContainsTerm(name, term) {
			matched++
		}
	}
	if matched == 0 {
		return 0
	}
	return minFloat64(float64(matched), 3) / 3
}

// searchNameContainsTerm matches a query term against an identifier, tolerating the one
// morphological difference that dominates issue text: a plural in the prose against a singular in
// the identifier ("sections" vs getPagesInSection).
func searchNameContainsTerm(lowerName, term string) bool {
	if len(term) < 3 {
		return false
	}
	if strings.Contains(lowerName, term) {
		return true
	}
	if strings.HasSuffix(term, "es") && len(term) > 4 && strings.Contains(lowerName, term[:len(term)-2]) {
		return true
	}
	if strings.HasSuffix(term, "s") && len(term) > 3 && strings.Contains(lowerName, term[:len(term)-1]) {
		return true
	}
	return false
}

// searchNameCoverageWeight is how much a fully name-covering candidate gains. Sized against the
// existing document-coverage weight of 7: the identifier is the stronger evidence, and the head it
// has to separate spans about five points on a prose query.
const searchNameCoverageWeight = 10.0

func searchNameCoversQuery(result SearchResult, q searchQuery) bool {
	name := strings.ToLower(result.QualifiedName)
	if name == "" {
		name = strings.ToLower(result.SymbolName)
	}
	if name == "" || len(q.terms) == 0 {
		return false
	}
	matched := 0
	for _, term := range q.terms {
		if strings.Contains(name, term) {
			matched++
		}
	}
	return float64(matched) >= 0.5*float64(len(q.terms))
}

func searchResultHasSignal(result SearchResult, signal string) bool {
	for _, s := range result.Signals {
		if s == signal {
			return true
		}
	}
	return false
}

type searchKindClassValue int

const (
	searchKindOther searchKindClassValue = iota
	searchKindExec
	searchKindData
	searchKindContainer
)

func searchKindClass(kind string) searchKindClassValue {
	switch kind {
	case "function", "method", "constructor":
		return searchKindExec
	case "field", "constant", "variable", "property":
		return searchKindData
	case "class", "struct", "interface", "trait", "enum", "record", "object", "protocol", "module", "type":
		return searchKindContainer
	}
	return searchKindOther
}

// searchStructuralAdjustment tilts ranking toward the code an agent edits.
// Three effects, each grounded in an observed retrieval failure:
//   - executable definitions outrank everything else at equal text evidence
//     (the fix for "X misbehaves" is in X's code, not in references to X);
//   - container chunks yield to their members: a class-spanning region absorbs
//     its members' matched text and then suppresses the member candidate via
//     overlap dedup, anchoring agents at class heads/constructors instead of
//     the method carrying the signal;
//   - symbols the query names directly (identifier-like terms matching the
//     qualified name path) outrank incidental body matches — issues name
//     their subject far more reliably than they describe its mechanism.
func searchStructuralAdjustment(candidate *searchCandidate, q searchQuery) float64 {
	adjustment := 0.0
	switch searchKindClass(candidate.result.Kind) {
	case searchKindExec:
		adjustment += 3
	case searchKindContainer:
		adjustment -= 3
	}
	// Subject bonus: when the symbol's own name covers most of the query, the
	// query is about this symbol (find/define intent) — any kind wins that.
	if searchNameCoversQuery(candidate.result, q) {
		adjustment += 4
	}
	qualified := strings.ToLower(candidate.result.QualifiedName)
	if qualified == "" {
		qualified = strings.ToLower(candidate.result.SymbolName)
	}
	if qualified != "" {
		matched := 0
		for _, term := range q.terms {
			if q.weights[term] >= 2 && strings.Contains(qualified, term) {
				matched++
			}
		}
		switch {
		case matched >= 2:
			adjustment += 8
		case matched == 1:
			adjustment += 2
		}
		// A dotted call mention in the query ("Type.method(...)") names the
		// fix site outright; the named symbol must not lose to neighbors whose
		// bodies merely repeat the query's data words.
		for _, call := range q.dottedCallMentions {
			if strings.HasSuffix(qualified, call) {
				adjustment += 15
				break
			}
		}
	}
	return adjustment
}

func minFloat64(left, right float64) float64 {
	if left < right {
		return left
	}
	return right
}

func maxFloat64(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func derivedSearchScore(parent, proposed float64) float64 {
	return minFloat64(parent-0.01, proposed)
}

const (
	// searchCallerBoostUnit and searchCallerBoostCap shape the caller-degree
	// ranking boost: unit * log2(1 + distinct production callers), capped so
	// hub symbols (loggers, error helpers) cannot bury strong lexical
	// evidence. One caller ≈ +2.6, three ≈ +5.2, cap reached near seven.
	searchCallerBoostUnit = 2.6
	searchCallerBoostCap  = 8.0
	// searchCallerBoostMinConfidence mirrors the graph-expansion confidence
	// floor: only high-precision call edges contribute ranking signal.
	searchCallerBoostMinConfidence = 0.7
)

// searchGraphCallerBoosts derives a per-symbol ranking boost from the call
// graph: a symbol invoked by distinct non-test callers is likelier to be the
// implementation an agent wants than same-named test scaffolding, which has
// no production in-edges. Callers under test paths are excluded so test
// harnesses do not vote for each other.
func searchGraphCallerBoosts(relations []RelationRecord, symbolsByID map[string]SymbolRecord) map[string]float64 {
	if len(relations) == 0 {
		return nil
	}
	callers := map[string]map[string]struct{}{}
	for _, relation := range relations {
		if relation.Confidence < searchCallerBoostMinConfidence || !searchCallerRelation(relation.Type) {
			continue
		}
		if relation.FromID == relation.ToID {
			continue
		}
		from, ok := symbolsByID[relation.FromID]
		if !ok {
			continue
		}
		if _, ok := symbolsByID[relation.ToID]; !ok {
			continue
		}
		if searchTestArtifactPath("/" + strings.ToLower(filepath.ToSlash(from.FilePath))) {
			continue
		}
		set, ok := callers[relation.ToID]
		if !ok {
			set = map[string]struct{}{}
			callers[relation.ToID] = set
		}
		set[relation.FromID] = struct{}{}
	}
	if len(callers) == 0 {
		return nil
	}
	boosts := make(map[string]float64, len(callers))
	for id, set := range callers {
		boosts[id] = minFloat64(searchCallerBoostCap, searchCallerBoostUnit*math.Log2(1+float64(len(set))))
	}
	return boosts
}

func searchCallerRelation(relationType string) bool {
	switch relationType {
	case "CALLS", "ASYNC_CALLS", "CONSTRUCTS":
		return true
	default:
		return false
	}
}

// applySearchCallerBoosts lifts scored candidates by their symbol's caller
// degree and tags them with the "graph:callers" signal. Returns how many
// candidates were boosted. Candidates whose only evidence is a body text
// match receive half the boost: caller degree is meant to break ties between
// plausibly-named results (a test workload versus the implementation it
// exercises), not to lift well-connected symbols that merely mention a query
// word past results with name or path evidence.
func applySearchCallerBoosts(candidates []searchCandidate, boosts map[string]float64) int {
	if len(boosts) == 0 {
		return 0
	}
	boosted := 0
	for i := range candidates {
		candidate := &candidates[i]
		boost, ok := boosts[candidate.result.SymbolID]
		if !ok || candidate.result.SymbolID == "" {
			continue
		}
		if searchBodyOnlyEvidence(candidate.result.Signals) {
			boost *= 0.5
		}
		candidate.score += boost
		candidate.result.Signals = appendUnique(candidate.result.Signals, "graph:callers")
		boosted++
	}
	return boosted
}

func countSearchCandidatesWithSignal(candidates []searchCandidate, signal string) int {
	count := 0
	for _, candidate := range candidates {
		for _, candidateSignal := range candidate.result.Signals {
			if candidateSignal == signal {
				count++
				break
			}
		}
	}
	return count
}

func searchBodyOnlyEvidence(signals []string) bool {
	sawBody := false
	for _, signal := range signals {
		switch signal {
		case "body", "sparse-region":
			sawBody = true
		case "path", "symbol-name", "exact-symbol", "signature":
			return false
		}
	}
	return sawBody
}

func expandGraphCandidates(seeds []searchCandidate, q searchQuery, relations []RelationRecord, symbolsByID map[string]SymbolRecord, callerBoosts map[string]float64, read contentReader, languages map[string]string, options SearchOptions) []searchCandidate {
	if len(seeds) == 0 || len(relations) == 0 {
		return nil
	}
	// SearchRepository normalizes options before calling here, but direct
	// callers (e.g. tests) may pass hand-built options. Mirror that
	// normalization so a non-positive limit cannot shrink end below start
	// and panic on the snippet slice below.
	if options.MaxRegionLines <= 0 {
		options.MaxRegionLines = defaultSearchMaxRegionLines
	}
	if options.MaxSnippetLines <= 0 {
		options.MaxSnippetLines = defaultSearchMaxSnippetLines
	}
	seedScores := map[string]float64{}
	for _, candidate := range seeds {
		if len(seedScores) >= 10 {
			break
		}
		if candidate.result.SymbolID != "" && candidate.score > seedScores[candidate.result.SymbolID] {
			seedScores[candidate.result.SymbolID] = candidate.score
		}
	}
	if len(seedScores) == 0 {
		return nil
	}
	best := map[string]searchCandidate{}
	contentCache := map[string][]string{}
	for _, relation := range relations {
		if relation.Confidence < 0.7 || !searchExpansionRelation(relation.Type) {
			continue
		}
		pairs := [][3]string{{relation.FromID, relation.ToID, "outgoing"}, {relation.ToID, relation.FromID, "incoming"}}
		for _, pair := range pairs {
			seedScore, ok := seedScores[pair[0]]
			if !ok {
				continue
			}
			symbol, ok := symbolsByID[pair[1]]
			if !ok || symbol.ID == pair[0] {
				continue
			}
			lines, ok := contentCache[symbol.FilePath]
			if !ok {
				content, readable := read(symbol.FilePath)
				if !readable {
					continue
				}
				lines = strings.Split(content, "\n")
				contentCache[symbol.FilePath] = lines
			}
			start, end := clampRegion(symbol.StartLine, symbol.EndLine, len(lines))
			if start == 0 {
				continue
			}
			if end-start+1 > options.MaxRegionLines {
				end = minInt(len(lines), start+options.MaxRegionLines-1)
			}
			snippetStart, snippetEnd := focusedSnippetRegion(start, end, start, options.MaxSnippetLines)
			candidate := searchCandidate{
				result: SearchResult{
					FilePath:         symbol.FilePath,
					StartLine:        start,
					EndLine:          end,
					FocusLine:        start,
					SnippetStartLine: snippetStart,
					SnippetEndLine:   snippetEnd,
					Language:         languages[symbol.FilePath],
					Kind:             symbol.Kind,
					SymbolID:         symbol.ID,
					SymbolName:       symbol.Name,
					QualifiedName:    symbol.QualifiedName,
					Signature:        symbol.Signature,
					SymbolStartLine:  symbol.StartLine,
					SymbolEndLine:    symbol.EndLine,
					Snippet:          strings.Join(lines[snippetStart-1:snippetEnd], "\n"),
				},
				aliases: append([]string(nil), symbol.Aliases...),
			}
			// A graph neighbor of a strong seed must be able to compete with
			// mid-ranked lexical candidates: the previous 0.28*seed+confidence
			// formula scored neighbors of a ~15-point seed near 5, below every
			// direct match, so expansion never surfaced the implementation
			// behind a well-matching type or doc heading. Inherit most of the
			// seed's evidence, then add edge confidence and the target's own
			// caller-degree signal; derivedSearchScore still caps the result
			// below the seed itself. The generous inheritance is reserved for
			// neighbors that carry at least one query term themselves —
			// zero-evidence neighbors (a utility helper the seed happens to
			// call) keep the old conservative score so they cannot displace
			// direct matches.
			inherit := 0.28 * seedScore
			callerBoost := 0.0
			regionText := strings.Join(lines[start-1:end], "\n")
			counts, _ := searchTermCounts(regionText, q.termSet)
			symbolMatch := searchSymbolNameMatchesQueryTerm(q, symbol)
			if len(counts) > 0 || symbolMatch {
				inherit = 0.55 * seedScore
				callerBoost = callerBoosts[symbol.ID]
				if !symbolMatch {
					callerBoost *= 0.5
				}
			}
			// Preserve main's vocabulary-gap bridge: a high-confidence call-like
			// neighbor whose own symbol name shares a query term should sit just
			// below its seed even when its body uses different vocabulary.
			if relation.Confidence >= 0.8 &&
				(relation.Type == "CALLS" || relation.Type == "ASYNC_CALLS" || relation.Type == "CONSTRUCTS") &&
				symbolMatch {
				inherit = 0.85 * seedScore
			}
			candidate.score = derivedSearchScore(
				seedScore,
				inherit+2.2*relation.Confidence+searchPathPrior(q, symbol.FilePath)+callerBoost,
			)
			candidate.baseScore = candidate.score
			candidate.result.Signals = appendUnique(candidate.result.Signals, "graph:"+strings.ToLower(relation.Type), "graph:"+pair[2])
			if callerBoost > 0 {
				candidate.result.Signals = appendUnique(candidate.result.Signals, "graph:callers")
			}
			if previous, exists := best[symbol.ID]; !exists || candidate.score > previous.score {
				best[symbol.ID] = candidate
			}
		}
	}
	out := make([]searchCandidate, 0, len(best))
	for _, candidate := range best {
		out = append(out, candidate)
	}
	sortSearchCandidates(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// searchSymbolNameMatchesQueryTerm reports whether a symbol's name/qualified-name shares a
// (non-trivial) query term — used to gate the graph-expansion boost to textually-plausible callees.
func searchSymbolNameMatchesQueryTerm(q searchQuery, symbol SymbolRecord) bool {
	name := strings.ToLower(symbol.Name)
	qualified := strings.ToLower(symbol.QualifiedName)
	for _, term := range q.terms {
		if len(term) < 3 {
			continue
		}
		if strings.Contains(name, term) || strings.Contains(qualified, term) {
			return true
		}
	}
	return false
}

func searchExpansionRelation(relation string) bool {
	switch relation {
	case "CALLS", "CONSTRUCTS", "ASYNC_CALLS", "IMPORTS", "EXTENDS", "INHERITS", "IMPLEMENTS", "OVERRIDES", "USES_TYPE", "TESTS", "CONFIGURES",
		// SIMILAR_TO is a near-duplicate body (MinHash >= 0.82). Copy-pasted code is the
		// classic reason a fix has to be applied in more than one place, so the twin of a
		// strong hit is exactly the second site an agent would otherwise discover only
		// after its patch failed review. The relation is emitted at profile fast/full only.
		"SIMILAR_TO":
		return true
	default:
		return false
	}
}

type usageFileSelector func(context.Context, []string) ([]string, bool)

func committedUsageFileSelector(
	repo string,
	treeish string,
	useHead bool,
	languages map[string]string,
	deep bool,
	filesExamined int,
	telemetry *searchScanTelemetry,
) usageFileSelector {
	if !useHead || deep {
		return nil
	}
	return func(ctx context.Context, identifiers []string) ([]string, bool) {
		if !searchTermsSafeForGitGrep(identifiers) {
			return nil, false
		}
		matches, err := gitutil.GrepTreePaths(ctx, repo, treeish, identifiers)
		if err != nil {
			return nil, false
		}
		if telemetry != nil {
			telemetry.backend = "git-tree-grep"
			telemetry.passes = 1
			telemetry.filesExamined = filesExamined
		}
		seen := make(map[string]bool, len(matches))
		files := make([]string, 0, len(matches))
		for _, match := range matches {
			if _, eligible := languages[match]; !eligible || seen[match] {
				continue
			}
			seen[match] = true
			files = append(files, match)
		}
		// The expansion routine performs the final stable sort. Sorting there
		// applies the same query-aware source/test/docs/generated prior to both
		// Git postings and the exhaustive correctness fallback.
		return files, true
	}
}

// expandIdentifierUsageCandidates complements typed graph edges with precise
// lexical use sites for strong compound-identifier seeds. Constants, exported
// values, callbacks, and other non-call references often have no relation edge,
// but their consumers are exactly the context a coding agent needs after finding
// a definition.
func expandIdentifierUsageCandidates(
	ctx context.Context,
	seeds []searchCandidate,
	q searchQuery,
	symbolsByID map[string]SymbolRecord,
	symbolsByFile map[string][]SymbolRecord,
	read contentReader,
	languages map[string]string,
	options SearchOptions,
	selectors ...usageFileSelector,
) []searchCandidate {
	type usageSeed struct {
		symbol SymbolRecord
		score  float64
	}
	const maxSeeds = 20
	const maxUsagesPerSeed = 3
	const maxRawUsagesPerSeed = 64
	var selectedSeeds []usageSeed
	seenSymbols := map[string]bool{}
	if len(seeds) == 0 || seeds[0].score <= 0 {
		return nil
	}
	bestScore := seeds[0].score
	for _, candidate := range seeds {
		if len(selectedSeeds) == maxSeeds {
			break
		}
		if candidate.score < 0.35*bestScore {
			continue
		}
		symbol, ok := symbolsByID[candidate.result.SymbolID]
		if !ok || seenSymbols[symbol.ID] || !expandableUsageIdentifier(symbol.Name) {
			continue
		}
		seenSymbols[symbol.ID] = true
		selectedSeeds = append(selectedSeeds, usageSeed{symbol: symbol, score: candidate.score})
	}
	if len(selectedSeeds) == 0 {
		return nil
	}

	filePaths := make([]string, 0, len(languages))
	for filePath := range languages {
		filePaths = append(filePaths, filePath)
	}
	if len(selectors) > 0 && selectors[0] != nil {
		names := make([]string, len(selectedSeeds))
		for index, seed := range selectedSeeds {
			names[index] = seed.symbol.Name
		}
		if selected, ok := selectors[0](ctx, names); ok {
			filePaths = selected
		}
	}
	sort.Slice(filePaths, func(i, j int) bool {
		leftScore := searchPathPrior(q, filePaths[i]) + pathSearchScore(q, filePaths[i])
		rightScore := searchPathPrior(q, filePaths[j]) + pathSearchScore(q, filePaths[j])
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return canonicalSearchPathLess(filePaths[i], filePaths[j])
	})
	type indexedUsage struct {
		candidate searchCandidate
		seedIndex int
	}
	rawUsageCounts := make([]int, len(selectedSeeds))
	seenLocations := map[string]bool{}
	var discovered []indexedUsage
	usageContext := minInt(options.ContextLines, 2)
	for _, filePath := range filePaths {
		if ctx.Err() != nil {
			return nil
		}
		allSaturated := true
		for seedIndex := range selectedSeeds {
			if rawUsageCounts[seedIndex] < maxRawUsagesPerSeed {
				allSaturated = false
				break
			}
		}
		if allSaturated {
			break
		}
		content, ok := read(filePath)
		if !ok || strings.IndexByte(content, 0) >= 0 {
			continue
		}
		lines := strings.Split(content, "\n")
		for seedIndex, seed := range selectedSeeds {
			if rawUsageCounts[seedIndex] == maxRawUsagesPerSeed {
				continue
			}
			if !strings.Contains(content, seed.symbol.Name) {
				continue
			}
			definitionStart := seed.symbol.StartLine
			if filePath == seed.symbol.FilePath {
				definitionStart = precedingDocumentationStart(lines, seed.symbol.StartLine, options.ContextLines)
			}
			for lineIndex, line := range lines {
				lineNumber := lineIndex + 1
				if !containsIdentifierUsage(line, seed.symbol.Name) || identifierUsageIsImport(line) ||
					(filePath == seed.symbol.FilePath && lineNumber >= definitionStart && lineNumber <= seed.symbol.EndLine) {
					continue
				}
				locationKey := fmt.Sprintf("%s:%d:%s", filePath, lineNumber, seed.symbol.ID)
				if seenLocations[locationKey] {
					continue
				}
				start := maxInt(1, lineNumber-usageContext)
				end := minInt(len(lines), lineNumber+usageContext)
				container := enclosingSearchSymbol(symbolsByFile[filePath], lineNumber)
				candidate, ok := makeSearchCandidate(q, filePath, languages[filePath], lines, start, end, container, options.MaxSnippetLines)
				if !ok {
					continue
				}
				candidate.result.FocusLine = lineNumber
				candidate.result.Signals = appendUnique(candidate.result.Signals, "symbol-usage")
				candidate.score = derivedSearchScore(seed.score, 0.9*seed.score+searchPathPrior(q, filePath))
				candidate.baseScore = candidate.score
				seenLocations[locationKey] = true
				rawUsageCounts[seedIndex]++
				discovered = append(discovered, indexedUsage{candidate: candidate, seedIndex: seedIndex})
				if rawUsageCounts[seedIndex] == maxRawUsagesPerSeed {
					break
				}
			}
		}
	}
	sort.SliceStable(discovered, func(i, j int) bool {
		if discovered[i].candidate.score != discovered[j].candidate.score {
			return discovered[i].candidate.score > discovered[j].candidate.score
		}
		leftPath := discovered[i].candidate.result.FilePath
		rightPath := discovered[j].candidate.result.FilePath
		if leftPath != rightPath {
			return canonicalSearchPathLess(leftPath, rightPath)
		}
		return searchCandidateLess(discovered[i].candidate, discovered[j].candidate)
	})
	selectedPerSeed := make([]int, len(selectedSeeds))
	out := make([]searchCandidate, 0, minInt(32, len(discovered)))
	for _, usage := range discovered {
		if selectedPerSeed[usage.seedIndex] == maxUsagesPerSeed {
			continue
		}
		selectedPerSeed[usage.seedIndex]++
		out = append(out, usage.candidate)
		if len(out) == 32 {
			break
		}
	}
	return out
}

func identifierUsageIsImport(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "import{") ||
		strings.HasPrefix(trimmed, "export {") || strings.HasPrefix(trimmed, "export{")
}

func expandableUsageIdentifier(name string) bool {
	if len(name) < 6 {
		return false
	}
	hasLetter, hasLower, hasUpper, hasSeparator := false, false, false, false
	for _, character := range name {
		switch {
		case unicode.IsLower(character):
			hasLetter = true
			hasLower = true
		case unicode.IsUpper(character):
			hasLetter = true
			hasUpper = true
		case unicode.IsLetter(character):
			hasLetter = true
		case character == '_' || character == '$':
			hasSeparator = true
		case unicode.IsDigit(character):
		default:
			return false
		}
	}
	return (hasLower && hasUpper) || (hasLetter && hasSeparator)
}

func containsIdentifierUsage(line, name string) bool {
	for offset := 0; offset <= len(line)-len(name); {
		index := strings.Index(line[offset:], name)
		if index < 0 {
			return false
		}
		index += offset
		leftBoundary := index == 0 || !searchIdentifierByte(line[index-1])
		right := index + len(name)
		rightBoundary := right == len(line) || !searchIdentifierByte(line[right])
		if leftBoundary && rightBoundary {
			return true
		}
		offset = index + len(name)
	}
	return false
}

func searchIdentifierByte(value byte) bool {
	return value == '_' || value == '$' ||
		(value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9')
}

func enclosingSearchSymbol(symbols []SymbolRecord, line int) SymbolRecord {
	var best SymbolRecord
	for _, symbol := range symbols {
		if line < symbol.StartLine || line > symbol.EndLine {
			continue
		}
		if best.ID == "" || symbol.EndLine-symbol.StartLine < best.EndLine-best.StartLine {
			best = symbol
		}
	}
	return best
}

// expandSameContainerNeighborCandidates follows strong callable results to the
// immediately preceding and following callable in the same lexical container.
// Lifecycle code is commonly split into small adjacent stages without an
// explicit call edge (for example tick -> dispatch -> run); the transition is
// often more useful to an agent than another weak match from an unrelated file.
func expandSameContainerNeighborCandidates(
	ctx context.Context,
	seeds []searchCandidate,
	q searchQuery,
	symbolsByID map[string]SymbolRecord,
	symbolsByFile map[string][]SymbolRecord,
	read contentReader,
	languages map[string]string,
	options SearchOptions,
) []searchCandidate {
	if len(seeds) == 0 {
		return nil
	}
	limit := minInt(len(seeds), maxInt(30, options.TopK*2))
	bestScore := seeds[0].score
	if bestScore <= 0 {
		return nil
	}
	seedSymbolIDs := map[string]bool{}
	for _, seed := range seeds[:limit] {
		if ctx.Err() != nil {
			return nil
		}
		seedSymbolIDs[seed.result.SymbolID] = true
	}
	seen := map[string]bool{}
	var out []searchCandidate
	for _, seed := range seeds[:limit] {
		if ctx.Err() != nil {
			return nil
		}
		if seed.score < 0.35*bestScore || !searchFlowSymbolKind(seed.result.Kind) {
			continue
		}
		symbol, ok := symbolsByID[seed.result.SymbolID]
		if !ok {
			continue
		}
		siblings := sameContainerFlowSymbols(symbolsByFile[symbol.FilePath], symbol.ContainerID)
		index := -1
		for i := range siblings {
			if siblings[i].ID == symbol.ID {
				index = i
				break
			}
		}
		if index < 0 {
			continue
		}
		content, ok := read(symbol.FilePath)
		if !ok || strings.IndexByte(content, 0) >= 0 {
			continue
		}
		lines := strings.Split(content, "\n")
		for _, neighborIndex := range []int{index - 1, index + 1} {
			if neighborIndex < 0 || neighborIndex >= len(siblings) {
				continue
			}
			neighbor := siblings[neighborIndex]
			if seedSymbolIDs[neighbor.ID] {
				continue
			}
			start, end := neighbor.StartLine, neighbor.EndLine
			if neighborIndex < index {
				end = minInt(symbol.StartLine-1, start+options.MaxSnippetLines-1)
			} else {
				start = maxInt(symbol.EndLine+1, neighbor.StartLine-options.ContextLines)
				end = minInt(neighbor.EndLine, start+options.MaxSnippetLines-1)
			}
			if start < 1 || start > end || start > len(lines) || end-start+1 > options.MaxSnippetLines {
				continue
			}
			gap := maxInt(0, maxInt(neighbor.StartLine-symbol.EndLine-1, symbol.StartLine-neighbor.EndLine-1))
			if gap > options.MaxSnippetLines {
				continue
			}
			key := fmt.Sprintf("%s:%d:%d", symbol.FilePath, start, end)
			if seen[key] {
				continue
			}
			candidate, ok := makeSearchCandidate(q, symbol.FilePath, languages[symbol.FilePath], lines, start, end, neighbor, options.MaxSnippetLines)
			if !ok {
				// Adjacency is itself the relevance signal; the neighboring stage
				// need not repeat narrative terms from the original query.
				candidate, ok = makeSearchCandidate(buildSearchQuery(neighbor.Name), symbol.FilePath, languages[symbol.FilePath], lines, start, end, neighbor, options.MaxSnippetLines)
				if !ok {
					continue
				}
				candidate.result.Signals = nil
			}
			// neighbor.StartLine is the meaningful focus point, but under a small
			// options.MaxSnippetLines the emitted region above can be truncated to a window
			// that no longer contains it. Clamp into the candidate's own region so FocusLine
			// never escapes [StartLine, EndLine] and trips SearchResponse.Validate.
			candidate.result.FocusLine = minInt(maxInt(neighbor.StartLine, candidate.result.StartLine), candidate.result.EndLine)
			candidate.result.Signals = appendUnique(candidate.result.Signals, "same-container-neighbor")
			candidate.score = derivedSearchScore(seed.score, 0.85*seed.score)
			candidate.baseScore = candidate.score
			seen[key] = true
			out = append(out, candidate)
		}
	}
	sortSearchCandidates(out)
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

func sameContainerFlowSymbols(symbols []SymbolRecord, containerID string) []SymbolRecord {
	var out []SymbolRecord
	for _, symbol := range symbols {
		if symbol.ContainerID == containerID && searchFlowSymbolKind(symbol.Kind) {
			out = append(out, symbol)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].EndLine < out[j].EndLine
	})
	return out
}

// expandSameFileBridgeCandidates exposes short omitted spans between two
// independently strong regions. These bridges frequently contain the glue
// call or state transition that explains how neighboring lifecycle stages are
// connected.
func expandSameFileBridgeCandidates(
	ctx context.Context,
	candidates []searchCandidate,
	q searchQuery,
	symbolsByFile map[string][]SymbolRecord,
	read contentReader,
	languages map[string]string,
	options SearchOptions,
) []searchCandidate {
	if len(candidates) < 2 {
		return nil
	}
	limit := minInt(len(candidates), maxInt(40, options.TopK*2))
	byFile := map[string][]searchCandidate{}
	bestScore := candidates[0].score
	if bestScore <= 0 {
		return nil
	}
	for _, candidate := range candidates[:limit] {
		if candidate.score < 0.3*bestScore || !searchFlowSymbolKind(candidate.result.Kind) || candidate.result.SymbolID == "" {
			continue
		}
		byFile[candidate.result.FilePath] = append(byFile[candidate.result.FilePath], candidate)
	}
	filePaths := make([]string, 0, len(byFile))
	for filePath := range byFile {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	var out []searchCandidate
	seenBridges := map[string]bool{}
	for _, filePath := range filePaths {
		if ctx.Err() != nil {
			return nil
		}
		regions := byFile[filePath]
		sort.Slice(regions, func(i, j int) bool {
			return searchCandidateLess(regions[i], regions[j])
		})
		content, ok := read(filePath)
		if !ok || strings.IndexByte(content, 0) >= 0 {
			continue
		}
		lines := strings.Split(content, "\n")
		for leftIndex := 0; leftIndex+1 < len(regions); leftIndex++ {
			for rightIndex := leftIndex + 1; rightIndex < len(regions); rightIndex++ {
				left, right := regions[leftIndex], regions[rightIndex]
				gapStart, gapEnd := left.result.EndLine+1, right.result.StartLine-1
				if gapStart > gapEnd {
					continue
				}
				if gapEnd-gapStart+1 > options.MaxSnippetLines {
					break
				}
				// Reserve at least half the snippet for the downstream operation.
				// A long omitted span must not consume the entire result and end one
				// line before the consumer that makes the bridge actionable.
				start := maxInt(gapStart, right.result.StartLine-options.MaxSnippetLines/2)
				end := minInt(right.result.EndLine, start+options.MaxSnippetLines-1)
				leftSymbol, leftOK := searchSymbolByID(symbolsByFile[filePath], left.result.SymbolID)
				rightSymbol, rightOK := searchSymbolByID(symbolsByFile[filePath], right.result.SymbolID)
				if !leftOK || !rightOK || leftSymbol.ContainerID != rightSymbol.ContainerID {
					continue
				}
				bridgeKey := fmt.Sprintf("%s:%d:%d", filePath, start, end)
				if seenBridges[bridgeKey] {
					continue
				}
				focus := searchFocusLine(q, lines, start, end)
				container := enclosingSearchSymbol(symbolsByFile[filePath], focus)
				candidate, ok := makeSearchCandidate(q, filePath, languages[filePath], lines, start, end, container, options.MaxSnippetLines)
				if !ok {
					continue
				}
				candidate.result.Signals = appendUnique(candidate.result.Signals, "same-file-bridge")
				candidate.score = derivedSearchScore(maxFloat64(left.score, right.score), 0.45*(left.score+right.score))
				candidate.baseScore = candidate.score
				seenBridges[bridgeKey] = true
				out = append(out, candidate)
			}
		}
	}
	sortSearchCandidates(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func searchSymbolByID(symbols []SymbolRecord, id string) (SymbolRecord, bool) {
	for _, symbol := range symbols {
		if symbol.ID == id {
			return symbol, true
		}
	}
	return SymbolRecord{}, false
}

func searchFlowSymbolKind(kind string) bool {
	switch kind {
	case "function", "method", "constructor", "destructor", "closure":
		return true
	default:
		return false
	}
}

// selectHybridCandidates uses reciprocal-rank fusion to put sparse windows
// from semantically strong files first, then reserves the rest of a deep
// ranking for distinct files found by the syntax-aware search. The split keeps
// region coverage from collapsing into repeated chunks from a few files while
// retaining enough sparse depth to find relevant locations inside large files.
func selectHybridCandidates(semantic, sparse []searchCandidate, topK int) []searchCandidate {
	if topK <= 0 {
		return nil
	}
	if len(semantic) == 0 {
		selected := append([]searchCandidate(nil), sparse[:minInt(topK, len(sparse))]...)
		for index := range selected {
			selected[index].score = 1 / float64(sparseSearchRRFConstant+index+1)
		}
		return selected
	}
	if len(sparse) == 0 {
		return append([]searchCandidate(nil), semantic[:minInt(topK, len(semantic))]...)
	}

	semanticFileRank := make(map[string]int, len(semantic))
	for index, candidate := range semantic {
		if _, exists := semanticFileRank[candidate.result.FilePath]; !exists {
			semanticFileRank[candidate.result.FilePath] = index + 1
		}
	}
	// RRF combines rankings at the requested retrieval depth. Allowing the
	// entire sparse tail to participate lets weak chunks from a highly ranked
	// file leapfrog genuinely strong top-K sparse regions.
	fusedSparse := append([]searchCandidate(nil), sparse[:minInt(topK, len(sparse))]...)
	for index := range fusedSparse {
		sparseRank := index + 1
		fusedSparse[index].score = 1 / float64(sparseSearchRRFConstant+sparseRank)
		if fileRank, exists := semanticFileRank[fusedSparse[index].result.FilePath]; exists {
			fusedSparse[index].score += sparseSearchFileRankWeight / float64(sparseSearchRRFConstant+fileRank)
		}
	}
	sortSearchCandidates(fusedSparse)

	semanticHead := minInt(
		len(semantic),
		minInt(maxDeepSearchSemanticHead, maxInt(1, topK/deepSearchSemanticHeadDivisor)),
	)
	sparseTarget := minInt(len(fusedSparse), topK*deepSearchSparseNumerator/deepSearchSparseDenominator)
	selected := make([]searchCandidate, 0, minInt(topK, len(semantic)+len(fusedSparse)))
	// Every append goes through the region-dedup guard, INCLUDING the two head blocks.
	// The head blocks used to append unchecked, so a region the semantic ranking and the
	// sparse ranking both found was seated twice — observed as one symbol holding ranks 1,
	// 2 and 3 of an 11-row payload that contained only 9 distinct locations.
	appendUnique := func(candidates []searchCandidate, limit int) {
		for _, candidate := range candidates {
			if limit >= 0 && len(selected) >= limit {
				return
			}
			if len(selected) >= topK {
				return
			}
			if containsSearchCandidate(selected, candidate) {
				continue
			}
			selected = append(selected, candidate)
		}
	}
	appendUnique(semantic[:semanticHead], -1)
	appendUnique(fusedSparse, minInt(topK, len(selected)+sparseTarget))

	// Prefer one representative for every additional semantic file. This is a
	// coverage reserve, so cross-modality region overlap is intentionally not a
	// reason to discard a sparse window.
	seenFiles := make(map[string]bool, len(selected))
	for _, candidate := range selected {
		seenFiles[candidate.result.FilePath] = true
	}
	for _, candidate := range semantic[semanticHead:] {
		if len(selected) == topK {
			break
		}
		if seenFiles[candidate.result.FilePath] || containsSearchCandidate(selected, candidate) {
			continue
		}
		seenFiles[candidate.result.FilePath] = true
		selected = append(selected, candidate)
	}
	appendUnique(semantic[semanticHead:], -1)
	appendUnique(fusedSparse[minInt(sparseTarget, len(fusedSparse)):], -1)
	return selected
}

func containsSearchCandidate(candidates []searchCandidate, target searchCandidate) bool {
	for _, candidate := range candidates {
		if candidate.result.FilePath == target.result.FilePath &&
			candidate.result.StartLine == target.result.StartLine &&
			candidate.result.EndLine == target.result.EndLine {
			return true
		}
	}
	return false
}

// searchCandidateLocationKey identifies the source region a candidate occupies. Two
// candidates with the same key are the same region reached by different retrieval
// modalities, never two answers.
func searchCandidateLocationKey(candidate searchCandidate) string {
	return fmt.Sprintf("%s\x00%d\x00%d", candidate.result.FilePath, candidate.result.StartLine, candidate.result.EndLine)
}

// rescaleFusedCandidateScores restores a relevance score on a fused ranking.
//
// Reciprocal-rank fusion works on ranks, so the intermediate scores it computes are rank
// artifacts (1/(k+rank)) with no relation to how well anything matched. Reporting those —
// or, worse, a bare 1/rank — leaves a caller unable to tell a strong hit from a repo that
// simply does not contain what was asked for. Each selected row therefore gets its OWN
// retrieval score back: the semantic score when the region was retrieved semantically,
// otherwise its BM25 score linearly rescaled so the sparse block's maximum lines up with
// the semantic maximum. Ordering is untouched — only the reported number changes.
func rescaleFusedCandidateScores(selected, semantic, sparse []searchCandidate) []searchCandidate {
	if len(selected) == 0 {
		return selected
	}
	semanticScores, maxSemantic := bestSearchCandidateScores(semantic)
	sparseScores, maxSparse := bestSearchCandidateScores(sparse)
	scale := 1.0
	if maxSparse > 0 && maxSemantic > 0 {
		scale = maxSemantic / maxSparse
	}
	for index := range selected {
		key := searchCandidateLocationKey(selected[index])
		if score, ok := semanticScores[key]; ok {
			selected[index].score = score
			continue
		}
		if score, ok := sparseScores[key]; ok {
			selected[index].score = score * scale
		}
	}
	return selected
}

func bestSearchCandidateScores(candidates []searchCandidate) (map[string]float64, float64) {
	scores := make(map[string]float64, len(candidates))
	best := 0.0
	for _, candidate := range candidates {
		key := searchCandidateLocationKey(candidate)
		if existing, exists := scores[key]; !exists || candidate.score > existing {
			scores[key] = candidate.score
		}
		if candidate.score > best {
			best = candidate.score
		}
	}
	return scores, best
}

func selectDiverseCandidates(candidates []searchCandidate, topK, maxPerFile int) []searchCandidate {
	remaining := append([]searchCandidate(nil), candidates...)
	selected := make([]searchCandidate, 0, minInt(topK, len(remaining)))
	perFile := map[string]int{}
	perSymbolName := map[string]int{}
	perBaseName := map[string]int{}
	distinctFileTarget := minInt(topK, (topK+1)/2)
	for len(selected) < topK {
		// Search is primarily a discovery operation for coding agents. Give the
		// agent one strong location from each candidate file before spending the
		// remaining result budget on additional regions in files it has already
		// seen. Without this first pass, a few large or repetitive files can fill
		// the context window even when other relevant implementation files ranked
		// close behind them. The reserve is relevance-bounded: an exact usage or
		// second requested symbol must not be buried under weak matches merely
		// because it shares a file with an earlier result.
		fileLimit := maxPerFile
		if len(perFile) < distinctFileTarget && shouldReserveUnseenSearchFile(
			remaining, selected, perFile, perSymbolName, perBaseName, maxPerFile,
		) {
			fileLimit = 1
		}
		bestIndex := -1
		bestAdjusted := -math.MaxFloat64
		for i := range remaining {
			candidate := remaining[i]
			if perFile[candidate.result.FilePath] >= fileLimit || overlapsSelected(candidate, selected) {
				continue
			}
			adjusted := adjustedSearchCandidateScore(candidate, perFile, perSymbolName, perBaseName)
			if adjusted > bestAdjusted || (adjusted == bestAdjusted && searchCandidateLess(candidate, remaining[bestIndex])) {
				bestIndex = i
				bestAdjusted = adjusted
			}
		}
		if bestIndex < 0 {
			break
		}
		selected = append(selected, remaining[bestIndex])
		chosen := remaining[bestIndex].result
		perFile[chosen.FilePath]++
		symbolKey := strings.ToLower(chosen.QualifiedName)
		if symbolKey == "" {
			symbolKey = strings.ToLower(chosen.SymbolName)
		}
		if symbolKey != "" {
			perSymbolName[symbolKey]++
		}
		perBaseName[strings.ToLower(filepath.Base(chosen.FilePath))]++
		remaining = append(remaining[:bestIndex], remaining[bestIndex+1:]...)
	}
	return selected
}

func shouldReserveUnseenSearchFile(
	candidates, selected []searchCandidate,
	perFile, perSymbolName, perBaseName map[string]int,
	maxPerFile int,
) bool {
	bestAny := -math.MaxFloat64
	bestUnseen := -math.MaxFloat64
	bestRelationship := -math.MaxFloat64
	for _, candidate := range candidates {
		if perFile[candidate.result.FilePath] >= maxPerFile || overlapsSelected(candidate, selected) {
			continue
		}
		adjusted := adjustedSearchCandidateScore(candidate, perFile, perSymbolName, perBaseName)
		if adjusted > bestAny {
			bestAny = adjusted
		}
		if perFile[candidate.result.FilePath] > 0 && searchRelationshipCandidate(candidate) && adjusted > bestRelationship {
			bestRelationship = adjusted
		}
		if perFile[candidate.result.FilePath] == 0 && adjusted > bestUnseen {
			bestUnseen = adjusted
		}
	}
	if bestUnseen == -math.MaxFloat64 {
		return false
	}
	if bestRelationship != -math.MaxFloat64 && bestRelationship >= 0.65*bestUnseen {
		return false
	}
	if bestAny <= 0 {
		return true
	}
	return bestUnseen >= searchDiversityRelevanceRatio*bestAny
}

func searchRelationshipCandidate(candidate searchCandidate) bool {
	for _, signal := range candidate.result.Signals {
		switch signal {
		case "symbol-usage", "same-container-neighbor", "same-file-bridge":
			return true
		}
	}
	return false
}

func adjustedSearchCandidateScore(
	candidate searchCandidate,
	perFile, perSymbolName, perBaseName map[string]int,
) float64 {
	symbolKey := strings.ToLower(candidate.result.QualifiedName)
	if symbolKey == "" {
		symbolKey = strings.ToLower(candidate.result.SymbolName)
	}
	symbolPenalty := 0.0
	if symbolKey != "" {
		symbolPenalty = 4 * float64(perSymbolName[symbolKey])
	}
	baseKey := strings.ToLower(filepath.Base(candidate.result.FilePath))
	return candidate.score -
		0.8*float64(perFile[candidate.result.FilePath]) -
		symbolPenalty -
		2*float64(perBaseName[baseKey])
}

func overlapsSelected(candidate searchCandidate, selected []searchCandidate) bool {
	for _, existing := range selected {
		if candidate.result.FilePath != existing.result.FilePath {
			continue
		}
		intersection := minInt(candidate.result.EndLine, existing.result.EndLine) - maxInt(candidate.result.StartLine, existing.result.StartLine) + 1
		if intersection <= 0 {
			continue
		}
		shorter := minInt(candidate.result.EndLine-candidate.result.StartLine+1, existing.result.EndLine-existing.result.StartLine+1)
		if float64(intersection)/float64(shorter) >= 0.6 {
			return true
		}
	}
	return false
}

func sortSearchCandidates(candidates []searchCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return searchCandidateLess(candidates[i], candidates[j])
	})
}

func searchCandidateLess(left, right searchCandidate) bool {
	if left.result.FilePath != right.result.FilePath {
		return left.result.FilePath < right.result.FilePath
	}
	if left.result.StartLine != right.result.StartLine {
		return left.result.StartLine < right.result.StartLine
	}
	if left.result.EndLine != right.result.EndLine {
		return left.result.EndLine < right.result.EndLine
	}
	if left.result.SymbolID != right.result.SymbolID {
		return left.result.SymbolID < right.result.SymbolID
	}
	if left.result.QualifiedName != right.result.QualifiedName {
		return left.result.QualifiedName < right.result.QualifiedName
	}
	if left.result.SymbolName != right.result.SymbolName {
		return left.result.SymbolName < right.result.SymbolName
	}
	if left.result.Kind != right.result.Kind {
		return left.result.Kind < right.result.Kind
	}
	if left.result.Language != right.result.Language {
		return left.result.Language < right.result.Language
	}
	if left.result.Signature != right.result.Signature {
		return left.result.Signature < right.result.Signature
	}
	if left.result.FocusLine != right.result.FocusLine {
		return left.result.FocusLine < right.result.FocusLine
	}
	if left.result.SnippetStartLine != right.result.SnippetStartLine {
		return left.result.SnippetStartLine < right.result.SnippetStartLine
	}
	if left.result.SnippetEndLine != right.result.SnippetEndLine {
		return left.result.SnippetEndLine < right.result.SnippetEndLine
	}
	if left.result.Snippet != right.result.Snippet {
		return left.result.Snippet < right.result.Snippet
	}
	for index := 0; index < len(left.result.Signals) && index < len(right.result.Signals); index++ {
		if left.result.Signals[index] != right.result.Signals[index] {
			return left.result.Signals[index] < right.result.Signals[index]
		}
	}
	return len(left.result.Signals) < len(right.result.Signals)
}

func matchingLineRegions(q searchQuery, lines []string, start, end, context, maxLines int) [][2]int {
	var hits []int
	for line := start; line <= end; line++ {
		if textMatchesSearchQuery(q, lines[line-1]) {
			hits = append(hits, line)
		}
	}
	return regionsAroundHits(hits, start, end, context, maxLines)
}

func regionsAroundHits(hits []int, lower, upper, context, maxLines int) [][2]int {
	if len(hits) == 0 {
		return nil
	}
	var regions [][2]int
	groupStart := hits[0]
	groupEnd := hits[0]
	flush := func() {
		start := maxInt(lower, groupStart-context)
		end := minInt(upper, groupEnd+context)
		if end-start+1 > maxLines {
			end = start + maxLines - 1
		}
		regions = append(regions, [2]int{start, end})
	}
	for _, hit := range hits[1:] {
		if hit-groupEnd <= context*2+1 && hit-groupStart < maxLines {
			groupEnd = hit
			continue
		}
		flush()
		groupStart, groupEnd = hit, hit
	}
	flush()
	return regions
}

func buildSearchQuery(query string) searchQuery {
	// Strip before anything reads the text, so terms, words and rawLower are all derived from
	// the same URL-state-free query. SearchResponse.Query still echoes the caller's original.
	query = stripSearchQueryURLState(query)
	weights := map[string]float64{}
	add := func(term string, weight float64) {
		if len(term) < 2 || searchStopWords[term] {
			return
		}
		hasAlphanumeric := false
		for _, character := range term {
			if unicode.IsLetter(character) || unicode.IsDigit(character) {
				hasAlphanumeric = true
				break
			}
		}
		if !hasAlphanumeric {
			return
		}
		if weight > weights[term] {
			weights[term] = weight
		}
	}
	constraints := parseSearchConstraints(query)
	// Case folding has to be score-neutral. codeLikeSearchToken reads consecutive capitals
	// as an identifier signal (SCREAMING_SNAKE constants, acronyms), which made a SHOUTED
	// prose query score the same symbol far above the identical lowercase query. When every
	// letter-bearing word of a multi-word query is uppercase, the capitals distinguish
	// nothing — they are emphasis, not identifier structure — so fold the query before
	// reading code-likeness out of it. A single-word query (`ENOENT`), any mixed-case query,
	// and separator-bearing tokens (`MAX_SIZE` stays code-like after folding) are untouched.
	rawTokens := searchWordPattern.FindAllString(query, -1)
	if searchQueryIsShouted(rawTokens) {
		for index := range rawTokens {
			rawTokens[index] = strings.ToLower(rawTokens[index])
		}
	}
	identifierTokens := map[string]bool{}
	for _, raw := range rawTokens {
		codeLike := codeLikeSearchToken(raw)
		if identifierShapedToken(raw) {
			if compacted := compactSearchIdentifier(raw); compacted != "" {
				identifierTokens[compacted] = true
			}
		}
		for index, term := range searchTokenVariants(raw) {
			weight := 1.0
			if codeLike && index == 0 {
				weight = codeLikeSearchWeight(raw)
			} else if codeLike {
				weight = 1.1
			}
			add(term, weight)
		}
	}
	for _, entity := range constraints.Entities {
		add(entity.Normalized, 1.0)
	}
	for _, phrase := range constraints.Phrases {
		for _, word := range phrase.Words {
			add(word, 1.0)
		}
	}
	originalTerms := make([]string, 0, len(weights))
	for term := range weights {
		originalTerms = append(originalTerms, term)
	}
	for _, term := range originalTerms {
		for _, related := range morphologicalSearchTerms(term) {
			add(related, 0.55)
		}
	}
	termSet := make(map[string]bool, len(weights))
	terms := make([]string, 0, len(weights))
	for term := range weights {
		termSet[term] = true
		terms = append(terms, term)
	}
	sort.Slice(terms, func(i, j int) bool {
		if weights[terms[i]] != weights[terms[j]] {
			return weights[terms[i]] > weights[terms[j]]
		}
		if len(terms[i]) != len(terms[j]) {
			return len(terms[i]) > len(terms[j])
		}
		return terms[i] < terms[j]
	})
	if len(terms) > maxSearchQueryTerms {
		terms = terms[:maxSearchQueryTerms]
		termSet = make(map[string]bool, len(terms))
		trimmedWeights := make(map[string]float64, len(terms))
		for _, term := range terms {
			termSet[term] = true
			trimmedWeights[term] = weights[term]
		}
		weights = trimmedWeights
	}
	rawLower := strings.ToLower(strings.TrimSpace(query))
	wordSequence := searchQueryWordSequence(rawLower)
	return searchQuery{
		raw:                query,
		rawLower:           rawLower,
		constraints:        constraints,
		terms:              terms,
		termSet:            termSet,
		weights:            weights,
		words:              searchQueryWords(rawLower),
		wordSequence:       wordSequence,
		matchableWords:     matchableQueryWords(wordSequence, termSet, nil),
		identifierTokens:   identifierTokens,
		dottedCallMentions: dottedCallMentionsIn(query),
	}
}

// dottedCallMentionsIn extracts lowercased container.member pairs the query
// names as explicit calls, e.g. "SegmentInfos.replace()" -> "segmentinfos.replace".
func dottedCallMentionsIn(query string) []string {
	matches := dottedCallMentionPattern.FindAllStringSubmatch(strings.ToLower(query), -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1]+"."+m[2])
	}
	return out
}

// identifierShapedToken reports whether a token was written the way code spells names: it
// carries a separator (`_ . / : $ + #`) or a case hump inside the word (fooBar, NonNull).
// It is deliberately stricter than codeLikeSearchToken, which also accepts any run of
// capitals — issue prose is full of shouted words (BUG, TODO, API, JSON) that are not the
// caller naming a symbol, and only this stricter shape may claim an exact name match.
func identifierShapedToken(raw string) bool {
	trimmed := strings.Trim(raw, "./:-")
	if strings.ContainsAny(trimmed, "_./:$+#") {
		return true
	}
	humped, lowercase := false, false
	for index, character := range trimmed {
		switch {
		case unicode.IsUpper(character):
			humped = humped || index > 0
		case unicode.IsLower(character):
			lowercase = true
		}
	}
	return humped && lowercase
}

// searchQueryIsShouted reports whether the capitals in a query carry no information: at
// least two letter-bearing words and not one lowercase letter among them. One all-caps word
// is an identifier a caller typed; a whole all-caps sentence is emphasis.
func searchQueryIsShouted(tokens []string) bool {
	lettered := 0
	for _, token := range tokens {
		hasLetter := false
		for _, character := range token {
			if !unicode.IsLetter(character) {
				continue
			}
			hasLetter = true
			if unicode.IsLower(character) {
				return false
			}
		}
		if hasLetter {
			lettered++
		}
	}
	return lettered >= 2
}

// searchQueryUnmatchableWord returns the predicate "this query word cannot contribute a
// match to ANY document in the corpus": either it never became a search term (a stop word,
// or too short to index), or it is a term no indexed file contains. Such a word is inert
// evidence, and inert evidence may not cost a document points it already earned.
// documentFrequency may be nil before corpus statistics exist, in which case only the
// never-became-a-term half of the test applies.
func searchQueryUnmatchableWord(termSet map[string]bool, documentFrequency map[string]int) func(string) bool {
	return func(word string) bool {
		if !termSet[word] {
			return true
		}
		if documentFrequency == nil {
			return false
		}
		return documentFrequency[word] == 0
	}
}

// matchableQueryWords drops from the written word order every word that cannot contribute a
// match to any document, so the phrase the caller meant survives the noise words around it.
func matchableQueryWords(wordSequence []string, termSet map[string]bool, documentFrequency map[string]int) []string {
	unmatchable := searchQueryUnmatchableWord(termSet, documentFrequency)
	matchable := make([]string, 0, len(wordSequence))
	for _, word := range wordSequence {
		if unmatchable(word) {
			continue
		}
		matchable = append(matchable, word)
	}
	return matchable
}

// withCorpusPresence returns a copy of q that knows which of its words the corpus can
// actually match, so scoring stops charging documents for words nothing contains.
// documentFrequency is keyed by term over the indexed file set.
func (q searchQuery) withCorpusPresence(documentFrequency map[string]int) searchQuery {
	q.matchableWords = matchableQueryWords(q.wordSequence, q.termSet, documentFrequency)
	return q
}

func buildSparseSearchQuery(query string) searchQuery {
	// Same strip as buildSearchQuery: the sparse half of hybrid search must not be scored
	// against a term set the dense half never sees, and its own cap
	// (maxSparseSearchQueryTerms) is filled first-come, so URL debris starves it too.
	query = stripSearchQueryURLState(query)
	terms := make([]string, 0, maxSparseSearchQueryTerms)
	termSet := make(map[string]bool, maxSparseSearchQueryTerms)
	weights := make(map[string]float64, maxSparseSearchQueryTerms)
	for _, term := range sparseSearchTokens(query) {
		if len(term) < 2 || sparseSearchStopWords[term] || termSet[term] {
			continue
		}
		termSet[term] = true
		weights[term] = 1
		terms = append(terms, term)
		if len(terms) == maxSparseSearchQueryTerms {
			break
		}
	}
	rawLower := strings.ToLower(strings.TrimSpace(query))
	wordSequence := searchQueryWordSequence(rawLower)
	return searchQuery{
		rawLower:       rawLower,
		terms:          terms,
		termSet:        termSet,
		weights:        weights,
		words:          searchQueryWords(rawLower),
		wordSequence:   wordSequence,
		matchableWords: matchableQueryWords(wordSequence, termSet, nil),
	}
}

func codeLikeSearchWeight(raw string) float64 {
	trimmed := strings.Trim(raw, "./:+-")
	letters := 0
	allUpper := true
	for _, character := range trimmed {
		if !unicode.IsLetter(character) {
			continue
		}
		letters++
		if unicode.IsLower(character) {
			allUpper = false
		}
	}
	if allUpper && letters > 0 && letters <= 4 {
		return 1.4
	}
	return 2.5
}

func codeLikeSearchToken(raw string) bool {
	trimmed := strings.Trim(raw, "./:-")
	if strings.ContainsAny(trimmed, "_./:$+#") || strings.HasPrefix(raw, "--") {
		return true
	}
	uppercase := 0
	lowercase := 0
	for index, character := range trimmed {
		if unicode.IsUpper(character) {
			uppercase++
			if index > 0 {
				return true
			}
		} else if unicode.IsLower(character) {
			lowercase++
		}
	}
	return uppercase >= 2 && lowercase > 0
}

func morphologicalSearchTerms(term string) []string {
	var out []string
	switch {
	case strings.HasSuffix(term, "ization") && len(term) > len("ization")+2:
		out = append(out, strings.TrimSuffix(term, "ization")+"ize")
	case strings.HasSuffix(term, "ification") && len(term) > len("ification")+2:
		out = append(out, strings.TrimSuffix(term, "ification")+"ify")
	case strings.HasSuffix(term, "ing") && len(term) > 5:
		stem := strings.TrimSuffix(term, "ing")
		out = append(out, stem, stem+"e")
	case strings.HasSuffix(term, "ed") && len(term) > 4:
		stem := strings.TrimSuffix(term, "ed")
		out = append(out, stem, stem+"e")
	}
	if strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") && len(term) > 4 {
		out = append(out, strings.TrimSuffix(term, "s"))
	}
	return appendUnique(nil, out...)
}

func searchTokenVariants(raw string) []string {
	raw = strings.Trim(raw, "./:-")
	if raw == "" {
		return nil
	}
	variants := []string{strings.ToLower(raw)}
	for _, segment := range strings.FieldsFunc(raw, func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	}) {
		variants = append(variants, strings.ToLower(segment))
	}
	var current []rune
	runes := []rune(raw)
	flush := func() {
		if len(current) > 0 {
			variants = append(variants, strings.ToLower(string(current)))
			current = nil
		}
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(current) > 0 && unicode.IsUpper(r) {
			previous := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextLower) {
				flush()
			}
		}
		current = append(current, r)
	}
	flush()
	return appendUnique(nil, variants...)
}

func sparseSearchTokens(text string) []string {
	var out []string
	for _, raw := range sparseSearchWordPattern.FindAllString(text, -1) {
		var current []rune
		currentDigit := false
		flush := func() {
			if len(current) > 0 {
				out = append(out, strings.ToLower(string(current)))
				current = nil
			}
		}
		runes := []rune(raw)
		for index, character := range runes {
			if character == '_' {
				flush()
				continue
			}
			isDigit := unicode.IsDigit(character)
			if len(current) > 0 && isDigit != currentDigit {
				flush()
			}
			if len(current) > 0 && !isDigit && unicode.IsUpper(character) {
				previous := runes[index-1]
				nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
				if unicode.IsLower(previous) || (unicode.IsUpper(previous) && nextLower) {
					flush()
				}
			}
			currentDigit = isDigit
			current = append(current, character)
		}
		flush()
	}
	return out
}

func sparseSearchTermCounts(text string, queryTerms map[string]bool) (map[string]int, int) {
	counts := map[string]int{}
	tokens := sparseSearchTokens(text)
	for _, token := range tokens {
		if queryTerms[token] {
			counts[token]++
		}
	}
	return counts, len(tokens)
}

func searchTermCounts(text string, queryTerms map[string]bool) (map[string]int, int) {
	counts := map[string]int{}
	length := 0
	for _, raw := range searchWordPattern.FindAllString(text, -1) {
		for _, token := range searchTokenVariants(raw) {
			if len(token) < 2 {
				continue
			}
			length++
			if queryTerms[token] {
				counts[token]++
			}
		}
	}
	return counts, length
}

func textMatchesSearchQuery(q searchQuery, text string) bool {
	lower := strings.ToLower(text)
	for _, term := range q.terms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func searchFocusLine(q searchQuery, lines []string, start, end int) int {
	bestLine := start
	bestMatches := 0.0
	for line := start; line <= end; line++ {
		lower := strings.ToLower(lines[line-1])
		matches := 0.0
		for _, term := range q.terms {
			if strings.Contains(lower, term) {
				matches += q.weights[term]
			}
		}
		if matches > bestMatches {
			bestLine = line
			bestMatches = matches
		}
	}
	return bestLine
}

func focusedSnippetRegion(start, end, focus, maxLines int) (int, int) {
	if maxLines <= 0 || end-start+1 <= maxLines {
		return start, end
	}
	half := maxLines / 2
	snippetStart := maxInt(start, focus-half)
	snippetEnd := minInt(end, snippetStart+maxLines-1)
	if snippetEnd-snippetStart+1 < maxLines {
		snippetStart = maxInt(start, snippetEnd-maxLines+1)
	}
	return snippetStart, snippetEnd
}

func pathSearchScore(q searchQuery, filePath string) float64 {
	lower := strings.ToLower(filepath.ToSlash(filePath))
	base := strings.ToLower(filepath.Base(filePath))
	score := 0.0
	for _, term := range q.terms {
		weight := q.weights[term]
		if weight != 1 && weight < 1.25 {
			continue
		}
		if strings.Contains(base, term) {
			score += 2.5 * weight
		} else if strings.Contains(lower, term) {
			score += 1.25 * weight
		}
	}
	return score
}

func searchPathPrior(q searchQuery, filePath string) float64 {
	lower := "/" + strings.ToLower(filepath.ToSlash(filePath))
	score := 0.0
	if strings.Contains(lower, "/src/") || strings.Contains(lower, "/include/") || strings.Contains(lower, "/lib/") || strings.Contains(lower, "/packages/") {
		score += 0.5
	}
	if strings.Contains(lower, "/.github/workflows/") && !searchQuerySupplied(q, "workflow", "pipeline", "ci", "action") {
		score -= 6
	}
	// .github/ metadata (ISSUE_TEMPLATE, PULL_REQUEST_TEMPLATE, CONTRIBUTING, FUNDING, etc.)
	// is NEVER a code fix site, yet issue-template prose matches the problem statement's own
	// filled-in fields (version/expected/actual boilerplate) and body-BM25-outranks the real
	// source (measured: briannesbitt .github/ISSUE_TEMPLATE.md ranked #1 over src/Carbon/*).
	// Hard-demote all non-workflow .github/ paths so template prose can never top the list.
	if strings.Contains(lower, "/.github/") && !strings.Contains(lower, "/.github/workflows/") {
		score -= 12
	}
	if strings.Contains(lower, "/dependencies/") || strings.Contains(lower, "/third_party/") || strings.Contains(lower, "/third-party/") {
		score -= 5
	}
	// "regression"/"regressions" are deliberately NOT test-intent words. In issue prose
	// "a regression" names behaviour that used to work, not a request for the regression
	// suite; a caller who does want tests writes "test"/"spec"/"fixture", and the phrase
	// "regression test" already contains "test". Keeping them here let the commonest word
	// in a bug report switch off the test demotion for the whole query.
	if searchTestArtifactPath(lower) && !searchQuerySupplied(q, searchTestIntentWords...) {
		// Strong demotion (was -1.5 — far too weak): a test file that exercises the buggy
		// function matches the issue's exact code tokens (function name + behaviour keywords),
		// so it out-scores the real source at ~26-47 while the implementation sits at ~13.
		// A bug-fix task never edits a test, so tests are pure exploration noise. -12 reliably
		// sinks the test below the editable source (measured: briannesbitt RoundTest.php crowded
		// out src/Carbon/Traits/Rounding.php; php-cs-fixer's 13 test files buried the fix). Not
		// excluded outright — a test still surfaces if it is the only match.
		score -= 12
	}
	if searchDocumentationArtifactPath(lower) && !searchQuerySupplied(q,
		"doc", "docs", "documentation", "readme", "readmes", "guide", "guides", "example", "examples",
	) {
		// Strong demotion: docs describe features in the issue's own vocabulary and
		// otherwise out-score the implementing code on a body-text match. A "fix the
		// bug" query wants code, not the manual. (Was -1.25 — too weak to flip a ~9.9
		// doc match below the ~5-8 code match; -6 reliably ranks code first.)
		score -= 6
	}
	if searchGeneratedArtifactPath(lower) && !searchQuerySupplied(q,
		"generated", "generator", "generators", "codegen", "codegens", "build", "builds", "dist", "bundle", "bundles",
		"amalgamation", "amalgamated", "single", "onefile",
	) {
		// Strong demotion (was -2): an amalgamation scores as high as the real source
		// (it IS the source, concatenated) so -2 leaves it competitive; -6 ranks the
		// editable per-file source first so the agent's edit actually sticks.
		score -= 6
	}
	return score
}

// There is no neighbor fold here. The types a hit is written in terms of are answered by
// the signature-type block (search_sigtypes.go) and its other change sites by the
// related-site block (search_related.go); both are displacement-funded so neither can
// shrink a complete head body, which a fold running before fitSearchResultsToBudget could.

func searchTestArtifactPath(lower string) bool {
	normalized := strings.Trim(strings.ToLower(filepath.ToSlash(lower)), "/")
	if normalized == "" {
		return false
	}
	if strings.HasPrefix(normalized, ".github/workflows/") {
		return false
	}
	parts := strings.Split(normalized, "/")
	for index, part := range parts {
		if index == len(parts)-1 {
			break
		}
		switch part {
		case "test", "tests", "testdata", "fixture", "fixtures", "__tests__", "__fixtures__", "spec", "specs":
			return true
		}
		for _, suffix := range []string{
			".test", ".tests", "-test", "-tests", "_test", "_tests",
			".spec", ".specs", "-spec", "-specs", "_spec", "_specs",
		} {
			if strings.HasSuffix(part, suffix) {
				return true
			}
		}
	}
	base := parts[len(parts)-1]
	for _, marker := range []string{
		".test.", ".spec.", "_test.", "_tests.", "_spec.", "_specs.",
		"-test.", "-tests.", "-spec.", "-specs.",
	} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	return strings.HasPrefix(base, "test_") || strings.HasPrefix(base, "test-") ||
		strings.HasPrefix(base, "test.") || strings.HasPrefix(base, "spec_") ||
		strings.HasPrefix(base, "spec-") || strings.HasPrefix(base, "spec.") ||
		strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".spec")
}

// searchDocumentationArtifactPath reports whether a path is prose documentation.
// The taxonomy lives in search_file_class.go so the additive prior here and the
// multiplicative file-class prior applied at ranking time can never disagree
// about what "documentation" means (segment-aware, so `versioned_docs/` and
// `website/` count, and .mdx counts alongside .md/.rst/.adoc/.txt).
func searchDocumentationArtifactPath(lower string) bool {
	segments := searchPathSegments(lower)
	dirs := segments
	if len(dirs) > 0 {
		dirs = dirs[:len(dirs)-1]
	}
	return searchDocumentationClassPath(lower, filepath.Base(lower), dirs)
}

func searchGeneratedArtifactPath(lower string) bool {
	base := filepath.Base(lower)
	// Amalgamated / single-header / concatenated build artifacts: a generated COPY of
	// the real per-file source (e.g. nlohmann's single_include/nlohmann/json.hpp is a
	// concatenation of include/nlohmann/**). They match the query as well as the real
	// source AND duplicate it, so an agent that edits the amalgamation makes a change
	// that gets overwritten by the next codegen — always steer to the real source.
	if strings.Contains(lower, "/single_include/") || strings.Contains(lower, "/single-include/") ||
		strings.Contains(lower, "amalgamat") || strings.Contains(lower, "/onefile/") ||
		strings.Contains(lower, "/amalgamation/") {
		return true
	}
	return strings.Contains(lower, "/generated/") || strings.Contains(lower, "/dist/") ||
		strings.Contains(lower, "/build/") || strings.Contains(lower, "/gen/") ||
		strings.Contains(base, ".generated.") || strings.Contains(base, "_generated.") ||
		strings.HasPrefix(base, "generated_") || strings.HasSuffix(base, ".min.js")
}

// searchQuerySupplied reports whether the caller ASKED FOR a class of file, which is how
// every relevance prior below 1 is switched back off ("update the docs for…", "fix the
// example", "add a test").
//
// Intent is read from the words the caller wrote, never from fragments mined out of an
// identifier they merely mentioned. The distinction is not cosmetic: the tokenizer splits
// `NamedByteArrayTest.java` into `named`/`byte`/`array`/`test` at weight 1.1, so under a
// plain weight lookup an issue that merely names a reproduction file switched OFF the -12
// test-artifact demotion for the whole query — and test fixtures, which restate the issue in
// its own vocabulary, took the top of the ranking away from the source being fixed
// (measured: a lombok issue quoting `NamedByteArrayTest.java` ranked a `test/transform/
// resource/before/*.java` fixture first). Mentioning an identifier is not a request.
func searchQuerySupplied(q searchQuery, terms ...string) bool {
	words := q.words
	if words == nil {
		// Hand-built queries (tests, direct API callers) carry no precomputed word set.
		// Derive it rather than silently answering "no intent" for every class.
		words = searchQueryWords(q.rawLower)
	}
	for _, term := range terms {
		if words[term] {
			return true
		}
	}
	return false
}

// searchQueryWords splits a raw query into the words a human wrote: maximal alphanumeric
// runs. Any separator a writer types — space, `/`, `.`, `_`, `-`, punctuation — ends a word,
// so `docs/api.md` contributes `docs`, while `NamedByteArrayTest` contributes only itself.
func searchQueryWords(rawLower string) map[string]bool {
	words := map[string]bool{}
	for _, word := range searchQueryWordSequence(rawLower) {
		words[word] = true
	}
	return words
}

// searchQueryWordSequence is searchQueryWords in written order, duplicates kept.
func searchQueryWordSequence(rawLower string) []string {
	var words []string
	current := make([]rune, 0, 16)
	flush := func() {
		if len(current) > 0 {
			words = append(words, string(current))
			current = current[:0]
		}
	}
	for _, character := range rawLower {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			current = append(current, unicode.ToLower(character))
			continue
		}
		flush()
	}
	flush()
	return words
}

func symbolSearchScore(q searchQuery, symbol SymbolRecord) (float64, []string) {
	name := strings.ToLower(symbol.Name)
	qualified := strings.ToLower(symbol.QualifiedName)
	signature := strings.ToLower(symbol.Signature)
	compactName := compactSearchIdentifier(name)
	score := 0.0
	switch symbol.Kind {
	case "function", "method":
		score += 2
	case "class", "interface", "struct", "trait", "type", "enum", "record", "object", "protocol":
		score += 0.75
	case "block", "workflow", "document":
		score -= 1.5
	case "section":
		score -= 0.5
	}
	var signals []string
	if matchesExactSymbolForm(q, compactName) {
		score += exactSymbolBonus(q, symbol)
		signals = append(signals, "exact-symbol")
	}
	for _, term := range q.terms {
		weight := q.weights[term]
		switch {
		case name == term:
			score += 6 * weight
			signals = append(signals, "symbol-name")
		case strings.Contains(name, term) || strings.Contains(qualified, term):
			score += 3 * weight
			signals = append(signals, "symbol-name")
		case strings.Contains(signature, term):
			score += 1.5 * weight
			signals = append(signals, "signature")
		}
	}
	// Registration-table aliases (e.g. a command verb bound to this handler) match
	// like an exact symbol name — the verb never appears in the body otherwise.
	aliasMatched := false
	for _, alias := range symbol.Aliases {
		aliasLower := strings.ToLower(alias)
		for _, term := range q.terms {
			if aliasLower == term {
				score += 6 * q.weights[term]
				aliasMatched = true
			}
		}
	}
	if aliasMatched {
		signals = append(signals, "alias")
	}
	return score, appendUnique(nil, signals...)
}

// exactSymbolBonus weights an exact name match by what the named thing is.
//
// A container — a class, an interface, an enum, a module-level constant — is a NOUN issue
// prose is full of, and reading it as the edit site crowds the head of the ranking with
// declarations. Measured: "GsonBuilder should throw when registering an adapter for Object or
// JsonElement" named three types, and at the flat bonus the classes GsonBuilder, JsonElement
// and the constant JSON_ELEMENT took ranks 1-3 from the method the patch actually changes.
// A callable, by contrast, is where a fix lands, so it keeps the full bonus. When the caller
// asks for the container by itself — the whole query is that one name — it is the answer, and
// the full bonus applies again.
//
// A CONSTRUCTOR is the one callable that belongs in the container tier. It is named after its
// own type (Java `GsonBuilder.GsonBuilder`, C++ `Foo::Foo`), so an exact match on it is a
// type-name match wearing a callable's clothes — the very thing the container tier exists to
// hold back. At the callable bonus it outranks the type it constructs (14 vs 6) and buries real
// methods: measured on 43 Java instances, `JsonElement.JsonElement` and `GsonBuilder.GsonBuilder`
// took ranks 1-2 from GsonBuilder.registerTypeAdapter on a query that named both types.
func exactSymbolBonus(q searchQuery, symbol SymbolRecord) float64 {
	switch symbol.Kind {
	case "class", "interface", "struct", "trait", "type", "enum", "record", "object", "protocol",
		"annotation", "const", "constant", "variable", "field", "property",
		"module", "namespace", "package":
		if len(q.wordSequence) > 1 {
			return 6
		}
	default:
		if namedAfterOwnContainer(symbol) && len(q.wordSequence) > 1 {
			// The type this constructor is named after is itself a candidate in the same
			// file, and it earns this bonus. Paying it twice for one name is what lifts the
			// constructor over the methods a fix lands in — a method whose own name is a
			// single word (`replace`) cannot earn the bonus at all inside a multi-word
			// query, so the duplicate is the whole margin. Spend the name once, on the type.
			return 0
		}
	}
	return 14
}

// namedAfterOwnContainer reports whether a member's own name repeats the name of the type that
// contains it — the constructor shape, in every language that spells constructors that way. It
// compares the last two segments of the qualified name (`GsonBuilder.GsonBuilder`,
// `Mode.FAST.FAST`) so a plain method that merely shares a name with some OTHER type is
// unaffected.
func namedAfterOwnContainer(symbol SymbolRecord) bool {
	qualified := symbol.QualifiedName
	cut := strings.LastIndex(qualified, ".")
	if cut <= 0 {
		return false
	}
	member := qualified[cut+1:]
	container := qualified[:cut]
	if inner := strings.LastIndex(container, "."); inner >= 0 {
		container = container[inner+1:]
	}
	return member != "" && member == container
}

// matchesExactSymbolForm reports whether the query spells this symbol out by name.
//
// It used to compare the symbol against the compaction of the WHOLE query, which made the
// bonus all-or-nothing over every token the caller typed: appending one more word — a
// politeness ("please"), a word the repository has never heard of ("xyzzy"), an article
// ("update THE market sizes") — revoked a bonus the symbol had already earned from the words
// that did match. That is the opposite of how a term-scoring model must behave: a term the
// document cannot match contributes zero, never a penalty.
//
// So the test is now local to the words that name the symbol. The bonus is earned when the
// symbol's name is spelled out by
//   - a contiguous run of two or more query words, in written order ("update market sizes"
//     inside a longer sentence, and "get the value" for a name really spelled get_the_value);
//   - the same over the matchable words only, so stop words and words nothing in the corpus
//     contains cannot break the run apart ("update THE market sizes PLEASE");
//   - one token the caller wrote in identifier shape, whatever surrounds it
//     ("buildGroupIndex please");
//   - or a single-word query equal to the name, which is the original whole-query rule.
//
// Every clause only ADDS a way to earn the bonus, so no word can take one away.
func matchesExactSymbolForm(q searchQuery, compactName string) bool {
	if compactName == "" {
		return false
	}
	if q.identifierTokens[compactName] {
		return true
	}
	if len(q.wordSequence) == 1 {
		return q.wordSequence[0] == compactName
	}
	return spellsCompactIdentifier(q.wordSequence, compactName) ||
		spellsCompactIdentifier(q.matchableWords, compactName)
}

// spellsCompactIdentifier reports whether two or more consecutive words of the sequence
// concatenate to exactly compact. Word boundaries are what keep this honest: a run must
// consume whole words, so "buildGroupIndexEntry" (one word) never spells buildGroupIndex.
func spellsCompactIdentifier(words []string, compact string) bool {
	for start := 0; start+1 < len(words); start++ {
		if !strings.HasPrefix(compact, words[start]) {
			continue
		}
		length := len(words[start])
		for end := start + 1; end < len(words); end++ {
			if length+len(words[end]) > len(compact) ||
				compact[length:length+len(words[end])] != words[end] {
				break
			}
			length += len(words[end])
			if length == len(compact) {
				return true
			}
		}
	}
	return false
}

func compactSearchIdentifier(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(unicode.ToLower(r))
		}
	}
	return out.String()
}

func clampRegion(start, end, lineCount int) (int, int) {
	if lineCount <= 0 {
		return 0, 0
	}
	start = maxInt(1, start)
	end = minInt(lineCount, maxInt(start, end))
	if start > lineCount {
		return 0, 0
	}
	return start, end
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]bool, len(values)+len(additions))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	return values
}

func (response SearchResponse) Validate() error {
	if actual := serializedSearchResultBytes(response.Results); response.Stats.ResultBytes != actual {
		return fmt.Errorf("search result byte accounting mismatch: %d != %d", response.Stats.ResultBytes, actual)
	}
	passages, passageBytes := searchPassageStats(response.Results)
	if response.Stats.ProsePassages != passages || response.Stats.ProsePassageBytes != passageBytes {
		return fmt.Errorf("search prose passage accounting mismatch: %d/%d != %d/%d",
			response.Stats.ProsePassages, response.Stats.ProsePassageBytes, passages, passageBytes)
	}
	if response.Stats.ContextBudgetBytes > 0 && response.Stats.ResultBytes > response.Stats.ContextBudgetBytes {
		return fmt.Errorf("search result context exceeds byte budget: %d > %d", response.Stats.ResultBytes, response.Stats.ContextBudgetBytes)
	}
	// Every block outside `results` — signature types, type card, container map — is accounted
	// and held to one combined ceiling in one place, because each block's own funder can only
	// see its own bytes. See search_blocks.go for the policy and the yield order.
	if err := validateSearchContextBlockBudget(response); err != nil {
		return err
	}
	for index, result := range response.Results {
		if result.Rank != index+1 {
			return fmt.Errorf("search result rank %d at index %d", result.Rank, index)
		}
		if result.FilePath == "" || result.StartLine < 1 || result.EndLine < result.StartLine || result.FocusLine < result.StartLine || result.FocusLine > result.EndLine || result.SnippetStartLine < result.StartLine || result.SnippetEndLine > result.EndLine || result.SnippetEndLine < result.SnippetStartLine {
			return fmt.Errorf("invalid search result at rank %d", result.Rank)
		}
		if err := validateSearchResultPassages(result); err != nil {
			return fmt.Errorf("invalid search result at rank %d: %w", result.Rank, err)
		}
	}
	return nil
}

var dottedCallMentionPattern = regexp.MustCompile(`([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)\s*\(`)

// repoIgnoredFileCount is the nil-safe count for the stats block.
func repoIgnoredFileCount(report *RepoIgnoreReport) int {
	if report == nil {
		return 0
	}
	return report.Files
}
