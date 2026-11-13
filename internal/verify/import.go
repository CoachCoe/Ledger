package verify

import (
	"context"

	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
)

// Import stages, in order. ImportOptions.Fault is called before each.
const (
	StageImportStaged  = "import_staged"
	StageImportRecords = "import_records"
	StageImportStore   = "import_store"
	StageImportOpLogs  = "import_oplogs"
)

// PublishResult is what a Publisher reports.
type PublishResult struct {
	// Written counts record files that were not present before.
	Written int `json:"written"`
	// Indexed counts groups newly indexed.
	Indexed int `json:"indexed"`
	// Detail is the publisher's own report.
	Detail any `json:"detail,omitempty"`
}

// Publisher publishes verified records through the RecordStore (the verify
// package cannot import it). Publishing must be idempotent and must never
// choose between conflicting records.
type Publisher interface {
	Publish(ctx context.Context, objs []envelope.Object) (PublishResult, error)
}

// ImportOptions configures Import.
type ImportOptions struct {
	Limits  Limits
	Trusted *identity.TrustList
	// Fault, if set, is called before each import stage; an error stops the
	// import there, as a crash would.
	Fault func(stage string) error
}

// RefResult is the outcome for one retention ref.
type RefResult struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
	// Status is "created", "present" or "ref_conflict".
	Status string `json:"status"`
	Local  string `json:"local,omitempty"`
}

// ImportReport describes an import. Trust is never granted by importing:
// the verification inside it uses only the local trust list.
type ImportReport struct {
	Verification   *Report       `json:"verification"`
	Refs           []RefResult   `json:"refs"`
	Records        int           `json:"records"`
	AlreadyPresent bool          `json:"already_present"`
	Publish        PublishResult `json:"publish"`
	// Objects counts store objects a v2 bundle added, and OpLogs reports
	// each operation log it carried.
	Objects int           `json:"objects,omitzero"`
	OpLogs  []OpLogResult `json:"oplogs,omitzero"`
}

// OpLogResult is the outcome for one operation log. Status is "created",
// "present" (the local log already starts with every bundled entry) or
// "oplog_conflict", in which case the local log is left alone.
type OpLogResult struct {
	Workspace string `json:"workspace"`
	Head      string `json:"head"`
	Status    string `json:"status"`
}

func importFault(o ImportOptions, stage string) error {
	if o.Fault == nil {
		return nil
	}
	return o.Fault(stage)
}
