package contextbuilder

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrUnsafeRepositoryPath = errors.New("contextbuilder: unsafe repository path")

func normalizeRepositoryRelativePath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	windowsAbsolute := len(value) >= 3 &&
		((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
		value[1] == ':' && value[2] == '/'
	if value == "" || strings.ContainsRune(value, 0) || windowsAbsolute ||
		filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", ErrUnsafeRepositoryPath
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", ErrUnsafeRepositoryPath
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", ErrUnsafeRepositoryPath
	}
	return clean, nil
}

func secureRepositoryPath(repoRoot, relative string) (string, error) {
	normalized, err := normalizeRepositoryRelativePath(relative)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("%w: resolve repository root: %v", ErrUnsafeRepositoryPath, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve repository root: %v", ErrUnsafeRepositoryPath, err)
	}
	target := filepath.Join(root, filepath.FromSlash(normalized))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrUnsafeRepositoryPath
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: symlink component %s", ErrUnsafeRepositoryPath, normalized)
		}
	}
	return target, nil
}

func readRepositoryFile(repoRoot, relative string) ([]byte, error) {
	path, err := secureRepositoryPath(repoRoot, relative)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func validateRepositoryPaths(repoRoot string, paths []string) error {
	for _, path := range paths {
		if _, err := secureRepositoryPath(repoRoot, path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w %q: %v", ErrUnsafeRepositoryPath, path, err)
		}
	}
	return nil
}
