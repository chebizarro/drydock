package loom

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/scope"
)

type membership map[string]bool

func (m membership) Contains(a string) bool { return m[a] }
func harness(t *testing.T, perRepo, total int) (*Service, *db.Store, scope.RepositoryRef, nostr.SecretKey) {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "ci.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	author := nostr.Generate()
	ref, err := scope.ParseRepositoryRef(fmt.Sprintf("%d:%s:repo", eventkind.RepositoryAnnouncement, author.Public().Hex()))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(store, membership{ref.Address: true}, Config{MaxPerRepository: perRepo, MaxTotal: total, TTL: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, store, ref, author
}
func signed(t *testing.T, sk nostr.SecretKey, kind nostr.Kind, at int64, tags nostr.Tags) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: kind, CreatedAt: nostr.Timestamp(at), Tags: tags}
	if err := e.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return e
}
func run(t *testing.T, s *Service, ref scope.RepositoryRef, author, publisher nostr.SecretKey, at int64) nostr.Event {
	t.Helper()
	e := signed(t, author, eventkind.HiveWorkflowRun, at, nostr.Tags{{"a", ref.Address}, {"publisher", publisher.Public().Hex()}, {"commit", strings.Repeat("b", 40)}})
	if err := s.HandleEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestLineageFiltersAndAuthorEnforcement(t *testing.T) {
	s, store, ref, author := harness(t, 64, 1024)
	ctx := context.Background()
	publisher, worker := nostr.Generate(), nostr.Generate()
	anchor := run(t, s, ref, author, publisher, time.Now().Unix())
	filters, err := s.Filters(ctx, []scope.RepositoryRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	f := filters[ref.Address]
	if len(f) != 3 {
		t.Fatalf("want run + exact job and workflow filters: %#v", f)
	}
	if len(f[0].Tags["a"]) != 1 || f[0].Tags["a"][0] != ref.Address {
		t.Fatal("missing repository scope")
	}
	for _, filter := range f[1:] {
		if len(filter.Authors) != 1 || len(filter.Tags["e"]) != 1 || filter.Tags["e"][0] != anchor.ID.Hex() {
			t.Fatalf("unscoped filter %#v", filter)
		}
	}
	if f[2].Authors[0] != publisher.Public() {
		t.Fatal("workflow author not bound to declared publisher")
	}
	job := signed(t, author, eventkind.LoomJobRequest, time.Now().Unix(), nostr.Tags{{"e", anchor.ID.Hex()}, {"p", worker.Public().Hex()}})
	if err := s.HandleEvent(ctx, job); err != nil {
		t.Fatal(err)
	}
	filters, _ = s.Filters(ctx, []scope.RepositoryRef{ref})
	f = filters[ref.Address]
	if len(f) != 4 || f[3].Authors[0] != worker.Public() || f[3].Tags["e"][0] != job.ID.Hex() {
		t.Fatalf("worker scope: %#v", f)
	}
	bad := signed(t, nostr.Generate(), eventkind.LoomJobResult, time.Now().Unix(), nostr.Tags{{"e", job.ID.Hex()}, {"success", "true"}, {"exit_code", "0"}})
	if err := s.HandleEvent(ctx, bad); err != nil {
		t.Fatal(err)
	}
	claims, _ := store.CIAttestationsForPatch(ctx, ref.RepositoryID, strings.Repeat("b", 40), "")
	if len(claims) != 0 {
		t.Fatal("non-declared signer accepted")
	}
	result := signed(t, worker, eventkind.LoomJobResult, time.Now().Unix(), bad.Tags)
	if err := s.HandleEvent(ctx, result); err != nil {
		t.Fatal(err)
	}
	claims, err = store.CIAttestationsForPatch(ctx, ref.RepositoryID, strings.Repeat("b", 40), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Worker != worker.Public().Hex() || claims[0].Signer != worker.Public().Hex() || claims[0].ClaimType != "compute job" || claims[0].Status != "success" {
		t.Fatalf("claims: %#v", claims)
	}
	remaining, _ := store.ListLoomRuns(ctx, time.Now().Unix())
	if len(remaining) != 0 {
		t.Fatal("result must evict lineage")
	}
	if err := s.HandleEvent(ctx, result); err != nil {
		t.Fatal(err)
	}
	claims, _ = store.CIAttestationsForPatch(ctx, ref.RepositoryID, strings.Repeat("b", 40), "")
	if len(claims) != 1 {
		t.Fatal("result duplicated")
	}
	filters, _ = s.Filters(ctx, []scope.RepositoryRef{ref})
	if len(filters[ref.Address]) != 1 {
		t.Fatal("result filters not evicted")
	}
}
func TestUnmonitoredAndMalformedRunsAddNothing(t *testing.T) {
	s, store, ref, author := harness(t, 64, 1024)
	ctx := context.Background()
	other := ref
	other.Address = strings.Replace(ref.Address, ":repo", ":other", 1)
	run(t, s, other, author, nostr.Generate(), time.Now().Unix())
	malformed := signed(t, author, eventkind.HiveWorkflowRun, time.Now().Unix(), nostr.Tags{{"a", ref.Address}, {"publisher", nostr.Generate().Public().Hex()}, {"publisher", nostr.Generate().Public().Hex()}, {"commit", strings.Repeat("b", 40)}})
	if err := s.HandleEvent(ctx, malformed); err != nil {
		t.Fatal(err)
	}
	runs, _ := store.ListLoomRuns(ctx, time.Now().Unix())
	if len(runs) != 0 {
		t.Fatal("unmonitored/ambiguous run tracked")
	}
	filters, _ := s.Filters(ctx, nil)
	if len(filters) != 0 {
		t.Fatal("no membership must mean no filters")
	}
}
func TestCapsPreferNewestAndTTLExpiry(t *testing.T) {
	s, store, ref, author := harness(t, 2, 3)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	s.now = func() time.Time { return now }
	// Deliver out of order; oldest must lose even when it arrives last.
	publisher := nostr.Generate()
	newest := run(t, s, ref, author, publisher, now.Unix()-1)
	run(t, s, ref, author, publisher, now.Unix()-2)
	run(t, s, ref, author, publisher, now.Unix()-3)
	rows, _ := store.ListLoomRuns(ctx, now.Unix())
	if len(rows) != 2 || rows[0].EventID != newest.ID.Hex() {
		t.Fatalf("per repo cap: %#v", rows)
	}
	other, _ := scope.ParseRepositoryRef(fmt.Sprintf("%d:%s:other", eventkind.RepositoryAnnouncement, author.Public().Hex()))
	s.membership.(membership)[other.Address] = true
	run(t, s, other, author, publisher, now.Unix())
	run(t, s, other, author, publisher, now.Unix()-1)
	rows, _ = store.ListLoomRuns(ctx, now.Unix())
	if len(rows) != 3 {
		t.Fatalf("global cap: %d", len(rows))
	}
	for _, r := range rows {
		if r.CreatedAt < now.Unix()-1 {
			t.Fatal("newest runs not retained")
		}
	}
	expiry, err := s.NextExpiry(ctx)
	if err != nil || expiry.IsZero() {
		t.Fatalf("expiry %v %v", expiry, err)
	}
	now = now.Add(2 * time.Hour)
	filters, err := s.Filters(ctx, []scope.RepositoryRef{ref, other})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range filters {
		if len(f) != 1 {
			t.Fatal("expired secondary filters retained")
		}
	}
	rows, _ = store.ListLoomRuns(ctx, 0)
	if len(rows) != 0 {
		t.Fatal("TTL rows not evicted")
	}
}
func TestWorkflowClaimUsesPublisherNotPretendWorker(t *testing.T) {
	s, store, ref, author := harness(t, 64, 1024)
	ctx := context.Background()
	publisher := nostr.Generate()
	anchor := run(t, s, ref, author, publisher, time.Now().Unix())
	e := signed(t, publisher, eventkind.HiveWorkflowResult, time.Now().Unix(), nostr.Tags{{"e", anchor.ID.Hex()}, {"status", "failed"}})
	if err := s.HandleEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.CIAttestationsForPatch(ctx, ref.RepositoryID, strings.Repeat("b", 40), "")
	if len(rows) != 1 || rows[0].Worker != "" || rows[0].Signer != publisher.Public().Hex() || rows[0].ClaimType != "workflow" {
		t.Fatalf("publisher is not worker: %#v", rows)
	}
}
