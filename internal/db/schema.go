package db

const schemaSQL = `
CREATE TABLE IF NOT EXISTS ingested_events (
  event_id TEXT PRIMARY KEY,
  kind INTEGER NOT NULL,
  author_pubkey TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  first_seen_at INTEGER NOT NULL,
  raw_event_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ingested_events_kind ON ingested_events(kind);

CREATE TABLE IF NOT EXISTS repositories (
  repo_id TEXT PRIMARY KEY,
  pubkey TEXT NOT NULL,
  identifier TEXT NOT NULL,
  announcement_event_id TEXT NOT NULL UNIQUE,
  name TEXT,
  description TEXT,
  clone_urls TEXT NOT NULL DEFAULT '',
  relays TEXT NOT NULL DEFAULT '',
  raw_event_json TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS patch_events (
  event_id TEXT PRIMARY KEY,
  repo_id TEXT NOT NULL DEFAULT '',
  kind INTEGER NOT NULL,
  author_pubkey TEXT NOT NULL,
  root_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  content TEXT NOT NULL DEFAULT '',
  raw_event_json TEXT NOT NULL,
  seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_patch_events_repo_id ON patch_events(repo_id);
CREATE INDEX IF NOT EXISTS idx_patch_events_root_id ON patch_events(root_id);
CREATE INDEX IF NOT EXISTS idx_patch_events_kind ON patch_events(kind);

CREATE TABLE IF NOT EXISTS patch_event_relays (
  patch_event_id TEXT NOT NULL,
  relay_url TEXT NOT NULL,
  seen_at INTEGER NOT NULL,
  PRIMARY KEY (patch_event_id, relay_url),
  FOREIGN KEY (patch_event_id) REFERENCES patch_events(event_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_patch_event_relays_patch ON patch_event_relays(patch_event_id);

CREATE TABLE IF NOT EXISTS review_events (
  event_id TEXT PRIMARY KEY,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  raw_event_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_review_events_patch_event_id ON review_events(patch_event_id);
CREATE INDEX IF NOT EXISTS idx_review_events_repo_id ON review_events(repo_id);

CREATE TABLE IF NOT EXISTS thread_cache (
  root_id TEXT PRIMARY KEY,
  event_ids TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS root_statuses (
  root_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL DEFAULT '',
  status_kind INTEGER NOT NULL,
  status_event_id TEXT NOT NULL,
  author_pubkey TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (root_event_id, repo_id)
);
CREATE INDEX IF NOT EXISTS idx_root_statuses_kind ON root_statuses(status_kind);

CREATE TABLE IF NOT EXISTS review_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'reviewing', 'published', 'failed')),
  review_event_id TEXT,
  failure_reason TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(patch_event_id, repo_id)
);
CREATE INDEX IF NOT EXISTS idx_review_log_status ON review_log(status);

CREATE TABLE IF NOT EXISTS repository_snapshots (
  repo_id TEXT PRIMARY KEY,
  snapshot_event_id TEXT NOT NULL,
  author_pubkey TEXT NOT NULL,
  head_branch TEXT NOT NULL DEFAULT '',
  ref_commits_csv TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_repository_snapshots_created_at ON repository_snapshots(created_at);

CREATE TABLE IF NOT EXISTS meta_review_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  context_hash TEXT NOT NULL,
  changed_lines_csv TEXT NOT NULL DEFAULT '',
  gate_reason TEXT NOT NULL DEFAULT '',
  response_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_meta_review_patch_repo ON meta_review_log(patch_event_id, repo_id);
CREATE INDEX IF NOT EXISTS idx_meta_review_context_hash ON meta_review_log(context_hash);

CREATE TABLE IF NOT EXISTS meta_review_routes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  why_missed TEXT NOT NULL,
  action TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_meta_review_routes_patch_repo ON meta_review_routes(patch_event_id, repo_id);

CREATE TABLE IF NOT EXISTS few_shot_reviews (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  example_type TEXT NOT NULL CHECK (example_type IN ('positive', 'negative')),
  content TEXT NOT NULL,
  confidence REAL NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_few_shot_reviews_created_at ON few_shot_reviews(created_at);

CREATE TABLE IF NOT EXISTS eval_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  dataset_id TEXT NOT NULL,
  total_cases INTEGER NOT NULL,
  expected_findings INTEGER NOT NULL,
  predicted_findings INTEGER NOT NULL,
  true_positives INTEGER NOT NULL,
  false_positives INTEGER NOT NULL,
  false_negatives INTEGER NOT NULL,
  recall REAL NOT NULL,
  false_positive_rate REAL NOT NULL,
  calibration_mae REAL NOT NULL,
  high_conf_precision REAL NOT NULL,
  details_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_eval_runs_created_at ON eval_runs(created_at);
`
