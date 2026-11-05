// Command m0driver writes records through the RecordStore for the M0
// demo, including crashes and concurrent external changes that the
// ledger CLI cannot produce on its own.
//
//	m0driver write N              write N intents
//	m0driver crash STAGE          write one intent and exit at STAGE
//	m0driver move-ref             write one intent while another writer moves the ref
//	m0driver race-close           close one intent from two writers at once
//	m0driver import-fork          close an intent locally and import a divergent close
//	m0driver hash FILE...         print the canonical hash of each JSON file
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
)

var ctx = context.Background()

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: m0driver write|crash|move-ref|race-close|import-fork|hash"))
	}
	if os.Args[1] == "hash" {
		for _, f := range os.Args[2:] {
			raw, err := os.ReadFile(f)
			check(err)
			c, err := canon.Canonical(raw)
			check(err)
			fmt.Printf("%s  %s\n", canon.Hash(c), f)
		}
		return
	}
	keyDir, err := identity.DefaultKeyDir()
	check(err)
	root, err := identity.LocalKeys{Dir: keyDir}.Root()
	check(err)
	wd, err := os.Getwd()
	check(err)
	repo, err := gitx.Open(ctx, wd)
	check(err)
	d := driver{root: root, repo: repo}

	switch os.Args[1] {
	case "write":
		n, err := strconv.Atoi(arg(2))
		check(err)
		s := d.open(record.Options{})
		for range n {
			d.createIntent(s)
		}
	case "crash":
		stage := arg(2)
		s := d.open(record.Options{Fault: func(st string) error {
			if st == stage {
				fmt.Printf("crashing at stage %q\n", stage)
				os.Exit(3)
			}
			return nil
		}})
		d.createIntent(s)
	case "move-ref":
		var moved bool
		s := d.open(record.Options{Fault: func(st string) error {
			if st == record.StageRefCAS && !moved {
				moved = true
				d.externalCommit(d.group(d.intent()))
				fmt.Println("another writer moved refs/ledger/records before our compare-and-swap")
			}
			return nil
		}})
		d.createIntent(s)
	case "race-close":
		s := d.open(record.Options{})
		id, creation := d.createIntent(s)
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i, reason := range []string{"completed", "abandoned"} {
			w := d.open(record.Options{})
			ev := d.closeEvent(id, creation, reason)
			wg.Go(func() { _, results[i] = w.Do(ctx, d.op("demo.close"), members(ev)) })
		}
		wg.Wait()
		for i, err := range results {
			fmt.Printf("writer %d: %v\n", i+1, orAccepted(err))
		}
	case "import-fork":
		s := d.open(record.Options{})
		id, creation := d.createIntent(s)
		_, err := s.Do(ctx, d.op("demo.close"), members(d.closeEvent(id, creation, "completed")))
		check(err)
		d.externalCommit(d.group(d.closeEvent(id, creation, "abandoned")))
		fmt.Printf("closed %s locally and imported a divergent close for it\n", id)
	default:
		fail(fmt.Errorf("unknown mode %q", os.Args[1]))
	}
}

type driver struct {
	root identity.Signer
	repo *gitx.Repo
}

func (d driver) open(opts record.Options) *record.Store {
	s, err := record.Open(ctx, d.repo, opts)
	check(err)
	return s
}

func (d driver) op(name string) record.Op {
	id := schema.NewID(schema.PrefixOperation)
	return record.Op{ID: id, Name: name, Digest: canon.Hash([]byte(id)), Signer: d.root}
}

func (d driver) seal(kind schema.Kind, payload any) envelope.Object {
	o, err := envelope.Seal(kind, payload, d.root)
	check(err)
	return o
}

func (d driver) intent() envelope.Object {
	return d.seal(schema.KindIntent, schema.Intent{
		ID: schema.NewID(schema.PrefixIntent), Description: "demo intent",
		Criteria: []schema.Criterion{{ID: "review", Statement: "a human reviews it",
			Verification: schema.Verification{Kind: "judgment", Reviewer: "human"}}},
		Source: schema.Source{Kind: "human"}, Owner: d.root.ID(), CreatedAt: now(),
	})
}

func (d driver) createIntent(s *record.Store) (id, creation string) {
	o := d.intent()
	res, err := s.Do(ctx, d.op("demo.intent"), members(o))
	check(err)
	var p schema.Intent
	check(o.Decode(&p))
	fmt.Printf("created %s (receipt %s)\n", p.ID, res.Receipt[:12])
	return p.ID, o.Hash
}

func (d driver) closeEvent(id, creation, reason string) envelope.Object {
	return d.seal(schema.KindLifecycleEvent, schema.LifecycleEvent{
		SubjectKind: "intent", SubjectID: id, Creation: creation,
		FromState: "open", ToState: "closed", Reason: &reason, Actor: d.root.ID(), At: now(),
	})
}

// group seals a receipt over objs, as another Ledger installation would.
func (d driver) group(objs ...envelope.Object) []envelope.Object {
	hashes := []string{}
	for _, o := range objs {
		hashes = append(hashes, o.Hash)
	}
	id := schema.NewID(schema.PrefixOperation)
	receipt := d.seal(schema.KindOperationReceipt, schema.OperationReceipt{
		OperationID: id, Operation: "demo.external", RequestDigest: canon.Hash([]byte(id)),
		Result: []byte(`{}`), Members: hashes, At: now(),
	})
	return append(objs, receipt)
}

// externalCommit adds objs to refs/ledger/records without writing them to
// .ledger/objects, as a fetch from another clone would.
func (d driver) externalCommit(objs []envelope.Object) {
	tip, err := d.repo.ResolveRef(ctx, record.RecordsRef)
	check(err)
	tmp, err := os.MkdirTemp("", "m0driver")
	check(err)
	defer os.RemoveAll(tmp)
	var entries []gitx.TreeEntry
	for _, o := range objs {
		p := filepath.Join(tmp, o.Hash)
		check(os.WriteFile(p, o.Bytes, 0o600))
		blobs, err := d.repo.WriteBlobs(ctx, []string{p})
		check(err)
		entries = append(entries, gitx.TreeEntry{Path: o.Hash[:2] + "/" + o.Hash[2:] + ".json", Blob: blobs[0]})
	}
	tree, err := d.repo.BuildTree(ctx, filepath.Join(tmp, "index"), tip, entries)
	check(err)
	commit, err := d.repo.CommitTree(ctx, tree, []string{tip}, "external records\n")
	check(err)
	check(d.repo.UpdateRefCAS(ctx, record.RecordsRef, commit, tip))
}

func members(objs ...envelope.Object) record.BuildFunc {
	return func(*index.Index) (any, []envelope.Object, error) {
		return map[string]int{"records": len(objs)}, objs, nil
	}
}

func orAccepted(err error) string {
	if err == nil {
		return "accepted"
	}
	return "rejected: " + err.Error()
}

func now() string { return schema.Timestamp(time.Now()) }

func arg(i int) string {
	if len(os.Args) <= i {
		fail(fmt.Errorf("missing argument"))
	}
	return os.Args[i]
}

func check(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "m0driver:", err)
	os.Exit(1)
}
