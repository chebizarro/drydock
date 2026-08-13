package targetidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Envelope binds review output to the repository target and exact context
// material that produced it. Field order is part of the canonical encoding.
type Envelope struct {
	RepoID                  string `json:"repo_id"`
	RootID                  string `json:"root_id"`
	PatchEventID            string `json:"patch_event_id"`
	CanonicalRemoteIdentity string `json:"canonical_remote_identity"`
	BaseCommit              string `json:"base_commit"`
	TipCommit               string `json:"tip_commit"`
	DiffSHA256              string `json:"diff_sha256"`
	BundleSHA256            string `json:"bundle_sha256"`
}

// New constructs a normalized envelope. The diff hash supplied by repository
// preparation is retained when present so VerifyMaterials can detect if a
// different diff reaches the review engine.
func New(repoID, rootID, patchEventID, remoteIdentity, baseCommit, tipCommit, preparedDiffSHA, diff, bundle string) Envelope {
	diffSHA := normalizeHex(preparedDiffSHA)
	if diffSHA == "" {
		diffSHA = SHA256(diff)
	}
	return Envelope{
		RepoID:                  strings.TrimSpace(repoID),
		RootID:                  normalizeHex(rootID),
		PatchEventID:            normalizeHex(patchEventID),
		CanonicalRemoteIdentity: strings.TrimSpace(remoteIdentity),
		BaseCommit:              normalizeHex(baseCommit),
		TipCommit:               normalizeHex(tipCommit),
		DiffSHA256:              diffSHA,
		BundleSHA256:            SHA256(bundle),
	}
}

// SHA256 returns the lowercase SHA-256 digest of text.
func SHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// RemoteIdentity returns a stable, credential-safe fingerprint of the
// canonical clone identities advertised by the repository event.
func RemoteIdentity(cloneURLs []string) string {
	values := make([]string, 0, len(cloneURLs))
	seen := make(map[string]struct{}, len(cloneURLs))
	for _, raw := range cloneURLs {
		value := strings.TrimRight(strings.TrimSpace(raw), "/")
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) == 0 {
		return ""
	}
	return "sha256:" + SHA256(strings.Join(values, "\n"))
}

// Hash returns SHA-256 over the canonical JSON envelope. encoding/json emits
// struct fields in declaration order, and New normalizes all hash material.
func (e Envelope) Hash() (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("marshal target identity envelope: %w", err)
	}
	return SHA256(string(canonical)), nil
}

// Validate fails closed when any required target identity field is absent.
// Base/tip may be empty for patch events that do not identify Git commits.
func (e Envelope) Validate() error {
	for name, value := range map[string]string{
		"repo_id": e.RepoID, "root_id": e.RootID, "patch_event_id": e.PatchEventID,
		"canonical_remote_identity": e.CanonicalRemoteIdentity,
		"diff_sha256":               e.DiffSHA256, "bundle_sha256": e.BundleSHA256,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("target identity envelope missing %s", name)
		}
	}
	return nil
}

// VerifyMaterials proves that the diff and context bundle reaching an output
// filter are the same bytes bound into the prepared target envelope.
func (e Envelope) VerifyMaterials(diff, bundle string) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if actual := SHA256(diff); actual != e.DiffSHA256 {
		return fmt.Errorf("target identity envelope diff mismatch: prepared %s, filtering %s", e.DiffSHA256, actual)
	}
	if actual := SHA256(bundle); actual != e.BundleSHA256 {
		return fmt.Errorf("target identity envelope bundle mismatch: prepared %s, filtering %s", e.BundleSHA256, actual)
	}
	return nil
}

func normalizeHex(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
