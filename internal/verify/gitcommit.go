package verify

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // Git object ids of sha1 repositories
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"slices"
	"strconv"
	"strings"

	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
)

// CommitReport resolves a Git commit exported by Ledger, such as the head
// of a proposal branch under review, to the manifest it names (P2-AC-034).
// Each condition is ok, mismatch or unavailable: Commit that an export
// mapping's fields hash to the commit id, Trailer that its message names
// the manifest, and Tree that the Git tree id computed from the manifest's
// native tree is the commit's tree. None of them rests on who signed the
// mapping.
type CommitReport struct {
	Commit    string `json:"commit"`
	Manifest  string `json:"manifest,omitempty"`
	Mapping   string `json:"mapping,omitempty"`
	CommitID  string `json:"commit_id"`
	Trailer   string `json:"trailer"`
	Tree      string `json:"tree"`
	Integrity string `json:"integrity"`
	Detail    string `json:"detail,omitempty"`
}

// OK reports whether the commit is bound to a manifest that verified.
func (r CommitReport) OK() bool {
	return r.CommitID == BindingOK && r.Trailer == BindingOK && r.Tree == BindingOK && r.Integrity == Verified
}

// ResolveCommit finds the export mapping for sha among src's records and
// checks it against rep's report of the manifest it names.
func ResolveCommit(src *Source, rep *Report, sha string) CommitReport {
	out := CommitReport{Commit: sha, CommitID: BindingUnavailable, Trailer: BindingUnavailable, Tree: BindingUnavailable, Integrity: "unavailable"}
	c := src.Corpus
	for _, h := range sortedKeys(c.Objects) {
		o := c.Objects[h]
		if o.Envelope.Kind != schema.KindBridgeMapping || !c.Committed[h] {
			continue
		}
		m, err := decode[schema.BridgeMapping](o)
		if err != nil || m.GitCommit != sha || m.Direction != "export" || m.Manifest == nil {
			continue
		}
		out.Mapping, out.Manifest = h, *m.Manifest
		out.CommitID = condition(commitID(m) == sha)
		if _, ok := trailer(m.Message, *m.Manifest); ok {
			out.Trailer = BindingOK
		} else {
			out.Trailer = BindingMismatch
		}
		mr := manifestReport(rep, *m.Manifest)
		if mr == nil {
			out.Detail = "the manifest is not in the records"
			return out
		}
		out.Integrity = mr.Integrity
		if mr.Tree != m.NativeTree {
			out.Tree, out.Detail = BindingMismatch, "the mapping's tree is not the manifest's"
			return out
		}
		if src.Objects == nil {
			out.Detail = "no content store to compute the Git tree from"
			return out
		}
		tree, err := gitTreeID(src.Objects, m.NativeTree, len(sha) == 64)
		if err != nil {
			out.Detail = err.Error()
			return out
		}
		out.Tree = condition(tree == m.GitTree)
		return out
	}
	out.Detail = "no export mapping names this commit"
	return out
}

func condition(ok bool) string {
	if ok {
		return BindingOK
	}
	return BindingMismatch
}

func manifestReport(rep *Report, hash string) *ManifestReport {
	for i := range rep.Manifests {
		if rep.Manifests[i].Hash == hash {
			return &rep.Manifests[i]
		}
	}
	return nil
}

var gitModes = map[string]string{store.TypeFile: "100644", store.TypeExecutable: "100755", store.TypeSymlink: "120000", store.TypeGitlink: "160000", store.TypeTree: "40000"}

// gitTreeID is the id Git gives the native tree id, computed from the
// store: entries are ordered as Git orders them, with a tree's name
// compared as if it ended in "/".
func gitTreeID(objects *store.Store, id string, sha256Format bool) (string, error) {
	t, err := objects.ReadTree(id)
	if err != nil {
		return "", err
	}
	entries := slices.Clone(t.Entries)
	key := func(e store.Entry) string {
		if e.Type == store.TypeTree {
			return e.Name + "/"
		}
		return e.Name
	}
	slices.SortFunc(entries, func(a, b store.Entry) int { return strings.Compare(key(a), key(b)) })
	var body bytes.Buffer
	for _, e := range entries {
		var oid string
		switch e.Type {
		case store.TypeTree:
			if oid, err = gitTreeID(objects, e.ID, sha256Format); err != nil {
				return "", err
			}
		case store.TypeGitlink:
			oid = e.ID
		default:
			var data bytes.Buffer
			if err := objects.WriteFile(e.ID, &data); err != nil {
				return "", err
			}
			oid = gitObjectID("blob", data.Bytes(), sha256Format)
		}
		raw, err := hex.DecodeString(oid)
		if err != nil {
			return "", err
		}
		body.WriteString(gitModes[e.Type] + " " + e.Name + "\x00")
		body.Write(raw)
	}
	return gitObjectID("tree", body.Bytes(), sha256Format), nil
}

func gitObjectID(kind string, data []byte, sha256Format bool) string {
	var h hash.Hash
	if sha256Format {
		h = sha256.New()
	} else {
		h = sha1.New() //nolint:gosec // Git's own object id in a sha1 repository
	}
	h.Write([]byte(kind + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
