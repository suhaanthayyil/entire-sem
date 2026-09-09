package sem

import (
	"fmt"
	"testing"
)

// TestRepoIgnoreDisclosureReserveIsExactlyTheFloorItFunds pins the price of the
// disclosure at what the disclosure costs.
//
// The floor is funded from inside --max-context-bytes, and the funding was the
// bound (maxRepoIgnoreFloorBytes) rather than the sentence: every ranking in a
// repository with an ignore rule paid 160 bytes for a 94-byte disclosure. The
// count is settled before the reservation is made — a listing's RepoIgnoreReport
// travels from preselection to SearchResponse unmodified — so the exact length is
// knowable at the reservation and the slack was bought from the ranking for
// nothing.
func TestRepoIgnoreDisclosureReserveIsExactlyTheFloorItFunds(t *testing.T) {
	t.Parallel()
	report := &RepoIgnoreReport{
		Files:   3,
		Sources: []RepoIgnoreSource{{File: ".graphignore", Files: 3}},
		Sample:  []RepoExclusion{{Path: "hidden/auth.go", Source: ".graphignore", Rule: "hidden/"}},
	}
	floor := len(RenderRepoIgnoreDisclosureFloor(report))
	if floor == 0 || floor >= maxRepoIgnoreFloorBytes {
		t.Fatalf("fixture is wrong: floor of %d bytes cannot distinguish the exact price from the bound %d",
			floor, maxRepoIgnoreFloorBytes)
	}
	if got := repoIgnoreDisclosureReserveBytes(report); got != floor {
		t.Errorf("reserve = %d, want the floor's own %d bytes: the ranking is charged the bound rather"+
			" than the disclosure it funds", got, floor)
	}
	if got := repoIgnoreDisclosureReserveBytes(nil); got != 0 {
		t.Errorf("reserve with nothing to disclose = %d, want 0", got)
	}
}

// TestSearchChargesTheDisclosureFloorOnlyToARendererThatPrintsIt is the review
// finding.
//
// Exactly one renderer emits the floor: `--format text`, which has no warning
// channel of its own. JSON and NDJSON carry the same disclosure as DATA — the
// W_REPO_IGNORED_SOURCE warning and the whole repo_ignored report — and emit it
// whatever the ranking spends, so a reservation taken out of their ranking funds
// nothing that reaches their reader. It only deletes ranked source, and at a
// small ceiling it deletes all of it: measured on this fixture at 600 bytes, the
// reservation cost the payload its only result.
func TestSearchChargesTheDisclosureFloorOnlyToARendererThatPrintsIt(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	write(t, repo, graphIgnoreFileName, "hidden/\n")
	write(t, repo, "hidden/auth.go", "package hidden\n\n"+
		"// ValidateToken checks the bearer token presented on a request.\n"+
		"func ValidateToken(token string) bool { return len(token) == 64 }\n")
	for i := 1; i <= 8; i++ {
		write(t, repo, fmt.Sprintf("visible/auth_stub%d.go", i), fmt.Sprintf("package visible\n\n"+
			"// ValidateTokenStub%d checks the bearer token presented on a request.\n"+
			"func ValidateTokenStub%d(token string) bool {\n\tif token == \"\" {\n\t\treturn false\n\t}\n"+
			"\tif len(token) != 64 {\n\t\treturn false\n\t}\n\treturn true\n}\n", i, i))
	}
	const budget = 600
	search := func(t *testing.T, omitsFloor bool) SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
			Worktree:                       true,
			Profile:                        ProfileSyntaxOnly,
			TopK:                           8,
			MaxContextBytes:                budget,
			OmitsRepoIgnoreDisclosureFloor: omitsFloor,
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.RepoIgnored == nil {
			t.Fatalf("fixture is wrong: %s did not hide anything", graphIgnoreFileName)
		}
		return response
	}

	structured := search(t, true)
	// The disclosure a structured consumer actually reads is unaffected by the
	// reservation: it rides the response, not the ranking's budget.
	if len(withRepoIgnoreDisclosure(structured.Warnings, structured.RepoIgnored)) == 0 {
		t.Fatal("a structured payload lost the disclosure it carries as data")
	}
	if len(structured.Results) == 0 {
		t.Errorf("a renderer that never prints the disclosure floor was still charged for one and came"+
			" back with no ranked source at all at a %d-byte ceiling", budget)
	}
	if structured.Stats.ResultBytes > budget {
		t.Errorf("result_bytes %d exceeds the caller's %d-byte ceiling", structured.Stats.ResultBytes, budget)
	}

	// The renderer that DOES print the floor still funds it, and still funds it
	// from inside the caller's ceiling.
	text := search(t, false)
	floor := len(RenderRepoIgnoreDisclosureFloor(text.RepoIgnored))
	funded := text.Stats.ResultBytes + text.Stats.TypeCardBytes + text.Stats.SignatureTypeBytes
	if funded+floor > budget {
		t.Errorf("the floor-printing renderer was fitted to %d funded bytes of a %d-byte ceiling and"+
			" cannot print its %d-byte floor without overrunning it", funded, budget, floor)
	}
	if structured.Stats.ResultBytes <= text.Stats.ResultBytes {
		t.Errorf("result_bytes %d (floor not rendered) is not more than %d (floor rendered): the"+
			" reservation is still charged to a renderer that cannot spend it",
			structured.Stats.ResultBytes, text.Stats.ResultBytes)
	}
}
