package eval

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/codemap"
	"git.sharegap.net/cascadia/drydock/internal/nostrscan"
	"git.sharegap.net/cascadia/drydock/internal/securityscan"
	"git.sharegap.net/cascadia/drydock/internal/testutil"
)

// nostrRoleFixture pairs an eval fixture repository with the rule IDs the
// production Nostr wiring actually flags on its vulnerable variant.
//
// These sets are what the production scan path emits over the eval corpus, not
// a wish list: they were observed by running the wiring, and each entry is a
// real detection that would break if the rule regressed.
//
// The corpus exercises the full NP25 set end to end (DRYDOCK-09c1): the client
// fixture seeds the absence walk for NOSTR-V1/V2/V7/R2 and the presence rules
// NOSTR-V3/V5/V6; the signer fixture covers NOSTR-V3/V4; and the relay fixture
// presents an ["EVENT", ...] ingest surface that reaches persistence, seeding
// NOSTR-R1. The vulnerable/fixed call chains are real function calls, so they
// hold under both the LSP-resolved and the grep-fallback code map.
type nostrRoleFixture struct {
	role      nostrscan.Role
	source    string   // file within the fixture repo, e.g. "client.go"
	wantRules []string // NP25 rule IDs the vulnerable variant triggers
}

var nostrRoleFixtures = []nostrRoleFixture{
	{role: nostrscan.RoleClient, source: "client.go", wantRules: []string{"NOSTR-V1", "NOSTR-V2", "NOSTR-V3", "NOSTR-V5", "NOSTR-V6", "NOSTR-V7", "NOSTR-R2"}},
	{role: nostrscan.RoleSigner, source: "signer.go", wantRules: []string{"NOSTR-V3", "NOSTR-V4"}},
	{role: nostrscan.RoleRelay, source: "relay.go", wantRules: []string{"NOSTR-R1"}},
}

// TestNostrProductionWiringFlagsVulnerableFixtures runs the exact scan wiring
// auditengine uses — securityscan.NewWithRuleSets(PresenceRulesForRoles(roles),
// SurfaceRules()).ScanFiles + LocateSurface + nostrscan.AnalyzeAbsences — over
// the paired vulnerable/fixed fixtures and checks that each vulnerable variant
// is flagged while its fixed counterpart is cleared. Nothing here is scripted:
// deleting or breaking a rule makes the corresponding assertion fail.
func TestNostrProductionWiringFlagsVulnerableFixtures(t *testing.T) {
	for _, fixture := range nostrRoleFixtures {
		t.Run(string(fixture.role), func(t *testing.T) {
			vulnRules := scanNostrFixture(t, fixture, "vulnerable")
			fixedRules := scanNostrFixture(t, fixture, "fixed")
			t.Logf("%s vulnerable rules=%v fixed rules=%v", fixture.role, sortedKeys(vulnRules), sortedKeys(fixedRules))

			for _, ruleID := range fixture.wantRules {
				if !vulnRules[ruleID] {
					t.Errorf("%s vulnerable fixture did not trigger %s (got %v)", fixture.role, ruleID, sortedKeys(vulnRules))
				}
				if fixedRules[ruleID] {
					t.Errorf("%s fixed fixture falsely triggered %s (got %v)", fixture.role, ruleID, sortedKeys(fixedRules))
				}
			}
		})
	}
}

// scanNostrFixture runs the production Nostr scan wiring against one fixture
// variant and returns the set of rule IDs it flagged.
func scanNostrFixture(t *testing.T, fixture nostrRoleFixture, variant string) map[string]bool {
	t.Helper()
	ctx := context.Background()
	repo := initNostrFixtureRepo(t, filepath.Join("testdata", "nostr", string(fixture.role), variant))
	files := []string{fixture.source}
	roles := []nostrscan.Role{fixture.role}

	scanner := securityscan.NewWithRuleSets(nostrscan.PresenceRulesForRoles(roles), nostrscan.SurfaceRules())
	presence, err := scanner.ScanFiles(ctx, repo, files, "")
	if err != nil {
		t.Fatalf("scan %s/%s: %v", fixture.role, variant, err)
	}
	surfaces := scanner.LocateSurface(ctx, repo, files)

	codeMap, err := codemap.New(codemap.WithCacheDir(t.TempDir())).Build(ctx, repo, "HEAD")
	if err != nil {
		t.Fatalf("build code map for %s/%s: %v", fixture.role, variant, err)
	}
	absence := nostrscan.AnalyzeAbsences(ctx, repo, codeMap, surfaces)

	found := make(map[string]bool)
	for _, f := range append(append([]securityscan.SecurityFinding(nil), presence.Findings...), absence.Findings...) {
		if nostrscan.RuleAppliesToRoles(f.RuleID, roles) {
			found[f.RuleID] = true
		}
	}
	return found
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func initNostrFixtureRepo(t *testing.T, fixture string) string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(fixture, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(fixture, path)
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
