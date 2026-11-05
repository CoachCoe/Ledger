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
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/lock"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/workspace"
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
	if err := gitx.CheckVersion(ctx); err != nil {
		return nil, err
	}
	repo, err := openRepo(ctx)
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
	if within(keyDir, repo.MainDir) || within(keyDir, repo.CommonDir) || within(keyDir, repo.Dir) {
		return nil, apierr.New("key_dir_in_repository", "the root key directory %s must be outside the repository", keyDir)
	}
	// Also reject key custody inside another working tree or bare repository.
	parent := keyDir
	for {
		if info, err := os.Stat(parent); err == nil && info.IsDir() {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	if _, err := gitx.Run(ctx, parent, nil, nil, "rev-parse", "--absolute-git-dir"); err == nil {
		return nil, apierr.New("key_dir_in_repository", "root keys must be outside every Git repository")
	}
	dir := filepath.Join(repo.MainDir, record.DirName)
	if err := atomicfile.MakeDir(filepath.Join(dir, "runtime"), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lock.Acquire(filepath.Join(dir, "runtime", "init.lock"), false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := ensureExcluded(repo.CommonDir); err != nil {
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
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return nil, readErr
	}
	keys := identity.LocalKeys{Dir: keyDir}
	var root identity.Signer
	created := false
	if prior.RootFingerprint != "" {
		root, err = keys.Root()
		if err == nil && root.ID() != prior.RootFingerprint {
			err = apierr.New("root_mismatch", "repository root does not match the configured key")
		}
	} else {
		root, created, err = keys.Ensure()
	}

	if err != nil {
		return nil, err
	}
	store, err := record.Open(ctx, repo, record.Options{Effect: workspace.GitProvider{Repo: repo}.Apply})
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := store.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := store.Recover(ctx); err != nil {
		return nil, err
	}
	operationID := prior.RootOperationID
	if operationID == "" {
		operationID, err = store.Index.RootOperation(ctx, root.ID())
		if err != nil {
			return nil, err
		}
	}
	if operationID == "" {
		operationID = schema.NewID(schema.PrefixOperation)
	}
	if err := writeConfig(filepath.Join(dir, "config.json"), config{CaseSensitive: caseSensitive, RootFingerprint: root.ID(), RootOperationID: operationID}); err != nil {
		return nil, err
	}
	res, err := publishRootKey(ctx, store, root, operationID)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"repository":       repo.MainDir,
		"ledger_dir":       dir,
		"root_fingerprint": root.ID(),
		"root_key_created": created,
		"root_key_dir":     keyDir,
		"case_sensitive":   caseSensitive,
		"root_key_receipt": res.Receipt,
	}, nil
}

// publishRootKey records the root public key under the repository's frozen
// operation id, avoiding collisions when one human initializes multiple repos.
func publishRootKey(ctx context.Context, s *record.Store, root identity.Signer, operationID string) (*record.Result, error) {
	request, err := canon.Marshal(map[string]string{"operation": "ledger.init", "root": root.ID()})
	if err != nil {
		return nil, err
	}
	sum := canon.Hash(request)
	op := record.Op{ID: operationID, Name: "ledger.init", Digest: sum, Signer: root}
	return s.Do(ctx, op, func(*index.Index) (any, []envelope.Object, error) {
		o, err := envelope.Seal(schema.KindRootKey, schema.RootKey{
			PublicKey: identity.EncodePublicKey(root.PublicKey()),
			CreatedAt: schema.Timestamp(time.Now()),
		}, root)
		if err != nil {
			return nil, nil, err
		}
		return map[string]string{"root_key": o.Hash}, []envelope.Object{o}, nil
	})
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ensureExcluded adds .ledger to the repository's local exclude file,
// preserving existing entries.
func ensureExcluded(commonDir string) error {
	path := filepath.Join(commonDir, "info", "exclude")
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

func openRepo(ctx context.Context) (*gitx.Repo, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return gitx.Open(ctx, wd)
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
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real, nil
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
