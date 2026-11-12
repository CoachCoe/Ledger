package core

import (
	"context"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// issueCapability keeps session-key authentication root-owned while allowing
// existing sessions to receive additional root or delegated authority.
func (s *Service) issueCapability(ctx context.Context, c *invocation) (prepared, error) {
	in, err := decode[struct {
		SessionID          string   `json:"session_id"`
		ParentCapabilityID string   `json:"parent_capability_id"`
		Allow              []string `json:"allow"`
		Deny               []string `json:"deny"`
		MayAddDependencies bool     `json:"may_add_dependencies"`
		ExpiresAt          string   `json:"expires_at"`
	}](c.Input)
	if err != nil {
		return prepared{}, err
	}
	sub, err := s.subject(ctx, in.SessionID, "session")
	if err != nil {
		return prepared{}, err
	}
	if sub.State != "active" {
		return prepared{}, apierr.New("session_inactive", "recipient session ended")
	}
	for i := range in.Allow {
		in.Allow[i] = norm.NFC.String(in.Allow[i])
	}
	for i := range in.Deny {
		in.Deny[i] = norm.NFC.String(in.Deny[i])
	}
	grant := schema.Capability{ID: schema.NewID(schema.PrefixCapability), SessionID: in.SessionID, Allow: in.Allow, Deny: in.Deny, MayAddDependencies: in.MayAddDependencies, ExpiresAt: in.ExpiresAt, CreatedAt: now()}
	if c.Operation == "capability.issue" {
		if err := requireRoot(c); err != nil {
			return prepared{}, err
		}
		grant.Issuer = schema.Issuer{Kind: "root", Fingerprint: c.signer.ID()}
	} else {
		if c.SessionID == "" {
			return prepared{}, apierr.New("unauthorized", "delegation requires the parent holder")
		}
		if err := s.authority(ctx, c.SessionID, in.ParentCapabilityID); err != nil {
			return prepared{}, err
		}
		parent, err := creation[schema.Capability](ctx, s.Store.Index, in.ParentCapabilityID, schema.KindCapability)
		if err != nil {
			return prepared{}, err
		}
		grant.Issuer = schema.Issuer{Kind: "capability", CapabilityID: parent.ID}
		if err := capability.Narrow(parent, grant, s.caseSensitive); err != nil {
			return prepared{}, err
		}
	}
	if err := capability.Validate(grant, s.caseSensitive); err != nil {
		return prepared{}, err
	}
	expires, _ := schema.ParseTimestamp(grant.ExpiresAt)
	if !expires.After(time.Now()) {
		return prepared{}, apierr.New("capability_expired", "capability expired")
	}
	o, err := s.seal(schema.KindCapability, grant, c.signer)
	return prepared{result: map[string]string{"session_id": in.SessionID, "capability_id": grant.ID, "capability_hash": o.Hash}, records: []envelope.Object{o}}, err
}
