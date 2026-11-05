package lifecycle

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func ptr(s string) *string { return &s }

func ev(kind, hash, prev, from, to string) Event {
	e := Event{Hash: hash, LifecycleEvent: schema.LifecycleEvent{SubjectKind: kind, FromState: from, ToState: to}}
	if prev != "" {
		e.Previous = ptr(prev)
	}
	if kind == Intent {
		e.Reason = ptr("completed")
	}
	return e
}

func TestTransitionTable(t *testing.T) {
	cases := []struct {
		kind, from, to string
		ok             bool
	}{
		{Intent, "open", "closed", true},
		{Intent, "closed", "open", false},
		{Session, "active", "ended", true},
		{Session, "ended", "active", false},
		{Workspace, "open", "proposed", true},
		{Workspace, "open", "closed", true},
		{Workspace, "open", "selected", false},
		{Workspace, "proposed", "open", true},
		{Workspace, "proposed", "selected", true},
		{Workspace, "proposed", "closed", true},
		{Workspace, "selected", "closed", false},
		{Workspace, "closed", "open", false},
		{SpeculationRun, "created", "collecting", true},
		{SpeculationRun, "created", "evaluating", false},
		{SpeculationRun, "collecting", "interrupted", true},
		{SpeculationRun, "evaluating", "resolved", true},
		{SpeculationRun, "resolved", "cancelled", false},
		{SpeculationRun, "cancelled", "collecting", false},
	}
	for _, c := range cases {
		e := ev(c.kind, "h", "", c.from, c.to)
		err := CheckTransition(&e.LifecycleEvent, c.from, "")
		if (err == nil) != c.ok {
			t.Errorf("%s %s->%s: %v", c.kind, c.from, c.to, err)
		}
	}
}

func TestStalePredecessorAndIntentReasons(t *testing.T) {
	e := ev(Workspace, "h", "", "open", "proposed")
	if err := CheckTransition(&e.LifecycleEvent, "proposed", ""); !apierr.HasCode(err, "stale_predecessor") {
		t.Fatalf("stale from-state: %v", err)
	}
	i := ev(Intent, "h", "", "open", "closed")
	i.Reason = ptr("bored")
	if CheckTransition(&i.LifecycleEvent, "open", "") == nil {
		t.Error("unknown close reason accepted")
	}
	i.Reason = ptr("superseded")
	if CheckTransition(&i.LifecycleEvent, "open", "") == nil {
		t.Error("superseded without replacement accepted")
	}
	i.ReplacementID = ptr("int_" + strings.Repeat("b", 26))
	if err := CheckTransition(&i.LifecycleEvent, "open", ""); err != nil {
		t.Error(err)
	}
	w := ev(Workspace, "h", "", "open", "closed")
	w.ReplacementID = ptr("int_x")
	if CheckTransition(&w.LifecycleEvent, "open", "") == nil {
		t.Error("replacement on a workspace accepted")
	}
}

func TestDeriveIsOrderIndependent(t *testing.T) {
	events := []Event{
		ev(Workspace, "a", "", "open", "proposed"),
		ev(Workspace, "b", "a", "proposed", "open"),
		ev(Workspace, "c", "b", "open", "proposed"),
		ev(Workspace, "d", "c", "proposed", "selected"),
	}
	want := State{State: "selected", Head: "d"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		r.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })
		if got := Derive(Workspace, events); !reflect.DeepEqual(got, want) {
			t.Fatalf("Derive = %+v, want %+v", got, want)
		}
	}
	if got := Derive(Workspace, nil); got.State != "open" || got.Head != "" {
		t.Fatalf("no events: %+v", got)
	}
}

func TestDeriveReportsForkAndRejected(t *testing.T) {
	fork := Derive(Workspace, []Event{
		ev(Workspace, "a", "", "open", "proposed"),
		ev(Workspace, "y", "a", "proposed", "closed"),
		ev(Workspace, "x", "a", "proposed", "selected"),
	})
	if fork.State != "proposed" || fork.Head != "a" || !reflect.DeepEqual(fork.Fork, []string{"x", "y"}) {
		t.Fatalf("fork: %+v", fork)
	}
	bad := Derive(Session, []Event{ev(Session, "a", "", "active", "ended"), ev(Session, "b", "a", "ended", "active")})
	if bad.State != "ended" || !reflect.DeepEqual(bad.Rejected, []string{"b"}) {
		t.Fatalf("rejected: %+v", bad)
	}
}

func TestInterruptedRunResumesOnlyItsStage(t *testing.T) {
	base := []Event{
		ev(SpeculationRun, "a", "", "created", "collecting"),
		ev(SpeculationRun, "b", "a", "collecting", "evaluating"),
		ev(SpeculationRun, "c", "b", "evaluating", "interrupted"),
	}
	st := Derive(SpeculationRun, base)
	if st.State != "interrupted" || st.Resume != "evaluating" {
		t.Fatalf("%+v", st)
	}
	wrong := Derive(SpeculationRun, append(base, ev(SpeculationRun, "d", "c", "interrupted", "collecting")))
	if len(wrong.Rejected) != 1 {
		t.Fatalf("resumed to the wrong stage: %+v", wrong)
	}
	right := Derive(SpeculationRun, append(base, ev(SpeculationRun, "d", "c", "interrupted", "evaluating")))
	if right.State != "evaluating" {
		t.Fatalf("%+v", right)
	}
}
