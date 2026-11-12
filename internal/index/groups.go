package index

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"github.com/CoachCoe/Ledger/internal/canon"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Group is an operation receipt with the records it commits. A group is
// accepted state only when the receipt and every member are present and
// verify (ADR 0009).
type Group struct {
	Receipt envelope.Object
	Members []envelope.Object
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Error codes returned by Verify.
const (
	CodeUnavailable = "dependency_unavailable"
	CodeInvalid     = "group_invalid"
)

// Verify checks receipt membership and every signature. Keys come from the
// group itself or from already-indexed groups; an unresolvable key fails
// with CodeUnavailable, anything else with CodeInvalid or
// "signature_invalid".
func (x *Index) Verify(ctx context.Context, g Group) error {
	return verify(ctx, x.conn(), g, x.caseSensitive())
}

func verify(ctx context.Context, q querier, g Group, sensitive bool) error {
	for _, o := range append(slices.Clone(g.Members), g.Receipt) {
		if _, err := envelope.ParseStored(o.Bytes, o.Hash); err != nil {
			return err
		}
		b, err := canon.Marshal(o.Envelope)
		if err != nil {
			return err
		}
		if !bytes.Equal(b, o.Bytes) {
			return apierr.New(CodeInvalid, "object envelope differs from stored bytes")
		}
	}

	if g.Receipt.Envelope.Kind != schema.KindOperationReceipt {
		return apierr.New(CodeInvalid, "%s is not an operation receipt", g.Receipt.Hash)
	}
	var r schema.OperationReceipt
	if err := g.Receipt.Decode(&r); err != nil {
		return err
	}
	have := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		if m.Envelope.Kind == schema.KindOperationReceipt {
			return apierr.New(CodeInvalid, "receipt %s lists another receipt", g.Receipt.Hash)
		}
		have = append(have, m.Hash)
	}
	want := slices.Clone(r.Members)
	slices.Sort(have)
	slices.Sort(want)
	if !slices.Equal(have, want) {
		return apierr.New(CodeInvalid, "receipt %s members do not match the group", g.Receipt.Hash)
	}
	local := map[string]ed25519.PublicKey{}
	// Root keys are self-signed and sessions are root-signed, so resolve
	// them in that order before anything they sign.
	ordered := slices.Clone(g.Members)
	rank := func(o envelope.Object) int {
		switch o.Envelope.Kind {
		case schema.KindRootKey:
			return 0
		case schema.KindSession:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(ordered, func(a, b envelope.Object) int { return rank(a) - rank(b) })
	for _, o := range g.Members {
		if o.Envelope.Kind == schema.KindWorkspaceBinding {
			var b schema.WorkspaceBinding
			if err := o.Decode(&b); err != nil {
				return err
			}
			if isSession(o.Envelope.Signer) || b.Actor != o.Envelope.Signer {
				return unauthorized("workspace recovery requires a root-signed binding")
			}
		}
	}
	for _, o := range append(ordered, g.Receipt) {
		var pub ed25519.PublicKey
		switch o.Envelope.Kind {
		case schema.KindRootKey:
			var p schema.RootKey
			if err := o.Decode(&p); err != nil {
				return err
			}
			k, err := identity.DecodePublicKey(p.PublicKey)
			if err != nil || identity.Fingerprint(k) != o.Envelope.Signer {
				return apierr.New(CodeInvalid, "root key %s is not signed by its own key", o.Hash)
			}
			pub = k
		case schema.KindSession:
			if isSession(o.Envelope.Signer) {
				return apierr.New(CodeInvalid, "session %s is not root-signed", o.Hash)
			}
		}
		if pub == nil {
			var err error
			if pub, err = resolveKey(ctx, q, o.Envelope.Signer, local); err != nil {
				return err
			}
		}
		if err := o.Verify(pub); err != nil {
			return err
		}
		switch o.Envelope.Kind {
		case schema.KindRootKey:
			local[o.Envelope.Signer] = pub
		case schema.KindSession:
			var p schema.Session
			if err := o.Decode(&p); err != nil {
				return err
			}
			k, err := identity.DecodePublicKey(p.PublicKey)
			if err != nil {
				return apierr.New(CodeInvalid, "session %s has a malformed key", p.ID)
			}
			local[p.ID] = k
		}
	}

	// Session authentication includes the root-issued capability binding,
	// regardless of member order and on replay as well as local publication.
	for _, o := range g.Members {
		if o.Envelope.Kind != schema.KindSession {
			continue
		}
		var p schema.Session
		if err := o.Decode(&p); err != nil {
			return err
		}
		var c schema.Capability
		var issuer string
		found := false
		for _, member := range g.Members {
			if member.Envelope.Kind != schema.KindCapability {
				continue
			}
			var candidate schema.Capability
			if err := member.Decode(&candidate); err != nil {
				return err
			}
			if candidate.ID == p.CapabilityID {
				c = candidate
				issuer = member.Envelope.Signer
				found = true
				break
			}
		}
		if !found {
			kind, payload, err := creation(ctx, q, p.CapabilityID)
			if err != nil {
				return err
			}
			if kind != schema.KindCapability {
				return apierr.New(CodeUnavailable, "no capability %s", p.CapabilityID)
			}
			if err := json.Unmarshal(payload, &c); err != nil {
				return err
			}
			if err := q.QueryRowContext(ctx, `SELECT signer FROM creations WHERE entity_id=?`, p.CapabilityID).Scan(&issuer); err != nil {
				return err
			}
		}
		if c.SessionID != p.ID || c.Issuer.Kind != "root" || c.Issuer.Fingerprint != o.Envelope.Signer || issuer != o.Envelope.Signer {
			return unauthorized("session %s does not match its root-issued capability", p.ID)
		}
	}
	return verifyAuthority(ctx, q, g, r, sensitive)
}

func isSession(principal string) bool { return strings.HasPrefix(principal, schema.PrefixSession) }

func resolveKey(ctx context.Context, q querier, signer string, local map[string]ed25519.PublicKey) (ed25519.PublicKey, error) {
	if k, ok := local[signer]; ok {
		return k, nil
	}
	kind := schema.KindRootKey
	if isSession(signer) {
		kind = schema.KindSession
	}
	rows, err := q.QueryContext(ctx, `SELECT payload FROM creations WHERE entity_id = ? AND kind = ?`, signer, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var p struct {
			PublicKey string `json:"public_key"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, err
		}
		if kind != schema.KindRootKey || !slices.Contains(keys, p.PublicKey) {
			keys = append(keys, p.PublicKey)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(keys) {
	case 0:
		return nil, apierr.New(CodeUnavailable, "no key for signer %s", signer)
	case 1:
		return identity.DecodePublicKey(keys[0])
	}
	return nil, apierr.New("conflicting_creation", "signer %s has conflicting keys", signer)
}

// Apply indexes a verified group. Conflicts with already-indexed records
// are recorded on the affected subjects, never resolved by choosing one
// side. Applying a group twice has no further effect.
func (x *Index) Apply(ctx context.Context, g Group) error {
	tx, err := x.conn().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verify(ctx, tx, g, x.caseSensitive()); err != nil {
		return err
	}
	var r schema.OperationReceipt
	if err := g.Receipt.Decode(&r); err != nil {
		return err
	}
	touched := map[string]bool{}
	for _, o := range append(slices.Clone(g.Members), g.Receipt) {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO objects (hash, kind, signer, payload) VALUES (?, ?, ?, ?)`,
			o.Hash, o.Envelope.Kind, o.Envelope.Signer, string(o.Envelope.Payload)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO members (receipt, hash) VALUES (?, ?)`,
			g.Receipt.Hash, o.Hash); err != nil {
			return err
		}
		id, err := index1(ctx, tx, o)
		if err != nil {
			return err
		}
		if id != "" {
			touched[id] = true
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO operations (operation_id, receipt, operation, request_digest, result)
		VALUES (?, ?, ?, ?, ?)`, r.OperationID, g.Receipt.Hash, r.Operation, r.RequestDigest, string(r.Result)); err != nil {
		return err
	}
	// A newly imported incompatible receipt also affects members of the
	// previously indexed receipt, not only this group's members.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT COALESCE(c.entity_id,e.subject_id,b.workspace_id)
 FROM operations op JOIN members m ON m.receipt=op.receipt
 LEFT JOIN creations c ON c.hash=m.hash LEFT JOIN events e ON e.hash=m.hash LEFT JOIN bindings b ON b.hash=m.hash
 WHERE op.operation_id=?`, r.OperationID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if id.Valid {
			touched[id.String] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id := range touched {
		if err := recompute(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// index1 records one object's entity, event, or binding rows and returns
// the subject id whose derived state it affects.
func index1(ctx context.Context, q querier, o envelope.Object) (string, error) {
	switch o.Envelope.Kind {
	case schema.KindLifecycleEvent:
		var e schema.LifecycleEvent
		if err := o.Decode(&e); err != nil {
			return "", err
		}
		_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO events (hash, subject_id, creation, signer, payload) VALUES (?, ?, ?, ?, ?)`,
			o.Hash, e.SubjectID, e.Creation, o.Envelope.Signer, string(o.Envelope.Payload))
		return e.SubjectID, err
	case schema.KindWorkspaceBinding:
		var b schema.WorkspaceBinding
		if err := o.Decode(&b); err != nil {
			return "", err
		}
		_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO bindings (hash, workspace_id, previous, session_id) VALUES (?, ?, ?, ?)`,
			o.Hash, b.WorkspaceID, b.Previous, b.SessionID)
		return b.WorkspaceID, err
	}
	id, err := entityID(o)
	if err != nil || id == "" {
		return "", err
	}
	_, err = q.ExecContext(ctx, `INSERT OR IGNORE INTO creations (entity_id, hash, kind, signer, payload) VALUES (?, ?, ?, ?, ?)`,
		id, o.Hash, o.Envelope.Kind, o.Envelope.Signer, string(o.Envelope.Payload))
	return id, err
}

// entityID returns the stable id a creation record establishes, or "" for
// records that create no entity. A root key's entity id is its fingerprint.
func entityID(o envelope.Object) (string, error) {
	switch o.Envelope.Kind {
	case schema.KindRootKey:
		return o.Envelope.Signer, nil
	case schema.KindIntent, schema.KindCapability, schema.KindSession, schema.KindWorkspace, schema.KindSpeculationRun:
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(o.Envelope.Payload, &p); err != nil {
			return "", err
		}
		return p.ID, nil
	}
	return "", nil
}

// Subject is a lifecycle subject's derived state.
type Subject struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Creation    string `json:"creation"`
	State       string `json:"state"`
	Head        string `json:"head"`
	Resume      string `json:"resume"`
	Owner       string `json:"owner"`
	BindingHead string `json:"binding_head"`
	// Conflict is "" or one of conflicting_creation, lifecycle_fork,
	// binding_fork; a conflicted subject cannot be mutated.
	Conflict string `json:"conflict"`
	Detail   Detail `json:"detail"`
}

// Detail lists the records behind a conflict or rejection.
type Detail struct {
	Creations   []string `json:"creations,omitempty"`
	Fork        []string `json:"fork,omitempty"`
	BindingFork []string `json:"binding_fork,omitempty"`
	Rejected    []string `json:"rejected,omitempty"`
}

func recompute(ctx context.Context, q querier, id string) error {
	rows, err := q.QueryContext(ctx, `SELECT hash, kind, signer, payload FROM creations WHERE entity_id = ? ORDER BY hash`, id)
	if err != nil {
		return err
	}
	type creation struct{ hash, kind, signer, payload string }
	var cs []creation
	for rows.Next() {
		var c creation
		if err := rows.Scan(&c.hash, &c.kind, &c.signer, &c.payload); err != nil {
			rows.Close()
			return err
		}
		if lifecycle.SubjectKind(schema.Kind(c.kind)) != "" {
			cs = append(cs, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cs) == 0 {
		_, err := q.ExecContext(ctx, `DELETE FROM subjects WHERE id = ?`, id)
		return err
	}
	s := Subject{ID: id, Kind: lifecycle.SubjectKind(schema.Kind(cs[0].kind)), Creation: cs[0].hash}
	if len(cs) > 1 {
		s.Conflict = "conflicting_creation"
		for _, c := range cs {
			s.Detail.Creations = append(s.Detail.Creations, c.hash)
		}
		return saveSubject(ctx, q, s)
	}
	c := cs[0]
	events, misattributed, err := loadEvents(ctx, q, id, c.hash)
	if err != nil {
		return err
	}
	st := lifecycle.Derive(s.Kind, events)
	s.State, s.Head, s.Resume = st.State, st.Head, st.Resume
	s.Detail.Fork = st.Fork
	s.Detail.Rejected = slices.Concat(misattributed, st.Rejected)
	slices.Sort(s.Detail.Rejected)
	if len(st.Fork) > 0 {
		s.Conflict = "lifecycle_fork"
	}
	switch s.Kind {
	case lifecycle.Intent:
		var p schema.Intent
		if err := json.Unmarshal([]byte(c.payload), &p); err != nil {
			return err
		}
		s.Owner = p.Owner
	case lifecycle.Session:
		s.Owner = id
	case lifecycle.SpeculationRun:
		s.Owner = c.signer
	case lifecycle.Workspace:
		var p schema.Workspace
		if err := json.Unmarshal([]byte(c.payload), &p); err != nil {
			return err
		}
		s.Owner, s.BindingHead = p.SessionID, c.hash
		if err := deriveBindings(ctx, q, &s); err != nil {
			return err
		}
	}
	return saveSubject(ctx, q, s)
}

// loadEvents returns a subject's events. Events whose actor is not their
// signer are returned separately and never applied.
func loadEvents(ctx context.Context, q querier, subject, creation string) (events []lifecycle.Event, misattributed []string, err error) {
	rows, err := q.QueryContext(ctx, `SELECT hash, signer, payload FROM events WHERE subject_id = ? AND creation = ?`, subject, creation)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e lifecycle.Event
		var signer, payload string
		if err := rows.Scan(&e.Hash, &signer, &payload); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal([]byte(payload), &e.LifecycleEvent); err != nil {
			return nil, nil, err
		}
		if e.Actor != signer {
			misattributed = append(misattributed, e.Hash)
			continue
		}
		if isSession(signer) {
			owner, err := eventOwner(ctx, q, e.LifecycleEvent)
			if err != nil {
				return nil, nil, err
			}
			if owner != signer {
				misattributed = append(misattributed, e.Hash)
				continue
			}
		}
		events = append(events, e)
	}
	return events, misattributed, rows.Err()
}

// deriveBindings follows the workspace's binding chain from its creation
// record; a fork stops at the last unambiguous binding.
func deriveBindings(ctx context.Context, q querier, s *Subject) error {
	rows, err := q.QueryContext(ctx, `SELECT hash, previous, session_id FROM bindings WHERE workspace_id = ?`, s.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type binding struct{ hash, session string }
	next := map[string][]binding{}
	for rows.Next() {
		var b binding
		var prev string
		if err := rows.Scan(&b.hash, &prev, &b.session); err != nil {
			return err
		}
		next[prev] = append(next[prev], b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for {
		children := next[s.BindingHead]
		if len(children) == 0 {
			return nil
		}
		if len(children) > 1 {
			for _, c := range children {
				s.Detail.BindingFork = append(s.Detail.BindingFork, c.hash)
			}
			slices.Sort(s.Detail.BindingFork)
			if s.Conflict == "" {
				s.Conflict = "binding_fork"
			}
			return nil
		}
		s.BindingHead, s.Owner = children[0].hash, children[0].session
	}
}

func saveSubject(ctx context.Context, q querier, s Subject) error {
	var conflicts int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM members m JOIN operations op ON op.receipt=m.receipt
 WHERE m.hash IN (SELECT hash FROM creations WHERE entity_id=? UNION SELECT hash FROM events WHERE subject_id=? UNION SELECT hash FROM bindings WHERE workspace_id=?)
 AND op.operation_id IN (SELECT operation_id FROM operations GROUP BY operation_id HAVING COUNT(*)>1)`, s.ID, s.ID, s.ID).Scan(&conflicts)
	if err != nil {
		return err
	}
	if conflicts > 0 {
		s.Conflict = "operation_conflict"
	}

	detail, err := json.Marshal(s.Detail)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT OR REPLACE INTO subjects
		(id, kind, creation, state, head, resume, owner, binding_head, conflict, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Kind, s.Creation, s.State, s.Head, s.Resume, s.Owner, s.BindingHead, s.Conflict, string(detail))
	return err
}

// Subject returns a subject's derived state, or nil if it does not exist.
func (x *Index) Subject(ctx context.Context, id string) (*Subject, error) {
	return loadSubject(ctx, x.conn(), id)
}

type subjectScanner interface{ Scan(...any) error }

func scanSubject(row subjectScanner) (*Subject, error) {
	var s Subject
	var detail string
	err := row.Scan(&s.ID, &s.Kind, &s.Creation, &s.State, &s.Head, &s.Resume, &s.Owner, &s.BindingHead, &s.Conflict, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, json.Unmarshal([]byte(detail), &s.Detail)
}

const subjectColumns = "id,kind,creation,state,head,resume,owner,binding_head,conflict,detail"

func loadSubject(ctx context.Context, q querier, id string) (*Subject, error) {
	return scanSubject(q.QueryRowContext(ctx, `SELECT `+subjectColumns+` FROM subjects WHERE id=?`, id))
}

// Subjects reads one SQLite statement snapshot, avoiding partial N+1
// reads or nil dereferences if another process reindexes concurrently.
func (x *Index) Subjects(ctx context.Context) ([]Subject, error) {
	return x.subjects(ctx, `SELECT `+subjectColumns+` FROM subjects ORDER BY id`)
}

// SubjectsIn returns the subjects of kind currently in state.
func (x *Index) SubjectsIn(ctx context.Context, kind, state string) ([]Subject, error) {
	return x.subjects(ctx, `SELECT `+subjectColumns+` FROM subjects WHERE kind=? AND state=? ORDER BY id`, kind, state)
}

func (x *Index) subjects(ctx context.Context, query string, args ...any) ([]Subject, error) {
	rows, err := x.conn().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subject{}
	for rows.Next() {
		s, err := scanSubject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// Operation is an indexed operation receipt.
type Operation struct {
	Receipt       string
	Operation     string
	RequestDigest string
	Result        string
}

// Operations returns the receipts indexed for an operation id. More than
// one means an imported receipt conflicts with a local one.
func (x *Index) Operations(ctx context.Context, operationID string) ([]Operation, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT receipt, operation, request_digest, result FROM operations
		WHERE operation_id = ? ORDER BY receipt`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		var o Operation
		if err := rows.Scan(&o.Receipt, &o.Operation, &o.RequestDigest, &o.Result); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// HasReceipt reports whether a receipt's group is indexed.
func (x *Index) HasReceipt(ctx context.Context, receipt string) (bool, error) {
	var n int
	err := x.conn().QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE receipt = ?`, receipt).Scan(&n)
	return n > 0, err
}

// Creation returns the payload of an entity's unique creation record. It
// fails with "conflicting_creation" if there is more than one, and returns
// "" if there is none.
func (x *Index) Creation(ctx context.Context, id string) (kind schema.Kind, payload []byte, err error) {
	return creation(ctx, x.conn(), id)
}

func creation(ctx context.Context, q querier, id string) (schema.Kind, []byte, error) {
	rows, err := q.QueryContext(ctx, `SELECT kind, payload FROM creations WHERE entity_id = ?`, id)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var kind, payload string
	n := 0
	for rows.Next() {
		if err := rows.Scan(&kind, &payload); err != nil {
			return "", nil, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if n > 1 && kind != string(schema.KindRootKey) {
		return "", nil, apierr.New("conflicting_creation", "%s has %d creation records", id, n)
	}
	if n == 0 {
		return "", nil, nil
	}
	return schema.Kind(kind), []byte(payload), nil
}

// Committed reports whether an object belongs to an indexed group.
func (x *Index) Committed(ctx context.Context, hash string) (bool, error) {
	var n int
	err := x.conn().QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE hash = ?`, hash).Scan(&n)
	return n > 0, err
}

// ConflictingOperations returns operation ids with more than one receipt.
func (x *Index) ConflictingOperations(ctx context.Context) (map[string][]string, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT operation_id, receipt FROM operations WHERE operation_id IN
		(SELECT operation_id FROM operations GROUP BY operation_id HAVING COUNT(*) > 1) ORDER BY operation_id, receipt`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, receipt string
		if err := rows.Scan(&id, &receipt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], receipt)
	}
	return out, rows.Err()
}

// Receipts lists the receipts of every indexed group, sorted.
func (x *Index) Receipts(ctx context.Context) ([]string, error) {
	rows, err := x.conn().QueryContext(ctx, `SELECT receipt FROM operations ORDER BY receipt`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
