package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/termsafe"
)

// defaultSearchContextBytes is the default `--max-context-bytes` ceiling for one search call.
//
// It is sized by TURN economics, not by payload economics. Measured on real agentic sessions,
// the search payload is ~0.6% of what a session spends, while a single extra agent turn costs
// ~42.5k tokens (95.9% of billed tokens are context re-read). A search that stops one Read
// short of an edit therefore costs about 40x more than the entire payload that caused it, so
// the ceiling's job is NOT to be small — it is to never be the reason a head result comes back
// as half a function.
//
// 24 kB ≈ 6k tokens: it clears the largest ranked payloads observed (~15 kB) with room for the
// five complete head bodies the allocator may buy, so the allocator is bounded by what bodies
// actually cost rather than silently truncated by the ceiling. The old 16 kB default could not
// fit even one complete body on top of a large ranking. In practice payloads do NOT expand to
// fill it: the allocator only spends bytes on complete head bodies and always picks the
// cheapest plan that delivers them (measured on a 14-repo probe: mean payload 11.8 kB, i.e.
// half the ceiling). `--max-context-bytes` remains honored exactly, so a caller who wants the
// old behaviour passes `--max-context-bytes 16384`.
const defaultSearchContextBytes = 24 * 1024

// searchReferenceBlocks names the three off-by-default reference blocks. They are addressable
// individually because they were measured individually: a caller who wants the navigation aid should
// not have to pay for the declaration card as well.
//
// `all` is accepted as shorthand. An unknown name is an error rather than a silent no-op — a flag
// that quietly does nothing is how a measurement gets attributed to the wrong build.
const (
	searchReferenceBlockContainerMap   = "container-map"
	searchReferenceBlockSignatureTypes = "signature-types"
	searchReferenceBlockTypeCard       = "type-card"
	searchReferenceBlocksAll           = "all"
)

type searchFlags struct {
	// VerifyExplain is appended to the emitted VERIFY line as `... 2>&1 | <cmd>`.
	VerifyExplain         string
	Repo                  string
	Query                 string
	Format                string
	Profile               string
	Worktree              bool
	TopK                  int
	ContextLines          int
	MaxRegionLines        int
	MaxSnippetLines       int
	MaxRegionsPerFile     int
	IgnoreFiles           []string
	IncludeFiles          []string
	CacheDir              string
	DisableCache          bool
	MaxIndexedFiles       int
	IndexAllFiles         bool
	MaxContextBytes       int
	BodyHeadRanks         int
	EnclosureContextLines int
	HeadWindowLines       int
	FullUnitTop           int
	EditSiteBodies        bool
	CalleeHop             bool
	VerifyPrefix          string
	VerifyPreFixStatus    string
	FileOutline           bool
	Deep                  bool
	SingleResolution      bool
	DocumentResolution    bool
	// The reference blocks, off unless asked for. See SearchOptions in internal/sem/search.go for
	// the session measurement that made OFF the default.
	ContainerMap   bool
	SignatureTypes bool
	TypeCard       bool
}

// applySearchReferenceBlocks turns a comma-separated block list into flags. It is used for both the
// `--reference-blocks` flag and the ENTIRE_GRAPH_REFERENCE_BLOCKS environment variable, so the two
// cannot drift.
func applySearchReferenceBlocks(flags *searchFlags, value string) error {
	for _, name := range strings.Split(value, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		switch name {
		case "":
		case searchReferenceBlocksAll:
			flags.ContainerMap, flags.SignatureTypes, flags.TypeCard = true, true, true
		case searchReferenceBlockContainerMap:
			flags.ContainerMap = true
		case searchReferenceBlockSignatureTypes:
			flags.SignatureTypes = true
		case searchReferenceBlockTypeCard:
			flags.TypeCard = true
		default:
			return fmt.Errorf("unknown search reference block %q: want %s, %s, %s, or %s",
				name, searchReferenceBlockContainerMap, searchReferenceBlockSignatureTypes,
				searchReferenceBlockTypeCard, searchReferenceBlocksAll)
		}
	}
	return nil
}

func runSearch(ctx context.Context, opts Options, args []string) error {
	flags, rest, err := parseSearchFlags(args)
	if err != nil {
		return err
	}
	// The environment sets a session-wide default; the flags then add to it. A flag can only ever
	// turn a block ON, so the two compose without either having to override the other.
	if value := strings.TrimSpace(opts.Env.ReferenceBlocks); value != "" {
		if err := applySearchReferenceBlocks(&flags, value); err != nil {
			return fmt.Errorf("%s: %w", envReferenceBlocks, err)
		}
	}
	if len(rest) != 0 {
		return unexpectedArgumentsError("search", opts.Version, rest)
	}
	if strings.TrimSpace(flags.Query) == "" {
		return errors.New("search requires --query")
	}
	// ZERO-TOLL DELIVERY. When the caller has already computed this session's payload and handed it
	// to the agent inside context the session pays for anyway, this call must cost nothing: echo
	// those bytes and return, before the profile, the repo, the cache and the index are touched.
	if path := strings.TrimSpace(opts.Env.PresearchPath); path != "" {
		return echoPresearchPayload(opts.Stdout, path)
	}
	// The echo is decided before any INDEXING work: a replayed payload must not pay for an index
	// build. See searchSession for what the cap is worth and why it is an echo.
	//
	// It does now pay for bounded tree, corpus-policy, and path-admissibility checks. Those checks
	// prove that the same committed identity and current effective corpus policy still admit every
	// recorded contributing path. Corpus broadening and worktree source bytes remain intentionally
	// session-stale, like the later query itself: this cap replays the first answer verbatim rather
	// than promising a fresh ranking. The checks stay deliberately ahead of the index build. See
	// searchSessionScope and sem.SearchReplayPolicy.
	session, err := newSearchSession(opts.Env, opts.Stderr)
	if err != nil {
		return err
	}
	profile, err := parseProfile(flags.Profile)
	if err != nil {
		return err
	}
	repo, err := resolveRepo(ctx, opts.Env, flags.Repo)
	if err != nil {
		return err
	}
	var (
		scope               searchSessionScope
		replayPolicy        sem.SearchReplayPolicy
		replayPolicyReady   bool
		forceSessionReplace bool
	)
	if session != nil {
		scope = searchSessionScopeFor(ctx, repo)
		scope.Format = flags.Format
		// A rendered payload is opaque: snippets and reference blocks cannot be safely removed from
		// it after the fact. Bind it to the semantic layer's effective corpus policy and validate
		// every contributing path before writing even the replay header. Policy resolution is an
		// optimization gate; failure declines the echo and lets the real search report any error.
		var (
			policy    sem.SearchReplayPolicy
			policyErr error
		)
		if flags.Worktree {
			// A mutable worktree cannot provide an immutable replay identity.
			// Skip the corpus-policy observation entirely; it cannot authorize a
			// replay and would add an unnecessary second filesystem traversal.
			forceSessionReplace = true
		} else {
			policy, policyErr = sem.ResolveSearchReplayPolicy(ctx, repo, sem.SearchOptions{
				Worktree:     false,
				IgnoreFiles:  flags.IgnoreFiles,
				IncludeFiles: flags.IncludeFiles,
			})
		}
		if policyErr == nil && !policy.MatchesTree(scope.Tree) {
			// Mutable worktree state cannot be pinned through the final
			// admission-to-output interval. Discard any payload written by an
			// older binary as the live search completes; retaining it would keep
			// credential-bearing bytes on disk even though this binary can never
			// safely replay them.
			forceSessionReplace = true
		}
		if policyErr == nil && policy.MatchesTree(scope.Tree) {
			replayPolicy = policy
			replayPolicyReady = true
			scope.PolicyFingerprint = policy.Fingerprint()
			state, ok := searchSessionState{}, false
			if searchFormatSupportsReplay(flags.Format) {
				state, ok = session.echo(scope)
			}
			if !ok && state.Payload != "" {
				forceSessionReplace = true
			}
			if ok {
				// Resolve both identities again immediately before output. This brackets the
				// candidate lookup with tree and policy observations, so a concurrent ref or
				// policy-file change declines replay instead of serving the earlier view.
				confirmedPolicy, confirmErr := sem.ResolveSearchReplayPolicy(ctx, repo, sem.SearchOptions{
					Worktree:     flags.Worktree,
					IgnoreFiles:  flags.IgnoreFiles,
					IncludeFiles: flags.IncludeFiles,
				})
				confirmedScope := searchSessionScopeFor(ctx, repo)
				confirmedScope.PolicyFingerprint = confirmedPolicy.Fingerprint()
				confirmedScope.Format = flags.Format
				if confirmErr == nil &&
					confirmedPolicy.Fingerprint() == replayPolicy.Fingerprint() &&
					confirmedPolicy.MatchesTree(confirmedScope.Tree) &&
					state.matches(confirmedScope) {
					if confirmedPolicy.AllowsReplayPaths(state.PayloadPaths) {
						// Path admission can stream a large tree. Recheck the identities and exact paths
						// after it so a concurrent ref or policy change cannot land in that interval and
						// still release the older payload. The second admission is intentional: Git's
						// effective worktree excludes include dynamic nested .gitignore inputs that are
						// not all part of the root-rule fingerprint.
						finalPolicy, finalErr := sem.ResolveSearchReplayPolicy(ctx, repo, sem.SearchOptions{
							Worktree:     flags.Worktree,
							IgnoreFiles:  flags.IgnoreFiles,
							IncludeFiles: flags.IncludeFiles,
						})
						finalScope := searchSessionScopeFor(ctx, repo)
						finalScope.PolicyFingerprint = finalPolicy.Fingerprint()
						finalScope.Format = flags.Format
						if finalErr == nil &&
							finalPolicy.Fingerprint() == confirmedPolicy.Fingerprint() &&
							finalPolicy.MatchesTree(finalScope.Tree) &&
							state.matches(finalScope) &&
							finalPolicy.AllowsReplayPaths(state.PayloadPaths) {
							// The persisted state is local input, not a trusted rendering sink. A live
							// text/agent response passes through termsafe; replay must preserve that
							// terminal-safety boundary even if the session file was tampered with.
							// termsafe.Writer, not the JSON-safe sink replaySearchPayload uses.
							// searchFormatSupportsReplay admits only text and agent here, so the
							// bytes below are terminal-bound prose: the full control rewrite is the
							// correct one, and the C1-only JSON rewrite would let a raw ESC or DEL
							// recorded by an older build through. The presearch sink is the opposite
							// case -- an arbitrary file whose format this process never chose -- and
							// takes the lossless JSON rewrite for that reason.
							replayOut := termsafe.NewWriter(opts.Stdout)
							if _, err := io.WriteString(replayOut, searchEchoHeader(flags.Query, state.Query)); err != nil {
								return err
							}
							_, err := io.WriteString(replayOut, state.Payload)
							return err
						}
					}
					// The candidate passed the first identity bracket but failed either path
					// admission or the final identity observation. Replace it after the fresh search.
					forceSessionReplace = true
				}
			}
		}
	}
	cacheDir := resolveCacheDir(flags.CacheDir, opts.Env.PluginDataDir)
	contextBudget := flags.MaxContextBytes
	// Agent output has a much smaller wire representation than the public JSON
	// response. Keep full snippets until that representation is budgeted below.
	if flags.Format == "agent" {
		flags.MaxContextBytes = 0
	}
	response, err := sem.SearchRepository(ctx, repo, opts.Version, flags.Query, sem.SearchOptions{
		Worktree:          flags.Worktree,
		IgnoreFiles:       flags.IgnoreFiles,
		IncludeFiles:      flags.IncludeFiles,
		Profile:           profile,
		TopK:              flags.TopK,
		ContextLines:      flags.ContextLines,
		MaxRegionLines:    flags.MaxRegionLines,
		MaxSnippetLines:   flags.MaxSnippetLines,
		MaxRegionsPerFile: flags.MaxRegionsPerFile,
		CacheDir:          cacheDir,
		DisableCache:      flags.DisableCache,
		MaxIndexedFiles:   flags.MaxIndexedFiles,
		IndexAllFiles:     flags.IndexAllFiles,
		MaxContextBytes:   flags.MaxContextBytes,
		// Only `--format text` renders the disclosure floor (writeTextSearch
		// below); json, ndjson and agent carry the same exclusion facts as data —
		// the W_REPO_IGNORED_SOURCE warning and the whole repo_ignored report —
		// outside the context budget. Charging their ranking for a floor they
		// never print bought them nothing and, at a tight ceiling, cost them
		// their only ranked result.
		OmitsRepoIgnoreDisclosureFloor: flags.Format != "text",
		BodyHeadRanks:                  flags.BodyHeadRanks,
		EnclosureContextLines:          flags.EnclosureContextLines,
		HeadWindowLines:                flags.HeadWindowLines,
		FullUnitTop:                    flags.FullUnitTop,
		EditSiteBodies:                 flags.EditSiteBodies,
		CalleeHop:                      flags.CalleeHop,
		VerifyPrefix:                   flags.VerifyPrefix,
		VerifyPreFixStatus:             flags.VerifyPreFixStatus,
		IncludeFileOutline:             flags.FileOutline,
		VerifyExplainCommand:           flags.VerifyExplain,
		Deep:                           flags.Deep,
		SingleResolution:               flags.SingleResolution,
		DocumentResolution:             flags.DocumentResolution,

		IncludeContainerMap:   flags.ContainerMap,
		IncludeSignatureTypes: flags.SignatureTypes,
		IncludeTypeCard:       flags.TypeCard,
	})
	if err != nil {
		return err
	}
	if err := response.Validate(); err != nil {
		return err
	}
	// With a session active the payload is teed, so the echo replays the exact bytes this call
	// handed the agent — the stored answer is the rendered output, not a re-render of the response.
	out := opts.Stdout
	var payload bytes.Buffer
	if session != nil {
		out = io.MultiWriter(opts.Stdout, &payload)
	}
	if err := writeSearchResponse(out, response, flags.Format, contextBudget); err != nil {
		return err
	}
	if session != nil {
		payloadPaths := sem.SearchResponsePaths(response)
		recordScope := scope
		// Empty is meaningful: a requested HEAD view can fall back to the worktree when no revision
		// is available. Never retain a tree observed before that fallback.
		recordScope.Tree = response.Tree
		replayable := false
		if replayPolicyReady {
			// Re-resolve after the search so a policy file changed concurrently cannot make a payload
			// produced under one policy replay under another one's fingerprint. A disagreement keeps
			// only the bounded search count; it never stores ambiguous response bytes.
			currentPolicy, policyErr := sem.ResolveSearchReplayPolicy(ctx, repo, sem.SearchOptions{
				Worktree:     flags.Worktree,
				IgnoreFiles:  flags.IgnoreFiles,
				IncludeFiles: flags.IncludeFiles,
			})
			if policyErr == nil {
				recordScope.PolicyFingerprint = currentPolicy.Fingerprint()
				replayable = searchResponseCanReplay(flags.Format, response) &&
					currentPolicy.Fingerprint() == replayPolicy.Fingerprint() &&
					currentPolicy.MatchesTree(response.Tree) &&
					currentPolicy.AllowsReplayPaths(payloadPaths)
			}
		}
		session.record(
			flags.Query,
			payload.Bytes(),
			payloadPaths,
			recordScope,
			forceSessionReplace,
			replayable,
		)
	}
	return nil
}

// searchSessionScopeFor describes the tree a payload recorded now would be answering for.
//
// Failure is not an error here. A directory git cannot describe still has a resolved path, and a
// path is enough to separate two checkouts in the common case; when even that is unavailable the
// zero scope matches nothing, so the echo is refused and the question gets a real answer. The one
// outcome this must never produce is a confident scope that is wrong.
func searchSessionScopeFor(ctx context.Context, repo string) searchSessionScope {
	scope := searchSessionScope{Repo: repo}
	if resolved, err := filepath.Abs(repo); err == nil {
		scope.Repo = resolved
	}
	if sem.EnsureGitMetadataSafeForSubprocess(repo) != nil {
		return scope
	}
	_, tree, err := gitutil.HeadCommitAndTree(ctx, repo)
	if err != nil {
		return scope
	}
	scope.Tree = tree
	return scope
}

func writeSearchResponse(out io.Writer, response sem.SearchResponse, format string, contextBudget int) error {
	switch format {
	case "json":
		encoder := json.NewEncoder(termsafe.NewJSONWriter(out))
		encoder.SetEscapeHTML(false)
		return encoder.Encode(response)
	case "ndjson":
		return writeNdjsonSearch(out, response)
	case "text":
		return writeTextSearch(out, response)
	case "agent":
		return writeAgentSearch(out, response, contextBudget)
	default:
		return fmt.Errorf("search --format must be json, ndjson, text, or agent, got %q", format)
	}
}

// searchFormatSupportsReplay is the pre-search half of searchResponseCanReplay. JSON and NDJSON
// always render corpus-wide statistics and completeness facts whose contributors are not carried
// as individual response paths, so those opaque payloads are never persisted or replayed. Agent
// output is conditionally safe; its diagnostic case is rejected after the live response exists.
func searchFormatSupportsReplay(format string) bool {
	return format == "text" || format == "agent"
}

func searchResponseCanReplay(format string, response sem.SearchResponse) bool {
	if !searchFormatSupportsReplay(format) {
		return false
	}
	// Agent diagnostics render corpus-wide language/file and warning/failure counts. Ordinary agent
	// output and text output render only fields whose direct source provenance is retained.
	return format != "agent" || (len(response.Warnings) == 0 && len(response.PartialFailures) == 0)
}

// echoPresearchPayload returns a payload that was computed before the agent started, verbatim.
//
// The toll this removes is the MESSAGE, not the bytes. Measured over 178 gated benchmark pairs,
// the number of graph calls a session makes is invariant — 1.00 / 1.03 / 1.00 per session across
// baseline-exploration bands <10 / 10-19 / >=20 turns — while what the call buys back is not:
// greps displaced go 1.35 / 2.16 / 3.74 over the same bands, and the cost ratio with it (1.140 CI
// [1.063,1.224] n=51 / 0.950 n=108 / 0.841 CI [0.704,0.998] n=19). Split by outcome instead of by
// band, the two cohorts separate cleanly: sessions where the call displaced no grep at all (n=43)
// cost 1.165 CI [1.073,1.264] and ran +1.21 turns, sessions where it displaced greps (n=135) cost
// 0.938 CI [0.889,0.990] and ran -2.07 turns. corr(turn delta, log cost ratio) = +0.735 CI
// [0.661,0.800] pair-level, +0.802 CI [0.662,0.885] repo-level — the strongest correlate in the
// set. Independently, a regression holding call count fixed still prices a +7.0% CI [1.9,13.2]
// tax on merely having made the call, and prices one call at 1.61 messages rather than 1.
//
// The toll is also unpredictable: no static feature of a payload predicts that its call will turn
// out to be a no-op (AUC 0.553 top score, 0.560 spread, 0.563 gap, 0.690 entropy, at a 28% base
// rate). Selective skipping is therefore not implementable — pre-delivery is the only form the
// fix can take, and it must be universal.
//
// Echoing rather than re-querying is what makes the delivery free: a call the agent still makes
// costs one message and adds zero information, so it cannot re-open the phase the pre-delivery
// closed. Retrieval is unaffected because the agent's own query adds almost nothing over the
// issue text the payload was derived from: of 188 pre-edit greps not covered by the payload, 2
// (1.1%) named a literal that was in the agent's query and not in the issue.
//
// A caller that names a payload it cannot produce gets an error, not a live query. Falling back
// silently would leave the arm measuring whichever mode each session happened to land in — the
// same reason an unknown --reference-blocks name is an error rather than a no-op.
//
// An EMPTY file is that same failure and is treated the same way. A zero-byte payload used to be
// written to stdout and reported as success, which is the worst available outcome: the agent asked
// a question, got nothing back, and every layer above — the exit code, the harness, the transcript
// — recorded a healthy call. A whole measured cell can run that way without anyone noticing, and
// a cell whose search verb returns nothing is not measuring the graph at all, it is measuring the
// baseline with extra steps. Whatever produced an empty file (a failed pre-computation, a
// truncated write, an unset variable expanding to nothing) is a bug in the caller, and the only
// safe report is a loud one.
//
// Note for whoever wires the caller: the payload must be computed with the SAME binary, flags and
// cached graph the session would have used, and the instruction telling the agent to search first
// has to go with it — the tool cannot be left un-called while that directive stands.
func echoPresearchPayload(out interface{ Write([]byte) (int, error) }, path string) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", envPresearch, err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("%s: %s is empty: a pre-delivered payload of zero bytes would answer the agent with nothing and still report success", envPresearch, path)
	}
	return replaySearchPayload(out, payload)
}

// replaySearchPayload is the sink every REPLAY of a stored search payload goes
// through, and it exists because wrapping the encoders does not reach these bytes.
//
// The machine-format rule is applied where a value is ENCODED. A replay encodes
// nothing: ENTIRE_GRAPH_PRESEARCH hands stdout a file another process wrote, and
// the search echo hands it a payload a previous PROCESS wrote — possibly a
// previous BUILD, one from before the rule existed. Those bytes carry whatever
// that writer left in them, so the rule is applied again on the way out, exactly
// as the warm provider-record cache in root.go re-applies it rather than trusting
// the entry that produced it.
//
// It is JSONWriter and not Writer, for both replay paths, because the escape has
// to be safe for a payload whose format this code does not know:
//
//   - JSONWriter's rewrite is lossless and leaves a machine format decodable —
//     \u009d is the JSON escape for the same code point. Writer's is not usable
//     here: it would rewrite DEL to \x7f, and DEL is a byte encoding/json leaves
//     RAW inside a string, so a payload holding one would come out of a replay
//     with an escape JSON does not define. Measured on Go 1.26.5,
//     `{"p":"a\x7fb"}` through Writer no longer decodes, and through JSONWriter
//     it does. Corrupting the document is a worse outcome than the byte, and
//     0x7f introduces no sequence.
//   - The text and agent renderers write through termsafe.Writer already, so a
//     payload recorded in those formats arrives here with its C1 controls ALREADY
//     in \u00XX form and every remaining byte below 0x80. JSONWriter leaves all of
//     that untouched, which is what makes one wrapper correct for every format a
//     payload can be in.
//
// The C1 range is the whole guarantee. A payload predating termsafe entirely
// could hold a raw ESC, and JSONWriter does not rewrite it — a C0 escape inside a
// JSON string would be an escape JSON does not define, the same way \x7f is.
func replaySearchPayload(out io.Writer, payload []byte) error {
	writer := termsafe.NewJSONWriter(out)
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	// This is the one-shot case JSONWriter's own doc calls out: a caller that
	// does not chunk its stream must still call Close, because a payload whose
	// LAST byte happens to be a bare 0xc2 (an incomplete two-byte UTF-8 lead,
	// not necessarily a C1 sequence at all) is withheld by Write pending a
	// continuation byte that this replay's single Write call will never
	// supply. Skipping Close silently drops that trailing byte from the
	// replayed payload.
	return writer.Close()
}

// writeNdjsonSearch streams a payload as one record per line: a header, the blocks that are their own
// records, every ranked result, and a summary that carries the rest.
func writeNdjsonSearch(out interface{ Write([]byte) (int, error) }, response sem.SearchResponse) error {
	{
		encoder := json.NewEncoder(termsafe.NewJSONWriter(out))
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(map[string]any{
			"record_type": "search_header",
			// The header record carries the envelope version for the same reason
			// the JSON payload does: a consumer reading the stream must be able to
			// branch on the shape before it parses a single result record.
			"format_version": response.FormatVersion,
			"query":          response.Query,
			"repo_root":      response.RepoRoot,
			"commit":         response.Commit,
			"tree":           response.Tree,
			"profile":        response.Profile,
		}); err != nil {
			return err
		}
		// The exclusion disclosure leads the stream, ahead of the results, because NDJSON is
		// consumed a record at a time: a consumer that acts on the first usable result never
		// reaches a trailing summary, and a disclosure it never reads reproduces exactly the
		// blind spot this payload exists to close. It is REPEATED on the summary below, so a
		// consumer that already parses `repo_ignored` there keeps working unchanged.
		if response.RepoIgnored != nil {
			if err := encoder.Encode(struct {
				RecordType string `json:"record_type"`
				*sem.RepoIgnoreReport
			}{RecordType: "search_repo_ignored", RepoIgnoreReport: response.RepoIgnored}); err != nil {
				return err
			}
		}
		// The closed-set warning leads the stream for the same reason it leads the text output: it is
		// the one record that changes what the patch has to contain.
		if response.ClosedSet != nil {
			if err := encoder.Encode(struct {
				RecordType string `json:"record_type"`
				*sem.SearchClosedSet
			}{RecordType: "search_closed_set", SearchClosedSet: response.ClosedSet}); err != nil {
				return err
			}
		}
		if response.ContainerMap != nil {
			if err := encoder.Encode(struct {
				RecordType string `json:"record_type"`
				*sem.SearchContainerMap
			}{RecordType: "search_container_map", SearchContainerMap: response.ContainerMap}); err != nil {
				return err
			}
		}
		for _, result := range response.Results {
			if err := encoder.Encode(struct {
				RecordType string `json:"record_type"`
				sem.SearchResult
			}{RecordType: "search_result", SearchResult: result}); err != nil {
				return err
			}
		}
		summary := map[string]any{
			"record_type":      "search_summary",
			"stats":            response.Stats,
			"warnings":         response.Warnings,
			"partial_failures": response.PartialFailures,
			"completeness":     response.Completeness,
		}
		// The declaration card rides on the summary rather than on a record of its own: it is
		// one block about the whole payload, and a streaming consumer that does not know the key
		// keeps working. The verify command and the literal cluster ride along for the same reason.
		if len(response.TypeCard) > 0 {
			summary["type_card"] = response.TypeCard
		}
		if response.VerifyCommand != nil {
			summary["verify_command"] = response.VerifyCommand
		}
		if response.LiteralCluster != nil {
			summary["literal_cluster"] = response.LiteralCluster
		}
		if response.CoverageNote != nil {
			summary["coverage_note"] = response.CoverageNote
		}
		if response.RepoIgnored != nil {
			summary["repo_ignored"] = response.RepoIgnored
		}
		// The signature-types block rides on the summary for the same reason the
		// declaration card does. It was the one optional block with no serializer
		// at all, so `--format ndjson --signature-types` charged the caller for
		// the work, counted its bytes in the statistics, and then emitted a stream
		// that did not contain it.
		if len(response.SignatureTypes) > 0 {
			summary["signature_types"] = response.SignatureTypes
		}
		return encoder.Encode(summary)
	}
}

// searchTextFullRanks is how deep `--format text` renders a full snippet for a result the
// snippet allocator did NOT upgrade to a complete body. Below it a hit is used only to decide
// "is this worth opening", so a full window there is token waste.
const searchTextFullRanks = 2

// searchTextMaxFullBodies caps how many BODIES the default text payload prints, however many the
// allocator upgraded.
//
// Turn-level forensics of 12 real sessions: payloads were 45-75% noise by byte, and the bodies past
// the third were never referenced — not once across the 12. The allocator is still right to buy them
// (it is optimising the ranking, and --full-unit-top/--callee-hop callers want them), so this is a
// RENDERING cap: everything past the third body degrades to its one-line locator, which is the part
// that was actually used. The flags keep working; they raise this ceiling for the ranks they force.
const searchTextMaxFullBodies = 3

// Section headers for `--format text`. They label what the group IS, because the failure they
// exist to prevent is an agent treating a non-fix-site as the fix site.
const (
	searchTextRelatedHeader = "RELATED SITES (the same change usually also lands here):"
	searchTextDocsHeader    = "DOCS & FIXTURES (matched the query; not fix sites):"
	searchTextTestHeader    = "COVERING TEST (what hit 1 is supposed to do; not a fix site):"
	searchTextTypesHeader   = "DECLARATIONS (names hit 1 uses; edit against these):"
	// The types the top hit's signature names. Placed last of the reference blocks: it is
	// material for writing the patch, not a place the patch might land.
	searchTextSignatureTypesHeader = "TYPES IN THIS SIGNATURE (fields & impl surface; names and signatures only):"
)

// writeTextSearch renders the default `--format text` output, grouped by section.
//
// The primary group is the only one presented as candidate fix sites. It is tiered, and the
// tier is decided by what the result CARRIES rather than by rank alone: a result the allocator
// spent budget to return as a complete function body is always printed in full — dropping it
// here would throw away the very bytes that let an agent edit without opening the file — while
// an ordinary window is printed for the first `searchTextFullRanks` entries of the group and
// collapsed to a terse "N. path:line symbol" locator below that.
//
// The tier counts POSITION IN THE GROUP, not absolute rank. A non-code hit that outranks the
// code is sectioned away, and charging its rank against the full-snippet tier would leave the
// agent one full window short for no reason.
//
// Ranks are the payload's own, unchanged, so a rank missing from the primary group is a visible
// signal that it was sectioned rather than dropped.
func writeTextSearch(out interface{ Write([]byte) (int, error) }, response sem.SearchResponse) error {
	// Everything below prints repository-controlled bytes: paths, snippets lifted
	// verbatim from source, declarations read off a line of the scanned tree. The
	// guarantee that none of them reaches the terminal as a control sequence is
	// made once, here, rather than at the dozens of print sites in this function
	// and in the sem renderers it calls.
	out = termsafe.NewWriter(out)
	// ORDER: the forgery notice first, the ignore disclosure second. Both blocks
	// claim the head of the payload and both keep it -- the forgery notice says the
	// bytes below may be lying about who wrote them, which a reader must know before
	// reading any of them, while the disclosure only has to precede the RESULTS. The
	// notice is a constant that carries no repository bytes, so it is outside the
	// budget arithmetic the disclosure does below and cannot shrink it.
	// The literal cluster is quarantined here, well before it is printed, so its verdict can feed
	// the notice below. It is the one sem block that writes a repository body unprefixed and
	// verbatim (search_literals.go, --edit-site-bodies), so it needs the same quarantine a ranked
	// snippet gets. The rewrite is applied to the BODIES and the block is rendered from the result,
	// not applied to the rendered bytes: see searchQuarantineLiteralCluster for why the renderer's
	// own header cannot survive being re-classified from bytes alone.
	quarantinedCluster, literalClusterForged := searchQuarantineLiteralCluster(response.LiteralCluster)
	literalCluster := sem.RenderSearchLiteralCluster(quarantinedCluster)
	// THE NOTICE IS DECIDED BY THE EXACT RENDERING PASS, without retaining that rendering. The
	// dry pass follows the same diet and section decisions as the real pass and records only whether
	// repository source that needed quarantine was selected. This keeps the notice exact while the
	// real payload remains incremental even when the caller disables the context-byte limit.
	//
	// The notice says "some source lines quoted below", and the diet below means the response is
	// not the payload: a ranked body past searchTextMaxFullBodies collapses to a locator and a
	// related site prints as one line with no source at all. Asking the RESPONSE therefore warned
	// about a line no reader could see, which is a false sentence and costs an honest repository
	// the "pays nothing for it" property searchForgeryNotice is documented to have. It cannot be
	// answered by predicting the diet either — a second copy of that decision is a copy that can
	// disagree with the loop, and disagreeing in the other direction drops the notice off a body
	// that IS printed. The rendered bytes are the one question no later step can outrun; it is the
	// sink test writeAgentSearch already applies (searchPayloadDisclosesItsQuarantine), asked here
	// in the other direction.
	//
	// The dry pass does not build a produced-line set or retain repository bytes; it records one bit.
	sectionForged, err := writeTextSearchSections(io.Discard, response, literalCluster)
	if err != nil {
		return err
	}
	forged := literalClusterForged || sectionForged
	// Ahead of everything, including the closed-set warning: it is the only block that says the
	// payload's own bytes may be lying about who wrote them, and a reader who has already acted on
	// a forged line will not come back for it.
	if forged {
		if _, err := out.Write(searchForgeryNotice); err != nil {
			return err
		}
	}
	// FIRST, ahead of every result: what the repository's own ignore rules took out
	// of the corpus this answer came from. A reader who has already read the ranked
	// hits has decided the answer is complete, so a disclosure printed after them is
	// a disclosure nobody acts on.
	//
	// This block is repository-controlled (excluded path names, .gitignore/
	// .graphignore file names, unreadable directory names) and is written here
	// AHEAD of the ranked results that response.Stats.ResultBytes was already fit
	// to response.Stats.ContextBudgetBytes for — so it is charged against that
	// same ceiling rather than added on top of it. What a ceiling too small for it
	// buys is a SHORTER disclosure, never a missing one (see the floor below); the
	// JSON channel carries the full, uncapped report regardless
	// (response.RepoIgnored).
	if block := sem.RenderRepoIgnoreDisclosure(response.RepoIgnored); len(block) > 0 {
		budget := response.Stats.ContextBudgetBytes
		// Charged against the REMAINING headroom, not against the whole ceiling.
		// The ranked results and the funded blocks were already fitted to this
		// same budget, so asking only whether the disclosure fits ON ITS OWN
		// admitted it on top of a payload already at the ceiling -- the rendered
		// output then exceeded --max-context-bytes by the full block, which is
		// repository-controlled and so attacker-sized.
		//
		// The three terms are the ones validateSearchContextBlockBudget funds
		// from inside the ceiling (search_blocks.go); keep them in step.
		funded := response.Stats.ResultBytes + response.Stats.TypeCardBytes + response.Stats.SignatureTypeBytes
		// DEGRADE, NEVER OMIT. Charging the block against the headroom is right; going
		// silent when that headroom is gone is not, and it failed in exactly the case
		// this disclosure exists for. The fitter spends the ceiling down to the last few
		// bytes (measured end to end: result_bytes 1993 of a 2000-byte ceiling, 3977 of
		// 4000), so a repository with enough material to answer the query leaves no
		// headroom — and `--format text` renders no warnings and no fallback marker of
		// its own, so the ONLY signal that a committed rule removed content disappeared.
		// The busy repository with many exclusions got the most complete-LOOKING answer.
		//
		// The full block still has to fit the headroom, because the repository sizes it.
		// Under it sits an irreducible floor carrying no repository-controlled bytes at
		// all — one sentence, one integer, a pointer at the JSON channel — and the floor
		// is admitted against the CEILING rather than the headroom. That is the trade: a
		// payload may exceed --max-context-bytes by at most the floor's documented bound,
		// an amount this repository picks and a caller can budget for, rather than stay
		// byte-exact by telling a reader a corpus was whole when it was not.
		//
		// A ceiling smaller than the floor itself prints nothing: the caller asked for
		// less room than the shortest honest disclosure needs.
		if budget > 0 && funded+len(block) > budget {
			block = sem.RenderRepoIgnoreDisclosureFloor(response.RepoIgnored)
			if len(block) > budget {
				block = nil
			}
		}
		if len(block) > 0 {
			if _, err := out.Write(block); err != nil {
				return err
			}
		}
	}
	if notice, _ := searchLowConfidenceNotices(response); len(notice) > 0 {
		if _, err := out.Write(notice); err != nil {
			return err
		}
	}
	_, err = writeTextSearchSections(out, response, literalCluster)
	return err
}

// writeTextSearchSections renders everything below the two leading notices, into a writer the
// caller supplies rather than straight to the terminal sink. The split exists so the forgery
// disclosure — which has to come FIRST — can be decided by the same control flow that prints the
// sections while the actual payload is still written incrementally.
func writeTextSearchSections(out interface{ Write([]byte) (int, error) }, response sem.SearchResponse, literalCluster []byte) (bool, error) {
	forged := false
	// The closed-set warning precedes everything, including the map: it is the only block that
	// changes what the patch has to CONTAIN, and a reader who has already written the edit will not
	// come back for it.
	if block := sem.RenderSearchClosedSet(response.ClosedSet); len(block) > 0 {
		if _, err := out.Write(block); err != nil {
			return forged, err
		}
	}
	// The map is printed before any body: it is what tells the reader whether the ranked
	// bodies below it are the whole change, and an agent that has already decided to read the
	// file will not scroll back for it.
	if block := sem.RenderSearchContainerMap(response.ContainerMap, false); len(block) > 0 {
		if _, err := out.Write(block); err != nil {
			return forged, err
		}
	}
	primary, related, docs, tests := partitionSearchSections(response.Results)
	// THE DIET. A body is printed for the first searchTextMaxFullBodies entries that carry source, and
	// every entry after that prints as a locator whatever the allocator gave it. Ranks the caller
	// explicitly forced (full-unit / callee-hop) are exempt: asking for a unit and being handed a
	// locator would make the flag a no-op.
	bodies := 0
	// POSITION IN THE BODY QUEUE, not index in the group. A low-value path that is demoted below
	// spends no body slot AND no rank tier: charging its rank against the full-snippet tier would
	// leave the agent one full window short for exactly the hits that are real source, which is the
	// same reason a sectioned-away non-code hit does not charge its rank either (see the doc comment
	// above). Without low-value hits in the group this is the group index, so the ordinary payload is
	// byte-identical.
	position := 0
	demoteLowValue := searchTextDemotesLowValueBodies(primary)
	// THE RE-ANCHOR FUNDING INVARIANT, the renderer's half. A body a hit can only claim because it was
	// re-anchored onto code (sem/search_reanchor.go) is funded out of SPARE slots, never out of a slot an
	// ordinary hit would have taken — the same rule seatForcedSearchUnits applies to forced units, and
	// the diet's three slots are the currency here rather than bytes.
	//
	// Measured on vuejs__core-11870: `arrayInstrumentations.ts:10→12` picked up a complete body, became
	// the third body in rank order, and pushed `runtime-core/src/helpers/renderList.ts:54-107` — 54 lines
	// of the production helper the issue is actually about — out of the payload as a bare locator. Net:
	// the payload traded a body it had for a body it did not need.
	//
	// So the ordinary hits' demand is counted FIRST and the re-anchored ones take what is left. A denied
	// re-anchored hit keeps its re-anchored `focus=`, which is the larger part of the win and costs
	// nothing. It still charges a tier position, in this pass and in the demand count above, because the
	// two loops have to walk the same queue to agree.
	reanchorSlots := searchTextMaxFullBodies - searchTextOrdinaryBodyDemand(primary, demoteLowValue)
	for _, result := range primary {
		if demoteLowValue && searchLowValueBodyPath(result.FilePath) && !searchResultForcedByFlag(result) {
			forged = forged || textSearchLocatorQuotesForgedBody(result)
			writeTextSearchLocator(out, result)
			continue
		}
		full := position < searchTextFullRanks || searchResultCarriesCompleteBody(result)
		position++
		if full && searchResultBodyIsReanchorGained(result) {
			if reanchorSlots <= 0 {
				full = false
			} else {
				reanchorSlots--
			}
		}
		if full && bodies >= searchTextMaxFullBodies && !searchResultForcedByFlag(result) {
			full = false
		}
		if full {
			bodies++
			forged = forged || textSearchResultQuotesForgedBody(result, true)
			writeTextSearchResult(out, result, true)
			continue
		}
		// The diet has to write the locator ITSELF. writeTextSearchResult re-checks
		// searchResultCarriesCompleteBody and prints a body whenever the signal is present, which
		// silently neutralised the cap: on carbon-2752 all five hits still came back bodied. That
		// re-check exists so the RANK TIER cannot throw away source the allocator paid for, and it is
		// right for that job — but a cap the caller set is a decision, not an accident.
		forged = forged || textSearchLocatorQuotesForgedBody(result)
		writeTextSearchLocator(out, result)
	}
	// Contract context before the related and docs groups: it is about the hit the reader has
	// just read, and it is the part that decides whether the edit is the right SHAPE.
	if len(tests) > 0 {
		fmt.Fprintf(out, "%s\n", searchTextTestHeader)
		for _, result := range tests {
			forged = forged || textSearchResultQuotesForgedBody(result, true)
			writeTextSearchResult(out, result, true)
		}
		// Directly under the body it qualifies: "this is what the code must do" and "these are the
		// other tests that say so" are one thought, and a reader who has taken the contract off the
		// body above will not scroll for the second half.
		if block := sem.RenderSearchCoverageNote(response.CoverageNote); len(block) > 0 {
			if _, err := out.Write(block); err != nil {
				return forged, err
			}
		}
	}
	// VERIFY immediately after the test it was derived from: what the edit has to achieve and the
	// command that proves it are one thought.
	if block := sem.RenderSearchVerifyCommand(response.VerifyCommand); len(block) > 0 {
		if _, err := out.Write(block); err != nil {
			return forged, err
		}
	}
	if block := sem.RenderSearchFileOutline(response.FileOutlines); len(block) > 0 {
		out.Write(block)
		fmt.Fprintln(out)
	}
	if len(literalCluster) > 0 {
		if _, err := out.Write(literalCluster); err != nil {
			return forged, err
		}
	}
	// The two reference blocks stay ADJACENT and both stay ahead of the related and docs
	// groups. They answer the same kind of question about the same subject — "what are the
	// names hit 1 is written in terms of" — so splitting them across the related-site block
	// would make the reader hold hit 1 in mind across two unrelated groups. Signature types
	// come first of the two: they are the anchor's own CONTRACT (what callers depend on and a
	// patch must not break), while the declaration card is about identifiers inside its body.
	if block := renderSignatureTypes(response.SignatureTypes); len(block) > 0 {
		if _, err := out.Write(block); err != nil {
			return forged, err
		}
	}
	writeTextSearchTypeCard(out, response.TypeCard)
	if len(related) > 0 {
		fmt.Fprintf(out, "%s\n", searchTextRelatedHeader)
		for _, result := range related {
			result = searchResultOnOneLine(result)
			name := searchResultDisplayName(result)
			fmt.Fprintf(out, "%d. %s:%d", result.Rank, result.FilePath, searchResultLocatorLine(result))
			if name != "" {
				fmt.Fprintf(out, " symbol=%s", name)
			}
			fmt.Fprintf(out, " (%s)\n", sem.RelatedSiteKind(result))
		}
	}
	if len(docs) > 0 {
		fmt.Fprintf(out, "%s\n", searchTextDocsHeader)
		for _, result := range docs {
			forged = forged || textSearchResultQuotesForgedBody(result, false)
			writeTextSearchResult(out, result, false)
		}
	}
	return forged, nil
}

func textSearchResultQuotesForgedBody(result sem.SearchResult, full bool) bool {
	if !full && !searchResultCarriesCompleteBody(result) {
		return false
	}
	if searchBodyCarriesRecordShape(result.Snippet) {
		return true
	}
	for _, passage := range result.Passages {
		if searchBodyCarriesRecordShape(passage.Snippet) {
			return true
		}
	}
	return false
}

func textSearchLocatorQuotesForgedBody(result sem.SearchResult) bool {
	if searchResultDisplayName(result) != "" {
		return false
	}
	_, _, window := sem.SearchLocatorWindow(result)
	return searchBodyCarriesRecordShape(window)
}

// renderSignatureTypes prints the declarations of the types the top hit's own
// signature names. It answers "what can I do with one of these" without a file
// read: names and signatures only, capped per type, with the omitted count so a
// truncated list can never be mistaken for a complete one.
func renderSignatureTypes(types []sem.SearchSignatureType) []byte {
	if len(types) == 0 {
		return nil
	}
	var buffer strings.Builder
	buffer.WriteString(searchTextSignatureTypesHeader + "\n")
	for _, entry := range types {
		fmt.Fprintf(&buffer, "  %s  %s:%d\n", termsafe.Line(entry.Name), termsafe.Line(entry.FilePath), entry.StartLine)
		if len(entry.Fields) > 0 {
			line := "    fields: " + termsafe.Line(strings.Join(entry.Fields, ", "))
			if omitted := entry.FieldsTotal - len(entry.Fields); omitted > 0 {
				line += fmt.Sprintf(" (+%d more)", omitted)
			}
			fmt.Fprintln(&buffer, line)
		}
		if len(entry.Methods) > 0 {
			line := fmt.Sprintf("    impl %s: %s", termsafe.Line(entry.Name), termsafe.Line(strings.Join(entry.Methods, ", ")))
			if omitted := entry.MethodsTotal - len(entry.Methods); omitted > 0 {
				line += fmt.Sprintf(" (+%d more)", omitted)
			}
			fmt.Fprintln(&buffer, line)
		}
	}
	return []byte(buffer.String())
}

// searchLowValueBodyDirSegments are path segments whose files are program text a fix essentially
// never lands in: the benchmark harness, the demo app, the example gallery. They are real code, so
// the ranker is right to score them and the payload is right to LIST them — a benchmark that
// exercises the broken API is a legitimate lead — but a body from one of them costs the same bytes
// as a body from the library and answers a different question.
//
// Measured on preactjs__preact-3010: `benches/src/keyed-children/index.js` and `karma.conf.js` took
// two of the three body slots (the third went to `src/component.js`) while the gold file,
// `src/diff/children.js`, arrived as a bodyless two-line locator.
var searchLowValueBodyDirSegments = map[string]bool{
	"bench": true, "benches": true, "benchmark": true, "benchmarks": true,
	"demo": true, "demos": true, "example": true, "examples": true,
}

// searchLowValueBodySuffixes are build/test-runner configuration scripts. They are `.js`/`.ts`, so
// they are program text and no data-file rule catches them, and they name the very packages and
// entry points a query is built from — which is how `karma.conf.js` outranked the library it
// configures.
var searchLowValueBodySuffixes = []string{".conf.js", ".config.js", ".karma.js"}

// searchLowValueBodyPath reports whether a path's bodies must yield their slot to real source.
// Directory names are matched as whole path SEGMENTS, so `benches/` is caught and
// `src/benchmarks_test_helper.js` is not.
func searchLowValueBodyPath(filePath string) bool {
	if filePath == "" {
		return false
	}
	lower := strings.ToLower(filepath.ToSlash(filePath))
	for _, suffix := range searchLowValueBodySuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	segments := strings.Split(strings.Trim(lower, "/"), "/")
	if len(segments) > 0 {
		segments = segments[:len(segments)-1]
	}
	for _, segment := range segments {
		if searchLowValueBodyDirSegments[segment] {
			return true
		}
	}
	return false
}

// searchTextDemotesLowValueBodies reports whether the group holds enough real source for the
// demotion to be safe. A payload whose only program text IS the benchmark or the config script must
// still print it: the rule is "never outrank real source for a body slot", not "never show a body".
// Two is the threshold because the body diet's own tier is two full windows — below that the
// demotion would be taking bytes away from a reader with nothing to read instead.
func searchTextDemotesLowValueBodies(primary []sem.SearchResult) bool {
	real := 0
	for _, result := range primary {
		if searchLowValueBodyPath(result.FilePath) || result.Snippet == "" {
			continue
		}
		real++
		if real >= 2 {
			return true
		}
	}
	return false
}

// searchResultBodyIsReanchorGained reports whether this hit's body exists only because the payload
// moved its anchor onto code. The flag is set by the allocator's own control comparison, NOT inferred
// from CommentFocusLine: most re-anchored hits already carried a body, and gating those would evict
// source the re-anchor never paid for. A hit the CALLER forced is never gated either — a flag that
// silently stops producing what it promises is worse than no flag.
func searchResultBodyIsReanchorGained(result sem.SearchResult) bool {
	return result.BodyFromReanchor && !searchResultForcedByFlag(result)
}

// searchTextOrdinaryBodyDemand counts the body slots the NON-re-anchored hits will claim, walking the
// same queue the render loop walks so the two agree on every tier position.
func searchTextOrdinaryBodyDemand(primary []sem.SearchResult, demoteLowValue bool) int {
	demand, position := 0, 0
	for _, result := range primary {
		if demoteLowValue && searchLowValueBodyPath(result.FilePath) && !searchResultForcedByFlag(result) {
			continue
		}
		full := position < searchTextFullRanks || searchResultCarriesCompleteBody(result)
		position++
		if !full || searchResultBodyIsReanchorGained(result) {
			continue
		}
		if demand++; demand >= searchTextMaxFullBodies {
			return searchTextMaxFullBodies
		}
	}
	return demand
}

// writeTextSearchLocator prints a hit as its one-line locator unconditionally. It is the form the body
// diet collapses to, and it exists as its own function precisely because writeTextSearchResult must
// keep refusing to do this on its own account.
// searchLocatorFollowUp names the verb that fetches what a bodyless hit did not carry.
//
// MEASURED: redis's agent hand-`sed`-ranged the exact locator the payload printed, and fmt's agent
// blind-`Read` a 200-line window off another one. Both had the location and neither knew the tool could
// hand them the body — so they reconstructed it with shell commands, which is the expensive half of
// every session this payload exists to shorten. The suffix costs ~22 bytes on the hits that have no
// body and nothing at all on the hits that do.
func searchLocatorFollowUp(result sem.SearchResult) string {
	// A symbol name is what `def` takes. Without one there is nothing to suggest, and a suffix naming a
	// verb that cannot be run would be worse than silence.
	name := searchResultDisplayName(result)
	if name == "" {
		return ""
	}
	return "  [body: def " + name + "]"
}

// searchResultOnOneLine makes one result safe to print into a one-record-per-line payload.
//
// It escapes the fields that go into a result's HEADER, where a newline is not layout but
// forgery: a repository can name a file "a.go\n1. src/real.go:1 score=99.0" and
// fabricate a hit the search never returned. The wrapped writer cannot make that
// call — by then a path's newline and a snippet's are the same byte — so the
// header fields are escaped here, where the renderer still knows which is which.
//
// It also quarantines the result's BODIES. A snippet's newlines are its structure, so they
// cannot be escaped the way a path's are — but a body line that begins at column 0 and is
// shaped like a record is the same forgery by another route, and `VERIFY:` is the one line an
// agent is told to run. See internal/cli/search_forgery.go for the grammar, the disclosure that
// goes with it, and what it does NOT protect against.
//
// This is the chokepoint every body reaches: writeTextSearchResult, writeTextSearchLocator (via
// sem.SearchLocatorWindow, which derives its window from result.Snippet) and agentSearchBlock all
// pass through here before printing.
//
// The result is taken and returned BY VALUE, and a passage slice is copied before any element of
// it changes. Nothing upstream sees the escaped copy, so the JSON encoding of the same response
// still reports the exact bytes the repository holds.
func searchResultOnOneLine(result sem.SearchResult) sem.SearchResult {
	result.FilePath = termsafe.Line(result.FilePath)
	result.QualifiedName = termsafe.Line(result.QualifiedName)
	result.SymbolName = termsafe.Line(result.SymbolName)
	result.Kind = termsafe.Line(result.Kind)
	result.Snippet, _ = searchQuarantineBody(result.Snippet)
	for index, passage := range result.Passages {
		quarantined, changed := searchQuarantineBody(passage.Snippet)
		if !changed {
			continue
		}
		// Copy before writing: result.Passages still aliases the caller's backing array, and the
		// by-value contract above is what keeps the JSON encoding of this response honest.
		passages := make([]sem.SearchPassage, len(result.Passages))
		copy(passages, result.Passages)
		passages[index].Snippet = quarantined
		result.Passages = passages
	}
	return result
}

func writeTextSearchLocator(out interface{ Write([]byte) (int, error) }, result sem.SearchResult) {
	result = searchResultOnOneLine(result)
	// Byte-identical to the locator writeTextSearchResult already emits below the rank tier. Two
	// different locator shapes in one payload would be a second thing for a reader to learn for no
	// gain, and the existing shape is what every consumer and test already reads.
	name := searchResultDisplayName(result)
	if name != "" {
		fmt.Fprintf(out, "%d. %s:%d %s%s\n", result.Rank, result.FilePath,
			searchResultLocatorLine(result), name, searchLocatorFollowUp(result))
		return
	}
	// NO NAME MEANS NO LOCATOR. The demotion contract above is "path, line and symbol name all
	// survive", and a hit whose focus line has no enclosing indexed symbol has no name to survive
	// with — see sem.SearchLocatorWindow for what lands here and how often. `searchLocatorFollowUp`
	// has already refused for the same reason, so the line would carry no source, no symbol and no
	// verb: coordinates and an obligatory file read. It keeps the bounded window the ranker already
	// computed for it instead, which costs only bytes the ranker had already allocated.
	//
	// The shape is the bodied shape with the fields a nameless hit does not have left off, so this
	// is not a third thing to learn: a range header, the matched line, then the source under it.
	// `focus=` is not optional here — the bare locator's ONE piece of information was the matched
	// line, and a range that silently replaced it would trade information for bytes.
	if start, end, window := sem.SearchLocatorWindow(result); window != "" {
		fmt.Fprintf(out, "%d. %s:%d-%d", result.Rank, result.FilePath, start, end)
		if result.FocusLine >= start && result.FocusLine <= end && end > start {
			fmt.Fprintf(out, " focus=%d", result.FocusLine)
		}
		fmt.Fprintf(out, "\n%s\n\n", window)
		return
	}
	fmt.Fprintf(out, "%d. %s:%d\n", result.Rank, result.FilePath, searchResultLocatorLine(result))
}

func writeTextSearchResult(out interface{ Write([]byte) (int, error) }, result sem.SearchResult, full bool) {
	result = searchResultOnOneLine(result)
	name := searchResultDisplayName(result)
	if !full && !searchResultCarriesCompleteBody(result) {
		// A locator is a single line number. Where the graph knows the named symbol's true extent,
		// print it: `main.c:638 umain` invites a blind sweep of the file, while
		// `main.c:638 umain (270-725, 456L)` is a read instruction. This is the cheapest possible
		// fix for the most expensive measured behaviour — jq swept 26,873 B across three reads to
		// recover what one anchored read would have given.
		span := ""
		if result.SymbolStartLine > 0 && result.SymbolEndLine >= result.SymbolStartLine {
			span = fmt.Sprintf(" (%d-%d, %dL)", result.SymbolStartLine, result.SymbolEndLine,
				result.SymbolEndLine-result.SymbolStartLine+1)
		}
		if name != "" {
			fmt.Fprintf(out, "%d. %s:%d %s%s%s\n", result.Rank, result.FilePath,
				searchResultLocatorLine(result), name, span, searchLocatorFollowUp(result))
		} else {
			fmt.Fprintf(out, "%d. %s:%d%s\n", result.Rank, result.FilePath, searchResultLocatorLine(result), span)
		}
		return
	}
	// The range is the one actually PRINTED below, not the ranked region: a header that names a
	// span the snippet does not contain sends the reader to the wrong lines.
	start, end := searchResultPrintedRange(result)
	fmt.Fprintf(out, "%d. %s:%d-%d", result.Rank, result.FilePath, start, end)
	// A merged span says so, and says the one thing that decides whether the reader re-reads the
	// region anyway: that the range is contiguous and nothing between the hits it absorbed has
	// been left out. Without that, a wide range reads exactly like a stitched-together excerpt,
	// which is what an agent opens the file to check — the measured turn-2 behaviour this merge
	// exists to remove (fluentd-3328: `sed -n '330,470p'` over a superset of what it already had).
	if len(result.MergedRanks) > 0 {
		fmt.Fprintf(out, " [contains ranks %s - contiguous, nothing elided]", joinSearchRanks(result.MergedRanks))
	}
	// A block that carries no relevance score must not print one. The covering test is not a
	// ranked answer — it is the statement of what the fix has to achieve — and `score=0.0000`
	// beside it reads as "worthless" rather than "not applicable".
	// A callee-hop entry is excluded for the same reason: it was admitted by a CALLS edge, not by
	// relevance, so it carries no ranked score and `score=0.0000` beside it would read as
	// "worthless" rather than "not applicable". The signals list already says why it is here.
	if result.Section != sem.SearchSectionCoveringTest && !searchResultIsCalleeHop(result) {
		fmt.Fprintf(out, " score=%.4f", result.Score)
	}
	if name != "" {
		fmt.Fprintf(out, " symbol=%s", name)
		// When the printed lines are only a SHARD of the named symbol, say so and give the symbol's
		// true extent. Otherwise `main.c:578-583 symbol=umain` reads as "umain is six lines", and an
		// agent that needs the rest has no anchor to read from.
		//
		// Measured on jqlang/jq-2235: `umain` spans 270-725 (456 lines) and the payload showed lines
		// 578-583 with no indication of that. The agent then swept main.c in three reads totalling
		// 26,873 B (lines 1-697, effectively the whole file) while the no-tool baseline used `grep -n`
		// anchors and read 17,536 B. The same shape cost php-cs-fixer 77,718 B of blind sweeps against
		// the baseline's 10,784 B of anchored reads.
		if result.SymbolStartLine > 0 && result.SymbolEndLine >= result.SymbolStartLine &&
			(result.SymbolStartLine < start || result.SymbolEndLine > end) {
			fmt.Fprintf(out, " span=%d-%d (%dL, body elided - read %d-%d for the rest)",
				result.SymbolStartLine, result.SymbolEndLine,
				result.SymbolEndLine-result.SymbolStartLine+1,
				result.SymbolStartLine, result.SymbolEndLine)
		}
	}
	// `kind=` says what sort of declaration the reader is looking at (function / method /
	// class / enum / route). A name alone does not: `Ops` reads the same whether it is the
	// enum whose variants the task adds to or the struct that consumes them, and that
	// distinction is what decides whether the closed-set warning applies to this hit.
	if result.Kind != "" {
		fmt.Fprintf(out, " kind=%s", result.Kind)
	}
	// THE LINE THE QUERY ACTUALLY MATCHED, not just the range that contains it.
	//
	// FocusLine is computed for every hit and has always been in the JSON. The text payload — which
	// is what an agent reads — printed only the snippet's range, so a hit on a 27-line method said
	// "the answer is somewhere in 135-161" and made the reader find the line itself. Asked what one
	// change to the returned payload would let it finish in the fewest calls, a Sonnet agent that had
	// just fixed apache/lucene-13170 from this tool named exactly this: its only non-essential call
	// was a Read of 135-161 to "pin down line 151 specifically before editing", and it wanted the
	// matched line marked so "the payload doubles as both 'here's the function' and 'here's the
	// precise line to change'". Re-reading a file the payload already printed is 10.1% of all
	// post-payload tool calls in the measured Sonnet sessions.
	//
	// It goes in the HEADER rather than as an inline `>>> 151:` marker on the snippet line, which is
	// what the agent literally asked for. Agents copy snippet text verbatim as the `old_string` anchor
	// of an edit, so decorating a body line would make that anchor fail to match the file and turn a
	// navigation aid into a broken patch.
	if result.FocusLine >= start && result.FocusLine <= end && end > start {
		// A re-anchored hit prints BOTH lines. The comment line is the evidence for why the hit is
		// in the payload at all, and a reader told only the code line cannot tell a hit the query
		// matched directly from one the payload moved (see sem/search_reanchor.go).
		if result.CommentFocusLine > 0 && result.CommentFocusLine != result.FocusLine {
			fmt.Fprintf(out, " focus=%d→%d", result.CommentFocusLine, result.FocusLine)
		} else {
			fmt.Fprintf(out, " focus=%d", result.FocusLine)
		}
	}
	// A section entry may carry no source at all — the covering test degrades to a locator when its
	// byte allowance cannot hold one line of body. Printing the empty string as if it were source
	// costs two blank lines and reads as a truncation bug.
	if result.Snippet == "" {
		fmt.Fprintf(out, " signals=%s\n", strings.Join(result.Signals, ","))
		writeTextSearchPassages(out, result)
		return
	}
	fmt.Fprintf(out, " signals=%s\n%s\n", strings.Join(result.Signals, ","), result.Snippet)
	// A forced unit the safety cap clipped says which of its own lines are missing, on its OWN line
	// after the body. Never interleaved: agents copy body text verbatim as an Edit anchor (see the
	// focus= comment above), so a marker inside the source turns a navigation aid into a broken patch.
	if note := sem.SearchUnitElisionNote(start, end, result.UnitStartLine, result.UnitEndLine); note != "" {
		fmt.Fprintf(out, "%s\n", note)
	}
	writeTextSearchPassages(out, result)
	fmt.Fprintln(out)
}

func writeTextSearchPassages(out interface{ Write([]byte) (int, error) }, result sem.SearchResult) {
	for _, passage := range result.Passages {
		fmt.Fprintf(out, "additional %s:%d-%d focus=%d\n%s\n",
			result.FilePath, passage.StartLine, passage.EndLine, passage.FocusLine, passage.Snippet)
	}
}

// searchResultPrintedRange is the range of the source that follows on the next
// lines — the SNIPPET's range, not the ranked region's.
//
// Those differ: a matched region can start a line or two above the definition
// (a blank line, a doc comment) while the snippet is snapped to the symbol's own
// bounds. Printing the region above the symbol's source made one symbol look
// like it had two different definition lines within one session — `:779-848`
// here against `:781` from a relation answer — with nothing saying which was
// which. The header now describes exactly the lines printed under it, so both
// verbs report the symbol's own span, and the number costs nothing extra.
func searchResultPrintedRange(result sem.SearchResult) (int, int) {
	if result.SnippetStartLine > 0 && result.SnippetEndLine >= result.SnippetStartLine {
		return result.SnippetStartLine, result.SnippetEndLine
	}
	return result.StartLine, result.EndLine
}

// partitionSearchSections splits a payload into its presentation groups while preserving rank
// order inside each. Results carrying an unknown section label are treated as primary: a
// renderer that silently hid a group it did not recognize would lose recall on upgrade.
func partitionSearchSections(results []sem.SearchResult) (primary, related, docs, tests []sem.SearchResult) {
	for _, result := range results {
		switch result.Section {
		case sem.SearchSectionRelated:
			related = append(related, result)
		case sem.SearchSectionDocs:
			docs = append(docs, result)
		case sem.SearchSectionCoveringTest:
			tests = append(tests, result)
		default:
			primary = append(primary, result)
		}
	}
	return primary, related, docs, tests
}

// writeTextSearchTypeCard renders the declaration card, one line per entry. A binding entry —
// a name the hit's own body creates — also prints where else that body uses it, because the
// coupling is the whole reason it is listed.
func writeTextSearchTypeCard(out interface{ Write([]byte) (int, error) }, card []sem.TypeCardEntry) {
	if len(card) == 0 {
		return
	}
	fmt.Fprintf(out, "%s\n", searchTextTypesHeader)
	for _, entry := range card {
		// One entry per line, and Decl is a declaration read verbatim off a single
		// line of the scanned file — so all three fields are one-line values. See
		// searchResultOnOneLine for why a newline here is forgery, not layout.
		fmt.Fprintf(out, "  %s %s:%d", termsafe.Line(entry.Name), termsafe.Line(entry.FilePath), entry.Line)
		if len(entry.UseLines) > 0 {
			fmt.Fprintf(out, " (used %s)", joinSearchUseLines(entry.UseLines))
		}
		fmt.Fprintf(out, "  %s\n", termsafe.Line(entry.Decl))
	}
}

// joinSearchRanks renders the pre-merge ranks a contiguous span absorbed. They are the ranking's
// own numbers from BEFORE the merge renumbered it, which is what makes a rank that no longer
// prints on its own account for itself instead of just going missing.
func joinSearchRanks(ranks []int) string {
	parts := make([]string, 0, len(ranks))
	for _, rank := range ranks {
		parts = append(parts, strconv.Itoa(rank))
	}
	return strings.Join(parts, ",")
}

func joinSearchUseLines(lines []int) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, strconv.Itoa(line))
	}
	return strings.Join(parts, ",")
}

func searchResultDisplayName(result sem.SearchResult) string {
	if result.QualifiedName != "" {
		return result.QualifiedName
	}
	return result.SymbolName
}

func searchResultLocatorLine(result sem.SearchResult) int {
	if result.FocusLine > 0 {
		return result.FocusLine
	}
	return result.StartLine
}

// The test is "did the allocator deliberately widen this result", not "is it a whole callable", and
// the three signals below are the three ways it can have done so. Two of them were being dropped:
//
//   - full-unit: --full-unit-top may reach a rank below the full-snippet tier, and a clipped forced
//     unit carries full-unit WITHOUT complete-symbol.
//   - head-window: the allocator's fallback for a head rank with no enclosable callable. Measured on
//     fmtlib__fmt-2457 the allocator seated a 60-line window (1,709 B) at rank 3 and this function
//     then reported "no body", so the renderer printed `include/fmt/ranges.h:682` and threw the whole
//     window away — bytes spent, budget charged, nothing delivered.
//
// Both only ever occur when a flag asked for them, so the default payload is unchanged.
// searchResultForcedByFlag reports whether the CALLER asked for this rank's source by name, which is
// what exempts it from the body diet. A flag that silently stops producing what it promises is worse
// than no flag.
func searchResultForcedByFlag(result sem.SearchResult) bool {
	for _, signal := range result.Signals {
		if signal == sem.FullUnitSignal || signal == sem.CalleeHopSignal {
			return true
		}
	}
	return false
}

func searchResultIsCalleeHop(result sem.SearchResult) bool {
	for _, signal := range result.Signals {
		if signal == sem.CalleeHopSignal {
			return true
		}
	}
	return false
}

func searchResultCarriesCompleteBody(result sem.SearchResult) bool {
	for _, signal := range result.Signals {
		switch signal {
		case sem.CompleteSymbolSignal, sem.FullUnitSignal, sem.HeadWindowSignal, sem.CalleeHopSignal:
			return true
		}
	}
	return false
}

// orderAgentSearchResults groups a payload for `--format agent`: candidate fix sites, then the
// related-site block, then docs and fixtures. The agent format is a hard byte cap that fits
// results one block at a time, so a group HEADER is the one thing it cannot afford — the label
// rides on each block's own location line instead (see agentSearchSectionTag), and grouping is
// expressed by order. Docs and fixtures sort last, so under a tight cap the entries dropped
// first are the ones that were never fix sites.
func orderAgentSearchResults(results []sem.SearchResult) []sem.SearchResult {
	primary, related, docs, tests := partitionSearchSections(results)
	if len(related) == 0 && len(docs) == 0 && len(tests) == 0 {
		return results
	}
	ordered := make([]sem.SearchResult, 0, len(results))
	ordered = append(ordered, primary...)
	// The covering test sorts ahead of the related and docs groups but behind every candidate
	// fix site: under a tight cap what is dropped first must still be the entries that were
	// never fix sites, and the test is the one non-fix-site entry that changes the edit.
	ordered = append(ordered, tests...)
	ordered = append(ordered, related...)
	return append(ordered, docs...)
}

// agentSearchSectionTag labels a block that is not a candidate fix site. Primary results carry
// no tag: the common case must cost zero bytes.
func agentSearchSectionTag(result sem.SearchResult) string {
	switch result.Section {
	case sem.SearchSectionRelated:
		if kind := sem.RelatedSiteKind(result); kind != "" {
			return "(" + kind + ")"
		}
		return "(related)"
	case sem.SearchSectionDocs:
		return "(docs)"
	case sem.SearchSectionCoveringTest:
		return "(covers)"
	}
	return ""
}

// agentSearchPrefixHead pairs the two outermost prefix blocks so the fitter can walk them as one
// flat list. See the comment at its only construction site in writeAgentSearch for the ordering
// the pairing encodes.
type agentSearchPrefixHead struct {
	notice []byte
	header []byte
}

func writeAgentSearch(out interface{ Write([]byte) (int, error) }, response sem.SearchResponse, budget int) error {
	// Same sink class as the text renderer, and the format agents are told to
	// prefer — so it gets the same guard. See writeTextSearch.
	out = termsafe.NewWriter(out)
	results := orderAgentSearchResults(response.Results)
	stats := response.Stats
	cacheState := "miss"
	if stats.IndexCacheHit {
		cacheState = "hit"
	}
	fullHeader := []byte(fmt.Sprintf(
		"Index: cache-%s (%dms) | Query: %dms | Preselect: %dms | Total: %dms\n",
		cacheState,
		stats.IndexLatencyMS,
		stats.QueryLatencyMS,
		stats.PreselectLatencyMS,
		stats.TotalLatencyMS,
	))
	// EVERY block below is measured against the caller's byte cap, so every block
	// is escaped BEFORE it is measured. The terminal-safety rewrite turns one ESC
	// into four printed bytes and one C1 byte into six, so fitting the raw bytes
	// and escaping them on the way out let a snippet carrying control bytes hand
	// an agent several times the output it asked for — --max-context-bytes is a
	// hard ceiling, not an estimate. Escaping is idempotent (its output is
	// printable ASCII plus the layout bytes it keeps) and distributes over the
	// concatenations below, so the writer wrap stays as defence in depth and the
	// arithmetic here is now the arithmetic of what is actually written.
	fullDiagnostics, compactDiagnostics := agentSearchSafeBlockPair(agentSearchDiagnostics(response))
	fullConfidence, compactConfidence := agentSearchSafeBlockPair(searchLowConfidenceNotices(response))
	fullMap := termsafe.Bytes(sem.RenderSearchContainerMap(response.ContainerMap, false))
	compactMap := termsafe.Bytes(sem.RenderSearchContainerMap(response.ContainerMap, true))
	closedSet := termsafe.Bytes(sem.RenderSearchClosedSet(response.ClosedSet))
	// Suffix blocks in priority order. VERIFY comes first because it is the one an agent acts on
	// immediately; the literal cluster next because it can end the search; the declaration card last
	// because it is pure reference and is off by default anyway.
	// VERIFY is NOT in the droppable suffix set. Everything else here names something the agent could
	// go and get; the verify command is the one block whose absence is measured to change behaviour
	// (sessions without one spent 15.2% fewer tokens than the baseline against 30.6% with one), and it
	// is two lines. It is prepended to the RANKING instead, where the byte fitter cannot silently drop
	// it — see fitAgentSearchSuffixes for why the rest stay surplus.
	verifyBlock := termsafe.Bytes(sem.RenderSearchVerifyCommand(response.VerifyCommand))
	// VERIFY is undroppable in every budget that can afford it, and the LAST thing tried before the
	// caller would get no ranked location at all. Those two rules are both load-bearing and they can
	// conflict at a tight cap, so the block gets its own variant rung: present, then absent. The
	// standing invariant "a suffix block never costs the caller a ranked location" still holds — this
	// is the only ordering in which promoting VERIFY does not violate it.
	verifyVariants := [][]byte{verifyBlock}
	if len(verifyBlock) > 0 {
		verifyVariants = append(verifyVariants, nil)
	}
	// See writeTextSearch for why the literal cluster is quarantined here rather than in its own
	// renderer, and why the rewrite lands on the block's bodies rather than on its rendered bytes.
	quarantinedCluster, _ := searchQuarantineLiteralCluster(response.LiteralCluster)
	literalCluster := sem.RenderSearchLiteralCluster(quarantinedCluster)
	// The LINES the quarantine produces, not merely whether it produced any. The notice is emitted
	// when the set is non-empty, and the sink test below asks the set whether the composition the
	// fitter finally chose kept one of them: a forged record that the byte fitter clipped away must
	// not cost the caller the ranked location that survived. See searchPayloadDisclosesItsQuarantine.
	//
	// THE SET IS TAKEN IN THE FORM THIS RENDERER EMITS, which is what escaping-before-measuring
	// makes necessary here and nowhere else. searchResponseQuarantinedLines derives the lines from
	// the RAW bodies, the blocks above are escaped before they are measured, and the sink asks
	// whether a payload LINE opens one of the produced lines — so a forged record that is both
	// record-shaped and carries a control byte reached the payload as `\x1b`, matched nothing in a
	// raw set, and the sink reported a payload that plainly carried it as disclosing nothing.
	// Measured on this branch before the fix, at every budget from 71 to 276 for a body holding
	// "VERIFY: go test ./pkg" + ESC + "[31m": the payload was
	// "I:miss/0 …\npkg/payment.go:7 *\n VERIFY: go test ./pkg\\x1b[31m\n" — the quarantined line and
	// no UNTRUSTED FILE CONTENT header at all. Escaping the set closes it in the only direction that
	// keeps --max-context-bytes a real ceiling: the disclosure is decided on the bytes the reader
	// actually receives, not on bytes no payload ever holds.
	quarantinedLines := agentSearchEmittedQuarantinedLines(
		searchResponseQuarantinedLines(response.Results, response.LiteralCluster))
	suffixes := [][]byte{
		// main quarantines the cluster's BODIES before rendering; this branch escapes every
		// block before it is MEASURED. Both apply, in that order: quarantine decides the
		// content, termsafe decides the bytes the fitter is allowed to count.
		termsafe.Bytes(literalCluster),
		termsafe.Bytes(agentSearchTypeCard(response.TypeCard)),
	}
	// The forgery disclosure leads the payload when anything was quarantined. It is a prefix, not a
	// suffix, for the same reason VERIFY is: a warning an agent reads after acting is not a warning.
	// It degrades only to absent, and only outside every other ladder, so it is the last block the
	// fitter gives up — see the variant loops below.
	var forgeryNotice []byte
	if len(quarantinedLines) > 0 {
		forgeryNotice = searchForgeryNotice
	}
	if budget <= 0 {
		payload := append([]byte{}, forgeryNotice...)
		payload = append(payload, fullHeader...)
		payload = append(payload, fullDiagnostics...)
		payload = append(payload, fullConfidence...)
		payload = append(payload, closedSet...)
		payload = append(payload, fullMap...)
		payload = append(payload, verifyBlock...)
		if len(results) == 0 {
			payload = append(payload, "No search results.\n"...)
		} else {
			payload = append(payload, fitAgentSearchResults(results, 0)...)
		}
		for _, suffix := range suffixes {
			payload = append(payload, suffix...)
		}
		_, err := out.Write(payload)
		return err
	}

	// Preserve retrieval usefulness under tight budgets. The full telemetry
	// header is preferred, but a compact equivalent leaves room for the
	// top-ranked location at budgets where the expanded diagnostic otherwise
	// crowds out every result.
	compactHeader := []byte(fmt.Sprintf(
		"I:%s/%d Q:%d P:%d T:%d\n",
		cacheState,
		stats.IndexLatencyMS,
		stats.QueryLatencyMS,
		stats.PreselectLatencyMS,
		stats.TotalLatencyMS,
	))
	// Two further rungs between the compact header and the legacy one, shedding the latency
	// fields in order of least diagnostic value (preselect, then query, then total) while
	// keeping the "I:<state>/<index-ms>" shape every consumer keys off. Without these the only
	// way to buy bytes back for the ranking was the legacy "Index: cache-miss (113ms)" form,
	// which changes the prefix — so a slow machine had to choose between losing the snippet and
	// losing the machine-readable header. It now loses neither.
	timedHeader := []byte(fmt.Sprintf("I:%s/%d T:%d\n", cacheState, stats.IndexLatencyMS, stats.TotalLatencyMS))
	terseHeader := []byte(fmt.Sprintf("I:%s/%d\n", cacheState, stats.IndexLatencyMS))
	legacyHeader := []byte(fmt.Sprintf("Index: cache-%s (%dms)\n", cacheState, stats.IndexLatencyMS))
	diagnosticVariants := [][]byte{fullDiagnostics}
	if !bytes.Equal(fullDiagnostics, compactDiagnostics) {
		diagnosticVariants = append(diagnosticVariants, compactDiagnostics)
	}
	// The low-confidence marker degrades like the coverage diagnostic and, when the budget
	// cannot hold even its compact form, is dropped rather than displacing the ranking: a
	// caller that got no results at all does not need to be told they are weak.
	confidenceVariants := [][]byte{fullConfidence}
	if !bytes.Equal(fullConfidence, compactConfidence) {
		confidenceVariants = append(confidenceVariants, compactConfidence)
	}
	if len(fullConfidence) > 0 {
		confidenceVariants = append(confidenceVariants, nil)
	}
	// The container map degrades like the diagnostics: full, then names-and-ranges only, then
	// dropped. It is tried BEFORE the ranking shrinks because a map plus the top hit answers
	// "what do I read" better than one more ranked block does — but it is never the reason a
	// caller gets no result at all, hence the final nil variant.
	mapVariants := [][]byte{fullMap}
	if !bytes.Equal(fullMap, compactMap) {
		mapVariants = append(mapVariants, compactMap)
	}
	if len(fullMap) > 0 {
		mapVariants = append(mapVariants, nil)
	}
	// The closed-set warning degrades to absent last of the prefix blocks and only when the cap
	// cannot hold it at all: it is the only prefix block whose absence can make a patch wrong.
	closedSetVariants := [][]byte{closedSet}
	if len(closedSet) > 0 {
		closedSetVariants = append(closedSetVariants, nil)
	}
	// TELEMETRY PRECISION MUST NOT COST RETRIEVAL CONTENT. The header carries measured
	// latencies, so its WIDTH depends on how fast this machine is: "I:miss/21 Q:12 P:25 T:59"
	// is 25 bytes on a warm laptop and "I:miss/113 Q:60 P:112 T:287" is 28 on a loaded CI
	// runner. Because the header ladder is the outermost loop, those 3 bytes used to be taken
	// out of the ranking instead of out of the diagnostic — at --max-context-bytes 64 the top
	// hit's snippet line ("def target():", 14 bytes) survived on the fast machine and vanished
	// on the slow one. Same repo, same query, same budget, less useful answer because the box
	// was busy. That is a real defect for an agent, and it is what made
	// TestSearchCommandAgentFormatKeepsTopLocationUnderTightBudget fail on windows-latest only.
	//
	// So make one pass that refuses to accept a plan unless it can hold the top hit's COMPLETE
	// block, degrading the header to buy it. Only if no header variant can does the second pass
	// fall back to the old best-effort behaviour, so a caller never loses a result outright.
	// The bar is "the head carries SOURCE", tested STRUCTURALLY.
	//
	// The first attempt derived a byte threshold by rendering the top hit at a one-byte budget
	// and requiring the real plan to beat it. That probe returns EMPTY (nothing fits in one
	// byte), so the threshold was 0, the `== 0` guard below skipped the whole pass, and only the
	// fallback ever ran — the protection was dead code. Confirmed by instrumentation:
	// `FIT protect=false header=26 diag=14 remaining=20 formatted=9 locOnly=0`.
	//
	// A rendered block is location-only when every line is a `N. path:line …` header, so ask
	// that question of the bytes instead of guessing a size.
	// The forgery notice and the header ladder are FLATTENED into one list of prefix heads rather
	// than nested, because the ladder is already six loops deep and a seventh would reindent all of
	// it for one block. The pairing order is what the nesting would have expressed: the notice
	// varies SLOWEST, so every header rung is tried with the notice present before any rung is
	// tried without it, which makes the notice the last prefix block the fitter gives up. A rung
	// without it is offered only when there is a notice at all, so an honest payload adds none.
	headers := [][]byte{fullHeader, compactHeader, timedHeader, terseHeader, legacyHeader}
	heads := make([]agentSearchPrefixHead, 0, 2*len(headers))
	for _, header := range headers {
		heads = append(heads, agentSearchPrefixHead{notice: forgeryNotice, header: header})
	}
	if len(forgeryNotice) > 0 {
		for _, header := range headers {
			heads = append(heads, agentSearchPrefixHead{header: header})
		}
	}
	for _, protectTopHit := range []bool{true, false} {
		if protectTopHit && len(results) == 0 {
			continue // nothing to protect; the fallback pass is the only pass
		}
		for _, head := range heads {
			forgery, header := head.notice, head.header
			for _, diagnostics := range diagnosticVariants {
				for _, confidence := range confidenceVariants {
					for _, warning := range closedSetVariants {
						for _, containerMap := range mapVariants {
							for _, verify := range verifyVariants {
								remaining := budget - len(forgery) - len(header) - len(diagnostics) - len(confidence) -
									len(warning) - len(containerMap) - len(verify)
								if remaining <= 0 {
									continue
								}
								prefix := func() []byte {
									payload := append([]byte{}, forgery...)
									payload = append(payload, header...)
									payload = append(payload, diagnostics...)
									payload = append(payload, confidence...)
									payload = append(payload, warning...)
									payload = append(payload, containerMap...)
									// VERIFY rides in the PREFIX, not the droppable suffixes: it is the one
									// block whose absence measurably changes behaviour, and two lines of it
									// must never be traded for one more ranked locator.
									return append(payload, verify...)
								}
								// The blocks sit on opposite sides of the ranking and degrade
								// differently, which is why they compose without a further nested
								// variant loop: every prefix block has its own full/compact/absent
								// ladder above, while a suffix block rides along only when the
								// fitted payload leaves room for it (fitAgentSearchSuffixes).
								// Neither can cost the caller a ranked location.
								if len(results) == 0 {
									noResults := []byte("No search results.\n")
									if len(noResults) <= remaining {
										payload := fitAgentSearchSuffixes(
											append(prefix(), noResults...), agentVerifyFirstSuffixes(verify, verifyBlock, suffixes), budget,
										)
										// A result-less plan can still carry quarantined source: the literal
										// cluster is a suffix and its renderer quarantines it. The produced
										// lines are passed in because the bytes alone cannot tell a line this
										// renderer indented from one the file already held indented.
										if !searchPayloadDisclosesItsQuarantine(string(payload), quarantinedLines) {
											continue
										}
										_, err := out.Write(payload)
										return err
									}
									continue
								}
								formatted := fitAgentSearchResults(results, remaining)
								if protectTopHit && !agentSearchBlockCarriesSource(formatted) {
									// This prefix is too wide to leave room for the top hit's complete block.
									// Degrade the prefix and try again rather than handing back a truncated
									// answer: the caller can afford a shorter latency line, not a missing snippet.
									continue
								}
								if len(formatted) > 0 {
									payload := fitAgentSearchSuffixes(
										append(prefix(), formatted...), agentVerifyFirstSuffixes(verify, verifyBlock, suffixes), budget,
									)
									// THE NOTICE IS NOT DROPPABLE WHILE THE INDENT IT EXPLAINS SURVIVES.
									// The notice-free rungs above exist so a tight cap can still buy a
									// ranked location; they are legitimate only for a plan whose FITTED
									// bytes hold no quarantined line. Rejecting the plan here rather than
									// deleting those rungs keeps the ranking at the caps where the fitter
									// clipped the forged line away, and costs the ladder nothing it was
									// entitled to: the next rung down is tried immediately.
									//
									// The test asks for the LINES this response quarantined, because a
									// one-space-indented record shape in the bytes is equally what an
									// honest file looks like when it holds one. Asked only for a shape, it
									// rejected every rung of a notice-free ladder over an honest indented
									// line and lost the caller its whole payload — both when nothing was
									// quarantined at all and when the forged result was clipped away and
									// an honest one survived.
									if !searchPayloadDisclosesItsQuarantine(string(payload), quarantinedLines) {
										continue
									}
									_, err := out.Write(payload)
									return err
								}
							}
						}
					}
				}
			}
		}
	}

	// Some positive budgets cannot hold even a single ranked location. Preserve
	// a degraded-coverage marker ahead of telemetry when one is required, and
	// never exceed the caller's exact byte cap.
	payload := legacyHeader
	if len(compactDiagnostics) > 0 {
		marker := "!N"
		if len(response.PartialFailures) > 0 {
			marker = "!D"
		}
		// The exclusion count degrades WITH the marker rather than being dropped
		// alongside it. This rung synthesizes its own text, so building it from the
		// marker alone silently discarded the X suffix agentSearchDiagnostics had
		// already decided was not droppable telemetry — a payload too small for a
		// ranked location would go back to implying it saw the whole repository. The
		// rungs are ordered so the count survives wherever it fits at all: the
		// shorter form that carries it beats the longer one that does not.
		excluded := ""
		if response.Stats.RepoIgnoredFiles > 0 {
			excluded = fmt.Sprintf(" X%d", response.Stats.RepoIgnoredFiles)
		}
		candidates := [][]byte{
			[]byte(fmt.Sprintf("Index: cache-%s%s%s\n", cacheState, marker, excluded)),
			[]byte(fmt.Sprintf("%s I:%s%s\n", marker, cacheState, excluded)),
		}
		if excluded != "" {
			// The count outlives the cache state, not the other way round. Below the
			// bytes the pair needs, every remaining rung named the cache state and
			// dropped the count — so the tightest budgets, which cannot hold a ranked
			// location either, went back to implying the answer saw the whole
			// repository. A stale-index marker is telemetry the caller can re-derive
			// by asking again; a corpus the repository narrowed is not. This rung is
			// only ever reached when the fuller line does not fit, so no budget that
			// can afford both is made to choose.
			candidates = append(candidates, []byte(fmt.Sprintf("%s%s\n", marker, excluded)))
		}
		candidates = append(candidates,
			[]byte(fmt.Sprintf("Index: cache-%s%s\n", cacheState, marker)),
			[]byte(fmt.Sprintf("%s I:%s\n", marker, cacheState)),
		)
		for _, candidate := range candidates {
			payload = candidate
			if len(candidate) <= budget {
				break
			}
		}
	}
	if len(payload) > budget {
		payload = payload[:budget]
	}
	_, err := out.Write(payload)
	return err
}

// searchLowConfidenceNotices renders the low-confidence marker in a full and a compact
// form. Both are empty when the payload is not weak, so a good query pays nothing.
//
// The wording states what to DO, because the failure it exists to prevent is an agent
// reading a plausible-looking hit as the fix site in a repo that never contained the thing
// it asked about.
func searchLowConfidenceNotices(response sem.SearchResponse) ([]byte, []byte) {
	assessment := sem.AssessSearchConfidence(response)
	if !assessment.Low {
		return nil, nil
	}
	// Gate the "below" suffix on the score AS DISPLAYED, not the raw value: 11.96 renders
	// as 12.0, and "top score 12.0 (weak, below 12)" is the contradiction this notice must
	// never contain. Parsing the rendered string back keeps the two in lockstep even where
	// %.1f's half-to-even rounding and math.Round disagree.
	display := fmt.Sprintf("%.1f", assessment.TopScore)
	score := "top score " + display
	shown, err := strconv.ParseFloat(display, 64)
	if ceiling := sem.LowConfidenceScoreCeiling(); err == nil && shown < ceiling {
		score += fmt.Sprintf(" (weak, below %.0f)", ceiling)
	}
	full := []byte(fmt.Sprintf(
		"LOW CONFIDENCE: %s and %s. This repo may not contain what you asked for; verify before editing.\n",
		score, assessment.Reason,
	))
	compact := []byte(fmt.Sprintf("!LOW s=%.1f\n", assessment.TopScore))
	return full, compact
}

// fitAgentSearchSuffixes appends the suffix blocks that still fit after the ranking has been fitted,
// in the caller's priority order, and skips the ones that do not.
//
// That order is the point. The ranking is what an agent cannot reconstruct — a location it never
// sees is a file it never opens — while every suffix block names something it could go and get. So
// the suffixes are surplus in this format: they ride along when the cap is roomy and are silently
// absent when the cap is tight, and they never cost the ranking a single location.
//
// A block that does not fit does not stop the ones after it: the blocks are independent, and a long
// literal cluster must not suppress a two-line verify command that would have fitted.
// agentVerifyFirstSuffixes re-offers the verify block as the HIGHEST-priority suffix when it had to be
// dropped from the prefix to fit the ranking.
//
// Without this the priority inverts at exactly one cap: VERIFY yields to the ranking (correctly), and
// then the leftover bytes go to a droppable suffix that VERIFY outranks. Re-offering it first means the
// only thing that can ever displace VERIFY is a ranked location.
func agentVerifyFirstSuffixes(seated, verifyBlock []byte, suffixes [][]byte) [][]byte {
	if len(seated) > 0 || len(verifyBlock) == 0 {
		return suffixes
	}
	return append([][]byte{verifyBlock}, suffixes...)
}

func fitAgentSearchSuffixes(payload []byte, suffixes [][]byte, budget int) []byte {
	if budget <= 0 {
		return payload
	}
	for _, suffix := range suffixes {
		if len(suffix) == 0 || len(payload)+len(suffix) > budget {
			continue
		}
		payload = append(payload, suffix...)
	}
	return payload
}

// agentSearchTypeCard renders the declaration card for `--format agent`: one line per entry,
// `D:` tagged so it cannot be mistaken for a ranked location. There is no compact variant — the
// card is already the compact form of a file read, and there is nothing left to abbreviate that
// would not make it unusable. See fitAgentSearchTypeCard for when it is dropped instead.
func agentSearchTypeCard(card []sem.TypeCardEntry) []byte {
	if len(card) == 0 {
		return nil
	}
	var output bytes.Buffer
	for _, entry := range card {
		fmt.Fprintf(&output, "D: %s %s:%d", termsafe.Line(entry.Name), termsafe.Line(entry.FilePath), entry.Line)
		if len(entry.UseLines) > 0 {
			fmt.Fprintf(&output, " used=%s", joinSearchUseLines(entry.UseLines))
		}
		fmt.Fprintf(&output, " | %s\n", termsafe.Line(entry.Decl))
	}
	return output.Bytes()
}

func agentSearchDiagnostics(response sem.SearchResponse) ([]byte, []byte) {
	if len(response.Warnings) == 0 && len(response.PartialFailures) == 0 &&
		response.Stats.RepoIgnoredFiles == 0 {
		return nil, nil
	}
	languages, files := searchCompletenessCounts(response.Completeness)
	level := "notice"
	if len(response.PartialFailures) > 0 {
		level = "degraded"
	}
	var full bytes.Buffer
	// The excluded count sits inside the file count's own parentheses on purpose.
	// This line is the payload's claim about how much of the repository the answer
	// saw; printing "2 files" beside a silently removed third is what let one
	// committed ignore line narrow a reader's field of view unannounced.
	fmt.Fprintf(&full, "Coverage: %s (%d language%s/%d file%s%s; %d warning%s; %d partial failure%s)\n",
		level, languages, pluralSuffix(languages), files, pluralSuffix(files),
		agentCoverageExcluded(response.Stats.RepoIgnoredFiles),
		len(response.Warnings), pluralSuffix(len(response.Warnings)),
		len(response.PartialFailures), pluralSuffix(len(response.PartialFailures)),
	)
	const maxAgentDiagnostics = 3
	// The exclusion disclosure is hoisted into a visible slot before the cap is
	// applied. It is the only warning whose whole value is the path it names, and
	// the omitted-diagnostics line replaces that path with a count — "something is
	// hidden" instead of "internal/auth/auth.go is hidden". Producers put it first
	// already; hoisting here means no future producer ordering can lose it.
	warnings := hoistRepoIgnoreDisclosure(response.Warnings)
	// The same argument on the failure side. The exclusion SHORTFALL is what turns
	// the coverage line's file count — and the compact form's X<n> — from a fact
	// into a lower bound, and producers append it after whatever parser failures
	// the run collected. Three unparseable files were enough to push it past the
	// cap, leaving a payload that reported a narrowed corpus as if it were whole.
	failures := hoistRepoIgnoreShortfall(response.PartialFailures)
	warningsVisible, failuresVisible := agentDiagnosticVisibility(
		len(warnings), len(failures), repoIgnoreShortfallCount(failures), maxAgentDiagnostics,
	)
	for _, warning := range warnings[:warningsVisible] {
		fmt.Fprintf(&full, "- warning %s%s\n", warning.Code, agentDiagnosticPath(warning.FilePath))
	}
	for _, failure := range failures[:failuresVisible] {
		fmt.Fprintf(&full, "- partial %s%s\n", failure.Code, agentDiagnosticPath(failure.FilePath))
	}
	visible := warningsVisible + failuresVisible
	if omitted := len(response.Warnings) + len(response.PartialFailures) - visible; omitted > 0 {
		fmt.Fprintf(&full, "- ... %d more diagnostic%s in JSON output\n", omitted, pluralSuffix(omitted))
	}
	marker := "N"
	if level == "degraded" {
		marker = "D"
	}
	excluded := ""
	if response.Stats.RepoIgnoredFiles > 0 {
		// X is not droppable telemetry: the compact form exists for tight budgets,
		// and a budget too small for the count is not a reason to let the payload
		// go back to implying it saw the whole repository.
		excluded = fmt.Sprintf(" X%d", response.Stats.RepoIgnoredFiles)
	}
	compact := []byte(fmt.Sprintf("!%s W%d F%d L%d/%d%s\n",
		marker, len(response.Warnings), len(response.PartialFailures), languages, files, excluded))
	return full.Bytes(), compact
}

// repoIgnoreDisclosureWarning is the code of the warning that names a file the
// repository's own ignore rules removed from the corpus.
const repoIgnoreDisclosureWarning = "W_REPO_IGNORED_SOURCE"

// hoistRepoIgnoreDisclosure returns warnings with the exclusion disclosure moved
// to the front, leaving every other warning in its original order. The input is
// never mutated.
func hoistRepoIgnoreDisclosure(warnings []sem.ProviderWarning) []sem.ProviderWarning {
	index := -1
	for i, warning := range warnings {
		if warning.Code == repoIgnoreDisclosureWarning {
			index = i
			break
		}
	}
	if index <= 0 {
		return warnings
	}
	hoisted := make([]sem.ProviderWarning, 0, len(warnings))
	hoisted = append(hoisted, warnings[index])
	hoisted = append(hoisted, warnings[:index]...)
	return append(hoisted, warnings[index+1:]...)
}

// repoIgnoreShortfallPrefix is the code prefix every partial failure that
// qualifies the exclusion accounting shares (E_REPO_IGNORE_UNREADABLE,
// E_REPO_IGNORE_COUNT_INCOMPLETE, E_REPO_IGNORE_GIT_UNAVAILABLE).
//
// A prefix rather than a list on purpose: the producer and this renderer are in
// different packages, and a list here would have to be edited every time the
// producer learns a new way for the count to fall short. A prefix makes the next
// one visible by construction instead of silently capped away.
const repoIgnoreShortfallPrefix = "E_REPO_IGNORE_"

// repoIgnoreShortfallCount reports how many failures qualify the exclusion
// accounting (see repoIgnoreShortfallPrefix). Shared by hoistRepoIgnoreShortfall
// and agentDiagnosticVisibility so the two agree on exactly what counts as one
// of these failures.
func repoIgnoreShortfallCount(failures []sem.PartialFailure) int {
	count := 0
	for _, failure := range failures {
		if strings.HasPrefix(failure.Code, repoIgnoreShortfallPrefix) {
			count++
		}
	}
	return count
}

// hoistRepoIgnoreShortfall returns failures with every exclusion-shortfall
// record moved to the front, each group keeping its original relative order.
// The input is never mutated.
func hoistRepoIgnoreShortfall(failures []sem.PartialFailure) []sem.PartialFailure {
	shortfalls := repoIgnoreShortfallCount(failures)
	if shortfalls == 0 || shortfalls == len(failures) {
		return failures
	}
	hoisted := make([]sem.PartialFailure, 0, len(failures))
	for _, failure := range failures {
		if strings.HasPrefix(failure.Code, repoIgnoreShortfallPrefix) {
			hoisted = append(hoisted, failure)
		}
	}
	for _, failure := range failures {
		if !strings.HasPrefix(failure.Code, repoIgnoreShortfallPrefix) {
			hoisted = append(hoisted, failure)
		}
	}
	return hoisted
}

// agentCoverageExcluded renders the repository-controlled exclusion count inside
// the coverage line, or nothing when the repository excluded nothing.
func agentCoverageExcluded(excluded int) string {
	if excluded <= 0 {
		return ""
	}
	return fmt.Sprintf(", %d excluded by repo ignore rules", excluded)
}

// agentDiagnosticVisibility splits maxAgentDiagnostics slots between warnings
// and failures. requiredFailures is a floor failuresVisible must reach
// whenever there are at least that many failures: the repo-ignore shortfall
// failures a single run can emit together (E_REPO_IGNORE_GIT_UNAVAILABLE,
// E_REPO_IGNORE_COUNT_INCOMPLETE, ...) each qualify the coverage line's X<n>
// count in a way the others do not stand in for, so showing only one of two
// leaves that count looking exact when it is actually a lower bound. Callers
// with no such floor pass 0; at least one failure of any kind is still
// guaranteed visible whenever one exists, as before this parameter existed.
func agentDiagnosticVisibility(warnings, failures, requiredFailures, limit int) (int, int) {
	if limit <= 0 {
		return 0, 0
	}
	if failures > 0 && requiredFailures < 1 {
		requiredFailures = 1
	}
	requiredFailures = minIntCLI(requiredFailures, minIntCLI(failures, limit))
	warningsVisible := minIntCLI(warnings, limit-requiredFailures)
	failuresVisible := minIntCLI(failures, limit-warningsVisible)
	if failuresVisible < requiredFailures {
		failuresVisible = requiredFailures
		warningsVisible = minIntCLI(warnings, limit-failuresVisible)
	}
	return warningsVisible, failuresVisible
}

func searchCompletenessCounts(report sem.CompletenessReport) (int, int) {
	files := 0
	for _, language := range report.Languages {
		files += language.Files
	}
	return len(report.Languages), files
}

// agentDiagnosticPath renders the path a warning or partial failure names. The
// file is unparseable BY DEFINITION here, so its name is the most attacker-shaped
// value in the payload — and these records print one per line, ahead of the
// ranking, which is where a forged line would do the most damage.
func agentDiagnosticPath(path string) string {
	if path == "" {
		return ""
	}
	return ": " + termsafe.Line(path)
}

// agentSearchBlockCarriesSource reports whether a rendered ranked block contains any source,
// as opposed to being nothing but locator lines.
//
// The shape matters and cost two wrong attempts. The AGENT format's locator is `path:line *`,
// with no `N. ` rank prefix (that is the TEXT format), so a prefix test classified the locator
// itself as source, the protection accepted a locator-only plan, and it stayed dead. A locator
// line is therefore recognised by its structure: a first token of `<path>:<digits>`.
func agentSearchBlockCarriesSource(block []byte) bool {
	for _, line := range bytes.Split(block, []byte("\n")) {
		trimmed := bytes.TrimRight(line, " \t")
		if len(bytes.TrimSpace(trimmed)) == 0 {
			continue
		}
		if !agentSearchLineIsLocator(trimmed) {
			return true
		}
	}
	return false
}

// agentSearchLineIsLocator matches `<path>:<digits>` at the head of a line, where <path> has no
// whitespace. Source lines fail it: `def target():` has a space before its colon, and an indented
// body line does not start with a path at all.
func agentSearchLineIsLocator(line []byte) bool {
	first := line
	if i := bytes.IndexAny(line, " \t"); i >= 0 {
		first = line[:i]
	}
	// leading whitespace means an indented body line, never a locator
	if len(first) == 0 {
		return false
	}
	colon := bytes.LastIndexByte(first, ':')
	if colon <= 0 || colon == len(first)-1 {
		return false
	}
	for _, c := range first[colon+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// fitAgentSearchResults returns the widest ranked block that fits the byte
// budget. The rendered block is escaped before it is measured: it is the block
// that carries repository source, so it is the block whose printed size differs
// most from its raw size, and measuring the raw form was how a snippet of ESC
// bytes overran the cap.
//
// When the escaped form overruns, the SIZE of the render is what has to come
// down, not only the NUMBER of results. Dropping results alone leaves nothing
// to drop for a single hit, so one snippet of control bytes took the whole
// ranked block to nil, every prefix variant in writeAgentSearchPayload failed
// with it, and the payload fell through to the telemetry tail: a larger budget
// bought a strictly worse answer, and the agent got no location at all. The
// locator itself always fits — searchResultOnOneLine escapes the path and name
// before they are ever measured, so the header cannot expand — which is exactly
// why re-rendering smaller reaches an answer where dropping results cannot.
func fitAgentSearchResults(results []sem.SearchResult, budget int) []byte {
	if budget <= 0 {
		return termsafe.Bytes(renderAgentSearchResults(results, nil))
	}
	for count := len(results); count > 0; count-- {
		// The separators between blocks are the caller's bytes too, so
		// `available` is what the RESULTS themselves may spend.
		separators := count - 1
		for available := budget - separators; available > 0; {
			rendered := renderAgentSearchResults(results[:count], rankedAgentSearchBudgets(count, available))
			if len(rendered) == 0 {
				break // nothing renderable this narrow; drop a result instead
			}
			formatted := termsafe.Bytes(rendered)
			if len(formatted) <= budget {
				return formatted
			}
			// Retry at the width the OBSERVED expansion implies, scaled from
			// what this attempt actually PRODUCED rather than from what it was
			// allowed to: a block that came in under its budget would otherwise
			// be re-rendered unchanged forever. len(formatted) exceeds budget
			// here, so the scaled width is strictly smaller than the produced
			// one and the loop always terminates — while staying proportional
			// keeps it from collapsing past the widths that would have fitted,
			// since no byte escapes to more than six (termsafe.appendEscapedAt).
			// int64 because the product of two caller-supplied byte counts
			// overflows a 32-bit int.
			next := int(int64(len(rendered))*int64(budget)/int64(len(formatted))) - separators
			if next >= available {
				next = available - 1 // belt and braces: never fail to make progress
			}
			available = next
		}
	}
	return nil
}

// agentSearchSafeBlockPair escapes a renderer's full/compact pair in one step,
// so both variants of a block are measured in the form they are printed in.
func agentSearchSafeBlockPair(full, compact []byte) ([]byte, []byte) {
	return termsafe.Bytes(full), termsafe.Bytes(compact)
}

// agentSearchEmittedQuarantinedLines restates the quarantine's produced lines in the form this
// renderer emits them, so the disclosure sink compares like with like.
//
// searchPayloadDisclosesItsQuarantine asks whether a FINISHED payload line opens a line the
// quarantine produced, and it is exact by design: the produced set is what separates a line this
// response rewrote from one an honest file already held indented. Exactness only holds while both
// sides are the same bytes. This renderer escapes every block before it measures it, so a produced
// line carrying a control byte arrives in the payload as its escaped spelling and is not the raw
// line any more; the lookup then missed, the sink answered "nothing to disclose", and a notice-free
// rung carrying the quarantined line was accepted. Escaping the set makes the sink ask about the
// bytes the reader receives.
//
// It is a no-op on every line that carries no control byte — the overwhelming case and every line
// the existing forgery suite is built from — because termsafe.Bytes returns its input unchanged
// there. The sort is redone because escaping reorders: `\x1b` sorts where a backslash sorts, not
// where an ESC does, and searchProducedLineOpensWith binary-searches this slice.
func agentSearchEmittedQuarantinedLines(produced []string) []string {
	emitted := make([]string, 0, len(produced))
	for _, line := range produced {
		emitted = append(emitted, agentSearchEmittedLine(line))
	}
	slices.Sort(emitted)
	return slices.Compact(emitted)
}

// agentSearchEmittedLine escapes one line the way the block that carries it is escaped.
//
// The line is escaped WITH ITS NEWLINE and the newline is then dropped, because termsafe reads one
// byte past a CR to decide it: a CR followed by LF is a Windows line ending and is kept, a lone CR
// is an overwrite and is escaped. A body line ending in CR is followed by its LF in the block, so
// escaping the line on its own would rewrite a CR the payload keeps raw and produce a set entry no
// payload line can match — the same miss this function exists to remove, one byte further along.
func agentSearchEmittedLine(line string) string {
	escaped := termsafe.Bytes([]byte(line + "\n"))
	return string(escaped[:len(escaped)-1])
}

func rankedAgentSearchBudgets(count, budget int) []int {
	if count <= 0 {
		return nil
	}
	budgets := make([]int, count)
	minimum := minIntCLI(128, budget/count)
	remaining := budget - minimum*count
	weightTotal := count * (count + 1) / 2
	allocated := 0
	for index := range budgets {
		extra := 0
		if weightTotal > 0 {
			extra = remaining * (count - index) / weightTotal
		}
		budgets[index] = minimum + extra
		allocated += extra
	}
	// Integer division can leave a few bytes. They are most valuable at the
	// top of the ranking, where agents make their first navigation decision.
	for index := 0; allocated < remaining; index = (index + 1) % count {
		budgets[index]++
		allocated++
	}
	return budgets
}

func renderAgentSearchResults(results []sem.SearchResult, budgets []int) []byte {
	var output bytes.Buffer
	wrote := false
	for index, result := range results {
		budget := 0
		if index < len(budgets) {
			budget = budgets[index]
		}
		block := agentSearchBlock(result, budget)
		if len(block) == 0 {
			return nil
		}
		if wrote {
			output.WriteByte('\n')
		}
		output.Write(block)
		wrote = true
	}
	return output.Bytes()
}

// agentSearchScoreTag renders the ` s=<n>` suffix for a block, or "" when the score carries
// no meaning. Related sites are not ranked answers — they are the other places the top hit's
// change usually has to land — and they carry no relevance score, so printing `s=0.0` beside
// one would read as "worthless" rather than "not applicable".
func agentSearchScoreTag(result sem.SearchResult) string {
	if result.Section == sem.SearchSectionRelated || result.Section == sem.SearchSectionCoveringTest {
		return ""
	}
	return fmt.Sprintf(" s=%.1f", result.Score)
}

func agentSearchBlock(result sem.SearchResult, budget int) []byte {
	// Every location header this block and its helpers emit is one line. See
	// searchResultOnOneLine.
	result = searchResultOnOneLine(result)
	if len(result.Passages) == 0 {
		return agentSearchPrimaryBlock(result, budget)
	}
	for count := len(result.Passages); count >= 0; count-- {
		passages := renderAgentSearchPassages(result.FilePath, result.Passages[:count])
		primaryBudget := budget
		if budget > 0 && len(passages) > 0 {
			primaryBudget -= len(passages) + 1
			if primaryBudget <= 0 {
				continue
			}
		}
		primary := agentSearchPrimaryBlock(result, primaryBudget)
		if len(primary) == 0 {
			continue
		}
		if len(passages) == 0 {
			return primary
		}
		combined := make([]byte, 0, len(primary)+1+len(passages))
		combined = append(combined, primary...)
		combined = append(combined, '\n')
		combined = append(combined, passages...)
		if budget <= 0 || len(combined) <= budget {
			return combined
		}
	}
	return nil
}

func renderAgentSearchPassages(path string, passages []sem.SearchPassage) []byte {
	var output bytes.Buffer
	for index, passage := range passages {
		if index > 0 {
			output.WriteByte('\n')
		}
		fmt.Fprintf(&output, "%s:%d-%d [additional focus:%d]\n%s",
			path, passage.StartLine, passage.EndLine, passage.FocusLine, passage.Snippet)
	}
	return output.Bytes()
}

func agentSearchPrimaryBlock(result sem.SearchResult, budget int) []byte {
	name := searchResultDisplayName(result)
	tag := agentSearchSectionTag(result)
	scored := agentSearchScoreTag(result)
	lines := strings.Split(result.Snippet, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	snippetStart := result.SnippetStartLine
	if snippetStart <= 0 {
		snippetStart = result.StartLine
	}
	if snippetStart <= 0 {
		snippetStart = 1
	}
	focusLine := result.FocusLine
	if focusLine <= 0 {
		focusLine = snippetStart
	}
	focus := focusLine - snippetStart
	if focus < 0 || focus >= len(lines) {
		focus = len(lines) / 2
		focusLine = snippetStart + focus
	}
	if len(lines) == 0 {
		return fitAgentSearchLocation(result.Rank, result.FilePath, focusLine, name, tag, scored, budget)
	}

	// Prefer the widest balanced span containing the focus line. The location
	// in the header is rebuilt for each candidate, so it always describes the
	// lines actually displayed rather than the original untrimmed region.
	for span := len(lines); span > 0; span-- {
		leftMin := focus - span + 1
		if leftMin < 0 {
			leftMin = 0
		}
		leftMax := focus
		if limit := len(lines) - span; leftMax > limit {
			leftMax = limit
		}
		bestBalance := len(lines) + 1
		var best []byte
		for left := leftMin; left <= leftMax; left++ {
			right := left + span - 1
			text := strings.Join(lines[left:right+1], "\n")
			startLine, endLine := snippetStart+left, snippetStart+right
			for _, header := range agentSearchLocationHeaders(result.Rank, result.FilePath, startLine, endLine, focusLine, name, tag, scored) {
				candidate := []byte(header + text + "\n")
				if budget <= 0 || len(candidate) <= budget {
					balance := focus - left - (right - focus)
					if balance < 0 {
						balance = -balance
					}
					if best == nil || balance < bestBalance {
						best, bestBalance = candidate, balance
					}
					break
				}
			}
		}
		if best != nil {
			return best
		}
	}
	return fitAgentSearchLocation(result.Rank, result.FilePath, focusLine, name, tag, scored, budget)
}

// agentSearchLocationHeaders builds the location line in decreasing cost. `tag` labels a block
// that is not a candidate fix site; it rides on the two roomier variants and is dropped by the
// minimal one, which exists for caps too small to hold a rank and a name at all.
//
// `scored` is the pre-rendered relevance-score suffix (see agentSearchScoreTag). It is NOT
// an optional decoration: the score is the only per-result signal that separates a real hit
// from the best of a bad lot, and its absence from this format let a query about a technology
// absent from a repo come back as six confident-looking hits. It rides on the two roomier
// variants at ~7 bytes each and is dropped by the minimal one along with rank and name.
func agentSearchLocationHeaders(rank int, path string, start, end, focus int, name, tag, scored string) []string {
	location := fmt.Sprintf("%d. %s:%d", rank, path, start)
	if end != start {
		location += fmt.Sprintf("-%d", end)
	}
	rich := location
	if name != "" {
		rich += " " + name
	}
	if tag != "" {
		rich += " " + tag
	}
	rich += scored + fmt.Sprintf(" [focus:%d]\n", focus)
	compact := location
	if name != "" {
		compact += " " + name
	}
	if tag != "" {
		compact += " " + tag
	}
	compact += scored + " *\n"
	minimal := fmt.Sprintf("%s:%d *\n", path, focus)
	return []string{rich, compact, minimal}
}

func fitAgentSearchLocation(rank int, path string, focus int, name, tag, scored string, budget int) []byte {
	for _, header := range agentSearchLocationHeaders(rank, path, focus, focus, focus, name, tag, scored) {
		if budget <= 0 || len(header) <= budget {
			return []byte(header)
		}
	}
	return nil
}

func minIntCLI(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func parseSearchFlags(args []string) (searchFlags, []string, error) {
	// Profile `fast` (upstream default: call-graph-lite ranking is good enough to be the
	// default, see the fast-profile ranking-ambiguity fix) with the measured 24 KiB context
	// budget (defaultSearchContextBytes), which exists so the ceiling is never the reason a
	// head result comes back as half a function.
	flags := searchFlags{Format: "json", Profile: "fast", Worktree: true, MaxContextBytes: defaultSearchContextBytes}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.Repo, i = value, next
		case "--query":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.Query, i = value, next
		case "--format":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.Format, i = value, next
		case "--profile":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.Profile, i = value, next
		case "--top-k":
			value, next, err := searchPositiveIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.TopK, i = value, next
		case "--context-lines":
			value, next, err := searchNonNegativeIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.ContextLines, i = value, next
		case "--max-region-lines":
			value, next, err := searchPositiveIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.MaxRegionLines, i = value, next
		case "--max-snippet-lines":
			value, next, err := searchPositiveIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.MaxSnippetLines, i = value, next
		// --body-head-ranks N: 0 is the DEFINED "built-in depth" value (see
		// SearchOptions.BodyHeadRanks), so it parses like the flag above.
		case "--body-head-ranks":
			value, next, err := searchNonNegativeIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.BodyHeadRanks, i = value, next
		case "--file-outline":
			flags.FileOutline = true
		case "--head-window-lines":
			value, next, err := searchPositiveIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.HeadWindowLines, i = value, next
		// --enclosure-context-lines N: 0 is the DEFINED "no padding" value (see
		// SearchOptions.EnclosureContextLines), so it is parsed as a non-negative
		// integer. Rejecting it made a configuration that serializes the default
		// explicitly fail before searching.
		case "--enclosure-context-lines":
			value, next, err := searchNonNegativeIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.EnclosureContextLines, i = value, next
		// --full-unit-top N: render the first N ranks as their complete enclosing unit whatever the
		// opportunistic body upgrade would have done. 0 (the default) is today's payload exactly.
		// See SearchOptions.FullUnitTop and the editability comment in internal/sem/search_enclosure.go.
		case "--full-unit-top":
			value, next, err := searchNonNegativeIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.FullUnitTop, i = value, next
		// --edit-site-bodies: give the SAME-CONCEPT LITERAL block's EDIT sites their source.
		case "--edit-site-bodies":
			flags.EditSiteBodies = true
		// --callee-hop: admit the top hit's outgoing CALLS targets as candidate fix sites. See
		// internal/sem/search_callee.go for the measured miss it closes and why the related-sites
		// block cannot.
		case "--callee-hop":
			flags.CalleeHop = true
		// --verify-prefix <token>: a decorator baked into the emitted VERIFY command, after any
		// `cd <dir> &&` the derivation added, so a harness can grep its own token out of a session
		// transcript. "VERIFY: " stays byte-identical at line start.
		case "--verify-prefix":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.VerifyPrefix, i = value, next
		// --verify-prefix-status <text>: one caller-computed line rendered verbatim under VERIFY:. The
		// harness validates the pristine tree; the binary must not invent a status for a run it never made.
		case "--verify-prefix-status":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.VerifyPreFixStatus, i = value, next
		case "--max-regions-per-file":
			value, next, err := searchPositiveIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.MaxRegionsPerFile, i = value, next
		case "--ignore-file":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.IgnoreFiles, i = append(flags.IgnoreFiles, value), next
		case "--include-file":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.IncludeFiles, i = append(flags.IncludeFiles, value), next
		case "--cache-dir":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.CacheDir, i = value, next
		case "--no-cache":
			flags.DisableCache = true
		case "--max-indexed-files":
			value, next, err := searchPositiveIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.MaxIndexedFiles, i = value, next
		case "--index-all-files":
			flags.IndexAllFiles = true
		case "--deep":
			flags.Deep = true
		case "--single-resolution":
			flags.SingleResolution = true
		case "--document-resolution":
			flags.DocumentResolution = true
		case "--container-map":
			flags.ContainerMap = true
		case "--signature-types":
			flags.SignatureTypes = true
		case "--type-card":
			flags.TypeCard = true
		case "--reference-blocks":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			if err := applySearchReferenceBlocks(&flags, value); err != nil {
				return flags, nil, err
			}
			i = next
		case "--verify-explain":
			value, next, err := searchFlagValue(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.VerifyExplain, i = value, next
		case "--max-context-bytes":
			value, next, err := searchNonNegativeIntFlag(args, i)
			if err != nil {
				return flags, nil, err
			}
			flags.MaxContextBytes, i = value, next
		case "--worktree", "--no-network":
			if args[i] == "--worktree" {
				flags.Worktree = true
			}
		case "--head":
			flags.Worktree = false
		default:
			rest = append(rest, args[i])
		}
	}
	return flags, rest, nil
}

func searchFlagValue(args []string, index int) (string, int, error) {
	if index+1 >= len(args) {
		return "", index, fmt.Errorf("%s requires a value", args[index])
	}
	return args[index+1], index + 1, nil
}

func searchPositiveIntFlag(args []string, index int) (int, int, error) {
	value, next, err := searchNonNegativeIntFlag(args, index)
	if err != nil {
		return 0, index, err
	}
	if value == 0 {
		return 0, index, fmt.Errorf("%s must be positive", args[index])
	}
	return value, next, nil
}

func searchNonNegativeIntFlag(args []string, index int) (int, int, error) {
	raw, next, err := searchFlagValue(args, index)
	if err != nil {
		return 0, index, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, index, fmt.Errorf("%s requires a non-negative integer", args[index])
	}
	return value, next, nil
}
