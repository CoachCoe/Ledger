package index

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Validate applies the rules for publishing a new local group: everything
// Verify checks, plus entity uniqueness, lifecycle predecessors, and actor
// authorization against current indexed state (ADRs 0008, 0011). It must
// run under the RecordStore lock so that state cannot change before the
// group is published.
func (x *Index) Validate(ctx context.Context, g Group) error {
	if err := verify(ctx, x.conn(), g, x.caseSensitive()); err != nil {
		return err
	}
	var r schema.OperationReceipt
	if err := g.Receipt.Decode(&r); err != nil {
		return err
	}
	ops, err := x.Operations(ctx, r.OperationID)
	if err != nil {
		return err
	}
	if len(ops) > 0 {
		return apierr.New("operation_exists", "operation %s is already committed", r.OperationID)
	}
	v := &validator{ctx: ctx, q: x.conn(), local: map[string]envelope.Object{}, objects: map[string]envelope.Object{}, sims: map[string]*Subject{}, at: r.At, sensitive: x.caseSensitive()}
	for _, o := range g.Members {
		v.objects[o.Hash] = o
	}
	if err := v.signerActive(g.Receipt.Envelope.Signer); err != nil {
		return err
	}
	for _, o := range g.Members {
		if err := v.check(o); err != nil {
			return err
		}
	}
	return nil
}

type validator struct {
	sensitive bool
	at        string
	ctx       context.Context
	q         querier
	local     map[string]envelope.Object // creations in this group, by entity id
	objects   map[string]envelope.Object // members of this group, by hash
	sims      map[string]*Subject        // subjects as changed by this group
}

func (v *validator) check(o envelope.Object) error {
	signer := o.Envelope.Signer
	if err := v.signerActive(signer); err != nil {
		return err
	}
	if id, err := entityID(o); err != nil {
		return err
	} else if id != "" {
		if err := v.unique(id); err != nil {
			return err
		}
		v.local[id] = o
	}
	switch o.Envelope.Kind {
	case schema.KindIntent:
		var p schema.Intent
		if err := o.Decode(&p); err != nil {
			return err
		}
		return v.checkIntent(&p, signer)
	case schema.KindCapability:
		var p schema.Capability
		if err := o.Decode(&p); err != nil {
			return err
		}
		return v.checkCapability(&p, signer)
	case schema.KindSession:
		var p schema.Session
		if err := o.Decode(&p); err != nil {
			return err
		}
		c, err := v.capability(p.CapabilityID)
		if err != nil {
			return err
		}
		if c.SessionID != p.ID || c.Issuer.Kind != "root" || c.Issuer.Fingerprint != signer {
			return unauthorized("capability %s was not issued by %s for session %s", p.CapabilityID, signer, p.ID)
		}
	case schema.KindWorkspace:
		var p schema.Workspace
		if err := o.Decode(&p); err != nil {
			return err
		}
		return v.checkWorkspace(&p, signer)
	case schema.KindChangeManifest:
		var p schema.ChangeManifest
		if err := o.Decode(&p); err != nil {
			return err
		}
		ws, err := v.subjectIn(p.WorkspaceID, lifecycle.Workspace, "open")
		if err != nil {
			return err
		}
		if signer != p.AuthorSession || ws.Owner != signer {
			return unauthorized("manifest author is not the workspace holder")
		}
		var w schema.Workspace
		if err := v.object(ws.Creation, schema.KindWorkspace, &w); err != nil {
			return err
		}
		if w.BaseCommit != p.BaseCommit || w.ChangeID != p.ChangeID || w.IntentID != p.IntentID {
			return unauthorized("manifest workspace binding differs")
		}
		_, err = v.subjectIn(p.IntentID, lifecycle.Intent, "open")
		return err
	case schema.KindEvidenceAttestation:
		var p schema.EvidenceAttestation
		if err := o.Decode(&p); err != nil {
			return err
		}
		return v.checkAttestation(&p, signer)
	case schema.KindSpeculationRun:
		var p schema.SpeculationRun
		if err := o.Decode(&p); err != nil {
			return err
		}
		_, err := v.subjectIn(p.IntentID, lifecycle.Intent, "open")
		return err
	case schema.KindLifecycleEvent:
		var e schema.LifecycleEvent
		if err := o.Decode(&e); err != nil {
			return err
		}
		return v.checkEvent(o.Hash, &e, signer)
	case schema.KindWorkspaceBinding:
		var b schema.WorkspaceBinding
		if err := o.Decode(&b); err != nil {
			return err
		}
		return v.checkBinding(o.Hash, &b, signer)
	}
	return nil
}

func unauthorized(format string, args ...any) error {
	return apierr.New("unauthorized", format, args...)
}

// signerActive requires session signers to be active, unconflicted
// sessions. Root signers were authenticated by Verify.
func (v *validator) signerActive(signer string) error {
	if !isSession(signer) {
		return nil
	}
	s, err := v.subject(signer)
	if err != nil {
		return err
	}
	if s == nil || s.Kind != lifecycle.Session {
		return unauthorized("unknown session %s", signer)
	}
	if s.Conflict != "" {
		return apierr.New("subject_conflict", "session %s is in conflict (%s)", signer, s.Conflict)
	}
	if s.State != "active" {
		return apierr.New("session_inactive", "session %s is %s", signer, s.State)
	}

	var session schema.Session
	if o, ok := v.local[signer]; ok {
		if err := o.Decode(&session); err != nil {
			return err
		}
	} else {
		_, raw, err := creation(v.ctx, v.q, signer)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &session); err != nil {
			return err
		}
	}
	c, err := v.capability(session.CapabilityID)
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, v.at)
	if err != nil {
		return err
	}
	started, err := schema.ParseTimestamp(session.StartedAt)
	if err != nil {
		return err
	}
	if at.Before(started) {
		return apierr.New("session_inactive", "operation precedes session start")
	}
	_, err = capability.Chain(c.ID, func(id string) (schema.Capability, error) {
		p, e := v.capability(id)
		if e != nil {
			return schema.Capability{}, e
		}
		return *p, nil
	}, v.sensitive, at)
	if err != nil {
		return err
	}
	return nil
}

func (v *validator) unique(id string) error {
	if _, ok := v.local[id]; ok {
		return apierr.New("entity_exists", "%s is created twice in one operation", id)
	}
	var n int
	if err := v.q.QueryRowContext(v.ctx, `SELECT COUNT(*) FROM creations WHERE entity_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return apierr.New("entity_exists", "%s already exists", id)
	}
	return nil
}

// capability returns the unique creation record of capability id from
// this group or the index.
func (v *validator) capability(id string) (*schema.Capability, error) {
	var c schema.Capability
	if o, ok := v.local[id]; ok {
		if o.Envelope.Kind != schema.KindCapability {
			return nil, apierr.New("unknown_entity", "%s is not a capability", id)
		}
		return &c, o.Decode(&c)
	}
	k, payload, err := creation(v.ctx, v.q, id)
	if err != nil {
		return nil, err
	}
	if k == "" {
		return nil, apierr.New(CodeUnavailable, "missing capability %s", id)
	}
	if k != schema.KindCapability {
		return nil, apierr.New("unknown_entity", "no capability %s", id)
	}
	return &c, json.Unmarshal(payload, &c)
}

// subject returns the current state of id, including changes made earlier
// in this group, or nil if it does not exist.
func (v *validator) subject(id string) (*Subject, error) {
	if s, ok := v.sims[id]; ok {
		return s, nil
	}
	var s *Subject
	if o, ok := v.local[id]; ok {
		kind := lifecycle.SubjectKind(o.Envelope.Kind)
		if kind == "" {
			return nil, nil
		}
		s = &Subject{ID: id, Kind: kind, Creation: o.Hash, State: lifecycle.Initial(kind), BindingHead: o.Hash}
		switch kind {
		case lifecycle.Intent:
			var p schema.Intent
			if err := o.Decode(&p); err != nil {
				return nil, err
			}
			s.Owner = p.Owner
		case lifecycle.Session:
			s.Owner = id
		case lifecycle.Workspace:
			var p schema.Workspace
			if err := o.Decode(&p); err != nil {
				return nil, err
			}
			s.Owner = p.SessionID
		case lifecycle.SpeculationRun:
			s.Owner = o.Envelope.Signer
		}
	} else {
		var err error
		if s, err = loadSubject(v.ctx, v.q, id); err != nil || s == nil {
			return nil, err
		}
	}
	v.sims[id] = s
	return s, nil
}

// subjectIn returns subject id, which must be of kind, unconflicted, and
// in one of the given states.
func (v *validator) subjectIn(id, kind string, states ...string) (*Subject, error) {
	s, err := v.subject(id)
	if err != nil {
		return nil, err
	}
	if s == nil || s.Kind != kind {
		return nil, apierr.New("unknown_entity", "no %s %s", kind, id)
	}
	if s.Conflict != "" {
		return nil, apierr.New("subject_conflict", "%s %s is in conflict (%s)", kind, id, s.Conflict)
	}
	for _, st := range states {
		if s.State == st {
			return s, nil
		}
	}
	return nil, apierr.New("invalid_state", "%s %s is %s", kind, id, s.State)
}

func (v *validator) checkIntent(p *schema.Intent, signer string) error {
	if p.Owner != signer {
		return unauthorized("intent owner %s must be its signer %s", p.Owner, signer)
	}
	if p.ParentID != nil {
		if *p.ParentID == p.ID {
			return apierr.New("invalid_intent", "an intent cannot parent itself")
		}
		if _, err := v.subjectIn(*p.ParentID, lifecycle.Intent, "open"); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, c := range p.Criteria {
		if seen[c.ID] {
			return apierr.New("invalid_intent", "criterion %q appears twice", c.ID)
		}
		seen[c.ID] = true
		if c.Verification.Kind != "check" {
			continue
		}
		var d schema.CheckDefinition
		if err := v.object(c.Verification.DefinitionHash, schema.KindCheckDefinition, &d); err != nil {
			return err
		}
		if d.Name != c.Verification.Check {
			return apierr.New("invalid_intent", "criterion %q pins check %q but names %q", c.ID, d.Name, c.Verification.Check)
		}
	}
	return nil
}

// object decodes a non-entity record by hash from this group or the index.
func (v *validator) object(hash string, kind schema.Kind, out any) error {
	if o, ok := v.objects[hash]; ok && o.Envelope.Kind == kind {
		return o.Decode(out)
	}
	var payload string
	err := v.q.QueryRowContext(v.ctx, `SELECT payload FROM objects WHERE hash = ? AND kind = ?`, hash, kind).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return apierr.New("unknown_entity", "no %s %s", kind, hash)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(payload), out)
}

func (v *validator) checkCapability(p *schema.Capability, signer string) error {
	if err := capability.Validate(*p, v.sensitive); err != nil {
		return err
	}
	if p.Issuer.Kind == "root" {
		if p.Issuer.Fingerprint != signer {
			return unauthorized("root capability %s must be signed by its issuer", p.ID)
		}
		return nil
	}
	parent, err := v.capability(p.Issuer.CapabilityID)
	if err != nil {
		return err
	}
	if err := capability.Narrow(*parent, *p, v.sensitive); err != nil {
		return err
	}
	if parent.SessionID != signer {
		return unauthorized("only the holder of %s may delegate it", p.Issuer.CapabilityID)
	}
	return nil
}

func (v *validator) checkWorkspace(p *schema.Workspace, signer string) error {
	if isSession(signer) && signer != p.SessionID {
		return unauthorized("session %s cannot create a workspace for %s", signer, p.SessionID)
	}
	if _, err := v.subjectIn(p.IntentID, lifecycle.Intent, "open"); err != nil {
		return err
	}
	if _, err := v.subjectIn(p.SessionID, lifecycle.Session, "active"); err != nil {
		return err
	}
	c, err := v.capability(p.CapabilityID)
	if err != nil {
		return err
	}
	if c.SessionID != p.SessionID {
		return unauthorized("capability %s is not held by %s", p.CapabilityID, p.SessionID)
	}
	return nil
}

func (v *validator) checkEvent(hash string, e *schema.LifecycleEvent, signer string) error {
	if e.Actor != signer {
		return unauthorized("event actor %s is not its signer %s", e.Actor, signer)
	}
	s, err := v.subject(e.SubjectID)
	if err != nil {
		return err
	}
	if s == nil || s.Kind != e.SubjectKind || s.Creation != e.Creation {
		return apierr.New("unknown_entity", "no %s %s created by %s", e.SubjectKind, e.SubjectID, e.Creation)
	}
	if s.Conflict != "" {
		return apierr.New("subject_conflict", "%s is in conflict (%s)", e.SubjectID, s.Conflict)
	}
	prev := ""
	if e.Previous != nil {
		prev = *e.Previous
	}
	if prev != s.Head {
		return apierr.New("stale_predecessor", "%s's latest event is %q, not %q", e.SubjectID, s.Head, prev).With("head", s.Head)
	}
	if e.ReplacementID != nil {
		if *e.ReplacementID == e.SubjectID {
			return apierr.New("invalid_transition", "an intent cannot supersede itself")
		}
		if _, err := v.subjectIn(*e.ReplacementID, lifecycle.Intent, "open", "closed"); err != nil {
			return err
		}
	}
	if err := lifecycle.CheckTransition(e, s.State, s.Resume); err != nil {
		return err
	}
	if isSession(signer) && s.Owner != signer {
		return unauthorized("%s does not own %s", signer, e.SubjectID)
	}
	if e.ToState == "interrupted" {
		s.Resume = s.State
	}
	s.State, s.Head = e.ToState, hash
	return nil
}

func (v *validator) checkBinding(hash string, b *schema.WorkspaceBinding, signer string) error {
	if isSession(signer) {
		return unauthorized("only the root may rebind a workspace")
	}
	if b.Actor != signer {
		return unauthorized("binding actor %s is not its signer %s", b.Actor, signer)
	}
	s, err := v.subjectIn(b.WorkspaceID, lifecycle.Workspace, "open", "proposed")
	if err != nil {
		return err
	}
	if b.Previous != s.BindingHead {
		return apierr.New("stale_predecessor", "%s's latest binding is %s, not %s", b.WorkspaceID, s.BindingHead, b.Previous).
			With("head", s.BindingHead)
	}
	if _, err := v.subjectIn(b.SessionID, lifecycle.Session, "active"); err != nil {
		return err
	}
	c, err := v.capability(b.CapabilityID)
	if err != nil {
		return err
	}
	if c.SessionID != b.SessionID {
		return unauthorized("capability %s is not held by %s", b.CapabilityID, b.SessionID)
	}
	s.BindingHead, s.Owner = hash, b.SessionID
	return nil
}

// checkAttestation enforces what a published attestation must satisfy
// regardless of who asks: an active verifier session that is neither the
// manifest author nor ever a holder of its workspace, the exact bound
// commit and tree, a check definition and criteria the intent pinned, and a
// result consistent with the recorded phases. Readers re-derive all of this.
func (v *validator) checkAttestation(p *schema.EvidenceAttestation, signer string) error {
	if !isSession(signer) || p.VerifierSession != signer {
		return unauthorized("an attestation must be signed by its verifier session")
	}
	var m schema.ChangeManifest
	if err := v.object(p.Manifest, schema.KindChangeManifest, &m); err != nil {
		return apierr.New("unknown_manifest", "no change manifest %s", p.Manifest)
	}
	holders, err := workspaceHolders(v.ctx, v.q, m.WorkspaceID)
	if err != nil {
		return err
	}
	if signer == m.AuthorSession || slices.Contains(holders, signer) {
		return apierr.New("author_signed_evidence", "session %s authored or held the workspace of manifest %s", signer, p.Manifest)
	}
	commit, err := manifestCommit(v.ctx, v.q, p.Manifest)
	if err != nil {
		return err
	}
	if commit == "" || commit != p.Commit {
		return apierr.New("binding_mismatch", "commit %s is not the commit bound to manifest %s", p.Commit, p.Manifest)
	}
	if p.ExecutedTree != m.Tree {
		return apierr.New("tree_mismatch", "executed tree %s is not the manifest tree %s", p.ExecutedTree, m.Tree)
	}
	var d schema.CheckDefinition
	if err := v.object(p.CheckDefinition, schema.KindCheckDefinition, &d); err != nil {
		return err
	}
	if d.Category != p.Category {
		return apierr.New("invalid_attestation", "category %s differs from the pinned definition's %s", p.Category, d.Category)
	}
	kind, raw, err := creation(v.ctx, v.q, m.IntentID)
	if err != nil {
		return err
	}
	if kind != schema.KindIntent {
		return apierr.New("unknown_entity", "no intent %s", m.IntentID)
	}
	var intent schema.Intent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return err
	}
	for _, id := range p.Criteria {
		i := slices.IndexFunc(intent.Criteria, func(c schema.Criterion) bool { return c.ID == id })
		if i < 0 {
			return apierr.New("invalid_attestation", "intent %s has no criterion %q", m.IntentID, id)
		}
		ver := intent.Criteria[i].Verification
		switch {
		case p.Category == "judgment" && ver.Kind != "judgment":
			return apierr.New("invalid_attestation", "criterion %q is not a judgment criterion", id)
		case p.Category != "judgment" && (ver.Kind != "check" || ver.DefinitionHash != p.CheckDefinition):
			return apierr.New("invalid_attestation", "criterion %q is not pinned to check definition %s", id, p.CheckDefinition)
		}
	}
	if p.Category == "judgment" {
		// A judgment runs nothing: no phases, one criterion, and the
		// definition record it references is the judgment definition.
		if p.Setup != nil || p.Check != nil || len(p.Criteria) != 1 || d.Name != "judgment" {
			return apierr.New("invalid_attestation", "a judgment names exactly one criterion, records no process phases and references its judgment definition")
		}
		return nil
	}
	if (p.Result == "pass") != (p.Reason == nil) {
		return apierr.New("inconsistent_result", "a pass has no reason and a failure has one")
	}
	if p.Result == "pass" && !passingPhases(p) {
		return apierr.New("inconsistent_result", "result pass contradicts the recorded setup and check phases")
	}
	return nil
}

// passingPhases reports whether setup (when present) and check both exited
// 0 without timing out.
func passingPhases(p *schema.EvidenceAttestation) bool {
	ok := func(ph *schema.Phase) bool {
		return ph != nil && ph.ExitStatus != nil && *ph.ExitStatus == 0 && !ph.TimedOut
	}
	return (p.Setup == nil || ok(p.Setup)) && ok(p.Check)
}
