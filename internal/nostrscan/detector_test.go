package nostrscan

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectorProfilesGolden(t *testing.T) {
	tests := []struct {
		name string
		role Role
	}{
		{name: "client", role: RoleClient},
		{name: "relay", role: RoleRelay},
		{name: "signer", role: RoleSigner},
		{name: "library", role: RoleLibrary},
		{name: "dvm", role: RoleDVM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := fixtureRepo(t, tt.name)
			profile, err := Detect(
				context.Background(),
				repo,
				"HEAD",
				WithCacheDir(t.TempDir()),
				WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !profile.IsNostr {
				t.Fatalf("profile not classified as Nostr: %+v", profile)
			}
			if !containsRole(profile.Roles, tt.role) {
				t.Fatalf("roles %v do not include %q", profile.Roles, tt.role)
			}

			got, err := json.MarshalIndent(profile, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := filepath.Join("testdata", tt.name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("profile mismatch (-want +got):\nwant:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}

func TestDependencyManifests(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
	}{
		{name: "go-nostr", path: "go.mod", content: "module test\nrequire github.com/nbd-wtf/go-nostr v0.50.0\n"},
		{name: "fiatjaf", path: "go.mod", content: "module test\nrequire fiatjaf.com/nostr v1.0.0\n"},
		{name: "javascript", path: "package.json", content: `{"dependencies":{"@nostr-dev-kit/ndk":"^3.0.0"}}`},
		{name: "rust", path: "Cargo.toml", content: "[dependencies]\nnostr-sdk = \"0.42\"\n"},
		{name: "dart", path: "pubspec.yaml", content: "dependencies:\n  nostr_tools: ^1.0.0\n"},
		{name: "swift", path: "Package.swift", content: `.package(url: "https://example.test/NostrSDK", from: "1.0.0")`},
		{name: "kotlin", path: "build.gradle.kts", content: `implementation("com.vitorpamplona.quartz:core:1.0.0")`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initRepo(t, map[string]string{tt.path: tt.content})
			profile, err := Detect(
				context.Background(),
				repo,
				"HEAD",
				WithCacheDir(t.TempDir()),
				WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !profile.IsNostr || profile.Confidence < DefaultMinConfidence {
				t.Fatalf("manifest was not classified as Nostr: %+v", profile)
			}
			if len(profile.Evidence) == 0 || profile.Evidence[0].Kind != MarkerDependency {
				t.Fatalf("missing dependency evidence: %+v", profile.Evidence)
			}
		})
	}
}

func TestDetectorCachesByCheckoutTree(t *testing.T) {
	repo := fixtureRepo(t, "client")
	cacheDir := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	detector := New(WithCacheDir(cacheDir), WithLogger(logger))

	first, err := detector.Detect(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	second, err := detector.Detect(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if first.Confidence != second.Confidence || len(first.Evidence) != len(second.Evidence) {
		t.Fatalf("cached profile differs: first=%+v second=%+v", first, second)
	}
	if !strings.Contains(logs.String(), `"cached":true`) {
		t.Fatalf("expected cache-hit audit log, got %s", logs.String())
	}
	matches, err := filepath.Glob(filepath.Join(cacheDir, "nostr", "profiles", "*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("cache files = %v, err = %v", matches, err)
	}
}

func TestDefaultCacheIsAlongsideCodemap(t *testing.T) {
	repo := fixtureRepo(t, "client")
	_, err := Detect(
		context.Background(),
		repo,
		"HEAD",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(repo, ".git", "drydock-codemap", "nostr", "profiles", "*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("default cache files = %v, err = %v", matches, err)
	}
}

func TestBelowFloorSkipsAndLogs(t *testing.T) {
	repo := fixtureRepo(t, "non_nostr")
	var logs bytes.Buffer
	profile, err := Detect(
		context.Background(),
		repo,
		"HEAD",
		WithCacheDir(t.TempDir()),
		WithMinConfidence(0.60),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	if profile.IsNostr {
		t.Fatalf("ordinary repository classified as Nostr: %+v", profile)
	}
	logged := logs.String()
	if !strings.Contains(logged, "nostr lens skipped") ||
		!strings.Contains(logged, "confidence below minimum") {
		t.Fatalf("missing explicit skip reason in log: %s", logged)
	}
}

func TestProtocolMarkersRequireCorroboration(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"README.md": "Integration follows NIP-01.",
	})
	var logs bytes.Buffer
	profile, err := Detect(
		context.Background(),
		repo,
		"HEAD",
		WithCacheDir(t.TempDir()),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	if profile.IsNostr || profile.Confidence >= DefaultMinConfidence {
		t.Fatalf("single documentation marker should not enable lens: %+v", profile)
	}
	if !strings.Contains(logs.String(), "nostr lens skipped") {
		t.Fatalf("skip was not logged: %s", logs.String())
	}
}

func TestShouldRunLogsExplicitFloorDecision(t *testing.T) {
	var logs bytes.Buffer
	profile := NostrProfile{Confidence: 0.59}
	if ShouldRun(profile, 0.60, slog.New(slog.NewJSONHandler(&logs, nil))) {
		t.Fatal("profile below floor enabled")
	}
	if !strings.Contains(logs.String(), "nostr lens skipped") {
		t.Fatalf("skip was not logged: %s", logs.String())
	}
}

func containsRole(roles []Role, want Role) bool {
	for _, role := range roles {
		if role == want {
			return true
		}
	}
	return false
}

func fixtureRepo(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join("testdata", name)
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return initRepo(t, files)
}

func initRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	for path, content := range files {
		fullPath := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "test@example.test")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "fixture")
	return repo
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
