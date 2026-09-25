package nostrscan

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/codemap"
	"git.sharegap.net/cascadia/drydock/internal/gitexec"
	"git.sharegap.net/cascadia/drydock/internal/testutil"
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
			golden := filepath.Join("testdata", tt.name+".golden.json")
			testutil.AssertGolden(t, golden, string(got)+"\n")
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
			repo := testutil.InitRepo(t, map[string]string{tt.path: tt.content})
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
	repo := testutil.InitRepo(t, map[string]string{
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

// TestCacheSharesCodemapDirAndVersion locks nostrscan's on-disk cache to
// codemap's shared directory and version constant. Both packages write into the
// same drydock-codemap directory; before they shared codemap.CacheVersion each
// carried its own cacheVersion = 1, so bumping codemap's version to invalidate a
// schema change would silently leave nostrscan reading stale entries.
func TestCacheSharesCodemapDirAndVersion(t *testing.T) {
	repo := fixtureRepo(t, "client")
	ctx := context.Background()

	dir, err := codemap.ResolveCacheDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(ctx, repo, "HEAD",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))); err != nil {
		t.Fatal(err)
	}

	treeHash, err := gitexec.Output(ctx, repo, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, "nostr", "profiles", treeHash+".json")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("profile cache not written under codemap cache dir %s: %v", dir, err)
	}
	var entry struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Version != codemap.CacheVersion {
		t.Fatalf("nostr cache version %d != codemap.CacheVersion %d; a codemap schema bump would not invalidate nostr entries in the shared dir", entry.Version, codemap.CacheVersion)
	}
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
	return testutil.InitRepo(t, files)
}
