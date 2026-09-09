package sem

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/gitutil"
)

func oversizedNestedIgnoreBody() string {
	return strings.Repeat("# padding\n", maxNestedIgnoreFileBytes/len("# padding\n")+1)
}

func parsedIgnoreRules(count int) string {
	var content strings.Builder
	for index := 0; index < count; index++ {
		fmt.Fprintf(&content, "never-match-%05d\n", index)
	}
	return content.String()
}

func initializeIgnorePolicyRepo(t *testing.T, repo string) string {
	t.Helper()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	git(t, repo, "config", "commit.gpgsign", "false")
	git(t, repo, "add", "-f", ".")
	git(t, repo, "commit", "-m", "ignore policy fixture")
	return rev(t, repo, "HEAD")
}

func stageUnmergedIgnorePath(t *testing.T, repo, candidate string) {
	t.Helper()
	blobs := []string{
		gitInput(t, repo, "# base\n", "hash-object", "-w", "--stdin"),
		gitInput(t, repo, "# ours\n", "hash-object", "-w", "--stdin"),
		gitInput(t, repo, "# theirs\n", "hash-object", "-w", "--stdin"),
	}
	git(t, repo, "update-index", "--force-remove", "--", candidate)
	var indexInfo strings.Builder
	for index, blob := range blobs {
		fmt.Fprintf(&indexInfo, "100644 %s %d\t%s%c", blob, index+1, candidate, byte(0))
	}
	gitInput(t, repo, indexInfo.String(), "update-index", "-z", "--index-info")
}

func wantIgnoreLimitError(t *testing.T, err error, file, limit string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want ignore-policy refusal for %q", file)
	}
	if !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), limit) {
		t.Fatalf("ignore-policy refusal = %q, want file %q and limit %q", err, file, limit)
	}
}

// wantIgnorePolicySubtreeExcluded is the filesystem fallback's contract for a
// directory whose OWN .gitignore cannot be read within its per-file bounds: its
// rules are unknown, so nothing under it is listed — the property the former
// hard error was protecting — and the omission is disclosed as a warning
// instead of failing the entire listing.
func wantIgnorePolicySubtreeExcluded(t *testing.T, paths []string, warnings []ProviderWarning, err error, dir string) {
	t.Helper()
	if err != nil {
		t.Fatalf("unreadable ignore policy under %q failed the whole listing: %v", dir, err)
	}
	for _, rel := range paths {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			t.Fatalf("listed %q under a directory whose ignore policy is unknown: %#v", rel, paths)
		}
	}
	for _, warning := range warnings {
		if warning.Code == "W_WALK_UNREADABLE_IGNORE_POLICY" && strings.Contains(warning.Detail, dir) {
			return
		}
	}
	t.Fatalf("excluded %q without disclosing it: %#v", dir, warnings)
}

func TestOversizedNestedIgnoreIsReportedAcrossListings(t *testing.T) {
	body := oversizedNestedIgnoreBody()
	if len(body) <= maxNestedIgnoreFileBytes {
		t.Fatalf("fixture is %d bytes, want more than %d", len(body), maxNestedIgnoreFileBytes)
	}

	t.Run("filesystem walk", func(t *testing.T) {
		repo := t.TempDir()
		writeFile(t, repo, "root.go", "package root\n")
		writeFile(t, repo, "nested/.gitignore", body)
		writeFile(t, repo, "nested/keep.go", "package nested\n")
		paths, warnings, err := walkWorktreeFiles(t.Context(), repo, ignoreMatcher{}, func(string) bool { return false }, nil)
		// The fallback excludes the subtree whose policy it cannot read and
		// says so. Returning the error here made one oversized metadata file an
		// outage for the whole repository; see wantIgnorePolicySubtreeExcluded.
		wantIgnorePolicySubtreeExcluded(t, paths, warnings, err, "nested")
		if !slices.Contains(paths, "root.go") {
			t.Fatalf("filesystem walk dropped the rest of the repository: %#v", paths)
		}
	})

	t.Run("Git worktree", func(t *testing.T) {
		repo := t.TempDir()
		writeFile(t, repo, "nested/.gitignore", body)
		writeFile(t, repo, "nested/keep.go", "package nested\n")
		initializeIgnorePolicyRepo(t, repo)
		ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
		wantIgnoreLimitError(t, err, "nested/.gitignore", strconv.Itoa(maxNestedIgnoreFileBytes))
	})

	t.Run("committed tree", func(t *testing.T) {
		repo := t.TempDir()
		writeFile(t, repo, "nested/.gitignore", body)
		writeFile(t, repo, "nested/keep.go", "package nested\n")
		revision := initializeIgnorePolicyRepo(t, repo)
		opened, err := openSource(t.Context(), repo, revision, sourceOptions{})
		if opened.close != nil {
			defer opened.close()
		}
		wantIgnoreLimitError(t, err, "nested/.gitignore", strconv.Itoa(maxNestedIgnoreFileBytes))
	})
}

func TestCommittedRootIgnoreLimitsAreReported(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "bytes", body: oversizedNestedIgnoreBody(), want: strconv.Itoa(maxIgnoreFileBytes)},
		{name: "rules", body: parsedIgnoreRules(maxIgnoreParsedRules + 1), want: strconv.Itoa(maxIgnoreParsedRules) + " parsed rules"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, repo, ".gitignore", test.body)
			writeFile(t, repo, "keep.go", "package keep\n")
			revision := initializeIgnorePolicyRepo(t, repo)
			opened, err := openSource(t.Context(), repo, revision, sourceOptions{})
			if opened.close != nil {
				defer opened.close()
			}
			wantIgnoreLimitError(t, err, ".gitignore", test.want)
		})
	}
}

func TestCommittedIgnoreReportsUnreadableBlob(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	knownBlob := gitInput(t, repo, "known\n", "hash-object", "-w", "--stdin")
	missingOID := strings.Repeat("1", len(knownBlob))
	tree := gitInput(
		t,
		repo,
		fmt.Sprintf("100644 blob %s\t.gitignore%c", missingOID, byte(0)),
		"mktree",
		"-z",
		"--missing",
	)

	reader := gitutil.NewLimitedFileReader(t.Context(), repo, tree, maxNestedIgnoreFileBytes)
	t.Cleanup(func() { _ = reader.Close() })
	if err := reader.Prime([]string{".gitignore"}); err != nil {
		t.Fatalf("prime committed ignore reader: %v", err)
	}
	content, present, err := readCommittedIgnoreFile(reader, ".gitignore", ".gitignore", false)
	if content != "" || present || err == nil {
		t.Fatalf("unreadable committed ignore = (%q, %v, %v), want reported refusal", content, present, err)
	}
	if !strings.Contains(err.Error(), "Git blob object is unavailable or unreadable") {
		t.Fatalf("unreadable committed ignore error = %q, want actionable blob error", err)
	}
	if strings.Contains(err.Error(), "unknown read status") {
		t.Fatalf("unreadable committed ignore error = %q, want typed status handling", err)
	}
}

func TestCommittedIgnoreReportsNonBlobTree(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	ignoreTree := gitInput(t, repo, "", "mktree", "-z")
	keepBlob := gitInput(t, repo, "package keep\n", "hash-object", "-w", "--stdin")
	tree := gitInput(
		t,
		repo,
		fmt.Sprintf(
			"040000 tree %s\t.gitignore%c100644 blob %s\tkeep.go%c",
			ignoreTree,
			byte(0),
			keepBlob,
			byte(0),
		),
		"mktree",
		"-z",
	)

	opened, err := openSource(t.Context(), repo, tree, sourceOptions{})
	if opened.close != nil {
		defer opened.close()
	}
	if err == nil || !strings.Contains(err.Error(), `committed ignore file ".gitignore" is not a blob`) {
		t.Fatalf("non-blob committed ignore error = %v, want actionable object-type refusal", err)
	}
}

func TestNestedIgnoreRulesShareOneOperationBudget(t *testing.T) {
	baseRules := parsedIgnoreRules(maxIgnoreParsedRules - 1)
	nestedRules := "!keep/**\nsecond-rule\n"
	limit := strconv.Itoa(maxIgnoreParsedRules) + " parsed rules"

	t.Run("filesystem walk", func(t *testing.T) {
		repo := t.TempDir()
		writeFile(t, repo, ".gitignore", baseRules)
		writeFile(t, repo, "vendor/.gitignore", nestedRules)
		writeFile(t, repo, "vendor/keep/keep.go", "package keep\n")
		ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = walkWorktreeFiles(t.Context(), repo, ignores, func(string) bool { return false }, nil)
		wantIgnoreLimitError(t, err, "vendor/.gitignore", limit)
	})

	t.Run("Git worktree", func(t *testing.T) {
		repo := t.TempDir()
		writeFile(t, repo, ".gitignore", baseRules)
		writeFile(t, repo, "vendor/.gitignore", nestedRules)
		writeFile(t, repo, "vendor/keep/keep.go", "package keep\n")
		initializeIgnorePolicyRepo(t, repo)
		ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
		wantIgnoreLimitError(t, err, "vendor/.gitignore", limit)
	})

	t.Run("committed tree shares explicit ledger", func(t *testing.T) {
		repo := t.TempDir()
		writeFile(t, repo, graphIgnoreFileName, baseRules)
		writeFile(t, repo, ".gitignore", "# committed root\n")
		writeFile(t, repo, "vendor/.gitignore", nestedRules)
		writeFile(t, repo, "vendor/keep/keep.go", "package keep\n")
		revision := initializeIgnorePolicyRepo(t, repo)
		opened, err := openSource(t.Context(), repo, revision, sourceOptions{})
		if opened.close != nil {
			defer opened.close()
		}
		wantIgnoreLimitError(t, err, "vendor/.gitignore", limit)
	})
}

func TestFilesystemWalkReleasesDepartedIgnoreRuleLevels(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "first/.gitignore", "first-rule\n")
	writeFile(t, repo, "first/keep.go", "package first\n")
	writeFile(t, repo, "second/.gitignore", "second-rule\n")
	writeFile(t, repo, "second/keep.go", "package second\n")
	base := ignoreMatcher{parsedRuleCount: maxIgnoreParsedRules - 1}
	if _, _, err := walkWorktreeFiles(t.Context(), repo, base, func(string) bool { return false }, nil); err != nil {
		t.Fatalf("sibling ignore levels were retained after departure: %v", err)
	}

	deep := t.TempDir()
	writeFile(t, deep, "first/.gitignore", "first-rule\n")
	writeFile(t, deep, "first/second/.gitignore", "second-rule\n")
	writeFile(t, deep, "first/second/keep.go", "package second\n")
	_, _, err := walkWorktreeFiles(t.Context(), deep, base, func(string) bool { return false }, nil)
	wantIgnoreLimitError(t, err, "first/second/.gitignore", strconv.Itoa(maxIgnoreParsedRules)+" parsed rules")
}

func TestNestedIgnoreFileCountIsReportedAcrossListings(t *testing.T) {
	repo := t.TempDir()
	for index := 0; index <= maxNestedIgnoreFiles; index++ {
		writeFile(t, repo, fmt.Sprintf("d%03d/.gitignore", index), "# empty\n")
	}
	revision := initializeIgnorePolicyRepo(t, repo)
	want := fmt.Sprintf("more than %d nested ignore files", maxNestedIgnoreFiles)

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	worktreeWant := fmt.Sprintf("exceed %d paths", maxNestedIgnoreFiles)
	if _, _, err := worktreeSourceFiles(t.Context(), repo, ignores, false, nil); err == nil || !strings.Contains(err.Error(), worktreeWant) {
		t.Fatalf("Git worktree nested-file limit error = %v, want %q", err, worktreeWant)
	}
	opened, err := openSource(t.Context(), repo, revision, sourceOptions{})
	if opened.close != nil {
		defer opened.close()
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("committed nested-file limit error = %v, want %q", err, want)
	}
	if _, _, err := walkWorktreeFiles(t.Context(), repo, ignores, func(string) bool { return false }, nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("filesystem nested-file limit error = %v, want %q", err, want)
	}
}

func TestWorktreeNestedIgnoreDoesNotFollowEscapingDirectorySymlink(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "vendor/.gitignore", "# indexed placeholder\n")
	writeFile(t, repo, "vendor/keep/keep.go", "package keep\n")
	initializeIgnorePolicyRepo(t, repo)

	external := t.TempDir()
	writeFile(t, external, ".gitignore", "!keep/**\n")
	writeFile(t, external, "keep/keep.go", "package escaped\n")
	writeFile(t, external, "only-external.go", "package escaped\n")
	if err := os.RemoveAll(filepath.Join(repo, "vendor")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(repo, "vendor")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths, warnings, err := worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
	if err != nil {
		if !strings.Contains(err.Error(), "vendor/.gitignore") {
			t.Fatalf("escaping nested-ignore symlink error = %v", err)
		}
		return
	}

	// Windows declines Git enumeration when the directory symlink is seen as a
	// reparse point, then completes through the no-follow filesystem fallback.
	// That safe outcome is intentionally a warning rather than the nested-ignore
	// error produced by the Git-listing path on POSIX.
	var fallbackDetail string
	for _, warning := range warnings {
		if warning.Code == "W_GIT_WORKTREE_FALLBACK" {
			fallbackDetail = warning.Detail
			break
		}
	}
	if fallbackDetail == "" || !strings.Contains(fallbackDetail, "vendor") {
		t.Fatalf("escaping directory symlink warnings = %+v, want explicit fallback for vendor", warnings)
	}
	for _, externalPath := range []string{"vendor/keep/keep.go", "vendor/only-external.go"} {
		if slices.Contains(paths, externalPath) {
			t.Fatalf("escaping directory symlink exposed external path %q in %v", externalPath, paths)
		}
	}
}

func TestReusableNestedIgnoreBudgetStartsFresh(t *testing.T) {
	base := ignoreMatcher{parsedRuleCount: maxIgnoreParsedRules - 1}
	for attempt := 0; attempt < 2; attempt++ {
		rules := newNestedIgnoreRules(base)
		if err := rules.addFile("vendor/.gitignore", "!keep/**\n"); err != nil {
			t.Fatalf("attempt %d reused a spent ignore budget: %v", attempt+1, err)
		}
	}
}

func TestUnmergedNestedIgnoreHasProviderParity(t *testing.T) {
	repo := t.TempDir()
	policy := parsedIgnoreRules(maxIgnoreParsedRules/2) + "*\n!.gitignore\n!mypkg/\n!mypkg/**\n"
	writeFile(t, repo, "vendor/.gitignore", policy)
	writeFile(t, repo, "vendor/mypkg/kept.go", "package kept\n")
	initializeIgnorePolicyRepo(t, repo)
	stageUnmergedIgnorePath(t, repo, "vendor/.gitignore")

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths, _, err := worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerAllows := false
	for _, candidate := range paths {
		if candidate == "vendor/mypkg/kept.go" {
			providerAllows = true
			break
		}
	}
	if !providerAllows {
		t.Fatalf("provider paths = %q, want vendored path re-admitted by nested policy", paths)
	}

}

func TestIgnoredNestedIgnoreHasProviderParity(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, ".gitignore", "vendor/.gitignore\n")
	writeFile(t, repo, "main.go", "package main\n")
	initializeIgnorePolicyRepo(t, repo)
	// Git still applies this ignored policy file to sibling paths: keep.go is
	// in the eligible listing even though vendor/.gitignore itself is not.
	writeFile(t, repo, "vendor/.gitignore", "!mypkg/**\n")
	writeFile(t, repo, "vendor/mypkg/keep.go", "package mypkg\n")

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths, _, err := worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerAllows := false
	for _, candidate := range paths {
		if candidate == "vendor/mypkg/keep.go" {
			providerAllows = true
			break
		}
	}
	if !providerAllows {
		t.Fatalf("provider paths = %q, want path re-admitted by an ignored nested policy file", paths)
	}

}

func TestIrrelevantIgnoredNestedIgnoresDoNotSpendProviderBudget(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, ".gitignore", "ignored/\n")
	writeFile(t, repo, "main.go", "package main\n")
	initializeIgnorePolicyRepo(t, repo)
	for index := 0; index <= maxNestedIgnoreFiles; index++ {
		writeFile(t, repo, fmt.Sprintf("ignored/d%03d/.gitignore", index), "# irrelevant\n")
	}

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths, _, err := worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
	if err != nil {
		t.Fatalf("irrelevant ignored policies exhausted provider budget: %v", err)
	}
	if !slices.Contains(paths, "main.go") {
		t.Fatalf("provider paths = %q, want main.go", paths)
	}

}

func TestCommittedSubdirectoryReadsRootAndNestedIgnoreFiles(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "bench/.gitignore", "vendor/*\n!vendor/rootpkg/\n!vendor/rootpkg/**\n")
	writeFile(t, repo, "bench/vendor/rootpkg/root.go", "package rootpkg\n")
	writeFile(t, repo, "bench/memory/.gitignore", "vendor/*\n!vendor/nestedpkg/\n!vendor/nestedpkg/**\n")
	writeFile(t, repo, "bench/memory/vendor/nestedpkg/nested.go", "package nestedpkg\n")
	revision := initializeIgnorePolicyRepo(t, repo)

	opened, err := openSource(t.Context(), filepath.Join(repo, "bench"), revision, sourceOptions{})
	if opened.close != nil {
		defer opened.close()
	}
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"vendor/rootpkg/root.go":            false,
		"memory/vendor/nestedpkg/nested.go": false,
	}
	for _, candidate := range opened.paths {
		if _, exists := want[candidate]; exists {
			want[candidate] = true
		}
	}
	for candidate, found := range want {
		if !found {
			t.Errorf("committed subdirectory paths = %q, missing %q", opened.paths, candidate)
		}
	}
}
