// Package loom consumes the public Budabit/Hive CI -> Loom lineage as third-party claims.
package loom

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/monitoring"
	"git.sharegap.net/cascadia/drydock/internal/scope"
)

type Config struct {
	MaxPerRepository, MaxTotal int
	TTL                        time.Duration
}

type Service struct {
	store      *db.Store
	membership monitoring.Membership
	cfg        Config
	logger     *slog.Logger
	mu         sync.Mutex
	changes    chan struct{}
	now        func() time.Time
	since      nostr.Timestamp
}

func New(store *db.Store, membership monitoring.Membership, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, membership: membership, cfg: cfg, logger: logger, changes: make(chan struct{}, 1), now: time.Now, since: nostr.Timestamp(time.Now().Add(-cfg.TTL).Unix())}
}
func (s *Service) Changes() <-chan struct{} { return s.changes }
func (s *Service) changed() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

// SingleTag rejects ambiguous lineage instead of choosing an attacker-controlled first tag.
func singleTag(tags nostr.Tags, name string) string {
	value := ""
	count := 0
	for _, t := range tags {
		if len(t) > 1 && t[0] == name {
			count++
			value = t[1]
		}
	}
	if count != 1 {
		return ""
	}
	return value
}
func validHex(s string, sizes ...int) bool {
	ok := false
	for _, n := range sizes {
		if len(s) == n {
			ok = true
		}
	}
	if !ok {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func pubkey(s string) (nostr.PubKey, bool) {
	p, e := nostr.PubKeyFromHex(s)
	return p, e == nil && p != nostr.ZeroPK && p.Hex() == s
}

func (s *Service) HandleEvent(ctx context.Context, e nostr.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	if e.Kind == eventkind.HiveWorkflowRun {
		ref, err := scope.RepositoryRefFromTags(e.Tags)
		if err != nil || s.membership == nil || !s.membership.Contains(ref.Address) {
			return nil
		}
		publisher, ok := pubkey(singleTag(e.Tags, "publisher"))
		if !ok {
			return nil
		}
		commit := strings.ToLower(singleTag(e.Tags, "commit"))
		if !validHex(commit, 40, 64) || e.CreatedAt.Time().Add(s.cfg.TTL).Unix() <= now {
			return nil
		}
		removed, err := s.store.TrackLoomRun(ctx, db.LoomRun{
			EventID: e.ID.Hex(), RepoID: ref.RepositoryID, RepositoryAddress: ref.Address, CommitRef: commit,
			Author: e.PubKey.Hex(), Publisher: publisher.Hex(), CreatedAt: int64(e.CreatedAt), ExpiresAt: e.CreatedAt.Time().Add(s.cfg.TTL).Unix(),
		}, s.cfg.MaxPerRepository, s.cfg.MaxTotal, now)
		if err != nil {
			return err
		}
		if removed > 0 {
			s.logger.Warn("loom run tracking truncated or expired", "removed", removed)
		}
		s.changed()
		return nil
	}
	runs, err := s.store.ListLoomRuns(ctx, now)
	if err != nil {
		return err
	}
	parent := singleTag(e.Tags, "e")
	if !validHex(parent, 64) {
		return nil
	}
	for _, r := range runs {
		if s.membership == nil || !s.membership.Contains(r.RepositoryAddress) {
			continue
		}
		if e.Kind == eventkind.LoomJobRequest && parent == r.EventID && e.PubKey.Hex() == r.Author {
			worker, ok := pubkey(singleTag(e.Tags, "p"))
			if !ok {
				return nil
			}
			if err := s.store.AttachLoomJob(ctx, r.EventID, e.ID.Hex(), worker.Hex(), int64(e.CreatedAt)); err != nil {
				return err
			}
			s.changed()
			return nil
		}
		claimType, status := "", ""
		switch {
		case e.Kind == eventkind.HiveWorkflowResult && parent == r.EventID && e.PubKey.Hex() == r.Publisher:
			claimType = "workflow"
			status = singleTag(e.Tags, "status")
			if status != "success" && status != "failed" {
				return nil
			}
		case e.Kind == eventkind.LoomJobResult && r.JobID != "" && parent == r.JobID && e.PubKey.Hex() == r.Worker:
			claimType = "compute job"
			success := singleTag(e.Tags, "success")
			exit, err := strconv.Atoi(singleTag(e.Tags, "exit_code"))
			if err != nil || exit < 0 || (success != "true" && success != "false") || (success == "true" && exit != 0) {
				return nil
			}
			status = "failed"
			if success == "true" {
				status = "success"
			}
		default:
			continue
		}
		if err := s.store.CompleteLoomRun(ctx, db.CIAttestation{
			EventID: e.ID.Hex(), RepoID: r.RepoID, CommitRef: r.CommitRef, RunID: r.EventID, Worker: r.Worker,
			Signer: e.PubKey.Hex(), ClaimType: claimType, Status: status, CreatedAt: int64(e.CreatedAt),
		}); err != nil {
			return err
		}
		s.changed()
		return nil
	}
	return nil
}

// Filters returns exact per-repository filters for the existing listener. There
// is no second subscription loop. The first terminal result evicts a run.
func (s *Service) Filters(ctx context.Context, refs []scope.RepositoryRef) (map[string][]nostr.Filter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	removed, err := s.store.PruneLoomRuns(ctx, s.cfg.MaxPerRepository, s.cfg.MaxTotal, now)
	if err != nil {
		return nil, err
	}
	if removed > 0 {
		s.logger.Warn("loom run tracking truncated or expired", "removed", removed)
	}
	runs, err := s.store.ListLoomRuns(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]nostr.Filter, len(refs))
	for _, ref := range refs {
		if s.membership == nil || !s.membership.Contains(ref.Address) {
			continue
		}
		out[ref.Address] = []nostr.Filter{{Kinds: []nostr.Kind{eventkind.HiveWorkflowRun}, Tags: nostr.TagMap{"a": {ref.Address}}, Since: s.since, Limit: s.cfg.MaxPerRepository}}
	}
	counts := map[string]int{}
	total := 0
	for _, r := range runs {
		if _, ok := out[r.RepositoryAddress]; !ok {
			continue
		}
		if counts[r.RepositoryAddress] >= s.cfg.MaxPerRepository || total >= s.cfg.MaxTotal {
			continue
		}
		counts[r.RepositoryAddress]++
		total++
		author, _ := nostr.PubKeyFromHex(r.Author)
		publisher, _ := nostr.PubKeyFromHex(r.Publisher)
		out[r.RepositoryAddress] = append(out[r.RepositoryAddress],
			nostr.Filter{Kinds: []nostr.Kind{eventkind.LoomJobRequest}, Authors: []nostr.PubKey{author}, Tags: nostr.TagMap{"e": {r.EventID}}},
			nostr.Filter{Kinds: []nostr.Kind{eventkind.HiveWorkflowResult}, Authors: []nostr.PubKey{publisher}, Tags: nostr.TagMap{"e": {r.EventID}}})
		if r.JobID != "" {
			worker, _ := nostr.PubKeyFromHex(r.Worker)
			out[r.RepositoryAddress] = append(out[r.RepositoryAddress], nostr.Filter{Kinds: []nostr.Kind{eventkind.LoomJobResult}, Authors: []nostr.PubKey{worker}, Tags: nostr.TagMap{"e": {r.JobID}}})
		}
	}
	return out, nil
}

// NextExpiry drives resource eviction, never event completion or delivery.
func (s *Service) NextExpiry(ctx context.Context) (time.Time, error) {
	runs, err := s.store.ListLoomRuns(ctx, s.now().Unix())
	if err != nil {
		return time.Time{}, err
	}
	var next int64
	for _, r := range runs {
		if next == 0 || r.ExpiresAt < next {
			next = r.ExpiresAt
		}
	}
	if next == 0 {
		return time.Time{}, nil
	}
	return time.Unix(next, 0), nil
}
