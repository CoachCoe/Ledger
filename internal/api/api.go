// Package api defines Ledger's provider-neutral versioned core protocol.
package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Version is the api_version this binary answers.
const Version = 2

type Request struct {
	Version     int            `json:"api_version"`
	Operation   string         `json:"operation"`
	OperationID string         `json:"operation_id"`
	SessionID   string         `json:"session_id"`
	Input       jsontext.Value `json:"input"`
}

type Response struct {
	Version int           `json:"api_version"`
	OK      bool          `json:"ok"`
	Result  any           `json:"result,omitzero"`
	Error   *apierr.Error `json:"error,omitempty"`
}

// Reply is ReplyVersion at api_version 1.
func Reply(result any, err error) Response { return ReplyVersion(1, result, err) }

// ReplyVersion answers a request of the given api_version.
func ReplyVersion(version int, result any, err error) Response {
	if err != nil {
		return Response{Version: version, Error: apierr.As(err)}
	}
	return Response{Version: version, OK: true, Result: result}
}

// Parse validates a request against the schemas of its api_version.
func Parse(raw []byte) (Request, error) {
	var r Request
	b, err := canon.Canonical(raw)
	if err != nil {
		return r, apierr.New("invalid_json", "%v", err)
	}
	version := RequestVersion(b)
	if err := schema.ValidateAPIVersion(version, "request", b); err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	if err := schema.ValidateAPIVersion(r.Version, r.Operation, r.Input); err != nil {
		return r, err
	}
	return r, nil
}

// RequestVersion is the api_version a raw request names, for validating
// and answering it; a request that names none is judged as version 1,
// whose schema then rejects it.
func RequestVersion(raw []byte) int {
	var v struct {
		Version int `json:"api_version"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Version == 0 {
		return 1
	}
	return v.Version
}

func (r Request) Digest() (string, error) {
	b, err := canon.Marshal(r)
	if err != nil {
		return "", err
	}
	return canon.Hash(b), nil
}

// ReadOnly reports whether op takes no operation id: it signs nothing, so a
// retry needs no frozen identity.
func ReadOnly(op string) bool { return readOnly[op] }
