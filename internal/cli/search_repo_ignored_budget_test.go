package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// TestTightestBudgetsStillCarryTheExclusionCount is the bottom of the agent
// budget ladder.
//
// The rung that cannot hold a ranked location is ordered so the count survives
// "wherever it fits at all" — that is the rule the code states about itself. It
// did not hold: every count-bearing candidate also carried the cache state, so
// below the 13 bytes that pair needs, the ladder fell through to a longer
// count-less form and the payload went back to implying the answer saw the whole
// repository. `!N X1` is six bytes and fits at every one of these caps.
//
// Uses only pre-existing API, so it compiles unchanged against the current head
// and FAILS THERE AT RUNTIME (budget 10 renders `!N I:miss`).
func TestTightestBudgetsStillCarryTheExclusionCount(t *testing.T) {
	response := repoIgnoredResponse(1, []sem.RepoExclusion{
		{Path: "internal/auth/auth.go", Source: ".graphignore", Rule: "internal/auth/auth.go"},
	})
	for _, budget := range []int{6, 7, 8, 9, 10, 11, 12} {
		var out bytes.Buffer
		if err := writeAgentSearch(&out, response, budget); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if len(text) > budget {
			t.Fatalf("budget %d exceeded: %d bytes %q", budget, len(text), text)
		}
		if !strings.Contains(text, "X1") {
			t.Errorf("budget %d: payload %q drops the exclusion count even though the six-byte "+
				"count-bearing form fits, so the answer implies it saw the whole repository", budget, text)
		}
	}
}

// TestRoomierBudgetsKeepTheCacheState is the kind-(b) guard on the rung above and
// must pass BEFORE and AFTER it. The count-only form is a last resort, not a
// promotion: wherever the fuller line fits, the cache state is not traded for it.
func TestRoomierBudgetsKeepTheCacheState(t *testing.T) {
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
		if !strings.Contains(text, "miss") || !strings.Contains(text, "X7") {
			t.Errorf("budget %d: payload %q lost the cache state or the count where both fit", budget, text)
		}
	}
}

// TestTightBudgetWithoutExclusionsIsUnchanged keeps the common path free. A
// response with nothing to disclose must render the same marker it always did,
// with no empty count suffix invented for it.
func TestTightBudgetWithoutExclusionsIsUnchanged(t *testing.T) {
	response := repoIgnoredResponse(0, nil)
	response.RepoIgnored = nil
	response.Warnings = nil
	for _, budget := range []int{6, 10, 12} {
		var out bytes.Buffer
		if err := writeAgentSearch(&out, response, budget); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if len(text) > budget {
			t.Fatalf("budget %d exceeded: %d bytes %q", budget, len(text), text)
		}
		if strings.Contains(text, "X") {
			t.Errorf("budget %d: payload %q invented an exclusion marker for a repository that excluded nothing", budget, text)
		}
	}
}
