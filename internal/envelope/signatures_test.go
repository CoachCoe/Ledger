package envelope

import (
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// testdata/signatures.json is produced by gen-signatures.js with Node's
// Ed25519 and an independent RFC 8785 implementation.
func TestIndependentSignatureFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/signatures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Kind        string `json:"kind"`
		PublicKey   string `json:"public_key"`
		Fingerprint string `json:"fingerprint"`
		Unsigned    string `json:"unsigned"`
		Signature   string `json:"signature"`
		Signed      string `json:"signed"`
		Hash        string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	root := testSigners["34750f98bd59fcfc946da45aaabe933be154a4b5094e1c4abf42866505f3c97e"]
	for _, f := range fixtures {
		pub, err := identity.DecodePublicKey(f.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		if identity.Fingerprint(pub) != f.Fingerprint {
			t.Errorf("%s: fingerprint mismatch", f.Kind)
		}
		o, err := Parse([]byte(f.Signed))
		if err != nil {
			t.Fatalf("%s: %v", f.Kind, err)
		}
		if string(o.Bytes) != f.Signed || o.Hash != f.Hash {
			t.Errorf("%s: canonical bytes or hash differ", f.Kind)
		}
		msg, err := SigningInput(o.Envelope)
		if err != nil {
			t.Fatal(err)
		}
		if string(msg) != SigningPrefix+f.Unsigned {
			t.Errorf("%s: signing input differs:\n got %s\nwant %s", f.Kind, msg, f.Unsigned)
		}
		if err := o.Verify(pub); err != nil {
			t.Errorf("%s: %v", f.Kind, err)
		}
		p, err := o.Payload()
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := Seal(schema.Kind(f.Kind), p, root)
		if err != nil {
			t.Fatal(err)
		}
		if sealed.Envelope.Signature != f.Signature || sealed.Hash != f.Hash {
			t.Errorf("%s: Seal does not reproduce the independent signature and hash", f.Kind)
		}
	}
}
