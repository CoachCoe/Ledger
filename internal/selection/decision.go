package selection

import (
	"encoding/json/jsontext"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Build assembles the selection decision for one exact revision. Statuses are
// always recomputed here from set; callers never supply them. Evidence that
// names another manifest, commit or tree is refused with
// cross_revision_evidence, not ignored. known is every attestation recorded
// for the binding and only feeds contrary_evidence. A decision records a
// choice: it is not acceptance and authorizes nothing.
func Build(b Binding, intentID string, set, known []Evidence, selector schema.Selector, competitors []string, scoring jsontext.Value, at string) (schema.SelectionDecision, error) {
	for _, e := range set {
		a := e.Att
		if a.Manifest != b.Manifest || a.Commit != b.Commit || a.ExecutedTree != b.Tree {
			return schema.SelectionDecision{}, apierr.New("cross_revision_evidence", "evidence %s was recorded for manifest %s, not %s", e.Hash, a.Manifest, b.Manifest).
				With("evidence", e.Hash).With("evidence_manifest", a.Manifest)
		}
	}
	res := Evaluate(b, set, known)
	hashes := make([]string, 0, len(set))
	for _, e := range set {
		hashes = append(hashes, e.Hash)
	}
	if len(scoring) == 0 {
		scoring = jsontext.Value("null")
	}
	return schema.SelectionDecision{
		IntentID: intentID, Manifest: b.Manifest, Commit: b.Commit,
		Competitors: sorted(competitors), Evidence: sorted(hashes), Criteria: res.Criteria,
		ContraryEvidence: res.Contrary, Selector: selector, Scoring: scoring, At: at,
	}, nil
}

// OutstandingIDs lists the ids of criteria that no applicable attestation has
// decided, in criterion order.
func OutstandingIDs(criteria []schema.CriterionStatus) []string {
	out := []string{}
	for _, c := range criteria {
		if c.Status == Outstanding {
			out = append(out, c.CriterionID)
		}
	}
	return out
}

// SameStatuses reports whether two criterion status lists are identical.
func SameStatuses(a, b []schema.CriterionStatus) bool {
	return slices.EqualFunc(a, b, func(x, y schema.CriterionStatus) bool {
		return x.CriterionID == y.CriterionID && x.Status == y.Status &&
			slices.Equal(x.Deciding, y.Deciding) && slices.Equal(x.Conflicting, y.Conflicting)
	})
}
