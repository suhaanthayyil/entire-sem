package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// searchNdjsonRecordKindsBeforeThisChange is the set of record kinds a consumer
// written against the search stream as documented could have known about. It is a
// CONSTANT rather than a snapshot of what the emitter happens to produce, so
// adding a record kind cannot quietly widen what the test below considers known.
var searchNdjsonRecordKindsBeforeThisChange = map[string]bool{
	"search_header":  true,
	"search_result":  true,
	"search_summary": true,
}

// TestSearchNdjsonSurvivesATolerantReader pins the compatibility rule a new
// record kind has to satisfy, for every record kind, not just the new one.
//
// ADR 0001 is this repository's ratified schema contract: "A new minor may add
// optional fields OR OPTIONAL RECORD KINDS", and docs/snapshot-format.md requires
// consumers to "ignore unknown record types within the supported major schema
// version". The search stream has already exercised that twice since the GA
// freeze — search_container_map (fe257b7c) and search_closed_set (e8f9df6c) both
// sit between the header and the results — so an added kind is the established
// additive move here, not a new class of break.
//
// What the contract actually obliges the emitter to do is the thing this test
// checks: a reader that drops every record kind it does not recognise must still
// be able to reach every fact. The exclusion disclosure is the one that matters,
// because a disclosure only a new-kind reader can see is a disclosure the
// consumers most at risk never get. It therefore rides BOTH channels — its own
// early record for a streaming reader that stops at the first usable result, and
// search_summary.repo_ignored for a tolerant reader that skipped it.
func TestSearchNdjsonSurvivesATolerantReader(t *testing.T) {
	t.Parallel()
	response := repoIgnoredResponse(2, []sem.RepoExclusion{
		{Path: "internal/auth/auth.go", Source: ".graphignore", Rule: "internal/auth/auth.go"},
		{Path: "internal/auth/token.go", Source: ".graphignore", Rule: "internal/auth/token.go"},
	})
	var out bytes.Buffer
	if err := writeNdjsonSearch(&out, response); err != nil {
		t.Fatal(err)
	}

	var summary struct {
		RepoIgnored *sem.RepoIgnoreReport `json:"repo_ignored"`
	}
	var early *sem.RepoIgnoreReport
	sawSummary := false
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		var kind struct {
			RecordType string `json:"record_type"`
		}
		if err := json.Unmarshal([]byte(line), &kind); err != nil {
			t.Fatalf("record is not JSON: %v\n%s", err, line)
		}
		if !searchNdjsonRecordKindsBeforeThisChange[kind.RecordType] {
			// This is exactly what a tolerant reader does with it: nothing.
			if kind.RecordType == "search_repo_ignored" {
				early = &sem.RepoIgnoreReport{}
				if err := json.Unmarshal([]byte(line), early); err != nil {
					t.Fatalf("the disclosure record does not decode as a report: %v\n%s", err, line)
				}
			}
			continue
		}
		if kind.RecordType == "search_summary" {
			sawSummary = true
			if err := json.Unmarshal([]byte(line), &summary); err != nil {
				t.Fatalf("summary is not JSON: %v\n%s", err, line)
			}
		}
	}

	if !sawSummary {
		t.Fatalf("stream has no search_summary:\n%s", out.String())
	}
	if summary.RepoIgnored == nil {
		t.Fatalf("a reader that ignores unknown record kinds — the behaviour ADR 0001 REQUIRES of "+
			"it — never learns the corpus was narrowed: the disclosure exists only on a record "+
			"kind it is entitled to skip\n%s", out.String())
	}
	if early == nil {
		t.Fatalf("the stream carries no early disclosure record, so a consumer that acts on the "+
			"first usable result never reaches one\n%s", out.String())
	}
	// Both channels must say the same thing, or "which one you parse" becomes a
	// difference in what the repository is reported to have hidden.
	if summary.RepoIgnored.Files != early.Files || len(summary.RepoIgnored.Sample) != len(early.Sample) {
		t.Fatalf("the two channels disagree: summary %+v vs early record %+v",
			*summary.RepoIgnored, *early)
	}
	for i, exclusion := range early.Sample {
		if summary.RepoIgnored.Sample[i] != exclusion {
			t.Fatalf("sample %d differs between channels: %+v vs %+v",
				i, summary.RepoIgnored.Sample[i], exclusion)
		}
	}
}
