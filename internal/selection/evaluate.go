// Package selection evaluates acceptance criteria against evidence. It is
// pure: it imports only schema, so the index validator, the core queries and
// bundle verification can share one definition of "applicable evidence".
package selection

import (
	"cmp"
	"slices"

	"github.com/CoachCoe/Ledger/internal/schema"
)

// Criterion statuses.
const (
	Outstanding = "outstanding"
	Satisfied   = "satisfied"
	Failed      = "failed"
)

// Reasons an attestation does not apply to a criterion. They are API values.
const (
	ReasonOtherManifest      = "other_manifest"
	ReasonOtherCommit        = "other_commit"
	ReasonOtherTree          = "other_tree"
	ReasonUnknownCriterion   = "unknown_criterion"
	ReasonCriterionNotListed = "criterion_not_listed"
	ReasonCategoryMismatch   = "category_mismatch"
	ReasonDefinitionMismatch = "definition_mismatch"
	ReasonNotIndependent     = "not_independent"
	ReasonReviewerKind       = "reviewer_kind_mismatch"
	ReasonInconsistent       = "inconsistent_result"
	ReasonIsolation          = "isolation_not_none"
)

// Evidence is one committed attestation with the facts a reader derives
// about its signer. Hash is the attestation record hash.
type Evidence struct {
	Hash               string
	Att                schema.EvidenceAttestation
	Signer             string
	SignerKind         string // participant_kind of the signing session; "" if unknown
	Independent        bool
	IndependenceReason string // why not independent; "" when independent
}

// Binding is the exact revision being evaluated and its criteria.
type Binding struct {
	Manifest string
	Commit   string
	Tree     string
	Criteria []schema.Criterion
}

// Rejection records one attestation claim that did not apply.
type Rejection struct {
	Hash      string `json:"hash"`
	Criterion string `json:"criterion_id"`
	Reason    string `json:"reason"`
}

// Result is the evaluation of every criterion over one evidence set.
type Result struct {
	Criteria []schema.CriterionStatus
	// Contrary lists every known applicable failing attestation for the
	// binding, whether or not it was in the evaluated set.
	Contrary []string
	Rejected []Rejection
	// MechanicalFailed reports a failed check criterion (M5 eligibility).
	MechanicalFailed bool
}

// Consistent reports why an attestation's recorded result contradicts its
// recorded phases, or "" when it is consistent. A pass needs a successful
// setup (when present) and check; judgments record no process phases.
func Consistent(a schema.EvidenceAttestation) string {
	if a.Result != "pass" && a.Result != "fail" {
		return ReasonInconsistent
	}
	if a.Category == "judgment" {
		if a.Setup != nil || a.Check != nil || len(a.Criteria) != 1 {
			return ReasonInconsistent
		}
		return ""
	}
	if (a.Result == "pass") != (a.Reason == nil) {
		return ReasonInconsistent
	}
	if a.Result == "pass" && !passingPhases(a) {
		return ReasonInconsistent
	}
	return ""
}

func passingPhases(a schema.EvidenceAttestation) bool {
	ok := func(p *schema.Phase) bool {
		return p != nil && p.ExitStatus != nil && *p.ExitStatus == 0 && !p.TimedOut
	}
	return (a.Setup == nil || ok(a.Setup)) && ok(a.Check)
}

// Applicable reports whether e may decide criterion c of binding b, and if
// not, why. A failure to apply never deletes the attestation; callers list
// it in Rejected.
func Applicable(b Binding, c schema.Criterion, e Evidence) (bool, string) {
	a := e.Att
	switch {
	case a.Manifest != b.Manifest:
		return false, ReasonOtherManifest
	case a.Commit != b.Commit:
		return false, ReasonOtherCommit
	case a.ExecutedOn() != b.Tree:
		return false, ReasonOtherTree
	case !slices.Contains(a.Criteria, c.ID):
		return false, ReasonCriterionNotListed
	}
	switch c.Verification.Kind {
	case "check":
		if a.Category != "mechanical" && a.Category != "behavioral" {
			return false, ReasonCategoryMismatch
		}
		if a.CheckDefinition != c.Verification.DefinitionHash {
			return false, ReasonDefinitionMismatch
		}
	case "judgment":
		if a.Category != "judgment" {
			return false, ReasonCategoryMismatch
		}
	default:
		return false, ReasonUnknownCriterion
	}
	if !e.Independent {
		return false, ReasonNotIndependent
	}
	if c.Verification.Kind == "judgment" && !reviewerAllowed(c.Verification.Reviewer, e.SignerKind) {
		return false, ReasonReviewerKind
	}
	if why := Consistent(a); why != "" {
		return false, why
	}
	if a.Isolation != "none" {
		return false, ReasonIsolation
	}
	return true, ""
}

// reviewerAllowed maps a criterion's reviewer requirement onto the
// authenticated participant kind of the signer. A service never reviews.
func reviewerAllowed(reviewer, kind string) bool {
	switch reviewer {
	case "human":
		return kind == "human"
	case "agent":
		return kind == "agent"
	case "any":
		return kind == "human" || kind == "agent"
	}
	return false
}

// Evaluate computes every criterion's status over set. known is every
// attestation recorded for the binding; it only feeds Contrary. Hash lists
// are sorted so equal inputs give byte-equal results.
func Evaluate(b Binding, set []Evidence, known []Evidence) Result {
	res := Result{Criteria: []schema.CriterionStatus{}, Contrary: []string{}, Rejected: []Rejection{}}
	set = dedupe(set)
	for _, c := range b.Criteria {
		var pass, fail []string
		for _, e := range set {
			if ok, _ := Applicable(b, c, e); !ok {
				continue
			}
			if e.Att.Result == "pass" {
				pass = append(pass, e.Hash)
			} else {
				fail = append(fail, e.Hash)
			}
		}
		st := schema.CriterionStatus{CriterionID: c.ID, Status: Outstanding, Deciding: []string{}, Conflicting: []string{}}
		switch {
		case len(fail) > 0:
			st.Status, st.Deciding, st.Conflicting = Failed, sorted(fail), sorted(pass)
			if c.Verification.Kind == "check" {
				res.MechanicalFailed = true
			}
		case len(pass) > 0:
			st.Status, st.Deciding = Satisfied, sorted(pass)
		}
		res.Criteria = append(res.Criteria, st)
	}
	for _, e := range set {
		for _, id := range sorted(e.Att.Criteria) {
			i := slices.IndexFunc(b.Criteria, func(c schema.Criterion) bool { return c.ID == id })
			if i < 0 {
				res.Rejected = append(res.Rejected, Rejection{Hash: e.Hash, Criterion: id, Reason: ReasonUnknownCriterion})
			} else if ok, why := Applicable(b, b.Criteria[i], e); !ok {
				res.Rejected = append(res.Rejected, Rejection{Hash: e.Hash, Criterion: id, Reason: why})
			}
		}
	}
	slices.SortFunc(res.Rejected, func(x, y Rejection) int {
		if x.Hash != y.Hash {
			return cmp.Compare(x.Hash, y.Hash)
		}
		return cmp.Compare(x.Criterion, y.Criterion)
	})
	res.Contrary = contrary(b, known)
	return res
}

// contrary returns the applicable failing attestations among known.
func contrary(b Binding, known []Evidence) []string {
	out := []string{}
	for _, e := range dedupe(known) {
		if e.Att.Result != "fail" {
			continue
		}
		for _, c := range b.Criteria {
			if ok, _ := Applicable(b, c, e); ok {
				out = append(out, e.Hash)
				break
			}
		}
	}
	return sorted(out)
}

func dedupe(in []Evidence) []Evidence {
	seen := map[string]bool{}
	out := make([]Evidence, 0, len(in))
	for _, e := range in {
		if !seen[e.Hash] {
			seen[e.Hash] = true
			out = append(out, e)
		}
	}
	return out
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
