package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"

	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
)

// Independence reasons. They are API values.
const (
	NotIndependentAuthor      = "signer_is_author"
	NotIndependentHolder      = "signer_held_workspace"
	NotIndependentUnknown     = "signer_unknown"
	NotIndependentNotRoot     = "signer_not_root_authenticated"
	NotIndependentSignerMatch = "signer_is_not_verifier_session"
)

// EvidenceFor returns every committed attestation recorded for manifest,
// ordered by hash, with the facts readers derive about each signer. Only
// objects of indexed groups exist in the index, so uncommitted attestations
// never appear. The lookup uses the objects_evidence_manifest index.
func (x *Index) EvidenceFor(ctx context.Context, manifest string) ([]selection.Evidence, error) {
	return evidenceFor(ctx, x.conn(), manifest)
}

func evidenceFor(ctx context.Context, q querier, manifest string) ([]selection.Evidence, error) {
	rows, err := q.QueryContext(ctx, `SELECT hash, signer, payload, version FROM objects WHERE kind='evidence_attestation' AND json_extract(payload,'$.manifest')=? ORDER BY hash`, manifest)
	if err != nil {
		return nil, err
	}
	raws, err := scanEvidence(rows)
	if err != nil {
		return nil, err
	}
	return derive(ctx, q, raws)
}

// EvidenceByHash returns the committed attestations among hashes. Hashes
// that name no committed attestation are omitted; callers decide whether
// that is an error.
func (x *Index) EvidenceByHash(ctx context.Context, hashes []string) ([]selection.Evidence, error) {
	return evidenceByHash(ctx, x.conn(), hashes)
}

func evidenceByHash(ctx context.Context, q querier, hashes []string) ([]selection.Evidence, error) {
	var raws []rawEvidence
	for _, h := range hashes {
		rows, err := q.QueryContext(ctx, `SELECT hash, signer, payload, version FROM objects WHERE hash=? AND kind='evidence_attestation'`, h)
		if err != nil {
			return nil, err
		}
		got, err := scanEvidence(rows)
		if err != nil {
			return nil, err
		}
		raws = append(raws, got...)
	}
	return derive(ctx, q, raws)
}

type rawEvidence struct {
	hash, signer string
	att          schema.EvidenceAttestation
}

func scanEvidence(rows *sql.Rows) ([]rawEvidence, error) {
	defer rows.Close()
	var out []rawEvidence
	for rows.Next() {
		var r rawEvidence
		var payload string
		var version int
		if err := rows.Scan(&r.hash, &r.signer, &payload, &version); err != nil {
			return nil, err
		}
		if err := schema.Decode(version, schema.KindEvidenceAttestation, []byte(payload), &r.att); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// derive computes independence for each attestation: its signer must be the
// attested verifier session, a session created by a root key, and neither the
// manifest's author nor ever a holder of its workspace. Independence says
// nothing about whether the evidence is correct.
func derive(ctx context.Context, q querier, raws []rawEvidence) ([]selection.Evidence, error) {
	type parties struct {
		author  string
		holders []string
		err     error
	}
	cache := map[string]*parties{}
	out := make([]selection.Evidence, 0, len(raws))
	for _, r := range raws {
		p := cache[r.att.Manifest]
		if p == nil {
			p = &parties{}
			m, err := manifestIn(ctx, q, r.att.Manifest)
			if err == nil {
				p.author = m.AuthorSession
				p.holders, err = workspaceHolders(ctx, q, m.WorkspaceID)
			}
			p.err = err
			cache[r.att.Manifest] = p
		}
		if p.err != nil {
			return nil, p.err
		}
		e := selection.Evidence{Hash: r.hash, Att: r.att, Signer: r.signer}
		kind, creator, err := sessionFacts(ctx, q, r.signer)
		switch {
		case err != nil:
			return nil, err
		case r.signer != r.att.VerifierSession:
			e.IndependenceReason = NotIndependentSignerMatch
		case creator == "":
			e.IndependenceReason = NotIndependentUnknown
		case isSession(creator):
			e.IndependenceReason = NotIndependentNotRoot
		case r.signer == p.author:
			e.IndependenceReason = NotIndependentAuthor
		case slices.Contains(p.holders, r.signer):
			e.IndependenceReason = NotIndependentHolder
		default:
			e.SignerKind, e.Independent = kind, true
		}
		if creator != "" {
			e.SignerKind = kind
		}
		out = append(out, e)
	}
	return out, nil
}

// sessionFacts returns a session's participant kind and the signer of its
// creation record ("" when no such session is indexed).
func sessionFacts(ctx context.Context, q querier, id string) (kind, creator string, err error) {
	var payload string
	err = q.QueryRowContext(ctx, `SELECT signer, payload FROM creations WHERE entity_id=? AND kind='session'`, id).Scan(&creator, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var s schema.Session
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		return "", "", err
	}
	return s.ParticipantKind, creator, nil
}
