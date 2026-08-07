package revieworder

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"drydock/internal/db"
	"drydock/internal/monitoring"
	"drydock/internal/scope"

	"fiatjaf.com/nostr"
)

func TestReactiveAdmissionFollowsLiveMonitoringAndFailsClosedWithoutList(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, ctx, filepath.Join(t.TempDir(), "revieworder.db"))
	operatorSK := nostr.Generate()
	operator := nostr.GetPublicKey(operatorSK)
	registry, err := monitoring.NewRegistry(store, operator.Hex())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Load(ctx); err != nil {
		t.Fatal(err)
	}

	repoSK := nostr.Generate()
	repository, err := scope.ParseRepositoryRef("30617:" + nostr.GetPublicKey(repoSK).Hex() + ":repo")
	if err != nil {
		t.Fatal(err)
	}
	service := New(Config{}, store, scope.Matcher{}, registry, nil, nil, testLogger())

	withoutList := signedPatch(t, nostr.Generate(), repository.Address, "without-list")
	result, err := service.SubmitReactive(ctx, withoutList, repository)
	if err != nil || !result.Skipped || result.SkipReason != "not_monitored" {
		t.Fatalf("startup admission = %+v err=%v", result, err)
	}
	if reason, ok, err := store.GetReviewSkip(ctx, withoutList.ID.Hex(), repository.RepositoryID); err != nil || !ok || reason != "not_monitored" {
		t.Fatalf("startup durable skip = %q ok=%v err=%v", reason, ok, err)
	}

	list := nostr.Event{
		Kind:      monitoring.MonitoredListKind,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", monitoring.ListIdentifier}, {"a", repository.Address}},
	}
	if err := list.Sign(operatorSK); err != nil {
		t.Fatal(err)
	}
	if applied, err := registry.ApplyList(ctx, list); err != nil || !applied {
		t.Fatalf("apply monitored list: applied=%v err=%v", applied, err)
	}

	afterAddition := signedPatch(t, nostr.Generate(), repository.Address, "after-addition")
	result, err = service.SubmitReactive(ctx, afterAddition, repository)
	if err != nil || !result.Queued || result.Skipped {
		t.Fatalf("live addition admission = %+v err=%v", result, err)
	}
	select {
	case task := <-service.Queue():
		if task.PatchEventID != afterAddition.ID.Hex() || task.Invocation != db.ReviewInvocationReactive {
			t.Fatalf("queued task = %+v", task)
		}
	default:
		t.Fatal("monitored patch was not queued")
	}

	emptyReplacement := nostr.Event{
		Kind:      monitoring.MonitoredListKind,
		CreatedAt: nostr.Timestamp(int64(list.CreatedAt) + 1),
		Tags:      nostr.Tags{{"d", monitoring.ListIdentifier}},
	}
	if err := emptyReplacement.Sign(operatorSK); err != nil {
		t.Fatal(err)
	}
	if applied, err := registry.ApplyList(ctx, emptyReplacement); err != nil || !applied {
		t.Fatalf("apply empty replacement: applied=%v err=%v", applied, err)
	}

	afterRemoval := signedPatch(t, nostr.Generate(), repository.Address, "after-removal")
	result, err = service.SubmitReactive(ctx, afterRemoval, repository)
	if err != nil || !result.Skipped || result.SkipReason != "not_monitored" {
		t.Fatalf("live removal admission = %+v err=%v", result, err)
	}
	if reason, ok, err := store.GetReviewSkip(ctx, afterRemoval.ID.Hex(), repository.RepositoryID); err != nil || !ok || reason != "not_monitored" {
		t.Fatalf("removal durable skip = %q ok=%v err=%v", reason, ok, err)
	}
}

func TestEnqueueRecoveredPreservesInvocationMetadataAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.db")
	store := testStore(t, ctx, path)
	task := db.ReviewTask{
		PatchEventID:    "patch",
		RepoID:          "owner:repo",
		Force:           true,
		Invocation:      db.ReviewInvocationContextVM,
		RequesterPubkey: "requester",
		OrderID:         "order-1",
	}
	acquired, err := store.BeginReviewWithClaim(ctx, task.PatchEventID, task.RepoID, db.ReviewClaim{
		Force:           task.Force,
		Invocation:      task.Invocation,
		RequesterPubkey: task.RequesterPubkey,
		OrderID:         task.OrderID,
	})
	if err != nil || !acquired {
		t.Fatalf("initial claim: acquired=%v err=%v", acquired, err)
	}
	if err := store.MarkReviewFailed(ctx, task.PatchEventID, task.RepoID, "temporary"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE review_log SET updated_at=0 WHERE patch_event_id='patch'"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := testStore(t, ctx, path)
	tasks, err := reopened.RequeueFailedReviews(ctx, 0, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("requeue after restart: tasks=%+v err=%v", tasks, err)
	}
	recovered := tasks[0]
	if recovered.Invocation != task.Invocation || recovered.RequesterPubkey != task.RequesterPubkey ||
		recovered.OrderID != task.OrderID || recovered.Force != task.Force {
		t.Fatalf("recovered metadata = %+v, want %+v", recovered, task)
	}

	service := New(Config{QueueSize: 1}, reopened, scope.Matcher{}, nil, nil, nil, testLogger())
	if err := service.EnqueueRecovered(ctx, recovered, "restart"); err != nil {
		t.Fatal(err)
	}
	select {
	case queued := <-service.Queue():
		if queued != recovered {
			t.Fatalf("queued recovery = %+v, want %+v", queued, recovered)
		}
	default:
		t.Fatal("recovered task was not queued")
	}
}

func TestQueueFullLeavesRecoveredTaskDurablyRetryable(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, ctx, filepath.Join(t.TempDir(), "queue-full.db"))
	service := New(Config{QueueSize: 1}, store, scope.Matcher{}, nil, nil, nil, testLogger())

	for _, task := range []db.ReviewTask{
		{PatchEventID: "first", RepoID: "owner:repo", Invocation: db.ReviewInvocationIDE, RequesterPubkey: "ide", OrderID: "one"},
		{PatchEventID: "second", RepoID: "owner:repo", Invocation: db.ReviewInvocationContextVM, RequesterPubkey: "external", OrderID: "two"},
	} {
		acquired, err := store.BeginReviewWithClaim(ctx, task.PatchEventID, task.RepoID, db.ReviewClaim{
			Invocation: task.Invocation, RequesterPubkey: task.RequesterPubkey, OrderID: task.OrderID,
		})
		if err != nil || !acquired {
			t.Fatalf("claim %s: acquired=%v err=%v", task.PatchEventID, acquired, err)
		}
		if err := store.MarkReviewFailed(ctx, task.PatchEventID, task.RepoID, "temporary"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx, "UPDATE review_log SET updated_at=0 WHERE patch_event_id=?", task.PatchEventID); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := store.RequeueFailedReviews(ctx, 0, 10)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("requeue tasks=%+v err=%v", tasks, err)
	}
	if err := service.EnqueueRecovered(ctx, tasks[0], "fill"); err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueRecovered(ctx, tasks[1], "overflow"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("overflow error = %v", err)
	}
	status, err := store.GetReviewStatus(ctx, tasks[1].PatchEventID, tasks[1].RepoID)
	if err != nil || status != "failed" {
		t.Fatalf("overflow status=%q err=%v", status, err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE review_log SET updated_at=0 WHERE patch_event_id=?", tasks[1].PatchEventID); err != nil {
		t.Fatal(err)
	}
	retry, err := store.RequeueFailedReviews(ctx, 0, 10)
	if err != nil || len(retry) != 1 || retry[0].Invocation != db.ReviewInvocationContextVM ||
		retry[0].RequesterPubkey != "external" || retry[0].OrderID != "two" {
		t.Fatalf("retry metadata=%+v err=%v", retry, err)
	}
}

func signedPatch(t *testing.T, sk nostr.SecretKey, repositoryAddress, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"a", repositoryAddress}},
		Content:   content,
	}
	if err := event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return event
}

func testStore(t *testing.T, ctx context.Context, path string) *db.Store {
	t.Helper()
	store, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
