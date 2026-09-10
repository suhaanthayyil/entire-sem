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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/termsafe"
)

type Options struct {
	Version string
	Env     EntireEnv
	Stdout  io.Writer
	Stderr  io.Writer
	// Stdin is where `explain` reads a failing build's output from. It is the only command that takes
	// piped input, because it is the only one whose question ("what are these names the build is
	// complaining about") is asked by composing with another command rather than by naming a symbol.
	Stdin io.Reader
}

func Execute(version string, args []string) error {
	return Run(context.Background(), Options{
		Version: version,
		Env:     EnvFromOS(),
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Stdin:   os.Stdin,
	}, args)
}

func Run(ctx context.Context, opts Options, args []string) error {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	if len(args) == 0 {
		printHelp(opts.Stdout)
		return nil
	}

	// Per-command help: `entire graph <cmd> --help` prints that command's detail
	// view and exits, without touching each runX. Only fires for commands that
	// have a doc entry — which includes `help` itself, so `help --help` prints
	// help's own detail view rather than the root listing. A bare `--help` has
	// no command word and is handled above.
	if doc, ok := findCommandDoc(args[0]); ok && wantsHelp(doc, args[1:]) {
		renderCommandHelp(opts.Stdout, args[0])
		return nil
	}

	switch args[0] {
	case "diff":
		return runDiff(ctx, opts, args[1:])
	case "commit":
		return runCommit(ctx, opts, args[1:])
	case "checkpoint":
		return runCheckpoint(ctx, opts, args[1:])
	case "analyze":
		return runAnalyze(ctx, opts, args[1:])
	case "doctor":
		return runDoctor(ctx, opts, args[1:])
	case "capabilities":
		return runCapabilities(opts, args[1:])
	case "snapshot":
		return runProviderRecords(ctx, opts, args[1:], "snapshot")
	case "snapshot-query":
		return runSnapshotQuery(opts, args[1:])
	case "symbols":
		return runProviderRecords(ctx, opts, args[1:], "symbols")
	case "edges":
		return runProviderRecords(ctx, opts, args[1:], "edges")
	case "search":
		return runSearch(ctx, opts, args[1:])
	case "index":
		return runIndex(ctx, opts, args[1:])
	case "def":
		return runDef(ctx, opts, args[1:])
	case "explain":
		return runExplain(ctx, opts, args[1:])
	case "neighbors":
		return runNeighbors(ctx, opts, args[1:])
	case "impact":
		return runImpact(ctx, opts, args[1:])
	case "risk":
		return runRisk(ctx, opts, args[1:])
	case "verify":
		return runVerify(ctx, opts, args[1:])
	case "stats":
		return runStats(ctx, opts, args[1:])
	case "agent-guide":
		return runAgentGuide(opts, args[1:])
	case "init-agents":
		return runInitAgents(opts, args[1:])
	case "version", "--version", "-v":
		if len(args) > 1 && args[1] == "--json" {
			return json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout)).Encode(map[string]string{
				"provider":          sem.ProviderName,
				"version":           opts.Version,
				"identity_revision": sem.IdentityRevision,
			})
		}
		fmt.Fprintln(opts.Stdout, opts.Version)
		return nil
	case "help", "--help", "-h":
		printHelp(opts.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run \"entire graph help\" to list commands", args[0])
	}
}

// printHelp renders the grouped root listing. Per-command detail lives in
// renderCommandHelp; both read from the commandDocs registry in help.go.
func printHelp(out io.Writer) {
	renderRootHelp(out)
}

func runDoctor(ctx context.Context, opts Options, args []string) error {
	asJSON := false
	var asserts []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--json":
			asJSON = true
		case "--assert":
			// Repeatable: a harness usually drives more than one verb, and finding out about the
			// second one only after the first has been fixed costs another whole run.
			if index+1 >= len(args) {
				return errors.New("doctor --assert needs a command line, for example --assert \"search --profile full\"")
			}
			index++
			asserts = append(asserts, args[index])
		default:
			return errors.New("doctor accepts only --json and --assert \"<command line>\"")
		}
	}
	// The assertions run FIRST and stop the report: a caller that asked whether this binary can
	// serve its command line wants that answer, and printing an otherwise-healthy environment
	// report above the failure buries it.
	for _, spec := range asserts {
		if err := checkPreflight(opts.Version, spec); err != nil {
			return err
		}
	}
	if len(asserts) > 0 && !asJSON {
		for _, spec := range asserts {
			fmt.Fprintf(opts.Stdout, "assert_ok=%q\n", spec)
		}
	}
	report := map[string]any{
		"provider":  sem.ProviderName,
		"version":   opts.Version,
		"no_egress": true,
		"environment": map[string]string{
			envCLIVersion:    valueOrUnset(opts.Env.CLIVersion),
			envRepoRoot:      valueOrUnset(opts.Env.RepoRoot),
			envPluginDataDir: valueOrUnset(opts.Env.PluginDataDir),
		},
		"phase_1_local_only": map[string]bool{
			"fetch_remote_code":              false,
			"download_grammars_or_assets":    false,
			"upload_telemetry":               false,
			"call_hosted_model_apis":         false,
			"call_remote_embedding_provider": false,
			"perform_network_discovery":      false,
		},
	}
	if len(asserts) > 0 {
		// Reaching here means every assertion parsed, so the JSON says so explicitly rather than
		// leaving a caller to infer success from the absence of an error.
		report["asserted_command_lines"] = asserts
	}
	if !asJSON {
		fmt.Fprintf(opts.Stdout, "ENTIRE_CLI_VERSION=%s\n", valueOrUnset(opts.Env.CLIVersion))
		fmt.Fprintf(opts.Stdout, "ENTIRE_REPO_ROOT=%s\n", valueOrUnset(opts.Env.RepoRoot))
		fmt.Fprintf(opts.Stdout, "ENTIRE_PLUGIN_DATA_DIR=%s\n", valueOrUnset(opts.Env.PluginDataDir))
		fmt.Fprintln(opts.Stdout, "no_egress=true")
	}

	if opts.Env.PluginDataDir != "" {
		if err := os.MkdirAll(opts.Env.PluginDataDir, 0o700); err != nil {
			return fmt.Errorf("create plugin data dir: %w", err)
		}
		probe, err := os.CreateTemp(opts.Env.PluginDataDir, ".write-test-*")
		if err != nil {
			return fmt.Errorf("write plugin data dir: %w", err)
		}
		probeName := probe.Name()
		if err := probe.Close(); err != nil {
			return fmt.Errorf("close plugin data probe: %w", err)
		}
		if err := os.Remove(probeName); err != nil {
			return fmt.Errorf("remove plugin data probe: %w", err)
		}
		report["plugin_data_dir"] = "writable"
		if !asJSON {
			fmt.Fprintln(opts.Stdout, "plugin_data_dir=writable")
		}
	}

	repo, err := resolveRepo(ctx, opts.Env, "")
	if err == nil {
		// Snapshot construction converts the resolved repository to an absolute,
		// cleaned path before deriving either RepoRoot or RepoKey. Doctor is the
		// preflight for that snapshot, so advertise the same spelling rather than
		// the caller's possibly-relative ENTIRE_REPO_ROOT value.
		repo, err = filepath.Abs(repo)
	}
	if err != nil {
		report["repo_root"] = ""
		report["repo_error"] = err.Error()
		if asJSON {
			return json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout)).Encode(report)
		}
		fmt.Fprintf(opts.Stdout, "repo_root=%s\n", valueOrUnset(""))
		fmt.Fprintf(opts.Stdout, "repo_error=%s\n", err)
		return nil
	}
	report["repo_root"] = repo
	// The repo_key and schema_version this binary WILL use are part of the
	// doctor report so a consumer can verify the seam contract up front. Brain
	// runs doctor before every snapshot; discovering a repo_key rule mismatch or
	// an unsupported schema major here costs milliseconds, whereas discovering
	// it from the finished snapshot costs the whole (up to 30 minute) run.
	report["repo_key"] = sem.RepoKey(ctx, repo)
	report["schema_version"] = sem.SchemaVersion
	if asJSON {
		return json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout)).Encode(report)
	}
	fmt.Fprintf(opts.Stdout, "repo_root=%s\n", repo)
	return nil
}

func runCapabilities(opts Options, args []string) error {
	if len(args) != 1 || args[0] != "--json" {
		return errors.New("capabilities requires --json")
	}
	return json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout)).Encode(sem.Capabilities())
}

func runProviderRecords(ctx context.Context, opts Options, args []string, mode string) error {
	flags, rest, err := parseProviderFlags(args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return unexpectedArgumentsError(mode, opts.Version, rest)
	}
	if mode != "snapshot" && mode != "symbols" && mode != "edges" {
		return fmt.Errorf("unknown provider record mode %q", mode)
	}
	filterActive := flags.To != "" || flags.From != "" || len(flags.Relation) > 0
	compact := flags.Format == "compact-ndjson"
	scip := flags.Format == "scip"
	if flags.Format != "ndjson" && !compact && !scip {
		if mode == "snapshot" {
			return fmt.Errorf("%s requires --format ndjson, compact-ndjson, or scip", mode)
		}
		return fmt.Errorf("%s requires --format ndjson", mode)
	}
	if (compact || scip) && mode != "snapshot" {
		return fmt.Errorf("--format %s is only valid for snapshot", flags.Format)
	}
	if (compact || scip) && filterActive {
		return fmt.Errorf("--format %s requires a complete snapshot; remove --to/--from/--relation", flags.Format)
	}
	if scip && flags.Progress {
		return errors.New("--format scip cannot be combined with --progress; stderr is reserved for the JSON omission note")
	}
	repo, err := resolveRepo(ctx, opts.Env, flags.Repo)
	if err != nil {
		return err
	}
	profile, err := parseProfile(flags.Profile)
	if err != nil {
		return err
	}
	options := sem.ProviderSnapshotOptions{
		NoNetwork:    flags.NoNetwork,
		Worktree:     flags.Worktree,
		IgnoreFiles:  flags.IgnoreFiles,
		IncludeFiles: flags.IncludeFiles,
		Profile:      profile,
	}
	if flags.Progress {
		options.Progress = func(event sem.ProgressEvent) {
			fmt.Fprintf(opts.Stderr, "graph progress phase=%s files=%d/%d symbols=%d relations=%d heap=%d rss=%d elapsed=%s\n",
				event.Phase,
				event.FilesDone,
				event.FilesTotal,
				event.Symbols,
				event.Relations,
				event.HeapAlloc,
				event.MaxRSSBytes,
				event.Elapsed.Round(time.Millisecond),
			)
		}
	}
	// Native and compact encoders stream records directly. SCIP must collect the
	// complete graph before writing its single protobuf Index message.
	var scipEncoder *sem.SCIPSnapshotEncoder
	newRecordEncoder := func(out io.Writer) func(any) error {
		if scip {
			// NOT wrapped. `--format scip` writes a binary protobuf Index, and the C1
			// rewrite is defined over a TEXT stream: it would rewrite any 0xc2 0x8X
			// pair the wire format happens to contain -- a varint, a length prefix, a
			// UTF-8 name -- into six ASCII bytes, and the result no longer parses as
			// an Index at all. A hostile pathname in this stream reaches a terminal
			// only after a consumer has decoded the protobuf and chosen to render it,
			// which is that consumer's escape to apply, not this encoder's.
			scipEncoder = sem.NewSCIPSnapshotEncoder(out, "")
			return scipEncoder.Encode
		}
		// Wrapped once here rather than in either branch below: both encoders write
		// repository-controlled pathnames and entity names, and a snapshot carries no
		// source text, so a hostile PATHNAME is the only C1 these streams can hold.
		out = termsafe.NewJSONWriter(out)
		if compact {
			return sem.NewCompactSnapshotEncoder(out).Encode
		}
		encoder := json.NewEncoder(out)
		encoder.SetEscapeHTML(false) // match json.Marshal used elsewhere (no < escaping)
		return encoder.Encode
	}
	encodeRecord := newRecordEncoder(opts.Stdout)
	if scipEncoder != nil {
		// The version is read inside the snapshot build, through the reader that
		// is already validated, bounded and pinned to this snapshot's revision.
		// Reading it here instead would run Git before the metadata preflight and
		// touch the filesystem without the provider's guards.
		options.ProjectVersion = scipEncoder.SetProjectVersion
	}

	// Targeted edge query: when --to/--from/--relation is set, emit only matching
	// relations (plus header/summary), never files/symbols. Turns "callers of X"
	// into a tiny reply instead of dumping the whole graph for the caller to grep.
	// Streaming-safe: idMatches keys off the stable ID's trailing name segment, so
	// no in-memory symbol table is needed. Only meaningful in edges/snapshot modes.
	// Capture the summary as it streams past so we can loudly warn on a partial
	// parse. Without this the CLI discards the summary and a run that silently
	// parsed only a fraction of the repo (e.g. a mis-scoped subdir) looks clean.
	var summary *sem.SnapshotSummary
	var snapshotHeader sem.SnapshotHeader
	capture := func(record any) {
		switch r := record.(type) {
		case sem.SnapshotHeader:
			snapshotHeader = r
		case sem.SnapshotSummary:
			s := r
			summary = &s
		}
	}

	if filterActive && mode == "symbols" {
		return fmt.Errorf("--to/--from/--relation filter relations; use `edges` (not `symbols`)")
	}
	if filterActive {
		var matched int
		if err := sem.StreamSnapshot(ctx, repo, opts.Version, options, func(record any) error {
			capture(record)
			switch r := record.(type) {
			case sem.RelationRecord:
				if !relationMatches(r, flags) {
					return nil
				}
				matched++
				return encodeRecord(r)
			case sem.FileRecord, sem.ExternalRecord, sem.SymbolRecord:
				return nil // suppressed for a targeted edge query
			default: // header, summary
				return encodeRecord(record)
			}
		}); err != nil {
			return err
		}
		warnIfPartial(opts.Stderr, flags.Worktree, summary)
		fmt.Fprintf(opts.Stderr, "graph: %d edge(s) matched (--to=%q --from=%q --relation=%s)\n",
			matched, flags.To, flags.From, strings.Join(flags.Relation, ","))
		return nil
	}

	// Whole-graph dump (no targeted filter): serve from the committed-record
	// cache when possible. The cache is keyed on the exact HEAD commit and tree,
	// the mode, and output-affecting options, so an unchanged HEAD skips the
	// expensive re-index. It is deliberately bypassed for --worktree and, by
	// returning above, for targeted queries.
	cacheDir := resolveCacheDir(flags.CacheDir, opts.Env.PluginDataDir)
	useCache := !flags.DisableCache && !flags.Worktree && !scip && cacheDir != ""
	var commit, tree string
	cacheContext := ctx
	if useCache {
		var validationErr error
		cacheContext, validationErr = sem.WithGitMetadataValidationForSetup(ctx, repo)
		if validationErr != nil {
			// Cache probing is optional and runs before provider construction. Unsafe
			// metadata disables the probe; the provider below selects its warned,
			// filesystem-only fallback without starting Git.
			useCache = false
		}
	}
	if useCache {
		if c, t, headErr := gitutil.HeadCommitAndTree(cacheContext, repo); headErr == nil && c != "" && t != "" {
			commit = c
			tree = t
		} else {
			useCache = false
		}
	}
	cacheMode := mode
	if compact {
		cacheMode = "snapshot:compact-ndjson-v1"
	}
	var recordsCache *sem.ProviderRecordsCacheTransaction
	if useCache {
		recordsCache, err = sem.BeginProviderRecordsCache(cacheContext, repo, opts.Version, commit, tree, cacheMode, cacheDir, options)
		if err != nil {
			// Cache setup is optional. The uncached stream below will surface any
			// policy-input error that also prevents a correct build.
			useCache = false
			recordsCache = nil
		} else {
			// Pin matcher construction to the exact policy bytes that keyed this
			// lookup, and carry the same transaction through storage below.
			options = recordsCache.Options()
		}
	}
	if recordsCache != nil {
		if records, cachedSummary, hit := recordsCache.Load(); hit {
			// Replayed bytes need the same wrap the encoder below gets. This path does
			// not go through encodeRecord at all, so a cache entry written before the
			// C1 rule existed — or by any build that predates it — would stream the
			// repository's raw control straight to the terminal on every hit. Escaping
			// is idempotent, so a clean entry passes through unchanged.
			if _, err := termsafe.NewJSONWriter(opts.Stdout).Write(records); err != nil {
				return err
			}
			warnIfPartial(opts.Stderr, flags.Worktree, cachedSummary)
			return nil
		}
	}

	// On a miss, tee the serialized record stream into a buffer so we can persist it after
	// a successful run without a second pass over the graph.
	//
	// BOUNDED, because the cache is on by default: resolveCacheDir falls back to the user cache
	// directory, so useCache is true for every ordinary committed snapshot/symbols/edges run and this
	// tee is no longer the opt-in it was written as. An unbounded tee makes a streaming command hold
	// its ENTIRE output in memory — proportional to the whole graph, on exactly the large repositories
	// the streaming contract exists for. Past the cap the buffer is released and the entry is not
	// stored: the cache is best effort, the stream is not.
	recordBuf := boundedRecordBuffer{limit: providerRecordsCacheMaxBytes}
	if recordsCache != nil {
		encodeRecord = newRecordEncoder(io.MultiWriter(opts.Stdout, &recordBuf))
	}
	if err := sem.StreamSnapshot(ctx, repo, opts.Version, options, func(record any) error {
		capture(record)
		if !includeRecord(mode, record) {
			return nil
		}
		return encodeRecord(record)
	}); err != nil {
		return err
	}
	if scipEncoder != nil {
		if err := writeSCIPOmissionNote(opts.Stderr, scipOmissionNoteWithSummary(scipEncoder.OmissionNote(), summary)); err != nil {
			return err
		}
	} else {
		warnIfPartial(opts.Stderr, flags.Worktree, summary)
	}
	if recordsCache != nil && !recordBuf.overflowed {
		// Best effort: a failed cache write never fails the command.
		_ = recordsCache.Store(recordBuf.buffer.Bytes(), summary, snapshotHeader)
	}
	return nil
}

// providerRecordsCacheMaxBytes caps the in-memory capture of a cold run's records. A var rather than
// a const so a test can shrink it to a size a fixture can actually exceed.
var providerRecordsCacheMaxBytes = 64 << 20

// boundedRecordBuffer accumulates the record stream up to a byte limit and then stops, releasing what
// it held and remembering that it did. Writes ALWAYS report success: this sits inside the
// io.MultiWriter that also feeds stdout, and a short write there would abort the stream the command
// exists to produce. Giving up on the cache entry is the correct trade; giving up on the output is
// not.
type boundedRecordBuffer struct {
	buffer     bytes.Buffer
	limit      int
	overflowed bool
}

func (b *boundedRecordBuffer) Write(records []byte) (int, error) {
	if b.overflowed {
		return len(records), nil
	}
	if b.buffer.Len()+len(records) > b.limit {
		b.overflowed = true
		// The entry will never be stored, so nothing here is worth the memory any longer.
		b.buffer = bytes.Buffer{}
		return len(records), nil
	}
	return b.buffer.Write(records)
}

func scipOmissionNoteWithSummary(note sem.SCIPOmissionNote, summary *sem.SnapshotSummary) sem.SCIPOmissionNote {
	if summary == nil {
		return note
	}
	note.WarningCount = len(summary.Warnings)
	note.PartialFailureCount = len(summary.PartialFailures)
	// The records themselves, not just how many. SCIP cannot represent a file
	// that was discovered but not parsed, so without these the note is the only
	// place that information could have survived, and it was being reduced to an
	// integer.
	note.PartialFailures = summary.PartialFailures
	// Which languages were only inventoried, so a consumer can scope trust per
	// language the way the feed contract expects.
	note.LanguageTiers = summary.LanguageTiers
	level := summary.Stats.CompletenessLevel
	if level == "" || level == "ok" {
		return note
	}
	note.PartialSnapshot = true
	note.CompletenessLevel = level
	return note
}

func writeSCIPOmissionNote(w io.Writer, note sem.SCIPOmissionNote) error {
	// The note is the one TEXT stream `--format scip` writes, and it is written to
	// a terminal. It is wrapped for the same structural reason every other machine
	// encoder here is: a sink left unwrapped is the one the next field added to
	// this record forgets about.
	encoder := json.NewEncoder(termsafe.NewJSONWriter(w))
	encoder.SetEscapeHTML(false)
	return encoder.Encode(note)
}

// warnIfPartial prints a loud stderr banner when the snapshot did not fully cover
// the repository, so a silent partial parse (the #1 sharp edge: running in a
// mis-scoped subdir without --worktree, which indexes only a stray config file
// and reports "ok") becomes impossible to miss. Silent on a clean "ok" run.
func warnIfPartial(w io.Writer, worktree bool, s *sem.SnapshotSummary) {
	if s == nil {
		return
	}
	level := s.Stats.CompletenessLevel
	if level == "" || level == "ok" {
		return
	}
	fmt.Fprintf(w, "\n⚠️  graph is %s: parsed %d/%d files, %d symbols, %d relations (languages: %s).\n",
		strings.ToUpper(level), s.Stats.ParsedFiles, s.Stats.Files, s.Stats.Symbols,
		s.Stats.Relations, strings.Join(s.Languages, ", "))
	switch {
	case s.Stats.Files <= 2 && !worktree:
		fmt.Fprintf(w, "   Only %d file(s) were discovered — you may be indexing a subdirectory or an\n"+
			"   unexpected commit. Run from the repo root, or pass --worktree to index the\n"+
			"   working tree instead of HEAD.\n", s.Stats.Files)
	case s.Stats.ParsedFiles*2 < s.Stats.Files:
		fmt.Fprintf(w, "   Over half the discovered files were not parsed (unsupported language or\n"+
			"   parse errors). Graph queries will miss code in those files.\n")
	default:
		fmt.Fprintf(w, "   The graph is incomplete; treat query results as partial.\n")
	}
}

// includeRecord filters streamed records for the symbols and edges modes, which
// emit a subset of the full snapshot.
func includeRecord(mode string, record any) bool {
	switch record.(type) {
	case sem.FileRecord, sem.ExternalRecord:
		return mode == "snapshot"
	case sem.SymbolRecord:
		return mode == "snapshot" || mode == "symbols"
	case sem.RelationRecord:
		return mode == "snapshot" || mode == "edges"
	default: // header, summary
		return true
	}
}

// defaultMaxSeconds is the wall-clock budget applied to diff/commit/analyze
// when --max-seconds is not given. Large repos with hot changed names can
// otherwise grind for many minutes and produce nothing; with the budget the
// command stops cleanly at the limit and emits the partial result plus
// machine-readable W_ANALYSIS_BUDGET_EXCEEDED warnings. --max-seconds 0
// disables the budget.
const defaultMaxSeconds = 120

type commonFlags struct {
	Repo     string
	JSON     bool
	Progress bool
	// MaxSeconds is the overall analysis budget in seconds; -1 means the flag
	// was not given (commands apply their default), 0 means unlimited.
	MaxSeconds int
}

type providerFlags struct {
	Repo         string
	Format       string
	Profile      string
	NoNetwork    bool
	Worktree     bool
	Progress     bool
	IgnoreFiles  []string
	IncludeFiles []string
	// Targeted edge filters (edges mode). When any is set the command emits only
	// the matching relation records (plus header/summary) instead of the whole
	// graph, so "callers of X" is a tiny reply rather than a 50MB dump that the
	// caller then greps client-side. --to/--from match a symbol by full stable ID
	// or by trailing name segment (IDs are `...:kind:name`); --relation is one or
	// more edge types (comma-separated, case-insensitive), e.g. CALLS,REFERENCES.
	To       string
	From     string
	Relation []string
	// CacheDir/DisableCache control the committed-record cache. Empty CacheDir
	// falls back to ENTIRE_PLUGIN_DATA_DIR; --no-cache disables it entirely.
	CacheDir     string
	DisableCache bool
}

// idMatches reports whether a stable symbol ID matches a user-supplied selector:
// either the exact ID, or a trailing name segment (IDs end `...:kind:name`, so
// `getConfPath` or `function:getConfPath` both select it). Streaming-safe — no
// symbol table needed.
func idMatches(id, sel string) bool {
	return id == sel || strings.HasSuffix(id, ":"+sel)
}

// relationMatches applies the --to/--from/--relation predicate to one edge.
func relationMatches(r sem.RelationRecord, f providerFlags) bool {
	if f.To != "" && !idMatches(r.ToID, f.To) {
		return false
	}
	if f.From != "" && !idMatches(r.FromID, f.From) {
		return false
	}
	if len(f.Relation) > 0 {
		t := strings.ToUpper(r.Type)
		hit := false
		for _, want := range f.Relation {
			if t == want {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// parseProfile validates the --profile value. Empty defaults to full.
func parseProfile(value string) (sem.Profile, error) {
	switch value {
	case "", "full":
		return sem.ProfileFull, nil
	case "fast":
		return sem.ProfileFast, nil
	case "syntax-only":
		return sem.ProfileSyntaxOnly, nil
	default:
		return "", fmt.Errorf("unknown --profile %q (want full, fast, or syntax-only)", value)
	}
}

func parseProviderFlags(args []string) (providerFlags, []string, error) {
	flags := providerFlags{Format: "ndjson"}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--repo requires a value")
			}
			flags.Repo = args[i]
		case "--format":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--format requires a value")
			}
			flags.Format = args[i]
		case "--profile":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--profile requires a value")
			}
			flags.Profile = args[i]
		case "--no-network":
			flags.NoNetwork = true
		case "--worktree":
			flags.Worktree = true
		case "--progress":
			flags.Progress = true
		case "--ignore-file":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--ignore-file requires a value")
			}
			flags.IgnoreFiles = append(flags.IgnoreFiles, args[i])
		case "--include-file":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--include-file requires a value")
			}
			flags.IncludeFiles = append(flags.IncludeFiles, args[i])
		case "--cache-dir":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--cache-dir requires a value")
			}
			flags.CacheDir = args[i]
		case "--no-cache":
			flags.DisableCache = true
		case "--to":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--to requires a value")
			}
			flags.To = args[i]
		case "--from":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--from requires a value")
			}
			flags.From = args[i]
		case "--relation":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--relation requires a value")
			}
			for _, part := range strings.Split(args[i], ",") {
				if part = strings.ToUpper(strings.TrimSpace(part)); part != "" {
					flags.Relation = append(flags.Relation, part)
				}
			}
		default:
			rest = append(rest, args[i])
		}
	}
	return flags, rest, nil
}

func parseCommonFlags(args []string) (commonFlags, []string, error) {
	flags := commonFlags{MaxSeconds: -1}
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--json":
			flags.JSON = true
		case "--progress":
			flags.Progress = true
		case "--max-seconds":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--max-seconds requires a value")
			}
			seconds, err := strconv.Atoi(args[i])
			if err != nil || seconds < 0 {
				return flags, nil, fmt.Errorf("--max-seconds requires a non-negative integer, got %q", args[i])
			}
			flags.MaxSeconds = seconds
		case "--repo":
			i++
			if i >= len(args) {
				return flags, nil, errors.New("--repo requires a value")
			}
			flags.Repo = args[i]
		case "--":
			rest = append(rest, args[i+1:]...)
			return flags, rest, nil
		default:
			rest = append(rest, arg)
		}
	}
	return flags, rest, nil
}

func runCommit(ctx context.Context, opts Options, args []string) error {
	flags, rest, err := parseCommonFlags(args)
	if err != nil {
		return err
	}
	rev := "HEAD"
	if len(rest) > 0 {
		rev = rest[0]
	}
	if len(rest) > 1 {
		return errors.New("commit accepts at most one revision")
	}
	if err := validateRevision("commit", rev); err != nil {
		return err
	}
	repo, err := resolveRepo(ctx, opts.Env, flags.Repo)
	if err != nil {
		return err
	}
	if err := sem.EnsureGitMetadataSafeForSubprocess(repo); err != nil {
		return err
	}
	base, err := gitutil.FirstParent(ctx, repo, rev)
	if err != nil {
		return err
	}
	return analyzeAndPrint(ctx, opts, repo, base, rev, nil, flags)
}

func runCheckpoint(ctx context.Context, opts Options, args []string) error {
	flags, rest, err := parseCommonFlags(args)
	if err != nil {
		return err
	}
	if flags.Progress {
		return errors.New("checkpoint does not support --progress")
	}
	if flags.MaxSeconds >= 0 {
		return errors.New("checkpoint does not support --max-seconds")
	}
	if len(rest) != 1 {
		return errors.New("checkpoint requires exactly one checkpoint ID")
	}
	repo, err := resolveRepo(ctx, opts.Env, flags.Repo)
	if err != nil {
		return err
	}
	result, err := sem.AnalyzeCheckpoint(ctx, repo, rest[0])
	if err != nil {
		return err
	}
	return printResult(opts.Stdout, result, flags.JSON, opts.Version)
}

func runAnalyze(ctx context.Context, opts Options, args []string) error {
	return runDiff(ctx, opts, args)
}

// validateRevision rejects a revision value that Git would read as an option instead.
//
// Every revision this package accepts is eventually spliced into a git argv — `git diff -z
// --raw --find-renames <base> <head> --` in gitutil.ChangedFiles, `git rev-parse
// <rev>^` in gitutil.FirstParent, `git show <rev>^{tree}:<path>` in gitutil.ShowFile. Git
// parses options anywhere ahead of `--`, so a value beginning with '-' stops being a revision
// and becomes a flag of the command it lands in: `diff --base '--output=FILE'` exited 0 having
// truncated FILE and replaced it with git's own name-status output (CWE-88), and `commit
// '--output=FILE'` reached the same argv because `git rev-parse '--output=FILE^'` does not
// fail first.
//
// Refusing at the boundary costs nothing legitimate. Values beginning with "--" are always
// Git options; bare "-" is ambiguous with git's path/option syntax. Single-hyphen refs such as
// "-foo" are valid and must not be rejected here — gitutil uses "--end-of-options" elsewhere
// when splicing user paths into git argv.
func validateRevision(name, rev string) error {
	if rev == "" {
		return fmt.Errorf("%s requires a revision", name)
	}
	if strings.HasPrefix(rev, "--") {
		return fmt.Errorf("%s %q is not a revision: a revision cannot begin with %q, which Git would read as an option", name, rev, "--")
	}
	if rev == "-" {
		return fmt.Errorf("%s %q is not a revision: bare %q is ambiguous with git's path/option syntax", name, rev, "-")
	}
	if strings.ContainsRune(rev, '\x00') {
		return fmt.Errorf("%s %q is not a revision: a revision cannot contain a NUL byte", name, rev)
	}
	return nil
}

// diffFlags is the parsed argument set for `diff` and its `analyze` alias.
type diffFlags struct {
	common commonFlags
	base   string
	head   string
	paths  []string
}

// parseDiffFlags parses `diff`/`analyze` arguments, returning any flag-shaped argument it does
// not recognize separately from the path filters, so the caller can reject it the way every
// other verb does.
//
// Separating the two is the point. The parsing used to live inline in runDiff, whose default
// branch filed EVERY unrecognized token under paths — so `diff --jsonn` exited 0 having filtered
// the diff to a path that does not exist, and printed "No semantic entity changes detected". A
// mistyped flag was answered with a confident empty result, which for a verb whose whole job is
// telling you what changed is the worst possible way to be wrong.
//
// Paths that genuinely look like flags still work: that is what the documented `-- path...`
// separator is for.
// diffValueFlags are the `diff`/`analyze` flags whose value is the NEXT argument: the two
// parseCommonFlags consumes and the two the loop below does. It exists only for the separator scan
// above, which runs before either parser and so cannot ask them.
var diffValueFlags = map[string]bool{
	"--repo":        true,
	"--max-seconds": true,
	"--base":        true,
	"--head":        true,
}

func parseDiffFlags(args []string) (diffFlags, []string, error) {
	parsed := diffFlags{base: "HEAD~1", head: "HEAD"}

	// Split on `--` here rather than letting parseCommonFlags consume it: that function flattens
	// everything after the separator into rest, which would leave a literal path named `--base`
	// indistinguishable from the real flag.
	//
	// Skipping each value-taking flag's value is what keeps `--` a separator rather than a pattern:
	// `diff --repo --` addresses a repository directory literally named `--`, and a raw scan cut the
	// arguments off in front of it and failed with "--repo requires a value" — with no `--repo=--`
	// spelling to fall back on, because these parsers accept no `=` form.
	flagArgs, literalPaths := args, []string(nil)
	for i := 0; i < len(args); i++ {
		if diffValueFlags[args[i]] {
			i++
			continue
		}
		if args[i] == "--" {
			flagArgs, literalPaths = args[:i], args[i+1:]
			break
		}
	}

	common, rest, err := parseCommonFlags(flagArgs)
	if err != nil {
		return diffFlags{}, nil, err
	}
	parsed.common = common

	var unknown []string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--base":
			i++
			if i >= len(rest) {
				return diffFlags{}, nil, errors.New("--base requires a value")
			}
			if err := validateRevision("--base", rest[i]); err != nil {
				return diffFlags{}, nil, err
			}
			parsed.base = rest[i]
		case "--head":
			i++
			if i >= len(rest) {
				return diffFlags{}, nil, errors.New("--head requires a value")
			}
			if err := validateRevision("--head", rest[i]); err != nil {
				return diffFlags{}, nil, err
			}
			parsed.head = rest[i]
		default:
			if strings.HasPrefix(rest[i], "-") && rest[i] != "-" {
				unknown = append(unknown, rest[i])
				continue
			}
			parsed.paths = append(parsed.paths, rest[i])
		}
	}
	parsed.paths = append(parsed.paths, literalPaths...)
	return parsed, unknown, nil
}

func runDiff(ctx context.Context, opts Options, args []string) error {
	parsed, unknown, err := parseDiffFlags(args)
	if err != nil {
		return err
	}
	if len(unknown) != 0 {
		return unexpectedArgumentsError("diff", opts.Version, unknown)
	}

	repo, err := resolveRepo(ctx, opts.Env, parsed.common.Repo)
	if err != nil {
		return err
	}
	return analyzeAndPrint(ctx, opts, repo, parsed.base, parsed.head, parsed.paths, parsed.common)
}

func resolveRepo(ctx context.Context, env EntireEnv, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env.RepoRoot != "" {
		return env.RepoRoot, nil
	}
	if os.Getenv("GIT_CEILING_DIRECTORIES") != "" {
		// Git canonicalizes ceiling entries before discovery. A caller-controlled
		// Windows UNC entry can therefore perform network I/O before Git looks for
		// repository metadata. Apply the boundary in-process with the provider's
		// same-volume resolver and never pass its raw path list to a subprocess.
		if root, ok := discoverImplicitCheckoutRoot("."); ok {
			return root, nil
		}
		return "", errors.New("no Git repository found below GIT_CEILING_DIRECTORIES")
	}
	if err := sem.EnsureGitMetadataSafeForSubprocess("."); err != nil {
		// Preserve the provider's warned filesystem-only fallback without asking
		// Git to discover the checkout. Analyze entry points apply their own strict
		// guard before resolving revisions.
		if root, ok := discoverImplicitCheckoutRoot("."); ok {
			return root, nil
		}
		return ".", nil
	}
	return gitutil.RepoRoot(ctx, ".")
}

// discoverImplicitCheckoutRoot applies an inherited ceiling without a Git
// subprocess. With no usable ceiling it keeps the established two-spelling
// filesystem fallback. Git canonicalizes entries before the first empty list
// element, while an empty element promises that subsequent entries contain no
// symlinks and can stay lexical. Canonicalization uses the provider's guarded
// same-volume walk, which refuses off-volume and UNC redirects before probing
// them. Unresolvable entries are discarded just as Git discards them.
func discoverImplicitCheckoutRoot(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	abs = filepath.Clean(abs)

	type ceilingEntry struct {
		path         string
		canonicalize bool
	}
	var entries []ceilingEntry
	canonicalize := true
	for _, entry := range strings.Split(os.Getenv("GIT_CEILING_DIRECTORIES"), string(os.PathListSeparator)) {
		// Git documents ceiling entries as absolute paths. The first empty
		// entry disables canonicalization for every subsequent entry.
		if entry == "" {
			canonicalize = false
			continue
		}
		absoluteEntry, absolute := sem.GitAbsolutePath(abs, entry)
		if !absolute {
			if sem.GitAbsolutePathNeedsFailClosed(entry) {
				return "", false
			}
			continue
		}
		entries = append(entries, ceilingEntry{path: filepath.Clean(absoluteEntry), canonicalize: canonicalize})
	}
	if len(entries) == 0 {
		return discoverCheckoutRoot(dir)
	}

	resolver, resolution := sem.NewGitCeilingPathResolver(abs)
	if resolution != sem.GitCeilingPathResolved {
		if resolution != sem.GitCeilingPathUnsupported {
			return "", false
		}
		// Platforms without a safe mount inventory retain the prior lexical
		// behavior only when the caller put every usable entry after the
		// empty marker and therefore promised that none needs resolution.
		var ceilings []string
		for _, entry := range entries {
			if entry.canonicalize {
				return "", false
			}
			ceilings = append(ceilings, entry.path)
		}
		return discoverImplicitCheckoutRootBelowCeilings(abs, abs, ceilings, nil)
	}
	defer resolver.Close()

	physicalAbs, resolution := resolver.Canonicalize(abs)
	if resolution != sem.GitCeilingPathResolved {
		return "", false
	}
	ceilings := make([]string, 0, len(entries))
	for _, entry := range entries {
		ceiling := entry.path
		if entry.canonicalize {
			resolved, resolution := resolver.Canonicalize(ceiling)
			switch resolution {
			case sem.GitCeilingPathResolved:
				ceiling = resolved
			case sem.GitCeilingPathUnresolvable:
				continue
			default:
				// Exact Git parity would require following the unsafe path to
				// learn whether it aliases an ancestor. Refuse implicit
				// discovery rather than risk either network I/O or walking
				// through a boundary whose canonical spelling is unknown.
				return "", false
			}
		}
		ceilings = append(ceilings, ceiling)
	}
	return discoverImplicitCheckoutRootBelowCeilings(abs, physicalAbs, ceilings, resolver)
}

func discoverImplicitCheckoutRootBelowCeilings(namedStart, physicalStart string, ceilings []string, resolver *sem.GitCeilingPathResolver) (string, bool) {
	applicable := ceilings[:0]
	for _, ceiling := range ceilings {
		rel, err := filepath.Rel(ceiling, physicalStart)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			applicable = append(applicable, ceiling)
		}
	}
	abs := physicalStart
	for {
		for _, ceiling := range applicable {
			equal := abs == ceiling
			if runtime.GOOS == "windows" {
				equal = strings.EqualFold(abs, ceiling)
			}
			if equal {
				return "", false
			}
		}
		if resolver == nil {
			if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
				return abs, true
			}
		} else {
			exists, resolution := resolver.HasGitEntry(abs)
			if resolution != sem.GitCeilingPathResolved {
				return "", false
			}
			if exists {
				return preferredCheckoutRootSpelling(resolver, namedStart, abs)
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", false
		}
		abs = parent
	}
}

// preferredCheckoutRootSpelling preserves the caller-visible spelling used by
// the filesystem fallback when it names the physical checkout Git discovery
// found. This matters on systems such as macOS where /var is an alias for
// /private/var: ceilings must be compared physically, but an unrelated ceiling
// must not silently change the checkout path returned to existing callers.
func preferredCheckoutRootSpelling(resolver *sem.GitCeilingPathResolver, namedStart, physicalRoot string) (string, bool) {
	for candidate := namedStart; ; candidate = filepath.Dir(candidate) {
		resolved, resolution := resolver.Canonicalize(candidate)
		if resolution == sem.GitCeilingPathUnsafe {
			return "", false
		}
		equal := resolved == physicalRoot
		if runtime.GOOS == "windows" {
			equal = strings.EqualFold(resolved, physicalRoot)
		}
		if resolution == sem.GitCeilingPathResolved && equal {
			return candidate, true
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return physicalRoot, true
		}
	}
}

func analyzeAndPrint(ctx context.Context, opts Options, repo, base, head string, paths []string, flags commonFlags) error {
	maxSeconds := flags.MaxSeconds
	if maxSeconds < 0 {
		maxSeconds = defaultMaxSeconds
	}
	analyzeOptions := sem.AnalyzeOptions{
		MaxDuration: time.Duration(maxSeconds) * time.Second,
	}
	if flags.Progress {
		analyzeOptions.Progress = func(event sem.AnalyzeProgressEvent) {
			fmt.Fprintln(opts.Stderr, diffProgressLine(event))
		}
	}
	result, err := sem.AnalyzeGitRangeWithOptions(ctx, repo, base, head, paths, analyzeOptions)
	if err != nil {
		return err
	}
	return printResult(opts.Stdout, result, flags.JSON, opts.Version)
}

// diffProgressLine renders one --progress event for stderr.
//
// It is a named function rather than an inline closure because of the escaping:
// event.Path is a raw Git pathname carried through from `git diff -z`, which may
// hold any byte but NUL and '/', and this is the one sink that has to escape by
// value instead of by wrapping its writer — stderr also carries the progress
// bar's own cursor control (see progressbar.go), which a wrap could not tell
// apart from an injected sequence.
func diffProgressLine(event sem.AnalyzeProgressEvent) string {
	line := fmt.Sprintf("graph diff progress phase=%s files=%d/%d elapsed=%s",
		event.Phase,
		event.FilesDone,
		event.FilesTotal,
		event.Elapsed.Round(time.Millisecond),
	)
	if event.Path != "" {
		line += " file=" + termsafe.Line(event.Path)
	}
	return line
}

func printResult(out io.Writer, result sem.Result, asJSON bool, producerVersion string) error {
	// ProducerVersion is set here, the one place a Result is rendered, rather
	// than at each of the two callers — it needs the CLI's build-time version
	// (Options.Version), which the sem package that builds the rest of the
	// Result has no access to.
	result.ProducerVersion = producerVersion
	if asJSON {
		encoded, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(termsafe.NewJSONWriter(out), string(encoded))
		return nil
	}
	sem.WriteText(out, result)
	return nil
}
