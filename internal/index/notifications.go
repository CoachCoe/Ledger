package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

type Notification struct {
	ID             string                   `json:"id"`
	Recipient      string                   `json:"recipient"`
	Kind           string                   `json:"kind"`
	Subject        NotificationSubject      `json:"subject"`
	Counterpart    *NotificationCounterpart `json:"counterpart"`
	Confidence     string                   `json:"confidence"`
	OverlapKind    string                   `json:"overlap_kind,omitzero"`
	Reason         string                   `json:"reason,omitzero"`
	CreatedAt      string                   `json:"created_at"`
	AcknowledgedAt *string                  `json:"acknowledged_at"`
}
type NotificationSubject struct {
	Path   string         `json:"path,omitzero"`
	Symbol *schema.Symbol `json:"symbol,omitempty"`
}
type NotificationCounterpart struct {
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
	IntentID    string `json:"intent_id"`
}

// Enqueue uses a caller-stable id, preserving original attribution and ack on
// repeated observations. A single SQLite statement is the crash boundary.
func (x *Index) Enqueue(ctx context.Context, n Notification) error {
	raw, err := canon.Marshal(n)
	if err != nil {
		return err
	}
	if err := schema.ValidateRuntime("notification", raw); err != nil {
		return err
	}
	if err := x.notificationFault("enqueue_before"); err != nil {
		return err
	}
	_, err = x.conn().ExecContext(ctx, `INSERT OR IGNORE INTO notifications(id,recipient,doc,created_at) VALUES(?,?,?,?)`, n.ID, n.Recipient, string(raw), n.CreatedAt)
	if err != nil {
		return err
	}
	return x.notificationFault("enqueue_after")
}
func (x *Index) Notifications(ctx context.Context, recipient, since string) ([]Notification, error) {
	return x.notifications(ctx, recipient, since, false)
}

// PendingNotifications avoids loading the recipient's acknowledged history.
func (x *Index) PendingNotifications(ctx context.Context, recipient, since string) ([]Notification, error) {
	return x.notifications(ctx, recipient, since, true)
}

func (x *Index) notifications(ctx context.Context, recipient, since string, pending bool) ([]Notification, error) {
	query := `SELECT doc,acknowledged_at FROM notifications WHERE recipient=? AND (?='' OR julianday(created_at)>=julianday(?))`
	if pending {
		query += ` AND acknowledged_at IS NULL`
	}
	query += ` ORDER BY created_at,id`
	rows, err := x.conn().QueryContext(ctx, query, recipient, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var raw string
		var ack sql.NullString
		if err := rows.Scan(&raw, &ack); err != nil {
			return nil, err
		}
		var n Notification
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			return nil, err
		}
		if ack.Valid {
			n.AcknowledgedAt = &ack.String
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func (x *Index) Acknowledge(ctx context.Context, recipient, id, at string) error {
	return x.AcknowledgeMany(ctx, recipient, []string{id}, at)
}
func (x *Index) AcknowledgeMany(ctx context.Context, recipient string, ids []string, at string) error {
	if err := x.notificationFault("ack_before"); err != nil {
		return err
	}
	tx, err := x.conn().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `UPDATE notifications SET acknowledged_at=COALESCE(acknowledged_at,?) WHERE id=? AND recipient=?`, at, id, recipient)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return apierr.New("unknown_notification", "no notification for recipient")
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return x.notificationFault("ack_after")
}

// Notification exists for recovery/fault assertions without exposing SQL.
func (x *Index) Notification(ctx context.Context, id string) (bool, error) {
	var n int
	err := x.conn().QueryRowContext(ctx, `SELECT 1 FROM notifications WHERE id=?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return n == 1, err
}

func (x *Index) notificationFault(stage string) error {
	if x.NotificationFault != nil {
		return x.NotificationFault(stage)
	}
	return nil
}
