package nostrscan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"drydock/internal/codemap"
	"drydock/internal/nostrscan/knowledge"
	"drydock/internal/securityscan"
	"drydock/internal/securityscan/surface"
)

// AbsenceConfidence is intentionally below the review gating threshold.
// Security verification may raise it only after confirming the missing check.
const AbsenceConfidence = 0.79

const maxAbsenceDepth = 24

// AbsenceAnalyzer finds Nostr security checks missing from ingest-to-use paths.
type AbsenceAnalyzer struct{}

// NewAbsenceAnalyzer constructs an absence-of-check analyzer.
func NewAbsenceAnalyzer() *AbsenceAnalyzer { return &AbsenceAnalyzer{} }

// AnalyzeAbsences is the package-level convenience entry point.
func AnalyzeAbsences(ctx context.Context, repoPath string, codeMap *codemap.Map, surfaces surface.Result) securityscan.ScanResult {
	return NewAbsenceAnalyzer().Analyze(ctx, repoPath, codeMap, surfaces)
}

type absenceNode struct {
	symbol codemap.Symbol
	body   string

	ingest      bool
	verify      bool
	recomputeID bool
	compareID   bool
	authPubkey  bool
	dedupID     bool
	freshness   bool
	lengthOrMAC bool

	v2Use int
	v7Use int
	v1Use int
	r1Use int
	r2Use int
}

type pathChecks struct {
	verify      bool
	recomputeID bool
	compareID   bool
	authPubkey  bool
	dedupID     bool
	freshness   bool
	lengthOrMAC bool
}

var (
	eventIngestRE         = regexp.MustCompile(`(?i)(?:parse|decode|unmarshal|handle|receive|on)[A-Za-z0-9_]*(?:event|message)|relayMessage|subscriptionCallback`)
	verifyRE              = regexp.MustCompile(`(?i)(?:CheckSignature|verifySignature|verifyEvent|checkEvent|schnorr\s*\.\s*Verify|\.VerifySignature)\s*\(`)
	explicitRecomputeIDRE = regexp.MustCompile(`(?i)(?:\.GetID\s*\(|(?:recompute|compute|calculate|derive)[A-Za-z0-9_]*ID\s*\()`)
	serializeIDRE         = regexp.MustCompile(`(?i)serialize[A-Za-z0-9_]*\s*\(`)
	sha256RE              = regexp.MustCompile(`(?i)sha(?:2)?256[A-Za-z0-9_.]*\s*\(`)
	compareIDRE           = regexp.MustCompile(`(?i)(?:(?:computed|recomputed|actual|expected|hash)[A-Za-z0-9_]*\s*(?:==|!=)\s*(?:event|ev|evt)?\s*\.?\s*id|(?:event|ev|evt)\s*\.\s*id\s*(?:==|!=)\s*(?:computed|recomputed|actual|expected|hash))`)
	authPubkeyRE          = regexp.MustCompile(`(?i)(?:pinned[A-Za-z0-9_]*(?:key|pubkey)|fingerprint|keyTransparency|transparencyLog|trustedPubkey|outOfBandKey)`)
	dedupIDRE             = regexp.MustCompile(`(?i)(?:dedup|alreadySeen|seenEvents|contains|exists|lookup|cache)[A-Za-z0-9_.]*\s*\([^\n]*(?:computed|recomputed|actual|expected|\.GetID\s*\()`)
	freshnessRE           = regexp.MustCompile(`(?i)(?:created_at|createdAt)[^\n]*(?:<|>|<=|>=|before|after|fresh|stale|monotonic)|(?:fresh|stale|monotonic)[^\n]*(?:created_at|createdAt)`)
	lengthOrMACRE         = regexp.MustCompile(`(?i)(?:(?:(?:ciphertext|payload|content)\s*\.\s*(?:len|length)|len\s*\(\s*(?:ciphertext|payload|content)\s*\))\s*(?:%|<|>|==|!=)|blockSize|block_size|(?:verify|check)[A-Za-z0-9_]*(?:mac|hmac|tag|integrity)|(?:hmac|aead|poly1305|authenticate)[A-Za-z0-9_]*\s*\()`)

	v2UseNameRE = regexp.MustCompile(`(?i)(?:store|save|persist|render|display|trust|accept|process)[A-Za-z0-9_]*(?:event|message|profile|contact|dm)?`)
	v7UseNameRE = regexp.MustCompile(`(?i)(?:lookup|find|get|contains|cached|cache|dedup|seen)[A-Za-z0-9_]*(?:event|id|profile)?`)
	v1UseNameRE = regexp.MustCompile(`(?i)(?:match[A-Za-z0-9_]*contact|attribute[A-Za-z0-9_]*profile|display[A-Za-z0-9_]*sender|trust[A-Za-z0-9_]*(?:key|pubkey)|dm[A-Za-z0-9_]*sender)`)
	r1UseNameRE = regexp.MustCompile(`(?i)(?:store|save|persist|insert|write)[A-Za-z0-9_]*(?:event|message|dm)?`)
	r2UseNameRE = regexp.MustCompile(`(?i)(?:decrypt)[A-Za-z0-9_]*(?:dm|message|event|content)?`)

	wireIDRE      = regexp.MustCompile(`(?i)(?:event|ev|evt)\s*(?:\.\s*id|\[\s*["']id["']\s*\])`)
	pubkeyRE      = regexp.MustCompile(`(?i)(?:event|ev|evt)\s*(?:\.\s*pubkey|\[\s*["']pubkey["']\s*\])`)
	persistenceRE = regexp.MustCompile(`(?i)\b(?:db|store|repo|repository|eventStore|events)\s*\.\s*(?:put|set|save|insert|write|persist|add)\s*\(`)
	cacheLookupRE = regexp.MustCompile(`(?i)\b(?:cache|eventCache|verificationCache|verifiedEvents|seenEvents|dedup)[A-Za-z0-9_]*\s*\.\s*(?:get|has|contains|lookup|find)\s*\(`)
)

// Analyze walks the codemap call graph from Nostr ingest surfaces to sensitive uses.
func (*AbsenceAnalyzer) Analyze(ctx context.Context, repoPath string, codeMap *codemap.Map, surfaces surface.Result) securityscan.ScanResult {
	result := securityscan.ScanResult{}
	if codeMap == nil || repoPath == "" {
		return result
	}
	nodes := loadAbsenceNodes(repoPath, codeMap, surfaces)
	result.FilesScanned = len(codeMap.Files)
	result.RulesChecked = 5

	var starts []string
	for id, node := range nodes {
		if node.ingest {
			starts = append(starts, id)
		}
	}
	sort.Strings(starts)

	seenFindings := make(map[string]bool)
	for _, start := range starts {
		if ctx.Err() != nil {
			break
		}
		walkAbsencePaths(ctx, codeMap, nodes, start, nil, pathChecks{}, seenFindings, &result.Findings)
	}
	sort.Slice(result.Findings, func(i, j int) bool {
		if result.Findings[i].File != result.Findings[j].File {
			return result.Findings[i].File < result.Findings[j].File
		}
		if result.Findings[i].Line != result.Findings[j].Line {
			return result.Findings[i].Line < result.Findings[j].Line
		}
		return result.Findings[i].RuleID < result.Findings[j].RuleID
	})
	return result
}

func loadAbsenceNodes(repoPath string, codeMap *codemap.Map, surfaces surface.Result) map[string]*absenceNode {
	nodes := make(map[string]*absenceNode)
	for _, file := range codeMap.Files {
		data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(file.Path)))
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, symbol := range file.Symbols {
			start, end := int(symbol.StartLine), int(symbol.EndLine)
			if start < 1 || start > len(lines) {
				continue
			}
			if end < start {
				end = start
			}
			if end > len(lines) {
				end = len(lines)
			}
			body := strings.Join(lines[start-1:end], "\n")
			node := &absenceNode{symbol: symbol, body: body}
			classifyAbsenceNode(node)
			nodes[symbol.ID] = node
		}
	}
	for _, location := range surfaces.Locations {
		for _, node := range nodes {
			if node.symbol.Path != filepath.ToSlash(location.File) || location.Line < int(node.symbol.StartLine) || location.Line > int(node.symbol.EndLine) {
				continue
			}
			switch location.Tag {
			case "nostr-event-ingest", "nostr-relay-subscription-handling":
				node.ingest = true
			case "nostr-signature-verification":
				node.verify = true
			case "nostr-event-cache":
				if lineHasWireID(node.body, location.Evidence) {
					node.v7Use = location.Line
				}
			case "nostr-preview-render-path":
				node.v2Use = firstPositive(node.v2Use, location.Line)
			case "nostr-encrypt-decrypt":
				if r2UseNameRE.MatchString(node.symbol.Name) {
					node.r2Use = firstPositive(node.r2Use, location.Line)
				}
			}
		}
	}
	return nodes
}

func classifyAbsenceNode(node *absenceNode) {
	body, name := node.body, node.symbol.Name
	node.ingest = eventIngestRE.MatchString(name) || strings.Contains(strings.ToUpper(body), `["EVENT"`) || strings.Contains(strings.ToUpper(body), `['EVENT'`)
	node.verify = verifyRE.MatchString(body)
	node.recomputeID = explicitRecomputeIDRE.MatchString(body) || (serializeIDRE.MatchString(body) && sha256RE.MatchString(body))
	node.compareID = compareIDRE.MatchString(body)
	node.authPubkey = authPubkeyRE.MatchString(body)
	node.dedupID = dedupIDRE.MatchString(body)
	node.freshness = freshnessRE.MatchString(body)
	node.lengthOrMAC = lengthOrMACRE.MatchString(body)

	if v2UseNameRE.MatchString(name) || persistenceRE.MatchString(body) {
		node.v2Use = int(node.symbol.StartLine)
	}
	if (v7UseNameRE.MatchString(name) || cacheLookupRE.MatchString(body)) && wireIDRE.MatchString(body) {
		node.v7Use = int(node.symbol.StartLine)
	}
	if v1UseNameRE.MatchString(name) && pubkeyRE.MatchString(body) {
		node.v1Use = int(node.symbol.StartLine)
	}
	if r1UseNameRE.MatchString(name) || persistenceRE.MatchString(body) {
		node.r1Use = int(node.symbol.StartLine)
	}
	if r2UseNameRE.MatchString(name) {
		node.r2Use = int(node.symbol.StartLine)
	}
}

func walkAbsencePaths(ctx context.Context, codeMap *codemap.Map, nodes map[string]*absenceNode, id string, path []string, checks pathChecks, seenFindings map[string]bool, findings *[]securityscan.SecurityFinding) {
	if ctx.Err() != nil || len(path) >= maxAbsenceDepth || containsString(path, id) {
		return
	}
	node := nodes[id]
	if node == nil {
		return
	}
	checks = checks.with(node)
	path = append(append([]string(nil), path...), id)

	if node.v2Use > 0 && !checks.verify {
		emitAbsence("NOSTR-V2", "high", node, node.v2Use, path, nodes, "A received Nostr event reaches a use site without signature verification.", "Verify every received event signature before it is stored, rendered, or trusted.", seenFindings, findings)
	}
	if node.v7Use > 0 && !(checks.recomputeID && checks.compareID) {
		emitAbsence("NOSTR-V7", "critical", node, node.v7Use, path, nodes, "A wire-supplied event id reaches a cache or store lookup without recomputation and integrity comparison.", "Recompute the NIP-01 event id and reject mismatches before cache or dedup lookup.", seenFindings, findings)
	}
	if node.v1Use > 0 && !checks.authPubkey {
		emitAbsence("NOSTR-V1", "high", node, node.v1Use, path, nodes, "A pubkey from a received event is used as a trust anchor without an authenticity check.", "Authenticate keys with a pinned key, out-of-band fingerprint, or key-transparency lookup.", seenFindings, findings)
	}
	if node.r1Use > 0 && !(checks.recomputeID && checks.dedupID) && !checks.freshness {
		emitAbsence("NOSTR-R1", "high", node, node.r1Use, path, nodes, "An accepted event reaches persistence without recomputed-id deduplication or created_at freshness/monotonicity validation.", "Deduplicate on the recomputed event id and enforce created_at freshness or monotonicity before persistence.", seenFindings, findings)
	}
	if node.r2Use > 0 && !checks.lengthOrMAC {
		emitAbsence("NOSTR-R2", "high", node, node.r2Use, path, nodes, "A DM decrypt path accepts arbitrary or truncated ciphertext without length or integrity validation.", "Validate ciphertext length and integrity before decryption; prefer authenticated NIP-44 encryption.", seenFindings, findings)
	}

	for _, next := range codeMap.Callees(id) {
		walkAbsencePaths(ctx, codeMap, nodes, next, path, checks, seenFindings, findings)
	}
}

func (c pathChecks) with(node *absenceNode) pathChecks {
	c.verify = c.verify || node.verify
	c.recomputeID = c.recomputeID || node.recomputeID
	c.compareID = c.compareID || node.compareID
	c.authPubkey = c.authPubkey || node.authPubkey
	c.dedupID = c.dedupID || node.dedupID
	c.freshness = c.freshness || node.freshness
	c.lengthOrMAC = c.lengthOrMAC || node.lengthOrMAC
	return c
}

func emitAbsence(ruleID, severity string, node *absenceNode, line int, path []string, nodes map[string]*absenceNode, description, action string, seen map[string]bool, findings *[]securityscan.SecurityFinding) {
	key := fmt.Sprintf("%s:%s:%d", ruleID, node.symbol.Path, line)
	if seen[key] {
		return
	}
	seen[key] = true
	labels := make([]string, 0, len(path))
	for _, id := range path {
		if n := nodes[id]; n != nil {
			labels = append(labels, fmt.Sprintf("%s:%d %s", n.symbol.Path, n.symbol.StartLine, n.symbol.Name))
		}
	}
	suggestion := remediation(ruleID, action)
	if source := knowledge.VulnerabilitySource(ruleID); source != "" && !strings.Contains(suggestion, source) {
		suggestion += " Source: " + source + "."
	}
	*findings = append(*findings, securityscan.SecurityFinding{
		RuleID: ruleID, Severity: severity, Category: "security",
		File: node.symbol.Path, Line: line,
		Evidence:    "[" + ruleID + "] " + strings.Join(labels, " -> "),
		Description: description, Suggestion: suggestion,
		Confidence: AbsenceConfidence,
	})
}

func lineHasWireID(body, evidence string) bool {
	return wireIDRE.MatchString(body) || wireIDRE.MatchString(evidence)
}

func firstPositive(a, b int) int {
	if a > 0 {
		return a
	}
	return b
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
