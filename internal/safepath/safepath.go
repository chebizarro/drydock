// Package safepath provides the repository-relative path confinement shared by
// every package that resolves untrusted paths against a trusted root. Keeping a
// single implementation means a hardening fix (UNC paths, unicode normalization,
// case-insensitive filesystems) lands everywhere at once instead of leaving a
// forgotten copy exploitable.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrInvalidPath is returned when a path is empty, absolute, contains a NUL
	// byte, or escapes its root via "..".
	ErrInvalidPath = errors.New("safepath: invalid path")
	// ErrOutsideScope is returned when a target resolves outside its root.
	ErrOutsideScope = errors.New("safepath: path outside scope")
	// ErrSymlink is returned when a path component is a symlink.
	ErrSymlink = errors.New("safepath: symlink component not allowed")
)

// Normalize cleans a repository-relative path and rejects anything that could
// escape the repository root: empty input, NUL bytes, Windows drive-absolute
// paths, OS-absolute paths, volume names, and any ".." component. When allowDot
// is false a bare "." is rejected as well. The returned path uses forward
// slashes.
func Normalize(path string, allowDot bool) (string, error) {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	windowsAbsolute := len(path) >= 3 &&
		((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) &&
		path[1] == ':' && path[2] == '/'
	if path == "" || strings.ContainsRune(path, 0) || windowsAbsolute || filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return "", ErrInvalidPath
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", ErrInvalidPath
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", ErrInvalidPath
	}
	if clean == "." && !allowDot {
		return "", ErrInvalidPath
	}
	return clean, nil
}

// RejectSymlinkComponents verifies that target is within root and that no path
// component between them is a symlink. It returns ErrOutsideScope when target
// escapes root, ErrSymlink when a component is a symlink, and the underlying
// os.Lstat error (e.g. fs.ErrNotExist) for any other stat failure. Both root and
// target are expected to be resolved, absolute paths.
func RejectSymlinkComponents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrOutsideScope
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, rel)
		}
	}
	return nil
}
