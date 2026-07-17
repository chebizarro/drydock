package db

const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at INTEGER NOT NULL
);

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
  status_event_id TEXT,
  status_event_kind INTEGER NOT NULL DEFAULT 0,
  status_published_at INTEGER NOT NULL DEFAULT 0,
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

CREATE TABLE IF NOT EXISTS listener_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

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

CREATE TABLE IF NOT EXISTS prompt_gap_queue (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  gap_text TEXT NOT NULL,
  consumed INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_gap_queue_consumed ON prompt_gap_queue(consumed);

CREATE TABLE IF NOT EXISTS prompt_versions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  prompt_name TEXT NOT NULL,
  version INTEGER NOT NULL,
  content TEXT NOT NULL,
  parent_version INTEGER NOT NULL DEFAULT 0,
  source_gap_ids TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'candidate'
    CHECK (status IN ('active', 'candidate', 'rolled_back')),
  eval_score REAL,
  created_at INTEGER NOT NULL,
  UNIQUE(prompt_name, version)
);
CREATE INDEX IF NOT EXISTS idx_prompt_versions_name_status ON prompt_versions(prompt_name, status);

CREATE TABLE IF NOT EXISTS drift_flags (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  meta_review_id INTEGER NOT NULL,
  notes TEXT NOT NULL DEFAULT '',
  flagged_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_drift_flags_meta_review_id ON drift_flags(meta_review_id);

CREATE TABLE IF NOT EXISTS conversations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  review_event_id TEXT NOT NULL,
  reply_event_id TEXT NOT NULL UNIQUE,
  response_event_id TEXT,
  repo_id TEXT NOT NULL,
  patch_event_id TEXT NOT NULL,
  reply_author TEXT NOT NULL,
  reply_content TEXT NOT NULL,
  response_content TEXT NOT NULL DEFAULT '',
  turn_number INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'published', 'failed')),
  created_at INTEGER NOT NULL,
  UNIQUE(review_event_id, turn_number)
);
CREATE INDEX IF NOT EXISTS idx_conversations_review ON conversations(review_event_id);
CREATE INDEX IF NOT EXISTS idx_conversations_reply ON conversations(reply_event_id);
CREATE INDEX IF NOT EXISTS idx_conversations_response ON conversations(response_event_id);

CREATE TABLE IF NOT EXISTS review_payments (
  patch_event_id TEXT PRIMARY KEY,
  repo_id TEXT NOT NULL,
  author_pubkey TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'token_spent', 'authorized')),
  access_kind TEXT NOT NULL DEFAULT ''
    CHECK (access_kind IN ('', 'free_tier', 'subscription', 'cashu_review', 'cashu_subscription')),
  requested_mode TEXT NOT NULL DEFAULT 'review'
    CHECK (requested_mode IN ('review', 'subscription')),
  token_hash TEXT,
  mint_url TEXT NOT NULL DEFAULT '',
  token_amount_sats INTEGER NOT NULL DEFAULT 0,
  invoice_id TEXT NOT NULL DEFAULT '',
  invoice_request TEXT NOT NULL DEFAULT '',
  invoice_expires_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_review_payments_token_hash
  ON review_payments(token_hash)
  WHERE token_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_review_payments_author_repo
  ON review_payments(author_pubkey, repo_id);

CREATE TABLE IF NOT EXISTS payment_subscriptions (
  author_pubkey TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  source_patch_event_id TEXT NOT NULL UNIQUE,
  source_token_hash TEXT NOT NULL UNIQUE,
  paid_amount_sats INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (author_pubkey, repo_id)
);
CREATE INDEX IF NOT EXISTS idx_payment_subscriptions_expires_at
  ON payment_subscriptions(expires_at);

CREATE TABLE IF NOT EXISTS free_review_usage (
  author_pubkey TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  usage_day TEXT NOT NULL,
  used_count INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (author_pubkey, repo_id, usage_day)
);

CREATE TABLE IF NOT EXISTS codechat_turns (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sender_pubkey TEXT NOT NULL,
  event_id TEXT NOT NULL UNIQUE,
  repo_id TEXT NOT NULL DEFAULT '',
  question TEXT NOT NULL,
  response TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'published', 'failed')),
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_codechat_turns_sender ON codechat_turns(sender_pubkey);
CREATE INDEX IF NOT EXISTS idx_codechat_turns_sender_repo ON codechat_turns(sender_pubkey, repo_id);
CREATE INDEX IF NOT EXISTS idx_codechat_turns_created ON codechat_turns(created_at);

CREATE TABLE IF NOT EXISTS ide_gateway_fixes (
  fix_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  author_pubkey TEXT NOT NULL DEFAULT '',
  file TEXT NOT NULL DEFAULT '',
  diff TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (fix_id, session_id)
);
CREATE INDEX IF NOT EXISTS idx_ide_gateway_fixes_session ON ide_gateway_fixes(session_id);
CREATE INDEX IF NOT EXISTS idx_ide_gateway_fixes_created ON ide_gateway_fixes(created_at);

CREATE TABLE IF NOT EXISTS reviewer_profiles (
  pubkey TEXT PRIMARY KEY,
  display_name TEXT NOT NULL DEFAULT '',
  languages TEXT NOT NULL DEFAULT '',
  domains TEXT NOT NULL DEFAULT '',
  availability TEXT NOT NULL DEFAULT 'available'
    CHECK (availability IN ('available', 'limited', 'unavailable')),
  price_per_review INTEGER NOT NULL DEFAULT 0,
  max_concurrent INTEGER NOT NULL DEFAULT 3,
  event_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reviewer_profiles_availability ON reviewer_profiles(availability);

CREATE TABLE IF NOT EXISTS reviewer_reputations (
  pubkey TEXT PRIMARY KEY,
  overall_score REAL NOT NULL DEFAULT 0.5,
  total_reviews INTEGER NOT NULL DEFAULT 0,
  accepted_reviews INTEGER NOT NULL DEFAULT 0,
  rejected_reviews INTEGER NOT NULL DEFAULT 0,
  average_rating REAL NOT NULL DEFAULT 0,
  acceptance_rate REAL NOT NULL DEFAULT 0,
  last_review_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reviewer_reputations_score ON reviewer_reputations(overall_score);

CREATE TABLE IF NOT EXISTS review_assignments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  patch_event_id TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  reviewer_pubkey TEXT NOT NULL,
  requester_pubkey TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'accepted', 'rejected', 'completed', 'expired')),
  priority INTEGER NOT NULL DEFAULT 2,
  price_sats INTEGER NOT NULL DEFAULT 0,
  assignment_event_id TEXT NOT NULL UNIQUE,
  acceptance_event_id TEXT,
  completion_event_id TEXT,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(patch_event_id, reviewer_pubkey)
);
CREATE INDEX IF NOT EXISTS idx_review_assignments_reviewer ON review_assignments(reviewer_pubkey);
CREATE INDEX IF NOT EXISTS idx_review_assignments_status ON review_assignments(status);
CREATE INDEX IF NOT EXISTS idx_review_assignments_expires ON review_assignments(expires_at);

CREATE TABLE IF NOT EXISTS review_feedback (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  assignment_id INTEGER NOT NULL,
  reviewer_pubkey TEXT NOT NULL,
  rater_pubkey TEXT NOT NULL,
  rating INTEGER NOT NULL CHECK (rating >= 1 AND rating <= 5),
  comment TEXT NOT NULL DEFAULT '',
  event_id TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  FOREIGN KEY (assignment_id) REFERENCES review_assignments(id) ON DELETE CASCADE,
  UNIQUE(assignment_id, rater_pubkey)
);
CREATE INDEX IF NOT EXISTS idx_review_feedback_reviewer ON review_feedback(reviewer_pubkey);
CREATE INDEX IF NOT EXISTS idx_review_feedback_assignment ON review_feedback(assignment_id);

CREATE TABLE IF NOT EXISTS rate_limits (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key TEXT NOT NULL,
  timestamp INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rate_limits_key_timestamp ON rate_limits(key, timestamp);
`
