package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"drydock/internal/db"
)

func mustOpenStore(t *testing.T) *db.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestHandler(t *testing.T) (*Handler, *db.Store) {
	t.Helper()
	store := mustOpenStore(t)
	h := New(store, nil)
	return h, store
}

func TestStatsEndpointEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var stats StatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if stats.EventsIngested != 0 {
		t.Errorf("expected 0 events, got %d", stats.EventsIngested)
	}
	if stats.UptimeSeconds < 0 {
		t.Error("expected non-negative uptime")
	}
}

func TestStatsEndpointWithData(t *testing.T) {
	h, store := newTestHandler(t)
	db := store.DB()
	ctx := context.Background()

	// Insert some test data.
	db.ExecContext(ctx, "INSERT INTO ingested_events (event_id, kind, author_pubkey, created_at, first_seen_at, raw_event_json) VALUES ('e1', 1, 'pk1', 1000, 1000, '{}')")
	db.ExecContext(ctx, "INSERT INTO ingested_events (event_id, kind, author_pubkey, created_at, first_seen_at, raw_event_json) VALUES ('e2', 1, 'pk2', 1001, 1001, '{}')")
	db.ExecContext(ctx, "INSERT INTO repositories (repo_id, pubkey, identifier, announcement_event_id, clone_urls, raw_event_json, created_at, updated_at) VALUES ('r1', 'pk1', 'test-repo', 'ae1', 'https://example.com/repo.git', '{}', 1000, 1000)")
	db.ExecContext(ctx, "INSERT INTO review_log (patch_event_id, repo_id, status, created_at, updated_at) VALUES ('p1', 'r1', 'published', 1000, 1001)")
	db.ExecContext(ctx, "INSERT INTO review_log (patch_event_id, repo_id, status, failure_reason, created_at, updated_at) VALUES ('p2', 'r1', 'failed', 'timeout', 1000, 1002)")

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var stats StatsResponse
	json.Unmarshal(w.Body.Bytes(), &stats)

	if stats.EventsIngested != 2 {
		t.Errorf("expected 2 events, got %d", stats.EventsIngested)
	}
	if stats.ReviewsPublished != 1 {
		t.Errorf("expected 1 published, got %d", stats.ReviewsPublished)
	}
	if stats.ReviewsFailed != 1 {
		t.Errorf("expected 1 failed, got %d", stats.ReviewsFailed)
	}
	if stats.ReposTracked != 1 {
		t.Errorf("expected 1 repo, got %d", stats.ReposTracked)
	}
}

func TestReviewsEndpointPagination(t *testing.T) {
	h, store := newTestHandler(t)
	d := store.DB()
	ctx := context.Background()

	// Insert 5 reviews.
	for i := 0; i < 5; i++ {
		d.ExecContext(ctx,
			"INSERT INTO review_log (patch_event_id, repo_id, status, created_at, updated_at) VALUES (?, 'r1', 'published', ?, ?)",
			"p"+string(rune('0'+i)), 1000+i, 1000+i)
	}

	mux := http.NewServeMux()
	h.Register(mux)

	// Page 1, limit 2.
	req := httptest.NewRequest("GET", "/api/reviews?page=1&limit=2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp ReviewsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Total != 5 {
		t.Errorf("expected total 5, got %d", resp.Total)
	}
	if len(resp.Reviews) != 2 {
		t.Errorf("expected 2 reviews on page, got %d", len(resp.Reviews))
	}
	if resp.Page != 1 {
		t.Errorf("expected page 1, got %d", resp.Page)
	}
}

func TestReviewsEndpointFilters(t *testing.T) {
	h, store := newTestHandler(t)
	d := store.DB()
	ctx := context.Background()

	d.ExecContext(ctx, "INSERT INTO review_log (patch_event_id, repo_id, status, created_at, updated_at) VALUES ('p1', 'r1', 'published', 1000, 1001)")
	d.ExecContext(ctx, "INSERT INTO review_log (patch_event_id, repo_id, status, created_at, updated_at) VALUES ('p2', 'r2', 'failed', 1000, 1002)")
	d.ExecContext(ctx, "INSERT INTO review_log (patch_event_id, repo_id, status, created_at, updated_at) VALUES ('p3', 'r1', 'published', 1000, 1003)")

	mux := http.NewServeMux()
	h.Register(mux)

	// Filter by status.
	req := httptest.NewRequest("GET", "/api/reviews?status=failed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp ReviewsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Total != 1 {
		t.Errorf("expected 1 failed review, got %d", resp.Total)
	}

	// Filter by repo.
	req = httptest.NewRequest("GET", "/api/reviews?repo=r1", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Total != 2 {
		t.Errorf("expected 2 reviews for r1, got %d", resp.Total)
	}
}

func TestReviewsEndpointEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/reviews", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp ReviewsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if len(resp.Reviews) != 0 {
		t.Errorf("expected empty reviews, got %d", len(resp.Reviews))
	}
	if resp.Total != 0 {
		t.Errorf("expected total 0, got %d", resp.Total)
	}
}

func TestReposEndpoint(t *testing.T) {
	h, store := newTestHandler(t)
	d := store.DB()
	ctx := context.Background()

	d.ExecContext(ctx, "INSERT INTO repositories (repo_id, pubkey, identifier, announcement_event_id, name, clone_urls, raw_event_json, created_at, updated_at) VALUES ('r1', 'pk1', 'myrepo', 'ae1', 'My Project', 'https://example.com/repo.git', '{}', 1000, 1000)")
	d.ExecContext(ctx, "INSERT INTO review_log (patch_event_id, repo_id, status, created_at, updated_at) VALUES ('p1', 'r1', 'published', 1000, 1001)")
	d.ExecContext(ctx, "INSERT INTO review_log (patch_event_id, repo_id, status, created_at, updated_at) VALUES ('p2', 'r1', 'published', 1002, 1003)")

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/repos", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var repos []RepoEntry
	json.Unmarshal(w.Body.Bytes(), &repos)

	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(repos))
	}
	if repos[0].Name != "My Project" {
		t.Errorf("expected name 'My Project', got %q", repos[0].Name)
	}
	if repos[0].ReviewCount != 2 {
		t.Errorf("expected 2 reviews, got %d", repos[0].ReviewCount)
	}
}

func TestReposEndpointEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/repos", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var repos []RepoEntry
	json.Unmarshal(w.Body.Bytes(), &repos)
	if len(repos) != 0 {
		t.Errorf("expected empty repos, got %d", len(repos))
	}
}

func TestQualityEndpointEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/quality", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var entries []QualityEntry
	json.Unmarshal(w.Body.Bytes(), &entries)
	if len(entries) != 0 {
		t.Errorf("expected empty quality data, got %d", len(entries))
	}
}

func TestQualityEndpointWithData(t *testing.T) {
	h, store := newTestHandler(t)
	d := store.DB()
	ctx := context.Background()

	d.ExecContext(ctx, `INSERT INTO eval_runs 
		(dataset_id, total_cases, expected_findings, predicted_findings, true_positives, false_positives, false_negatives,
		 recall, false_positive_rate, calibration_mae, high_conf_precision, details_json, created_at)
		VALUES ('ds1', 10, 5, 4, 3, 1, 2, 0.6, 0.1, 0.05, 0.9, '{}', 1000)`)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/api/quality", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var entries []QualityEntry
	json.Unmarshal(w.Body.Bytes(), &entries)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Recall != 0.6 {
		t.Errorf("expected recall 0.6, got %f", entries[0].Recall)
	}
	if entries[0].DatasetID != "ds1" {
		t.Errorf("expected dataset ds1, got %s", entries[0].DatasetID)
	}
}

func TestDashboardHTMLServed(t *testing.T) {
	h, _ := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for dashboard HTML, got %d", w.Code)
	}
	body := w.Body.String()
	if len(body) < 100 {
		t.Error("expected HTML content")
	}
	if !containsDashboard(body, "Drydock") {
		t.Error("expected dashboard title in HTML")
	}
}

func TestQueryInt(t *testing.T) {
	tests := []struct {
		url  string
		key  string
		def  int
		want int
	}{
		{"/api?page=3", "page", 1, 3},
		{"/api", "page", 1, 1},
		{"/api?page=abc", "page", 1, 1},
		{"/api?page=-5", "page", 1, 1},
		{"/api?limit=200", "limit", 20, 200},
	}
	for _, tt := range tests {
		req := httptest.NewRequest("GET", tt.url, nil)
		got := queryInt(req, tt.key, tt.def)
		if got != tt.want {
			t.Errorf("queryInt(%s, %s, %d) = %d, want %d", tt.url, tt.key, tt.def, got, tt.want)
		}
	}
}

func containsDashboard(s, sub string) bool {
	return len(s) > 0 && len(sub) > 0 && contains(s, sub)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
