package safepath

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		allow   bool
		want    string
		wantErr error
	}{
		{name: "simple", in: "internal/app/main.go", want: "internal/app/main.go"},
		{name: "backslashes", in: "internal\\app\\main.go", want: "internal/app/main.go"},
		{name: "dot prefix cleaned", in: "./internal/main.go", want: "internal/main.go"},
		{name: "dot allowed", in: ".", allow: true, want: "."},
		{name: "dot rejected", in: ".", allow: false, wantErr: ErrInvalidPath},
		{name: "empty", in: "", wantErr: ErrInvalidPath},
		{name: "traversal component", in: "a/../../etc/passwd", wantErr: ErrInvalidPath},
		{name: "leading traversal", in: "../secrets", wantErr: ErrInvalidPath},
		{name: "absolute", in: "/etc/passwd", wantErr: ErrInvalidPath},
		{name: "windows drive absolute", in: "C:/Windows/System32", wantErr: ErrInvalidPath},
		{name: "nul byte", in: "a\x00b", wantErr: ErrInvalidPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.in, tc.allow)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Normalize(%q, %v) err = %v, want %v", tc.in, tc.allow, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q, %v) unexpected err = %v", tc.in, tc.allow, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q, %v) = %q, want %q", tc.in, tc.allow, got, tc.want)
			}
		})
	}
}

func TestRejectSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "sub", "file.txt")
	if err := os.WriteFile(regular, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RejectSymlinkComponents(root, regular); err != nil {
		t.Fatalf("regular path rejected: %v", err)
	}

	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	if err := RejectSymlinkComponents(root, outside); !errors.Is(err, ErrOutsideScope) {
		t.Fatalf("outside path err = %v, want ErrOutsideScope", err)
	}

	if err := RejectSymlinkComponents(root, filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing path err = %v, want fs.ErrNotExist", err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is unreliable on windows")
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "sub"), linkDir); err != nil {
		t.Fatal(err)
	}
	viaSymlink := filepath.Join(linkDir, "file.txt")
	if err := RejectSymlinkComponents(root, viaSymlink); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlinked component err = %v, want ErrSymlink", err)
	}
}
