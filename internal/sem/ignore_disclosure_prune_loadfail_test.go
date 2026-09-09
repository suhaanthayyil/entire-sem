package sem

import (
	"strings"
	"testing"
)

// TestPruneAccountingRefusesToDescendPastAnUnreadableNestedIgnore pins the one
// case the byte budget already covers but a parse failure did not.
//
// enterCharged returns false — do not descend — when the budget cannot pay to
// read a nested .gitignore, because "the rules of an unread ignore file are
// rules this stack does not have, so every verdict below that directory would be
// reached without them". A .gitignore that is read but does not PARSE leaves the
// stack in exactly that state, and the code returned success anyway.
//
// bufio.Scanner refuses a token above 64 KiB, so a nested .gitignore holding one
// long line fails to load while sitting far under both maxNestedIgnoreFileBytes
// (1 MiB) and the 4 MiB accounting budget. Every rule in it is then dropped —
// including ones the scanner had already parsed, since the partial matcher is
// discarded with the error — and `hidden/nested/secret.go`, a path Git's own
// nested rule hides, is disclosed as a file the repository's `.graphignore`
// removed from the corpus.
//
// Before the fix this fails with the sample naming hidden/nested/secret.go and
// CountIncomplete false: a complete-looking security disclosure blaming the
// wrong file.
func TestPruneAccountingRefusesToDescendPastAnUnreadableNestedIgnore(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	write(t, repo, "hidden/plain.go", "package hidden\n\nfunc Plain() {}\n")
	write(t, repo, "hidden/nested/secret.go", "package nested\n\nfunc Secret() {}\n")
	// Line 1 parses; line 2 is over bufio.Scanner's 64 KiB token limit, so
	// loadContent fails and the whole matcher — line 1 included — is thrown away.
	write(t, repo, "hidden/nested/.gitignore", "secret.go\n"+strings.Repeat("a", 70_000)+"\n")
	write(t, repo, graphIgnoreFileName, "hidden/\n")

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	if _, _, err := walkWorktreeFiles(t.Context(), repo, ignores, func(string) bool { return false }, ledger); err != nil {
		t.Fatal(err)
	}
	report := ledger.report()
	if report == nil {
		t.Fatal("the pruned tree left the corpus with no disclosure at all")
	}
	for _, exclusion := range report.Sample {
		if strings.HasPrefix(exclusion.Path, "hidden/nested/") {
			t.Fatalf("the disclosure blames %s for %q, but the nested .gitignore that hides it could"+
				" not be loaded, so the stack never had the rule that would have swallowed it: %+v",
				exclusion.Source, exclusion.Path, report.Sample)
		}
	}
	if !report.CountIncomplete {
		t.Fatalf("a subtree whose ignore rules could not be read is excluded and uncounted, and the"+
			" report presents its count as exact: %+v", *report)
	}
	// The half of the tree whose rules DID load is still disclosed exactly.
	if report.Files != 1 || report.Sample[0].Path != "hidden/plain.go" {
		t.Fatalf("files = %d sample = %+v, want the one descendant outside the unreadable subtree",
			report.Files, report.Sample)
	}
}

// TestPruneAccountingRefusesToDescendPastAnOversizedNestedIgnore is the size
// counterpart to the unparseable case above. Git still applies a nested
// .gitignore regardless of its size, so before the fix an existing file over
// maxNestedIgnoreFileBytes was treated as if absent and the walk kept
// descending — the same "swallowed a rule the stack never had" outcome the
// unparseable case exists to prevent, reached through a file that is simply
// too large rather than malformed.
func TestPruneAccountingRefusesToDescendPastAnOversizedNestedIgnore(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	write(t, repo, "hidden/plain.go", "package hidden\n\nfunc Plain() {}\n")
	write(t, repo, "hidden/nested/secret.go", "package nested\n\nfunc Secret() {}\n")
	oversized := "secret.go\n# " + strings.Repeat("a", maxNestedIgnoreFileBytes) + "\n"
	write(t, repo, "hidden/nested/.gitignore", oversized)
	write(t, repo, graphIgnoreFileName, "hidden/\n")

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	if _, _, err := walkWorktreeFiles(t.Context(), repo, ignores, func(string) bool { return false }, ledger); err != nil {
		t.Fatal(err)
	}
	report := ledger.report()
	if report == nil {
		t.Fatal("the pruned tree left the corpus with no disclosure at all")
	}
	for _, exclusion := range report.Sample {
		if strings.HasPrefix(exclusion.Path, "hidden/nested/") {
			t.Fatalf("the disclosure blames %s for %q, but the oversized nested .gitignore that hides"+
				" it was never read, so the stack never had the rule that would have swallowed it: %+v",
				exclusion.Source, exclusion.Path, report.Sample)
		}
	}
	if !report.CountIncomplete {
		t.Fatalf("a subtree whose ignore rules were too large to read is excluded and uncounted, and"+
			" the report presents its count as exact: %+v", *report)
	}
	if report.Files != 1 || report.Sample[0].Path != "hidden/plain.go" {
		t.Fatalf("files = %d sample = %+v, want the one descendant outside the oversized-ignore subtree",
			report.Files, report.Sample)
	}
}
