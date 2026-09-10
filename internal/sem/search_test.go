package sem

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestSearchRepositoryRanksExactSymbol(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "config/service.go", `package config

type ServiceConfig struct { Name string }

func NewServiceConfig(name string) ServiceConfig {
	return ServiceConfig{Name: name}
}
`)
	write(t, repo, "docs/example.go", `package docs

// This example discusses constructing a service configuration.
func Example() {}
`)

	response, err := SearchRepository(t.Context(), repo, "test", "NewServiceConfig", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 {
		t.Fatal("search returned no results")
	}
	first := response.Results[0]
	if first.FilePath != "config/service.go" || first.SymbolName != "NewServiceConfig" {
		t.Fatalf("first result = %#v", first)
	}
	if !containsString(first.Signals, "exact-symbol") {
		t.Fatalf("exact symbol signal missing: %#v", first.Signals)
	}
}

func TestSearchRepositorySupportsPunctuatedLanguageQuery(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "native/bridge.cpp", "// C++ bridge implementation\nint bridge() { return 1; }\n")
	write(t, repo, "docs/unrelated.txt", "plain language documentation\n")

	response, err := SearchRepository(t.Context(), repo, "test", "C++", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 || response.Results[0].FilePath != "native/bridge.cpp" {
		t.Fatalf("C++ search results = %#v", response.Results)
	}
}

func TestColdRepositoryProgressivePreselectionIsDeterministicAndBounded(t *testing.T) {
	repo := t.TempDir()
	for index := 0; index < 12; index++ {
		write(t, repo, fmt.Sprintf("src/part%02d.go", index), fmt.Sprintf(`package sample

func Match%02d() string { return "needle" }
`, index))
	}
	q := buildSearchQuery("needle")
	sparseQuery := buildSparseSearchQuery("needle")
	var first []string
	for run := 0; run < 20; run++ {
		selection, err := preselectSearchFiles(
			t.Context(), repo, q, sparseQuery, SearchOptions{
				Worktree:                true,
				Profile:                 ProfileSyntaxOnly,
				MaxIndexedFiles:         2,
				progressivePreselection: true,
			}, ProviderSnapshotOptions{NoNetwork: true, Worktree: true}, ProviderSnapshot{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if selection.preselectionPasses != 3 || !selection.preselectionWidened || !selection.preselectionBounded {
			t.Fatalf("selection = %#v, want a deterministic capped progressive selection", selection)
		}
		if len(selection.files) > 8 {
			t.Fatalf("selected files = %d, want at most four times the base", len(selection.files))
		}
		if run == 0 {
			first = append([]string(nil), selection.files...)
			continue
		}
		if !reflect.DeepEqual(selection.files, first) {
			t.Fatalf("run %d files = %#v, first = %#v", run, selection.files, first)
		}
	}
}

func TestColdPreselectionKeepsLegacyOrderAndStatsWhenEligibleSetFitsBase(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "src/zeta.go", "package sample\n// quasar nebula\nfunc Zeta() {}\n")
	write(t, repo, "src/alpha.go", "package sample\n// quasar nebula\nfunc Alpha() {}\n")
	write(t, repo, "src/lower.go", "package sample\n// quasar\nfunc Lower() {}\n")
	for index := 0; index < 5; index++ {
		write(t, repo, fmt.Sprintf("noise/file%02d.go", index), fmt.Sprintf(
			"package noise\nfunc Unrelated%02d() {}\n", index,
		))
	}
	q := buildSearchQuery("quasar nebula")
	selection, err := preselectSearchFiles(
		t.Context(), repo, q, buildSparseSearchQuery("quasar nebula"),
		SearchOptions{Worktree: true, MaxIndexedFiles: 3},
		ProviderSnapshotOptions{NoNetwork: true, Worktree: true}, ProviderSnapshot{}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{"src/alpha.go", "src/zeta.go", "src/lower.go"}
	if !reflect.DeepEqual(selection.files, wantFiles) {
		t.Fatalf("selected files = %#v, want legacy score/path order %#v", selection.files, wantFiles)
	}
	if selection.preselectionBackend != "go-content" || selection.preselectionPasses != 1 ||
		selection.preselectionFilesExamined != 8 || selection.filesContentRead != 8 {
		t.Fatalf("legacy scan stats changed: %#v", selection)
	}
	if selection.preselectionConfidence != 0 || selection.preselectionCoverage != 0 ||
		selection.preselectionDiversity != 0 || selection.preselectionWidened || selection.preselectionBounded {
		t.Fatalf("small eligible set unexpectedly reported progressive metrics: %#v", selection)
	}
}

func TestColdPreselectionExcludesConstraintOnlyDistractorWhenLegacySetFitsBase(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "src/target.go", "package sample\n// needle\nfunc Target() {}\n")
	write(t, repo, "src/distractor.go", "package sample\n// NamedWidget follows ordered phrase.\nfunc Distractor() {}\n")
	for index := 0; index < 3; index++ {
		write(t, repo, fmt.Sprintf("noise/file%02d.go", index), fmt.Sprintf(
			"package noise\nfunc Unrelated%02d() {}\n", index,
		))
	}
	q := preselectionTestQuery("needle")
	q.constraints = searchConstraints{
		Entities: []searchEntityConstraint{{Raw: "NamedWidget", Normalized: "namedwidget"}},
		Phrases:  []searchPhraseConstraint{{Words: []string{"ordered", "phrase"}, Explicit: true}},
	}
	selection, err := preselectSearchFiles(
		t.Context(), repo, q, buildSparseSearchQuery("needle"),
		SearchOptions{Worktree: true, MaxIndexedFiles: 2},
		ProviderSnapshotOptions{NoNetwork: true, Worktree: true}, ProviderSnapshot{}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selection.files, []string{"src/target.go"}) {
		t.Fatalf("selected files = %#v, want only the ordinary path/content match", selection.files)
	}
	if selection.preselectionPasses != 1 || selection.preselectionConfidence != 0 ||
		selection.preselectionCoverage != 0 || selection.preselectionDiversity != 0 ||
		selection.preselectionWidened || selection.preselectionBounded {
		t.Fatalf("constraint-only file incorrectly activated progressive selection: %#v", selection)
	}
}

func TestSearchRepositoryFindsConceptualBodyText(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "encoding/serializer.go", `package encoding

// MarshalCompact emits minified serialization output for network transport.
func MarshalCompact(value any) []byte { return nil }

// MarshalIndented provides pretty printing for human-readable output.
func MarshalIndented(value any) []byte { return nil }
`)
	write(t, repo, "transport/socket.go", `package transport

// Send writes bytes to a socket.
func Send(value []byte) error { return nil }
`)

	response, err := SearchRepository(t.Context(), repo, "test", "minified and pretty printing serialization output", SearchOptions{
		Worktree:          true,
		Profile:           ProfileSyntaxOnly,
		TopK:              5,
		MaxRegionsPerFile: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) < 2 {
		t.Fatalf("results = %#v stats = %#v", response.Results, response.Stats)
	}
	seen := map[string]bool{}
	for _, result := range response.Results {
		if result.FilePath == "encoding/serializer.go" {
			seen[result.SymbolName] = true
		}
	}
	if !seen["MarshalCompact"] || !seen["MarshalIndented"] {
		t.Fatalf("conceptual query did not preserve both relevant regions: %#v", response.Results)
	}
}

func TestSearchRepositoryPreservesDistinctRegionsInOneFile(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "timing/wheel.go", `package timing

// NewTimingWheel creates the production timing wheel.
func NewTimingWheel(interval int) *Wheel {
	return newTimingWheelWithClock(interval, systemClock{})
}

// newTimingWheelWithClock creates the timing wheel with an injected clock.
func newTimingWheelWithClock(interval int, clock Clock) *Wheel {
	return &Wheel{}
}

type Wheel struct{}
type Clock interface{}
type systemClock struct{}
`)

	response, err := SearchRepository(t.Context(), repo, "test", "create timing wheel with injected clock", SearchOptions{
		Worktree:          true,
		Profile:           ProfileSyntaxOnly,
		TopK:              5,
		MaxRegionsPerFile: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]SearchResult{}
	for _, result := range response.Results {
		seen[result.SymbolName] = result
	}
	constructor, constructorOK := seen["NewTimingWheel"]
	helper, helperOK := seen["newTimingWheelWithClock"]
	if !constructorOK || !helperOK {
		t.Fatalf("missing distinct same-file regions: %#v", response.Results)
	}
	if constructor.StartLine == helper.StartLine {
		t.Fatalf("regions collapsed to one location: %#v", response.Results)
	}
}

func TestSearchRepositoryUsesFocusedDefaultRegions(t *testing.T) {
	repo := t.TempDir()
	body := "package source\n\nfunc LargeHandler() {\n" +
		strings.Repeat("\t// unrelated implementation detail\n", 90) +
		"\t// rare retrieval needle\n}\n"
	write(t, repo, "source/large.go", body)

	response, err := SearchRepository(t.Context(), repo, "test", "rare retrieval needle", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 {
		t.Fatal("focused default search returned no results")
	}
	const (
		wantMaxLines = 80
		needleLine   = 94
		// LargeHandler's own bounds: `func` on line 3, closing brace on line 95.
		bodyStart = 3
		bodyEnd   = 95
	)
	for _, result := range response.Results {
		// The region cap bounds what RANKING may emit. A result the snippet allocator
		// upgraded to a complete body is deliberately exempt — it reports the callable's
		// true bounds, which is the whole point of the upgrade — so it is held to the
		// stricter standard instead: exactly the enclosing function, not a bigger window.
		if containsString(result.Signals, searchCompleteSymbolSignal) {
			if result.SnippetStartLine != bodyStart || result.SnippetEndLine != bodyEnd {
				t.Fatalf("complete body spans %d-%d, want %d-%d: %#v",
					result.SnippetStartLine, result.SnippetEndLine, bodyStart, bodyEnd, result)
			}
		} else if lines := result.EndLine - result.StartLine + 1; lines > wantMaxLines {
			t.Fatalf("default result spans %d lines, want at most %d: %#v", lines, wantMaxLines, result)
		}
		if result.StartLine > needleLine || result.EndLine < needleLine {
			t.Fatalf("focused result omitted needle line %d: %#v", needleLine, result)
		}
	}
}

func TestSearchRepositoryBuildsSparseRegionsOnlyForDeepSearch(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "source/large.go", "package source\n"+strings.Repeat("// filler line\n", 140)+"// sparse retrieval needle\n")

	shallow, err := SearchRepository(t.Context(), repo, "test", "sparse retrieval needle", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     defaultSearchTopK,
	})
	if err != nil {
		t.Fatal(err)
	}
	if shallow.Stats.SparseCandidates != 0 {
		t.Fatalf("shallow search built %d sparse candidates", shallow.Stats.SparseCandidates)
	}

	deep, err := SearchRepository(t.Context(), repo, "test", "sparse retrieval needle", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     defaultSearchTopK + 1,
		Deep:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if deep.Stats.SparseCandidates == 0 {
		t.Fatal("deep search did not build sparse candidates")
	}
	foundSparse := false
	for _, result := range deep.Results {
		if containsString(result.Signals, "sparse-region") {
			foundSparse = true
		}
	}
	if !foundSparse {
		t.Fatalf("deep results omitted sparse regions: %#v", deep.Results)
	}
	if deep.Stats.SparseFilesRead == 0 {
		t.Fatal("deep search did not report sparse file reads")
	}
	for _, result := range deep.Results {
		if containsString(result.Signals, "sparse-region") && result.Snippet == "" {
			t.Fatalf("sparse result was not hydrated: %#v", result)
		}
	}
}

func TestSelectHybridCandidatesBalancesSparseDepthAndFileDiversity(t *testing.T) {
	semantic := make([]searchCandidate, 100)
	sparse := make([]searchCandidate, 100)
	for index := range semantic {
		semantic[index] = searchCandidate{
			result: SearchResult{FilePath: fmt.Sprintf("semantic/%03d.go", index), StartLine: 1, EndLine: 10},
			score:  float64(100 - index),
		}
		sparse[index] = searchCandidate{
			result: SearchResult{
				FilePath:  fmt.Sprintf("semantic/%03d.go", 99-index),
				StartLine: index*60 + 1,
				EndLine:   index*60 + 80,
				Signals:   []string{"sparse-region"},
			},
			score: float64(100 - index),
		}
	}

	selected := selectHybridCandidates(semantic, sparse, 100)
	if len(selected) != 100 {
		t.Fatalf("hybrid selection returned %d candidates, want 100", len(selected))
	}
	if selected[0].result.FilePath != semantic[0].result.FilePath {
		t.Fatalf("hybrid ranking did not preserve a semantic head: %#v", selected[0])
	}
	sparseCount := 0
	files := map[string]bool{}
	for _, candidate := range selected {
		files[candidate.result.FilePath] = true
		if containsString(candidate.result.Signals, "sparse-region") {
			sparseCount++
		}
	}
	if sparseCount <= len(selected)/2 || sparseCount >= len(selected)*3/4 {
		t.Fatalf("hybrid sparse count = %d, want a majority with a semantic reserve", sparseCount)
	}
	if len(files) < 90 {
		t.Fatalf("hybrid selection covered only %d distinct files", len(files))
	}
}

func TestSelectHybridCandidatesFallsBackToSparseOnly(t *testing.T) {
	sparse := []searchCandidate{
		{result: SearchResult{FilePath: "first.go", StartLine: 1, EndLine: 80}},
		{result: SearchResult{FilePath: "second.go", StartLine: 1, EndLine: 80}},
	}
	selected := selectHybridCandidates(nil, sparse, 2)
	if len(selected) != 2 || selected[0].result.FilePath != "first.go" || selected[1].result.FilePath != "second.go" {
		t.Fatalf("sparse-only selection = %#v", selected)
	}
	if selected[0].score <= selected[1].score {
		t.Fatalf("sparse-only scores are not rank-monotonic: %#v", selected)
	}
}

func TestSparseSearchFocusLineUsesSubtokenEvidence(t *testing.T) {
	query := buildSparseSearchQuery("HTTPServer")
	lines := []string{"unrelated", "http server handler", "unrelated"}
	if got := sparseSearchFocusLine(query, lines, 1, len(lines)); got != 2 {
		t.Fatalf("sparse focus line = %d, want 2", got)
	}
}

func TestScaleSearchCorpusValueSaturatesWithoutOverflow(t *testing.T) {
	maxValue := int(^uint(0) >> 1)
	if got := scaleSearchCorpusValue(maxValue, maxValue, 1); got != maxValue {
		t.Fatalf("scaled max int = %d, want %d", got, maxValue)
	}
	if got := scaleSearchCorpusValue(10, 20, 5); got != 40 {
		t.Fatalf("scaled corpus value = %d, want 40", got)
	}
}

func TestSearchRepositorySkipsSparsePassWithoutSparseTerms(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "source.go", "package source\n// why\n")
	response, err := SearchRepository(t.Context(), repo, "test", "why", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     defaultSearchTopK + 1,
		Deep:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Stats.SparseFilesRead != 0 || response.Stats.SparseCandidates != 0 {
		t.Fatalf("stopword-only sparse pass stats = %#v", response.Stats)
	}
}

func TestBuildSparseSearchQueryUsesLexicalSubtokens(t *testing.T) {
	query := buildSparseSearchQuery("HTTPServer foo_bar and build2D 123")
	want := []string{"http", "server", "foo", "bar", "build", "123"}
	if fmt.Sprint(query.terms) != fmt.Sprint(want) {
		t.Fatalf("sparse query terms = %v, want %v", query.terms, want)
	}
}

func TestSelectHybridCandidatesBoundsSparseFusionAtTopK(t *testing.T) {
	semantic := []searchCandidate{{
		result: SearchResult{FilePath: "strong.go", StartLine: 1, EndLine: 10},
		score:  100,
	}}
	sparse := make([]searchCandidate, 101)
	for index := range sparse {
		sparse[index] = searchCandidate{
			result: SearchResult{
				FilePath:  fmt.Sprintf("weak/%03d.go", index),
				StartLine: 1,
				EndLine:   80,
				Signals:   []string{"sparse-region"},
			},
			score: float64(101 - index),
		}
	}
	sparse[100].result.FilePath = "strong.go"
	sparse[100].result.StartLine = 1001
	sparse[100].result.EndLine = 1080

	selected := selectHybridCandidates(semantic, sparse, 100)
	for _, candidate := range selected {
		if candidate.result.FilePath == "strong.go" && candidate.result.StartLine == 1001 {
			t.Fatal("sparse candidate below TopK participated in reciprocal-rank fusion")
		}
	}
}

func TestSearchRepositoryExpandsMorphologicalIssueTerms(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "reader/collection.go", `package reader

// initializeCollectionReader selects the collection implementation.
func initializeCollectionReader(kind string) *CollectionReader { return &CollectionReader{} }

// readObject reads values from the input stream.
func (r *CollectionReader) readObject(input []byte) any { return nil }

type CollectionReader struct{}
`)

	response, err := SearchRepository(t.Context(), repo, "test", "collection reader initialization", SearchOptions{
		Worktree:          true,
		Profile:           ProfileSyntaxOnly,
		TopK:              10,
		MaxRegionsPerFile: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	seenInitializer := false
	for _, result := range response.Results {
		if result.SymbolName == "initializeCollectionReader" {
			seenInitializer = true
		}
	}
	if !seenInitializer {
		t.Fatalf("initialization did not expand to initialize: %#v", response.Results)
	}
}

func TestSearchRepositoryExpandsSemanticNeighbor(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "auth/auth.go", `package auth

// Authenticate creates a user session after successful login.
func Authenticate(raw string) bool {
	return checkSignature(raw)
}

func checkSignature(raw string) bool {
	return len(raw) > 4
}
`)

	response, err := SearchRepository(t.Context(), repo, "test", "authenticate user session login", SearchOptions{
		Worktree: true,
		Profile:  ProfileFast,
		TopK:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	var helper SearchResult
	for _, result := range response.Results {
		if result.SymbolName == "checkSignature" {
			helper = result
			break
		}
	}
	if helper.SymbolName == "" {
		t.Fatalf("graph neighbor missing: %#v", response.Results)
	}
	foundGraphSignal := false
	for _, signal := range helper.Signals {
		if strings.HasPrefix(signal, "graph:") {
			foundGraphSignal = true
		}
	}
	if !foundGraphSignal {
		t.Fatalf("helper was not identified through graph expansion: %#v", helper)
	}
}

func TestSearchExpandsCompoundIdentifierConsumers(t *testing.T) {
	const filePath = "src/policy.ts"
	content := "export const cacheRefreshPolicy = 'eager'\n\nexport function configureCache() {\n  install(cacheRefreshPolicy)\n}\n"
	definition := SymbolRecord{ID: "definition", Name: "cacheRefreshPolicy", FilePath: filePath, StartLine: 1, EndLine: 1}
	consumer := SymbolRecord{ID: "consumer", Name: "configureCache", FilePath: filePath, StartLine: 3, EndLine: 5}
	got := expandIdentifierUsageCandidates(
		t.Context(),
		[]searchCandidate{{score: 30, result: SearchResult{SymbolID: definition.ID}}},
		buildSearchQuery("cache refresh policy"),
		map[string]SymbolRecord{definition.ID: definition},
		map[string][]SymbolRecord{filePath: {definition, consumer}},
		func(path string) (string, bool) { return content, path == filePath },
		map[string]string{filePath: "typescript"}, SearchOptions{ContextLines: 2, MaxSnippetLines: 20},
	)
	if len(got) != 1 || got[0].result.FocusLine != 4 || got[0].result.SymbolName != "configureCache" || !containsString(got[0].result.Signals, "symbol-usage") {
		t.Fatalf("consumer expansion = %#v", got)
	}
}

func TestSearchUsageIdentifiersIncludeScreamingSnakeAndUnicode(t *testing.T) {
	for _, name := range []string{"MAX_RETRY_COUNT", "überCache_Value", "数据_缓存"} {
		if !expandableUsageIdentifier(name) {
			t.Fatalf("compound identifier %q was excluded", name)
		}
	}
	if expandableUsageIdentifier("ordinary") {
		t.Fatal("plain lowercase word was accepted as a compound identifier")
	}
}

func TestSearchUsageExpansionHandlesEmptySeeds(t *testing.T) {
	got := expandIdentifierUsageCandidates(
		t.Context(), nil, buildSearchQuery("cache policy"), nil, nil,
		func(string) (string, bool) { t.Fatal("empty seeds should not read content"); return "", false },
		nil, SearchOptions{},
	)
	if got != nil {
		t.Fatalf("empty seed expansion = %#v", got)
	}
}

func TestCachedContentReaderBoundsLargeFiles(t *testing.T) {
	calls := map[string]int{}
	large := strings.Repeat("x", maxSearchContentCacheFileBytes+1)
	read := cachedContentReader(func(path string) (string, bool) {
		calls[path]++
		if path == "large" {
			return large, true
		}
		return "small", true
	})
	read("small")
	read("small")
	read("large")
	read("large")
	if calls["small"] != 1 || calls["large"] != 2 {
		t.Fatalf("content reader calls = %#v", calls)
	}
}

func TestDerivedSearchScoreStaysBelowParent(t *testing.T) {
	for _, proposed := range []float64{1, 10, 100} {
		if got := derivedSearchScore(10, proposed); got >= 10 {
			t.Fatalf("derived score %v was not below parent", got)
		}
	}
}

func TestSearchRelationshipCanPrecedeWeakFileDiversity(t *testing.T) {
	selected := []searchCandidate{{result: SearchResult{FilePath: "src/a.go", StartLine: 1, EndLine: 3}}}
	perFile := map[string]int{"src/a.go": 1}
	perSymbol := map[string]int{}
	perBase := map[string]int{}
	related := searchCandidate{score: 10, result: SearchResult{FilePath: "src/a.go", StartLine: 20, EndLine: 22, Signals: []string{"symbol-usage"}}}
	unseen := searchCandidate{score: 10, result: SearchResult{FilePath: "src/b.go", StartLine: 1, EndLine: 3}}
	if shouldReserveUnseenSearchFile([]searchCandidate{related, unseen}, selected, perFile, perSymbol, perBase, 3) {
		t.Fatal("strong causal relationship was incorrectly delayed for file diversity")
	}
	related.score = 6
	if !shouldReserveUnseenSearchFile([]searchCandidate{related, unseen}, selected, perFile, perSymbol, perBase, 3) {
		t.Fatal("weak causal relationship incorrectly displaced a stronger unseen file")
	}
}

func TestSearchPrioritizesExecutableIdentifierConsumers(t *testing.T) {
	definitionPath, consumerPath, testPath := "src/snippets.ts", "src/index.ts", "src/__tests__/snippets.spec.ts"
	definition := SymbolRecord{ID: "definition", Name: "modernChunkLegacyGuard", FilePath: definitionPath, StartLine: 1, EndLine: 1}
	consumer := SymbolRecord{ID: "consumer", Name: "renderChunk", Kind: "method", FilePath: consumerPath, StartLine: 1, EndLine: 3}
	contents := map[string]string{
		definitionPath: "export const modernChunkLegacyGuard = guard\n",
		consumerPath:   "renderChunk(chunk) {\n  prepend(modernChunkLegacyGuard)\n}\n",
		testPath:       "import { modernChunkLegacyGuard } from '../snippets'\nexpect(modernChunkLegacyGuard).toBeTruthy()\n",
	}
	got := expandIdentifierUsageCandidates(
		t.Context(),
		[]searchCandidate{{score: 30, result: SearchResult{SymbolID: definition.ID}}}, buildSearchQuery("legacy guard consumer"),
		map[string]SymbolRecord{definition.ID: definition}, map[string][]SymbolRecord{consumerPath: {consumer}},
		func(path string) (string, bool) { content, ok := contents[path]; return content, ok },
		map[string]string{definitionPath: "typescript", consumerPath: "typescript", testPath: "typescript"},
		SearchOptions{ContextLines: 2, MaxSnippetLines: 20},
	)
	if len(got) == 0 || got[0].result.FilePath != consumerPath || got[0].result.FocusLine != 2 {
		t.Fatalf("top executable consumer = %#v", got)
	}
	for _, candidate := range got {
		if candidate.result.FilePath == testPath && candidate.result.FocusLine == 1 {
			t.Fatalf("import was emitted as a consumer: %#v", got)
		}
	}
}

func TestSearchBridgesNearbyStrongRegions(t *testing.T) {
	const filePath = "src/scheduler.ts"
	content := "function enqueue() {\n  return pending\n}\nfunction dispatchDue() {\n  advanceBeforeRun()\n}\nfunction runOne() {\n  return execute()\n}\n"
	left := SymbolRecord{ID: "enqueue", Name: "enqueue", Kind: "function", FilePath: filePath, StartLine: 1, EndLine: 3}
	right := SymbolRecord{ID: "run", Name: "runOne", Kind: "function", FilePath: filePath, StartLine: 7, EndLine: 9}
	got := expandSameFileBridgeCandidates(
		t.Context(),
		[]searchCandidate{{score: 30, result: SearchResult{FilePath: filePath, StartLine: 1, EndLine: 3, SymbolID: left.ID, Kind: left.Kind}}, {score: 20, result: SearchResult{FilePath: filePath, StartLine: 7, EndLine: 9, SymbolID: right.ID, Kind: right.Kind}}},
		buildSearchQuery("scheduled automation run"), map[string][]SymbolRecord{filePath: {left, right}},
		func(path string) (string, bool) { return content, path == filePath }, map[string]string{filePath: "typescript"},
		SearchOptions{TopK: 20, MaxSnippetLines: 40},
	)
	if len(got) != 1 || got[0].result.StartLine != 4 || got[0].result.EndLine != 9 || !containsString(got[0].result.Signals, "same-file-bridge") {
		t.Fatalf("bridge expansion = %#v", got)
	}
}

func TestSearchExpandsAdjacentLifecycleStage(t *testing.T) {
	const filePath = "src/scheduler.ts"
	content := "class Scheduler {\n  tick() {\n    return ready\n  }\n  dispatchDue() {\n    return runOne()\n  }\n}\n"
	tick := SymbolRecord{ID: "tick", Name: "tick", Kind: "method", FilePath: filePath, ContainerID: "scheduler", StartLine: 2, EndLine: 4}
	dispatch := SymbolRecord{ID: "dispatch", Name: "dispatchDue", Kind: "method", FilePath: filePath, ContainerID: "scheduler", StartLine: 5, EndLine: 7}
	got := expandSameContainerNeighborCandidates(
		t.Context(),
		[]searchCandidate{{score: 30, result: SearchResult{FilePath: filePath, StartLine: 2, EndLine: 4, SymbolID: tick.ID, Kind: tick.Kind}}},
		buildSearchQuery("scheduled automation tick"), map[string]SymbolRecord{tick.ID: tick}, map[string][]SymbolRecord{filePath: {tick, dispatch}},
		func(path string) (string, bool) { return content, path == filePath }, map[string]string{filePath: "typescript"},
		SearchOptions{TopK: 20, ContextLines: 2, MaxSnippetLines: 20},
	)
	if len(got) != 1 || got[0].result.StartLine != 5 || got[0].result.SymbolName != "dispatchDue" || !containsString(got[0].result.Signals, "same-container-neighbor") {
		t.Fatalf("neighbor expansion = %#v", got)
	}
}

func TestSearchRepositoryRejectsStopWordsOnly(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "main.go", "package main\n")
	_, err := SearchRepository(t.Context(), repo, "test", "the and with", SearchOptions{Worktree: true})
	if err == nil {
		t.Fatal("expected an error for a stop-word-only query")
	}
}

func TestSearchQueryDropsNarrativeStopWordsBeforePreselection(t *testing.T) {
	query := buildSearchQuery("automation did not run while computer was asleep")
	for _, stopWord := range []string{"did", "not", "while"} {
		if query.termSet[stopWord] {
			t.Fatalf("narrative stop word %q survived: %#v", stopWord, query.terms)
		}
	}
	for _, meaningful := range []string{"automation", "computer", "asleep"} {
		if !query.termSet[meaningful] {
			t.Fatalf("meaningful term %q missing: %#v", meaningful, query.terms)
		}
	}
}

func TestSearchRepositoryPreservesHeadProvenanceWhenPreselectionIsEmpty(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "alpha.go", "package source\nfunc Alpha() {}\n")
	write(t, repo, "beta.go", "package source\nfunc Beta() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")

	response, err := SearchRepository(t.Context(), repo, "test", "unmatched-provenance-query", SearchOptions{
		Profile:         ProfileSyntaxOnly,
		MaxIndexedFiles: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 || response.Stats.FilesIndexed != 0 {
		t.Fatalf("unexpected results: results=%#v stats=%#v", response.Results, response.Stats)
	}
	if response.Commit != rev(t, repo, "HEAD") || response.Tree != rev(t, repo, "HEAD^{tree}") {
		t.Fatalf("provenance does not identify HEAD: commit=%q tree=%q", response.Commit, response.Tree)
	}
	if response.Warnings == nil || len(response.Warnings) != 0 {
		t.Fatalf("warnings = %#v", response.Warnings)
	}
}

func TestSearchRepositoryPreservesSourceWarningsWhenPreselectionIsEmpty(t *testing.T) {
	t.Run("worktree", func(t *testing.T) {
		repo := t.TempDir()
		git(t, repo, "init")
		git(t, repo, "config", "user.name", "Entire Graph Test")
		git(t, repo, "config", "user.email", "graph@example.com")
		write(t, repo, "alpha.go", "package source\nfunc Alpha() {}\n")
		write(t, repo, "beta.go", "package source\nfunc Beta() {}\n")
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-m", "initial")

		response, err := SearchRepository(t.Context(), repo, "test", "unmatched-worktree-query", SearchOptions{
			Worktree:        true,
			Profile:         ProfileSyntaxOnly,
			MaxIndexedFiles: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.Commit == "" || response.Tree == "" {
			t.Fatalf("worktree response lost baseline provenance: %#v", response)
		}
		if len(response.Warnings) != 1 || response.Warnings[0].Code != "W_WORKTREE_SNAPSHOT" {
			t.Fatalf("warnings = %#v", response.Warnings)
		}
	})

	t.Run("no git head", func(t *testing.T) {
		repo := t.TempDir()
		write(t, repo, "alpha.go", "package source\nfunc Alpha() {}\n")
		write(t, repo, "beta.go", "package source\nfunc Beta() {}\n")

		response, err := SearchRepository(t.Context(), repo, "test", "unmatched-fallback-query", SearchOptions{
			Profile:         ProfileSyntaxOnly,
			MaxIndexedFiles: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.Commit != "" || response.Tree != "" {
			t.Fatalf("unexpected git provenance: %#v", response)
		}
		if len(response.Warnings) != 1 || response.Warnings[0].Code != "E_NO_GIT_HEAD" {
			t.Fatalf("warnings = %#v", response.Warnings)
		}
	})
}

func TestSearchRepositoryReusesCommittedIndexCache(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth.go", `package auth

// ValidateToken verifies an authentication token.
func ValidateToken(token string) bool { return token != "" }
`)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	options := SearchOptions{
		Profile:  ProfileFast,
		TopK:     5,
		CacheDir: t.TempDir(),
	}
	first, err := SearchRepository(t.Context(), repo, "test-version", "validate authentication token", options)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stats.IndexCacheHit {
		t.Fatal("first search unexpectedly hit the index cache")
	}
	second, err := SearchRepository(t.Context(), repo, "test-version", "validate authentication token", options)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Stats.IndexCacheHit {
		t.Fatal("second search did not reuse the committed index cache")
	}
	if len(first.Results) == 0 || len(second.Results) == 0 || first.Results[0].SymbolID != second.Results[0].SymbolID {
		t.Fatalf("cache changed retrieval: first=%#v second=%#v", first.Results, second.Results)
	}
}

func TestWriteSearchSnapshotReplacesExistingEntry(t *testing.T) {
	entry := testCacheEntry(t)
	first := cachedSearchSnapshot{CacheVersion: searchSnapshotCacheVersion, Tree: "first"}
	second := cachedSearchSnapshot{CacheVersion: searchSnapshotCacheVersion, Tree: "second"}
	if err := writeSearchSnapshot(entry, first); err != nil {
		t.Fatalf("write first snapshot: %v", err)
	}
	if err := writeSearchSnapshot(entry, second); err != nil {
		t.Fatalf("replace snapshot: %v", err)
	}
	got, err := readSearchSnapshot(entry)
	if err != nil {
		t.Fatalf("read replaced snapshot: %v", err)
	}
	if got.Tree != second.Tree {
		t.Fatalf("cached tree = %q, want %q", got.Tree, second.Tree)
	}
}

func TestSearchRepositorySelectivelyIndexesLargeRepositories(t *testing.T) {
	repo := t.TempDir()
	for index := 0; index < 20; index++ {
		write(t, repo, fmt.Sprintf("noise/file_%02d.go", index), fmt.Sprintf("package noise\nfunc Noise%d() int { return %d }\n", index, index))
	}
	write(t, repo, "target/needle.go", `package target

// NeedleTarget handles the rare selective-indexing request.
func NeedleTarget() bool { return true }
`)
	response, err := SearchRepository(t.Context(), repo, "test", "NeedleTarget selective indexing", SearchOptions{
		Worktree:        true,
		Profile:         ProfileSyntaxOnly,
		TopK:            5,
		MaxIndexedFiles: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Stats.FilesScanned != 21 || response.Stats.FilesIndexed > 4 {
		t.Fatalf("unexpected selective-index stats: %#v", response.Stats)
	}
	if len(response.Results) == 0 || response.Results[0].SymbolName != "NeedleTarget" {
		t.Fatalf("selective index lost the target: %#v", response.Results)
	}
}

func TestDefaultSearchIndexedFilesScalesWithRequestedDepth(t *testing.T) {
	tests := []struct {
		topK int
		want int
	}{
		{topK: 1, want: defaultSearchMaxIndexedFiles},
		{topK: defaultSearchTopK, want: defaultSearchMaxIndexedFiles},
		{topK: 40, want: 120},
		{topK: 100, want: deepSearchMaxIndexedFiles},
		{topK: 1_000, want: deepSearchMaxIndexedFiles},
		{topK: int(^uint(0) >> 1), want: deepSearchMaxIndexedFiles},
	}
	for _, test := range tests {
		if got := defaultSearchIndexedFiles(test.topK); got != test.want {
			t.Fatalf("defaultSearchIndexedFiles(%d) = %d, want %d", test.topK, got, test.want)
		}
	}
}

func TestSearchRepositoryAppliesAdaptiveAndExplicitFileLimits(t *testing.T) {
	repo := t.TempDir()
	for index := 0; index < 130; index++ {
		write(t, repo, fmt.Sprintf("docs/file_%03d.md", index), "adaptive retrieval needle\n")
	}

	adaptive, err := SearchRepository(t.Context(), repo, "test", "adaptive retrieval needle", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if adaptive.Stats.FilesIndexed != 120 {
		t.Fatalf("adaptive files indexed = %d, want 120", adaptive.Stats.FilesIndexed)
	}
	if adaptive.Stats.PreselectionWidened || adaptive.Stats.PreselectionBounded {
		t.Fatalf("adaptive hard limit unexpectedly widened: %#v", adaptive.Stats)
	}

	explicit, err := SearchRepository(t.Context(), repo, "test", "adaptive retrieval needle", SearchOptions{
		Worktree:        true,
		Profile:         ProfileSyntaxOnly,
		TopK:            40,
		MaxIndexedFiles: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Stats.FilesIndexed != 4 {
		t.Fatalf("explicit files indexed = %d, want 4", explicit.Stats.FilesIndexed)
	}
	if explicit.Stats.PreselectionWidened || explicit.Stats.PreselectionBounded {
		t.Fatalf("explicit hard limit unexpectedly widened: %#v", explicit.Stats)
	}
}

func TestSearchRepositoryDoesNotTreatPathPriorAsEvidence(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "src/target.go", "package source\nfunc TargetNeedle() bool { return true }\n")
	write(t, repo, "src/unrelated.go", "package source\nfunc UnrelatedOperation() bool { return false }\n")
	response, err := SearchRepository(t.Context(), repo, "test", "TargetNeedle", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if result.SymbolName == "UnrelatedOperation" {
			t.Fatalf("source-directory prior created an unrelated candidate: %#v", response.Results)
		}
	}
}

func TestSearchRepositoryDropsWeakFragmentPathOnlyCandidates(t *testing.T) {
	repo := t.TempDir()
	// "config" is only a derived fragment (weight 1.1) of the compound query
	// identifier, and the file body contains no query tokens: the file must
	// not produce a candidate at all, and in particular must not produce a
	// zero-value result that fails response validation.
	write(t, repo, "config/notes.md", "# unrelated documentation\nplain prose only\n")
	write(t, repo, "svc/main.go", "package svc\nfunc NewServiceConfig() {}\n")

	response, err := SearchRepository(t.Context(), repo, "test", "NewServiceConfig", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("response validation failed: %v (results=%#v)", err, response.Results)
	}
	if len(response.Results) == 0 || response.Results[0].SymbolName != "NewServiceConfig" {
		t.Fatalf("expected symbol result first: %#v", response.Results)
	}
	for _, result := range response.Results {
		if result.FilePath == "config/notes.md" || result.FilePath == "" {
			t.Fatalf("weak fragment-only path match produced a candidate: %#v", response.Results)
		}
	}
}

func TestSearchRepositoryKeepsFullTermPathOnlyCandidates(t *testing.T) {
	repo := t.TempDir()
	// "authentication" matches the path as a full-weight query term, so the
	// path-only fallback candidate must survive even though the body has no
	// query tokens.
	write(t, repo, "docs/authentication.md", "# overview\nplain prose only\n")
	write(t, repo, "svc/main.go", "package svc\nfunc Unrelated() {}\n")

	response, err := SearchRepository(t.Context(), repo, "test", "authentication", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	found := false
	for _, result := range response.Results {
		if result.FilePath == "docs/authentication.md" {
			found = true
			if !containsString(result.Signals, "path") {
				t.Fatalf("path-only candidate lost its path signal: %#v", result)
			}
		}
	}
	if !found {
		t.Fatalf("full-term path-only candidate was dropped: %#v", response.Results)
	}
}

func TestCommittedPreselectionRequiresExactFullPreindexForUnboundedCandidates(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	queryText := "alphaaaa bravoooo charlieee deltaaaa echooooo foxtrott golfzzzz hotelzzz indiazzz julietzz kiloaaaa limaaaaa mikeaaaa november oscaraaa papaaaaa quebecaa romeoooo sierraaa tangoaaa"
	query := buildSearchQuery(queryText)
	bounded := searchPreselectionPatterns(query)
	lateTerm := ""
	for _, term := range query.terms {
		if query.weights[term] >= 1 && !containsString(bounded, term) {
			lateTerm = term
			break
		}
	}
	if lateTerm == "" {
		t.Fatalf("test query did not produce an expanded term beyond the legacy bounded set: terms=%#v bounded=%#v", query.terms, bounded)
	}
	const eligibleFiles = 12
	for index := 0; index < eligibleFiles; index++ {
		write(t, repo, fmt.Sprintf("src/match_%02d.go", index), fmt.Sprintf(
			"package source\n// %s\nfunc Match%d() int { return %d }\n", lateTerm, index, index,
		))
	}
	for index := 0; index < 20; index++ {
		write(t, repo, fmt.Sprintf("noise/file_%02d.go", index), fmt.Sprintf(
			"package noise\nfunc Noise%d() int { return %d }\n", index, index,
		))
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")

	cold, err := SearchRepository(t.Context(), repo, "test", queryText, SearchOptions{
		Profile: ProfileSyntaxOnly, TopK: 10, MaxIndexedFiles: 1, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cold.Stats.FilesIndexed > searchPreselectionMaxWideningFactor {
		t.Fatalf("cold committed search exceeded the progressive cap: %#v", cold.Stats)
	}
	if cold.Stats.PreselectionBackend != "go-content" || cold.Stats.FilesContentRead == 0 {
		t.Fatalf("cold committed search did not retain bounded content preselection: %#v", cold.Stats)
	}

	cacheDir := t.TempDir()
	if _, hit, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{
		Profile: ProfileSyntaxOnly,
	}, cacheDir); err != nil {
		t.Fatal(err)
	} else if hit {
		t.Fatal("first preindex unexpectedly hit cache")
	}
	warm, err := SearchRepository(t.Context(), repo, "test", queryText, SearchOptions{
		Profile: ProfileSyntaxOnly, TopK: 10, MaxIndexedFiles: 1, CacheDir: cacheDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if warm.Stats.FilesIndexed != eligibleFiles {
		t.Fatalf("warm committed preselection indexed %d files, want every one of %d eligible files", warm.Stats.FilesIndexed, eligibleFiles)
	}
	if warm.Stats.PreselectionBackend != "git-tree-grep" || warm.Stats.PreselectionPasses != 1 ||
		warm.Stats.PreselectionFilesExamined != warm.Stats.FilesScanned {
		t.Fatalf("warm Git scan was not represented honestly: %#v", warm.Stats)
	}
	if warm.Stats.FilesContentRead != 0 || warm.Stats.QueryFilesRead != eligibleFiles {
		t.Fatalf("warm committed query did not use batched preselection and bounded hydration: %#v", warm.Stats)
	}
	if len(warm.Results) == 0 || !strings.HasPrefix(warm.Results[0].FilePath, "src/match_") {
		t.Fatalf("expanded-term matches were lost: %#v", warm.Results)
	}
}

func TestIdentifierUsagePostingsAvoidRepositoryWideReads(t *testing.T) {
	definition := SymbolRecord{ID: "definition", Name: "cacheRefreshPolicy", FilePath: "src/policy.ts", StartLine: 1, EndLine: 1}
	sourcePath := "src/consumer.ts"
	testPath := "tests/consumer.test.ts"
	languages := map[string]string{definition.FilePath: "typescript", sourcePath: "typescript", testPath: "typescript"}
	for index := 0; index < 100; index++ {
		languages[fmt.Sprintf("noise/file_%03d.ts", index)] = "typescript"
	}
	contents := map[string]string{
		definition.FilePath: "export const cacheRefreshPolicy = 'eager'\n",
		sourcePath:          "export function applyPolicy() { install(cacheRefreshPolicy) }\n",
		testPath:            "test('policy', () => expect(cacheRefreshPolicy).toBeTruthy())\n",
	}
	reads := 0
	selector := func(_ context.Context, identifiers []string) ([]string, bool) {
		if len(identifiers) != 1 || identifiers[0] != definition.Name {
			t.Fatalf("usage identifiers = %#v", identifiers)
		}
		return []string{testPath, definition.FilePath, sourcePath}, true
	}
	got := expandIdentifierUsageCandidates(
		t.Context(), []searchCandidate{{score: 30, result: SearchResult{SymbolID: definition.ID}}},
		buildSearchQuery("cache refresh policy behavior"), map[string]SymbolRecord{definition.ID: definition}, nil,
		func(path string) (string, bool) { reads++; content, ok := contents[path]; return content, ok },
		languages, SearchOptions{ContextLines: 2, MaxSnippetLines: 20}, selector,
	)
	if reads != 3 {
		t.Fatalf("usage postings read %d files, want only the three matched files out of %d", reads, len(languages))
	}
	if len(got) < 2 || got[0].result.FilePath != sourcePath || got[1].result.FilePath != testPath {
		t.Fatalf("query-aware usage ranking = %#v", got)
	}
}

func TestSearchPathPriorIsQueryAwareAcrossArtifacts(t *testing.T) {
	plain := buildSearchQuery("cache refresh policy")
	explicit := buildSearchQuery("cache refresh policy tests documentation generated")
	source := searchPathPrior(plain, "src/policy.ts")
	for _, artifact := range []string{"tests/policy.test.ts", "docs/policy.md", "generated/policy_generated.go"} {
		if got := searchPathPrior(plain, artifact); got >= source {
			t.Fatalf("artifact %q prior = %v, want below source prior %v", artifact, got, source)
		}
	}
	for _, artifact := range []string{"tests/policy.test.ts", "docs/policy.md", "generated/policy_generated.go"} {
		if got := searchPathPrior(explicit, artifact); got != 0 {
			t.Fatalf("explicit artifact query retained a prior for %q: %v", artifact, got)
		}
	}
	for _, test := range []struct {
		query    string
		artifact string
	}{
		{query: "cache fixtures", artifact: "fixtures/policy.json"},
		{query: "cache guides", artifact: "docs/guides/policy.md"},
		{query: "cache examples", artifact: "docs/examples/policy.md"},
		{query: "cache generators", artifact: "generated/policy_generated.go"},
	} {
		if got := searchPathPrior(buildSearchQuery(test.query), test.artifact); got != 0 {
			t.Fatalf("plural artifact query %q retained a prior for %q: %v", test.query, test.artifact, got)
		}
	}
}

// TestSearchQuerySuppliedReadsWrittenWordsOnly pins the rule that class intent comes from the
// words a caller wrote, never from fragments the tokenizer mined out of an identifier they
// merely mentioned. Without it, quoting a reproduction file name in a bug report silently
// switched off the demotion for that whole class of file.
func TestSearchQuerySuppliedReadsWrittenWordsOnly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		query string
		term  string
		want  bool
	}{
		{"standalone word is intent", "please add a test for this", "test", true},
		{"word inside a path is intent", "the bug is in docs/api.md", "docs", true},
		{"snake_case segment is intent", "read_bytes_limit is wrong", "bytes", true},
		{"camelCase fragment is not intent", "NamedByteArrayTest.java reproduces it", "test", false},
		{"identifier suffix is not intent", "parseSpec returns nil", "spec", false},
		{"morphological variant is not intent", "documented behaviour differs", "documentation", false},
		{"unrelated query has no intent", "the timezone shift is wrong", "test", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := searchQuerySupplied(buildSearchQuery(test.query), test.term); got != test.want {
				t.Fatalf("searchQuerySupplied(%q, %q) = %v, want %v", test.query, test.term, got, test.want)
			}
		})
	}
}

// TestSearchPathPriorTreatsRegressionAsBehaviour guards the word-sense call: "a regression" in a
// bug report is behaviour that broke, not a request for the regression suite, so it must not
// switch off the test-artifact demotion.
func TestSearchPathPriorTreatsRegressionAsBehaviour(t *testing.T) {
	t.Parallel()
	regression := buildSearchQuery("nested fallback routes regression with merge")
	if got := searchPathPrior(regression, "src/routing/tests/fallback.rs"); got >= 0 {
		t.Fatalf("test artifact prior under a regression report = %v, want a demotion", got)
	}
	asked := buildSearchQuery("add a regression test for nested fallback routes")
	if got, plain := searchPathPrior(asked, "src/routing/tests/fallback.rs"),
		searchPathPrior(asked, "src/routing/fallback.rs"); got != plain {
		t.Fatalf("explicit test request kept a demotion: %v vs %v for a non-test sibling", got, plain)
	}
}

// TestAttributeSearchRegionPrefersInnerCallable covers the container-attribution rule: a matching
// region that does not contain the declaration it is named after is credited to the smallest
// callable it actually lies in, so a huge class cannot lend its name score to every region in it.
func TestAttributeSearchRegionPrefersInnerCallable(t *testing.T) {
	t.Parallel()
	container := SymbolRecord{ID: "c", Kind: "class", Name: "Calculation", StartLine: 1, EndLine: 400}
	method := SymbolRecord{ID: "m", Kind: "method", Name: "roundDown", StartLine: 200, EndLine: 240}
	fn := SymbolRecord{ID: "f", Kind: "function", Name: "helper", StartLine: 20, EndLine: 60}
	symbols := []SymbolRecord{container, method, fn}
	lines := make([]string, 400)
	for index := range lines {
		lines[index] = "// filler"
	}
	lines[29] = "// rounddown needle"  // inside `helper`
	lines[219] = "// rounddown needle" // inside `roundDown`
	q := buildSearchQuery("rounddown needle")

	for _, test := range []struct {
		name       string
		start, end int
		symbol     SymbolRecord
		want       string
	}{
		{"region inside a member is credited to the member", 210, 230, container, "m"},
		// The region opens on `class Calculation` itself, so the name really did match
		// here — it keeps the container even though it also spans an inner callable.
		{"region holding the declaration keeps the container", 1, 60, container, "c"},
		{"region with no inner callable keeps the container", 300, 340, container, "c"},
		{"a callable is already the smallest unit", 30, 50, fn, "f"},
		{"a symbol-less region is untouched", 210, 230, SymbolRecord{}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := attributeSearchRegion(q, lines, test.start, test.end, test.symbol, symbols)
			if got.ID != test.want {
				t.Fatalf("attributeSearchRegion(%d-%d, %q) = %q, want %q",
					test.start, test.end, test.symbol.ID, got.ID, test.want)
			}
		})
	}
}

func TestSearchRepositoryKeepsUntrackedFiles(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	for index := 0; index < 20; index++ {
		write(t, repo, fmt.Sprintf("src/noise_%02d.go", index), fmt.Sprintf("package source\nfunc Noise%d() int { return %d }\n", index, index))
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "tracked source")
	write(t, repo, "src/draft.go", "package source\nfunc DraftNeedle() bool { return true }\n")

	response, err := SearchRepository(t.Context(), repo, "test", "DraftNeedle", SearchOptions{
		Worktree:        true,
		Profile:         ProfileSyntaxOnly,
		TopK:            5,
		MaxIndexedFiles: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 || response.Results[0].SymbolName != "DraftNeedle" {
		t.Fatalf("git preselection lost an untracked edit: %#v", response.Results)
	}
}

func TestSearchRepositoryGitPreselectionKeepsFallbackTiers(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "noise/selected.go", "package noise\n// sentinel alphaone\nfunc SelectedNoise() {}\n")
	write(t, repo, "targets/morph.go", "package targets\n// configure\nfunc MorphTarget() {}\n")
	write(t, repo, "targets/fragment.go", "package targets\n// profile\nfunc FragmentTarget() {}\n")
	write(t, repo, "targets/direct.go", "package targets\n// tenthdirect\nfunc DirectTarget() {}\n")
	fillerDir := filepath.Join(repo, "filler")
	if err := os.MkdirAll(fillerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < minGitGrepPreselectionFiles-4; index++ {
		path := filepath.Join(fillerDir, fmt.Sprintf("file_%05d.go", index))
		if err := os.WriteFile(path, []byte("package filler\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "large tracked worktree")

	tests := []struct {
		name       string
		query      string
		targetPath string
	}{
		{name: "morphology", query: "configuring sentinel", targetPath: "targets/morph.go"},
		{name: "code fragment", query: "RenderProfileButton sentinel", targetPath: "targets/fragment.go"},
		{
			name:       "saturated direct term",
			query:      "AlphaOne BetaTwo GammaThree DeltaFour EpsilonFive ZetaSix EtaSeven ThetaEight ninthdirect tenthdirect",
			targetPath: "targets/direct.go",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := SearchRepository(t.Context(), repo, "test", test.query, SearchOptions{
				Worktree:        true,
				Profile:         ProfileSyntaxOnly,
				TopK:            10,
				MaxIndexedFiles: 2,
				DisableCache:    true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.Stats.FilesScanned != minGitGrepPreselectionFiles {
				t.Fatalf("files scanned = %d, want %d", response.Stats.FilesScanned, minGitGrepPreselectionFiles)
			}
			if response.Stats.FilesContentRead <= 0 || response.Stats.FilesContentRead > 8 {
				t.Fatalf("git preselection read %d files, want 1..8", response.Stats.FilesContentRead)
			}
			found := false
			for _, result := range response.Results {
				found = found || result.FilePath == test.targetPath
			}
			if !found {
				t.Fatalf("git preselection lost %s for query %q: %#v", test.targetPath, test.query, response.Results)
			}
		})
	}
}

func TestSearchGitPreselectionThresholdTargetsLargeWorktrees(t *testing.T) {
	if shouldUseGitGrepPreselection(true, minGitGrepPreselectionFiles-1) {
		t.Fatal("small worktree selected git-grep accelerator")
	}
	if !shouldUseGitGrepPreselection(true, minGitGrepPreselectionFiles) {
		t.Fatal("large worktree did not select git-grep accelerator")
	}
	if shouldUseGitGrepPreselection(false, minGitGrepPreselectionFiles*2) {
		t.Fatal("immutable tree selected worktree-only accelerator")
	}
}

func TestSearchGitGrepPatternsCoverGoUnicodeLoweringOrFallBack(t *testing.T) {
	patterns, safe := searchGitGrepPatterns([]string{"issueneedle", "kernelneedle"})
	if !safe {
		t.Fatal("small Unicode-lowering pattern set unexpectedly required exhaustive fallback")
	}
	if !containsString(patterns, "İssueneedle") {
		t.Fatalf("Git patterns do not cover Go dotted-I lowering: %#v", patterns)
	}
	if _, safe := searchGitGrepPatterns([]string{strings.Repeat("i", 20)}); safe {
		t.Fatal("combinatorial Unicode pattern expansion did not fail closed to exhaustive search")
	}
}

func TestScoreSearchPathsStopsDispatchAfterCancellation(t *testing.T) {
	paths := make([]string, 10_000)
	for index := range paths {
		paths[index] = fmt.Sprintf("src/file_%05d.go", index)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	files := scoreSearchPaths(ctx, paths, 8, func(path string) (searchFileCandidate, bool) {
		if calls.Add(1) == 1 {
			cancel()
		}
		return searchFileCandidate{path: path, score: 1}, true
	})
	if got := calls.Load(); got > 8 {
		t.Fatalf("canceled dispatch scored %d paths, want at most one in-flight path per worker", got)
	}
	if len(files) > 8 {
		t.Fatalf("canceled dispatch returned %d files, want at most one per worker", len(files))
	}
}

func TestSearchTermMatcherIsCaseInsensitiveAndFindsOverlaps(t *testing.T) {
	terms := []string{"list", "listitem", "secondaryaction", "missing"}
	matched := newSearchTermMatcher(terms).match("export const ListItemSecondaryAction = true")
	for _, index := range []int{0, 1, 2} {
		if !matched[index] {
			t.Fatalf("term %q was not matched: %#v", terms[index], matched)
		}
	}
	if matched[3] {
		t.Fatalf("absent term matched: %#v", matched)
	}
}

func TestSearchPreselectionPatternsPreferCodeShapedTerms(t *testing.T) {
	query := buildSearchQuery("refactor RenderProfileButton documentation behavior with trailingAction")
	patterns := searchPreselectionPatterns(query)
	if !containsString(patterns, "renderprofilebutton") || !containsString(patterns, "trailingaction") {
		t.Fatalf("preselection patterns = %#v", patterns)
	}
	fragmentCount := 0
	for _, fragment := range []string{"render", "profile", "button", "trailing", "action"} {
		if containsString(patterns, fragment) {
			fragmentCount++
		}
	}
	if fragmentCount == 0 || fragmentCount > 2 {
		t.Fatalf("bounded compound-fragment fallback count = %d, patterns = %#v", fragmentCount, patterns)
	}
	if len(patterns) > 12 {
		t.Fatalf("preselection pattern count = %d", len(patterns))
	}
}

func TestSearchPreselectionPatternsReserveMorphologicalFallbacks(t *testing.T) {
	patterns := searchPreselectionPatterns(buildSearchQuery("configuring serializer behavior"))
	if !containsString(patterns, "configuring") {
		t.Fatalf("direct query term missing from preselection: %#v", patterns)
	}
	if !containsString(patterns, "configur") && !containsString(patterns, "configure") {
		t.Fatalf("morphological fallback missing from preselection: %#v", patterns)
	}
	if len(patterns) > 12 {
		t.Fatalf("preselection pattern count = %d", len(patterns))
	}
}

func TestGitGrepPreselectionBoundsLargeRepoPatternFanout(t *testing.T) {
	patterns := searchGitGrepPreselectionPatterns(buildSearchQuery(
		"scheduled automation computer asleep misleading status catchup durable project memory",
	))
	if len(patterns) != 6 {
		t.Fatalf("git-grep patterns = %#v, want six", patterns)
	}
	for _, meaningful := range []string{"automation", "scheduled", "computer"} {
		if !containsString(patterns, meaningful) {
			t.Fatalf("high-priority term %q missing: %#v", meaningful, patterns)
		}
	}
	derived := false
	query := buildSearchQuery("scheduled automation computer asleep misleading status catchup durable project memory")
	for _, pattern := range patterns {
		if weight, exists := query.weights[pattern]; exists && weight > 0 && weight < 1 {
			derived = true
		}
	}
	if !derived {
		t.Fatalf("git-grep cap dropped every morphological fallback: %#v", patterns)
	}
}

func TestSearchPreselectionPatternsPreserveDirectTermsAtCapacity(t *testing.T) {
	plainTerms := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}
	plainPatterns := searchPreselectionPatterns(buildSearchQuery(strings.Join(plainTerms, " ")))
	for _, direct := range plainTerms {
		if !containsString(plainPatterns, direct) {
			t.Fatalf("plain query term %q missing from saturated preselection: %#v", direct, plainPatterns)
		}
	}

	query := buildSearchQuery("AlphaOne BetaTwo GammaThree DeltaFour EpsilonFive ZetaSix EtaSeven ThetaEight ninthdirect tenthdirect")
	patterns := searchPreselectionPatterns(query)
	for _, direct := range []string{"ninthdirect", "tenthdirect"} {
		if !containsString(patterns, direct) {
			t.Fatalf("direct query term %q missing from saturated preselection: %#v", direct, patterns)
		}
	}
	if len(patterns) > 12 {
		t.Fatalf("preselection pattern count = %d", len(patterns))
	}
}

func TestSearchPathScoreIgnoresDerivedURLAndIdentifierFragments(t *testing.T) {
	query := buildSearchQuery("https://example.com/packages/ui-kit RenderProfileButton")
	if got := pathSearchScore(query, "packages/ui-kit/src/unrelated.js"); got != 0 {
		t.Fatalf("derived URL fragments created path evidence: %v", got)
	}
	if got := pathSearchScore(query, "packages/renderprofilebutton/implementation.js"); got == 0 {
		t.Fatal("original compound identifier did not create path evidence")
	}
}

func TestSearchTokenVariantsKeepQualifiedCompoundIdentifiers(t *testing.T) {
	variants := searchTokenVariants("d.cc.NewServiceConfig")
	for _, want := range []string{"d.cc.newserviceconfig", "newserviceconfig", "new", "service", "config"} {
		if !containsString(variants, want) {
			t.Fatalf("variant %q missing from %#v", want, variants)
		}
	}
}

func TestSearchQueryPreservesPunctuatedLanguageNames(t *testing.T) {
	for query, want := range map[string]string{"C++": "c++", "C#": "c#", "F#": "f#"} {
		t.Run(query, func(t *testing.T) {
			got := buildSearchQuery(query)
			if !got.termSet[want] {
				t.Fatalf("query %q terms = %#v, want %q", query, got.terms, want)
			}
		})
	}
	for _, query := range []string{"++", "#", ":-"} {
		if got := buildSearchQuery(query); len(got.terms) != 0 {
			t.Fatalf("punctuation-only query %q produced terms %#v", query, got.terms)
		}
	}
}

func TestCodeLikeSearchTokenIgnoresProsePunctuation(t *testing.T) {
	for _, token := range []string{"documentation.", "Currently,", "spaces."} {
		if codeLikeSearchToken(token) {
			t.Fatalf("prose token %q classified as code", token)
		}
	}
	for _, token := range []string{"NewServiceConfig", "resolver_conn_wrapper", "foo/bar.go", "--head", "DOM", "C++", "C#"} {
		if !codeLikeSearchToken(token) {
			t.Fatalf("code token %q was not classified as code", token)
		}
	}
}

func TestCodeLikeSearchWeightDoesNotOverweightShortAcronyms(t *testing.T) {
	if got := codeLikeSearchWeight("DOM"); got >= 2 {
		t.Fatalf("short acronym weight = %v", got)
	}
	if got := codeLikeSearchWeight("NewServiceConfig"); got <= 2 {
		t.Fatalf("compound identifier weight = %v", got)
	}
}

func TestDiverseSelectionDoesNotSpendBudgetOnClones(t *testing.T) {
	candidates := []searchCandidate{
		{score: 10, result: SearchResult{FilePath: "bench/a/item.go", StartLine: 1, EndLine: 2, SymbolName: "same"}},
		{score: 9.9, result: SearchResult{FilePath: "bench/b/item.go", StartLine: 1, EndLine: 2, SymbolName: "same"}},
		{score: 8, result: SearchResult{FilePath: "src/implementation.go", StartLine: 1, EndLine: 2, SymbolName: "implementation"}},
	}
	selected := selectDiverseCandidates(candidates, 2, 3)
	if len(selected) != 2 || selected[1].result.SymbolName != "implementation" {
		t.Fatalf("clone consumed diversity budget: %#v", selected)
	}
}

func TestDiverseSelectionCoversFilesBeforeAddingRegions(t *testing.T) {
	candidates := []searchCandidate{
		{score: 10, result: SearchResult{FilePath: "src/large.go", StartLine: 1, EndLine: 10, SymbolName: "first"}},
		{score: 9.9, result: SearchResult{FilePath: "src/large.go", StartLine: 30, EndLine: 40, SymbolName: "second"}},
		{score: 9.8, result: SearchResult{FilePath: "src/large.go", StartLine: 60, EndLine: 70, SymbolName: "third"}},
		{score: 8, result: SearchResult{FilePath: "src/related.go", StartLine: 1, EndLine: 10, SymbolName: "related"}},
	}
	selected := selectDiverseCandidates(candidates, 3, 3)
	if len(selected) != 3 {
		t.Fatalf("selected %d candidates, want 3", len(selected))
	}
	if selected[0].result.FilePath != "src/large.go" || selected[1].result.FilePath != "src/related.go" {
		t.Fatalf("first discovery pass did not cover distinct files: %#v", selected)
	}
	if selected[2].result.SymbolName != "second" {
		t.Fatalf("second region was not restored after file coverage: %#v", selected)
	}
}

func TestDiverseSelectionDoesNotBuryExactSameFileUsages(t *testing.T) {
	candidates := []searchCandidate{
		{score: 88, result: SearchResult{FilePath: "src/snippets.ts", StartLine: 1, EndLine: 5, SymbolName: "guardDefinition"}},
		{score: 58, result: SearchResult{FilePath: "src/snippets.ts", StartLine: 20, EndLine: 25, SymbolName: "guardUsageOne"}},
		{score: 40, result: SearchResult{FilePath: "src/snippets.ts", StartLine: 40, EndLine: 45, SymbolName: "guardUsageTwo"}},
		{score: 10, result: SearchResult{FilePath: "src/unrelated.ts", StartLine: 1, EndLine: 5, SymbolName: "fragment"}},
	}
	selected := selectDiverseCandidates(candidates, 4, 3)
	if len(selected) != 4 {
		t.Fatalf("selected %d candidates, want 4", len(selected))
	}
	for index, want := range []string{"guardDefinition", "guardUsageOne", "guardUsageTwo"} {
		if selected[index].result.SymbolName != want {
			t.Fatalf("rank %d = %q, want %q: %#v", index+1, selected[index].result.SymbolName, want, selected)
		}
	}
}

func TestSearchPathPriorPrefersProductCodeUnlessWorkflowRequested(t *testing.T) {
	issue := buildSearchQuery("render account profile")
	if source, workflow := searchPathPrior(issue, "src/profile/render.go"), searchPathPrior(issue, ".github/workflows/release.yml"); source <= workflow {
		t.Fatalf("source prior %v did not exceed workflow prior %v", source, workflow)
	}
	workflowIssue := buildSearchQuery("fix CI workflow pipeline")
	if got := searchPathPrior(workflowIssue, ".github/workflows/test.yml"); got < 0 {
		t.Fatalf("explicit workflow query was penalized: %v", got)
	}
}

func TestSearchResultContextBudgetPreservesDiverseFocusedResults(t *testing.T) {
	results := make([]SearchResult, 10)
	for index := range results {
		results[index] = SearchResult{
			Rank:             index + 1,
			FilePath:         fmt.Sprintf("src/component_%d.go", index),
			StartLine:        1,
			EndLine:          40,
			FocusLine:        20,
			SnippetStartLine: 1,
			SnippetEndLine:   40,
			Signature:        "func HandleConfigurationRequestWithManyArguments(input string, output string, retries int) error",
			Signals:          []string{"body", "exact-code-token"},
			Snippet:          strings.Repeat("configuration request handler with implementation detail\n", 40),
		}
	}
	const budget = 4096
	compacted, resultBytes, dropped, truncated := fitSearchResultsToBudget(results, buildSearchQuery("configuration handler"), budget)
	encoded, err := json.Marshal(compacted)
	if err != nil {
		t.Fatal(err)
	}
	if resultBytes != len(encoded) || resultBytes > budget {
		t.Fatalf("budget accounting = %d, encoded = %d, budget = %d", resultBytes, len(encoded), budget)
	}
	if len(compacted) < 2 || compacted[0].FilePath != results[0].FilePath {
		t.Fatalf("budgeting lost ranked diversity: %#v", compacted)
	}
	if dropped != len(results)-len(compacted) || truncated == 0 {
		t.Fatalf("unexpected budget stats: dropped=%d truncated=%d results=%d", dropped, truncated, len(compacted))
	}
	for _, result := range compacted {
		if result.FocusLine < result.SnippetStartLine || result.FocusLine > result.SnippetEndLine {
			t.Fatalf("focus line fell outside compacted snippet: %#v", result)
		}
	}
}

func TestCompactSearchResultKeepsLargestFocusedWindowThatFits(t *testing.T) {
	lines := []string{
		"first context line with enough detail to consume bytes",
		"second context line with enough detail to consume bytes",
		"focus configuration handler with enough detail to consume bytes",
		"fourth context line with enough detail to consume bytes",
		"fifth context line with enough detail to consume bytes",
	}
	result := SearchResult{
		Rank:             1,
		FilePath:         "src/configuration.go",
		StartLine:        1,
		EndLine:          len(lines),
		FocusLine:        3,
		SnippetStartLine: 1,
		SnippetEndLine:   len(lines),
		Signature:        "func HandleConfiguration() error",
		Signals:          []string{"body"},
		Snippet:          strings.Join(lines, "\n"),
	}
	threeLine := result
	threeLine.SnippetStartLine = 2
	threeLine.SnippetEndLine = 4
	threeLine.Snippet = strings.Join(lines[1:4], "\n")
	budget := serializedSearchResultBytes(threeLine)
	if serializedSearchResultBytes(result) <= budget {
		t.Fatal("test fixture full snippet unexpectedly fits compacted budget")
	}

	got, size := compactSearchResultToBytes(result, buildSearchQuery("configuration handler"), budget)
	if size > budget {
		t.Fatalf("compacted size = %d, budget = %d", size, budget)
	}
	if got.SnippetStartLine != 2 || got.SnippetEndLine != 4 || got.Snippet != threeLine.Snippet {
		t.Fatalf("compacted snippet = lines %d-%d %q, want balanced lines 2-4", got.SnippetStartLine, got.SnippetEndLine, got.Snippet)
	}
}

func TestTruncateSearchTextNeverExceedsByteBudget(t *testing.T) {
	query := buildSearchQuery("configuration handler")
	values := []string{
		strings.Repeat("å", 20) + " configuration handler " + strings.Repeat("界", 20),
		"configuration handler " + strings.Repeat("界", 20),
		strings.Repeat("å", 20) + " configuration handler",
	}
	for _, value := range values {
		for budget := 0; budget <= 64; budget++ {
			got := truncateSearchText(value, budget, query)
			if len(got) > budget {
				t.Fatalf("budget %d produced %d bytes: %q", budget, len(got), got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("budget %d split UTF-8: %q", budget, got)
			}
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestExpandGraphCandidatesSkipsOutOfRangeSymbolLines(t *testing.T) {
	seeds := []searchCandidate{{
		result: SearchResult{SymbolID: "seed", FilePath: "seed.go"},
		score:  5,
	}}
	relations := []RelationRecord{{
		FromID:     "seed",
		ToID:       "target",
		Type:       "CALLS",
		Confidence: 1.0,
	}}
	symbolsByID := map[string]SymbolRecord{
		"seed":   {ID: "seed", Name: "Seed", FilePath: "seed.go", StartLine: 1, EndLine: 3},
		"target": {ID: "target", Name: "Target", FilePath: "target.go", StartLine: 100, EndLine: 101},
	}
	read := func(path string) (string, bool) {
		if path == "target.go" {
			return "line1\nline2\nline3", true
		}
		return "", false
	}
	options := SearchOptions{MaxRegionLines: 40, MaxSnippetLines: 40}

	// Pre-fix, clampRegion returns (0,0) for the out-of-range target and the
	// unguarded lines[snippetStart-1:snippetEnd] slice panics with
	// "slice bounds out of range [-1:]".
	out := expandGraphCandidates(seeds, searchQuery{}, relations, symbolsByID, nil, read, nil, options)

	for _, candidate := range out {
		if candidate.result.SymbolID == "target" {
			t.Fatalf("expected out-of-range target candidate to be skipped, got %+v", candidate.result)
		}
	}
}

func TestExpandGraphCandidatesIncludesInRangeSymbol(t *testing.T) {
	seeds := []searchCandidate{{
		result: SearchResult{SymbolID: "seed", FilePath: "seed.go"},
		score:  5,
	}}
	relations := []RelationRecord{{
		FromID:     "seed",
		ToID:       "target",
		Type:       "CALLS",
		Confidence: 1.0,
	}}
	symbolsByID := map[string]SymbolRecord{
		"seed":   {ID: "seed", Name: "Seed", FilePath: "seed.go", StartLine: 1, EndLine: 3},
		"target": {ID: "target", Name: "Target", FilePath: "target.go", StartLine: 2, EndLine: 3},
	}
	read := func(path string) (string, bool) {
		if path == "target.go" {
			return "line1\nline2\nline3", true
		}
		return "", false
	}
	options := SearchOptions{MaxRegionLines: 40, MaxSnippetLines: 40}

	out := expandGraphCandidates(seeds, searchQuery{}, relations, symbolsByID, nil, read, nil, options)

	found := false
	for _, candidate := range out {
		if candidate.result.SymbolID == "target" {
			found = true
			if candidate.result.Snippet == "" {
				t.Fatalf("expected in-range target candidate to carry a snippet")
			}
		}
	}
	if !found {
		t.Fatalf("expected in-range target candidate to be included, got %d candidates", len(out))
	}
}

func TestExpandGraphCandidatesClampsNonPositiveRegionLines(t *testing.T) {
	seeds := []searchCandidate{{
		result: SearchResult{SymbolID: "seed", FilePath: "seed.go"},
		score:  5,
	}}
	relations := []RelationRecord{{
		FromID:     "seed",
		ToID:       "target",
		Type:       "CALLS",
		Confidence: 1.0,
	}}
	symbolsByID := map[string]SymbolRecord{
		"seed":   {ID: "seed", Name: "Seed", FilePath: "seed.go", StartLine: 1, EndLine: 3},
		"target": {ID: "target", Name: "Target", FilePath: "target.go", StartLine: 2, EndLine: 3},
	}
	read := func(path string) (string, bool) {
		if path == "target.go" {
			return "line1\nline2\nline3", true
		}
		return "", false
	}

	for _, maxRegionLines := range []int{-1, 0} {
		// Pre-clamp, MaxRegionLines <= -1 shrinks end below start
		// (end = start+MaxRegionLines-1) and the snippet slice panics with
		// "slice bounds out of range" (low > high).
		options := SearchOptions{MaxRegionLines: maxRegionLines, MaxSnippetLines: -1}
		out := expandGraphCandidates(seeds, searchQuery{}, relations, symbolsByID, nil, read, nil, options)

		found := false
		for _, candidate := range out {
			if candidate.result.SymbolID == "target" {
				found = true
				if candidate.result.SnippetEndLine < candidate.result.SnippetStartLine {
					t.Fatalf("MaxRegionLines=%d produced inverted snippet region %d-%d",
						maxRegionLines, candidate.result.SnippetStartLine, candidate.result.SnippetEndLine)
				}
				if candidate.result.Snippet == "" {
					t.Fatalf("MaxRegionLines=%d: expected target candidate to carry a snippet", maxRegionLines)
				}
			}
		}
		if !found {
			t.Fatalf("MaxRegionLines=%d: expected in-range target candidate to be included, got %d candidates",
				maxRegionLines, len(out))
		}
	}
}

func TestSearchGraphCallerBoostsCountDistinctProductionCallers(t *testing.T) {
	symbolsByID := map[string]SymbolRecord{
		"impl":       {ID: "impl", Name: "checkQuorum", FilePath: "store/quorum.go"},
		"caller1":    {ID: "caller1", Name: "replicateObject", FilePath: "store/replicate.go"},
		"caller2":    {ID: "caller2", Name: "storeQuarantine", FilePath: "store/quarantine.go"},
		"testfn":     {ID: "testfn", Name: "TestQuorum", FilePath: "test/workloads/push_quorum.go"},
		"unitfile":   {ID: "unitfile", Name: "TestCheck", FilePath: "store/quorum_test.go"},
		"rubyspec":   {ID: "rubyspec", Name: "checks quorum", FilePath: "spec/store/quorum_spec.rb"},
		"pythontest": {ID: "pythontest", Name: "test_quorum", FilePath: "store/quorum_test.py"},
		"dottedtest": {ID: "dottedtest", Name: "ChecksQuorum", FilePath: "Store.Tests/Quorum.cs"},
	}
	relations := []RelationRecord{
		{FromID: "caller1", ToID: "impl", Type: "CALLS", Confidence: 0.8},
		// Duplicate edge from the same caller must not count twice.
		{FromID: "caller1", ToID: "impl", Type: "CALLS", Confidence: 0.8},
		{FromID: "caller2", ToID: "impl", Type: "CALLS", Confidence: 0.92},
		// Test-path callers carry no production signal.
		{FromID: "testfn", ToID: "impl", Type: "CALLS", Confidence: 0.92},
		{FromID: "unitfile", ToID: "impl", Type: "CALLS", Confidence: 0.92},
		{FromID: "rubyspec", ToID: "impl", Type: "CALLS", Confidence: 0.92},
		{FromID: "pythontest", ToID: "impl", Type: "CALLS", Confidence: 0.92},
		{FromID: "dottedtest", ToID: "impl", Type: "CALLS", Confidence: 0.92},
		// Below the confidence floor.
		{FromID: "caller1", ToID: "caller2", Type: "CALLS", Confidence: 0.5},
		// Structural relations are not caller evidence.
		{FromID: "caller1", ToID: "caller2", Type: "CONTAINS", Confidence: 1.0},
		// Self-edges are ignored.
		{FromID: "impl", ToID: "impl", Type: "CALLS", Confidence: 1.0},
	}

	boosts := searchGraphCallerBoosts(relations, symbolsByID)

	want := searchCallerBoostUnit * math.Log2(3) // two distinct production callers
	if got := boosts["impl"]; math.Abs(got-want) > 1e-9 {
		t.Fatalf("impl boost = %v, want %v", got, want)
	}
	if _, ok := boosts["caller2"]; ok {
		t.Fatalf("low-confidence edge produced a boost: %v", boosts)
	}
}

func TestApplySearchCallerBoostsTagsAndCounts(t *testing.T) {
	candidates := []searchCandidate{
		{result: SearchResult{SymbolID: "impl"}, score: 10},
		{result: SearchResult{SymbolID: "other"}, score: 12},
		{result: SearchResult{}, score: 9},
	}
	boosted := applySearchCallerBoosts(candidates, map[string]float64{"impl": 3, "": 99})
	if boosted != 1 {
		t.Fatalf("boosted = %d, want 1", boosted)
	}
	if candidates[0].score != 13 || !containsString(candidates[0].result.Signals, "graph:callers") {
		t.Fatalf("boosted candidate = %+v", candidates[0])
	}
	if candidates[1].score != 12 || candidates[2].score != 9 {
		t.Fatalf("unboosted candidates changed: %+v", candidates[1:])
	}
}

func TestApplySearchCallerBoostsHalvesSparseBodyEvidence(t *testing.T) {
	candidates := []searchCandidate{
		{
			result: SearchResult{SymbolID: "impl", Signals: []string{"sparse-region"}},
			score:  10,
		},
		{
			result: SearchResult{SymbolID: "impl", Signals: []string{"sparse-region", "path"}},
			score:  10,
		},
	}

	boosted := applySearchCallerBoosts(candidates, map[string]float64{"impl": 4})

	if boosted != 2 {
		t.Fatalf("boosted = %d, want 2", boosted)
	}
	if candidates[0].score != 12 {
		t.Fatalf("sparse body-only score = %v, want 12", candidates[0].score)
	}
	if candidates[1].score != 14 {
		t.Fatalf("sparse path score = %v, want 14", candidates[1].score)
	}
}

func TestApplySearchCallerBoostsHalvesBodyDerivedSignals(t *testing.T) {
	for _, signals := range [][]string{
		{"body", "exact-code-token"},
		{"body", "all-query-terms"},
		{"body", "exact-code-token", "all-query-terms"},
	} {
		candidates := []searchCandidate{{
			result: SearchResult{SymbolID: "impl", Signals: signals},
			score:  10,
		}}
		if boosted := applySearchCallerBoosts(candidates, map[string]float64{"impl": 4}); boosted != 1 {
			t.Fatalf("signals %#v: boosted = %d, want 1", signals, boosted)
		}
		if candidates[0].score != 12 {
			t.Fatalf("signals %#v: score = %v, want 12 with half caller boost", signals, candidates[0].score)
		}
	}
}

func TestExpandGraphCandidatesDoesNotBoostZeroEvidenceHub(t *testing.T) {
	q := buildSearchQuery("quorum replication")
	seeds := []searchCandidate{{
		result: SearchResult{SymbolID: "seed"},
		score:  15,
	}}
	relations := []RelationRecord{{
		FromID:     "seed",
		ToID:       "hub",
		Type:       "CALLS",
		Confidence: 0.8,
	}}
	symbolsByID := map[string]SymbolRecord{
		"seed": {ID: "seed", Name: "replicateObject", FilePath: "replicate.go", StartLine: 1, EndLine: 1},
		"hub":  {ID: "hub", Name: "logValue", FilePath: "logging.go", StartLine: 1, EndLine: 3},
	}
	read := func(path string) (string, bool) {
		if path == "logging.go" {
			return "func logValue(value any) {\n\tfmt.Println(value)\n}", true
		}
		return "", false
	}

	candidates := expandGraphCandidates(
		seeds,
		q,
		relations,
		symbolsByID,
		map[string]float64{"hub": searchCallerBoostCap},
		read,
		map[string]string{"logging.go": "Go"},
		SearchOptions{},
	)

	if len(candidates) != 1 {
		t.Fatalf("graph candidates = %#v, want one hub", candidates)
	}
	want := 0.28*15 + 2.2*0.8
	if got := candidates[0].score; math.Abs(got-want) > 1e-9 {
		t.Fatalf("zero-evidence hub score = %v, want %v without caller boost", got, want)
	}
	if containsString(candidates[0].result.Signals, "graph:callers") {
		t.Fatalf("zero-evidence hub signals = %#v, must not include graph:callers", candidates[0].result.Signals)
	}
}

func TestExpandGraphCandidatesHalvesBodyOnlyCallerBoost(t *testing.T) {
	q := buildSearchQuery("quorum replication")
	seeds := []searchCandidate{{
		result: SearchResult{SymbolID: "seed"},
		score:  15,
	}}
	relations := []RelationRecord{{
		FromID:     "seed",
		ToID:       "hub",
		Type:       "CALLS",
		Confidence: 0.8,
	}}
	symbolsByID := map[string]SymbolRecord{
		"seed": {ID: "seed", Name: "replicateObject", FilePath: "replicate.go", StartLine: 1, EndLine: 1},
		"hub":  {ID: "hub", Name: "logValue", FilePath: "logging.go", StartLine: 1, EndLine: 3},
	}
	read := func(path string) (string, bool) {
		if path == "logging.go" {
			return "func logValue(value any) {\n\tfmt.Println(\"quorum replication\", value)\n}", true
		}
		return "", false
	}

	candidates := expandGraphCandidates(
		seeds,
		q,
		relations,
		symbolsByID,
		map[string]float64{"hub": searchCallerBoostCap},
		read,
		map[string]string{"logging.go": "Go"},
		SearchOptions{},
	)

	if len(candidates) != 1 {
		t.Fatalf("graph candidates = %#v, want one hub", candidates)
	}
	want := 0.55*15 + 2.2*0.8 + 0.5*searchCallerBoostCap
	if got := candidates[0].score; math.Abs(got-want) > 1e-9 {
		t.Fatalf("body-only hub score = %v, want %v with half caller boost", got, want)
	}
	if !containsString(candidates[0].result.Signals, "graph:callers") {
		t.Fatalf("body-only hub signals = %#v, want graph:callers", candidates[0].result.Signals)
	}
	if got := countSearchCandidatesWithSignal(candidates, "graph:callers"); got != 1 {
		t.Fatalf("boosted graph candidate count = %d, want 1", got)
	}
}

func TestSearchTestArtifactPathCommonLayouts(t *testing.T) {
	for _, path := range []string{
		"test/workloads/push_quorum.go",
		"store/quorum_test.go",
		"spec/store/quorum_spec.rb",
		"store/quorum_test.py",
		"Store.Tests/Quorum.cs",
		"src/__tests__/quorum.test.ts",
		"src/quorum.test",
		"src/quorum.spec",
	} {
		if !searchTestArtifactPath(path) {
			t.Errorf("searchTestArtifactPath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"internal/contest/score.go",
		"internal/latest/value.go",
		"internal/speculator/run.go",
		"internal/testingutil/helper.go",
		".github/workflows/test.yml",
	} {
		if searchTestArtifactPath(path) {
			t.Errorf("searchTestArtifactPath(%q) = true, want false", path)
		}
	}
}

func TestSearchRanksCalledImplementationAboveTestScaffolding(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "store/quorum.go", `package store

// checkQuorum validates that enough providers succeeded to meet quorum and
// availability-zone coverage requirements.
func checkQuorum(outcomes []int) bool {
	return len(outcomes) > 1
}
`)
	write(t, repo, "store/replicate.go", `package store

// replicateObject fans the object out to every target and fails when the
// cluster-wide quorum check fails.
func replicateObject(outcomes []int) bool {
	return checkQuorum(outcomes)
}
`)
	write(t, repo, "store/quarantine.go", `package store

// storeQuarantine stages a pack and verifies quorum before commit.
func storeQuarantine(outcomes []int) bool {
	return checkQuorum(outcomes)
}
`)
	write(t, repo, "store/repair.go", `package store

// repairScan re-verifies quorum for under-replicated objects.
func repairScan(outcomes []int) bool {
	return checkQuorum(outcomes)
}
`)
	// The test workload matches the query about as well as the implementation
	// does lexically (concept terms in path, type doc, and assertion doc), the
	// shape the entiredb feedback reported: without graph signal it outranks
	// the implementation, which has no production path/doc advantage — only
	// real callers.
	write(t, repo, "test/workloads/push_quorum.go", `package workloads

// PushQuorumWorkload drives a push and asserts replication behavior.
type PushQuorumWorkload struct{}

// Check asserts that the push met quorum requirements.
func (w PushQuorumWorkload) Check(outcomes []int) bool {
	return len(outcomes) >= 0
}
`)

	// Default options: the profile must resolve to fast so same-package CALLS
	// edges exist for caller-degree ranking.
	response, err := SearchRepository(t.Context(), repo, "test", "quorum replication", SearchOptions{
		Worktree: true,
		TopK:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Profile != string(ProfileFast) {
		t.Fatalf("default search profile = %q, want %q", response.Profile, ProfileFast)
	}
	implRank, testRank := 0, 0
	var implSignals []string
	for _, result := range response.Results {
		if result.SymbolName == "checkQuorum" && implRank == 0 {
			implRank = result.Rank
			implSignals = result.Signals
		}
		if strings.HasPrefix(result.FilePath, "test/") && testRank == 0 {
			testRank = result.Rank
		}
	}
	if implRank == 0 {
		t.Fatalf("checkQuorum missing from results: %#v", response.Results)
	}
	if !containsString(implSignals, "graph:callers") {
		t.Fatalf("checkQuorum signals = %#v, want graph:callers", implSignals)
	}
	if testRank != 0 && implRank > testRank {
		t.Fatalf("implementation ranked %d below test scaffolding at %d", implRank, testRank)
	}
	if response.Stats.CallerBoostedCandidates == 0 {
		t.Fatalf("stats did not record caller-boosted candidates: %#v", response.Stats)
	}
}

func TestSortSearchCandidatesBreaksLocationTiesBySemanticIdentity(t *testing.T) {
	location := SearchResult{FilePath: "same.go", StartLine: 10, EndLine: 20}
	candidates := []searchCandidate{
		{score: 5, result: func() SearchResult {
			result := location
			result.SymbolID = "symbol-b"
			return result
		}()},
		{score: 5, result: func() SearchResult {
			result := location
			result.SymbolID = "symbol-a"
			return result
		}()},
	}

	sortSearchCandidates(candidates)
	if candidates[0].result.SymbolID != "symbol-a" || candidates[1].result.SymbolID != "symbol-b" {
		t.Fatalf("location-tied candidates were not ordered by stable symbol identity: %#v", candidates)
	}

	left, right := searchCandidate{result: location}, searchCandidate{result: location}
	left.result.QualifiedName = "Alpha.Run"
	right.result.QualifiedName = "Beta.Run"
	if !searchCandidateLess(left, right) || searchCandidateLess(right, left) {
		t.Fatalf("qualified-name tie breaker is not deterministic: left=%#v right=%#v", left, right)
	}
}

// TestSearchRepositorySurvivesMaxSnippetLinesOne is a regression test for the
// --max-snippet-lines 1 crash: a same-container-neighbor result's FocusLine was set
// to neighbor.StartLine even when the emitted region (truncated by the tiny snippet
// budget) no longer contained it, so SearchResponse.Validate rejected the whole
// response with "invalid search result at rank N".
func TestSearchRepositorySurvivesMaxSnippetLinesOne(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "flow.go", `package flow

func alphaUniqueFocusTerm() {
	println("alpha")
}

func beta() {
	println("beta")
}

func gamma() {
	println("gamma")
}
`)

	response, err := SearchRepository(t.Context(), repo, "test", "alphaUniqueFocusTerm", SearchOptions{
		Worktree:        true,
		Profile:         ProfileSyntaxOnly,
		TopK:            5,
		MaxSnippetLines: 1,
	})
	if err != nil {
		t.Fatalf("SearchRepository with MaxSnippetLines=1 returned error: %v", err)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("MaxSnippetLines=1 produced an invalid response: %v", err)
	}
	for _, result := range response.Results {
		if result.FocusLine < result.StartLine || result.FocusLine > result.EndLine {
			t.Fatalf("result has out-of-range FocusLine: %#v", result)
		}
	}
}

// TestComputeSearchNeighborsFoldsDefinedTypeAndImplNeighbors was removed with the per-result
// neighbor fold it covered. The question it asked — "which repo-defined types is this hit
// written in terms of, and where are they" — is answered by the signature-type block
// (search_sigtypes_test.go), which resolves those types to their declarations WITH their
// fields and member signatures, is funded by displacing tail locators so it can never shrink a
// complete head body, and is gated off by default. Its exclusions are covered there too:
// external targets and test files cannot enter the block.

// Regression for the four-model field test on gohugoio/hugo#12171. All four models were given the
// query form our own doctrine mandates ("the bug in one sentence") and all four abandoned it: the
// gold site pageMap.getPagesInSection sat at position 87 of 101 while the head was five points wide.
// The discriminating fact was unscored — the gold NAME carries two of the query's domain terms
// (pages, section) where its competitors carry one — and an exact substring test scored the plural
// "sections" against the singular identifier as a miss.
func TestSearchNameTermCoverageIsPluralTolerantAndSaturates(t *testing.T) {
	t.Parallel()
	q := buildSearchQuery("parent pages collection filter by section path prefix")
	gold := SearchResult{QualifiedName: "pageMap.getPagesInSection"}
	rival := SearchResult{QualifiedName: "Site.Sections"}
	g := searchNameTermCoverage(gold, q, nil)
	r := searchNameTermCoverage(rival, q, nil)
	if !(g > r) {
		t.Fatalf("gold name coverage %v must exceed single-term rival %v", g, r)
	}
	// "sections" (plural, from the issue) must match "Section" inside the identifier.
	if !searchNameTokenMatchesTerm(searchTokenVariants("pageMap.getPagesInSection"), "sections") {
		t.Fatal("plural query term must match the singular identifier")
	}
	// Saturation: a name carrying many terms is not unboundedly better than one carrying three.
	many := SearchResult{QualifiedName: "parentPagesCollectionFilterSectionPathPrefix"}
	if got := searchNameTermCoverage(many, q, nil); got != 1 {
		t.Fatalf("coverage should saturate at 1, got %v", got)
	}
	// A name sharing nothing scores zero, so the signal cannot lift unrelated code.
	if got := searchNameTermCoverage(SearchResult{QualifiedName: "imaging.newFilterOpts"}, buildSearchQuery("zzz qqq"), nil); got != 0 {
		t.Fatalf("unrelated name must score 0, got %v", got)
	}
	// Short tokens must not match: a 2-char fragment appears in almost any identifier.
	if searchNameTokenMatchesTerm(searchTokenVariants("pageMap.getPagesInSection"), "in") {
		t.Fatal("short tokens must not count as name coverage")
	}
}

// TestSearchNameCoverageExpandsProseAbbreviations pins the entireio/cli regression: the prose
// sentence "the main authentication function that logs a user in" returned the LOGGING package,
// because "authentication" matched no identifier while "logs" matched RestoreLogsOnly. The name a
// developer writes is `auth`; the word a reporter writes is "authentication".
func TestSearchNameCoverageExpandsProseAbbreviations(t *testing.T) {
	t.Parallel()
	q := buildSearchQuery("the main authentication function that logs a user in")
	login := SearchResult{QualifiedName: "runLogin"}
	authFlag := SearchResult{QualifiedName: "addInsecureHTTPAuthFlag"}
	logs := SearchResult{QualifiedName: "ManualCommitStrategy.RestoreLogsOnly"}

	// Before the abbreviation table this was 0: "authentication" matched no token of any auth
	// identifier, so the one signal that exists to separate the head scored the correct answers
	// exactly as low as unrelated code.
	if got := searchNameTermCoverage(authFlag, q, nil); got == 0 {
		t.Fatal("an identifier spelled with the abbreviation must score for the prose spelling")
	}
	// Deliberately >=, not >. Both names carry exactly one of this query's terms, so at the
	// coverage layer they TIE; the table removes the auth side's zero, it does not by itself
	// outrank logging. Ordering the full query is BM25's job and is covered end to end by the
	// bench, not here — asserting > would be asserting something this function does not do.
	if a, l := searchNameTermCoverage(authFlag, q, nil), searchNameTermCoverage(logs, q, nil); a < l {
		t.Fatalf("auth identifier %v must not score below the logging identifier %v", a, l)
	}
	// runLogin scores, and the REASON is what this pins. It must match through "login" — the term
	// compound joining recovers from the split phrasal verb — and must NOT match through "logs".
	// Under the old substring test the situation was exactly inverted: there was no "login" term
	// at all, and runLogin scored because plural-stripped "logs" was found inside "runlogin", the
	// same accident that matched RestoreLogsOnly. Right answer, wrong reason, and a signal that
	// could not separate the two clusters.
	if got := searchNameTermCoverage(login, q, nil); got == 0 {
		t.Fatal("runLogin must match the recovered login term")
	}
	loginTokens := searchTokenVariants("runLogin")
	if !searchNameTokenMatchesTerm(loginTokens, "login") {
		t.Fatal("runLogin must match through the joined compound")
	}
	if searchNameTokenMatchesTerm(loginTokens, "logs") {
		t.Fatal("login must not match a logging term at token boundaries")
	}
	if !searchNameTokenMatchesTerm(searchTokenVariants("ManualCommitStrategy.RestoreLogsOnly"), "logs") {
		t.Fatal("a genuine logging identifier must still match the logging term")
	}
	if searchNameMatchesAbbreviation(searchTokenVariants("runLogin"), "authentication") {
		t.Fatal("the table must not have invented a login/authentication synonym")
	}
}

// TestSearchNameMatchesAbbreviationIsTokenScoped guards the false positives that a raw substring
// test would manufacture from short abbreviations.
func TestSearchNameMatchesAbbreviationIsTokenScoped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		term  string
		ident string
		want  bool
	}{
		{"long alias matches token prefix", "authentication", "runAuthenticated", true},
		{"long alias matches exact token", "configuration", "loadConfig", true},
		{"plural long form", "configurations", "loadConfig", true},
		{"plural ies long form", "repositories", "openRepo", true},
		{"long alias spans qualified name", "repository", "gitrepo.OpenCurrent", true},
		{"short alias matches whole token", "context", "withCtx", true},
		{"short alias matches whole token db", "database", "openDB", true},
		{"short alias rejects substring", "request", "frequencyTable", false},
		{"short alias rejects substring db", "database", "debugPrint", false},
		{"short alias rejects substring int", "integer", "interfaceBuilder", false},
		{"unmapped term never matches", "kubernetes", "kubeClient", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := searchNameMatchesAbbreviation(searchTokenVariants(tc.ident), tc.term)
			if got != tc.want {
				t.Fatalf("searchNameMatchesAbbreviation(%q, %q) = %v, want %v", tc.ident, tc.term, got, tc.want)
			}
		})
	}
}

// TestSearchCompoundJoinSeparablePhrasalVerbs pins the entireio/cli regression. The motivating
// sentence is "the main authentication function that logs a user in", where the phrasal verb is
// split around its object: `logs` and `in` are four tokens apart, so an adjacent-pair scan sees
// nothing. Before the joined term existed the query returned the LOGGING package with no auth in
// the top five.
func TestSearchCompoundJoinSeparablePhrasalVerbs(t *testing.T) {
	t.Parallel()
	q := buildSearchQuery("the main authentication function that logs a user in")
	if !q.termSet["login"] {
		t.Fatalf("separated phrasal verb must yield the joined term; got %v", q.terms)
	}
	// Added, never substituted: the split spelling has to survive for repos that use it.
	if !q.termSet["logs"] && !q.termSet["log"] {
		t.Fatalf("the split spelling must survive alongside the joined one; got %v", q.terms)
	}
}

func TestSearchCompoundJoins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
		want  string
		found bool
	}{
		{"adjacent phrasal verb", "cannot log in", "login", true},
		{"separated by determiner", "logs a user in", "login", true},
		{"separated, possessive object", "log the user out", "logout", true},
		{"inflected verb", "logging a user in", "login", true},
		{"past tense", "logged the user in", "login", true},
		{"adjacent noun compound", "the end point returns 404", "endpoint", true},
		{"other phrasal verb", "roll the migration back", "rollback", true},
		// The object-head requirement keeps an unrelated SEPARATED particle out: no determiner
		// directly after "log", so the gap scan must not reach the trailing "in".
		{"no object head, no separated join", "write every log entry in json", "login", false},
		// noun + preposition, not a phrasal verb. The determiner in front of "log" marks it as a
		// noun, which is the only cue English gives. Without this check the query returned
		// persistLogin, RecordLoginContext and runLogin at ranks 1-3.
		{"determiner marks a noun, not a phrasal verb", "write the log in json format", "login", false},
		// ...but the same words with a verb in front are the phrasal verb, and must still join.
		{"no determiner, still a phrasal verb", "the user cannot log in", "login", true},
		{"logging an error as JSON", "log the error in json", "login", false},
		{"logging a message as JSON", "log a message in json format", "login", false},
		{"logging as YAML", "logs the response in YAML", "login", false},
		{"logging as plain text", "log the message in plain text", "login", false},
		{"adjacent formatting preposition", "write log in JSON", "login", false},
		{"format with determiner", "log the message in a binary format", "login", false},
		{"custom format", "log the message in custom format", "login", false},
		{"formatting words elsewhere", "log the user in and return JSON", "login", true},
		{"login location", "log the user in from the browser", "login", true},
		{"check in binary files", "check in binary files", "checkin", true},
		{"check in a JSON file", "check in a JSON file", "checkin", true},
		{"checking a file format", "check the file in json format", "checkin", false},
		{"signing a request format", "sign the request in json format", "signin", false},
		{"checking YAML content", "check the response in YAML", "checkin", false},
		{"signing a custom encoding", "sign the request in custom encoding", "signin", false},
		{"separated check in", "check the file in and return JSON", "checkin", true},
		{"separated sign in", "sign the user in and return JSON", "signin", true},
		// Noun compounds are not separable; only the adjacent form counts.
		{"noun compound is not separable", "the end of the point", "endpoint", false},
		{"gap too wide", "logs every authenticated request payload in", "login", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildSearchQuery(tc.query).termSet[tc.want]
			if got != tc.found {
				t.Fatalf("buildSearchQuery(%q).termSet[%q] = %v, want %v", tc.query, tc.want, got, tc.found)
			}
		})
	}
}

func TestSearchQueryAbbreviationsIncludeWordVariants(t *testing.T) {
	t.Parallel()
	for query, alias := range map[string]string{
		"configurations": "config",
		"repositories":   "repo",
		"directories":    "dir",
		"authenticating": "auth",
	} {
		t.Run(query, func(t *testing.T) {
			q := buildSearchQuery(query)
			if !q.termSet[alias] {
				t.Fatalf("query %q must retrieve through %q; got %v", query, alias, q.terms)
			}
			if q.weights[alias] != searchAbbreviationTermWeight {
				t.Fatalf("inferred alias weight = %v, want %v", q.weights[alias], searchAbbreviationTermWeight)
			}
		})
	}
}
