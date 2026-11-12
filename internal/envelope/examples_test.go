package envelope

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

var update = flag.Bool("update", false, "re-sign schemas/examples with the test keys")

// Deterministic keys that sign the committed examples.
var testSigners = func() map[string]identity.Signer {
	key := func(b byte) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32)) }
	agent, verifier := "ses_"+strings.Repeat("a", 26), "ses_"+strings.Repeat("v", 26)
	root := identity.RootSigner(key(1))
	return map[string]identity.Signer{
		root.ID(): root,
		agent:     identity.SessionSigner(agent, key(2)),
		verifier:  identity.SessionSigner(verifier, key(3)),
	}
}()

const examplesDir = "../../schemas/examples"

// readExamples returns every example keyed "v<version>/<kind>".
func readExamples(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, v := range schema.Versions {
		dir := fmt.Sprintf("v%d", v)
		files, err := filepath.Glob(filepath.Join(examplesDir, dir, "*.json"))
		if err != nil || len(files) != len(schema.KindsFor(v)) {
			t.Fatalf("found %d %s examples, want one per kind (%d): %v", len(files), dir, len(schema.KindsFor(v)), err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if *update {
				raw = resign(t, raw)
				if err := os.WriteFile(f, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out[dir+"/"+strings.TrimSuffix(filepath.Base(f), ".json")] = raw
		}
	}
	return out
}

func TestExamplesRoundTrip(t *testing.T) {
	for key, raw := range readExamples(t) {
		t.Run(key, func(t *testing.T) {
			o, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("v%d/%s", o.Envelope.SchemaVersion, o.Envelope.Kind) != key {
				t.Fatalf("version %d kind %q in %s.json", o.Envelope.SchemaVersion, o.Envelope.Kind, key)
			}
			s := testSigners[o.Envelope.Signer]
			if s == nil {
				t.Fatalf("unknown signer %s", o.Envelope.Signer)
			}
			if err := o.Verify(s.PublicKey()); err != nil {
				t.Fatal(err)
			}
			// Decoding into the Go type and re-encoding must reproduce the
			// canonical payload, so Go types and schemas cannot drift.
			p, err := o.Payload()
			if err != nil {
				t.Fatal(err)
			}
			again, err := canon.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, o.Envelope.Payload) {
				t.Fatalf("payload round trip changed:\n got %s\nwant %s", again, o.Envelope.Payload)
			}
			resealed, err := SealVersion(o.Envelope.SchemaVersion, o.Envelope.Kind, p, s)
			if err != nil {
				t.Fatal(err)
			}
			if resealed.Hash != o.Hash {
				t.Fatalf("resealed hash %s, want %s", resealed.Hash, o.Hash)
			}
		})
	}
}

func resign(t *testing.T, raw []byte) []byte {
	t.Helper()
	var env schema.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	s := testSigners[env.Signer]
	if s == nil {
		t.Fatalf("unknown signer %s", env.Signer)
	}
	msg, err := SigningInput(env)
	if err != nil {
		t.Fatal(err)
	}
	env.Signature = identity.EncodeSignature(s.Sign(msg))
	out, err := json.Marshal(env, jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func mutated(t *testing.T, raw []byte, mutate func(env, payload map[string]any)) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	mutate(env, env["payload"].(map[string]any))
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGenericMutationsRejected(t *testing.T) {
	mutations := map[string]func(env, payload map[string]any){
		"unknown envelope member": func(env, _ map[string]any) { env["extra"] = true },
		"unknown payload member":  func(_, p map[string]any) { p["extra"] = true },
		"unknown kind":            func(env, _ map[string]any) { env["kind"] = "mystery" },
		"missing signature":       func(env, _ map[string]any) { delete(env, "signature") },
		"padded signature":        func(env, _ map[string]any) { env["signature"] = env["signature"].(string) + "==" },
		"bad signer":              func(env, _ map[string]any) { env["signer"] = "root" },
		"fraction in payload":     func(_, p map[string]any) { p["extra"] = 1.5 },
	}
	for kind, raw := range readExamples(t) {
		for name, mutate := range mutations {
			if _, err := Parse(mutated(t, raw, mutate)); err == nil {
				t.Errorf("%s: %s accepted", kind, name)
			}
		}
	}
}

// Every payload member is required; optional values are explicit nulls.
func TestEveryPayloadMemberRequired(t *testing.T) {
	for kind, raw := range readExamples(t) {
		o, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(o.Envelope.Payload, &p); err != nil {
			t.Fatal(err)
		}
		for field := range p {
			b := mutated(t, raw, func(_, p map[string]any) { delete(p, field) })
			if _, err := Parse(b); err == nil {
				t.Errorf("%s without %s accepted", kind, field)
			}
		}
	}
}

func TestTargetedRejections(t *testing.T) {
	ex := readExamples(t)
	cases := []struct {
		name, kind string
		mutate     func(p map[string]any)
	}{
		{"intent without criteria", "v1/intent", func(p map[string]any) { p["criteria"] = []any{} }},
		{"check criterion without hash", "v1/intent", func(p map[string]any) {
			v := p["criteria"].([]any)[0].(map[string]any)["verification"].(map[string]any)
			delete(v, "definition_hash")
		}},
		{"judgment with unknown reviewer", "v1/intent", func(p map[string]any) {
			v := p["criteria"].([]any)[1].(map[string]any)["verification"].(map[string]any)
			v["reviewer"] = "robot"
		}},
		{"uppercase hash", "v1/lifecycle_event", func(p map[string]any) { p["creation"] = strings.Repeat("A", 64) }},
		{"bad timestamp", "v1/lifecycle_event", func(p map[string]any) { p["at"] = "2026-11-05 12:00:00" }},
		{"non-UTC timestamp", "v1/lifecycle_event", func(p map[string]any) { p["at"] = "2026-11-05T12:00:00+01:00" }},
		{"unknown state", "v1/lifecycle_event", func(p map[string]any) { p["to_state"] = "merged" }},
		{"wrong id prefix", "v1/workspace", func(p map[string]any) { p["id"] = "int_" + strings.Repeat("a", 26) }},
		{"isolation claimed", "v1/evidence_attestation", func(p map[string]any) { p["isolation"] = "container" }},
		{"unsafe integer", "v1/attempt_record", func(p map[string]any) { p["score"] = int64(canon.MaxSafe) + 1 }},
		{"empty allow list", "v1/capability", func(p map[string]any) { p["allow"] = []any{} }},
		{"duplicate receipt members", "v1/operation_receipt", func(p map[string]any) {
			m := p["members"].([]any)
			p["members"] = append(m, m[0])
		}},
		{"v2 symbol without revision", "v2/change_manifest", func(p map[string]any) {
			delete(p["symbols_read"].([]any)[0].(map[string]any), "revision")
		}},
		{"v2 manifest with a Git tree", "v2/change_manifest", func(p map[string]any) { p["tree"] = strings.Repeat("d", 40) }},
		{"v2 manifest naming a commit", "v2/change_manifest", func(p map[string]any) { p["base_commit"] = strings.Repeat("c", 40) }},
		{"v2 symbol map naming a Git blob", "v2/change_manifest", func(p map[string]any) {
			p["symbol_maps"] = map[string]any{"src/app.ts": strings.Repeat("1", 40)}
		}},
		{"v2 attestation naming a commit", "v2/evidence_attestation", func(p map[string]any) { p["commit"] = strings.Repeat("c", 40) }},
		{"v2 selection naming a commit", "v2/selection_decision", func(p map[string]any) { p["commit"] = strings.Repeat("c", 40) }},
		{"v2 capability without may_advance", "v2/capability", func(p map[string]any) { delete(p, "may_advance") }},
		{"v2 capability with a bad line", "v2/capability", func(p map[string]any) { p["may_advance"] = []any{"Main Line"} }},
		{"v2 repository with another hash", "v2/repository", func(p map[string]any) { p["hash_algorithm"] = "sha1" }},
		{"v2 repository with a bad case policy", "v2/repository", func(p map[string]any) { p["case_policy"] = "fold" }},
		{"v2 advance without a selection", "v2/line_advance", func(p map[string]any) { delete(p, "selection") }},
		{"v1 workspace naming a tree", "v1/workspace", func(p map[string]any) { p["base_tree"] = strings.Repeat("c", 64) }},
	}
	for _, c := range cases {
		if ex[c.kind] == nil {
			t.Fatalf("%s: no example %s", c.name, c.kind)
		}
		b := mutated(t, ex[c.kind], func(_, p map[string]any) { c.mutate(p) })
		if _, err := Parse(b); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
}

func TestUnsupportedVersionCode(t *testing.T) {
	b := mutated(t, readExamples(t)["v2/root_key"], func(env, _ map[string]any) { env["schema_version"] = 3 })
	if _, err := Parse(b); !apierr.HasCode(err, "unsupported_schema_version") {
		t.Fatalf("Parse = %v, want unsupported_schema_version", err)
	}
}

// A payload is judged by its envelope's version: a v1 payload relabelled v2
// (or the reverse) fails its schema instead of being read as the other.
func TestPayloadsBoundToTheirVersion(t *testing.T) {
	ex := readExamples(t)
	for _, kind := range []string{"workspace", "change_manifest", "evidence_attestation", "selection_decision", "speculation_run", "capability"} {
		for from, to := range map[string]int{"v1": 2, "v2": 1} {
			b := mutated(t, ex[from+"/"+kind], func(env, _ map[string]any) { env["schema_version"] = to })
			if _, err := Parse(b); !apierr.HasCode(err, "schema_invalid") {
				t.Errorf("%s/%s as version %d: %v", from, kind, to, err)
			}
		}
	}
	for _, kind := range []string{"repository", "line_advance"} {
		b := mutated(t, ex["v2/"+kind], func(env, _ map[string]any) { env["schema_version"] = 1 })
		if _, err := Parse(b); !apierr.HasCode(err, "schema_invalid") {
			t.Errorf("%s as version 1: %v", kind, err)
		}
	}
}

// The signing prefix names the version, so a signature over v1 bytes never
// verifies as v2.
func TestSignatureBoundToVersion(t *testing.T) {
	raw := readExamples(t)["v1/intent"]
	o, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	relabelled, err := Parse(mutated(t, raw, func(env, _ map[string]any) { env["schema_version"] = 2 }))
	if err != nil {
		t.Fatal(err)
	}
	if err := relabelled.Verify(testSigners[o.Envelope.Signer].PublicKey()); !apierr.HasCode(err, "signature_invalid") {
		t.Fatalf("v1 signature verified as v2: %v", err)
	}
}

func TestTamperedBytesFailVerification(t *testing.T) {
	raw := readExamples(t)["v1/intent"]
	o, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pub := testSigners[o.Envelope.Signer].PublicKey()
	tampered := mutated(t, raw, func(_, p map[string]any) { p["description"] = "Add a health endpoint!" })
	t2, err := Parse(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := t2.Verify(pub); !apierr.HasCode(err, "signature_invalid") {
		t.Fatalf("tampered payload verified: %v", err)
	}
	wrongSigner := mutated(t, raw, func(env, _ map[string]any) { env["signer"] = strings.Repeat("0", 64) })
	t3, err := Parse(wrongSigner)
	if err != nil {
		t.Fatal(err)
	}
	if err := t3.Verify(pub); err == nil {
		t.Fatal("changed signer still verified")
	}
}

func TestParseStored(t *testing.T) {
	pretty := readExamples(t)["v1/root_key"]
	o, err := Parse(pretty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseStored(o.Bytes, o.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseStored(o.Bytes[:len(o.Bytes)-1], o.Hash); !apierr.HasCode(err, "record_hash_mismatch") {
		t.Fatalf("truncated: %v", err)
	}
	if _, err := ParseStored(pretty, canon.Hash(pretty)); !apierr.HasCode(err, "record_not_canonical") {
		t.Fatalf("non-canonical: %v", err)
	}
}

func TestInvalidCalendarTimestampRejected(t *testing.T) {
	raw := mutated(t, readExamples(t)["v1/intent"], func(_, p map[string]any) { p["created_at"] = "2026-99-99T99:99:99Z" })
	if _, err := Parse(raw); err == nil {
		t.Fatal("impossible calendar time accepted")
	}
}

// Code that reads records uses one Go type per kind; a v2 payload fills
// the v2 members and leaves the Git ones empty.
func TestV2DecodesIntoDomainTypes(t *testing.T) {
	ex := readExamples(t)
	parse := func(key string) Object {
		o, err := Parse(ex[key])
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	var w schema.Workspace
	if err := parse("v2/workspace").Decode(&w); err != nil || w.BaseTree == "" || w.BaseCommit != "" || w.BaseLineAdvance == nil {
		t.Fatalf("workspace %+v, %v", w, err)
	}
	var m schema.ChangeManifest
	if err := parse("v2/change_manifest").Decode(&m); err != nil || m.BaseTree == "" || m.OpHead == "" ||
		len(m.SymbolMaps) != 2 || m.SymbolsRead[0].Revision == "" || m.BaseCommit != "" {
		t.Fatalf("manifest %+v, %v", m, err)
	}
	var a schema.EvidenceAttestation
	if err := parse("v2/evidence_attestation").Decode(&a); err != nil || a.Tree == "" || a.Commit != "" || a.ExecutedTree != "" {
		t.Fatalf("attestation %+v, %v", a, err)
	}
	var r schema.SpeculationRun
	if err := parse("v2/speculation_run").Decode(&r); err != nil || r.BaseTree == "" || r.BaseCommit != "" {
		t.Fatalf("run %+v, %v", r, err)
	}
	var c schema.Capability
	if err := parse("v2/capability").Decode(&c); err != nil || len(c.MayAdvance) != 1 {
		t.Fatalf("capability %+v, %v", c, err)
	}
	var sd schema.SelectionDecision
	if err := parse("v2/selection_decision").Decode(&sd); err != nil || sd.Commit != "" || sd.Manifest == "" {
		t.Fatalf("selection %+v, %v", sd, err)
	}
	var v1 schema.Workspace
	if err := parse("v1/workspace").Decode(&v1); err != nil || v1.BaseCommit == "" || v1.BaseTree != "" {
		t.Fatalf("v1 workspace %+v, %v", v1, err)
	}
	if err := parse("v2/workspace").Decode(&schema.Intent{}); err == nil {
		t.Fatal("a v2 workspace decoded into an intent")
	}
}
