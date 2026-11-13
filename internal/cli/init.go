package cli

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/cow"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/lock"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
	"github.com/CoachCoe/Ledger/internal/symbols"
)

const excludeLine = "/" + record.DirName + "/"

// config is local, per-repository settings written by init.
type config struct {
	CaseSensitive   bool   `json:"case_sensitive"`
	RootFingerprint string `json:"root_fingerprint"`
	RootOperationID string `json:"root_operation_id"`
}

func cmdInit(args []string) (result any, err error) {
	if err := noFlags("init", args); err != nil {
		return nil, err
	}
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err := initRoot(ctx, wd)
	if err != nil {
		return nil, err
	}
	keyDir, err := identity.DefaultKeyDir()
	if err != nil {
		return nil, err
	}
	keyDir, err = resolvePath(keyDir)
	if err != nil {
		return nil, err
	}
	if within(keyDir, root) {
		return nil, apierr.New("key_dir_in_repository", "the root key directory %s must be outside the repository", keyDir)
	}
	if inside, err := insideRepository(keyDir); err != nil {
		return nil, err
	} else if inside != "" {
		return nil, apierr.New("key_dir_in_repository", "root keys must be outside every repository; %s is inside %s", keyDir, inside)
	}
	dir := filepath.Join(root, record.DirName)
	if err := atomicfile.MakeDir(filepath.Join(dir, "runtime"), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lock.Acquire(filepath.Join(dir, "runtime", "init.lock"), false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := ensureExcluded(root); err != nil {
		return nil, err
	}
	caseSensitive, err := probeCaseSensitive(filepath.Join(dir, "runtime"))
	if err != nil {
		return nil, err
	}
	// An initialized repository must never silently rotate identity if its
	// key has disappeared or the caller changes XDG_CONFIG_HOME.
	var prior config
	raw, readErr := os.ReadFile(filepath.Join(dir, "config.json"))
	if readErr == nil {
		if err := json.Unmarshal(raw, &prior, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		caseSensitive = prior.CaseSensitive
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return nil, readErr
	}
	keys := identity.LocalKeys{Dir: keyDir}
	var signer identity.Signer
	created := false
	if prior.RootFingerprint != "" {
		signer, err = keys.Root()
		if err == nil && signer.ID() != prior.RootFingerprint {
			err = apierr.New("root_mismatch", "repository root does not match the configured key")
		}
	} else {
		signer, created, err = keys.Ensure()
	}
	if err != nil {
		return nil, err
	}
	objects, err := store.Open(filepath.Join(dir, record.StoreDir, "objects"), caseSensitive)
	if err != nil {
		return nil, err
	}
	s, err := record.OpenNative(ctx, root, record.Options{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := s.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := s.Recover(ctx); err != nil {
		return nil, err
	}
	operationID := prior.RootOperationID
	if operationID == "" {
		operationID, err = s.Index.RootOperation(ctx, signer.ID())
		if err != nil {
			return nil, err
		}
	}
	if operationID == "" {
		operationID = schema.NewID(schema.PrefixOperation)
	}
	if err := writeConfig(filepath.Join(dir, "config.json"), config{CaseSensitive: caseSensitive, RootFingerprint: signer.ID(), RootOperationID: operationID}); err != nil {
		return nil, err
	}
	workspaces, err := cow.New(cow.Config{Store: objects, Runtime: filepath.Join(dir, "runtime"), Logs: filepath.Join(dir, "oplog"), CaseSensitive: caseSensitive})
	if err != nil {
		return nil, err
	}
	res, err := createRepository(ctx, s, signer, operationID, caseSensitive, func() (string, error) { return workspaces.Import(root) })
	if err != nil {
		return nil, err
	}
	var made struct {
		Repository string `json:"repository"`
		Tree       string `json:"tree"`
	}
	if err := json.Unmarshal(res.Result, &made); err != nil {
		return nil, err
	}
	return map[string]any{
		"repository":       root,
		"repository_id":    made.Repository,
		"tree":             made.Tree,
		"ledger_dir":       dir,
		"root_fingerprint": signer.ID(),
		"root_key_created": created,
		"root_key_dir":     keyDir,
		"case_sensitive":   caseSensitive,
		"root_key_receipt": res.Receipt,
	}, nil
}

// initRoot is where init creates or completes a repository: the native
// repository dir is already in, or dir itself. A Phase 1 repository is
// never written again.
func initRoot(ctx context.Context, dir string) (string, error) {
	repo, err := record.Locate(ctx, dir)
	if err == nil && !repo.Native() {
		return "", migrateRequired(repo.Root)
	}
	if err == nil {
		return repo.Root, nil
	}
	if e, ok := errors.AsType[*apierr.Error](err); !ok || e.Code != "not_initialized" {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}

func migrateRequired(root string) error {
	return apierr.New("migrate_required", "%s is a Phase 1 repository; this binary only reads and verifies it until ledger migrate converts it", root)
}

// createRepository publishes the root public key and the repository record
// under the repository's frozen operation id, so a repeated init, even after
// the index and config are lost, replays the first instead of creating a
// second repository. The tree is imported only when the record is new.
func createRepository(ctx context.Context, s *record.Store, root identity.Signer, operationID string, caseSensitive bool, tree func() (string, error)) (*record.Result, error) {
	request, err := canon.Marshal(map[string]string{"operation": "ledger.init", "root": root.ID()})
	if err != nil {
		return nil, err
	}
	op := record.Op{ID: operationID, Name: "ledger.init", Digest: canon.Hash(request), Signer: root}
	return s.Do(ctx, op, func(*index.Index) (any, []envelope.Object, error) {
		t, err := tree()
		if err != nil {
			return nil, nil, err
		}
		now := schema.Timestamp(time.Now())
		key, err := envelope.SealVersion(2, schema.KindRootKey, schema.RootKey{PublicKey: identity.EncodePublicKey(root.PublicKey()), CreatedAt: now}, root)
		if err != nil {
			return nil, nil, err
		}
		policy := "sensitive"
		if !caseSensitive {
			policy = "insensitive"
		}
		id := schema.NewID(schema.PrefixRepository)
		repo, err := envelope.SealVersion(2, schema.KindRepository, schema.Repository{ID: id, HashAlgorithm: "sha256",
			CasePolicy: policy, Analyzers: symbols.Languages(), Tree: t, CreatedAt: now}, root)
		if err != nil {
			return nil, nil, err
		}
		return map[string]string{"root_key": key.Hash, "repository": id, "tree": t}, []envelope.Object{key, repo}, nil
	})
}

// insideRepository returns the nearest ancestor of path that holds a Git or
// Ledger directory, or "" if there is none.
func insideRepository(path string) (string, error) {
	for d := path; ; {
		for _, name := range []string{".git", record.DirName} {
			if _, err := os.Lstat(filepath.Join(d, name)); err == nil {
				return d, nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", nil
		}
		d = parent
	}
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ensureExcluded keeps .ledger out of a Git repository sharing root, by
// adding it to the local exclude file and preserving existing entries. Only
// a plain .git directory is handled: Ledger reads no Git configuration.
func ensureExcluded(root string) error {
	fi, err := os.Stat(filepath.Join(root, ".git"))
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
		return nil
	}
	if err != nil {
		return err
	}
	path := filepath.Join(root, ".git", "info", "exclude")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for line := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(line) == excludeLine {
			return nil
		}
	}
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		existing = append(existing, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // matches the mode Git gives .git/info
		return err
	}
	return atomicfile.Write(path, append(existing, excludeLine+"\n"...), 0o644) //nolint:gosec // Git's own exclude file mode
}

// probeCaseSensitive reports whether the filesystem holding dir
// distinguishes file names that differ only in case.
func probeCaseSensitive(dir string) (bool, error) {
	f, err := os.CreateTemp(dir, "Case-Probe-*")
	if err != nil {
		return false, err
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	lower := filepath.Join(dir, strings.ToLower(filepath.Base(name)))
	_, err = os.Stat(lower)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, err
}

// readConfig reads the config init wrote into ledgerDir; a repository
// without one is case sensitive.
func readConfig(ledgerDir string) (config, error) {
	c := config{CaseSensitive: true}
	raw, err := os.ReadFile(filepath.Join(ledgerDir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(raw, &c, json.RejectUnknownMembers(true))
}

func writeConfig(path string, c config) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	return atomicfile.Write(path, b, 0o600)
}

// locate finds the repository the working directory belongs to.
func locate(ctx context.Context) (record.Repository, error) {
	wd, err := os.Getwd()
	if err != nil {
		return record.Repository{}, err
	}
	return record.Locate(ctx, wd)
}

// locateNative locates a repository the daemon can serve: a Phase 1 one
// is read-only until it is migrated.
func locateNative(ctx context.Context) (record.Repository, error) {
	repo, err := locate(ctx)
	if err == nil && !repo.Native() {
		return repo, migrateRequired(repo.Root)
	}
	return repo, err
}

// resolvePath resolves existing ancestors even when the final directory
// has not been created yet, preventing an alias into the repository.
func resolvePath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}
