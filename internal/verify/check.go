package verify

import (
	"crypto/ed25519"
	"slices"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/selection"
)

// Integrity states. They are reported separately from trust.
const (
	Verified    = "verified"
	Invalid     = "invalid"
	Unavailable = "unavailable"
)

// Trust states.
const (
	Trusted   = "trusted"
	Untrusted = "untrusted"
	Unknown   = "unknown"
)

// stateOf maps a verification error to an integrity state. A missing
// dependency is unavailable, never invalid. A history that forks or has
// conflicting creations cannot be authenticated unambiguously; it is
// reported as a conflict and its authority as unavailable, never resolved.
func stateOf(err error) string {
	switch {
	case err == nil:
		return Verified
	case apierr.HasCode(err, "dependency_unavailable"), isConflictErr(err):
		return Unavailable
	}
	return Invalid
}

func isConflictErr(err error) bool {
	return apierr.HasCode(err, "conflicting_creation") || apierr.HasCode(err, "binding_fork") || apierr.HasCode(err, "conflicting_receipt")
}

// worst combines integrity states: invalid beats unavailable beats verified.
func worst(states ...string) string {
	out := Verified
	for _, s := range states {
		switch s {
		case Invalid:
			return Invalid
		case Unavailable:
			out = Unavailable
		}
	}
	return out
}

// EvidenceReport is the verification of one attestation. A failing check is
// an Evidence result, not an integrity failure.
type EvidenceReport struct {
	Hash        string   `json:"hash"`
	Integrity   string   `json:"integrity"`
	Result      string   `json:"result"`
	Category    string   `json:"category"`
	Criteria    []string `json:"criteria"`
	Independent bool     `json:"independent"`
	Detail      string   `json:"detail,omitempty"`
}

// SelectionReport is the verification of one selection decision, whose
// recorded statuses must equal a recomputation from its evidence.
type SelectionReport struct {
	Hash      string `json:"hash"`
	Integrity string `json:"integrity"`
	Detail    string `json:"detail,omitempty"`
}

// Conflict is a preserved semantic fork. Verification and import never
// choose a side.
type Conflict struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Records []string `json:"records,omitempty"`
}

// Independence reasons, equal to the indexed derivation.
const (
	reasonAuthor   = "signer_is_author"
	reasonHolder   = "signer_held_workspace"
	reasonUnknown  = "signer_unknown"
	reasonNotRoot  = "signer_not_root_authenticated"
	reasonVerifier = "signer_is_not_verifier_session"
)

// rawSignatures verifies the signature of every object against the key its
// signer's records name, whatever group the object is in. A signer whose
// key record is absent is unavailable; a signature that fails is invalid.
func (g *graph) rawSignatures() (problems []Problem, missing []string) {
	need := map[string]bool{}
	for _, h := range sortedKeys(g.objects) {
		o := g.objects[h]
		key, err := g.signerKey(o)
		if err != nil {
			if apierr.HasCode(err, "dependency_unavailable") {
				need[o.Envelope.Signer] = true
				continue
			}
			e := apierr.As(err)
			problems = append(problems, Problem{Hash: h, Code: e.Code, Error: e.Message})
			continue
		}
		if err := o.Verify(key); err != nil {
			e := apierr.As(err)
			problems = append(problems, Problem{Hash: h, Code: e.Code, Error: e.Message})
		}
	}
	for s := range need {
		missing = append(missing, "key:"+s)
	}
	slices.Sort(missing)
	return problems, missing
}

func (g *graph) signerKey(o envelope.Object) (ed25519.PublicKey, error) {
	id := o.Envelope.Signer
	if o.Envelope.Kind == schema.KindRootKey {
		root, err := decode[schema.RootKey](o)
		if err != nil {
			return nil, err
		}
		key, err := identity.DecodePublicKey(root.PublicKey)
		if err != nil || identity.Fingerprint(key) != id {
			return nil, apierr.New("unauthorized", "root key %s is not signed by its own fingerprint", o.Hash)
		}
		return key, nil
	}
	kind := schema.KindRootKey
	if strings.HasPrefix(id, schema.PrefixSession) {
		kind = schema.KindSession
	}
	var keys []string
	for _, e := range g.entities[id] {
		if e.Envelope.Kind != kind {
			continue
		}
		if kind == schema.KindRootKey {
			r, err := decode[schema.RootKey](e)
			if err != nil {
				return nil, err
			}
			keys = append(keys, r.PublicKey)
		} else {
			s, err := decode[schema.Session](e)
			if err != nil {
				return nil, err
			}
			keys = append(keys, s.PublicKey)
		}
	}
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	switch len(keys) {
	case 0:
		return nil, apierr.New("dependency_unavailable", "no key for signer %s", id)
	case 1:
		return identity.DecodePublicKey(keys[0])
	}
	return nil, apierr.New("conflicting_creation", "signer %s has conflicting keys", id)
}

// holders lists every session that ever held workspace id: its creation
// holder and every recorded binding.
func (g *graph) holders(id string) ([]string, error) {
	wobj, err := g.entity(id, schema.KindWorkspace)
	if err != nil {
		return nil, err
	}
	w, err := decode[schema.Workspace](wobj)
	if err != nil {
		return nil, err
	}
	out := []string{w.SessionID}
	for _, o := range g.objects {
		if o.Envelope.Kind != schema.KindWorkspaceBinding {
			continue
		}
		b, err := decode[schema.WorkspaceBinding](o)
		if err != nil {
			return nil, err
		}
		if b.WorkspaceID == id {
			out = append(out, b.SessionID)
		}
	}
	return out, nil
}

// facts derives what a reader derives about one attestation: its signer's
// kind and independence. Structural gaps fail; non-independence does not.
func (g *graph) facts(o envelope.Object) (selection.Evidence, schema.ChangeManifest, error) {
	att, err := decode[schema.EvidenceAttestation](o)
	if err != nil {
		return selection.Evidence{}, schema.ChangeManifest{}, err
	}
	mobj, ok := g.objects[att.Manifest]
	if !ok {
		return selection.Evidence{}, schema.ChangeManifest{}, apierr.New("dependency_unavailable", "missing manifest %s", att.Manifest)
	}
	if mobj.Envelope.Kind != schema.KindChangeManifest {
		return selection.Evidence{}, schema.ChangeManifest{}, apierr.New("invalid_attestation", "manifest reference %s names a %s", att.Manifest, mobj.Envelope.Kind)
	}
	m, err := decode[schema.ChangeManifest](mobj)
	if err != nil {
		return selection.Evidence{}, m, err
	}
	signer := o.Envelope.Signer
	e := selection.Evidence{Hash: o.Hash, Att: att, Signer: signer}
	sobj, err := g.entity(signer, schema.KindSession)
	if err != nil {
		return e, m, err
	}
	s, err := decode[schema.Session](sobj)
	if err != nil {
		return e, m, err
	}
	e.SignerKind = s.ParticipantKind
	holders, err := g.holders(m.WorkspaceID)
	if err != nil {
		return e, m, err
	}
	switch {
	case signer != att.VerifierSession:
		e.IndependenceReason = reasonVerifier
	case strings.HasPrefix(sobj.Envelope.Signer, schema.PrefixSession):
		e.IndependenceReason = reasonNotRoot
	case sobj.Envelope.Signer == "":
		e.IndependenceReason = reasonUnknown
	case signer == m.AuthorSession:
		e.IndependenceReason = reasonAuthor
	case slices.Contains(holders, signer):
		e.IndependenceReason = reasonHolder
	default:
		e.Independent = true
	}
	return e, m, nil
}

// evidence verifies one attestation. commit is the commit bound to the
// attested manifest by its retention ref ("" when unknown, and always for
// a v2 manifest, which binds evidence by tree alone).
func (g *graph) evidence(o envelope.Object, commit string) EvidenceReport {
	r := EvidenceReport{Hash: o.Hash, Integrity: Verified}
	att, err := decode[schema.EvidenceAttestation](o)
	if err != nil {
		r.Integrity, r.Detail = Invalid, err.Error()
		return r
	}
	r.Result, r.Category, r.Criteria = att.Result, att.Category, att.Criteria
	fail := func(err error) EvidenceReport {
		r.Integrity, r.Detail = stateOf(err), apierr.As(err).Message
		return r
	}
	e, m, err := g.facts(o)
	if err != nil {
		return fail(err)
	}
	r.Independent = e.Independent
	if err := g.signature(o); err != nil {
		return fail(err)
	}
	native := m.BaseTree != ""
	switch {
	case !e.Independent:
		return fail(apierr.New("author_signed_evidence", "attestation is not independent: %s", e.IndependenceReason))
	case native && att.Tree != m.Tree:
		return fail(apierr.New("cross_revision_evidence", "evidence ran on tree %s, not the manifest tree %s", att.Tree, m.Tree))
	case !native && att.ExecutedTree != m.Tree:
		return fail(apierr.New("tree_mismatch", "executed tree %s differs from manifest tree %s", att.ExecutedTree, m.Tree))
	case commit != "" && att.Commit != commit:
		return fail(apierr.New("binding_mismatch", "attested commit %s differs from the manifest commit %s", att.Commit, commit))
	case selection.Consistent(att) != "":
		return fail(apierr.New("inconsistent_result", "recorded result contradicts its phases"))
	case att.Isolation != "none":
		return fail(apierr.New("isolation_not_none", "unknown isolation %q", att.Isolation))
	}
	def, ok := g.objects[att.CheckDefinition]
	if !ok {
		return fail(apierr.New("dependency_unavailable", "missing check definition %s", att.CheckDefinition))
	}
	if def.Envelope.Kind != schema.KindCheckDefinition {
		return fail(apierr.New("invalid_attestation", "check definition reference %s names a %s", att.CheckDefinition, def.Envelope.Kind))
	}
	d, err := decode[schema.CheckDefinition](def)
	if err != nil {
		return fail(err)
	}
	if d.Category != att.Category {
		return fail(apierr.New("invalid_attestation", "attestation category differs from its check definition"))
	}
	iobj, err := g.entity(m.IntentID, schema.KindIntent)
	if err != nil {
		return fail(err)
	}
	intent, err := decode[schema.Intent](iobj)
	if err != nil {
		return fail(err)
	}
	for _, id := range att.Criteria {
		i := slices.IndexFunc(intent.Criteria, func(c schema.Criterion) bool { return c.ID == id })
		if i < 0 {
			return fail(apierr.New("invalid_attestation", "intent has no criterion %q", id))
		}
		v := intent.Criteria[i].Verification
		if att.Category == "judgment" {
			if v.Kind != "judgment" || d.Name != "judgment" {
				return fail(apierr.New("invalid_attestation", "judgment must reference a judgment criterion and definition"))
			}
		} else if v.Kind != "check" || v.DefinitionHash != att.CheckDefinition {
			return fail(apierr.New("invalid_attestation", "criterion %q is not pinned to the attested check definition", id))
		}
	}
	return r
}

// selection verifies one decision by recomputing its statuses.
func (g *graph) selection(o envelope.Object, commit string) SelectionReport {
	r := SelectionReport{Hash: o.Hash, Integrity: Verified}
	fail := func(err error) SelectionReport {
		r.Integrity, r.Detail = stateOf(err), apierr.As(err).Message
		return r
	}
	d, err := decode[schema.SelectionDecision](o)
	if err != nil {
		return fail(err)
	}
	if err := g.signature(o); err != nil {
		return fail(err)
	}
	if d.Selector.Kind != "speculation_run" {
		wantKind := "root"
		if strings.HasPrefix(o.Envelope.Signer, schema.PrefixSession) {
			wantKind = "session"
		}
		if d.Selector.Kind != wantKind || d.Selector.ID != o.Envelope.Signer {
			return fail(apierr.New("unauthorized", "the selector is not the signer"))
		}
	}
	mobj, ok := g.objects[d.Manifest]
	if !ok {
		return fail(apierr.New("dependency_unavailable", "missing manifest %s", d.Manifest))
	}
	if mobj.Envelope.Kind != schema.KindChangeManifest {
		return fail(apierr.New("invalid_selection", "manifest reference %s names a %s", d.Manifest, mobj.Envelope.Kind))
	}
	m, err := decode[schema.ChangeManifest](mobj)
	if err != nil {
		return fail(err)
	}
	if o.Envelope.Signer == m.AuthorSession {
		return fail(apierr.New("unauthorized", "the author cannot select its own manifest"))
	}
	if d.IntentID != m.IntentID {
		return fail(apierr.New("invalid_selection", "the selected intent differs from the manifest's intent"))
	}
	native := m.BaseTree != ""
	if !native && commit == "" {
		return fail(apierr.New("dependency_unavailable", "the selected manifest's commit is unavailable"))
	}
	if d.Commit != commit {
		return fail(apierr.New("binding_mismatch", "the selected commit differs from the manifest's retained commit"))
	}
	for i, h := range d.Competitors {
		if h == d.Manifest || (i > 0 && h <= d.Competitors[i-1]) {
			return fail(apierr.New("invalid_selection", "competitors must be sorted, unique and exclude the selected manifest"))
		}
		other, ok := g.objects[h]
		if !ok {
			return fail(apierr.New("dependency_unavailable", "missing competitor manifest %s", h))
		}
		if other.Envelope.Kind != schema.KindChangeManifest {
			return fail(apierr.New("invalid_selection", "competitor reference %s names a %s", h, other.Envelope.Kind))
		}
		cm, err := decode[schema.ChangeManifest](other)
		if err != nil {
			return fail(err)
		}
		if cm.IntentID != m.IntentID {
			return fail(apierr.New("invalid_selection", "competitor %s belongs to a different intent", h))
		}
	}
	if !slices.IsSorted(d.Evidence) || len(slices.Compact(slices.Clone(d.Evidence))) != len(d.Evidence) {
		return fail(apierr.New("invalid_selection", "evidence hashes must be sorted and unique"))
	}
	iobj, err := g.entity(m.IntentID, schema.KindIntent)
	if err != nil {
		return fail(err)
	}
	intent, err := decode[schema.Intent](iobj)
	if err != nil {
		return fail(err)
	}
	var set, known []selection.Evidence
	// Contrary evidence is a snapshot of what was recorded when this choice
	// was made. Later runs must remain visible without rewriting that history.
	recordedAt := map[string]time.Time{}
	for _, ro := range g.objects {
		if ro.Envelope.Kind != schema.KindOperationReceipt {
			continue
		}
		rc, err := decode[schema.OperationReceipt](ro)
		if err != nil {
			return fail(err)
		}
		at, err := schema.ParseTimestamp(rc.At)
		if err != nil {
			return fail(err)
		}
		for _, member := range rc.Members {
			if prev, ok := recordedAt[member]; !ok || at.Before(prev) {
				recordedAt[member] = at
			}
		}
	}
	selectedAt, recorded := recordedAt[o.Hash]
	if !recorded {
		return fail(apierr.New("dependency_unavailable", "missing selection receipt"))
	}
	for _, h := range sortedKeys(g.objects) {
		eo := g.objects[h]
		if eo.Envelope.Kind != schema.KindEvidenceAttestation {
			continue
		}
		e, _, err := g.facts(eo)
		if err != nil {
			if slices.Contains(d.Evidence, h) {
				return fail(err)
			}
			continue
		}
		at, recorded := recordedAt[h]
		if e.Att.Manifest == d.Manifest && recorded && !at.After(selectedAt) {
			known = append(known, e)
		}
		if slices.Contains(d.Evidence, h) {
			set = append(set, e)
		}
	}
	for _, h := range d.Evidence {
		if !slices.ContainsFunc(set, func(e selection.Evidence) bool { return e.Hash == h }) {
			if object, ok := g.objects[h]; ok && object.Envelope.Kind != schema.KindEvidenceAttestation {
				return fail(apierr.New("invalid_selection", "evidence reference %s names a %s", h, object.Envelope.Kind))
			}
			return fail(apierr.New("dependency_unavailable", "missing evidence %s", h))
		}
	}
	b := selection.Binding{Manifest: d.Manifest, Commit: d.Commit, Tree: m.Tree, Criteria: intent.Criteria}
	for _, e := range set {
		if e.Att.Manifest != b.Manifest || e.Att.Commit != b.Commit || e.Att.ExecutedOn() != b.Tree {
			return fail(apierr.New("cross_revision_evidence", "evidence %s belongs to another revision", e.Hash))
		}
	}
	res := selection.Evaluate(b, set, known)
	if !selection.SameStatuses(res.Criteria, d.Criteria) || !slices.Equal(res.Contrary, d.ContraryEvidence) {
		return fail(apierr.New("status_mismatch", "recorded criteria differ from a recomputation of the evidence"))
	}
	if d.Selector.Kind == "speculation_run" {
		if err := g.runSelection(o, d, m, intent); err != nil {
			return fail(err)
		}
	}
	return r
}

// conflicts finds semantic forks among committed records. They are reported,
// never resolved, and no clock decides between sides.
func conflicts(c *Corpus) []Conflict {
	var out []Conflict
	creations := map[string][]string{}
	events := map[string][]lifecycle.Event{}
	subjectKind := map[string]string{}
	ops := map[string][]string{}
	bindings := map[string][]string{}
	for _, h := range sortedKeys(c.Objects) {
		if !c.Committed[h] {
			continue
		}
		o := c.Objects[h]
		switch o.Envelope.Kind {
		case schema.KindSession, schema.KindCapability, schema.KindWorkspace, schema.KindIntent, schema.KindSpeculationRun:
			var p struct {
				ID string `json:"id"`
			}
			if err := decodeInto(o, &p); err == nil {
				creations[p.ID] = append(creations[p.ID], h)
			}
		case schema.KindOperationReceipt:
			if r, err := decode[schema.OperationReceipt](o); err == nil {
				ops[r.OperationID] = append(ops[r.OperationID], h)
			}
		case schema.KindWorkspaceBinding:
			if b, err := decode[schema.WorkspaceBinding](o); err == nil {
				k := b.WorkspaceID + " " + b.Previous
				bindings[k] = append(bindings[k], h)
			}
		case schema.KindLifecycleEvent:
			e, err := decode[schema.LifecycleEvent](o)
			if err != nil || e.Actor != o.Envelope.Signer {
				continue
			}
			k := e.SubjectID + " " + e.Creation
			events[k] = append(events[k], lifecycle.Event{Hash: h, LifecycleEvent: e})
			subjectKind[k] = e.SubjectKind
		}
	}
	for _, id := range sortedKeys(creations) {
		if len(creations[id]) > 1 {
			out = append(out, Conflict{ID: id, Kind: "conflicting_creation", Records: creations[id]})
		}
	}
	for _, id := range sortedKeys(ops) {
		if len(ops[id]) > 1 {
			out = append(out, Conflict{ID: id, Kind: "operation_conflict", Records: ops[id]})
		}
	}
	for _, k := range sortedKeys(bindings) {
		if len(bindings[k]) > 1 {
			id, _, _ := strings.Cut(k, " ")
			out = append(out, Conflict{ID: id, Kind: "binding_fork", Records: bindings[k]})
		}
	}
	for _, k := range sortedKeys(events) {
		if st := lifecycle.Derive(subjectKind[k], events[k]); len(st.Fork) > 0 {
			id, _, _ := strings.Cut(k, " ")
			out = append(out, Conflict{ID: id, Kind: "lifecycle_fork", Records: st.Fork})
		}
	}
	return out
}
