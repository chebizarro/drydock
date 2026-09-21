package contextvm

import (
	"strconv"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/scope"

	"fiatjaf.com/nostr"
)

// EnvelopePolicy configures the shared addressing and replay-protection checks
// applied to a signed ContextVM request envelope. Per-method differences —
// which tags are bound and, crucially, whether replay protection (an
// expiration tag) is mandatory — are expressed as fields here rather than as
// independent per-handler validators that drift apart.
type EnvelopePolicy struct {
	// ServicePubkey, when non-empty, requires exactly one p tag addressing it.
	ServicePubkey string
	// Method, when non-empty, requires exactly one method tag equal to it.
	Method string
	// RelatedID, when non-empty, requires exactly one e tag equal to it.
	RelatedID string
	// ExpirationRequired makes a valid, in-window expiration tag mandatory.
	// When false, an expiration tag is optional but is still validated when
	// present. This single flag is the one intentional replay-protection
	// difference between methods.
	ExpirationRequired bool
	// MaxLifetime, when > 0, bounds the expiration relative to the event's
	// created_at timestamp.
	MaxLifetime time.Duration
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
}

// ValidateEnvelope authenticates and validates a signed ContextVM request
// envelope against policy. It is the single source of the addressing and
// replay-protection policy so no method's envelope handling silently diverges
// from the others. Callers layer only genuinely per-method binding (repository
// address, IDE session/request) on top of the result.
func ValidateEnvelope(req Request, policy EnvelopePolicy) *Error {
	if req.Event.ID == nostr.ZeroID || req.Sender == nostr.ZeroPK {
		return &Error{Code: ErrorInvalidRequest, Message: "authenticated request event and sender are required"}
	}
	if policy.ServicePubkey != "" {
		recipients := envelopeTagValues(req.Event.Tags, "p")
		if len(recipients) != 1 {
			return &Error{Code: ErrorInvalidParams, Message: "exactly one p tag is required"}
		}
		recipient, err := scope.ParsePubkey(recipients[0])
		if err != nil || recipient.Hex() != policy.ServicePubkey {
			return &Error{Code: ErrorInvalidParams, Message: "p tag must address the service"}
		}
	}
	if policy.Method != "" {
		methods := envelopeTagValues(req.Event.Tags, "method")
		if len(methods) != 1 || methods[0] != policy.Method {
			return &Error{Code: ErrorInvalidParams, Message: "exactly one matching method tag is required"}
		}
	}
	if policy.RelatedID != "" {
		related := envelopeTagValues(req.Event.Tags, "e")
		if len(related) != 1 || related[0] != policy.RelatedID {
			return &Error{Code: ErrorInvalidParams, Message: "exactly one e tag matching the related id is required"}
		}
	}
	return validateExpiration(req, policy)
}

func validateExpiration(req Request, policy EnvelopePolicy) *Error {
	expirations := envelopeTagValues(req.Event.Tags, "expiration")
	if len(expirations) > 1 {
		return &Error{Code: ErrorInvalidParams, Message: "at most one expiration tag is allowed"}
	}
	if len(expirations) == 0 {
		if policy.ExpirationRequired {
			return &Error{Code: ErrorInvalidParams, Message: "exactly one expiration tag is required"}
		}
		return nil
	}
	expiresAt, err := strconv.ParseInt(expirations[0], 10, 64)
	if err != nil {
		return &Error{Code: ErrorInvalidParams, Message: "expiration must be a Unix timestamp"}
	}
	now := time.Now
	if policy.Now != nil {
		now = policy.Now
	}
	if expiresAt <= now().Unix() {
		return &Error{Code: ErrorExpired, Message: "request envelope expired"}
	}
	if policy.MaxLifetime > 0 {
		createdAt := int64(req.Event.CreatedAt)
		if createdAt <= 0 || expiresAt <= createdAt {
			return &Error{Code: ErrorInvalidParams, Message: "expiration must be after the event timestamp"}
		}
		if expiresAt > createdAt+int64(policy.MaxLifetime/time.Second) {
			return &Error{Code: ErrorInvalidParams, Message: "expiration exceeds the maximum envelope lifetime"}
		}
	}
	return nil
}

func envelopeTagValues(tags nostr.Tags, name string) []string {
	values := make([]string, 0, 1)
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			values = append(values, strings.TrimSpace(tag[1]))
		}
	}
	return values
}
