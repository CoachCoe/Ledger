// Package envelope seals, parses, and verifies signed Ledger records
// (ADR 0007).
package envelope

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// SigningPrefix returns the literal prepended to the canonical unsigned
// envelope to form the Ed25519 signing message for a schema_version.
func SigningPrefix(version int) string {
	return fmt.Sprintf("ledger-record-v%d\n", version)
}

// Object is a sealed record: its canonical bytes and their SHA-256 hash.
// The hash is the record's object id; it is never part of the record.
type Object struct {
	Hash     string
	Bytes    []byte
	Envelope schema.Envelope
}

// SigningInput returns the exact bytes signed for env.
func SigningInput(env schema.Envelope) ([]byte, error) {
	env.Signature = ""
	b, err := canon.Marshal(env)
	if err != nil {
		return nil, err
	}
	return append([]byte(SigningPrefix(env.SchemaVersion)), b...), nil
}

// Seal encodes payload as a record of the given kind at schema.Current,
// signed by s.
func Seal(kind schema.Kind, payload any, s identity.Signer) (Object, error) {
	return SealVersion(schema.Current, kind, payload, s)
}

// SealVersion is Seal at an explicit schema_version.
func SealVersion(version int, kind schema.Kind, payload any, s identity.Signer) (Object, error) {
	p, err := canon.Marshal(payload)
	if err != nil {
		return Object{}, err
	}
	env := schema.Envelope{SchemaVersion: version, Kind: kind, Payload: p, Signer: s.ID()}
	msg, err := SigningInput(env)
	if err != nil {
		return Object{}, err
	}
	env.Signature = identity.EncodeSignature(s.Sign(msg))
	b, err := canon.Marshal(env)
	if err != nil {
		return Object{}, err
	}
	if err := schema.ValidateEnvelope(b); err != nil {
		return Object{}, err
	}
	return Object{Hash: canon.Hash(b), Bytes: b, Envelope: env}, nil
}

// Parse validates raw against the canonical JSON policy and the record
// schemas and returns the canonical object. It does not check the
// signature; see Verify.
func Parse(raw []byte) (Object, error) {
	b, err := canon.Canonical(raw)
	if err != nil {
		return Object{}, apierr.New("record_malformed", "%v", err)
	}
	if err := schema.ValidateEnvelope(b); err != nil {
		return Object{}, err
	}
	var env schema.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Object{}, apierr.New("record_malformed", "%v", err)
	}
	return Object{Hash: canon.Hash(b), Bytes: b, Envelope: env}, nil
}

// ParseStored parses bytes read from the object store, which must already
// be canonical and hash to want.
func ParseStored(raw []byte, want string) (Object, error) {
	if canon.Hash(raw) != want {
		return Object{}, apierr.New("record_hash_mismatch", "content does not hash to %s", want)
	}
	o, err := Parse(raw)
	if err != nil {
		return Object{}, err
	}
	if !bytes.Equal(o.Bytes, raw) {
		return Object{}, apierr.New("record_not_canonical", "stored record %s is not canonical", want)
	}
	return o, nil
}

// Verify checks the signature against pub.
func (o Object) Verify(pub ed25519.PublicKey) error {
	msg, err := SigningInput(o.Envelope)
	if err != nil {
		return err
	}
	if !identity.VerifySignature(pub, msg, o.Envelope.Signature) {
		return apierr.New("signature_invalid", "signature on %s does not verify", o.Hash)
	}
	return nil
}

// Decode unmarshals the payload into v, which must be the payload type for
// the object's kind. Unknown members are rejected.
func (o Object) Decode(v any) error {
	if err := json.Unmarshal(o.Envelope.Payload, v, json.RejectUnknownMembers(true)); err != nil {
		return apierr.New("record_malformed", "%s payload: %v", o.Envelope.Kind, err)
	}
	return nil
}

// Payload decodes the payload into a new value of its kind's type.
func (o Object) Payload() (any, error) {
	v := schema.PayloadType(o.Envelope.SchemaVersion, o.Envelope.Kind)
	if v == nil {
		return nil, fmt.Errorf("envelope: unknown kind %q at schema_version %d", o.Envelope.Kind, o.Envelope.SchemaVersion)
	}
	return v, o.Decode(v)
}
