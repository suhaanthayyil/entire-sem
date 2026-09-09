package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func repoIgnoredResponse(excluded int, sample []sem.RepoExclusion) sem.SearchResponse {
	return sem.SearchResponse{
		Results: []sem.SearchResult{{
			Rank: 1, FilePath: "internal/auth/auth_stub.go", StartLine: 1, EndLine: 4,
			FocusLine: 3, SnippetStartLine: 1, SnippetEndLine: 4,
			SymbolName: "ValidateTokenStub", Snippet: "func ValidateTokenStub(t string) bool { return t != \"\" }",
		}},
		RepoIgnored: &sem.RepoIgnoreReport{
			Files:   excluded,
			Sources: []sem.RepoIgnoreSource{{File: ".graphignore", Files: excluded}},
			Sample:  sample,
		},
		Stats: sem.SearchStats{RepoIgnoredFiles: excluded},
		Completeness: sem.CompletenessReport{Languages: map[string]sem.LanguageCompleteness{
			"Go": {Files: 2, Symbols: 2},
		}},
		Warnings: []sem.ProviderWarning{{
			Code:     "W_REPO_IGNORED_SOURCE",
			FilePath: "internal/auth/auth.go",
			Detail:   "1 file excluded by .graphignore",
		}},
	}
}

// TestAgentCoverageLineCountsRepoExclusions pins the specific sentence the
// finding was about. The coverage line is the agent payload's claim about how
// much of the repository the answer saw; before this change it reported the two
// files that survived a committed ignore rule and said nothing about the third.
func TestAgentCoverageLineCountsRepoExclusions(t *testing.T) {
	response := repoIgnoredResponse(1, []sem.RepoExclusion{
		{Path: "internal/auth/auth.go", Source: ".graphignore", Rule: "internal/auth/auth.go"},
	})
	var out bytes.Buffer
	if err := writeAgentSearch(&out, response, 4096); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"1 excluded by repo ignore rules",
		"warning W_REPO_IGNORED_SOURCE: internal/auth/auth.go",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("agent payload omitted %q:\n%s", want, text)
		}
	}
}

// TestAgentPayloadWithoutExclusionsIsUnchanged keeps the disclosure free on the
// common path: a repository that excluded nothing must not pay a byte for it.
func TestAgentPayloadWithoutExclusionsIsUnchanged(t *testing.T) {
	response := repoIgnoredResponse(0, nil)
	response.RepoIgnored = nil
	response.Warnings = nil
	var out bytes.Buffer
	if err := writeAgentSearch(&out, response, 4096); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "excluded by repo ignore rules") {
		t.Fatalf("payload mentioned exclusions that did not happen:\n%s", out.String())
	}
}

// TestTextSearchDisclosureIsBoundedAndLeads checks both halves of the rendering
// contract: the disclosure comes before the ranked hits (a reader who has already
// read the answer will not act on a footnote), and a repository that legitimately
// keeps dozens of vendored blobs out of the graph cannot turn every payload into
// a wall of paths.
func TestTextSearchDisclosureIsBoundedAndLeads(t *testing.T) {
	sample := make([]sem.RepoExclusion, 0, 10)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		sample = append(sample, sem.RepoExclusion{
			Path: "vendor/" + name + "/parser.c", Source: ".graphignore", Rule: "parser.c",
		})
	}
	response := repoIgnoredResponse(23, sample)
	var out bytes.Buffer
	if err := writeTextSearch(&out, response); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	header := strings.Index(text, "EXCLUDED: 23 files removed")
	if header != 0 {
		t.Fatalf("the disclosure must lead the payload, found at %d:\n%s", header, text)
	}
	if !strings.Contains(text, "... 20 more") {
		t.Fatalf("payload should name 3 paths and count the rest:\n%s", text)
	}
	if listed := strings.Count(text, "/parser.c"); listed != 3 {
		t.Fatalf("payload listed %d paths, want 3:\n%s", listed, text)
	}
}

// TestTextSearchUnreadableListIsBounded reproduces the "unbounded" half of the
// re-review finding: report.Unreadable, unlike Sources and Sample above it,
// was joined into the "LOWER BOUND" line with no cap of its own. The ledger
// samples up to maxRepoExclusionSample (10) unreadable directories, each an
// arbitrary repository-controlled path, so that single line could grow with
// the size of the broken subtree instead of the size of the answer.
func TestTextSearchUnreadableListIsBounded(t *testing.T) {
	response := repoIgnoredResponse(5, []sem.RepoExclusion{
		{Path: "vendor/keep.go", Source: ".graphignore", Rule: "vendor/"},
	})
	unreadable := make([]string, 0, 10)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		unreadable = append(unreadable, "vendor/"+name+"/broken")
	}
	response.RepoIgnored.CountIncomplete = true
	response.RepoIgnored.Unreadable = unreadable
	var out bytes.Buffer
	if err := writeTextSearch(&out, response); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if listed := strings.Count(text, "/broken"); listed != 3 {
		t.Fatalf("payload listed %d unreadable paths, want the same cap of 3 as Sources/Sample:\n%s", listed, text)
	}
	if !strings.Contains(text, "+7 more") {
		t.Fatalf("payload must count the omitted unreadable paths:\n%s", text)
	}
}

// TestTextSearchDisclosureIsChargedAgainstTheContextBudget reproduces the
// trail finding: the disclosure block was written into the text payload
// entirely outside response.Stats.ContextBudgetBytes, after the ranked
// results it labels had already been fit to that same ceiling. A caller with
// a tiny explicit ceiling still got the full disclosure block on top of it —
// a repository-controlled payload that competed with (or dwarfed) the actual
// answer for a budget the caller asked to bound.
func TestTextSearchDisclosureIsChargedAgainstTheContextBudget(t *testing.T) {
	response := repoIgnoredResponse(1, []sem.RepoExclusion{
		{Path: "internal/auth/auth.go", Source: ".graphignore", Rule: "internal/auth/auth.go"},
	})

	unbudgeted := response
	unbudgeted.Stats.ContextBudgetBytes = 0
	var withoutBudget bytes.Buffer
	if err := writeTextSearch(&withoutBudget, unbudgeted); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withoutBudget.String(), "EXCLUDED:") {
		t.Fatalf("an unset budget (0, historical behavior) must still print the disclosure:\n%s", withoutBudget.String())
	}

	tooSmall := response
	tooSmall.Stats.ContextBudgetBytes = 8
	var withTinyBudget bytes.Buffer
	if err := writeTextSearch(&withTinyBudget, tooSmall); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(withTinyBudget.String(), "EXCLUDED:") {
		t.Fatalf("a ceiling smaller than the disclosure block itself must drop it rather than blow"+
			" past the ceiling before a single ranked result is printed:\n%s", withTinyBudget.String())
	}

	roomy := response
	roomy.Stats.ContextBudgetBytes = 65536
	var withRoomyBudget bytes.Buffer
	if err := writeTextSearch(&withRoomyBudget, roomy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withRoomyBudget.String(), "EXCLUDED:") {
		t.Fatalf("a ceiling comfortably larger than the disclosure block must still show it:\n%s", withRoomyBudget.String())
	}
}

// TestTextSearchDisclosureDegradesRatherThanVanishesWhenTheBudgetIsSpent
// reproduces the trail finding.
//
// Charging the disclosure against the REMAINING headroom stopped it from blowing
// past --max-context-bytes, but it also made it disappear from the payload where
// it matters most. The fitter spends the ceiling down to the last few bytes — a
// real run measured result_bytes 1993 against a 2000-byte ceiling, and 3977
// against 4000 — so a repository with enough material to answer the query has no
// headroom left for anything. `--format text` renders no warnings and no fallback
// marker of its own, so the disclosure was the only signal that a committed
// ignore rule had removed content, and it went out in silence: the busier the
// repository and the more it excluded, the more complete its answer looked.
//
// The requirement is not that the full block survives — its size is chosen by the
// repository, so it must not. It is that SOMETHING says the corpus is not whole.
func TestTextSearchDisclosureDegradesRatherThanVanishesWhenTheBudgetIsSpent(t *testing.T) {
	sample := make([]sem.RepoExclusion, 0, 10)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		sample = append(sample, sem.RepoExclusion{
			Path: "vendor/" + name + "/parser.c", Source: ".graphignore", Rule: "parser.c",
		})
	}
	response := repoIgnoredResponse(23, sample)
	// The measured shape of a saturated payload: the ranking was fitted to the
	// ceiling and left 7 bytes behind it.
	response.Stats.ContextBudgetBytes = 2000
	response.Stats.ResultBytes = 1993

	var out bytes.Buffer
	if err := writeTextSearch(&out, response); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.HasPrefix(text, "EXCLUDED:") {
		t.Fatalf("a payload whose ranking spent the whole ceiling disclosed nothing about the 23 files"+
			" the repository's own rules removed; --format text has no other channel that says so:\n%s", text)
	}
	// Degraded, not merely admitted: the repository-controlled path list is exactly
	// what may not ride on top of a budget the ranking has already spent.
	if strings.Contains(text, "vendor/a/parser.c") {
		t.Fatalf("the repository-sized path list was printed on top of a spent ceiling:\n%s", text)
	}
	disclosure, _, _ := strings.Cut(text, "\n")
	if len(disclosure)+1 > 160 {
		t.Fatalf("the disclosure that survives a spent budget must be bounded, got %d bytes: %q",
			len(disclosure)+1, disclosure)
	}
	if !strings.Contains(disclosure, "23 files") || !strings.Contains(disclosure, "repo_ignored") {
		t.Fatalf("the degraded disclosure must still carry the count and point at the full report: %q", disclosure)
	}
}

// TestSearchCommandDisclosesExclusionsAtEveryBudget is the same finding proved end
// to end, through the real fitter rather than a hand-built response: one committed
// `.graphignore` rule hides a file, and every ceiling the caller might pick must
// still say so. Before the fix the sweep failed at 600, 1400, 2000 and 4000 —
// the budgets at which the ranking had enough material to consume the ceiling.
func TestSearchCommandDisclosesExclusionsAtEveryBudget(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	write(t, repo, ".graphignore", "hidden/\n")
	write(t, repo, "hidden/auth.go", "package hidden\n\n"+
		"// ValidateToken checks the bearer token presented on a request.\n"+
		"func ValidateToken(token string) bool { return len(token) == 64 }\n")
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("visible/auth_stub%d.go", i)
		write(t, repo, name, fmt.Sprintf("package visible\n\n"+
			"// ValidateTokenStub%d checks the bearer token presented on a request.\n"+
			"func ValidateTokenStub%d(token string) bool {\n\tif token == \"\" {\n\t\treturn false\n\t}\n"+
			"\tif len(token) != 64 {\n\t\treturn false\n\t}\n\treturn true\n}\n", i, i))
	}
	for _, budget := range []string{"600", "1400", "2000", "4000", "24576"} {
		var out bytes.Buffer
		err := Run(t.Context(), Options{Version: "0.1.0", Env: EntireEnv{RepoRoot: repo}, Stdout: &out}, []string{
			"search", "--repo", repo, "--query", "bearer token validation", "--format", "text",
			"--profile", "syntax-only", "--worktree", "--max-context-bytes", budget,
		})
		if err != nil {
			t.Fatalf("budget %s: %v", budget, err)
		}
		if !strings.Contains(out.String(), "EXCLUDED:") {
			t.Fatalf("budget %s: a committed ignore rule removed hidden/auth.go and the payload said"+
				" nothing about it:\n%s", budget, out.String())
		}
	}
}

// writeRepoIgnoredBudgetRepo is the fixture of the sweep above: one committed
// `.graphignore` rule hides a file, and eight visible files give the ranking
// enough material to consume a tight ceiling.
func writeRepoIgnoredBudgetRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	write(t, repo, ".graphignore", "hidden/\n")
	write(t, repo, "hidden/auth.go", "package hidden\n\n"+
		"// ValidateToken checks the bearer token presented on a request.\n"+
		"func ValidateToken(token string) bool { return len(token) == 64 }\n")
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("visible/auth_stub%d.go", i)
		write(t, repo, name, fmt.Sprintf("package visible\n\n"+
			"// ValidateTokenStub%d checks the bearer token presented on a request.\n"+
			"func ValidateTokenStub%d(token string) bool {\n\tif token == \"\" {\n\t\treturn false\n\t}\n"+
			"\tif len(token) != 64 {\n\t\treturn false\n\t}\n\treturn true\n}\n", i, i))
	}
	return repo
}

// The disclosure floor is funded from inside --max-context-bytes, and exactly one
// renderer prints it. Charging every format for it deleted ranked source from the
// formats that do not: at a 600-byte ceiling over an excluding repository, the
// JSON payload lost its only result and bought nothing with the bytes, because
// json/ndjson carry the exclusion facts as data outside the budget anyway.
//
// Both directions are asserted from one fixture and one ceiling: the format that
// does not render the floor keeps its ranked source, and the format that does
// still discloses.
func TestSearchCommandChargesTheDisclosureFloorOnlyToTheFormatThatPrintsIt(t *testing.T) {
	t.Parallel()
	repo := writeRepoIgnoredBudgetRepo(t)
	run := func(format string) string {
		t.Helper()
		var out bytes.Buffer
		err := Run(t.Context(), Options{Version: "0.1.0", Env: EntireEnv{RepoRoot: repo}, Stdout: &out}, []string{
			"search", "--repo", repo, "--query", "bearer token validation", "--format", format,
			"--profile", "syntax-only", "--worktree", "--max-context-bytes", "600",
		})
		if err != nil {
			t.Fatalf("format %s: %v", format, err)
		}
		return out.String()
	}
	var decoded struct {
		Results     []struct{} `json:"results"`
		RepoIgnored *struct {
			Files int `json:"files"`
		} `json:"repo_ignored"`
	}
	payload := run("json")
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("json payload: %v\n%s", err, payload)
	}
	if decoded.RepoIgnored == nil || decoded.RepoIgnored.Files == 0 {
		t.Fatalf("the fixture excluded nothing, so this proves nothing:\n%s", payload)
	}
	if len(decoded.Results) == 0 {
		t.Fatalf("a 600-byte ceiling returned no ranked source at all: the ranking was charged"+
			" for a disclosure floor that --format json never prints\n%s", payload)
	}
	text := run("text")
	if !strings.Contains(text, "EXCLUDED:") {
		t.Fatalf("--format text renders the floor and must still disclose at the same ceiling:\n%s", text)
	}
	// The reservation is still charged to text, so the FULL disclosure block fits
	// inside the ceiling. Withdrawing it there would let the ranking spend the
	// bytes and degrade the same payload to the bounded floor.
	if strings.Contains(text, "repo_ignored)") {
		t.Fatalf("--format text degraded to the bounded floor at a 600-byte ceiling: the block it"+
			" reserved for was not funded\n%s", text)
	}
}
