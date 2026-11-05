// Package lifecycle defines the state machines of Ledger's mutable
// subjects and derives current state from a set of lifecycle events
// independently of the order they were read in (ADR 0011).
package lifecycle

import (
	"sort"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Subject kinds.
const (
	Intent         = "intent"
	Session        = "session"
	Workspace      = "workspace"
	SpeculationRun = "speculation_run"
)

var initial = map[string]string{
	Intent:         "open",
	Session:        "active",
	Workspace:      "open",
	SpeculationRun: "created",
}

var transitions = map[string]map[string][]string{
	Intent:    {"open": {"closed"}},
	Session:   {"active": {"ended"}},
	Workspace: {"open": {"proposed", "closed"}, "proposed": {"open", "selected", "closed"}},
	SpeculationRun: {
		"created":     {"collecting", "cancelled"},
		"collecting":  {"evaluating", "cancelled", "interrupted"},
		"evaluating":  {"resolved", "cancelled", "interrupted"},
		"interrupted": {"cancelled"},
	},
}

var intentCloseReasons = map[string]bool{"completed": true, "abandoned": true, "superseded": true}

// SubjectKind returns the lifecycle subject kind created by a record kind,
// or "" if the kind creates no lifecycle subject.
func SubjectKind(k schema.Kind) string {
	switch k {
	case schema.KindIntent:
		return Intent
	case schema.KindSession:
		return Session
	case schema.KindWorkspace:
		return Workspace
	case schema.KindSpeculationRun:
		return SpeculationRun
	}
	return ""
}

// Initial returns the state a subject has on creation.
func Initial(kind string) string { return initial[kind] }

// CheckTransition validates one event against the subject's current state.
// resume is the state an interrupted speculation run may return to.
func CheckTransition(ev *schema.LifecycleEvent, current, resume string) error {
	if ev.FromState != current {
		return apierr.New("stale_predecessor", "%s %s is %s, not %s", ev.SubjectKind, ev.SubjectID, current, ev.FromState)
	}
	allowed := transitions[ev.SubjectKind][current]
	if ev.SubjectKind == SpeculationRun && current == "interrupted" && resume != "" {
		allowed = append([]string{resume}, allowed...)
	}
	ok := false
	for _, s := range allowed {
		ok = ok || s == ev.ToState
	}
	if !ok {
		return apierr.New("invalid_transition", "%s cannot go from %s to %s", ev.SubjectKind, current, ev.ToState)
	}
	if ev.SubjectKind == Intent {
		if ev.Reason == nil || !intentCloseReasons[*ev.Reason] {
			return apierr.New("invalid_transition", "closing an intent needs reason completed, abandoned, or superseded")
		}
		if (*ev.Reason == "superseded") != (ev.ReplacementID != nil) {
			return apierr.New("invalid_transition", "replacement_id is required exactly when an intent is superseded")
		}
	} else if ev.ReplacementID != nil {
		return apierr.New("invalid_transition", "replacement_id applies only to intents")
	}
	return nil
}

// Event is a committed lifecycle event and its object hash.
type Event struct {
	Hash string
	schema.LifecycleEvent
}

// State is a subject's derived state.
type State struct {
	State string
	// Head is the hash of the latest accepted event, or "" if none.
	Head string
	// Resume is the state an interrupted run may return to.
	Resume string
	// Fork lists the competing events when two events claim the same
	// predecessor. A forked subject is blocked until resolved.
	Fork []string
	// Rejected lists events that chain correctly but violate the state
	// machine; they are reported, never applied.
	Rejected []string
}

// Derive computes a subject's state from all of its committed events. The
// result does not depend on the order of events.
func Derive(kind string, events []Event) State {
	next := map[string][]Event{}
	for _, e := range events {
		prev := ""
		if e.Previous != nil {
			prev = *e.Previous
		}
		next[prev] = append(next[prev], e)
	}
	st := State{State: initial[kind]}
	for {
		children := next[st.Head]
		if len(children) == 0 {
			return st
		}
		if len(children) > 1 {
			for _, c := range children {
				st.Fork = append(st.Fork, c.Hash)
			}
			sort.Strings(st.Fork)
			return st
		}
		e := children[0]
		if err := CheckTransition(&e.LifecycleEvent, st.State, st.Resume); err != nil {
			st.Rejected = append(st.Rejected, e.Hash)
			return st
		}
		if e.ToState == "interrupted" {
			st.Resume = st.State
		}
		st.State, st.Head = e.ToState, e.Hash
	}
}
