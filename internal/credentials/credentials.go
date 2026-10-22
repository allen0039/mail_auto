package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

func Random(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Hash(password string) string {
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		panic(e)
	}
	h := argon2.IDKey([]byte(password), salt, 2, 32*1024, 1, 32)
	return "a2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(h)
}
func Verify(encoded, password string) bool {
	p := strings.Split(encoded, "$")
	if len(p) != 3 || p[0] != "a2id" {
		return false
	}
	s, e := base64.RawStdEncoding.DecodeString(p[1])
	if e != nil || len(s) != 16 {
		return false
	}
	want, e := base64.RawStdEncoding.DecodeString(p[2])
	if e != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), s, 2, 32*1024, 1, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}
func Digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type Vault struct {
	aead        cipher.AEAD
	Fingerprint string
}

func Open(keyDir string, allowCreate bool) (*Vault, error) {
	if e := os.MkdirAll(keyDir, 0700); e != nil {
		return nil, e
	}
	path := filepath.Join(keyDir, "master.key")
	b, e := os.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) && allowCreate {
		key := make([]byte, 32)
		if _, e = rand.Read(key); e != nil {
			return nil, e
		}
		// A hard link publishes a complete, fsynced key atomically without replacing another initializer's key.
		f, err := os.CreateTemp(keyDir, ".master-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		if _, err = f.Write(key); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err = os.Link(f.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		b, e = os.ReadFile(path)
	}
	if e != nil {
		return nil, fmt.Errorf("master key unavailable; restore the original key volume: %w", e)
	}
	info, e := os.Stat(path)
	if e != nil {
		return nil, e
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("master.key permissions must be 0600 or stricter")
	}
	if len(b) != 32 {
		return nil, errors.New("invalid master key length")
	}
	block, e := aes.NewCipher(b)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	return &Vault{aead: a, Fingerprint: Digest(string(b))}, nil
}
func (v *Vault) Encrypt(plain, context string) ([]byte, error) {
	n := make([]byte, v.aead.NonceSize())
	if _, e := rand.Read(n); e != nil {
		return nil, e
	}
	return v.aead.Seal(n, n, []byte(plain), []byte(context)), nil
}
func (v *Vault) Decrypt(b []byte, context string) (string, error) {
	n := v.aead.NonceSize()
	if len(b) < n {
		return "", errors.New("invalid encrypted credential")
	}
	p, e := v.aead.Open(nil, b[:n], b[n:], []byte(context))
	if e != nil {
		return "", errors.New("credential cannot be decrypted with this key")
	}
	return string(p), nil
}
func ExternalSecret(root, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return "", errors.New("secret must be a filename in secret_dir")
	}
	resolved, e := filepath.EvalSymlinks(filepath.Join(root, name))
	if e != nil {
		return "", errors.New("external secret unavailable")
	}
	base, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", e
	}
	rel, e := filepath.Rel(base, resolved)
	if e != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", errors.New("secret escapes secret_dir")
	}
	f, e := os.Open(resolved)
	if e != nil {
		return "", e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return "", errors.New("secret must be a regular file")
	}
	b, e := io.ReadAll(io.LimitReader(f, 4097))
	if e != nil || len(b) > 4096 {
		return "", errors.New("secret exceeds 4096 bytes")
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("empty external secret")
	}
	return s, nil
}
