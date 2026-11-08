// Package verify checks M2 signed manifest authority from immutable records
// without SQLite. M3 adds bundle inventories, Git-object closure and trust UI.
package verify

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"slices"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

type Result struct {
	Integrity string        `json:"integrity"`
	Trust     string        `json:"trust"`
	Root      string        `json:"root"`
	Error     *apierr.Error `json:"error,omitempty"`
}

type graph struct {
	objects   map[string]envelope.Object
	entities  map[string][]envelope.Object
	visiting  map[string]bool
	at        time.Time
	sensitive bool
	roots     map[string]bool
}

// Manifest checks the manifest's signature, historical holder, authenticated
// author key, capability delegation and recorded file scope. Integrity is
// independent of whether the caller trusts the root identity. Git tree contents
// and receipt completeness must additionally be checked by the M3 verifier.
func Manifest(records []envelope.Object, hash string, trusted []string, sensitive bool) Result {
	g, err := newGraph(records, sensitive)
	if err != nil {
		return failResult(err)
	}
	return g.manifest(hash, trusted)
}

func failResult(err error) Result {
	state := "invalid"
	if apierr.HasCode(err, "dependency_unavailable") {
		state = "unavailable"
	}
	return Result{Integrity: state, Trust: "unknown", Error: apierr.As(err)}
}

// newGraph indexes records by hash and by stable entity id, checking that
// each is canonical and hashes to its own name.
func newGraph(records []envelope.Object, sensitive bool) (*graph, error) {
	g := &graph{objects: map[string]envelope.Object{}, entities: map[string][]envelope.Object{}, visiting: map[string]bool{}, sensitive: sensitive, roots: map[string]bool{}}
	for _, o := range records {
		if _, ok := g.objects[o.Hash]; ok {
			continue
		}
		parsed, err := envelope.ParseStored(o.Bytes, o.Hash)
		if err != nil {
			return nil, err
		}
		raw, err := canon.Marshal(o.Envelope)
		if err != nil {
			return nil, err
		}
		if string(raw) != string(o.Bytes) {
			return nil, apierr.New("record_invalid", "envelope differs from bytes")
		}
		o = parsed
		g.objects[o.Hash] = o
		var id string
		switch o.Envelope.Kind {
		case schema.KindRootKey:
			id = o.Envelope.Signer
		case schema.KindSession, schema.KindCapability, schema.KindWorkspace, schema.KindIntent, schema.KindSpeculationRun:
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(o.Envelope.Payload, &p); err != nil {
				return nil, err
			}
			id = p.ID
		}
		if id != "" {
			g.entities[id] = append(g.entities[id], o)
		}
	}
	return g, nil
}

// manifest verifies one manifest in g. It may be called repeatedly; each
// call starts with no contributing roots.
func (g *graph) manifest(hash string, trusted []string) Result {
	g.roots = map[string]bool{}
	g.at = time.Time{}
	fail := failResult
	mobj, ok := g.objects[hash]
	if !ok || mobj.Envelope.Kind != schema.KindChangeManifest {
		return fail(apierr.New("dependency_unavailable", "missing manifest"))
	}
	var m schema.ChangeManifest
	if err := mobj.Decode(&m); err != nil {
		return fail(err)
	}
	var receipt *envelope.Object
	for _, o := range g.objects {
		if o.Envelope.Kind != schema.KindOperationReceipt {
			continue
		}
		var r schema.OperationReceipt
		if err := o.Decode(&r); err != nil {
			return fail(err)
		}
		if slices.Contains(r.Members, hash) {
			if receipt != nil {
				return fail(apierr.New("conflicting_receipt", "multiple manifest receipts"))
			}
			at, err := schema.ParseTimestamp(r.At)
			if err != nil {
				return fail(apierr.New("invalid_receipt", "receipt time %q: %v", r.At, err))
			}
			copyOf := o
			receipt = &copyOf
			g.at = at
		}
	}
	if receipt == nil {
		return fail(apierr.New("dependency_unavailable", "missing operation receipt"))
	}
	if err := g.signature(*receipt); err != nil {
		return fail(err)
	}
	if err := g.signature(mobj); err != nil {
		return fail(err)
	}
	if mobj.Envelope.Signer != m.AuthorSession {
		return fail(apierr.New("unauthorized", "manifest author differs"))
	}
	wobj, err := g.entity(m.WorkspaceID, schema.KindWorkspace)
	if err != nil {
		return fail(err)
	}
	if err := g.signature(wobj); err != nil {
		return fail(err)
	}
	var w schema.Workspace
	if err := wobj.Decode(&w); err != nil {
		return fail(err)
	}
	if w.BaseCommit != m.BaseCommit || w.ChangeID != m.ChangeID || w.IntentID != m.IntentID {
		return fail(apierr.New("unauthorized", "manifest workspace differs"))
	}
	owner, cid, head := w.SessionID, w.CapabilityID, wobj.Hash
	for range len(g.objects) + 1 {
		var next *envelope.Object
		for _, o := range g.objects {
			if o.Envelope.Kind != schema.KindWorkspaceBinding {
				continue
			}
			var b schema.WorkspaceBinding
			if err := o.Decode(&b); err != nil {
				return fail(err)
			}
			at, err := schema.ParseTimestamp(b.At)
			if err != nil {
				return fail(err)
			}
			if b.WorkspaceID == w.ID && b.Previous == head && !at.After(g.at) {
				if next != nil {
					return fail(apierr.New("binding_fork", "conflicting historical bindings"))
				}
				copyOf := o
				next = &copyOf
			}
		}
		if next == nil {
			break
		}
		var b schema.WorkspaceBinding
		if err := next.Decode(&b); err != nil {
			return fail(err)
		}
		if strings.HasPrefix(next.Envelope.Signer, "ses_") || b.Actor != next.Envelope.Signer {
			return fail(apierr.New("unauthorized", "recovery is not root authorized"))
		}
		if err := g.signature(*next); err != nil {
			return fail(err)
		}
		owner, cid, head = b.SessionID, b.CapabilityID, next.Hash
	}
	if owner != m.AuthorSession {
		return fail(apierr.New("unauthorized", "wrong historical workspace holder"))
	}
	chain, err := g.chain(cid)
	if err != nil {
		return fail(err)
	}
	if chain[0].SessionID != owner {
		return fail(apierr.New("unauthorized", "wrong capability holder"))
	}
	for _, p := range m.Files {
		if err := capability.Authorize(chain, p, g.sensitive, g.at); err != nil {
			return fail(err)
		}
	}
	// Every root contributing authority must be trusted, including a target
	// session authenticated by a different root than the delegation root.
	result := Result{Integrity: "verified", Trust: "trusted", Root: chain[len(chain)-1].Issuer.Fingerprint}
	for root := range g.roots {
		if !slices.Contains(trusted, root) {
			result.Trust = "untrusted"
		}
	}
	return result
}

func (g *graph) entity(id string, kind schema.Kind) (envelope.Object, error) {
	objects := g.entities[id]
	if len(objects) == 0 {
		return envelope.Object{}, apierr.New("dependency_unavailable", "missing %s", id)
	}
	if len(objects) != 1 {
		return envelope.Object{}, apierr.New("conflicting_creation", "ambiguous %s", id)
	}
	if objects[0].Envelope.Kind != kind {
		return envelope.Object{}, apierr.New("unauthorized", "wrong entity kind")
	}
	return objects[0], nil
}
func (g *graph) signature(o envelope.Object) error {
	// Authenticate the signer at this record's own recorded operation time.
	// A later manifest must not retime a historical delegation signature.
	savedAt := g.at
	defer func() { g.at = savedAt }()
	for _, candidate := range g.objects {
		if candidate.Envelope.Kind != schema.KindOperationReceipt {
			continue
		}
		var r schema.OperationReceipt
		if err := candidate.Decode(&r); err != nil {
			return err
		}
		if candidate.Hash == o.Hash || slices.Contains(r.Members, o.Hash) {
			at, err := schema.ParseTimestamp(r.At)
			if err != nil {
				return err
			}
			g.at = at
			break
		}
	}

	id := o.Envelope.Signer
	if g.visiting[o.Hash] {
		return apierr.New("invalid_capability_chain", "signature cycle")
	}
	g.visiting[o.Hash] = true
	defer delete(g.visiting, o.Hash)
	var key ed25519.PublicKey
	if strings.HasPrefix(id, "ses_") {
		sessionObject, err := g.entity(id, schema.KindSession)
		if err != nil {
			return err
		}
		if err := g.signature(sessionObject); err != nil {
			return err
		}
		if strings.HasPrefix(sessionObject.Envelope.Signer, "ses_") {
			return apierr.New("unauthorized", "session is not root authenticated")
		}
		var s schema.Session
		if err := sessionObject.Decode(&s); err != nil {
			return err
		}
		chain, err := g.chain(s.CapabilityID)
		if err != nil {
			return err
		}
		if len(chain) != 1 || chain[0].SessionID != s.ID || chain[0].Issuer.Fingerprint != sessionObject.Envelope.Signer {
			return apierr.New("unauthorized", "session root capability differs")
		}
		started, err := schema.ParseTimestamp(s.StartedAt)
		if err != nil {
			return err
		}
		if g.at.Before(started) {
			return apierr.New("session_inactive", "operation precedes session")
		}
		key, err = identity.DecodePublicKey(s.PublicKey)
		if err != nil {
			return err
		}

		// An authenticated end before this operation revokes further use of the
		// session; later end/recovery records do not invalidate earlier signatures.
		for _, event := range g.objects {
			if event.Envelope.Kind != schema.KindLifecycleEvent {
				continue
			}
			var e schema.LifecycleEvent
			if err := event.Decode(&e); err != nil {
				return err
			}
			if e.SubjectKind != "session" || e.SubjectID != id || e.Creation != sessionObject.Hash || e.ToState != "ended" || e.Previous != nil || e.Actor != event.Envelope.Signer {
				continue
			}
			ended, err := schema.ParseTimestamp(e.At)
			if err != nil {
				return err
			}
			if !ended.Before(g.at) {
				continue
			}
			committed := false
			for _, candidate := range g.objects {
				if candidate.Envelope.Kind != schema.KindOperationReceipt {
					continue
				}
				var r schema.OperationReceipt
				if err := candidate.Decode(&r); err != nil {
					return err
				}
				if slices.Contains(r.Members, event.Hash) {
					committed = true
					break
				}
			}
			if !committed {
				continue
			}
			if event.Envelope.Signer == id {
				if err := event.Verify(key); err != nil {
					return err
				}
			} else {
				if strings.HasPrefix(event.Envelope.Signer, "ses_") {
					continue
				}
				if err := g.signature(event); err != nil {
					return err
				}
			}
			return apierr.New("session_inactive", "operation follows authenticated session end")
		}
	} else {
		rootObject, err := g.entity(id, schema.KindRootKey)
		if err != nil {
			return err
		}
		var root schema.RootKey
		if err := rootObject.Decode(&root); err != nil {
			return err
		}
		key, err = identity.DecodePublicKey(root.PublicKey)
		if err != nil {
			return err
		}
		if identity.Fingerprint(key) != id {
			return apierr.New("unauthorized", "root fingerprint differs")
		}
		if err := rootObject.Verify(key); err != nil {
			return err
		}
		g.roots[id] = true
	}
	return o.Verify(key)
}
func (g *graph) chain(cid string) ([]schema.Capability, error) {
	return capability.Chain(cid, func(id string) (schema.Capability, error) {
		o, err := g.entity(id, schema.KindCapability)
		if err != nil {
			return schema.Capability{}, err
		}
		var c schema.Capability
		if err := o.Decode(&c); err != nil {
			return c, err
		}
		if c.Issuer.Kind == "root" {
			if c.Issuer.Fingerprint != o.Envelope.Signer || strings.HasPrefix(o.Envelope.Signer, "ses_") {
				return c, apierr.New("unauthorized", "root issuer differs")
			}
		} else {
			p, err := g.entity(c.Issuer.CapabilityID, schema.KindCapability)
			if err != nil {
				return c, err
			}
			var parent schema.Capability
			if err := json.Unmarshal(p.Envelope.Payload, &parent); err != nil {
				return c, err
			}
			if parent.SessionID != o.Envelope.Signer {
				return c, apierr.New("unauthorized", "delegation signer is not parent holder")
			}
		}
		return c, g.signature(o)
	}, g.sensitive, g.at)
}
