package pipeline

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
)

func TestCICommitReferenceUsesExactPatchOrResolvedPRTip(t *testing.T) {
	commit, tip := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, tc := range []struct {
		kind      nostr.Kind
		tags      nostr.Tags
		tip, want string
	}{
		{eventkind.Patch, nostr.Tags{{"commit", commit}}, "", commit},
		{eventkind.Patch, nostr.Tags{{"c", strings.ToUpper(commit)}}, "", commit},
		{eventkind.Patch, nostr.Tags{{"commit", commit}, {"c", tip}}, "", ""},
		{eventkind.Patch, nostr.Tags{{"commit", "abcdef0"}}, "", ""},
		{eventkind.Patch, nil, "", ""},
		{eventkind.GitPullRequest, nostr.Tags{{"commit", commit}}, tip, tip},
		{eventkind.GitPullRequestUpdate, nostr.Tags{{"commit", commit}}, "", ""},
	} {
		if got := ciCommitReference(nostr.Event{Kind: tc.kind, Tags: tc.tags}, tc.tip); got != tc.want {
			t.Fatalf("commit reference: %q, want %q", got, tc.want)
		}
	}
}
