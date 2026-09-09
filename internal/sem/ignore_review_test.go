package sem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalInfoExcludeIsNotDisclosedAsRepoControlled draws the line the
// disclosure depends on. `.git/info/exclude` is machine-local Git metadata: it is
// never part of the tree, so no contributor can push one and no reader of the
// repository is being hidden from by it. Reporting it as something "the
// repository's own ignore rules" removed is a false alarm, and it puts the local
// operator's private exclusion list into a payload.
func TestLocalInfoExcludeIsNotDisclosedAsRepoControlled(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "internal/auth/auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return len(token) == 64 }\n")
	write(t, repo, "internal/auth/auth_stub.go", "package auth\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	// The operator's own local exclude, on a TRACKED file so it actually removes
	// something from the corpus.
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("internal/auth/auth.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored != nil {
		t.Fatalf("a local .git/info/exclude was reported as a repository-controlled exclusion: %+v", *response.RepoIgnored)
	}
	if response.Stats.RepoIgnoredFiles != 0 {
		t.Fatalf("stats counted %d repo-controlled exclusions, want 0", response.Stats.RepoIgnoredFiles)
	}
	response.RepoRoot = ""
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "info/exclude") {
		t.Fatalf("payload leaked the operator's local exclude file:\n%s", payload)
	}
}

// TestWalkFallbackDisclosesGraphIgnoreExclusions covers the listing mode the
// git-backed accounting does not reach: a directory Git cannot enumerate still
// honours `.graphignore`, so the same one-line corpus narrowing works there, and
// it was silent.
//
// `.graphignore` is the channel that must be disclosed here: Git does not know
// the file, so anything it removes is content Git itself would still have shown.
// Ordinary `.gitignore` removals stay silent in this mode by design — see
// walkWorktreeFiles.
func TestWalkFallbackDisclosesGraphIgnoreExclusions(t *testing.T) {
	// No initRepo: with no git directory, ListWorktreeFiles fails and the
	// filesystem walk is the listing.
	repo := t.TempDir()
	write(t, repo, "internal/auth/auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return len(token) == 64 }\n")
	write(t, repo, "internal/auth/auth_stub.go", "package auth\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")
	write(t, repo, graphIgnoreFileName, "internal/auth/auth.go\n")
	response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if result.FilePath == "internal/auth/auth.go" {
			t.Fatalf("fixture is wrong: .graphignore did not hide auth.go in walk mode")
		}
	}
	if response.RepoIgnored == nil {
		t.Fatalf("the filesystem-walk fallback narrowed the corpus and disclosed nothing; stats say %d excluded",
			response.Stats.RepoIgnoredFiles)
	}
	if got := response.RepoIgnored.Files; got != 1 {
		t.Fatalf("disclosed %d exclusions, want 1: %+v", got, *response.RepoIgnored)
	}
	if response.Stats.RepoIgnoredFiles != 1 {
		t.Fatalf("stats counted %d, want 1", response.Stats.RepoIgnoredFiles)
	}
	if response.RepoIgnored.Sample[0].Path != "internal/auth/auth.go" {
		t.Fatalf("wrong path disclosed: %+v", response.RepoIgnored.Sample)
	}
}

// TestTruncatedDisclosurePointsAtWhatJSONActuallyHolds keeps the suggested action
// honest. The text rendering names three paths and counts the rest; it used to
// point at "the full list" in JSON, but the JSON sample is capped too, so for a
// repository with more than ten exclusions that instruction cannot be followed.
func TestTruncatedDisclosurePointsAtWhatJSONActuallyHolds(t *testing.T) {
	sample := make([]RepoExclusion, 0, maxRepoExclusionSample)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		sample = append(sample, RepoExclusion{Path: "vendor/" + name + "/parser.c", Source: ".graphignore", Rule: "parser.c"})
	}
	truncated := RenderRepoIgnoreDisclosure(&RepoIgnoreReport{
		Files:           23,
		Sources:         []RepoIgnoreSource{{File: ".graphignore", Files: 23}},
		Sample:          sample,
		SampleTruncated: true,
	})
	if strings.Contains(string(truncated), "full list") {
		t.Fatalf("the rendering promises a full list the JSON does not hold (sample is capped at %d of 23):\n%s",
			maxRepoExclusionSample, truncated)
	}
	if !strings.Contains(string(truncated), "10") {
		t.Fatalf("the rendering should say how many paths the JSON actually names:\n%s", truncated)
	}
	// An uncapped report may still point at the complete list, because there it is complete.
	whole := RenderRepoIgnoreDisclosure(&RepoIgnoreReport{
		Files:   5,
		Sources: []RepoIgnoreSource{{File: ".graphignore", Files: 5}},
		Sample:  sample[:5],
	})
	if !strings.Contains(string(whole), "full list") {
		t.Fatalf("an uncapped report should still point at the complete list:\n%s", whole)
	}
}

// TestRenderRepoIgnoreDisclosureCapsTheSourcesHeaderLine reproduces the trail
// finding: the header line joined EVERY entry of report.Sources with no cap of
// its own, unlike the sample list below it (capped at maxRenderedRepoExclusions).
// Sources carries one entry per distinct .gitignore/.git-info-exclude/
// .graphignore file that contributed an exclusion, and nothing bounds how many
// distinct files a repository can nest -- each one an arbitrary
// repository-controlled path. --format text has no separate byte-budget
// parameter of its own; every OTHER block in this renderer stays bounded by a
// fixed count (the sample, the body count, top-k) rather than a byte ceiling,
// so an uncapped join here was the one place a repository could make a single
// line grow without limit ahead of the results --max-context-bytes already
// bounds elsewhere.
func TestRenderRepoIgnoreDisclosureCapsTheSourcesHeaderLine(t *testing.T) {
	sources := make([]RepoIgnoreSource, 0, 500)
	for i := 0; i < 500; i++ {
		sources = append(sources, RepoIgnoreSource{
			File:  fmt.Sprintf("vendor/pkg%04d/nested/deeply/.gitignore", i),
			Files: 1,
		})
	}
	rendered := string(RenderRepoIgnoreDisclosure(&RepoIgnoreReport{
		Files:   500,
		Sources: sources,
		Sample:  []RepoExclusion{{Path: "vendor/pkg0000/nested/deeply/x.go", Source: sources[0].File, Rule: "*"}},
	}))
	header, _, _ := strings.Cut(rendered, "\n")
	if len(header) > 2048 {
		t.Fatalf("header line is %d bytes for %d sources: it must stay capped like the sample list, not"+
			" grow with the number of distinct ignore files a repository nests:\n%s", len(header), len(sources), header)
	}
	if !strings.Contains(rendered, "+") || !strings.Contains(rendered, "more") {
		t.Fatalf("rendered disclosure does not disclose that the source list was truncated:\n%s", rendered)
	}
}

// TestRenderRepoIgnoreDisclosureShowsEverySourceWhenFew is the widening
// direction: an ordinary report with only a couple of source files must still
// name all of them, unchanged.
func TestRenderRepoIgnoreDisclosureShowsEverySourceWhenFew(t *testing.T) {
	rendered := string(RenderRepoIgnoreDisclosure(&RepoIgnoreReport{
		Files: 2,
		Sources: []RepoIgnoreSource{
			{File: ".gitignore", Files: 1},
			{File: "vendor/.gitignore", Files: 1},
		},
		Sample: []RepoExclusion{{Path: "build/out.js", Source: ".gitignore", Rule: "build/"}},
	}))
	header, _, _ := strings.Cut(rendered, "\n")
	if !strings.Contains(header, ".gitignore") || !strings.Contains(header, "vendor/.gitignore") {
		t.Fatalf("header dropped a source name below the cap: %q", header)
	}
	if strings.Contains(header, "more") {
		t.Fatalf("a header at or under the source cap must not claim there is more:\n%s", header)
	}
}

// TestWalkFallbackDoesNotDiscloseWhatGitWouldHideAnyway is the other half of the
// walk fallback's contract. The disclosure's whole claim is "Git would still have
// shown you this file", which is why only `.graphignore` is accounted for there.
// A path that a Git-applied rule ALSO covers fails that claim even when the
// `.graphignore` rule is the one that wins the precedence contest — it is ordinary
// build output, and reporting it both cries wolf and prints paths nobody asked
// about.
func TestWalkFallbackDoesNotDiscloseWhatGitWouldHideAnyway(t *testing.T) {
	// No initRepo: the filesystem walk is the listing.
	repo := t.TempDir()
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	write(t, repo, "bundle.gen.go", "package main\n\nfunc GeneratedBundle() string { return \"bearer token validation\" }\n")
	// Git already hides the generated file; .graphignore covers it too, and its
	// rule is the one that wins (loaded later, matches the path itself).
	write(t, repo, ".gitignore", "bundle.gen.go\n")
	write(t, repo, graphIgnoreFileName, "*.gen.go\n")
	response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if result.FilePath == "bundle.gen.go" {
			t.Fatalf("fixture is wrong: the generated file was not excluded")
		}
	}
	if response.RepoIgnored != nil {
		t.Fatalf("ordinary Git-ignored build output was reported as repository-hidden source: %+v",
			*response.RepoIgnored)
	}
	if response.Stats.RepoIgnoredFiles != 0 {
		t.Fatalf("stats counted %d, want 0", response.Stats.RepoIgnoredFiles)
	}
}

// TestWalkFallbackStatesWhatItCannotSeeInARealCheckout pins the fallback's blind
// spot in the mode it actually happens in: a real git working tree that Git
// itself cannot enumerate (no usable git binary), where a tracked source matched
// by both .gitignore and .graphignore leaves the corpus. Git does not apply
// .gitignore to a tracked file, so Git's own listing would still have shown it —
// but this listing mode cannot ask which files are tracked, so the exclusion
// cannot be attributed. Reporting nothing answered "the repository hid nothing"
// to a corpus it had narrowed.
func TestWalkFallbackStatesWhatItCannotSeeInARealCheckout(t *testing.T) {
	repo := t.TempDir()
	// A .git entry is what makes this a checkout with tracked files, and it is all
	// the fallback can consult: the walk runs precisely because git cannot.
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	write(t, repo, "tracked_hidden.go", "package main\n\nfunc TrackedHidden() {}\n")
	write(t, repo, ".gitignore", "tracked_hidden.go\n")
	write(t, repo, graphIgnoreFileName, "tracked_hidden.go\n")

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	paths, _, err := walkWorktreeFiles(t.Context(), repo, ignores, func(string) bool { return false }, ledger)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if path == "tracked_hidden.go" {
			t.Fatalf("fixture is wrong: the file was not excluded from the listing")
		}
	}
	report := ledger.report()
	if report == nil {
		t.Fatal("a tracked source left the corpus with no disclosure at all")
	}
	if !report.GitListingUnavailable {
		t.Fatalf("the report does not say Git's listing was unavailable: %+v", *report)
	}
	rendered := string(RenderRepoIgnoreDisclosure(report))
	if !strings.Contains(rendered, "Git could not list this checkout") {
		t.Fatalf("text payload does not state the limitation: %q", rendered)
	}
	failures := withRepoIgnorePartialFailures(nil, report)
	if len(failures) != 1 || failures[0].Code != repoIgnoreGitUnavailableCode {
		t.Fatalf("partial failures = %+v, want one %s", failures, repoIgnoreGitUnavailableCode)
	}
}

// TestWalkFallbackOutsideACheckoutStaysSilent is the other direction of the same
// change: where there is no .git there are no tracked files, so nothing a
// .gitignore removes could be content Git would still have listed, and the new
// statement must not fire. Without this the warning would print over every
// ordinary directory that has a .gitignore.
func TestWalkFallbackOutsideACheckoutStaysSilent(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	write(t, repo, "build.out.go", "package main\n\nfunc BuildOutput() {}\n")
	write(t, repo, ".gitignore", "build.out.go\n")

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	if _, _, err := walkWorktreeFiles(t.Context(), repo, ignores, func(string) bool { return false }, ledger); err != nil {
		t.Fatal(err)
	}
	if report := ledger.report(); report != nil {
		t.Fatalf("ordinary gitignored build output raised a disclosure outside a checkout: %+v", *report)
	}
}

// TestRepoExclusionRuleTextIsBounded pins the payload's size to the report, not
// to the repository. The deciding rule is copied into every sample entry, so one
// long ignore line multiplied by the sample cap; a 60KiB line put 600KiB of
// repository-controlled text into a payload budgeted at 24KiB.
func TestRepoExclusionRuleTextIsBounded(t *testing.T) {
	long := strings.Repeat("**/", 20000) + "src/*.go"
	ledger := &repoIgnoreLedger{}
	for i := range maxRepoExclusionSample {
		ledger.note(RepoExclusion{
			Path:   fmt.Sprintf("src/w%d.go", i),
			Source: graphIgnoreFileName,
			Rule:   long,
		})
	}
	report := ledger.report()
	if report == nil {
		t.Fatal("no report")
	}
	total := 0
	for _, exclusion := range report.Sample {
		if len(exclusion.Rule) > maxRepoExclusionRuleBytes+len("...") {
			t.Fatalf("rule is %d bytes, want at most %d", len(exclusion.Rule), maxRepoExclusionRuleBytes+3)
		}
		if !strings.HasSuffix(exclusion.Rule, "...") {
			t.Fatalf("a truncated rule does not say it was truncated: %q", exclusion.Rule)
		}
		total += len(exclusion.Rule)
	}
	if total > 4096 {
		t.Fatalf("sample carries %d bytes of rule text", total)
	}
	// The other direction: an ordinary pattern still arrives whole, because the
	// rule is what tells a reader which line to edit.
	short := &repoIgnoreLedger{}
	short.note(RepoExclusion{Path: "vendor/x.go", Source: ".graphignore", Rule: "vendor/**"})
	if got := short.report().Sample[0].Rule; got != "vendor/**" {
		t.Fatalf("ordinary rule was altered: %q", got)
	}
}

// TestDisclosureRendersAnUncountableExclusion covers the count the text renderer
// used to drop: an ignored directory unreadable before its first descendant is
// reached excludes an unknown number of files, so Files is 0 while
// CountIncomplete is true. The JSON channel said so; the text payload printed
// nothing, telling a reader who only sees text that the corpus was whole.
func TestDisclosureRendersAnUncountableExclusion(t *testing.T) {
	rendered := string(RenderRepoIgnoreDisclosure(&RepoIgnoreReport{
		Files:           0,
		CountIncomplete: true,
		Unreadable:      []string{"hidden"},
	}))
	if rendered == "" {
		t.Fatal("an uncountable exclusion rendered nothing")
	}
	if !strings.Contains(rendered, "EXCLUDED") || !strings.Contains(rendered, "hidden") {
		t.Fatalf("rendered payload does not name the exclusion: %q", rendered)
	}
	if !strings.Contains(rendered, "LOWER BOUND") {
		t.Fatalf("rendered payload presents an incomplete count as exact: %q", rendered)
	}
	// A partially counted tree states the same shortfall beside its number.
	partial := string(RenderRepoIgnoreDisclosure(&RepoIgnoreReport{
		Files:           2,
		Sources:         []RepoIgnoreSource{{File: ".graphignore", Files: 2}},
		Sample:          []RepoExclusion{{Path: "hidden/a.go", Source: ".graphignore", Rule: "hidden/"}},
		CountIncomplete: true,
		Unreadable:      []string{"hidden/deep"},
	}))
	if !strings.Contains(partial, "LOWER BOUND") {
		t.Fatalf("a partial count rendered as exact: %q", partial)
	}
	// And nothing to disclose still renders nothing, so the widening cannot put a
	// block at the head of every ordinary payload.
	if got := RenderRepoIgnoreDisclosure(&RepoIgnoreReport{}); got != nil {
		t.Fatalf("empty report rendered %q", got)
	}
	if got := RenderRepoIgnoreDisclosure(nil); got != nil {
		t.Fatalf("nil report rendered %q", got)
	}
}

// TestRepoIgnoreDisclosureFloorIsBoundedAndRepositoryFree pins the property the
// whole degrade-instead-of-omit rule rests on.
//
// The full disclosure is sized by the repository being searched, which is why the
// text renderer may only print it when it fits the remaining context headroom.
// The floor is what gets printed when it does not, and it is admitted against the
// ceiling rather than the headroom — so it may push a payload past
// --max-context-bytes. That is only defensible while the push is bounded by a
// number THIS file picks: the floor must name no repository-controlled bytes at
// all, and must stay under maxRepoIgnoreFloorBytes however hostile the report is.
func TestRepoIgnoreDisclosureFloorIsBoundedAndRepositoryFree(t *testing.T) {
	t.Parallel()
	hostile := strings.Repeat("h", 4096)
	report := &RepoIgnoreReport{
		Files:           20000,
		Sources:         []RepoIgnoreSource{{File: hostile + "/.gitignore", Files: 20000}},
		Sample:          []RepoExclusion{{Path: hostile + "/secret.go", Source: hostile, Rule: hostile}},
		SampleTruncated: true,
		CountIncomplete: true,
		Unreadable:      []string{hostile + "/broken"},
	}
	full := string(RenderRepoIgnoreDisclosure(report))
	if len(full) <= maxRepoIgnoreFloorBytes {
		t.Fatalf("fixture is not hostile enough to exercise the floor: full block is %d bytes", len(full))
	}
	floor := string(RenderRepoIgnoreDisclosureFloor(report))
	if len(floor) > maxRepoIgnoreFloorBytes {
		t.Fatalf("floor is %d bytes, want at most %d — a floor the repository can grow is not a floor:\n%s",
			len(floor), maxRepoIgnoreFloorBytes, floor)
	}
	if strings.Contains(floor, "h") && strings.Contains(floor, hostile[:64]) {
		t.Fatalf("floor carries repository-controlled bytes:\n%s", floor)
	}
	// It still has to be a disclosure: the marker readers scan for, the count, and
	// where the names it dropped can be found.
	for _, want := range []string{"EXCLUDED:", "at least 20000 files", "repo_ignored"} {
		if !strings.Contains(floor, want) {
			t.Fatalf("floor omitted %q:\n%s", want, floor)
		}
	}
	// A shortfall with nothing counted is still something to disclose — same rule
	// the full block applies, and for the same reason.
	unknown := string(RenderRepoIgnoreDisclosureFloor(&RepoIgnoreReport{CountIncomplete: true}))
	if !strings.Contains(unknown, "EXCLUDED:") || !strings.Contains(unknown, "unknown") {
		t.Fatalf("an uncountable exclusion must still disclose itself:\n%s", unknown)
	}
	if got := RenderRepoIgnoreDisclosureFloor(&RepoIgnoreReport{}); got != nil {
		t.Fatalf("a repository that excluded nothing must pay no bytes: %q", got)
	}
	if got := RenderRepoIgnoreDisclosureFloor(nil); got != nil {
		t.Fatalf("a nil report must render nothing: %q", got)
	}
}
