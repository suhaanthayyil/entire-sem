package sem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreadableDirectoriesHold probes whether this user and filesystem actually
// honour a mode-000 directory, the way foldsCase probes case folding: running as
// root, or on a filesystem that ignores the permission bits, makes the fixture
// below meaningless and the assertion vacuous. A GOOS check would be a guess
// about the same property.
func unreadableDirectoriesHold(t *testing.T) bool {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.MkdirAll(filepath.Join(probe, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(probe, 0o000); err != nil {
		return false
	}
	t.Cleanup(func() { _ = os.Chmod(probe, 0o755) })
	_, err := os.ReadDir(probe)
	return err != nil
}

// prunedTreeResponse runs the walk fallback over root and hands back the whole
// response, because the claim under test spans two of its fields.
func prunedTreeResponse(t *testing.T, root string) SearchResponse {
	t.Helper()
	response, err := SearchRepository(t.Context(), root, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// TestPrunedTreeDisclosureAdmitsWhatItCouldNotRead is the honesty of the count.
//
// RepoIgnoreReport.Files documents itself as "the exact number of listed paths
// excluded", and the coverage line renders it as a count of files the repository
// removed. The prune enumerator walked the ignored tree discarding every WalkDir
// error, so a directory it could not open contributed nothing and said nothing:
// the response came back successful, with a smaller number, still presented as
// exact. An understated security disclosure is worse than a loud one, because
// the reader has no way to tell it is understated.
//
// Uses only fields that exist before the fix, so it compiles unchanged against
// the current head and FAILS THERE AT RUNTIME: three files leave the corpus, the
// report claims one, and partial_failures is empty.
func TestPrunedTreeDisclosureAdmitsWhatItCouldNotRead(t *testing.T) {
	t.Parallel()
	if !unreadableDirectoriesHold(t) {
		t.Skip("this user can read a mode-000 directory (root, or a filesystem that ignores the bits), so the fixture cannot be built")
	}
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, "hidden/auth.go", "package hidden\n\nfunc ValidateToken(token string) bool { return len(token) == 64 }\n")
	write(t, root, "hidden/sub/deep.go", "package sub\n\nfunc DeepToken(token string) bool { return len(token) == 32 }\n")
	write(t, root, "hidden/sub/deeper.go", "package sub\n\nfunc DeeperToken(token string) bool { return token != \"\" }\n")
	write(t, root, "visible/auth_stub.go", "package visible\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")

	unreadable := filepath.Join(root, "hidden", "sub")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir's own cleanup, so it runs first and the tree is
	// removable again.
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	response := prunedTreeResponse(t, root)
	if response.RepoIgnored == nil {
		t.Fatalf("a %s prune removed three source files and the response disclosed nothing", graphIgnoreFileName)
	}
	if response.RepoIgnored.Files == 3 {
		return // The walk read everything after all; nothing was understated.
	}
	admitted := ""
	for _, failure := range response.PartialFailures {
		if strings.Contains(failure.FilePath, "hidden/sub") || strings.Contains(failure.Detail, "hidden/sub") {
			admitted = failure.Code
		}
	}
	if admitted == "" {
		t.Fatalf("the disclosure counts %d of the 3 files the %s rule removed because it could not read "+
			"hidden/sub, and reports the shortfall nowhere: partial_failures=%+v, report=%+v",
			response.RepoIgnored.Files, graphIgnoreFileName, response.PartialFailures, response.RepoIgnored)
	}
}

// TestReadablePrunedTreeAdmitsNothing is the kind-(b) guard on the fix above and
// must pass BEFORE and AFTER it. A prune the walk read completely has nothing to
// admit, and a partial failure raised on the ordinary path would be exactly the
// noise that teaches readers to skip the disclosure that matters.
func TestReadablePrunedTreeAdmitsNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, "hidden/auth.go", "package hidden\n\nfunc ValidateToken(token string) bool { return len(token) == 64 }\n")
	write(t, root, "hidden/sub/deep.go", "package sub\n\nfunc DeepToken(token string) bool { return len(token) == 32 }\n")
	write(t, root, "visible/auth_stub.go", "package visible\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")

	response := prunedTreeResponse(t, root)
	if response.RepoIgnored == nil || response.RepoIgnored.Files != 2 {
		t.Fatalf("the readable prune should disclose exactly its two files: %+v", response.RepoIgnored)
	}
	if len(response.PartialFailures) != 0 {
		t.Fatalf("a fully readable prune raised %+v; the disclosure must stay quiet when its count is exact",
			response.PartialFailures)
	}
}

// TestTotallyUnreadablePruneStillWarns is the same honesty one step further in.
//
// TestPrunedTreeDisclosureAdmitsWhatItCouldNotRead covers the PARTIAL shortfall:
// some descendants were counted, so Files > 0 and the disclosure warning fires.
// When the unreadable directory is reached before any descendant is counted the
// report comes back Files == 0 with CountIncomplete set — the TOTAL shortfall —
// and withRepoIgnoreDisclosure's `report.Files == 0` guard returned the caller's
// warnings untouched, so the one channel whose documented job is "a caller that
// only parses warnings still learns that this answer was assembled from a
// narrowed corpus" said nothing in the worst case it has.
//
// The text renderer already distinguishes the two (RenderRepoIgnoreDisclosure
// prints "how much could not be determined" for Files == 0), so before the fix
// the same report rendered as an exclusion on one channel and as silence on the
// other.
func TestTotallyUnreadablePruneStillWarns(t *testing.T) {
	t.Parallel()
	report := &RepoIgnoreReport{Files: 0, CountIncomplete: true, Unreadable: []string{"hidden/sub"}}
	got := withRepoIgnoreDisclosure([]ProviderWarning{{Code: "W_ONE"}}, report)
	if len(got) != 2 || got[0].Code != repoIgnoreDisclosureCode {
		t.Fatalf("an ignored tree that was unreadable before its first descendant was counted disclosed "+
			"nothing on the warnings channel: %+v", got)
	}
	if got[0].FilePath != "hidden/sub" {
		t.Errorf("FilePath = %q, want the path that stopped the count — a renderer that prints only a "+
			"warning's code and file has nothing else to act on", got[0].FilePath)
	}
	if strings.Contains(got[0].Detail, "0 files excluded") {
		t.Errorf("Detail = %q, but zero is a lower bound here, not a count", got[0].Detail)
	}
	if !strings.Contains(got[0].Detail, "unknown number") {
		t.Errorf("Detail = %q, want it to say the number is unknown", got[0].Detail)
	}
	if got[1].Code != "W_ONE" {
		t.Errorf("the caller's warnings lost their order: %+v", got)
	}
}

// TestNothingExcludedStillWarnsNothing is the kind-(b) guard on the widened
// condition above: a report with nothing to disclose must stay silent, or the
// widening buys the disclosure at the price of the noise that teaches readers to
// skip it.
func TestNothingExcludedStillWarnsNothing(t *testing.T) {
	t.Parallel()
	existing := []ProviderWarning{{Code: "W_ONE"}}
	if got := withRepoIgnoreDisclosure(existing, nil); len(got) != 1 {
		t.Fatalf("a nil report disclosed %+v", got)
	}
	if got := withRepoIgnoreDisclosure(existing, &RepoIgnoreReport{}); len(got) != 1 {
		t.Fatalf("an empty report disclosed %+v", got)
	}
}

// TestTotallyUnreadablePruneWarnsEndToEnd runs the same shortfall through a real
// repository rather than a hand-built report, so the fix is pinned to a shape the
// walk actually produces and not to one only the unit test can construct.
func TestTotallyUnreadablePruneWarnsEndToEnd(t *testing.T) {
	t.Parallel()
	if !unreadableDirectoriesHold(t) {
		t.Skip("this user can read a mode-000 directory (root, or a filesystem that ignores the bits), so the fixture cannot be built")
	}
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	// Every ignored source sits under the unreadable directory, so the accounting
	// reaches the thing it cannot read before it has counted a single path.
	write(t, root, "hidden/sub/deep.go", "package sub\n\nfunc DeepToken(token string) bool { return len(token) == 32 }\n")
	write(t, root, "hidden/sub/deeper.go", "package sub\n\nfunc DeeperToken(token string) bool { return token != \"\" }\n")
	write(t, root, "visible/auth_stub.go", "package visible\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")

	unreadable := filepath.Join(root, "hidden", "sub")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	response := prunedTreeResponse(t, root)
	if response.RepoIgnored == nil || response.RepoIgnored.Files != 0 || !response.RepoIgnored.CountIncomplete {
		t.Skipf("the walk counted the pruned tree after all, so this run does not exercise the total shortfall: %+v",
			response.RepoIgnored)
	}
	for _, warning := range response.Warnings {
		if warning.Code == repoIgnoreDisclosureCode {
			return
		}
	}
	t.Fatalf("the repository's own ignore rules removed an unknown amount of content and the warnings channel "+
		"never said so: warnings=%+v report=%+v", response.Warnings, response.RepoIgnored)
}
