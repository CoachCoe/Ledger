package identity

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

// TrustFileVersion is the only trust list format this build reads.
const TrustFileVersion = 1

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TrustList is the set of root fingerprints a verifier chooses to trust.
// It is local policy: records, bundles and imports can never add to it.
type TrustList struct {
	labels map[string]string
}

// NewTrustList returns an empty list.
func NewTrustList() *TrustList { return &TrustList{labels: map[string]string{}} }

type trustFile struct {
	Version int          `json:"version"`
	Roots   []trustEntry `json:"roots"`
}

type trustEntry struct {
	Fingerprint string `json:"fingerprint"`
	Label       string `json:"label,omitzero"`
}

// DefaultTrustPath is $XDG_CONFIG_HOME/ledger/trusted-roots.json, or
// ~/.config/ledger/trusted-roots.json.
func DefaultTrustPath() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "ledger", "trusted-roots.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", apierr.New("trust_unavailable", "cannot locate the home directory for the default trust list")
	}
	return filepath.Join(home, ".config", "ledger", "trusted-roots.json"), nil
}

// ValidFingerprint reports whether fp is 64 lowercase hex characters.
func ValidFingerprint(fp string) bool { return fingerprintPattern.MatchString(fp) }

// Add trusts fp, which must be a root key fingerprint.
func (t *TrustList) Add(fp, label string) error {
	if !ValidFingerprint(fp) {
		return apierr.New("invalid_fingerprint", "%q is not a 64 character lowercase hex root fingerprint", fp)
	}
	t.labels[fp] = label
	return nil
}

// Merge trusts every fingerprint in flags (the --trust values).
func (t *TrustList) Merge(flags []string) error {
	for _, fp := range flags {
		if err := t.Add(fp, "flag"); err != nil {
			return err
		}
	}
	return nil
}

// Contains reports whether fp is trusted. A nil list trusts nothing.
func (t *TrustList) Contains(fp string) bool {
	if t == nil {
		return false
	}
	_, ok := t.labels[fp]
	return ok
}

// Fingerprints returns the trusted fingerprints, sorted.
func (t *TrustList) Fingerprints() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.labels))
	for fp := range t.labels {
		out = append(out, fp)
	}
	slices.Sort(out)
	return out
}

// LoadTrustFile reads a trust list. A missing file returns an error that
// satisfies errors.Is(err, fs.ErrNotExist) so callers may treat the default
// path as an empty list.
func LoadTrustFile(path string) (*TrustList, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f trustFile
	if err := json.Unmarshal(raw, &f, json.RejectUnknownMembers(true)); err != nil {
		return nil, apierr.New("trust_file_invalid", "%s: %v", path, err)
	}
	if f.Version != TrustFileVersion {
		return nil, apierr.New("trust_file_invalid", "%s: unsupported trust list version %d", path, f.Version)
	}
	t := NewTrustList()
	for _, e := range f.Roots {
		if err := t.Add(e.Fingerprint, e.Label); err != nil {
			return nil, apierr.New("trust_file_invalid", "%s: %v", path, err)
		}
	}
	return t, nil
}
