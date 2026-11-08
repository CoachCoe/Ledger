package api

import (
	"slices"
	"testing"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestProtocolRejectsInvalidAndUnknownInput(t *testing.T) {
	good := Request{Version: 1, Operation: "session.create", OperationID: schema.NewID(schema.PrefixOperation), Input: []byte(`{}`)}
	raw, err := canon.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Request){
		"version": func(r *Request) { r.Version = 2 }, "operation": func(r *Request) { r.Operation = "unknown" }, "id": func(r *Request) { r.OperationID = "" }, "extra": func(r *Request) { r.Input = []byte(`{"unknown":true}`) }, "malformed": func(r *Request) { r.Input = []byte(`{"model":1}`) },
	} {
		r := good
		mutate(&r)
		raw, err := canon.Marshal(r)
		if err == nil {
			_, err = Parse(raw)
		}
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	raw = []byte(`{"api_version":1,"operation":"system.status","operation_id":"","session_id":"","input":{},"input":{}}`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestOutputSchemasRemainClosed(t *testing.T) {
	for _, where := range []string{"valid", "envelope", "receipt", "domain", "state", "version"} {
		t.Run(where, func(t *testing.T) {
			domain := map[string]any{"workspace_id": schema.NewID(schema.PrefixWorkspace), "state": "closed"}
			result := map[string]any{"operation_id": schema.NewID(schema.PrefixOperation), "receipt": canon.Hash(nil), "members": []string{}, "replayed": false, "result": domain}
			reply := map[string]any{"api_version": 1, "ok": true, "result": result}
			switch where {
			case "envelope":
				reply["extra"] = true
			case "receipt":
				result["extra"] = true
			case "domain":
				domain["extra"] = true
			case "state":
				domain["state"] = "open"
			case "version":
				reply["api_version"] = 2
			}
			raw, err := canon.Marshal(reply)
			if err != nil {
				t.Fatal(err)
			}
			err = schema.ValidateAPI("workspace.close.output", raw)
			if (err == nil) != (where == "valid") {
				t.Fatalf("unexpected output validation: %v", err)
			}
		})
	}
	for _, name := range []string{"workspace.close.output", "system.status.output"} {
		if err := schema.ValidateAPI(name, []byte(`{"api_version":1,"ok":false,"error":{"code":"test","message":"test"}}`)); err != nil {
			t.Fatal(err)
		}
		if err := schema.ValidateAPI(name, []byte(`{"api_version":1,"ok":false,"error":{"code":"test","message":"test","extra":true}}`)); err == nil {
			t.Fatal("unknown error property accepted")
		}
	}
}

func TestReadOnlyOperationsComeFromTheRequestSchema(t *testing.T) {
	ops, err := Operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(readOnly) == 0 {
		t.Fatal("no read-only operations")
	}
	for op := range readOnly {
		if !slices.Contains(ops, op) {
			t.Errorf("read-only %s is not an operation", op)
		}
	}
	for _, op := range []string{"system.status", "history.why", "coordination.ack", "speculation.status"} {
		if !ReadOnly(op) {
			t.Errorf("%s needs no operation id", op)
		}
	}
	for _, op := range []string{"workspace.propose", "speculation.score", "bundle.import", "workspace.write"} {
		if ReadOnly(op) {
			t.Errorf("%s is a mutation", op)
		}
	}
}
