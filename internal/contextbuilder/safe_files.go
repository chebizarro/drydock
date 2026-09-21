package contextbuilder

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"git.sharegap.net/cascadia/drydock/internal/safepath"
)

var ErrUnsafeRepositoryPath = errors.New("contextbuilder: unsafe repository path")

func normalizeRepositoryRelativePath(value string) (string, error) {
	clean, err := safepath.Normalize(value, false)
	if err != nil {
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
	if err := safepath.RejectSymlinkComponents(root, target); err != nil {
		switch {
		case errors.Is(err, safepath.ErrOutsideScope):
			return "", ErrUnsafeRepositoryPath
		case errors.Is(err, safepath.ErrSymlink):
			return "", fmt.Errorf("%w: symlink component %s", ErrUnsafeRepositoryPath, normalized)
		default:
			return "", err
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
