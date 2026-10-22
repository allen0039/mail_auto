package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVaultAndSecrets(t *testing.T) {
	dir := t.TempDir()
	v, e := Open(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	cipher, e := v.Encrypt("private-password", "account:a")
	if e != nil {
		t.Fatal(e)
	}
	again, e := Open(dir, false)
	if e != nil || again.Fingerprint != v.Fingerprint {
		t.Fatal("key not persisted", e)
	}
	plain, e := again.Decrypt(cipher, "account:a")
	if e != nil || plain != "private-password" {
		t.Fatal("decrypt", e)
	}
	if _, e = v.Decrypt(cipher, "account:b"); e == nil {
		t.Fatal("credential swapped between accounts")
	}
	cipher[len(cipher)-1] ^= 1
	if _, e = v.Decrypt(cipher, "account:a"); e == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	root := t.TempDir()
	if e = os.WriteFile(filepath.Join(root, "qq"), []byte("app-password\n"), 0600); e != nil {
		t.Fatal(e)
	}
	secret, e := ExternalSecret(root, "qq")
	if e != nil || secret != "app-password" {
		t.Fatal(e)
	}
	if _, e = ExternalSecret(root, "../qq"); e == nil {
		t.Fatal("path traversal")
	}
	outside := filepath.Join(t.TempDir(), "private")
	os.WriteFile(outside, []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(root, "escape"))
	if _, e = ExternalSecret(root, "escape"); e == nil {
		t.Fatal("symlink traversal")
	}
}
func TestPassword(t *testing.T) {
	h := Hash("password")
	if !Verify(h, "password") || Verify(h, "other") {
		t.Fatal("password verification")
	}
	if Verify("malformed", "password") {
		t.Fatal("invalid hash accepted")
	}
}
