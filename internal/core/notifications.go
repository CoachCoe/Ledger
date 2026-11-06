package core

import (
	"context"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

func (s *Service) notifications(ctx context.Context, r api.Request) (any, error) {
	in, err := decode[struct {
		SessionID      string `json:"session_id"`
		Since          string `json:"since"`
		NotificationID string `json:"notification_id"`
	}](r.Input)
	if err != nil {
		return nil, err
	}
	if r.SessionID != "" && r.SessionID != in.SessionID {
		return nil, apierr.New("unauthorized", "queue belongs to another session")
	}
	if r.SessionID == "" {
		if _, err := s.root(); err != nil {
			return nil, err
		}
	}
	if _, err := s.subject(ctx, in.SessionID, "session"); err != nil {
		return nil, err
	}
	if r.Operation == "coordination.ack" {
		err := s.Store.Index.Acknowledge(ctx, in.SessionID, in.NotificationID, now())
		return map[string]bool{"acknowledged": err == nil}, err
	}
	n, err := s.Store.Index.Notifications(ctx, in.SessionID, in.Since)
	return map[string]any{"notifications": n}, err
}

func (s *Service) warnScope(ctx context.Context, w schema.Workspace, snap workspace.Snapshot) error {
	sub, _, cid, err := s.boundWorkspace(ctx, w.ID, "")
	if err != nil {
		return err
	}
	at := time.Now()
	chain, chainErr := s.chain(ctx, cid, at)
	var bad map[string]error
	if chainErr != nil {
		bad = map[string]error{}
		for _, p := range snap.Files {
			bad[p] = chainErr
		}
	} else {
		bad, err = workspace.Violations(ctx, s.Provider.Path(w.ID), w.BaseCommit, snap, chain, s.caseSensitive, at)
		if err != nil {
			return err
		}
	}
	for p := range bad {
		// Stable per binding/path/reason, including acknowledged warnings. Restart
		// never floods the recipient; a new binding gets its own attribution.
		id := "ntf_" + canon.Hash([]byte(sub.BindingHead + ":" + p + ":" + apierr.As(bad[p]).Code))[:26]
		kind := "scope_violation"
		if chainErr != nil && apierr.As(chainErr).Code == "capability_expired" {
			kind = "expiry"
		}
		n := index.Notification{ID: id, Recipient: sub.Owner, Kind: kind, Subject: index.NotificationSubject{Path: p}, Counterpart: &index.NotificationCounterpart{WorkspaceID: w.ID, SessionID: sub.Owner, IntentID: w.IntentID}, Confidence: "file_level", CreatedAt: schema.Timestamp(at)}
		if err := s.Store.Index.Enqueue(ctx, n); err != nil {
			return err
		}
	}
	return nil
}
