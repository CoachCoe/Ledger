package schema

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

func TestWireRoundTripsThroughDecode(t *testing.T) {
	tree, base := strings.Repeat("d", 64), strings.Repeat("c", 64)
	advance := strings.Repeat("e", 64)
	cases := []struct {
		kind Kind
		in   any
		out  any
	}{
		{KindCapability, &Capability{ID: "cap_1", Allow: []string{"**"}, Deny: []string{}, MayAdvance: []string{"main"}}, new(Capability)},
		{KindWorkspace, Workspace{ID: "wsp_1", BaseTree: base, BaseLineAdvance: &advance}, new(Workspace)},
		{KindChangeManifest, ChangeManifest{BaseTree: base, Tree: tree, OpHead: advance, SymbolMaps: map[string]string{"a.ts": tree},
			SymbolsWritten: []Symbol{{Path: "a.ts", Name: "f", Revision: tree}}}, new(ChangeManifest)},
		{KindEvidenceAttestation, EvidenceAttestation{Manifest: advance, Tree: tree}, new(EvidenceAttestation)},
		{KindSelectionDecision, SelectionDecision{Manifest: advance}, new(SelectionDecision)},
		{KindSpeculationRun, SpeculationRun{ID: "spr_1", BaseTree: base}, new(SpeculationRun)},
	}
	for _, c := range cases {
		w, err := Wire(2, c.kind, c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		if err := Decode(2, c.kind, raw, c.out); err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		back, err := Wire(2, c.kind, c.out)
		if err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(back)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(raw) {
			t.Errorf("%s: round trip\n got %s\nwant %s", c.kind, again, raw)
		}
	}
}

func TestWireRefusesGitBindingsAtVersion2(t *testing.T) {
	for kind, p := range map[Kind]any{
		KindWorkspace:           Workspace{BaseCommit: "abc"},
		KindChangeManifest:      ChangeManifest{BaseCommit: "abc"},
		KindEvidenceAttestation: EvidenceAttestation{ExecutedTree: "abc"},
		KindSelectionDecision:   SelectionDecision{Commit: "abc"},
		KindSpeculationRun:      SpeculationRun{BaseCommit: "abc"},
	} {
		if _, err := Wire(2, kind, p); err == nil {
			t.Errorf("%s with a Git binding converted to version 2", kind)
		}
		if w, err := Wire(1, kind, p); err != nil || !reflect.DeepEqual(w, p) {
			t.Errorf("%s at version 1: %v, %v", kind, w, err)
		}
	}
}
