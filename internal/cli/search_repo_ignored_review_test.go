package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// TestNdjsonDisclosesExclusionsBeforeResults pins WHERE the disclosure lands in a
// stream. NDJSON is consumed a record at a time, and a consumer that stops once it
// has a usable result never reaches a trailing summary — so a disclosure carried
// only there reproduces the exact blindness this change exists to end.
func TestNdjsonDisclosesExclusionsBeforeResults(t *testing.T) {
	response := repoIgnoredResponse(1, []sem.RepoExclusion{
		{Path: "internal/auth/auth.go", Source: ".graphignore", Rule: "internal/auth/auth.go"},
	})
	var out bytes.Buffer
	if err := writeNdjsonSearch(&out, response); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	firstResult := -1
	disclosure := -1
	for i, line := range lines {
		var record struct {
			RecordType  string          `json:"record_type"`
			RepoIgnored json.RawMessage `json:"repo_ignored"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record %d is not JSON: %v\n%s", i, err, line)
		}
		if record.RecordType == "search_result" && firstResult < 0 {
			firstResult = i
		}
		if disclosure < 0 && (record.RecordType == "search_repo_ignored" || len(record.RepoIgnored) > 0) {
			disclosure = i
		}
	}
	if firstResult < 0 {
		t.Fatalf("fixture produced no result record:\n%s", out.String())
	}
	if disclosure < 0 {
		t.Fatalf("stream never discloses the exclusion:\n%s", out.String())
	}
	if disclosure > firstResult {
		t.Fatalf("exclusion disclosed at record %d, AFTER the first result at %d: a consumer that "+
			"acts on the first usable result never learns the corpus was narrowed\n%s",
			disclosure, firstResult, out.String())
	}
}

// TestTightBudgetKeepsExclusionCount covers the last rung of the agent budget
// ladder: the one that cannot hold a ranked location at all and synthesizes its
// own marker. It used to build that marker from scratch and drop the exclusion
// count even where the count would have fit.
func TestTightBudgetKeepsExclusionCount(t *testing.T) {
	response := repoIgnoredResponse(7, []sem.RepoExclusion{
		{Path: "internal/auth/auth.go", Source: ".graphignore", Rule: "internal/auth/auth.go"},
	})
	for _, budget := range []int{13, 15, 22, 23} {
		var out bytes.Buffer
		if err := writeAgentSearch(&out, response, budget); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if len(text) > budget {
			t.Fatalf("budget %d exceeded: %d bytes %q", budget, len(text), text)
		}
		if !strings.Contains(text, "X7") {
			t.Fatalf("budget %d: payload %q carries no exclusion count, so it still implies the "+
				"answer saw the whole repository", budget, text)
		}
	}
}
