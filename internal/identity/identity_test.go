package identity

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

func TestEnsureCreatesOnceWithPrivateModes(t *testing.T) {
	keys := LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	if _, err := keys.Root(); !apierr.HasCode(err, "root_unavailable") {
		t.Fatalf("Root before create = %v", err)
	}
	s, created, err := keys.Ensure()
	if err != nil || !created {
		t.Fatalf("Ensure = %v, %v", created, err)
	}
	if fi, _ := os.Stat(keys.Dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(keys.Dir, rootKeyFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}
	again, created, err := keys.Ensure()
	if err != nil || created || again.ID() != s.ID() {
		t.Fatalf("second Ensure replaced the key: %v %v", created, err)
	}
	if s.ID() != Fingerprint(s.PublicKey()) || len(s.ID()) != 64 {
		t.Fatalf("root id %q is not its fingerprint", s.ID())
	}
	leftovers, _ := filepath.Glob(filepath.Join(keys.Dir, ".root-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestConcurrentEnsureAgreesOnOneKey(t *testing.T) {
	keys := LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	var wg sync.WaitGroup
	ids := make([]string, 16)
	for i := range ids {
		wg.Go(func() {
			s, _, err := keys.Ensure()
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = s.ID()
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different keys returned: %v", ids)
		}
	}
}

func TestInsecureKeyFileRefused(t *testing.T) {
	keys := LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	if _, _, err := keys.Ensure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(keys.Dir, rootKeyFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Root(); !apierr.HasCode(err, "root_key_insecure") {
		t.Fatalf("world-readable key accepted: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Root(); !apierr.HasCode(err, "root_key_malformed") {
		t.Fatalf("malformed key accepted: %v", err)
	}
}

func TestSignatureAndKeyEncoding(t *testing.T) {
	s, err := NewSession("ses_" + strings.Repeat("a", 26))
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("message")
	sig := EncodeSignature(s.Sign(msg))
	if !VerifySignature(s.PublicKey(), msg, sig) {
		t.Fatal("signature does not verify")
	}
	if VerifySignature(s.PublicKey(), []byte("massage"), sig) || VerifySignature(s.PublicKey(), msg, sig+"==") {
		t.Fatal("bad signature verified")
	}
	pub, err := DecodePublicKey(EncodePublicKey(s.PublicKey()))
	if err != nil || !pub.Equal(s.PublicKey()) {
		t.Fatal("public key round trip failed")
	}
	for _, bad := range []string{"", "AAAA", EncodePublicKey(s.PublicKey()) + "="} {
		if _, err := DecodePublicKey(bad); err == nil {
			t.Errorf("DecodePublicKey(%q) accepted", bad)
		}
	}
}

func TestDefaultKeyDirHonorsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if d, _ := DefaultKeyDir(); d != "/x/ledger/keys" {
		t.Fatalf("DefaultKeyDir = %s", d)
	}
}

func TestWritableKeyDirectoryRefused(t *testing.T) {
	keys := LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	if _, _, err := keys.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keys.Dir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Root(); !apierr.HasCode(err, "root_key_insecure") {
		t.Fatalf("unsafe directory accepted: %v", err)
	}
}
