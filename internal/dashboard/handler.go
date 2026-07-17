// Package dashboard provides an embedded web dashboard for Drydock operators.
// It serves a lightweight htmx-based UI and JSON API endpoints for reviewing
// service health, review history, repository activity, and quality trends.
package dashboard

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"drydock/internal/metrics"
)

//go:embed assets
var assetsFS embed.FS

// DataStore is the subset of db.Store needed by the dashboard.
type DataStore interface {
	DB() *sql.DB
}

// Handler serves the analytics dashboard UI and API endpoints.
type Handler struct {
	store       DataStore
	logger      *slog.Logger
	startTime   time.Time
	bearerToken string
}

// Option configures a dashboard handler.
type Option func(*Handler)

// WithBearerToken protects dashboard and API routes with a bearer token.
func WithBearerToken(token string) Option {
	return func(h *Handler) { h.bearerToken = strings.TrimSpace(token) }
}

// New creates a dashboard handler.
func New(store DataStore, logger *slog.Logger, opts ...Option) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		store:     store,
		logger:    logger,
		startTime: time.Now(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Register mounts dashboard routes onto the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	// Serve embedded static assets.
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		h.logger.Error("failed to load embedded dashboard assets", "error", err)
		return
	}
	mux.Handle("/dashboard/", h.authorize(http.StripPrefix("/dashboard/", http.FileServer(http.FS(sub)))))

	// API endpoints.
	mux.Handle("/api/stats", h.authorize(http.HandlerFunc(h.handleStats)))
	mux.Handle("/api/reviews", h.authorize(http.HandlerFunc(h.handleReviews)))
	mux.Handle("/api/repos", h.authorize(http.HandlerFunc(h.handleRepos)))
	mux.Handle("/api/quality", h.authorize(http.HandlerFunc(h.handleQuality)))
}

// --- Stats endpoint ---

// StatsResponse contains aggregate dashboard statistics.
type StatsResponse struct {
	EventsIngested   int64 `json:"events_ingested"`
	PatchesReceived  int64 `json:"patches_received"`
	ReviewsPublished int64 `json:"reviews_published"`
	ReviewsFailed    int64 `json:"reviews_failed"`
	ReviewsPending   int64 `json:"reviews_pending"`
	ReposTracked     int64 `json:"repos_tracked"`
	MetaReviewsRun   int64 `json:"meta_reviews_run"`
	Conversations    int64 `json:"conversations"`
	UptimeSeconds    int64 `json:"uptime_seconds"`
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
	queries := []struct {
		query string
		dest  *int64
	}{
		{"SELECT COUNT(*) FROM ingested_events", &stats.EventsIngested},
		{"SELECT COUNT(*) FROM patch_events", &stats.PatchesReceived},
		{"SELECT COUNT(*) FROM review_log WHERE status = 'published'", &stats.ReviewsPublished},
		{"SELECT COUNT(*) FROM review_log WHERE status = 'failed'", &stats.ReviewsFailed},
		{"SELECT COUNT(*) FROM review_log WHERE status IN ('pending', 'reviewing')", &stats.ReviewsPending},
		{"SELECT COUNT(*) FROM repositories", &stats.ReposTracked},
		{"SELECT COUNT(*) FROM meta_review_log", &stats.MetaReviewsRun},
		{"SELECT COUNT(*) FROM conversations", &stats.Conversations},
	}
	for _, query := range queries {
		if err := db.QueryRowContext(ctx, query.query).Scan(query.dest); err != nil {
			h.writeFailure(w, "stats", err)
			return
		}
	}

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
	if err := db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		h.writeFailure(w, "reviews", err)
		return
	}

	// Paginated results.
	query := "SELECT patch_event_id, repo_id, status, COALESCE(review_event_id, ''), COALESCE(failure_reason, ''), created_at, updated_at FROM review_log" +
		where + " ORDER BY updated_at DESC LIMIT ? OFFSET ?"
	queryArgs := append(append([]any{}, args...), limit, offset)

	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		h.writeFailure(w, "reviews", err)
		return
	}
	defer rows.Close()

	var reviews []ReviewEntry
	for rows.Next() {
		var re ReviewEntry
		if err := rows.Scan(&re.PatchEventID, &re.RepoID, &re.Status, &re.ReviewEventID, &re.FailureReason, &re.CreatedAt, &re.UpdatedAt); err != nil {
			h.writeFailure(w, "reviews", err)
			return
		}
		reviews = append(reviews, re)
	}
	if err := rows.Err(); err != nil {
		h.writeFailure(w, "reviews", err)
		return
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
		h.writeFailure(w, "repos", err)
		return
	}
	defer rows.Close()

	var repos []RepoEntry
	for rows.Next() {
		var re RepoEntry
		if err := rows.Scan(&re.RepoID, &re.Name, &re.ReviewCount, &re.LastActivity); err != nil {
			h.writeFailure(w, "repos", err)
			return
		}
		repos = append(repos, re)
	}
	if err := rows.Err(); err != nil {
		h.writeFailure(w, "repos", err)
		return
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
		h.writeFailure(w, "quality", err)
		return
	}
	defer rows.Close()

	var entries []QualityEntry
	for rows.Next() {
		var e QualityEntry
		if err := rows.Scan(&e.ID, &e.DatasetID, &e.Recall, &e.FPR, &e.CalMAE, &e.CreatedAt); err != nil {
			h.writeFailure(w, "quality", err)
			return
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		h.writeFailure(w, "quality", err)
		return
	}
	if entries == nil {
		entries = []QualityEntry{}
	}

	writeJSON(w, entries)
}

// --- Helpers ---

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newErrorResponse(code, message string) errorResponse {
	var response errorResponse
	response.Error.Code = code
	response.Error.Message = message
	return response
}

func (h *Handler) authorize(next http.Handler) http.Handler {
	if h.bearerToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(token), []byte(h.bearerToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="drydock-management"`)
			writeJSONStatus(w, http.StatusUnauthorized, newErrorResponse("unauthorized", "valid bearer token required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) writeFailure(w http.ResponseWriter, endpoint string, err error) {
	metrics.DashboardFailures.With(endpoint).Inc()
	h.logger.Error("dashboard request failed", "endpoint", endpoint, "error", err)
	writeJSONStatus(w, http.StatusInternalServerError, newErrorResponse("database_error", fmt.Sprintf("dashboard %s data unavailable", endpoint)))
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

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
