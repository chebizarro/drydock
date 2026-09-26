package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/deprunner"
)

// toolCommand is one toolchain invocation within an ecosystem's update plan.
type toolCommand struct {
	name string
	args []string
}

// ecosystemPlan describes how to produce a version bump for one ecosystem
// without executing untrusted package code by default. Every command here is a
// lockfile/manifest-resolving operation, not a build:
//
//   - go:    `go get pkg@ver` updates go.mod/go.sum; it does not compile packages.
//   - npm:   `npm install --package-lock-only --ignore-scripts` solves the lock
//     without writing node_modules or running lifecycle scripts.
//   - cargo: `cargo add` + `cargo update --precise` edit Cargo.toml/Cargo.lock;
//     neither compiles, so build.rs never runs.
//   - pip:   there is no lockfile; the plan downloads the target *wheel* (which,
//     unlike an sdist, never runs setup.py) purely to prove it installs
//     without code execution, then rewrites the pinned requirement line.
type ecosystemPlan struct {
	name            string
	primaryManifest string
	outputFiles     []string
	buildEnv        func(opts Options, homeDir string) []string
	commands        func(req deprunner.UpdateRequest, opts Options) []toolCommand
	postProcess     func(workdir string, req deprunner.UpdateRequest) error
}

var plans = map[string]ecosystemPlan{
	deprunner.EcosystemGo: {
		name:            deprunner.EcosystemGo,
		primaryManifest: "go.mod",
		outputFiles:     []string{"go.mod", "go.sum"},
		buildEnv:        goEnv,
		commands: func(req deprunner.UpdateRequest, _ Options) []toolCommand {
			return []toolCommand{{name: "go", args: []string{"get", req.Package + "@" + req.ToVersion}}}
		},
	},
	deprunner.EcosystemNPM: {
		name:            deprunner.EcosystemNPM,
		primaryManifest: "package.json",
		outputFiles:     []string{"package.json", "package-lock.json", "npm-shrinkwrap.json"},
		buildEnv:        npmEnv,
		commands: func(req deprunner.UpdateRequest, opts Options) []toolCommand {
			args := []string{"install", req.Package + "@" + req.ToVersion, "--package-lock-only", "--no-audit", "--no-fund"}
			if !opts.AllowScripts {
				args = append(args, "--ignore-scripts")
			}
			return []toolCommand{{name: "npm", args: args}}
		},
	},
	deprunner.EcosystemCargo: {
		name:            deprunner.EcosystemCargo,
		primaryManifest: "Cargo.toml",
		outputFiles:     []string{"Cargo.toml", "Cargo.lock"},
		buildEnv:        cargoEnv,
		commands: func(req deprunner.UpdateRequest, _ Options) []toolCommand {
			return []toolCommand{
				{name: "cargo", args: []string{"add", req.Package + "@" + req.ToVersion}},
				{name: "cargo", args: []string{"update", "-p", req.Package, "--precise", req.ToVersion}},
			}
		},
	},
	deprunner.EcosystemPip: {
		name:            deprunner.EcosystemPip,
		primaryManifest: "requirements.txt",
		outputFiles:     []string{"requirements.txt"},
		buildEnv:        pipEnv,
		commands: func(req deprunner.UpdateRequest, _ Options) []toolCommand {
			// Wheel-only download proves the target installs without executing
			// setup.py; --no-deps keeps it to the single package.
			return []toolCommand{{name: "pip", args: []string{
				"download", req.Package + "==" + req.ToVersion,
				"--no-deps", "--only-binary=:all:", "-d", "wheels",
			}}}
		},
		postProcess: rewritePinnedRequirement,
	},
}

// planFor returns the update plan for an ecosystem.
func planFor(eco string) (ecosystemPlan, bool) {
	p, ok := plans[eco]
	return p, ok
}

// defaultToolchainPATH is used when the operator does not pin one. It matches the
// dep-runner image layout (Go under /usr/local/go/bin, everything else under
// /usr/local/bin).
const defaultToolchainPATH = "/usr/local/go/bin:/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin"

func toolchainPATH(opts Options) string {
	if strings.TrimSpace(opts.ToolchainPath) != "" {
		return opts.ToolchainPath
	}
	return defaultToolchainPATH
}

func goEnv(opts Options, homeDir string) []string {
	env := []string{
		"PATH=" + toolchainPATH(opts),
		"HOME=" + homeDir,
		"GOTOOLCHAIN=local", // never auto-download a toolchain a go.mod directive asks for
		"GOFLAGS=-mod=mod",
		"GOMODCACHE=" + filepath.Join(homeDir, "go", "pkg", "mod"),
		"GOCACHE=" + filepath.Join(homeDir, "go", "cache"),
		"GOPATH=" + filepath.Join(homeDir, "go"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if v := strings.TrimSpace(opts.GoProxy); v != "" {
		env = append(env, "GOPROXY="+v)
	}
	if v := strings.TrimSpace(opts.GoSumDB); v != "" {
		env = append(env, "GOSUMDB="+v)
	}
	return env
}

func npmEnv(opts Options, homeDir string) []string {
	env := []string{
		"PATH=" + toolchainPATH(opts),
		"HOME=" + homeDir,
		"npm_config_cache=" + filepath.Join(homeDir, "npm-cache"),
		"npm_config_audit=false",
		"npm_config_fund=false",
		"npm_config_update_notifier=false",
	}
	// Belt-and-braces with the CLI --ignore-scripts flag: a repo-local .npmrc
	// could otherwise re-enable scripts.
	if opts.AllowScripts {
		env = append(env, "npm_config_ignore_scripts=false")
	} else {
		env = append(env, "npm_config_ignore_scripts=true")
	}
	if v := strings.TrimSpace(opts.NPMRegistry); v != "" {
		env = append(env, "npm_config_registry="+v)
	}
	return env
}

func cargoEnv(opts Options, homeDir string) []string {
	return []string{
		"PATH=" + toolchainPATH(opts),
		"HOME=" + homeDir,
		"CARGO_HOME=" + filepath.Join(homeDir, "cargo"),
		"CARGO_NET_RETRY=2",
		"GIT_TERMINAL_PROMPT=0",
	}
}

func pipEnv(opts Options, homeDir string) []string {
	env := []string{
		"PATH=" + toolchainPATH(opts),
		"HOME=" + homeDir,
		"PIP_NO_INPUT=1",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PIP_NO_CACHE_DIR=1",
		"PIP_ROOT_USER_ACTION=ignore",
	}
	if v := strings.TrimSpace(opts.PipIndexURL); v != "" {
		env = append(env, "PIP_INDEX_URL="+v)
	}
	return env
}

// rewritePinnedRequirement updates the pinned version of req.Package in
// requirements.txt. pip has no lockfile to regenerate, so a targeted line edit
// is the only mechanism; the preceding wheel download already proved the target
// resolves without executing package code. Only simple `name==version` pins are
// rewritten (extras and environment markers are preserved); anything else is
// left untouched and reported as no-change by the caller.
func rewritePinnedRequirement(workdir string, req deprunner.UpdateRequest) error {
	reqPath := filepath.Join(workdir, "requirements.txt")
	data, err := os.ReadFile(reqPath)
	if err != nil {
		return fmt.Errorf("read requirements: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		name, rest, ok := parsePinnedRequirement(line)
		if !ok {
			continue
		}
		if !samePyPIName(name, req.Package) {
			continue
		}
		lines[i] = name + "==" + req.ToVersion + rest
		changed = true
	}
	if !changed {
		return nil
	}
	if err := os.WriteFile(reqPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return fmt.Errorf("write requirements: %w", err)
	}
	return nil
}

// parsePinnedRequirement splits a `name==version[; markers]` line, returning the
// package name and the trailing remainder after the version (extras/markers/
// comments) so a rewrite preserves them. It reports false for anything that is
// not a simple == pin.
func parsePinnedRequirement(line string) (name, trailer string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
		return "", "", false
	}
	idx := strings.Index(trimmed, "==")
	if idx <= 0 {
		return "", "", false
	}
	name = strings.TrimSpace(trimmed[:idx])
	if name == "" || strings.ContainsAny(name, " \t<>=!~[") {
		return "", "", false // extras/complex specifiers: leave alone
	}
	after := trimmed[idx+2:]
	// The version runs until whitespace, ';', or '#'; keep the rest as trailer.
	end := strings.IndexAny(after, " \t;#")
	if end < 0 {
		return name, "", true
	}
	return name, after[end:], true
}

// samePyPIName compares two PyPI names using PEP 503 normalization (case-fold,
// runs of -_. collapse to a single -).
func samePyPIName(a, b string) bool {
	return normalizePyPIName(a) == normalizePyPIName(b)
}

func normalizePyPIName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var out strings.Builder
	prevSep := false
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' {
			if !prevSep {
				out.WriteByte('-')
				prevSep = true
			}
			continue
		}
		out.WriteRune(r)
		prevSep = false
	}
	return strings.Trim(out.String(), "-")
}
