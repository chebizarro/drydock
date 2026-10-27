package repoconfig

import (
	"fmt"
	"strings"
	"testing"
)

func TestConcordNotificationConfig(t *testing.T) {
	valid := fmt.Sprintf("notifications:\n  concord:\n    community: %q\n    channel: %q\n    epoch: 0\n    relays: [wss://community.example]\n", strings.Repeat("a", 64), strings.Repeat("b", 64))
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.Concord == nil || cfg.Notifications.Concord.CredentialID() != strings.Repeat("a", 64)+"/"+strings.Repeat("b", 64)+"/0" {
		t.Fatal("public locator not preserved")
	}
	empty, err := Parse(nil)
	if err != nil || empty.Notifications.Concord != nil {
		t.Fatal("must default off")
	}
	for _, bad := range []string{
		strings.Replace(valid, strings.Repeat("a", 64), "npub1not-a-community", 1),
		strings.Replace(valid, strings.Repeat("b", 64), strings.Repeat("B", 64), 1),
		strings.Replace(valid, "    epoch: 0\n", "", 1),
		strings.Replace(valid, "epoch: 0", "epoch: -1", 1),
		strings.Replace(valid, "wss://community.example", "ws://community.example", 1),
		strings.Replace(valid, "wss://community.example", "wss://user:password@community.example", 1),
		strings.Replace(valid, "wss://community.example", "'wss://community.example?secret=x'", 1),
		strings.Replace(valid, "[wss://community.example]", "[]", 1),
	} {
		// Invalid notification values must not fail the whole file (callers
		// would fall back to Default() and lose the security policy); they
		// disable notifications for this repository only.
		cfg, err := Parse([]byte(bad))
		if err != nil {
			t.Fatalf("invalid notification config failed the whole file: %s: %v", bad, err)
		}
		if cfg.Notifications.Concord != nil {
			t.Fatalf("accepted invalid notification config: %s", bad)
		}
	}
	// A secret in repo config is an unknown field and is refused outright.
	if _, err := Parse([]byte(valid + "    stream_key: do-not-accept-secrets-here\n")); err == nil {
		t.Fatal("stream_key in .drydock.yaml must be rejected")
	}
	// Notifications are optional delivery policy, not an authorization block.
	if RequiresFailClosed([]byte(valid)) {
		t.Fatal("notification config must not become review authority")
	}
}

func TestInvalidNotificationsDoNotDiscardSecurityPolicy(t *testing.T) {
	cfg, err := Parse([]byte(`
security:
  secret_scan: true
notifications:
  concord:
    community: "not-a-valid-community"
    channel: ""
    relays: ["ftp://nope"]
`))
	if err != nil {
		t.Fatalf("an invalid notifications block must not fail the whole file: %v", err)
	}
	if cfg.Notifications.Concord != nil {
		t.Fatalf("invalid notifications must be dropped, got %+v", cfg.Notifications.Concord)
	}
	if !cfg.Security.SecretScan {
		t.Fatal("security policy was discarded alongside the invalid notifications block")
	}
}
