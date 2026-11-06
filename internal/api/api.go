// Package api defines Ledger's provider-neutral versioned core protocol.
package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

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

func Reply(result any, err error) Response {
	if err != nil {
		return Response{Version: 1, Error: apierr.As(err)}
	}
	return Response{Version: 1, OK: true, Result: result}
}

func Parse(raw []byte) (Request, error) {
	var r Request
	b, err := canon.Canonical(raw)
	if err != nil {
		return r, apierr.New("invalid_json", "%v", err)
	}
	if err := schema.ValidateAPI("request", b); err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	if err := schema.ValidateAPI(r.Operation, r.Input); err != nil {
		return r, err
	}
	return r, nil
}

func (r Request) Digest() (string, error) {
	b, err := canon.Marshal(r)
	if err != nil {
		return "", err
	}
	return canon.Hash(b), nil
}

func ReadOnly(op string) bool {
	return op == "coordination.notifications" || op == "coordination.ack" || op == "system.status" || op == "intent.status" || op == "workspace.status" || op == "evidence.resolve"
}
