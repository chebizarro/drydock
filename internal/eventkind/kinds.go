// Package eventkind centralizes the Nostr event kinds consumed by Drydock.
package eventkind

import "fiatjaf.com/nostr"

const (
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
	ReviewFeedback                    = nostr.KindJobFeedback
	ZapReceipt             nostr.Kind = 9735
)
