package sem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoreRuleLoadingAcceptsBoundaries(t *testing.T) {
	t.Run("file bytes", func(t *testing.T) {
		repo := t.TempDir()
		line := "#" + strings.Repeat("x", maxIgnoreRuleBytes-2) + "\n"
		content := strings.Repeat(line, maxIgnoreFileBytes/len(line))
		if len(content) != maxIgnoreFileBytes {
			t.Fatalf("boundary fixture has %d bytes, want %d", len(content), maxIgnoreFileBytes)
		}
		writeIgnoreLimitFile(t, filepath.Join(repo, ".gitignore"), content)
		if _, err := loadWorktreeIgnoreMatcher(repo, nil, nil); err != nil {
			t.Fatalf("exact-byte-limit root .gitignore was rejected: %v", err)
		}
	})

	t.Run("line bytes", func(t *testing.T) {
		var matcher ignoreMatcher
		line := strings.Repeat("x", maxIgnoreRuleBytes)
		if err := matcher.loadContent(line, false, repoIgnoreOrigin(".gitignore")); err != nil {
			t.Fatalf("exact-line-limit rule was rejected: %v", err)
		}
		if matcher.parsedRuleCount != 1 {
			t.Fatalf("exact-line-limit rule count = %d, want 1", matcher.parsedRuleCount)
		}
	})

	t.Run("cumulative parsed rules", func(t *testing.T) {
		var matcher ignoreMatcher
		if err := matcher.loadContent(strings.Repeat("a\n", maxIgnoreParsedRules), false, repoIgnoreOrigin(".gitignore")); err != nil {
			t.Fatalf("exact parsed-rule limit was rejected: %v", err)
		}
		if matcher.parsedRuleCount != maxIgnoreParsedRules {
			t.Fatalf("parsed rule count = %d, want %d", matcher.parsedRuleCount, maxIgnoreParsedRules)
		}
		if err := matcher.loadContent("b\n", false, repoIgnoreOrigin(".gitignore")); err == nil ||
			!strings.Contains(err.Error(), "parsed rules") {
			t.Fatalf("one cumulative rule over the limit error = %v", err)
		}
	})
}

func TestIgnoreRuleLoadingRejectsEveryExternalSurface(t *testing.T) {
	tests := []struct {
		name      string
		content   func() string
		configure func(t *testing.T, repo, content string) func() error
		wantLabel string
		wantLimit string
	}{
		{
			name:    "root gitignore oversized file",
			content: func() string { return strings.Repeat("# x\n", maxIgnoreFileBytes/4+1) },
			configure: func(t *testing.T, repo, content string) func() error {
				writeIgnoreLimitFile(t, filepath.Join(repo, ".gitignore"), content)
				return func() error {
					_, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
					return err
				}
			},
			wantLabel: "ignore file",
			wantLimit: "file exceeds",
		},
		{
			name:    "graphignore overlong line",
			content: func() string { return "#" + strings.Repeat("x", maxIgnoreRuleBytes) },
			configure: func(t *testing.T, repo, content string) func() error {
				writeIgnoreLimitFile(t, filepath.Join(repo, graphIgnoreFileName), content)
				return func() error {
					_, err := loadExplicitIgnoreMatcher(repo, nil, nil)
					return err
				}
			},
			wantLabel: "ignore file",
			wantLimit: "rule line exceeds",
		},
		{
			name:    "info exclude too many rules",
			content: func() string { return strings.Repeat("a\n", maxIgnoreParsedRules+1) },
			configure: func(t *testing.T, repo, content string) func() error {
				writeIgnoreLimitFile(t, filepath.Join(repo, ".git", "info", "exclude"), content)
				return func() error {
					_, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
					return err
				}
			},
			wantLabel: "ignore file",
			wantLimit: "parsed rules",
		},
		{
			name:    "explicit ignore overlong line",
			content: func() string { return "#" + strings.Repeat("x", maxIgnoreRuleBytes) },
			configure: func(t *testing.T, repo, content string) func() error {
				path := filepath.Join(repo, "explicit.ignore")
				writeIgnoreLimitFile(t, path, content)
				return func() error {
					_, err := loadExplicitIgnoreMatcher(repo, []string{path}, nil)
					return err
				}
			},
			wantLabel: "ignore file",
			wantLimit: "rule line exceeds",
		},
		{
			name:    "explicit include too many rules",
			content: func() string { return strings.Repeat("a\n", maxIgnoreParsedRules+1) },
			configure: func(t *testing.T, repo, content string) func() error {
				path := filepath.Join(repo, "explicit.include")
				writeIgnoreLimitFile(t, path, content)
				return func() error {
					_, err := loadExplicitIgnoreMatcher(repo, nil, []string{path})
					return err
				}
			},
			wantLabel: "include file",
			wantLimit: "parsed rules",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := t.TempDir()
			run := test.configure(t, repo, test.content())
			err := run()
			if err == nil {
				t.Fatal("oversized ignore input was accepted")
			}
			if !strings.Contains(err.Error(), test.wantLabel) ||
				!strings.Contains(err.Error(), test.wantLimit) {
				t.Fatalf("error = %q, want %q and %q", err, test.wantLabel, test.wantLimit)
			}
		})
	}
}

func TestIgnoreRuleLoadingOptionalAndRequiredLabels(t *testing.T) {
	repo := t.TempDir()
	if _, err := loadWorktreeIgnoreMatcher(repo, nil, nil); err != nil {
		t.Fatalf("absent optional ignore files were fatal: %v", err)
	}

	for _, test := range []struct {
		name         string
		ignoreFiles  []string
		includeFiles []string
		want         string
	}{
		{name: "required ignore", ignoreFiles: []string{"missing.ignore"}, want: "ignore file"},
		{name: "required include", includeFiles: []string{"missing.include"}, want: "include file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadExplicitIgnoreMatcher(repo, test.ignoreFiles, test.includeFiles)
			if err == nil || !strings.Contains(err.Error(), test.want) ||
				!strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("missing required input error = %v, want %q does not exist", err, test.want)
			}
		})
	}
}

func TestIgnoreRuleLoadingCountsRulesAcrossFiles(t *testing.T) {
	repo := t.TempDir()
	writeIgnoreLimitFile(
		t,
		filepath.Join(repo, graphIgnoreFileName),
		strings.Repeat("a\n", maxIgnoreParsedRules/2),
	)
	explicit := filepath.Join(repo, "explicit.ignore")
	writeIgnoreLimitFile(
		t,
		explicit,
		strings.Repeat("b\n", maxIgnoreParsedRules-maxIgnoreParsedRules/2+1),
	)

	_, err := loadExplicitIgnoreMatcher(repo, []string{explicit}, nil)
	if err == nil || !strings.Contains(err.Error(), "ignore file") ||
		!strings.Contains(err.Error(), "parsed rules") {
		t.Fatalf("cross-file rule limit error = %v", err)
	}
}

func writeIgnoreLimitFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
