// Package canon implements Ledger's canonical JSON (RFC 8785, JCS) and
// content addressing.
//
// Ledger accepts a strict subset of JSON so that every implementation
// produces identical bytes (ADR 0002): numbers must be integers in plain
// syntax within ±(2^53−1), object names must be unique after unescaping,
// and strings must be valid Unicode. Strings are never normalized.
package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// MaxSafe is the largest integer magnitude a record may contain.
const MaxSafe = 1<<53 - 1

var (
	ErrNumber   = errors.New("canon: only safe integers in plain syntax are allowed")
	ErrTrailing = errors.New("canon: trailing data after JSON value")
)

// Canonical validates raw against Ledger's input policy and returns its
// canonical bytes.
func Canonical(raw []byte) ([]byte, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	v := jsontext.Value(bytes.Clone(raw))
	if err := v.Canonicalize(); err != nil {
		return nil, fmt.Errorf("canon: %w", err)
	}
	return v, nil
}

// Marshal encodes v as JSON and returns its canonical bytes.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canon: %w", err)
	}
	return Canonical(raw)
}

// Hash returns the lowercase hexadecimal SHA-256 of b.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Validate checks raw against the input policy without canonicalizing it.
// The jsontext decoder's defaults reject duplicate names and invalid UTF-8,
// including unpaired surrogate escapes.
func Validate(raw []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	depth := 0
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return fmt.Errorf("canon: %w", err)
		}
		switch tok.Kind() {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '0':
			if err := checkNumber(tok.String()); err != nil {
				return err
			}
		}
		if depth == 0 {
			break
		}
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return ErrTrailing
	}
	return nil
}

func checkNumber(s string) error {
	digits := s
	if len(digits) > 0 && digits[0] == '-' {
		digits = digits[1:]
	}
	plain := len(digits) > 0 && (digits == "0" || digits[0] != '0')
	for i := 0; plain && i < len(digits); i++ {
		plain = digits[i] >= '0' && digits[i] <= '9'
	}
	if !plain || s == "-0" {
		return fmt.Errorf("%w: %q", ErrNumber, s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > MaxSafe || n < -MaxSafe {
		return fmt.Errorf("%w: %q", ErrNumber, s)
	}
	return nil
}
