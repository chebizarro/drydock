// Package dashboard provides an embedded web dashboard for Drydock operators.
// It serves a lightweight htmx-based UI and JSON API endpoints for reviewing
// service health, review history, repository activity, and quality trends.
package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed assets
var assetsFS embed.FS

// DataStore is the subset of db.Store needed by the dashboard.
type DataStore interface {
	DB() *sql.DB
}

// Handler serves the analytics dashboard UI and API endpoints.
type Handler struct {
	store     DataStore
	logger    *slog.Logger
	startTime time.Time
}

// New creates a dashboard handler.
func New(store DataStore, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		store:     store,
		logger:    logger,
		startTime: time.Now(),
	}
}

// Register mounts dashboard routes onto the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	// Serve embedded static assets.
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		h.logger.Error("failed to load embedded dashboard assets", "error", err)
		return
	}
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.FS(sub))))

	// API endpoints.
	mux.HandleFunc("/api/stats", h.handleStats)
	mux.HandleFunc("/api/reviews", h.handleReviews)
	mux.HandleFunc("/api/repos", h.handleRepos)
	mux.HandleFunc("/api/quality", h.handleQuality)
}

// --- Stats endpoint ---

// StatsResponse contains aggregate dashboard statistics.
type StatsResponse struct {
	EventsIngested   int64  `json:"events_ingested"`
	PatchesReceived  int64  `json:"patches_received"`
	ReviewsPublished int64  `json:"reviews_published"`
	ReviewsFailed    int64  `json:"reviews_failed"`
	ReviewsPending   int64  `json:"reviews_pending"`
	ReposTracked     int64  `json:"repos_tracked"`
	MetaReviewsRun   int64  `json:"meta_reviews_run"`
	Conversations    int64  `json:"conversations"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db := h.store.DB()
	stats := StatsResponse{
		UptimeSeconds: int64(time.Since(h.startTime).Seconds()),
	}

	// Run count queries concurrently would be better but for simplicity
	// and SQLite's write-serialized nature, sequential is fine.
	row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ingested_events")
	row.Scan(&stats.EventsIngested)

	row = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM patch_events")
	row.Scan(&stats.PatchesReceived)

	row = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM review_log WHERE status = 'published'")
	row.Scan(&stats.ReviewsPublished)

	row = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM review_log WHERE status = 'failed'")
	row.Scan(&stats.ReviewsFailed)

	row = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM review_log WHERE status IN ('pending', 'reviewing')")
	row.Scan(&stats.ReviewsPending)

	row = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM repositories")
	row.Scan(&stats.ReposTracked)

	row = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM meta_review_log")
	row.Scan(&stats.MetaReviewsRun)

	row = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conversations")
	row.Scan(&stats.Conversations)

	writeJSON(w, stats)
}

// --- Reviews endpoint ---

// ReviewEntry represents a single review in the history list.
type ReviewEntry struct {
	PatchEventID  string `json:"patch_event_id"`
	RepoID        string `json:"repo_id"`
	Status        string `json:"status"`
	ReviewEventID string `json:"review_event_id,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// ReviewsResponse is the paginated reviews list.
type ReviewsResponse struct {
	Reviews []ReviewEntry `json:"reviews"`
	Total   int64         `json:"total"`
	Page    int           `json:"page"`
	Limit   int           `json:"limit"`
}

func (h *Handler) handleReviews(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	page := queryInt(r, "page", 1)
	limit := queryInt(r, "limit", 20)
	if limit > 100 {
		limit = 100
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit

	repoFilter := r.URL.Query().Get("repo")
	statusFilter := r.URL.Query().Get("status")

	db := h.store.DB()

	// Build where clause.
	var conditions []string
	var args []any
	if repoFilter != "" {
		conditions = append(conditions, "repo_id = ?")
		args = append(args, repoFilter)
	}
	if statusFilter != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, statusFilter)
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// Total count.
	var total int64
	countQuery := "SELECT COUNT(*) FROM review_log" + where
	db.QueryRowContext(ctx, countQuery, args...).Scan(&total)

	// Paginated results.
	query := "SELECT patch_event_id, repo_id, status, COALESCE(review_event_id, ''), COALESCE(failure_reason, ''), created_at, updated_at FROM review_log" +
		where + " ORDER BY updated_at DESC LIMIT ? OFFSET ?"
	queryArgs := append(args, limit, offset)

	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		h.logger.Warn("dashboard reviews query failed", "error", err)
		writeJSON(w, ReviewsResponse{Reviews: []ReviewEntry{}, Total: 0, Page: page, Limit: limit})
		return
	}
	defer rows.Close()

	var reviews []ReviewEntry
	for rows.Next() {
		var re ReviewEntry
		if err := rows.Scan(&re.PatchEventID, &re.RepoID, &re.Status, &re.ReviewEventID, &re.FailureReason, &re.CreatedAt, &re.UpdatedAt); err != nil {
			continue
		}
		reviews = append(reviews, re)
	}
	if reviews == nil {
		reviews = []ReviewEntry{}
	}

	writeJSON(w, ReviewsResponse{
		Reviews: reviews,
		Total:   total,
		Page:    page,
		Limit:   limit,
	})
}

// --- Repos endpoint ---

// RepoEntry represents a repository with review counts.
type RepoEntry struct {
	RepoID       string `json:"repo_id"`
	Name         string `json:"name"`
	ReviewCount  int64  `json:"review_count"`
	LastActivity int64  `json:"last_activity"`
}

func (h *Handler) handleRepos(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db := h.store.DB()
	rows, err := db.QueryContext(ctx, `
		SELECT r.repo_id, COALESCE(r.name, r.identifier), 
		       COALESCE(rl.cnt, 0), COALESCE(rl.last_update, r.created_at)
		FROM repositories r
		LEFT JOIN (
			SELECT repo_id, COUNT(*) as cnt, MAX(updated_at) as last_update
			FROM review_log
			GROUP BY repo_id
		) rl ON r.repo_id = rl.repo_id
		ORDER BY COALESCE(rl.last_update, r.created_at) DESC
		LIMIT 100
	`)
	if err != nil {
		h.logger.Warn("dashboard repos query failed", "error", err)
		writeJSON(w, []RepoEntry{})
		return
	}
	defer rows.Close()

	var repos []RepoEntry
	for rows.Next() {
		var re RepoEntry
		if err := rows.Scan(&re.RepoID, &re.Name, &re.ReviewCount, &re.LastActivity); err != nil {
			continue
		}
		repos = append(repos, re)
	}
	if repos == nil {
		repos = []RepoEntry{}
	}

	writeJSON(w, repos)
}

// --- Quality endpoint ---

// QualityEntry represents a single eval run data point.
type QualityEntry struct {
	ID        int64   `json:"id"`
	DatasetID string  `json:"dataset_id"`
	Recall    float64 `json:"recall"`
	FPR       float64 `json:"fpr"`
	CalMAE    float64 `json:"calibration_mae"`
	CreatedAt int64   `json:"created_at"`
}

func (h *Handler) handleQuality(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db := h.store.DB()
	rows, err := db.QueryContext(ctx, `
		SELECT id, dataset_id, recall, false_positive_rate, calibration_mae, created_at
		FROM eval_runs
		ORDER BY created_at DESC
		LIMIT 50
	`)
	if err != nil {
		h.logger.Warn("dashboard quality query failed", "error", err)
		writeJSON(w, []QualityEntry{})
		return
	}
	defer rows.Close()

	var entries []QualityEntry
	for rows.Next() {
		var e QualityEntry
		if err := rows.Scan(&e.ID, &e.DatasetID, &e.Recall, &e.FPR, &e.CalMAE, &e.CreatedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if entries == nil {
		entries = []QualityEntry{}
	}

	writeJSON(w, entries)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func queryInt(r *http.Request, key string, defaultVal int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}
