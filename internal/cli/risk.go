package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/termsafe"
)

// defaultRiskMaxEntities bounds a changeset report. The command is intended to
// make a review decision promptly, not to turn one large diff into unbounded
// repeated graph traversals.
const defaultRiskMaxEntities = 5

type riskLevel string

const (
	riskHigh   riskLevel = "high"
	riskMedium riskLevel = "medium"
	riskLow    riskLevel = "low"
)

type riskFlags struct {
	Repo        string
	Base        string
	Head        string
	Checkpoint  string
	Format      string
	Profile     string
	MaxEntities int
}

type riskEvidence struct {
	DirectCallers     int                `json:"direct_callers"`
	TransitiveCallers int                `json:"transitive_callers"`
	TypeConsumers     int                `json:"type_consumers"`
	Callees           int                `json:"callees"`
	TopCallers        []neighborEndpoint `json:"top_callers"`
}

type riskEntry struct {
	Name            string       `json:"name"`
	Kind            string       `json:"kind"`
	ChangeType      string       `json:"change_type"`
	FilePath        string       `json:"file_path,omitempty"`
	StartLine       int          `json:"start_line,omitempty"`
	DependentsCount int          `json:"dependents_count"`
	Level           riskLevel    `json:"level"`
	Evidence        riskEvidence `json:"graph_evidence"`
	Tests           []string     `json:"affected_tests"`
	Inference       string       `json:"inference"`
	Limitations     []string     `json:"limitations,omitempty"`
}

type riskReport struct {
	FormatVersion    int                   `json:"format_version"`
	Base             string                `json:"base,omitempty"`
	Head             string                `json:"head,omitempty"`
	Checkpoint       string                `json:"checkpoint,omitempty"`
	OverallRisk      riskLevel             `json:"overall_risk"`
	EntitiesChanged  int                   `json:"entities_changed"`
	EntitiesAnalyzed int                   `json:"entities_analyzed"`
	EntitiesSkipped  int                   `json:"entities_skipped,omitempty"`
	Entries          []riskEntry           `json:"entries"`
	RecommendedTests []string              `json:"recommended_tests"`
	Warnings         []sem.ProviderWarning `json:"warnings,omitempty"`
	Limitations      []string              `json:"limitations,omitempty"`
}

func parseRiskFlags(args []string) (riskFlags, error) {
	flags := riskFlags{Base: "HEAD~1", Head: "HEAD", Format: "text", Profile: "full", MaxEntities: defaultRiskMaxEntities}
	baseSet, headSet := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			return args[i], nil
		}
		switch arg {
		case "--repo":
			item, err := value()
			if err != nil {
				return flags, err
			}
			flags.Repo = item
		case "--base":
			item, err := value()
			if err != nil {
				return flags, err
			}
			if err := validateRevision("--base", item); err != nil {
				return flags, err
			}
			flags.Base, baseSet = item, true
		case "--head":
			item, err := value()
			if err != nil {
				return flags, err
			}
			if err := validateRevision("--head", item); err != nil {
				return flags, err
			}
			flags.Head, headSet = item, true
		case "--checkpoint":
			item, err := value()
			if err != nil {
				return flags, err
			}
			if strings.TrimSpace(item) == "" {
				return flags, errors.New("--checkpoint requires a value")
			}
			flags.Checkpoint = item
		case "--format":
			item, err := value()
			if err != nil {
				return flags, err
			}
			flags.Format = item
		case "--profile":
			item, err := value()
			if err != nil {
				return flags, err
			}
			flags.Profile = item
		case "--max-entities":
			item, err := value()
			if err != nil {
				return flags, err
			}
			count, parseErr := strconv.Atoi(item)
			if parseErr != nil || count <= 0 {
				return flags, fmt.Errorf("--max-entities requires a positive integer, got %q", item)
			}
			flags.MaxEntities = count
		default:
			return flags, fmt.Errorf("risk received unexpected argument %q", arg)
		}
	}
	if flags.Checkpoint != "" && (baseSet || headSet) {
		return flags, errors.New("risk --checkpoint cannot be combined with --base or --head")
	}
	if flags.Format != "text" && flags.Format != "json" {
		return flags, fmt.Errorf("risk --format must be text or json, got %q", flags.Format)
	}
	return flags, nil
}

func runRisk(ctx context.Context, opts Options, args []string) error {
	flags, err := parseRiskFlags(args)
	if err != nil {
		return err
	}
	repo, err := resolveRepo(ctx, opts.Env, flags.Repo)
	if err != nil {
		return err
	}
	profile, err := parseProfile(flags.Profile)
	if err != nil {
		return err
	}

	var diff sem.Result
	if flags.Checkpoint != "" {
		diff, err = sem.AnalyzeCheckpoint(ctx, repo, flags.Checkpoint)
	} else {
		// Match the normal diff command's default budget. The report preserves
		// the semantic diff's explicit budget warning rather than silently
		// treating a partial result as a complete changeset.
		diff, err = sem.AnalyzeGitRangeWithOptions(ctx, repo, flags.Base, flags.Head, nil, sem.AnalyzeOptions{
			MaxDuration: time.Duration(defaultMaxSeconds) * time.Second,
		})
	}
	if err != nil {
		return err
	}
	// A range describes committed trees, so build the same committed current-tree
	// view used by normal head-mode graph queries. A non-HEAD range is still
	// reported honestly; symbol resolution may be incomplete if its head is not
	// the checkout's current committed tree.
	snapshot, _, err := sem.LoadOrBuildProviderSnapshot(ctx, repo, opts.Version, sem.ProviderSnapshotOptions{
		NoNetwork: true, Worktree: false, Profile: profile,
	}, resolveCacheDir("", opts.Env.PluginDataDir), false)
	if err != nil {
		return err
	}
	readSource, closeSource := openSnapshotLineReaderOrDegrade(ctx, snapshot, false, opts.Stderr)
	if closeSource != nil {
		defer closeSource()
	}
	evidenceAvailable, evidenceLimitation := riskEvidenceMatchesHead(ctx, repo, flags.Head, snapshot)
	report := buildRiskReport(diff, snapshot, flags, readSource, evidenceAvailable, evidenceLimitation)
	if flags.Checkpoint != "" {
		report.Checkpoint = flags.Checkpoint
		report.Limitations = append(report.Limitations, "checkpoint impact evidence is resolved against the current committed checkout; verify it if the checkpoint commit is not checked out")
	} else {
		report.Base, report.Head = flags.Base, flags.Head
	}
	if profile != sem.ProfileFull {
		report.Limitations = append(report.Limitations, "the selected graph profile may omit relationship families; use --profile full for the most complete risk evidence")
	}
	switch flags.Format {
	case "json":
		encoder := json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout))
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	case "text":
		return writeRiskText(opts.Stdout, report)
	default:
		return fmt.Errorf("risk --format must be text or json, got %q", flags.Format)
	}
}

// riskEvidenceMatchesHead prevents a range from being paired with graph data
// from another committed tree. Provider snapshots can only describe the
// current checkout, so a non-current --head must not borrow its callers or
// type consumers as evidence.
func riskEvidenceMatchesHead(ctx context.Context, repo, head string, snapshot sem.ProviderSnapshot) (bool, string) {
	if snapshot.Header.Tree == "" {
		return false, "graph evidence was omitted because the current committed snapshot has no tree provenance"
	}
	requestedTree, err := gitutil.RevParse(ctx, repo, head+"^{tree}")
	if err != nil {
		return false, "graph evidence was omitted because the requested --head tree could not be resolved against the current committed snapshot"
	}
	if requestedTree != snapshot.Header.Tree {
		return false, "graph evidence was omitted because the requested --head is not the current committed checkout; check out that revision before analyzing its callers or type consumers"
	}
	return true, ""
}

type riskChangedEntity struct {
	change sem.EntityChange
	path   string
}

func buildRiskReport(diff sem.Result, snapshot sem.ProviderSnapshot, flags riskFlags, readSource lineReader, evidenceAvailable bool, evidenceLimitation string) riskReport {
	report := riskReport{FormatVersion: 1, OverallRisk: riskLow, Entries: []riskEntry{}, RecommendedTests: []string{}, Warnings: append([]sem.ProviderWarning{}, diff.Warnings...)}
	report.Warnings = append(report.Warnings, snapshot.Header.Warnings...)
	if !evidenceAvailable && evidenceLimitation != "" {
		report.Limitations = append(report.Limitations, evidenceLimitation)
	}
	var changes []riskChangedEntity
	for _, file := range diff.Files {
		for _, change := range file.Changes {
			path := change.NewPath
			if path == "" {
				path = file.Path
			}
			if path == "" {
				path = change.OldPath
			}
			changes = append(changes, riskChangedEntity{change: change, path: path})
		}
	}
	report.EntitiesChanged = len(changes)
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].change.DependentsCount != changes[j].change.DependentsCount {
			return changes[i].change.DependentsCount > changes[j].change.DependentsCount
		}
		if changes[i].path != changes[j].path {
			return changes[i].path < changes[j].path
		}
		return changes[i].change.Name < changes[j].change.Name
	})
	if len(changes) > flags.MaxEntities {
		report.EntitiesSkipped = len(changes) - flags.MaxEntities
		changes = changes[:flags.MaxEntities]
		report.Limitations = append(report.Limitations, fmt.Sprintf("analysis is capped at %d entities; %d lower-priority entity changes were skipped", flags.MaxEntities, report.EntitiesSkipped))
	}
	tests := map[string]bool{}
	for _, item := range changes {
		entry := buildRiskEntry(snapshot, item, readSource, evidenceAvailable, evidenceLimitation)
		for _, test := range entry.Tests {
			tests[test] = true
		}
		report.Entries = append(report.Entries, entry)
	}
	report.EntitiesAnalyzed = len(report.Entries)
	for test := range tests {
		report.RecommendedTests = append(report.RecommendedTests, test)
	}
	sort.Strings(report.RecommendedTests)
	report.OverallRisk = classifyChangesetRisk(report.Entries)
	if len(snapshot.Header.PartialFailures) > 0 {
		report.Limitations = append(report.Limitations, "the graph reported partial parse/index failures; inspect the evidence before relying on a complete blast radius")
	}
	if report.EntitiesChanged == 0 {
		report.Limitations = append(report.Limitations, "no semantic entity changes were detected")
	}
	return report
}

func buildRiskEntry(snapshot sem.ProviderSnapshot, item riskChangedEntity, readSource lineReader, evidenceAvailable bool, evidenceLimitation string) riskEntry {
	change := item.change
	name := change.NewName
	if name == "" {
		name = change.Name
	}
	if name == "" {
		name = change.OldName
	}
	line := change.AfterStartLine
	if line == 0 {
		line = change.BeforeStartLine
	}
	entry := riskEntry{Name: name, Kind: change.Kind, ChangeType: change.Type, FilePath: item.path, StartLine: line, DependentsCount: change.DependentsCount, Tests: []string{}}
	if !evidenceAvailable {
		entry.Limitations = append(entry.Limitations, evidenceLimitation)
		entry.Level = classifyEntityRisk(change, riskEvidence{})
		entry.Inference = riskInference(change, entry.Level, riskEvidence{})
		return entry
	}
	impact := buildImpactResponseFromReader(snapshot, impactFlags{Symbol: name, File: item.path, Line: line, Kind: change.Kind, Depth: 2, Limit: defaultImpactSectionLimit}, readSource)
	if impact.Focus == nil || impact.DisambiguationRequired {
		if impact.DisambiguationRequired {
			entry.Limitations = append(entry.Limitations, "graph symbol resolution is ambiguous; no blast radius was inferred")
		} else {
			entry.Limitations = append(entry.Limitations, "changed symbol is absent from the current graph snapshot (commonly an added, removed, or non-current-range entity)")
		}
		entry.Level = classifyEntityRisk(change, riskEvidence{})
		entry.Inference = riskInference(change, entry.Level, riskEvidence{})
		return entry
	}
	annotateImpactCallSites(&impact, readSource)
	evidence := riskEvidence{DirectCallers: impact.Callers.Direct, TransitiveCallers: impact.Callers.Transitive, TypeConsumers: impact.TypeConsumers.Total, Callees: impact.Callees.Total, TopCallers: []neighborEndpoint{}}
	for _, caller := range impact.Callers.Entries {
		evidence.TopCallers = append(evidence.TopCallers, caller.Endpoint)
	}
	entry.Evidence = evidence
	entry.Tests = collectAffectedTests(impact)
	entry.Level = classifyEntityRisk(change, evidence)
	entry.Inference = riskInference(change, entry.Level, evidence)
	if impact.Degenerate {
		entry.Limitations = append(entry.Limitations, impact.DegenerateReason)
	}
	if len(impact.PartialFailures) > 0 {
		entry.Limitations = append(entry.Limitations, "graph evidence is incomplete because this snapshot has partial failures")
	}
	return entry
}

// classifyEntityRisk is deliberately deterministic and conservative. Graph
// counts are evidence; the verdict below is the tool's inference from them.
func classifyEntityRisk(change sem.EntityChange, evidence riskEvidence) riskLevel {
	if evidence.DirectCallers >= 10 || evidence.TypeConsumers >= 10 || (change.Type == "signature_changed" && evidence.DirectCallers >= 5) || (change.Type == "removed" && change.DependentsCount >= 5) {
		return riskHigh
	}
	if evidence.DirectCallers > 0 || evidence.TypeConsumers > 0 || change.DependentsCount > 0 || change.Type == "signature_changed" || change.Type == "removed" {
		return riskMedium
	}
	return riskLow
}

func classifyChangesetRisk(entries []riskEntry) riskLevel {
	level := riskLow
	for _, entry := range entries {
		if entry.Level == riskHigh {
			return riskHigh
		}
		if entry.Level == riskMedium {
			level = riskMedium
		}
	}
	return level
}

func collectAffectedTests(impact impactResponse) []string {
	seen := map[string]bool{}
	collect := func(section impactSection) {
		for _, item := range section.Entries {
			if isConventionalTestPath(item.Endpoint.FilePath) {
				seen[item.Endpoint.FilePath] = true
			}
		}
	}
	collect(impact.Callers)
	collect(impact.CoChanges)
	tests := make([]string, 0, len(seen))
	for path := range seen {
		tests = append(tests, path)
	}
	sort.Strings(tests)
	return tests
}

func riskInference(change sem.EntityChange, level riskLevel, evidence riskEvidence) string {
	if evidence.DirectCallers > 0 {
		return fmt.Sprintf("%s change with %d direct graph caller%s and %d type consumer%s → %s risk.", change.Type, evidence.DirectCallers, plural(evidence.DirectCallers), evidence.TypeConsumers, plural(evidence.TypeConsumers), strings.ToUpper(string(level)))
	}
	if change.DependentsCount > 0 {
		return fmt.Sprintf("%s change has %d semantic-diff dependent%s but no resolved current-snapshot callers → %s risk.", change.Type, change.DependentsCount, plural(change.DependentsCount), strings.ToUpper(string(level)))
	}
	return fmt.Sprintf("%s change has no resolved graph callers or dependents → %s risk.", change.Type, strings.ToUpper(string(level)))
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func writeRiskText(out io.Writer, report riskReport) error {
	out = termsafe.NewWriter(out)
	fmt.Fprintf(out, "CHANGESET RISK: %s %s  (%d entities changed, %d analyzed)\n", strings.ToUpper(string(report.OverallRisk)), riskIcon(report.OverallRisk), report.EntitiesChanged, report.EntitiesAnalyzed)
	if report.Checkpoint != "" {
		fmt.Fprintf(out, "Checkpoint: %s\n", report.Checkpoint)
	} else {
		fmt.Fprintf(out, "Base: %s  Head: %s\n", report.Base, report.Head)
	}
	if len(report.Entries) == 0 {
		fmt.Fprintln(out, "No semantic entity changes detected.")
	}
	for _, entry := range report.Entries {
		fmt.Fprintf(out, "\n%s %s  %s [%s]\n", riskIcon(entry.Level), strings.ToUpper(string(entry.Level)), entry.Name, entry.ChangeType)
		if entry.FilePath != "" {
			fmt.Fprintf(out, "  Evidence location: %s:%d (%s)\n", entry.FilePath, entry.StartLine, entry.Kind)
		}
		fmt.Fprintln(out, "  Graph evidence:")
		fmt.Fprintf(out, "    %d direct callers, %d transitive callers, %d type consumers, %d callees\n", entry.Evidence.DirectCallers, entry.Evidence.TransitiveCallers, entry.Evidence.TypeConsumers, entry.Evidence.Callees)
		for _, caller := range entry.Evidence.TopCallers {
			fmt.Fprintf(out, "    - %s:%d  %s\n", caller.FilePath, caller.StartLine, caller.Name)
		}
		fmt.Fprintf(out, "  Our inference: %s\n", entry.Inference)
		for _, limitation := range entry.Limitations {
			fmt.Fprintf(out, "  Limitation: %s\n", limitation)
		}
	}
	fmt.Fprintf(out, "\nRECOMMENDED TESTS (%d files, from graph caller/co-change relationships)\n", len(report.RecommendedTests))
	for _, test := range report.RecommendedTests {
		fmt.Fprintf(out, "  %s\n", test)
	}
	if len(report.RecommendedTests) > 0 {
		fmt.Fprintln(out, "  Verify by running the relevant test files; recommendations are path-based, not coverage-based.")
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintln(out, "\nWARNINGS")
		for _, warning := range report.Warnings {
			fmt.Fprintf(out, "  %s", warning.Code)
			if warning.FilePath != "" {
				fmt.Fprintf(out, " %s", warning.FilePath)
			}
			if warning.EffectOnCompleteness != "" {
				fmt.Fprintf(out, " (%s)", warning.EffectOnCompleteness)
			}
			if warning.Detail != "" {
				fmt.Fprintf(out, ": %s", warning.Detail)
			}
			fmt.Fprintln(out)
		}
	}
	for _, limitation := range report.Limitations {
		fmt.Fprintf(out, "Limitation: %s\n", limitation)
	}
	return nil
}

func riskIcon(level riskLevel) string {
	switch level {
	case riskHigh:
		return "🔴"
	case riskMedium:
		return "🟡"
	default:
		return "🟢"
	}
}
