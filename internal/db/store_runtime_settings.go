package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Runtime setting keys owned by the monitoring control plane.
const (
	RuntimeSettingMonitoringOperator         = "monitoring.operator"
	RuntimeSettingMonitoringOperatorPrevious = "monitoring.operator.previous"
	// RuntimeSettingMonitoringOperatorHasPrevious is "1" when a rollback
	// target exists; an empty previous operator then means "no ceiling".
	RuntimeSettingMonitoringOperatorHasPrevious = "monitoring.operator.has_previous"
)

// GetRuntimeSetting returns a persisted operator setting. Missing keys report
// found=false rather than an error so callers can fall back to bootstrap
// configuration.
func (s *Store) GetRuntimeSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key=?`, strings.TrimSpace(key)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get runtime setting %q: %w", key, err)
	}
	return value, true, nil
}

// SetRuntimeSettings writes several settings atomically, recording who changed
// them and through which event, so a live control-plane change is auditable.
func (s *Store) SetRuntimeSettings(ctx context.Context, values map[string]string, changedBy, changeEventID string) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin runtime settings transaction: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			return errors.New("runtime setting key is required")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_settings(key, value, changed_by, change_event_id, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, changed_by=excluded.changed_by,
				change_event_id=excluded.change_event_id, updated_at=excluded.updated_at`,
			key, value, strings.TrimSpace(changedBy), strings.TrimSpace(changeEventID), now); err != nil {
			return fmt.Errorf("set runtime setting %q: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit runtime settings: %w", err)
	}
	return nil
}
