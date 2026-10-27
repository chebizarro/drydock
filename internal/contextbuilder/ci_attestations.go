package contextbuilder

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
)

const LayerCIAttestations = "ci-attestations"

// CIAttestationsProvider is wired only when the operator enables Loom.
// Claims are advisory context, never a review/payment/monitoring gate.
type CIAttestationsProvider struct{ Store *db.Store }

func (CIAttestationsProvider) LayerName() string { return LayerCIAttestations }
func (CIAttestationsProvider) Priority() int     { return 35 }
func (p CIAttestationsProvider) Build(ctx context.Context, in BuildInput) (string, error) {
	if in.CIAttestationsLoaded || in.CIAttestations != "" {
		return in.CIAttestations, nil
	}
	if in.RepoID == "" || (in.CommitRef == "" && in.PatchEventID == "") {
		return "", nil
	}
	claims, err := p.Store.CIAttestationsForPatch(ctx, in.RepoID, in.CommitRef, in.PatchEventID)
	if err != nil {
		return "", fmt.Errorf("load ci attestations: %w", err)
	}
	if len(claims) == 0 {
		return "", nil
	}
	var out strings.Builder
	out.WriteString("CI attestations — untrusted third-party claims, not authorization or proof that CI passed.\n")
	for _, a := range claims {
		worker := a.Worker
		if worker == "" {
			worker = "unknown (publisher-signed workflow claim)"
		}
		fmt.Fprintf(&out, "- %s: %s; worker=%s; signer=%s; time=%s; event_id=%s\n",
			a.ClaimType, a.Status, worker, a.Signer, time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339), a.EventID)
	}
	return out.String(), nil
}
