package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/deprunner"
)

type recordingRunner struct {
	specs []commandSpec
	fn    func(spec commandSpec) (commandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, spec commandSpec) (commandResult, error) {
	r.specs = append(r.specs, spec)
	if r.fn == nil {
		return commandResult{}, nil
	}
	return r.fn(spec)
}

func newTestSandbox(t *testing.T, opts Options, runner commandRunner) *Sandbox {
	t.Helper()
	opts.WorkRoot = t.TempDir()
	return newSandbox(opts, runner, nil)
}

func TestSandboxGoUpdateCollectsChangedFiles(t *testing.T) {
	runner := &recordingRunner{fn: func(spec commandSpec) (commandResult, error) {
		// Simulate `go get` editing go.mod and creating go.sum.
		os.WriteFile(filepath.Join(spec.Dir, "go.mod"), []byte("module x\n\nrequire foo v1.2.3\n"), 0o600)
		os.WriteFile(filepath.Join(spec.Dir, "go.sum"), []byte("foo v1.2.3 h1:abc=\n"), 0o600)
		return commandResult{}, nil
	}}
	sb := newTestSandbox(t, Options{GoProxy: "https://proxy.example"}, runner)

	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemGo, Package: "foo", ToVersion: "v1.2.3",
		Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: "module x\n\nrequire foo v1.0.0\n"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if resp.Status != deprunner.StatusOK {
		t.Fatalf("status = %q (%s)", resp.Status, resp.Error)
	}
	got := map[string]string{}
	for _, f := range resp.ChangedFiles {
		got[f.Path] = f.Content
	}
	if !strings.Contains(got["go.mod"], "v1.2.3") {
		t.Fatalf("go.mod not updated: %q", got["go.mod"])
	}
	if _, ok := got["go.sum"]; !ok {
		t.Fatalf("go.sum not returned: %#v", resp.ChangedFiles)
	}
	// Command + hardened env assertions.
	if len(runner.specs) != 1 {
		t.Fatalf("expected 1 command, got %d", len(runner.specs))
	}
	spec := runner.specs[0]
	if spec.Name != "go" || spec.Args[0] != "get" || spec.Args[1] != "foo@v1.2.3" {
		t.Fatalf("unexpected command %v %v", spec.Name, spec.Args)
	}
	assertEnv(t, spec.Env, "GOTOOLCHAIN=local")
	assertEnv(t, spec.Env, "GOFLAGS=-mod=mod")
	assertEnv(t, spec.Env, "GOPROXY=https://proxy.example")
	if envHasKey(spec.Env, "GITHUB_TOKEN") || len(spec.Env) == 0 {
		t.Fatalf("env not scrubbed/allowlisted: %v", spec.Env)
	}
}

func TestSandboxNoChange(t *testing.T) {
	runner := &recordingRunner{} // writes nothing
	sb := newTestSandbox(t, Options{}, runner)
	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemGo, Package: "foo", ToVersion: "v1.0.0",
		Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: "module x\n"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if resp.Status != deprunner.StatusNoChange {
		t.Fatalf("status = %q, want no_change", resp.Status)
	}
}

func TestSandboxNpmBlocksScriptsByDefault(t *testing.T) {
	runner := &recordingRunner{fn: func(spec commandSpec) (commandResult, error) {
		os.WriteFile(filepath.Join(spec.Dir, "package-lock.json"), []byte(`{"version":"1.2.3"}`), 0o600)
		return commandResult{}, nil
	}}
	sb := newTestSandbox(t, Options{}, runner)
	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemNPM, Package: "left-pad", ToVersion: "1.2.3",
		Manifests: []deprunner.ManifestFile{{Path: "package.json", Content: `{"name":"x"}`}},
	})
	if err != nil || resp.Status != deprunner.StatusOK {
		t.Fatalf("Update() = %v / %q", err, resp.Status)
	}
	spec := runner.specs[0]
	if !argsContain(spec.Args, "--ignore-scripts") || !argsContain(spec.Args, "--package-lock-only") {
		t.Fatalf("npm args missing script/lock guards: %v", spec.Args)
	}
	assertEnv(t, spec.Env, "npm_config_ignore_scripts=true")
}

func TestSandboxNpmAllowScriptsOptIn(t *testing.T) {
	runner := &recordingRunner{}
	sb := newTestSandbox(t, Options{AllowScripts: true}, runner)
	_, _ = sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemNPM, Package: "left-pad", ToVersion: "1.2.3",
		Manifests: []deprunner.ManifestFile{{Path: "package.json", Content: `{"name":"x"}`}},
	})
	spec := runner.specs[0]
	if argsContain(spec.Args, "--ignore-scripts") {
		t.Fatalf("operator opt-in should drop --ignore-scripts: %v", spec.Args)
	}
	assertEnv(t, spec.Env, "npm_config_ignore_scripts=false")
}

func TestSandboxPipRewritesPinnedRequirement(t *testing.T) {
	runner := &recordingRunner{} // pip download succeeds (exit 0), writes nothing
	sb := newTestSandbox(t, Options{}, runner)
	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemPip, Package: "Django", ToVersion: "4.2.11",
		Manifests: []deprunner.ManifestFile{{Path: "requirements.txt", Content: "django==4.2.0  # web\nrequests==2.31.0\n"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if resp.Status != deprunner.StatusOK || len(resp.ChangedFiles) != 1 {
		t.Fatalf("unexpected response %#v", resp)
	}
	content := resp.ChangedFiles[0].Content
	if !strings.Contains(content, "django==4.2.11  # web") {
		t.Fatalf("requirement not rewritten with preserved comment: %q", content)
	}
	if !strings.Contains(content, "requests==2.31.0") {
		t.Fatalf("unrelated requirement changed: %q", content)
	}
}

func TestSandboxPipRefusesSdistOnly(t *testing.T) {
	runner := &recordingRunner{fn: func(spec commandSpec) (commandResult, error) {
		return commandResult{Stderr: []byte("ERROR: No matching distribution")}, nil // exit 1 below
	}}
	runner.fn = func(spec commandSpec) (commandResult, error) {
		return commandResult{ExitCode: 1, Stderr: []byte("no wheel")}, nil
	}
	sb := newTestSandbox(t, Options{}, runner)
	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemPip, Package: "foo", ToVersion: "1.0.0",
		Manifests: []deprunner.ManifestFile{{Path: "requirements.txt", Content: "foo==0.9.0\n"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if resp.Status != deprunner.StatusError {
		t.Fatalf("sdist-only should be refused, got %q", resp.Status)
	}
}

func TestSandboxOnlyReturnsAllowlistedFiles(t *testing.T) {
	runner := &recordingRunner{fn: func(spec commandSpec) (commandResult, error) {
		os.WriteFile(filepath.Join(spec.Dir, "go.mod"), []byte("module x\nrequire foo v2\n"), 0o600)
		// A lifecycle script (or buggy tool) drops a stray file. It must not be
		// returned to drydock.
		os.WriteFile(filepath.Join(spec.Dir, "evil.sh"), []byte("rm -rf /"), 0o600)
		return commandResult{}, nil
	}}
	sb := newTestSandbox(t, Options{}, runner)
	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemGo, Package: "foo", ToVersion: "v2",
		Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: "module x\n"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	for _, f := range resp.ChangedFiles {
		if f.Path == "evil.sh" {
			t.Fatalf("stray non-manifest file leaked back to caller")
		}
	}
}

func TestSandboxManifestBudget(t *testing.T) {
	sb := newTestSandbox(t, Options{MaxManifestBytes: 8}, &recordingRunner{})
	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemGo, Package: "foo", ToVersion: "v1",
		Manifests: []deprunner.ManifestFile{{Path: "go.mod", Content: strings.Repeat("x", 64)}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if resp.Status != deprunner.StatusError || !strings.Contains(resp.Error, "exceed") {
		t.Fatalf("expected budget rejection, got %q / %q", resp.Status, resp.Error)
	}
}

func TestSandboxMissingPrimaryManifest(t *testing.T) {
	sb := newTestSandbox(t, Options{}, &recordingRunner{})
	resp, err := sb.Update(context.Background(), deprunner.UpdateRequest{
		Ecosystem: deprunner.EcosystemGo, Package: "foo", ToVersion: "v1",
		Manifests: []deprunner.ManifestFile{{Path: "go.sum", Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if resp.Status != deprunner.StatusError || !strings.Contains(resp.Error, "go.mod") {
		t.Fatalf("expected missing-manifest error, got %q / %q", resp.Status, resp.Error)
	}
}

func assertEnv(t *testing.T, env []string, want string) {
	t.Helper()
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Fatalf("env missing %q in %v", want, env)
}

func envHasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
