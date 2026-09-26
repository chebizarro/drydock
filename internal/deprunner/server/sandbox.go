package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/deprunner"
)

// Default resource bounds. CPU, memory, and disk are bounded by the container
// runtime (compose deploy.resources / ulimits / a size-limited tmpfs work
// volume); wall-clock and captured-output size are bounded here in-process.
const (
	defaultTimeout          = 4 * time.Minute
	defaultMaxOutputBytes   = 1 << 20  // 1 MiB per stream captured for diagnostics
	defaultMaxManifestBytes = 16 << 20 // 16 MiB total across all supplied manifests
)

// Options configures the sandbox. Registries and AllowScripts are operator-only;
// nothing in an UpdateRequest can change them.
type Options struct {
	WorkRoot         string        // parent directory for per-request temp roots
	AllowScripts     bool          // operator opt-in to package lifecycle scripts
	Timeout          time.Duration // per-request wall-clock ceiling
	MaxOutputBytes   int
	MaxManifestBytes int
	GoProxy          string
	GoSumDB          string
	NPMRegistry      string
	CargoRegistry    string
	PipIndexURL      string
	ToolchainPath    string // PATH used for toolchain lookups
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = defaultMaxOutputBytes
	}
	if o.MaxManifestBytes <= 0 {
		o.MaxManifestBytes = defaultMaxManifestBytes
	}
	return o
}

// Sandbox runs one dependency-update job at a time in an isolated temp root.
type Sandbox struct {
	opts   Options
	runner commandRunner
	logger *slog.Logger
}

// NewSandbox builds a production sandbox backed by the OS command runner.
func NewSandbox(opts Options, logger *slog.Logger) *Sandbox {
	return newSandbox(opts, osCommandRunner{}, logger)
}

func newSandbox(opts Options, runner commandRunner, logger *slog.Logger) *Sandbox {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sandbox{opts: opts.withDefaults(), runner: runner, logger: logger}
}

// AllowScripts reports the operator's lifecycle-script policy (for /healthz).
func (s *Sandbox) AllowScripts() bool { return s.opts.AllowScripts }

// Update performs one update job. It always returns a structured response;
// operational failures are carried in the response Error/Status, and only
// programming/environment faults (e.g. cannot create a temp dir) return err.
func (s *Sandbox) Update(ctx context.Context, req deprunner.UpdateRequest) (deprunner.UpdateResponse, error) {
	if err := req.Validate(); err != nil {
		return deprunner.UpdateResponse{Status: deprunner.StatusError, Error: err.Error()}, nil
	}
	plan, ok := planFor(req.Ecosystem)
	if !ok {
		return deprunner.UpdateResponse{Status: deprunner.StatusError, Error: "unsupported ecosystem: " + req.Ecosystem}, nil
	}
	if err := s.checkManifestBudget(req); err != nil {
		return deprunner.UpdateResponse{Status: deprunner.StatusError, Error: err.Error()}, nil
	}

	start := time.Now()

	root, err := os.MkdirTemp(s.opts.WorkRoot, "deprun-")
	if err != nil {
		return deprunner.UpdateResponse{}, fmt.Errorf("create sandbox root: %w", err)
	}
	// Always tear the whole tree down: manifests, caches, downloaded artifacts.
	defer func() {
		if rmErr := os.RemoveAll(root); rmErr != nil {
			s.logger.Warn("dep-runner: sandbox cleanup failed", "root", root, "error", rmErr)
		}
	}()

	manifestRoot := filepath.Join(root, "src")
	homeDir := filepath.Join(root, "home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return deprunner.UpdateResponse{}, fmt.Errorf("create sandbox home: %w", err)
	}

	snapshot, err := s.writeManifests(manifestRoot, req.Manifests)
	if err != nil {
		return deprunner.UpdateResponse{Status: deprunner.StatusError, Error: err.Error()}, nil
	}

	workdir, err := locateWorkdir(manifestRoot, req.Manifests, plan.primaryManifest)
	if err != nil {
		return deprunner.UpdateResponse{Status: deprunner.StatusError, Error: err.Error()}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	env := plan.buildEnv(s.opts, homeDir)
	var stdout, stderr strings.Builder
	for _, c := range plan.commands(req, s.opts) {
		res, runErr := s.runner.Run(runCtx, commandSpec{
			Dir:            workdir,
			Name:           c.name,
			Args:           c.args,
			Env:            env,
			MaxOutputBytes: s.opts.MaxOutputBytes,
		})
		stdout.Write(res.Stdout)
		stderr.Write(res.Stderr)
		if runErr != nil {
			return deprunner.UpdateResponse{
				Status:     deprunner.StatusError,
				Error:      fmt.Sprintf("%s %s: %v", c.name, strings.Join(c.args, " "), runErr),
				Stdout:     stdout.String(),
				Stderr:     stderr.String(),
				DurationMS: time.Since(start).Milliseconds(),
			}, nil
		}
		if res.ExitCode != 0 {
			return deprunner.UpdateResponse{
				Status:     deprunner.StatusError,
				Error:      fmt.Sprintf("%s %s exited %d", c.name, strings.Join(c.args, " "), res.ExitCode),
				Stdout:     stdout.String(),
				Stderr:     stderr.String(),
				DurationMS: time.Since(start).Milliseconds(),
			}, nil
		}
	}

	if plan.postProcess != nil {
		if err := plan.postProcess(workdir, req); err != nil {
			return deprunner.UpdateResponse{
				Status:     deprunner.StatusError,
				Error:      "post-process: " + err.Error(),
				Stdout:     stdout.String(),
				Stderr:     stderr.String(),
				DurationMS: time.Since(start).Milliseconds(),
			}, nil
		}
	}

	changed, err := collectChangedFiles(manifestRoot, plan.outputFiles, snapshot)
	if err != nil {
		return deprunner.UpdateResponse{}, fmt.Errorf("collect changed files: %w", err)
	}

	status := deprunner.StatusOK
	if len(changed) == 0 {
		status = deprunner.StatusNoChange
	}
	return deprunner.UpdateResponse{
		Status:       status,
		ChangedFiles: changed,
		Stdout:       stdout.String(),
		Stderr:       stderr.String(),
		DurationMS:   time.Since(start).Milliseconds(),
	}, nil
}

func (s *Sandbox) checkManifestBudget(req deprunner.UpdateRequest) error {
	total := 0
	for _, m := range req.Manifests {
		total += len(m.Content)
		if total > s.opts.MaxManifestBytes {
			return fmt.Errorf("manifests exceed %d bytes", s.opts.MaxManifestBytes)
		}
	}
	return nil
}

// writeManifests writes the supplied files under root, confining every path to
// root, and returns a snapshot of relpath -> content for change detection.
func (s *Sandbox) writeManifests(root string, files []deprunner.ManifestFile) (map[string]string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create manifest root: %w", err)
	}
	snapshot := make(map[string]string, len(files))
	for _, m := range files {
		rel := filepath.FromSlash(m.Path)
		abs := filepath.Join(root, rel)
		// Confinement: the cleaned absolute path must remain under root.
		if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return nil, fmt.Errorf("manifest path escapes sandbox: %s", m.Path)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return nil, fmt.Errorf("create manifest dir for %s: %w", m.Path, err)
		}
		if err := os.WriteFile(abs, []byte(m.Content), 0o600); err != nil {
			return nil, fmt.Errorf("write manifest %s: %w", m.Path, err)
		}
		snapshot[filepath.ToSlash(rel)] = m.Content
	}
	return snapshot, nil
}

// locateWorkdir returns the directory of the shallowest supplied file whose base
// name matches the ecosystem's primary manifest.
func locateWorkdir(root string, files []deprunner.ManifestFile, primary string) (string, error) {
	best := ""
	bestDepth := -1
	for _, m := range files {
		if filepath.Base(filepath.FromSlash(m.Path)) != primary {
			continue
		}
		depth := strings.Count(m.Path, "/")
		if bestDepth == -1 || depth < bestDepth {
			bestDepth = depth
			best = m.Path
		}
	}
	if best == "" {
		return "", fmt.Errorf("required manifest %q not supplied", primary)
	}
	return filepath.Dir(filepath.Join(root, filepath.FromSlash(best))), nil
}

// collectChangedFiles walks root and returns files whose base name is in the
// ecosystem output allowlist and whose content differs from the snapshot (or is
// new). The allowlist means a lifecycle script that wrote a stray file cannot
// smuggle it back to drydock.
func collectChangedFiles(root string, outputFiles []string, snapshot map[string]string) ([]deprunner.ManifestFile, error) {
	allow := make(map[string]struct{}, len(outputFiles))
	for _, name := range outputFiles {
		allow[name] = struct{}{}
	}
	var changed []deprunner.ManifestFile
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if _, ok := allow[d.Name()]; !ok {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if prev, existed := snapshot[relSlash]; existed && prev == string(data) {
			return nil
		}
		changed = append(changed, deprunner.ManifestFile{Path: relSlash, Content: string(data)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}
