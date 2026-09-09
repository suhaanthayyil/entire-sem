package sem

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ignoreInputKeyer struct {
	name string
	key  func(ProviderSnapshotOptions) (string, error)
}

func ignoreInputKeyers(repo string) []ignoreInputKeyer {
	return []ignoreInputKeyer{
		{
			name: "search snapshot",
			key: func(options ProviderSnapshotOptions) (string, error) {
				return searchSnapshotKey(repo, "repo", "version", "tree", options)
			},
		},
		{
			name: "provider records",
			key: func(options ProviderSnapshotOptions) (string, error) {
				return providerRecordsKey(repo, "repo", "version", "commit", "tree", "snapshot", options)
			},
		},
	}
}

func TestCacheKeysRequireExplicitIgnoreInputs(t *testing.T) {
	repo := t.TempDir()
	inputs := []struct {
		name    string
		options ProviderSnapshotOptions
	}{
		{"ignore", ProviderSnapshotOptions{IgnoreFiles: []string{"missing.ignore"}}},
		{"include", ProviderSnapshotOptions{IncludeFiles: []string{"missing.include"}}},
	}
	for _, input := range inputs {
		for _, keyer := range ignoreInputKeyers(repo) {
			t.Run(input.name+"/"+keyer.name, func(t *testing.T) {
				_, err := keyer.key(input.options)
				if err == nil {
					t.Fatal("missing explicit input was treated as optional")
				}
				if !strings.Contains(err.Error(), "does not exist") {
					t.Fatalf("error = %v, want required-input wording", err)
				}
			})
		}
	}
}

func TestCacheKeysRejectOversizedIgnoreInputs(t *testing.T) {
	repo := t.TempDir()
	ignore := filepath.Join(repo, "oversized.ignore")
	if err := os.WriteFile(ignore, bytes.Repeat([]byte{'#'}, maxIgnoreFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	options := ProviderSnapshotOptions{IgnoreFiles: []string{ignore}}
	for _, keyer := range ignoreInputKeyers(repo) {
		t.Run(keyer.name, func(t *testing.T) {
			_, err := keyer.key(options)
			if err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("oversized cache-key input error = %v, want size refusal", err)
			}
		})
	}
}

func TestCaptureProviderCachePolicyBoundsAggregateInputs(t *testing.T) {
	repo := t.TempDir()
	comments := filepath.Join(repo, "comments.ignore")
	if err := os.WriteFile(comments, bytes.Repeat([]byte("#\n"), maxIgnoreFileBytes/2), 0o600); err != nil {
		t.Fatal(err)
	}
	tooManyBytes := ProviderSnapshotOptions{IgnoreFiles: []string{
		comments, comments, comments, comments, comments,
	}}
	if _, err := CaptureProviderCachePolicy(repo, tooManyBytes); err == nil || !strings.Contains(err.Error(), "captured bytes") {
		t.Fatalf("aggregate byte limit error = %v", err)
	}

	empty := filepath.Join(repo, "empty.ignore")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tooManyInputs := ProviderSnapshotOptions{IgnoreFiles: make([]string, maxCapturedIgnoreInputs+1)}
	for index := range tooManyInputs.IgnoreFiles {
		tooManyInputs.IgnoreFiles[index] = empty
	}
	if _, err := CaptureProviderCachePolicy(repo, tooManyInputs); err == nil || !strings.Contains(err.Error(), "captured inputs") {
		t.Fatalf("aggregate input limit error = %v", err)
	}
}

func TestSearchCacheCaptureBudgetFallsBackToUncachedBuild(t *testing.T) {
	repo := cacheTestRepo(t)
	comments := filepath.Join(repo, "comments.ignore")
	if err := os.WriteFile(comments, bytes.Repeat([]byte("#\n"), maxIgnoreFileBytes/2), 0o600); err != nil {
		t.Fatal(err)
	}
	options := ProviderSnapshotOptions{
		Profile:     ProfileFull,
		IgnoreFiles: []string{comments, comments, comments, comments, comments},
	}
	cacheDir := t.TempDir()
	snapshot, hit, err := LoadOrBuildProviderSnapshot(t.Context(), repo, "test", options, cacheDir, false)
	if err != nil {
		t.Fatalf("optional cache changed valid uncached behavior: %v", err)
	}
	if hit {
		t.Fatal("aggregate policy overflow unexpectedly produced a cache hit")
	}
	if !hasSymbolNamed(snapshot.Symbols, "Alpha") {
		t.Fatal("uncached fallback omitted the repository snapshot")
	}
	response, err := SearchRepository(t.Context(), repo, "test", "Alpha", SearchOptions{
		Profile:     ProfileFull,
		TopK:        5,
		CacheDir:    cacheDir,
		IgnoreFiles: append([]string(nil), options.IgnoreFiles...),
	})
	if err != nil {
		t.Fatalf("search cache capture changed valid uncached behavior: %v", err)
	}
	if len(response.Results) == 0 || response.Results[0].SymbolName != "Alpha" {
		t.Fatalf("uncached search fallback lost Alpha: %#v", response.Results)
	}
}

func TestCacheKeysDistinguishMissingGraphIgnoreFromMarkerContent(t *testing.T) {
	repo := t.TempDir()
	for _, keyer := range ignoreInputKeyers(repo) {
		t.Run(keyer.name, func(t *testing.T) {
			missing, err := keyer.key(ProviderSnapshotOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, graphIgnoreFileName), []byte("missing"), 0o600); err != nil {
				t.Fatal(err)
			}
			present, err := keyer.key(ProviderSnapshotOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if missing == present {
				t.Fatal("absent .graphignore collided with a present file containing the old missing marker")
			}
			if err := os.Remove(filepath.Join(repo, graphIgnoreFileName)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCacheKeysFrameRawIgnoreContent(t *testing.T) {
	repo := t.TempDir()
	first := filepath.Join(repo, "first.ignore")
	second := filepath.Join(repo, "second.ignore")
	if err := os.WriteFile(second, []byte("right"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Under NUL-delimited, untyped framing these two inputs serialize identically:
	// the one-file content can impersonate the delimiter, second path, delimiter,
	// and second content of the two-file form.
	crafted := append([]byte("left\x00"+filepath.Clean(second)+"\x00"), []byte("right")...)
	if err := os.WriteFile(first, crafted, 0o600); err != nil {
		t.Fatal(err)
	}
	oneFile := ProviderSnapshotOptions{IgnoreFiles: []string{first}}

	for _, keyer := range ignoreInputKeyers(repo) {
		t.Run(keyer.name, func(t *testing.T) {
			if err := os.WriteFile(first, crafted, 0o600); err != nil {
				t.Fatal(err)
			}
			oneKey, err := keyer.key(oneFile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(first, []byte("left"), 0o600); err != nil {
				t.Fatal(err)
			}
			twoKey, err := keyer.key(ProviderSnapshotOptions{IgnoreFiles: []string{first, second}})
			if err != nil {
				t.Fatal(err)
			}
			if oneKey == twoKey {
				t.Fatal("raw NUL content crossed field boundaries in the cache key")
			}
		})
	}
}

func TestProviderRecordsKeyIncludesGraphIgnore(t *testing.T) {
	repo := t.TempDir()
	key := func() string {
		t.Helper()
		got, err := providerRecordsKey(repo, "repo", "version", "commit", "tree", "snapshot", ProviderSnapshotOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	absent := key()
	if err := os.WriteFile(filepath.Join(repo, graphIgnoreFileName), []byte("vendor/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	present := key()
	if absent == present {
		t.Fatal("adding .graphignore did not invalidate provider records")
	}
	if err := os.WriteFile(filepath.Join(repo, graphIgnoreFileName), []byte("dist/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if edited := key(); edited == present {
		t.Fatal("editing .graphignore did not invalidate provider records")
	}
}

func TestSearchSnapshotKeyRejectsWorktree(t *testing.T) {
	repo := t.TempDir()
	if _, err := searchSnapshotKey(repo, "repo", "version", "tree", ProviderSnapshotOptions{Worktree: true}); err == nil {
		t.Fatal("working-tree snapshot received a persistent cache key")
	}
}

func TestWorktreeSnapshotAppliesGitInfoExcludeWithoutCaching(t *testing.T) {
	repo := cacheTestRepo(t)
	cacheDir := t.TempDir()
	options := ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull, NoNetwork: true}

	first, hit, err := LoadOrBuildProviderSnapshot(t.Context(), repo, "test", options, cacheDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("cold worktree cache reported a hit")
	}
	if !hasSymbolNamed(first.Symbols, "Alpha") {
		t.Fatal("cold snapshot did not include app.go")
	}

	exclude, err := gitInfoExcludePath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if exclude == "" {
		t.Fatal("test repository did not resolve info/exclude")
	}
	if err := os.WriteFile(exclude, []byte("/app.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, hit, err := LoadOrBuildProviderSnapshot(t.Context(), repo, "test", options, cacheDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("info/exclude edit reused the stale worktree snapshot")
	}
	// app.go is TRACKED (cacheTestRepo commits it), and Git applies
	// .git/info/exclude only while discovering UNTRACKED files: with `/app.go` in
	// info/exclude, `git ls-files --cached --others --exclude-standard` still
	// lists app.go and `git check-ignore -v app.go` exits 1, i.e. not ignored
	// (verified against git 2.54.0). Reapplying the operator's local exclude list
	// on top of Git's own listing deleted tracked source Git would have shown, and
	// because that list is the operator's rather than the repository's the
	// deletion carried no disclosure — the file left the corpus in silence. The
	// snapshot must keep it. info/exclude remains a policy input for untracked
	// content, which is why the key checks above still hold.
	if !hasSymbolNamed(second.Symbols, "Alpha") {
		t.Fatal("snapshot dropped a TRACKED file merely named in info/exclude; git still lists it")
	}

	third, hit, err := LoadOrBuildProviderSnapshot(t.Context(), repo, "test", options, cacheDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("unchanged info/exclude made the worktree snapshot cacheable")
	}
	if !hasSymbolNamed(third.Symbols, "Alpha") {
		t.Fatal("second rebuilt snapshot dropped the tracked file git still lists")
	}
}
