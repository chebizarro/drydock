package nostrscan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultMinConfidence = 0.60
	cacheVersion         = 1
	maxScanFileBytes     = 2 * 1024 * 1024
	maxScanBytes         = 32 * 1024 * 1024
)

// Option configures a Detector.
type Option func(*Detector)

// WithCacheDir stores detector profiles in dir. The default is the checkout's
// shared .git/drydock-codemap directory, alongside code-map caches.
func WithCacheDir(dir string) Option {
	return func(d *Detector) { d.cacheDir = dir }
}

// WithMinConfidence sets the floor at which the Nostr lens is enabled.
func WithMinConfidence(floor float64) Option {
	return func(d *Detector) { d.minConfidence = floor }
}

// WithLogger sets the logger used for auditable classification and skip logs.
func WithLogger(logger *slog.Logger) Option {
	return func(d *Detector) { d.logger = logger }
}

// Detector classifies repository checkouts and caches profiles by Git tree.
type Detector struct {
	cacheDir      string
	minConfidence float64
	logger        *slog.Logger
}

// New creates a detector.
func New(opts ...Option) *Detector {
	d := &Detector{minConfidence: DefaultMinConfidence, logger: slog.Default()}
	for _, opt := range opts {
		opt(d)
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	return d
}

// Detect classifies ref in repoPath. An empty ref means HEAD.
func Detect(ctx context.Context, repoPath, ref string, opts ...Option) (NostrProfile, error) {
	return New(opts...).Detect(ctx, repoPath, ref)
}

// Detect classifies ref in repoPath. Results are cached by Git tree hash.
func (d *Detector) Detect(ctx context.Context, repoPath, ref string) (NostrProfile, error) {
	if strings.TrimSpace(repoPath) == "" {
		return NostrProfile{}, fmt.Errorf("nostrscan: repository path is required")
	}
	if d.minConfidence < 0 || d.minConfidence > 1 {
		return NostrProfile{}, fmt.Errorf("nostrscan: minimum confidence must be between 0 and 1")
	}
	if ref == "" {
		ref = "HEAD"
	}

	treeHash, err := gitOutput(ctx, repoPath, "rev-parse", ref+"^{tree}")
	if err != nil {
		return NostrProfile{}, fmt.Errorf("nostrscan: resolve tree for %s: %w", ref, err)
	}
	cacheDir, err := d.resolveCacheDir(ctx, repoPath)
	if err != nil {
		return NostrProfile{}, err
	}
	cachePath := filepath.Join(cacheDir, "nostr", "profiles", treeHash+".json")
	if profile, ok := readCache(cachePath, treeHash); ok {
		d.applyFloorAndLog(&profile, true)
		return profile, nil
	}

	files, err := gitFiles(ctx, repoPath, ref)
	if err != nil {
		return NostrProfile{}, fmt.Errorf("nostrscan: list tree: %w", err)
	}
	profile, err := scanFiles(ctx, repoPath, ref, files)
	if err != nil {
		return NostrProfile{}, err
	}
	if err := writeCache(cachePath, treeHash, profile); err != nil {
		return NostrProfile{}, fmt.Errorf("nostrscan: write profile cache: %w", err)
	}
	d.applyFloorAndLog(&profile, false)
	return profile, nil
}

// ShouldRun reports whether profile reaches floor and logs the decision. It is
// provided for callers applying an explicit configuration floor.
func ShouldRun(profile NostrProfile, floor float64, logger *slog.Logger) bool {
	if logger == nil {
		logger = slog.Default()
	}
	enabled := profile.Confidence >= floor
	logDecision(logger, profile, floor, enabled, false)
	return enabled
}

func (d *Detector) applyFloorAndLog(profile *NostrProfile, cached bool) {
	profile.IsNostr = profile.Confidence >= d.minConfidence
	logDecision(d.logger, *profile, d.minConfidence, profile.IsNostr, cached)
}

func logDecision(logger *slog.Logger, profile NostrProfile, floor float64, enabled, cached bool) {
	reasons := make([]string, 0, len(profile.Evidence))
	for _, marker := range profile.Evidence {
		reasons = append(reasons, fmt.Sprintf("%s:%s:%s", marker.Kind, marker.Path, marker.Name))
	}
	attrs := []any{
		"confidence", profile.Confidence,
		"min_confidence", floor,
		"roles", profile.Roles,
		"evidence", reasons,
		"cached", cached,
	}
	if enabled {
		logger.Info("nostr repository classified; enabling nostr lens", attrs...)
		return
	}
	attrs = append(attrs, "reason", "confidence below minimum")
	logger.Info("nostr lens skipped", attrs...)
}

type cacheEntry struct {
	Version  int          `json:"version"`
	TreeHash string       `json:"tree_hash"`
	Profile  NostrProfile `json:"profile"`
}

func readCache(path, treeHash string) (NostrProfile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return NostrProfile{}, false
	}
	var entry cacheEntry
	if json.Unmarshal(data, &entry) != nil || entry.Version != cacheVersion || entry.TreeHash != treeHash {
		return NostrProfile{}, false
	}
	entry.Profile.IsNostr = false
	return entry.Profile, true
}

func writeCache(path, treeHash string, profile NostrProfile) error {
	profile.IsNostr = false
	data, err := json.Marshal(cacheEntry{Version: cacheVersion, TreeHash: treeHash, Profile: profile})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (d *Detector) resolveCacheDir(ctx context.Context, repoPath string) (string, error) {
	if d.cacheDir != "" {
		return d.cacheDir, nil
	}
	gitDir, err := gitOutput(ctx, repoPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("nostrscan: resolve git directory: %w", err)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoPath, gitDir)
	}
	return filepath.Join(filepath.Clean(gitDir), "drydock-codemap"), nil
}

type treeFile struct {
	hash string
	path string
}

func gitFiles(ctx context.Context, repoPath, ref string) ([]treeFile, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "ls-tree", "-r", "-z", ref).Output()
	if err != nil {
		return nil, err
	}
	var files []treeFile
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		meta := strings.Fields(parts[0])
		if len(meta) != 3 || meta[1] != "blob" || !strings.HasPrefix(meta[0], "100") {
			continue
		}
		if scanPath(parts[1]) {
			files = append(files, treeFile{hash: meta[2], path: parts[1]})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func scanPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	for _, part := range []string{"/vendor/", "/node_modules/", "/.git/", "/dist/", "/build/"} {
		if strings.Contains("/"+lower, part) {
			return false
		}
	}
	base := filepath.Base(lower)
	switch base {
	case "go.mod", "package.json", "cargo.toml", "pubspec.yaml", "package.swift",
		"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts":
		return true
	}
	switch filepath.Ext(lower) {
	case ".go", ".rs", ".js", ".jsx", ".ts", ".tsx", ".dart", ".swift", ".kt", ".kts",
		".java", ".py", ".rb", ".md", ".txt", ".toml", ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

func gitBlob(ctx context.Context, repoPath, hash string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "cat-file", "blob", hash)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, maxScanFileBytes+1))
	if len(data) > maxScanFileBytes {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, waitErr
	}
	if len(data) > maxScanFileBytes {
		return nil, nil
	}
	return data, nil
}

var (
	nipRE       = regexp.MustCompile(`(?i)\bNIP[-_ ]?[0-9]{2,3}\b`)
	wssRE       = regexp.MustCompile(`(?i)wss://[^\s"'<>]+`)
	bech32RE    = regexp.MustCompile(`(?i)\b(nsec1|npub1|nevent1)[023456789acdefghjklmnpqrstuvwxyz]{6,}\b`)
	kindRE      = regexp.MustCompile(`(?i)\b(?:event[_-]?kind|kind)[a-z0-9_]*\s*(?::|=)\s*([0-9]{1,5})\b`)
	eventTypeRE = regexp.MustCompile(`(?i)(?:type\s+[a-z0-9_]*event[a-z0-9_]*\s+struct|struct\s+[a-z0-9_]*event[a-z0-9_]*|(?:data\s+)?class\s+[a-z0-9_]*event[a-z0-9_]*)`)
)

type scanState struct {
	evidence       []Marker
	dependencyHits int
	protocolKinds  map[string]bool
	structural     bool
	roles          map[Role]Marker
	seen           map[string]bool
}

func scanFiles(ctx context.Context, repoPath, ref string, files []treeFile) (NostrProfile, error) {
	state := scanState{
		protocolKinds: make(map[string]bool),
		roles:         make(map[Role]Marker),
		seen:          make(map[string]bool),
	}
	total := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return NostrProfile{}, err
		}
		data, err := gitBlob(ctx, repoPath, file.hash)
		if err != nil {
			return NostrProfile{}, fmt.Errorf("nostrscan: read %s at %s: %w", file.path, ref, err)
		}
		if len(data) == 0 || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		total += len(data)
		if total > maxScanBytes {
			break
		}
		state.scanFile(file.path, data)
	}
	return state.profile(), nil
}

func (s *scanState) add(marker Marker) {
	key := string(marker.Kind) + "\x00" + marker.Name + "\x00" + marker.Path
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.evidence = append(s.evidence, marker)
}

func (s *scanState) addRole(role Role, path string, line int, detail string) {
	if _, exists := s.roles[role]; exists {
		return
	}
	marker := Marker{Kind: MarkerRole, Name: string(role), Path: path, Line: line, Detail: detail}
	s.roles[role] = marker
	s.add(marker)
}

func (s *scanState) scanFile(path string, data []byte) {
	text := string(data)
	lower := strings.ToLower(text)
	base := strings.ToLower(filepath.Base(path))

	if name, line, detail := dependencyMarker(base, text); name != "" {
		s.dependencyHits++
		s.add(Marker{Kind: MarkerDependency, Name: name, Path: path, Line: line, Weight: 0.70, Detail: detail})
	}

	if loc := nipRE.FindStringIndex(text); loc != nil {
		match := text[loc[0]:loc[1]]
		s.protocolKinds["nip"] = true
		s.add(Marker{Kind: MarkerProtocol, Name: "nip-reference", Path: path, Line: lineAt(data, loc[0]), Weight: 0.15, Detail: match})
	}
	if loc := wssRE.FindStringIndex(text); loc != nil {
		s.protocolKinds["relay-url"] = true
		s.add(Marker{Kind: MarkerProtocol, Name: "relay-url", Path: path, Line: lineAt(data, loc[0]), Weight: 0.20, Detail: text[loc[0]:loc[1]]})
	}
	if loc := bech32RE.FindStringIndex(text); loc != nil {
		s.protocolKinds["bech32"] = true
		prefix := strings.ToLower(text[loc[0] : loc[0]+5])
		s.add(Marker{Kind: MarkerProtocol, Name: "nostr-bech32", Path: path, Line: lineAt(data, loc[0]), Weight: 0.25, Detail: prefix + " bech32 prefix"})
	}
	if loc, value := findEventKind(text); loc >= 0 {
		s.protocolKinds["event-kind"] = true
		s.add(Marker{Kind: MarkerProtocol, Name: "event-kind", Path: path, Line: lineAt(data, loc), Weight: 0.15, Detail: "kind " + value})
	}
	if loc, verbs := findMessageVerbs(text); len(verbs) >= 2 {
		s.protocolKinds["message-verbs"] = true
		s.add(Marker{Kind: MarkerProtocol, Name: "message-verbs", Path: path, Line: lineAt(data, loc), Weight: 0.25, Detail: strings.Join(verbs, ",")})
	}
	if loc, ok := eventShape(text); ok {
		s.structural = true
		s.add(Marker{Kind: MarkerStructural, Name: "nip01-event-shape", Path: path, Line: lineAt(data, loc), Weight: 0.65, Detail: "id,pubkey,created_at,kind,tags,content,sig"})
		if strings.Contains(lower, "schnorr") || strings.Contains(lower, "secp256k1") {
			s.protocolKinds["event-crypto"] = true
			s.add(Marker{Kind: MarkerProtocol, Name: "event-crypto", Path: path, Line: lineAt(data, loc), Weight: 0.30, Detail: "schnorr/secp256k1 adjacent to event type"})
		}
	}

	s.inferRoles(path, lower, base)
}

func dependencyMarker(base, text string) (string, int, string) {
	patterns := map[string][]string{
		"go.mod":           {"github.com/nbd-wtf/go-nostr", "fiatjaf.com/nostr", "github.com/nostr-protocol/"},
		"package.json":     {"nostr-tools", "@nostr-dev-kit/ndk", "nostr-relaypool"},
		"cargo.toml":       {"nostr-sdk", "nostr ="},
		"pubspec.yaml":     {"nostr_tools", "ndk", "nostr_sdk", "nostr:"},
		"package.swift":    {"nostr-sdk", "nostrsdk", "swift-nostr", "nostrkit"},
		"build.gradle":     {"nostr-sdk", "nostr-tools", "quartz", "nostr:"},
		"build.gradle.kts": {"nostr-sdk", "nostr-tools", "quartz", "nostr:"},
	}
	for _, pattern := range patterns[base] {
		if idx := strings.Index(strings.ToLower(text), pattern); idx >= 0 {
			return pattern, lineAt([]byte(text), idx), "Nostr dependency in " + base
		}
	}
	return "", 0, ""
}

func findMessageVerbs(text string) (int, []string) {
	verbs := []string{"EVENT", "REQ", "CLOSE", "OK", "EOSE", "AUTH"}
	found := make([]string, 0, len(verbs))
	first := -1
	for _, verb := range verbs {
		re := regexp.MustCompile(`["']` + verb + `["']`)
		if loc := re.FindStringIndex(text); loc != nil {
			if first < 0 || loc[0] < first {
				first = loc[0]
			}
			found = append(found, verb)
		}
	}
	return first, found
}

func findEventKind(text string) (int, string) {
	for _, loc := range kindRE.FindAllStringSubmatchIndex(text, -1) {
		value := text[loc[2]:loc[3]]
		n, err := strconv.Atoi(value)
		if err == nil && knownEventKind(n) {
			return loc[0], value
		}
	}
	return -1, ""
}

func knownEventKind(kind int) bool {
	if kind >= 0 && kind <= 7 {
		return true
	}
	if kind >= 40 && kind <= 44 {
		return true
	}
	if kind >= 5000 && kind <= 6999 {
		return true
	}
	if kind == 9734 || kind == 9735 || kind == 22242 {
		return true
	}
	return kind >= 10000 && kind <= 39999
}

func eventShape(text string) (int, bool) {
	for _, loc := range eventTypeRE.FindAllStringIndex(text, -1) {
		end := loc[0] + 4096
		if end > len(text) {
			end = len(text)
		}
		window := strings.ToLower(text[loc[0]:end])
		fields := []bool{
			hasWord(window, "id"),
			hasAnyWord(window, "pubkey", "pub_key", "publickey", "public_key"),
			hasAnyWord(window, "created_at", "createdat"),
			hasWord(window, "kind"),
			hasWord(window, "tags"),
			hasWord(window, "content"),
			hasAnyWord(window, "sig", "signature"),
		}
		ok := true
		for _, present := range fields {
			ok = ok && present
		}
		if ok {
			return loc[0], true
		}
	}
	return -1, false
}

func hasWord(text, word string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`)
	return re.MatchString(text)
}

func hasAnyWord(text string, words ...string) bool {
	for _, word := range words {
		if hasWord(text, word) {
			return true
		}
	}
	return false
}

func (s *scanState) inferRoles(path, lower, base string) {
	pathLower := strings.ToLower(filepath.ToSlash(path))
	switch {
	case strings.Contains(lower, "nip-90") || strings.Contains(lower, "nip90") ||
		strings.Contains(lower, "data vending machine") || strings.Contains(pathLower, "/dvm"):
		s.addRole(RoleDVM, path, firstLineContaining(lower, "nip"), "DVM/NIP-90 marker")
	}
	if strings.Contains(pathLower, "relay") || strings.Contains(lower, "relay server") ||
		(strings.Contains(lower, `"event"`) && strings.Contains(lower, `"ok"`) && strings.Contains(lower, "websocket")) {
		s.addRole(RoleRelay, path, firstLineContaining(lower, "relay"), "relay implementation marker")
	}
	if strings.Contains(pathLower, "client") || strings.Contains(lower, "subscribe(") ||
		(strings.Contains(lower, `"req"`) && strings.Contains(lower, `"eose"`)) {
		s.addRole(RoleClient, path, firstLineContaining(lower, "req"), "client subscription marker")
	}
	if strings.Contains(pathLower, "signer") || strings.Contains(lower, "sign_event") ||
		strings.Contains(lower, "signevent") || strings.Contains(lower, "nip-46") ||
		strings.Contains(lower, "nsec1") || strings.Contains(lower, "bunker://") {
		s.addRole(RoleSigner, path, firstLineContaining(lower, "sign"), "signing/key marker")
	}
	if strings.Contains(pathLower, "/lib/") || strings.Contains(lower, "nostr sdk") ||
		strings.Contains(lower, "nostr library") || strings.Contains(base, "package.swift") && strings.Contains(lower, "library(") {
		s.addRole(RoleLibrary, path, 1, "library/SDK marker")
	}
}

func firstLineContaining(text, needle string) int {
	idx := strings.Index(text, needle)
	if idx < 0 {
		return 1
	}
	return lineAt([]byte(text), idx)
}

func (s *scanState) profile() NostrProfile {
	protocol := 0.0
	for kind := range s.protocolKinds {
		switch kind {
		case "nip":
			protocol += 0.15
		case "relay-url":
			protocol += 0.20
		case "event-kind":
			protocol += 0.15
		case "message-verbs":
			protocol += 0.25
		case "bech32":
			protocol += 0.25
		case "event-crypto":
			protocol += 0.30
		}
	}
	if protocol > 0.85 {
		protocol = 0.85
	}
	dependency := 0.0
	if s.dependencyHits > 0 {
		dependency = 0.70
		if s.dependencyHits > 1 {
			dependency = 0.75
		}
	}
	structural := 0.0
	if s.structural {
		structural = 0.65
	}
	confidence := max(dependency, protocol, structural)
	groups := 0
	for _, score := range []float64{dependency, protocol, structural} {
		if score > 0 {
			groups++
		}
	}
	if groups >= 2 {
		confidence += 0.10
	}
	if groups == 3 {
		confidence += 0.05
	}
	if confidence > 1 {
		confidence = 1
	}
	confidence = float64(int(confidence*100+0.5)) / 100

	roleOrder := []Role{RoleClient, RoleRelay, RoleSigner, RoleLibrary, RoleDVM}
	roles := make([]Role, 0, len(s.roles))
	for _, role := range roleOrder {
		if _, ok := s.roles[role]; ok {
			roles = append(roles, role)
		}
	}
	sort.Slice(s.evidence, func(i, j int) bool {
		a, b := s.evidence[i], s.evidence[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return NostrProfile{Confidence: confidence, Roles: roles, Evidence: s.evidence}
}

func lineAt(data []byte, offset int) int {
	if offset < 0 {
		return 0
	}
	if offset > len(data) {
		offset = len(data)
	}
	return bytes.Count(data[:offset], []byte{'\n'}) + 1
}

func gitOutput(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", repoPath}, args...)
	out, err := exec.CommandContext(ctx, "git", cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
