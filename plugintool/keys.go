package plugintool

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// File names and modes.
const (
	// PrivateKeyExt and PublicKeyExt end the two files KeyGen writes.
	PrivateKeyExt = ".key"
	PublicKeyExt  = ".pub"

	privateKeyMode   = 0o600
	publicKeyMode    = 0o644
	keyDirMode       = 0o700
	groupOtherAccess = 0o077
	// maxKeyFileBytes is the engine's cap on a key file.
	maxKeyFileBytes = 16 << 10
	// keyIDBytes: a key id is the first 16 bytes of sha256(SPKI DER).
	keyIDBytes = 16

	pemPublicKey  = "PUBLIC KEY"
	pemPrivateKey = "PRIVATE KEY"
	pemBegin      = "-----BEGIN "
)

var (
	errKeyNotOnePEM = errors.New("the key file must hold exactly one PEM block and nothing else")
	errKeyNotP256   = errors.New("the key is not a P-256 key")
	errKeyEncoding  = errors.New("the key is not a valid key of its PEM type")
)

// KeyPairFiles names the files KeyGen wrote.
type KeyPairFiles struct {
	KeyID       string
	PrivatePath string
	PublicPath  string
}

// KeyGen makes a P-256 key pair and writes DIR/<keyId>.key (mode 0600, PEM
// PKCS#8) and DIR/<keyId>.pub (PEM SubjectPublicKeyInfo). It creates DIR with
// mode 0700 when it is missing and never overwrites a file.
func KeyGen(dir string) (KeyPairFiles, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyPairFiles{}, err
	}
	id, err := KeyID(&priv.PublicKey)
	if err != nil {
		return KeyPairFiles{}, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return KeyPairFiles{}, errKeyEncoding
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return KeyPairFiles{}, errKeyEncoding
	}
	if err := os.MkdirAll(dir, keyDirMode); err != nil {
		return KeyPairFiles{}, fmt.Errorf("key directory %s cannot be created", dir)
	}
	out := KeyPairFiles{KeyID: id, PrivatePath: filepath.Join(dir, id+PrivateKeyExt), PublicPath: filepath.Join(dir, id+PublicKeyExt)}
	if err := writeExclusive(out.PrivatePath, pem.EncodeToMemory(&pem.Block{Type: pemPrivateKey, Bytes: privDER}), privateKeyMode); err != nil {
		return KeyPairFiles{}, err
	}
	if err := writeExclusive(out.PublicPath, pem.EncodeToMemory(&pem.Block{Type: pemPublicKey, Bytes: pubDER}), publicKeyMode); err != nil {
		_ = os.Remove(out.PrivatePath)
		return KeyPairFiles{}, err
	}
	return out, nil
}

// KeyID is the id the engine computes for a key: base64url, no padding, of the
// first 16 bytes of sha256 of the key's DER SubjectPublicKeyInfo.
func KeyID(pub *ecdsa.PublicKey) (string, error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return "", errKeyNotP256
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", errKeyEncoding
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:keyIDBytes]), nil
}

func writeExclusive(p string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists; keygen never overwrites a file", p)
	}
	if err != nil {
		return fmt.Errorf("%s cannot be created", p)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(p)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s cannot be set to mode %o", p, mode)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s cannot be written", p)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s cannot be written", p)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s cannot be written", p)
	}
	ok = true
	return nil
}

// ReadSigningKey reads a private key file. It refuses a symlink, a file that
// is not regular, one owned by another user, and one that group or others can
// read or write. Its errors name the file, never its contents.
func ReadSigningKey(path string) (*ecdsa.PrivateKey, error) {
	f, err := os.OpenFile(path, noFollowFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("key file %s cannot be opened, or is a symlink", path)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("key file %s is not a regular file", path)
	}
	if err := checkOwner(fi); err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	if perm := fi.Mode().Perm(); perm&groupOtherAccess != 0 {
		return nil, fmt.Errorf("key file %s is mode %04o; it must be readable by you only (chmod 600)", path, perm)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("key file %s cannot be read", path)
	}
	priv, err := ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return priv, nil
}

// ParsePrivateKey parses a PEM PKCS#8 P-256 private key.
func ParsePrivateKey(b []byte) (*ecdsa.PrivateKey, error) {
	block, err := onePEMBlock(b, pemPrivateKey)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errKeyEncoding
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, errKeyNotP256
	}
	return priv, nil
}

// ParsePublicKey parses a PEM SubjectPublicKeyInfo P-256 public key and
// returns it with its key id.
func ParsePublicKey(b []byte) (*ecdsa.PublicKey, string, error) {
	block, err := onePEMBlock(b, pemPublicKey)
	if err != nil {
		return nil, "", err
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, "", errKeyEncoding
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, "", errKeyNotP256
	}
	id, err := KeyID(pub)
	return pub, id, err
}

func onePEMBlock(b []byte, typ string) (*pem.Block, error) {
	if len(b) > maxKeyFileBytes || !bytes.HasPrefix(bytes.TrimLeft(b, " \t\r\n"), []byte(pemBegin)) ||
		bytes.Count(b, []byte(pemBegin)) != 1 {
		return nil, errKeyNotOnePEM
	}
	block, rest := pem.Decode(b)
	if block == nil || block.Type != typ || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errKeyNotOnePEM
	}
	return block, nil
}
