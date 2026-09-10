package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
	scippb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// adr0005Path is the RFD whose freshness contract tells peregrine how to decide
// per-language whether to trust the SCIP feed. It is checked against the code the
// same way AGENTS.md is (see agentguide_test.go): a design document that other
// teams implement against is only useful while it describes the artifact that
// actually ships.
const adr0005Path = "0005-converge-code-search.md"

// adr0005FreshnessItem2 returns the freshness-contract item that describes what
// the omission note carries, isolated from the rest of the RFD so a field name
// mentioned in some unrelated paragraph cannot satisfy the check below.
func adr0005FreshnessItem2(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "adr", adr0005Path))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	const start = "2. Per-language trust is driven by the omission note"
	begin := strings.Index(text, start)
	if begin < 0 {
		t.Fatalf("%s no longer has a freshness-contract item 2; this test is pinned to it", adr0005Path)
	}
	end := strings.Index(text[begin:], "\n3. ")
	if end < 0 {
		t.Fatalf("%s freshness-contract item 2 has no item 3 after it", adr0005Path)
	}
	return text[begin : begin+end]
}

// TestADR0005NamesTheFailureRecordItActuallyGets is the docs/spec consistency
// check for a mismatch that was live: item 2 said "only records say which
// language and which file", so a consumer reading the RFD would have gone
// looking for a language on a failure record that has never had one. It listed
// the record as carrying "path and code" while the shipped record is five
// fields.
//
// The assertion is deliberately driven off the struct rather than off a fixed
// list: adding a field to sem.PartialFailure (a `language`, one day) makes this
// fail until the RFD paragraph that describes the record is brought with it,
// which is the direction the drift actually runs.
func TestADR0005NamesTheFailureRecordItActuallyGets(t *testing.T) {
	t.Parallel()
	item := adr0005FreshnessItem2(t)
	recordType := reflect.TypeOf(sem.PartialFailure{})
	named := 0
	for i := range recordType.NumField() {
		tag := strings.Split(recordType.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		named++
		if !strings.Contains(item, "`"+tag+"`") {
			t.Errorf("the RFD's per-language fallback contract never names `%s`, which every failure record "+
				"in the feed carries; a consumer implementing from this paragraph does not know the field is there",
				tag)
		}
	}
	if named == 0 {
		t.Fatal("sem.PartialFailure has no JSON fields; this test asserted nothing")
	}
	// The specific false claim. A record with no language field cannot be the
	// thing that says which language, and saying it does sends the consumer
	// after a field to read instead of the join it has to perform.
	if _, hasLanguage := recordType.FieldByNameFunc(func(name string) bool { return name == "Language" }); !hasLanguage {
		if strings.Contains(item, "records say which language") {
			t.Error("the RFD still says the failure records say which language, and sem.PartialFailure has no " +
				"language field")
		}
	}
}

// TestSCIPFeedResolvesAFailingFilesLanguage is the mechanism the corrected
// paragraph prescribes, run end to end: a consumer holding only the artifact
// pair must be able to get from a failure record to the language it should stop
// trusting. It joins partial_failures[].file_path to the SCIP Document of the
// same relative_path and reads that Document's language.
//
// It is the reason the RFD was corrected towards the implementation rather than
// the other way round: the information the contract needs is already in the
// artifact, so no field and no format bump is required to deliver it.
func TestSCIPFeedResolvesAFailingFilesLanguage(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	write(t, repo, "main.go", "package sample\n\nfunc caller() { callee() }\nfunc callee() {}\n")
	// One very long line is what the provider calls minified, so this produces a
	// real E_MINIFIED failure record naming a TypeScript file without needing a
	// multi-megabyte fixture.
	write(t, repo, "bundle.ts", "export const bundled="+strings.Repeat("\"x\"+", 4000)+"\"x\";\n")

	var stdout, stderr bytes.Buffer
	if err := Run(t.Context(), Options{
		Version: "scip-test",
		Env:     EntireEnv{RepoRoot: repo},
		Stdout:  &stdout,
		Stderr:  &stderr,
	}, []string{"snapshot", "--repo", repo, "--worktree", "--format", "scip"}); err != nil {
		t.Fatal(err)
	}
	var note sem.SCIPOmissionNote
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &note); err != nil {
		t.Fatalf("stderr omission note is not JSON: %q: %v", stderr.String(), err)
	}
	if len(note.PartialFailures) == 0 {
		t.Fatalf("fixture produced no failure record, so the join is not exercised: %#v", note)
	}
	var index scippb.Index
	if err := proto.Unmarshal(stdout.Bytes(), &index); err != nil {
		t.Fatalf("scip output is not a valid Index protobuf: %v", err)
	}
	documents := map[string]*scippb.Document{}
	for _, doc := range index.GetDocuments() {
		documents[doc.GetRelativePath()] = doc
	}
	joined := 0
	for _, failure := range note.PartialFailures {
		if failure.FilePath == "" {
			continue
		}
		doc := documents[failure.FilePath]
		if doc == nil {
			t.Errorf("failure %s names %q and the index has no document for it, so a consumer cannot tell which "+
				"language to stop trusting", failure.Code, failure.FilePath)
			continue
		}
		if doc.GetLanguage() == "" {
			t.Errorf("failure %s names %q whose document carries no language", failure.Code, failure.FilePath)
			continue
		}
		joined++
	}
	if joined == 0 {
		t.Fatalf("no failure record resolved to a language: failures=%#v documents=%v", note.PartialFailures, documents)
	}
	// And the tiers the RFD tells the ingestion step to partition by must know
	// that language, or the resolved name has nothing to be looked up in.
	if len(note.LanguageTiers) == 0 {
		t.Fatalf("the note carries no language tiers: %#v", note)
	}
	if _, ok := note.LanguageTiers["TypeScript"]; !ok {
		t.Fatalf("language tiers omit the failing file's language: %v", note.LanguageTiers)
	}
}
