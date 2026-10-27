package relayroute

import (
	"fmt"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

func ref(t *testing.T, identifier string) scope.RepositoryRef {
	t.Helper()
	r, err := scope.ParseRepositoryRef("30617:" + nostr.GetPublicKey(nostr.Generate()).Hex() + ":" + identifier)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rejectedReasons(p Plan) map[string]string {
	out := map[string]string{}
	for _, r := range p.Rejected {
		out[r.Relay] = r.Reason
	}
	return out
}

func TestNormalizeRelayURL(t *testing.T) {
	cases := map[string]string{
		"WSS://Relay.Example.COM/":        "wss://relay.example.com",
		"wss://relay.example.com/path/":   "wss://relay.example.com/path",
		"ws://relay.example.com:7777?x=1": "ws://relay.example.com:7777",
	}
	for in, want := range cases {
		got, err := NormalizeRelayURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeRelayURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "https://relay.example.com", "wss://", "wss://user:pw@relay.example.com", "relay.example.com"} {
		if _, err := NormalizeRelayURL(bad); err == nil {
			t.Errorf("NormalizeRelayURL(%q) accepted", bad)
		}
	}
}

func TestBuildProductionPolicyRejectsInsecureAndPrivateHosts(t *testing.T) {
	r := ref(t, "svc")
	plan := Build([]Repository{{Ref: r, Relays: []string{
		"ws://plain.example.com",
		"wss://localhost:4848",
		"wss://127.0.0.1",
		"wss://10.1.2.3",
		"wss://192.168.1.9",
		"wss://[fe80::1]",
		"wss://relay.local",
		"wss://[fe80::1%25en0]",
		"wss://[::1]",
		"wss://good.example.com",
	}}}, nil, Policy{MaxPerRepository: -1, MaxTotal: -1})
	if got := plan.RelayURLs(); len(got) != 1 || got[0] != "wss://good.example.com" {
		t.Fatalf("admitted = %v", got)
	}
	reasons := rejectedReasons(plan)
	if reasons["ws://plain.example.com"] != "insecure scheme" {
		t.Fatalf("plain scheme reason = %q", reasons["ws://plain.example.com"])
	}
	for _, u := range []string{"wss://localhost:4848", "wss://127.0.0.1", "wss://10.1.2.3", "wss://192.168.1.9", "wss://[fe80::1]", "wss://relay.local", "wss://[fe80::1%25en0]", "wss://[::1]"} {
		if reasons[u] != "loopback or private host" {
			t.Errorf("%s reason = %q", u, reasons[u])
		}
	}
}

func TestBuildDevelopmentPolicyAllowsInsecure(t *testing.T) {
	r := ref(t, "svc")
	plan := Build([]Repository{{Ref: r, Relays: []string{"ws://localhost:4848"}}}, nil, Policy{MaxPerRepository: -1, MaxTotal: -1, AllowInsecure: true})
	if got := plan.RelayURLs(); len(got) != 1 || got[0] != "ws://localhost:4848" {
		t.Fatalf("admitted = %v rejected=%v", got, plan.Rejected)
	}
}

func TestBuildAllowDenySuffixes(t *testing.T) {
	r := ref(t, "svc")
	relays := []string{"wss://a.grasp.example.org", "wss://relay.evil.net", "wss://grasp.example.org", "wss://other.example.com"}
	plan := Build([]Repository{{Ref: r, Relays: relays}}, nil, Policy{
		MaxPerRepository: -1, MaxTotal: -1,
		Allow: []string{".grasp.example.org", "example.com"},
		Deny:  []string{"evil.net", "other.example.com"},
	})
	got := plan.RelayURLs()
	if len(got) != 2 || got[0] != "wss://a.grasp.example.org" || got[1] != "wss://grasp.example.org" {
		t.Fatalf("admitted = %v", got)
	}
	reasons := rejectedReasons(plan)
	if reasons["wss://relay.evil.net"] != "denied host" || reasons["wss://other.example.com"] != "denied host" {
		t.Fatalf("deny reasons = %v", reasons)
	}
}

func TestBuildPerRepositoryCapKeepsDeclarationOrderAndSkipsBase(t *testing.T) {
	r := ref(t, "svc")
	base := []string{"wss://base.example.com/"}
	relays := []string{"wss://base.example.com", "wss://one.example.com", "wss://one.example.com/", "wss://two.example.com", "wss://three.example.com", "wss://four.example.com"}
	plan := Build([]Repository{{Ref: r, Relays: relays}}, base, Policy{MaxPerRepository: 2, MaxTotal: -1})
	got := plan.RelayURLs()
	if len(got) != 2 || got[0] != "wss://one.example.com" || got[1] != "wss://two.example.com" {
		t.Fatalf("admitted = %v", got)
	}
	reasons := rejectedReasons(plan)
	if reasons["wss://three.example.com"] != "per-repository relay cap" || reasons["wss://four.example.com"] != "per-repository relay cap" {
		t.Fatalf("cap reasons = %v", reasons)
	}
	if _, ok := reasons["wss://base.example.com"]; ok {
		t.Fatal("base relay must be silently skipped, not rejected")
	}
}

func TestBuildGlobalCapIsDeterministicAndPrefersSharedRelays(t *testing.T) {
	shared := "wss://shared.example.com"
	var repos []Repository
	for i := 0; i < 5; i++ {
		repos = append(repos, Repository{Ref: ref(t, fmt.Sprintf("r%d", i)), Relays: []string{shared, fmt.Sprintf("wss://solo%d.example.com", i)}})
	}
	plan := Build(repos, nil, Policy{MaxPerRepository: -1, MaxTotal: 3})
	got := plan.RelayURLs()
	if len(got) != 3 || got[0] != shared || !plan.Truncated {
		t.Fatalf("admitted = %v truncated=%v", got, plan.Truncated)
	}
	if len(plan.Relays[shared]) != 5 {
		t.Fatalf("shared relay repos = %d", len(plan.Relays[shared]))
	}
	again := Build(repos, nil, Policy{MaxPerRepository: -1, MaxTotal: 3})
	if strings.Join(again.RelayURLs(), ",") != strings.Join(got, ",") {
		t.Fatalf("plan not deterministic: %v vs %v", again.RelayURLs(), got)
	}
	truncated := 0
	for _, rej := range plan.Rejected {
		if rej.Reason == "global relay cap" {
			truncated++
		}
	}
	if truncated != 3 {
		t.Fatalf("truncated rejections = %d, want 3 (six candidates, cap of three)", truncated)
	}
}

func TestBuildZeroCapsDisableCustomerRelays(t *testing.T) {
	r := ref(t, "svc")
	for _, policy := range []Policy{{MaxPerRepository: 0, MaxTotal: -1}, {MaxPerRepository: -1, MaxTotal: 0}} {
		plan := Build([]Repository{{Ref: r, Relays: []string{"wss://good.example.com"}}}, nil, policy)
		if len(plan.Relays) != 0 || len(plan.Rejected) != 1 || plan.Rejected[0].Reason != "customer relays disabled" {
			t.Fatalf("policy %+v: %+v", policy, plan)
		}
	}
}
