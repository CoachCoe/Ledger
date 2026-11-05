package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

const rootKeyFile = "root.key"

// LocalKeys keeps the root private key as a file in Dir.
type LocalKeys struct {
	Dir string
}

// DefaultKeyDir is $XDG_CONFIG_HOME/ledger/keys, or ~/.config/ledger/keys.
func DefaultKeyDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "ledger", "keys"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ledger", "keys"), nil
}

// Root loads the existing root key. It fails with code "root_unavailable"
// if there is none.
func (l LocalKeys) Root() (Signer, error) {
	priv, err := l.load()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apierr.New("root_unavailable", "no root key in %s; run ledger init", l.Dir)
	}
	if err != nil {
		return nil, err
	}
	return RootSigner(priv), nil
}

// Ensure loads the root key, creating it first if none exists. It never
// replaces an existing key: if another process creates one concurrently,
// that key wins and is returned.
func (l LocalKeys) Ensure() (signer Signer, created bool, err error) {
	if s, err := l.Root(); !apierr.HasCode(err, "root_unavailable") {
		return s, false, err
	}
	if err := atomicfile.MakeDir(l.Dir, 0o700); err != nil {
		return nil, false, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	tmp, err := os.CreateTemp(l.Dir, ".root-*.tmp")
	if err != nil {
		return nil, false, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(b64.EncodeToString(priv.Seed()) + "\n")
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, false, err
	}
	// A hard link fails if the target exists, so a concurrent creator's
	// key is never overwritten.
	if err := os.Link(tmp.Name(), filepath.Join(l.Dir, rootKeyFile)); errors.Is(err, fs.ErrExist) {
		s, err := l.Root()
		return s, false, err
	} else if err != nil {
		return nil, false, err
	}
	if err := syncDir(l.Dir); err != nil {
		return nil, false, err
	}
	return RootSigner(priv), true, nil
}

func (l LocalKeys) load() (ed25519.PrivateKey, error) {
	dir, err := os.Lstat(l.Dir)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, apierr.New("root_key_insecure", "%s must be a private directory", l.Dir)
	}

	path := filepath.Join(l.Dir, rootKeyFile)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, apierr.New("root_key_insecure", "%s must be a regular file readable only by its owner", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := b64.Strict().DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, apierr.New("root_key_malformed", "%s is not a Ledger root key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func syncDir(path string) error { return atomicfile.SyncDir(path) }
