// Package eventkind centralizes the Nostr event kinds consumed by Drydock.
package eventkind

import "fiatjaf.com/nostr"

const (
	Deletion               nostr.Kind = 5
	MonitoredRepositories  nostr.Kind = 30001
	RepositoryAnnouncement            = nostr.KindRepositoryAnnouncement
	RepositoryState                   = nostr.KindRepositoryState
	Patch                             = nostr.KindPatch
	GitPullRequest         nostr.Kind = 1618
	GitPullRequestUpdate   nostr.Kind = 1619
	Issue                  nostr.Kind = 1621
	Comment                           = nostr.KindComment
	StatusOpen                        = nostr.KindStatusOpen
	StatusApplied                     = nostr.KindStatusApplied
	StatusClosed                      = nostr.KindStatusClosed
	StatusDraft                       = nostr.KindStatusDraft
	Label                  nostr.Kind = 1985
	EncryptedDirectMessage            = nostr.KindEncryptedDirectMessage
	SealedDirectMessage    nostr.Kind = 14
	GiftWrap                          = nostr.KindGiftWrap
	IDESession                        = nostr.KindApplicationSpecificData
	ContextVM              nostr.Kind = 25910
	ReviewerProfile                   = nostr.KindHandlerInformation
	ZapReceipt             nostr.Kind = 9735
	CASAudit               nostr.Kind = 4903
)

// AutofixTagValue is the NIP-12 "t" topic-tag value Drydock stamps on the
// autofix patches it publishes. The publisher stamps it and the ingest path
// reads it back for review-loop suppression, so both sides must agree on the
// exact string; a rename on one side alone makes Drydock review its own output.
const AutofixTagValue = "drydock-autofix"

// DependencyUpgradeTagValue is the NIP-12 "t" topic-tag value Drydock stamps on
// the dependency-upgrade patches it publishes. Like AutofixTagValue it is the
// loop-suppression marker: the upgrade publisher stamps it and the ingest path
// reads it back so Drydock does not ingest and review its own upgrade patches.
// A root NIP-34 upgrade patch carries no autofix tag, so it needs its own marker
// or ingest would treat it as an ordinary inbound patch. Both sides must agree
// on the exact string.
const DependencyUpgradeTagValue = "drydock-dependency-upgrade"
