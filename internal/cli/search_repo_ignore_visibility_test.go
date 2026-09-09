package cli

import (
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// TestAgentDiagnosticsKeepRepoIgnoreDisclosureVisible pins the one warning that
// names a hidden file against the diagnostics cap.
//
// The agent payload prints three diagnostics and replaces the rest with a count,
// so a disclosure that arrives behind three unrelated warnings loses its path and
// the reader is left with "something is hidden" — a shrug, not a next action.
//
// FAILS AT RUNTIME on the current head: the rendered block names the three
// unrelated warnings and omits the excluded path.
func TestAgentDiagnosticsKeepRepoIgnoreDisclosureVisible(t *testing.T) {
	t.Parallel()
	response := sem.SearchResponse{
		Warnings: []sem.ProviderWarning{
			{Code: "W_ONE", Severity: "warning", FilePath: "a/one.go"},
			{Code: "W_TWO", Severity: "warning", FilePath: "a/two.go"},
			{Code: "W_THREE", Severity: "warning", FilePath: "a/three.go"},
			{
				Code:     "W_REPO_IGNORED_SOURCE",
				Severity: "warning",
				FilePath: "internal/auth/auth.go",
				Detail:   "1 file excluded by .graphignore; including internal/auth/auth.go",
			},
		},
		Stats: sem.SearchStats{RepoIgnoredFiles: 1},
	}
	full, compact := agentSearchDiagnostics(response)
	if !strings.Contains(string(full), "W_REPO_IGNORED_SOURCE: internal/auth/auth.go") {
		t.Fatalf("the disclosure lost its path to the diagnostics cap:\n%s", full)
	}
	if !strings.Contains(string(compact), "X1") {
		t.Errorf("compact form must keep the exclusion count: %s", compact)
	}
	// The cap itself still holds: three diagnostics, then the count.
	if lines := strings.Count(string(full), "- warning "); lines != 3 {
		t.Errorf("rendered %d warning lines, want the cap of 3:\n%s", lines, full)
	}
	if !strings.Contains(string(full), "1 more diagnostic") {
		t.Errorf("the omitted diagnostic must still be counted:\n%s", full)
	}
}
