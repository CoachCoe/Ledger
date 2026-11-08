// Package coord computes advisory runtime overlap and lease conflicts.
package coord

import (
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/capability"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/symbols"
)

type Overlap struct {
	Path       string         `json:"path,omitzero"`
	Symbol     *schema.Symbol `json:"symbol,omitempty"`
	Kind       string         `json:"kind"`
	Confidence string         `json:"confidence"`
	Reason     string         `json:"reason,omitzero"`
}

func Overlaps(a, b symbols.Summary) []Overlap {
	out := []Overlap{}
	seen := map[string]bool{}
	add := func(o Overlap) {
		key := o.Path + ":" + o.Kind + ":" + o.Reason
		if o.Symbol != nil {
			key += symbols.Key(*o.Symbol)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, o)
		}
	}
	compare := func(left, right []schema.Symbol, kind, confidence string) {
		for _, s := range left {
			for _, r := range right {
				if symbols.Same(s, r) {
					add(Overlap{Symbol: &s, Kind: kind, Confidence: confidence})
				}
			}
		}
	}
	compare(a.Written, b.Written, "write_write", "direct")
	compare(a.Written, b.Read, "write_read", "static_inferred")
	compare(b.Written, a.Read, "write_read", "static_inferred")
	fallback := func(left, right symbols.Summary) {
		for _, f := range left.Fallbacks {
			targets := slices.Clone(right.Written)
			if f.Relation != "read" {
				targets = append(targets, right.Read...)
			}
			for _, s := range targets {
				if s.Path == f.Path {
					add(Overlap{Path: f.Path, Kind: "file_level", Confidence: "file_level", Reason: f.Reason})
				}
			}
			for _, other := range right.Fallbacks {
				if f.Path == other.Path && (f.Relation != "read" || other.Relation != "read") {
					add(Overlap{Path: f.Path, Kind: "file_level", Confidence: "file_level", Reason: f.Reason})
				}
			}
		}
	}
	fallback(a, b)
	fallback(b, a)
	slices.SortFunc(out, func(a, b Overlap) int {
		ka, kb := a.Path+a.Kind+a.Reason, b.Path+b.Kind+b.Reason
		if a.Symbol != nil {
			ka += symbols.Key(*a.Symbol)
		}
		if b.Symbol != nil {
			kb += symbols.Key(*b.Symbol)
		}
		return strings.Compare(ka, kb)
	})
	return out
}

type Lease struct {
	ID        string          `json:"id"`
	Holder    string          `json:"holder"`
	Paths     []string        `json:"paths"`
	Symbols   []schema.Symbol `json:"symbols"`
	Reason    string          `json:"reason"`
	ExpiresAt string          `json:"expires_at"`
}

// LeaseConflicts deliberately returns warnings, never an authorization result.
func LeaseConflicts(s symbols.Summary, l Lease, sensitive bool) []Overlap {
	normalize := func(p string) string {
		normalized, err := capability.Normalize(p, sensitive)
		if err != nil {
			return p
		}
		return normalized
	}
	s.Written = slices.Clone(s.Written)
	s.Fallbacks = slices.Clone(s.Fallbacks)
	for i := range s.Written {
		s.Written[i].Path = normalize(s.Written[i].Path)
	}
	for i := range s.Fallbacks {
		s.Fallbacks[i].Path = normalize(s.Fallbacks[i].Path)
	}
	other := symbols.Summary{Written: slices.Clone(l.Symbols)}
	for i := range other.Written {
		other.Written[i].Path = normalize(other.Written[i].Path)
	}
	for _, p := range l.Paths {
		other.Fallbacks = append(other.Fallbacks, symbols.Fallback{Path: normalize(p), Reason: "advisory_lease"})
	}
	// Only writes conflict with leases: ordinary references do not hold a lock.
	s.Read = nil
	s.Fallbacks = slices.DeleteFunc(s.Fallbacks, func(f symbols.Fallback) bool { return f.Relation == "read" })
	return Overlaps(s, other)
}
