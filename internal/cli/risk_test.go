package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func TestClassifyEntityRisk(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		change   sem.EntityChange
		evidence riskEvidence
		want     riskLevel
	}{
		{"broad callers", sem.EntityChange{Type: "body_changed"}, riskEvidence{DirectCallers: 10}, riskHigh},
		{"signature moderate callers", sem.EntityChange{Type: "signature_changed"}, riskEvidence{DirectCallers: 5}, riskHigh},
		{"signature no snapshot match", sem.EntityChange{Type: "signature_changed"}, riskEvidence{}, riskMedium},
		{"ordinary caller", sem.EntityChange{Type: "body_changed"}, riskEvidence{DirectCallers: 1}, riskMedium},
		{"isolated addition", sem.EntityChange{Type: "added"}, riskEvidence{}, riskLow},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := classifyEntityRisk(testCase.change, testCase.evidence); got != testCase.want {
				t.Fatalf("classifyEntityRisk() = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestCollectAffectedTestsUsesOnlyImpactRelationships(t *testing.T) {
	t.Parallel()
	impact := impactResponse{
		Callers: impactSection{Entries: []impactEntry{
			{Endpoint: neighborEndpoint{FilePath: "internal/service_test.go"}},
			{Endpoint: neighborEndpoint{FilePath: "internal/service.go"}},
		}},
		CoChanges: impactSection{Entries: []impactEntry{
			{Endpoint: neighborEndpoint{FilePath: "tests/test_api.py"}},
			{Endpoint: neighborEndpoint{FilePath: "docs/example.md"}},
		}},
		TypeConsumers: impactSection{Entries: []impactEntry{{Endpoint: neighborEndpoint{FilePath: "spec/not_used_test.go"}}}},
	}
	got := collectAffectedTests(impact)
	want := []string{"internal/service_test.go", "tests/test_api.py"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("affected tests = %#v, want %#v", got, want)
	}
}

func TestParseRiskFlags(t *testing.T) {
	t.Parallel()
	flags, err := parseRiskFlags([]string{"--base", "main", "--head", "HEAD", "--format", "json", "--max-entities", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if flags.Base != "main" || flags.Head != "HEAD" || flags.Format != "json" || flags.MaxEntities != 3 {
		t.Fatalf("flags = %#v", flags)
	}
	for _, args := range [][]string{
		{"--checkpoint", "abc", "--base", "HEAD~1"},
		{"--max-entities", "0"},
		{"--format", "yaml"},
	} {
		if _, err := parseRiskFlags(args); err == nil {
			t.Fatalf("parseRiskFlags(%q) unexpectedly succeeded", args)
		}
	}
}

func TestWriteRiskJSONShape(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	report := riskReport{FormatVersion: 1, OverallRisk: riskMedium, Entries: []riskEntry{}, RecommendedTests: []string{}}
	encoder := json.NewEncoder(&out)
	if err := encoder.Encode(report); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["overall_risk"] != "medium" || decoded["format_version"] != float64(1) {
		t.Fatalf("JSON report = %#v", decoded)
	}
}

func TestBuildRiskReportHandlesEmptyAndAmbiguousGraphResults(t *testing.T) {
	t.Parallel()
	empty := buildRiskReport(sem.Result{}, sem.ProviderSnapshot{}, riskFlags{MaxEntities: defaultRiskMaxEntities}, nil, true, "")
	if empty.EntitiesChanged != 0 || len(empty.Entries) != 0 || len(empty.Limitations) == 0 {
		t.Fatalf("empty report = %#v", empty)
	}

	diff := sem.Result{Files: []sem.FileChange{{Changes: []sem.EntityChange{{Name: "Handle", Kind: "function", Type: "body_changed"}}}}}
	snapshot := sem.ProviderSnapshot{Symbols: []sem.SymbolRecord{
		{ID: "one", Name: "Handle", QualifiedName: "one.Handle", Kind: "function", FilePath: "one.go", StartLine: 1},
		{ID: "two", Name: "Handle", QualifiedName: "two.Handle", Kind: "function", FilePath: "two.go", StartLine: 1},
	}}
	report := buildRiskReport(diff, snapshot, riskFlags{MaxEntities: defaultRiskMaxEntities}, nil, true, "")
	if len(report.Entries) != 1 || len(report.Entries[0].Limitations) == 0 || !strings.Contains(report.Entries[0].Limitations[0], "ambiguous") {
		t.Fatalf("ambiguous report = %#v", report)
	}
}

func TestBuildRiskReportOmitsMismatchedSnapshotEvidence(t *testing.T) {
	t.Parallel()
	diff := sem.Result{Files: []sem.FileChange{{Changes: []sem.EntityChange{{Name: "Target", Kind: "function", Type: "body_changed"}}}}}
	snapshot := sem.ProviderSnapshot{Symbols: []sem.SymbolRecord{
		{ID: "target", Name: "Target", Kind: "function", FilePath: "sample.go", StartLine: 1},
		{ID: "caller", Name: "Caller", Kind: "function", FilePath: "sample.go", StartLine: 3},
	}, Relations: []sem.RelationRecord{{Type: "CALLS", FromID: "caller", ToID: "target"}}}
	report := buildRiskReport(diff, snapshot, riskFlags{MaxEntities: defaultRiskMaxEntities}, nil, false, "graph evidence was omitted because the requested --head is not the current committed checkout")
	if len(report.Entries) != 1 || report.Entries[0].Evidence.DirectCallers != 0 {
		t.Fatalf("mismatched snapshot leaked graph evidence: %#v", report)
	}
	if !strings.Contains(strings.Join(report.Entries[0].Limitations, "\n"), "omitted") {
		t.Fatalf("entry does not explain omitted evidence: %#v", report.Entries[0])
	}
}

func TestRiskEvidenceMatchesHead(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "sample.go", "package sample\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	write(t, repo, "sample.go", "package sample\n\nfunc Current() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "current")

	snapshot := sem.ProviderSnapshot{Header: sem.SnapshotHeader{Tree: rev(t, repo, "HEAD^{tree}")}}
	if matched, limitation := riskEvidenceMatchesHead(t.Context(), repo, "HEAD", snapshot); !matched || limitation != "" {
		t.Fatalf("current head should match snapshot: matched=%t limitation=%q", matched, limitation)
	}
	if matched, limitation := riskEvidenceMatchesHead(t.Context(), repo, "HEAD~1", snapshot); matched || !strings.Contains(limitation, "omitted") {
		t.Fatalf("non-current head should omit evidence: matched=%t limitation=%q", matched, limitation)
	}
}

func TestWriteRiskTextIncludesWarnings(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	report := riskReport{
		OverallRisk:      riskLow,
		Entries:          []riskEntry{},
		RecommendedTests: []string{},
		Warnings: []sem.ProviderWarning{{
			Code:                 "W_ANALYSIS_BUDGET_EXCEEDED",
			FilePath:             "large.go",
			EffectOnCompleteness: "partial",
			Detail:               "analysis stopped before this file",
		}},
	}
	if err := writeRiskText(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WARNINGS", "W_ANALYSIS_BUDGET_EXCEEDED", "large.go", "partial", "analysis stopped before this file"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("text output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRiskEndToEndReportsGraphEvidenceAndTests(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "sample.go", "package sample\n\nfunc Target(value string) string { return value }\n\nfunc Caller() string { return Target(\"ok\") }\n")
	write(t, repo, "sample_test.go", "package sample\n\nimport \"testing\"\n\nfunc TestCaller(t *testing.T) { if Caller() == \"\" { t.Fatal(\"empty\") } }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	write(t, repo, "sample.go", "package sample\n\nfunc Target(value string, prefix string) string { return prefix + value }\n\nfunc Caller() string { return Target(\"ok\", \"\") }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "signature change")

	var stdout, stderr bytes.Buffer
	err := Run(t.Context(), Options{Version: "test", Env: EntireEnv{RepoRoot: repo, PluginDataDir: t.TempDir()}, Stdout: &stdout, Stderr: &stderr},
		[]string{"risk", "--base", "HEAD~1", "--head", "HEAD", "--format", "json"})
	if err != nil {
		t.Fatalf("risk: %v\nstderr: %s", err, stderr.String())
	}
	var report riskReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("risk JSON: %v\n%s", err, stdout.String())
	}
	if report.EntitiesChanged == 0 || report.EntitiesAnalyzed == 0 {
		t.Fatalf("empty risk report: %#v", report)
	}
	var target *riskEntry
	for index := range report.Entries {
		if report.Entries[index].Name == "Target" {
			target = &report.Entries[index]
			break
		}
	}
	if target == nil || target.Evidence.DirectCallers == 0 {
		t.Fatalf("Target lacks graph caller evidence: %#v", report.Entries)
	}
	if !strings.Contains(strings.Join(report.RecommendedTests, "\n"), "sample_test.go") {
		t.Fatalf("recommended tests = %#v, want sample_test.go", report.RecommendedTests)
	}
}
