package sem

import (
	"fmt"
	"strconv"
	"testing"
)

// TestSearchFundsTheIgnoreDisclosureFromInsideTheCallerCeiling is the re-review
// finding, fixed where it is producible rather than where it is printed.
//
// A text payload leads with what the repository's own rules removed, and the
// disclosure degrades to an irreducible floor rather than vanishing — that is
// what makes `--format text`, which renders no warnings of its own, able to say
// the corpus is not whole. But the ranking is fitted to the WHOLE ceiling, and
// the fitter really does spend it to the last few bytes (measured: result_bytes
// 1993 of a 2000-byte ceiling). The floor was then admitted on top, so a payload
// that had something to disclose exceeded --max-context-bytes by the floor's
// full length.
//
// Neither half is negotiable on its own: shrinking the disclosure to nothing
// re-hides the corpus, and admitting it on top overruns a number the caller
// sized a context window against. So the reservation is made BEFORE the ranking
// is fitted — a listing that has something to disclose funds the floor out of
// the ceiling, and the ranking is fitted to what remains. The disclosure then
// fits inside the caller's number instead of riding on top of it.
func TestSearchFundsTheIgnoreDisclosureFromInsideTheCallerCeiling(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	write(t, repo, graphIgnoreFileName, "hidden/\n")
	write(t, repo, "hidden/auth.go", "package hidden\n\n"+
		"// ValidateToken checks the bearer token presented on a request.\n"+
		"func ValidateToken(token string) bool { return len(token) == 64 }\n")
	// Enough ranked material that the fitter can spend any of the ceilings below.
	for i := 1; i <= 8; i++ {
		write(t, repo, fmt.Sprintf("visible/auth_stub%d.go", i), fmt.Sprintf("package visible\n\n"+
			"// ValidateTokenStub%d checks the bearer token presented on a request.\n"+
			"func ValidateTokenStub%d(token string) bool {\n\tif token == \"\" {\n\t\treturn false\n\t}\n"+
			"\tif len(token) != 64 {\n\t\treturn false\n\t}\n\treturn true\n}\n", i, i))
	}
	floorBudget := len(RenderRepoIgnoreDisclosureFloor(&RepoIgnoreReport{Files: 1}))
	for _, budget := range []int{floorBudget, floorBudget + 1, 600, 1400, 2000, 4000, 24576} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
				Worktree:        true,
				Profile:         ProfileSyntaxOnly,
				TopK:            8,
				MaxContextBytes: budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.RepoIgnored == nil {
				t.Fatalf("fixture is wrong: %s did not hide anything", graphIgnoreFileName)
			}
			if response.Stats.ContextBudgetBytes != budget {
				t.Fatalf("Stats.ContextBudgetBytes = %d, want the caller's own %d: the reservation is"+
					" taken out of what the ranking may spend, not out of the number reported back",
					response.Stats.ContextBudgetBytes, budget)
			}
			floor := len(RenderRepoIgnoreDisclosureFloor(response.RepoIgnored))
			if floor == 0 {
				t.Fatalf("a report of %d excluded files rendered no floor", response.RepoIgnored.Files)
			}
			funded := response.Stats.ResultBytes + response.Stats.TypeCardBytes + response.Stats.SignatureTypeBytes
			// Empty JSON results cost two bytes in statistics, but text emits
			// no array delimiters and has no ranked source to fund.
			if len(response.Results) == 0 {
				funded -= serializedSearchResultBytes([]SearchResult{})
			}
			if funded+floor > budget {
				t.Errorf("the ranking was fitted to %d funded bytes of a %d-byte ceiling, leaving %d for a"+
					" %d-byte disclosure floor: a payload that says what the repository removed can only"+
					" do it by overrunning --max-context-bytes",
					funded, budget, budget-funded, floor)
			}
		})
	}
}
