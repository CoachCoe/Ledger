package verify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// Sign the inconsistent claims and their receipt: these are semantic errors,
// not corrupted bytes or broken signatures.
func TestSelectionClaimsMatchProposal(t *testing.T) {
	c := newChain(t, "true")
	for _, name := range []string{"valid_empty_set", "intent", "self_competitor", "missing_competitor", "wrong_competitor_kind", "wrong_evidence_kind"} {
		t.Run(name, func(t *testing.T) {
			src, err := verify.OpenNativeSource(c.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			var d schema.SelectionDecisionV2
			if err := src.Corpus.Objects[c.choice].Decode(&d); err != nil {
				t.Fatal(err)
			}
			d.Evidence = []string{}
			for i := range d.Criteria {
				d.Criteria[i].Status = "outstanding"
				d.Criteria[i].Deciding = []string{}
				d.Criteria[i].Conflicting = []string{}
			}
			switch name {
			case "intent":
				d.IntentID = schema.NewID(schema.PrefixIntent)
			case "self_competitor":
				d.Competitors = []string{c.manifest}
			case "missing_competitor":
				d.Competitors = []string{strings.Repeat("1", 64)}
			case "wrong_competitor_kind":
				d.Competitors = []string{c.evidence}
			case "wrong_evidence_kind":
				d.Evidence = []string{c.manifest}
			}
			o, err := envelope.SealVersion(2, schema.KindSelectionDecision, d, c.root)
			if err != nil {
				t.Fatal(err)
			}
			r, err := envelope.SealVersion(2, schema.KindOperationReceipt, schema.OperationReceipt{
				OperationID: schema.NewID(schema.PrefixOperation), Operation: "selection.record",
				RequestDigest: canon.Hash([]byte(name)), Result: []byte("{}"), Members: []string{o.Hash}, At: d.At,
			}, c.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, obj := range []envelope.Object{o, r} {
				src.Corpus.Objects[obj.Hash] = obj
				src.Corpus.Committed[obj.Hash] = true
			}
			rep, err := verify.Verify(context.Background(), src, verify.Options{})
			if err != nil {
				t.Fatal(err)
			}
			want := verify.Invalid
			if name == "valid_empty_set" {
				want = verify.Verified
			}
			if name == "missing_competitor" {
				want = verify.Unavailable
			}
			if rep.Integrity != want {
				t.Fatalf("integrity=%s, want %s; selections=%+v", rep.Integrity, want, rep.Manifests[0].Selections)
			}
			if name == "missing_competitor" && (rep.Complete || len(rep.Missing) == 0) {
				t.Fatal("missing competitor must make the source incomplete so import refuses it")
			}
		})
	}
}

func TestEvidenceClaimsMatchPinnedCriteria(t *testing.T) {
	c := newChain(t, "true")
	signer, err := identity.NewSession(schema.NewID(schema.PrefixSession))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "category", "unknown_criterion", "judgment_criterion", "wrong_definition", "wrong_definition_kind"} {
		t.Run(name, func(t *testing.T) {
			src, err := verify.OpenNativeSource(c.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			var session schema.Session
			var capability schema.Capability
			for _, o := range src.Corpus.Objects {
				if o.Envelope.Kind == schema.KindSession {
					var s schema.Session
					if err := o.Decode(&s); err != nil {
						t.Fatal(err)
					}
					if s.ID == c.sid {
						session = s
					}
				}
				if o.Envelope.Kind == schema.KindCapability {
					var cp schema.Capability
					if err := o.Decode(&cp); err != nil {
						t.Fatal(err)
					}
					if cp.ID == c.cap {
						capability = cp
					}
				}
			}
			session.ID, session.CapabilityID = signer.ID(), schema.NewID(schema.PrefixCapability)
			session.PublicKey = identity.EncodePublicKey(signer.PublicKey())
			capability.ID, capability.SessionID = session.CapabilityID, session.ID
			add := func(kind schema.Kind, payload any, key identity.Signer) envelope.Object {
				t.Helper()
				o, err := envelope.SealVersion(2, kind, payload, key)
				if err != nil {
					t.Fatal(err)
				}
				src.Corpus.Objects[o.Hash], src.Corpus.Committed[o.Hash] = o, true
				return o
			}
			s := add(schema.KindSession, session, c.root)
			cp := add(schema.KindCapability, capability, c.root)
			add(schema.KindOperationReceipt, schema.OperationReceipt{
				OperationID: schema.NewID(schema.PrefixOperation), Operation: "session.create",
				RequestDigest: canon.Hash([]byte("verifier")), Result: []byte("{}"), Members: []string{s.Hash, cp.Hash}, At: session.StartedAt,
			}, c.root)
			var att schema.EvidenceAttestation
			if err := src.Corpus.Objects[c.evidence].Decode(&att); err != nil {
				t.Fatal(err)
			}
			att.VerifierSession = signer.ID()
			switch name {
			case "category":
				att.Category = "behavioral"
			case "unknown_criterion":
				att.Criteria = []string{"missing"}
			case "judgment_criterion":
				att.Criteria = []string{"review"}
			case "wrong_definition_kind":
				att.CheckDefinition = c.manifest
			case "wrong_definition":
				var def schema.CheckDefinition
				if err := src.Corpus.Objects[att.CheckDefinition].Decode(&def); err != nil {
					t.Fatal(err)
				}
				def.Name = "unrelated"
				att.CheckDefinition = add(schema.KindCheckDefinition, def, c.root).Hash
			}
			o := add(schema.KindEvidenceAttestation, att, signer)
			add(schema.KindOperationReceipt, schema.OperationReceipt{
				OperationID: schema.NewID(schema.PrefixOperation), Operation: "evidence.run",
				RequestDigest: canon.Hash([]byte(name)), Result: []byte("{}"), Members: []string{o.Hash}, At: att.EndedAt,
			}, signer)
			rep, err := verify.Verify(context.Background(), src, verify.Options{})
			if err != nil {
				t.Fatal(err)
			}
			want := verify.Invalid
			if name == "valid" {
				want = verify.Verified
			}
			if rep.Integrity != want {
				t.Fatalf("integrity=%s, want %s; evidence=%+v", rep.Integrity, want, rep.Manifests[0].Evidence)
			}
		})
	}
}

func TestSelectionSurvivesLaterContraryEvidence(t *testing.T) {
	c := newChain(t, "true")
	c.call("evidence.judge", "", map[string]any{
		"manifest": c.manifest, "criterion_id": "review",
		"result": "fail", "reason": "later review found a problem",
	})
	rep := verifyBundle(t, c.mustExport())
	if rep.Integrity != verify.Verified {
		t.Fatalf("later evidence invalidated an earlier selection: %+v", rep.Manifests[0].Selections)
	}
	dst := c.earlier()
	dst.mustImport(c.mustExport())
	before := dst.digest()
	if _, err := dst.store.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dst.digest() != before {
		t.Fatal("reindex changed the imported history")
	}
}
