package sem

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
)

// capturedIgnoreFile is one immutable input to the ignore policy used by a
// cache transaction. present distinguishes an absent optional .graphignore
// from a present empty file; explicit files are always present.
type capturedIgnoreFile struct {
	path    string
	content []byte
	present bool
}

// capturedIgnorePolicy owns every working-tree policy byte that can shape a
// committed snapshot. A cache transaction captures it once, then uses this
// same value for key derivation and matcher construction even if the files on
// disk change while Git is listing or reading the committed tree.
type capturedIgnorePolicy struct {
	absRepo      string
	graphIgnore  capturedIgnoreFile
	ignoreFiles  []capturedIgnoreFile
	includeFiles []capturedIgnoreFile
}

const (
	// Capturing cache policy retains raw input bytes until keying and snapshot
	// construction finish. Bound both dimensions so repeated comment-only files
	// cannot consume memory without reaching the matcher's parsed-rule ceiling.
	maxCapturedIgnoreInputs = 128
	maxCapturedIgnoreBytes  = 4 << 20
)

type ignorePolicyCaptureBudget struct {
	inputs int
	bytes  int
}

func (budget *ignorePolicyCaptureBudget) add(path string, content []byte) error {
	if budget.inputs >= maxCapturedIgnoreInputs {
		return fmt.Errorf("ignore policy exceeds %d captured inputs (at %q)", maxCapturedIgnoreInputs, path)
	}
	if len(content) > maxCapturedIgnoreBytes-budget.bytes {
		return fmt.Errorf("ignore policy exceeds %d captured bytes (at %q)", maxCapturedIgnoreBytes, path)
	}
	budget.inputs++
	budget.bytes += len(content)
	return nil
}

// CaptureProviderCachePolicy freezes the bounded external ignore inputs in
// options for one cache transaction. Carry the returned options through cache
// lookup, snapshot construction, and cache storage; its policy bytes are not
// read from disk again during that transaction.
func CaptureProviderCachePolicy(repo string, options ProviderSnapshotOptions) (ProviderSnapshotOptions, error) {
	if err := validateCapturedIgnoreInputCount(options.IgnoreFiles, options.IncludeFiles); err != nil {
		return ProviderSnapshotOptions{}, err
	}
	options = cloneProviderSnapshotOptions(options)
	if options.Profile == "" {
		options.Profile = ProfileFull
	}
	// Freeze the resolved file limit alongside policy bytes. MaxFiles=0
	// consults ENTIRE_GRAPH_MAX_FILES; resolving it once prevents an environment
	// change from shaping the build differently from the already-derived key.
	options.MaxFiles = resolveMaxSourceFiles(options.MaxFiles)
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return ProviderSnapshotOptions{}, err
	}
	policy, err := captureIgnorePolicy(absRepo, options.IgnoreFiles, options.IncludeFiles)
	if err != nil {
		return ProviderSnapshotOptions{}, err
	}
	options.cachePolicy = policy
	return options, nil
}

func validateCapturedIgnoreInputCount(ignoreFiles, includeFiles []string) error {
	if len(ignoreFiles) > maxCapturedIgnoreInputs || len(includeFiles) > maxCapturedIgnoreInputs-len(ignoreFiles) {
		return fmt.Errorf("ignore policy exceeds %d captured inputs", maxCapturedIgnoreInputs)
	}
	return nil
}

func cloneProviderSnapshotOptions(options ProviderSnapshotOptions) ProviderSnapshotOptions {
	options.IgnoreFiles = append([]string(nil), options.IgnoreFiles...)
	options.IncludeFiles = append([]string(nil), options.IncludeFiles...)
	options.OnlyFiles = append([]string(nil), options.OnlyFiles...)
	return options
}

func ensureProviderCachePolicy(repo string, options ProviderSnapshotOptions) (ProviderSnapshotOptions, error) {
	if options.cachePolicy == nil {
		return CaptureProviderCachePolicy(repo, options)
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return ProviderSnapshotOptions{}, err
	}
	if err := options.cachePolicy.validate(absRepo, options.IgnoreFiles, options.IncludeFiles); err != nil {
		return ProviderSnapshotOptions{}, err
	}
	return options, nil
}

func captureIgnorePolicy(absRepo string, ignoreFiles, includeFiles []string) (*capturedIgnorePolicy, error) {
	policy := &capturedIgnorePolicy{absRepo: filepath.Clean(absRepo)}
	budget := &ignorePolicyCaptureBudget{}
	var err error
	policy.ignoreFiles, err = captureExplicitIgnoreFiles(absRepo, ignoreFiles, false, budget)
	if err != nil {
		return nil, err
	}
	policy.includeFiles, err = captureExplicitIgnoreFiles(absRepo, includeFiles, true, budget)
	if err != nil {
		return nil, err
	}
	graphIgnorePath := filepath.Join(absRepo, graphIgnoreFileName)
	// The same no-follow reader loadPath uses for a repository-controlled ignore
	// file. Without it a cache-enabled search follows a symlinked .graphignore
	// that an uncached search refuses, so the two enforce different policies and
	// the cached one echoes an outside file's lines through
	// repo_ignored.sample[].rule as though the repository had written them. It is
	// one call rather than a gate plus a separate read because two resolutions of
	// the path are two different objects under a concurrent rename.
	content, present, err := readRepoIgnoreFile(graphIgnorePath, ignoreFileLabel(false), false)
	if err != nil {
		return nil, err
	}
	if present {
		if err := budget.add(graphIgnorePath, content); err != nil {
			return nil, err
		}
	}
	policy.graphIgnore = capturedIgnoreFile{
		path:    graphIgnorePath,
		content: content,
		present: present,
	}
	return policy, nil
}

func captureExplicitIgnoreFiles(absRepo string, paths []string, includeMode bool, budget *ignorePolicyCaptureBudget) ([]capturedIgnoreFile, error) {
	if len(paths) > maxCapturedIgnoreInputs-budget.inputs {
		return nil, fmt.Errorf("ignore policy exceeds %d captured inputs", maxCapturedIgnoreInputs)
	}
	captured := make([]capturedIgnoreFile, 0, len(paths))
	for _, rulePath := range paths {
		resolved := resolveIgnorePath(absRepo, rulePath)
		content, _, err := readBoundedRegularFile(
			resolved,
			ignoreFileLabel(includeMode),
			true,
			maxIgnoreFileBytes,
		)
		if err != nil {
			return nil, err
		}
		if err := budget.add(resolved, content); err != nil {
			return nil, err
		}
		captured = append(captured, capturedIgnoreFile{
			path:    resolved,
			content: content,
			present: true,
		})
	}
	return captured, nil
}

func resolveIgnorePath(absRepo, rulePath string) string {
	if !filepath.IsAbs(rulePath) {
		rulePath = filepath.Join(absRepo, rulePath)
	}
	return filepath.Clean(rulePath)
}

func (policy *capturedIgnorePolicy) validate(absRepo string, ignoreFiles, includeFiles []string) error {
	if policy == nil {
		return fmt.Errorf("captured ignore policy is nil")
	}
	if filepath.Clean(absRepo) != policy.absRepo {
		return fmt.Errorf("captured ignore policy belongs to repository %q, not %q", policy.absRepo, absRepo)
	}
	if !capturedIgnorePathsMatch(policy.ignoreFiles, absRepo, ignoreFiles) ||
		!capturedIgnorePathsMatch(policy.includeFiles, absRepo, includeFiles) {
		return fmt.Errorf("provider ignore/include paths changed after cache policy capture")
	}
	return nil
}

func capturedIgnorePathsMatch(captured []capturedIgnoreFile, absRepo string, paths []string) bool {
	if len(captured) != len(paths) {
		return false
	}
	for index, rulePath := range paths {
		if captured[index].path != resolveIgnorePath(absRepo, rulePath) {
			return false
		}
	}
	return true
}

func cachePolicyForOptions(absRepo string, options ProviderSnapshotOptions) (*capturedIgnorePolicy, error) {
	if options.cachePolicy != nil {
		if err := options.cachePolicy.validate(absRepo, options.IgnoreFiles, options.IncludeFiles); err != nil {
			return nil, err
		}
		return options.cachePolicy, nil
	}
	return captureIgnorePolicy(absRepo, options.IgnoreFiles, options.IncludeFiles)
}

func (policy *capturedIgnorePolicy) matcher() (ignoreMatcher, error) {
	var matcher ignoreMatcher
	if policy.graphIgnore.present {
		if err := matcher.loadCaptured(policy.graphIgnore, false, graphIgnoreOrigin()); err != nil {
			return ignoreMatcher{}, err
		}
	}
	matcher.loadBuiltinSecretRules()
	for _, input := range policy.ignoreFiles {
		if err := matcher.loadCaptured(input, false, callerIgnoreOrigin(input.path)); err != nil {
			return ignoreMatcher{}, err
		}
	}
	for _, input := range policy.includeFiles {
		if err := matcher.loadCaptured(input, true, callerIgnoreOrigin(input.path)); err != nil {
			return ignoreMatcher{}, err
		}
	}
	return matcher, nil
}

func (matcher *ignoreMatcher) loadCaptured(input capturedIgnoreFile, includeMode bool, origin ignoreOrigin) error {
	if err := matcher.loadReader(bytes.NewReader(input.content), includeMode, origin); err != nil {
		return fmt.Errorf("read %s %q: %w", ignoreFileLabel(includeMode), input.path, err)
	}
	return nil
}

// writeCacheKey binds the ordered explicit files and the optional
// .graphignore to a cache key without consulting the filesystem.
func (policy *capturedIgnorePolicy) writeCacheKey(writer io.Writer) {
	for groupIndex, group := range [][]capturedIgnoreFile{policy.ignoreFiles, policy.includeFiles} {
		writeCacheKeyString(writer, "path-group", fmt.Sprintf("%d", groupIndex))
		for _, input := range group {
			writeCacheKeyString(writer, "rule-path", input.path)
			writeCacheKeyField(writer, "rule-content", input.content)
		}
	}
	if policy.graphIgnore.present {
		writeCacheKeyField(writer, "graphignore-content", policy.graphIgnore.content)
	} else {
		writeCacheKeyField(writer, "graphignore-missing", nil)
	}
}
