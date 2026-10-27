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
		strings.Replace(valid, "wss://community.example", "wss://community.example?secret=x", 1),
		strings.Replace(valid, "[wss://community.example]", "[]", 1),
		valid + "    stream_key: do-not-accept-secrets-here\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid notification config: %s", bad)
		}
	}
	// Notifications are optional delivery policy, not an authorization block.
	if RequiresFailClosed([]byte(valid)) {
		t.Fatal("notification config must not become review authority")
	}
}
