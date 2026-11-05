package envelope

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
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

func readExamples(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(examplesDir, "*.json"))
	if err != nil || len(files) != len(schema.Kinds) {
		t.Fatalf("found %d examples, want one per kind (%d): %v", len(files), len(schema.Kinds), err)
	}
	out := map[string][]byte{}
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
		out[strings.TrimSuffix(filepath.Base(f), ".json")] = raw
	}
	return out
}

func TestExamplesRoundTrip(t *testing.T) {
	for kind, raw := range readExamples(t) {
		t.Run(kind, func(t *testing.T) {
			o, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if string(o.Envelope.Kind) != kind {
				t.Fatalf("kind %q in %s.json", o.Envelope.Kind, kind)
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
			resealed, err := Seal(o.Envelope.Kind, p, s)
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
		{"intent without criteria", "intent", func(p map[string]any) { p["criteria"] = []any{} }},
		{"check criterion without hash", "intent", func(p map[string]any) {
			v := p["criteria"].([]any)[0].(map[string]any)["verification"].(map[string]any)
			delete(v, "definition_hash")
		}},
		{"judgment with unknown reviewer", "intent", func(p map[string]any) {
			v := p["criteria"].([]any)[1].(map[string]any)["verification"].(map[string]any)
			v["reviewer"] = "robot"
		}},
		{"uppercase hash", "lifecycle_event", func(p map[string]any) { p["creation"] = strings.Repeat("A", 64) }},
		{"bad timestamp", "lifecycle_event", func(p map[string]any) { p["at"] = "2026-11-05 12:00:00" }},
		{"non-UTC timestamp", "lifecycle_event", func(p map[string]any) { p["at"] = "2026-11-05T12:00:00+01:00" }},
		{"unknown state", "lifecycle_event", func(p map[string]any) { p["to_state"] = "merged" }},
		{"wrong id prefix", "workspace", func(p map[string]any) { p["id"] = "int_" + strings.Repeat("a", 26) }},
		{"isolation claimed", "evidence_attestation", func(p map[string]any) { p["isolation"] = "container" }},
		{"unsafe integer", "attempt_record", func(p map[string]any) { p["score"] = int64(canon.MaxSafe) + 1 }},
		{"empty allow list", "capability", func(p map[string]any) { p["allow"] = []any{} }},
		{"duplicate receipt members", "operation_receipt", func(p map[string]any) {
			m := p["members"].([]any)
			p["members"] = append(m, m[0])
		}},
	}
	for _, c := range cases {
		b := mutated(t, ex[c.kind], func(_, p map[string]any) { c.mutate(p) })
		if _, err := Parse(b); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
}

func TestUnsupportedVersionCode(t *testing.T) {
	b := mutated(t, readExamples(t)["root_key"], func(env, _ map[string]any) { env["schema_version"] = 2 })
	if _, err := Parse(b); !apierr.HasCode(err, "unsupported_schema_version") {
		t.Fatalf("Parse = %v, want unsupported_schema_version", err)
	}
}

func TestTamperedBytesFailVerification(t *testing.T) {
	raw := readExamples(t)["intent"]
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
	pretty := readExamples(t)["root_key"]
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
	raw := mutated(t, readExamples(t)["intent"], func(_, p map[string]any) { p["created_at"] = "2026-99-99T99:99:99Z" })
	if _, err := Parse(raw); err == nil {
		t.Fatal("impossible calendar time accepted")
	}
}
