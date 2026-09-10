package sem

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Offline retrieval eval.
//
// Ranking quality in this package has been guarded by hand-measured cases recorded in code
// comments, which is enough to justify a change and not enough to detect the damage it does
// elsewhere. Two ranking mechanisms reached entireio/entire-graph#247 as recommendations before
// measurement contradicted them: a document-frequency ceiling that the actual term distribution
// refuted, and an "the cost is bounded" claim about a false positive that turned out to hijack a
// whole query. Both were plausible read as prose. Neither survived a number.
//
// This is the smallest thing that produces a number. A fixture corpus with two competing clusters,
// a table of queries, and one assertion per query: the gold cluster must outrank the distractor
// cluster. It is deliberately NOT an exact-rank assertion — the question every defect in #247 asks
// is "did retrieval pick the right neighbourhood", and pinning rank 1 to a literal symbol makes the
// eval fail on score drift that changed nothing that matters.
//
// It runs offline against a temp corpus, so it costs no network and no clone, unlike bench/.
//
// KNOWN LIMIT, and it bit during construction. A four-file fixture does not reproduce real BM25
// dynamics, so a case can fail here and pass on a real repository, or the reverse. The first draft
// of the abbreviation case used the query "where do we handle authentication"; the fixture ranked
// the logging cluster, while entireio/cli ranked handleClaudeCodePostTodo first and auth fourth —
// two different answers, neither of them the fixture's. The case was narrowed to a query the
// fixture can actually discriminate.
//
// So: a FAILURE here is a signal worth chasing, and a PASS is not proof the ranking is good on real
// code. Treat this as a regression tripwire, not a quality score. A corpus-scale eval over real
// repositories is still the missing piece.

// searchEvalCase is one query and the two clusters whose relative order is the verdict.
type searchEvalCase struct {
	name string
	// why records what regression this case exists to catch, so a future failure is diagnosable
	// without archaeology.
	why string
	// query is written the way the tool documents its input: the task in one plain sentence.
	query string
	// gold and distractor identify clusters by substring of the qualified name.
	gold       []string
	distractor []string
	// knownGap, when set, skips the case and states what is missing. A case is parked here only
	// when its expectation is CORRECT and the code does not meet it — never to quiet a case whose
	// expectation turned out to be wrong. Those get deleted or rewritten instead.
	knownGap string
}

func searchEvalCorpus(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()

	writeFile(t, repo, "auth.go", `package app

// Deliberately abbreviation-only. No comment or identifier here spells
// "authentication", "authenticate" or "credential" in full, so the only bridge from
// the prose query to this file is the alias table mapping the long form onto "auth".
// If a body word could carry the query, the case would pass with the table removed
// and would be testing nothing — which is exactly what the first draft did.

func runLogin(server string, user string) error {
	token, err := requestAuthToken(server, user)
	if err != nil {
		return err
	}
	return persistLogin(server, token)
}

func persistLogin(server string, token string) error {
	return storeAuthToken(server, token)
}

func newAuthCmd() string { return "auth" }

func validateAuthToken(token string) bool { return token != "" }

func requestAuthToken(server string, user string) (string, error) { return user + server, nil }

func storeAuthToken(server string, token string) error { return nil }
`)

	writeFile(t, repo, "logging.go", `package app

// RestoreLogsOnly restores the session log files without touching the working tree.
// Only the logs are written; every other artifact of the checkpoint is left alone.
func RestoreLogsOnly(dir string) error {
	entries, err := collectLogEntries(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := writeLogEntry(dir, entry); err != nil {
			return err
		}
	}
	return nil
}

// newDoctorLogsCmd builds the command that tails the diagnostic logs.
func newDoctorLogsCmd() string { return "logs" }

// collectLogEntries reads every log entry recorded for the session.
func collectLogEntries(dir string) ([]string, error) { return nil, nil }

// writeLogEntry appends one log entry, encoded as JSON, to the log file.
func writeLogEntry(dir string, entry string) error { return nil }
`)

	writeFile(t, repo, "config.go", `package app

// Abbreviation-only for the same reason as auth.go: nothing here spells "configuration".

func loadConfig(path string) (string, error) { return path, nil }

func applyConfigDefaults(cfg string) string { return cfg }
`)

	// Hugo-shaped: the case searchNameTermCoverage was calibrated on. Two domain nouns in the
	// gold name, one in each rival, so a change that stops rewarding name coverage shows up here.
	writeFile(t, repo, "pages.go", `package app

type pageMap struct{}

// getPagesInSection returns the child pages collected under one section of the site.
func (m *pageMap) getPagesInSection(section string) []string { return nil }

type Site struct{}

// Sections lists the sections of the site.
func (s *Site) Sections() []string { return nil }

type Pages struct{}

// Reverse returns the pages in reverse order.
func (p *Pages) Reverse() []string { return nil }
`)
	return repo
}

func searchEvalCases() []searchEvalCase {
	return []searchEvalCase{
		{
			name:       "separated phrasal verb finds login, not logging",
			why:        "#247 headline. 'logs a user in' splits the verb around its object, so the joined token only exists if the phrasal-verb scan is separable.",
			query:      "the main authentication function that logs a user in",
			gold:       []string{"runLogin", "persistLogin", "RecordLoginContext"},
			distractor: []string{"RestoreLogsOnly", "newDoctorLogsCmd", "writeLogEntry"},
		},
		{
			name:       "prose abbreviation reaches the code spelling",
			why:        "#247 defect 1. Code says auth, prose says authentication, and the matcher tolerated only the plural.",
			query:      "authentication",
			gold:       []string{"newAuthCmd", "validateAuthToken", "requestAuthToken"},
			distractor: []string{"RestoreLogsOnly", "newDoctorLogsCmd"},
		},
		{
			name:       "configuration reaches config",
			why:        "Same abbreviation class as auth, different word, so the fix is not overfitted to one entry.",
			query:      "configuration",
			gold:       []string{"loadConfig", "applyConfigDefaults"},
			distractor: []string{"RestoreLogsOnly", "runLogin"},
		},
		{
			name:       "plural configuration reaches config",
			why:        "Alias retrieval must include singular forms derived from plural prose.",
			query:      "configurations",
			gold:       []string{"loadConfig", "applyConfigDefaults"},
			distractor: []string{"RestoreLogsOnly", "runLogin"},
		},
		{
			name:       "noun plus preposition must not become a phrasal verb",
			why:        "#247 defect 3 false positive. Joining 'the log in json' put login code at ranks 1-3 of a logging query.",
			query:      "write the log entry in json format",
			gold:       []string{"writeLogEntry", "collectLogEntries", "RestoreLogsOnly"},
			distractor: []string{"runLogin", "persistLogin", "RecordLoginContext"},
		},
		{
			name:       "hugo name-coverage case still holds",
			why:        "searchNameTermCoverage was calibrated on gohugoio/hugo#12171; two domain nouns in the name must still beat one.",
			query:      "parent pages collection filter by section path prefix",
			gold:       []string{"getPagesInSection"},
			distractor: []string{"Sections", "Reverse"},
		},
	}
}

// rankOfCluster returns the best (lowest) rank at which any member of cluster appears, and whether
// the cluster appeared at all.
func rankOfCluster(results []SearchResult, cluster []string) (int, bool) {
	for index, result := range results {
		name := result.QualifiedName
		if name == "" {
			name = result.SymbolName
		}
		for _, member := range cluster {
			if strings.Contains(name, member) {
				return index + 1, true
			}
		}
	}
	return 0, false
}

func TestSearchRetrievalEval(t *testing.T) {
	t.Parallel()
	repo := searchEvalCorpus(t)
	for _, tc := range searchEvalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.knownGap != "" {
				t.Skipf("known gap: %s\nwhy this case exists: %s", tc.knownGap, tc.why)
			}
			response, err := SearchRepository(context.Background(), repo, "eval", tc.query, SearchOptions{
				Worktree:     true,
				Profile:      ProfileFull,
				TopK:         10,
				DisableCache: true,
			})
			if err != nil {
				t.Fatalf("search failed: %v", err)
			}
			goldRank, goldFound := rankOfCluster(response.Results, tc.gold)
			if !goldFound {
				t.Fatalf("gold cluster %v absent from top %d\nwhy this case exists: %s\ngot: %s",
					tc.gold, len(response.Results), tc.why, formatEvalRanking(response.Results))
			}
			distractorRank, distractorFound := rankOfCluster(response.Results, tc.distractor)
			if distractorFound && distractorRank < goldRank {
				t.Fatalf("distractor %v outranks gold %v (%d vs %d)\nwhy this case exists: %s\ngot: %s",
					tc.distractor, tc.gold, distractorRank, goldRank, tc.why,
					formatEvalRanking(response.Results))
			}
		})
	}
}

// formatEvalRanking renders the ranking a failing case actually produced, so the failure message
// is a diagnosis rather than a prompt to go re-run the query by hand.
func formatEvalRanking(results []SearchResult) string {
	var b strings.Builder
	for index, result := range results {
		name := result.QualifiedName
		if name == "" {
			name = result.SymbolName
		}
		fmt.Fprintf(&b, "\n  %2d  %6.2f  %s", index+1, result.Score, strings.TrimSpace(name))
	}
	return b.String()
}
