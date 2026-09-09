package cli

import (
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// The exclusion accounting's shortfall records are what tell a reader that the
// coverage line's file count — and the compact form's X<n> — is a LOWER BOUND
// rather than the number. The producer appends them after whatever parser
// failures the run collected, and the agent payload renders only three
// diagnostics, so a repository with a few unparseable files pushed the shortfall
// into "... N more diagnostics in JSON output" and reported a narrowed corpus as
// if it were whole.
//
// Reproduced end to end before the fix: a checkout whose tree git could not list,
// with eight E_PARSE_ERROR files, rendered
//
//	Coverage: degraded (4 languages/10 files; 1 warning; 9 partial failures)
//	- warning W_WORKTREE_SNAPSHOT
//	- partial E_PARSE_ERROR: bad1.js
//	- partial E_PARSE_ERROR: bad1.ts
//	- ... 7 more diagnostics in JSON output
//
// with E_REPO_IGNORE_GIT_UNAVAILABLE ninth of nine and therefore invisible.
func TestAgentDiagnosticsKeepTheExclusionShortfallVisible(t *testing.T) {
	t.Parallel()

	response := sem.SearchResponse{
		Warnings: []sem.ProviderWarning{{Code: "W_WORKTREE_SNAPSHOT", Severity: "warning"}},
		PartialFailures: []sem.PartialFailure{
			{Code: "E_PARSE_ERROR", Severity: "warning", FilePath: "bad1.js"},
			{Code: "E_PARSE_ERROR", Severity: "warning", FilePath: "bad2.js"},
			{Code: "E_PARSE_ERROR", Severity: "warning", FilePath: "bad3.js"},
			{Code: "E_REPO_IGNORE_COUNT_INCOMPLETE", Severity: "warning", FilePath: "vendor"},
		},
		Stats: sem.SearchStats{RepoIgnoredFiles: 7},
	}

	full, compact := agentSearchDiagnostics(response)
	if !strings.Contains(string(full), "- partial E_REPO_IGNORE_COUNT_INCOMPLETE") {
		t.Fatalf("the payload claims X%d excluded without showing that the count is only a lower"+
			" bound; agent block:\n%s", response.Stats.RepoIgnoredFiles, full)
	}
	if !strings.Contains(string(compact), "X7") {
		t.Fatalf("compact form lost the exclusion count: %s", compact)
	}
}

// Hoisting must not drop, duplicate or reorder anything else: every diagnostic
// still has to reach the JSON-overflow count, and the parser failures keep their
// own order behind the shortfall.
func TestHoistRepoIgnoreShortfallPreservesEveryRecord(t *testing.T) {
	t.Parallel()

	failures := []sem.PartialFailure{
		{Code: "E_PARSE_ERROR", FilePath: "a.js"},
		{Code: "E_REPO_IGNORE_GIT_UNAVAILABLE"},
		{Code: "E_PARSE_ERROR", FilePath: "b.js"},
		{Code: "E_REPO_IGNORE_COUNT_INCOMPLETE", FilePath: "vendor"},
	}
	hoisted := hoistRepoIgnoreShortfall(failures)
	want := []string{
		"E_REPO_IGNORE_GIT_UNAVAILABLE", "E_REPO_IGNORE_COUNT_INCOMPLETE",
		"E_PARSE_ERROR", "E_PARSE_ERROR",
	}
	if len(hoisted) != len(want) {
		t.Fatalf("hoisted %d records, want %d: %+v", len(hoisted), len(want), hoisted)
	}
	for i, code := range want {
		if hoisted[i].Code != code {
			t.Fatalf("hoisted[%d].Code = %q, want %q (%+v)", i, hoisted[i].Code, code, hoisted)
		}
	}
	if hoisted[2].FilePath != "a.js" || hoisted[3].FilePath != "b.js" {
		t.Fatalf("parser failures lost their relative order: %+v", hoisted)
	}
	if failures[0].Code != "E_PARSE_ERROR" {
		t.Fatalf("input was mutated: %+v", failures)
	}
}

// TestAgentDiagnosticsKeepBothExclusionShortfallsVisible is the two-shortfall
// case the single-slot guarantee still lost: a fallback worktree search with
// counted exclusions can emit W_REPO_IGNORED_SOURCE and W_WORKTREE_SNAPSHOT
// together, consuming two of the three visible slots, and separately can emit
// E_REPO_IGNORE_GIT_UNAVAILABLE alongside E_REPO_IGNORE_COUNT_INCOMPLETE.
// Reserving only one failure slot showed the former and hid the latter,
// leaving the coverage line's X<n> looking exact when it was a lower bound
// missing its own explanation.
func TestAgentDiagnosticsKeepBothExclusionShortfallsVisible(t *testing.T) {
	t.Parallel()

	response := sem.SearchResponse{
		Warnings: []sem.ProviderWarning{
			{Code: "W_REPO_IGNORED_SOURCE", Severity: "info", FilePath: "internal/auth/auth.go"},
			{Code: "W_WORKTREE_SNAPSHOT", Severity: "warning"},
		},
		PartialFailures: []sem.PartialFailure{
			{Code: "E_REPO_IGNORE_GIT_UNAVAILABLE", Severity: "warning"},
			{Code: "E_REPO_IGNORE_COUNT_INCOMPLETE", Severity: "warning", FilePath: "vendor"},
		},
		Stats: sem.SearchStats{RepoIgnoredFiles: 7},
	}

	full, _ := agentSearchDiagnostics(response)
	if !strings.Contains(string(full), "- partial E_REPO_IGNORE_GIT_UNAVAILABLE") {
		t.Fatalf("E_REPO_IGNORE_GIT_UNAVAILABLE must stay visible: agent block:\n%s", full)
	}
	if !strings.Contains(string(full), "- partial E_REPO_IGNORE_COUNT_INCOMPLETE") {
		t.Fatalf("E_REPO_IGNORE_COUNT_INCOMPLETE must stay visible alongside it, not just one of the"+
			" two: agent block:\n%s", full)
	}
}
