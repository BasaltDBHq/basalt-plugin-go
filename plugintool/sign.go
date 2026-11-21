package plugintool

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sort"
)

// ManifestSigContext is the line that starts every signed message, so a
// manifest signature is never another Basalt signature.
const ManifestSigContext = "basalt-plugin-manifest.1\n"

// SigAlg is the signature algorithm: ECDSA P-256 with SHA-256.
const SigAlg = "ES256"

const (
	sigBytes    = 64 // r ‖ s, 32 bytes each
	scalarBytes = 32
)

// signatureFile is the JSON of basalt-plugin.json.sig, in this order.
type signatureFile struct {
	KeyID string `json:"keyId"`
	Alg   string `json:"alg"`
	Sig   string `json:"sig"`
}

// SignDir signs the manifest in dir with priv and writes
// dir/basalt-plugin.json.sig. Before it signs, it checks that the manifest
// lists no file named the manifest or its signature and that every listed
// file has the hash the manifest states. It returns the signature's path and
// the key id.
func SignDir(dir string, priv *ecdsa.PrivateKey) (sigPath, keyID string, err error) {
	manifest, head, err := readManifest(dir)
	if err != nil {
		return "", "", err
	}
	if len(head.Endpoint.Remote) == 0 && len(head.Files) == 0 {
		return "", "", errors.New("the manifest lists no files; run basalt-plugin manifest first")
	}
	rels := make([]string, 0, len(head.Files))
	for rel := range head.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if rel == ManifestFile || rel == SignatureFile {
			return "", "", errors.New("files must not list the manifest or its signature")
		}
		got, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", "", err
		}
		if got != head.Files[rel] {
			return "", "", fmt.Errorf("%s: its SHA-256 is not the one the manifest states; run basalt-plugin manifest again", filepath.Join(dir, filepath.FromSlash(rel)))
		}
	}
	sig, err := SignManifest(manifest, priv)
	if err != nil {
		return "", "", err
	}
	if keyID, err = KeyID(&priv.PublicKey); err != nil {
		return "", "", err
	}
	if err := writeFileAtomic(dir, SignatureFile, sig, signatureFileMode); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, SignatureFile), keyID, nil
}

// SignManifest signs the manifest's exact bytes and returns the contents of
// the signature file.
func SignManifest(manifest []byte, priv *ecdsa.PrivateKey) ([]byte, error) {
	if priv == nil || priv.Curve != elliptic.P256() {
		return nil, errKeyNotP256
	}
	id, err := KeyID(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	digest := manifestDigest(manifest)
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return nil, err
	}
	sig := make([]byte, sigBytes)
	r.FillBytes(sig[:scalarBytes])
	s.FillBytes(sig[scalarBytes:])
	out, err := json.MarshalIndent(signatureFile{KeyID: id, Alg: SigAlg, Sig: base64.RawURLEncoding.EncodeToString(sig)}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// VerifySignature reports whether sigFile (the contents of a signature file)
// is a valid signature of manifest by pub. An author may run it before
// publishing; this module's tests use it on signatures the engine made.
func VerifySignature(pub *ecdsa.PublicKey, manifest, sigFile []byte) error {
	var f signatureFile
	if err := json.Unmarshal(sigFile, &f); err != nil {
		return errors.New("the signature file is not valid JSON")
	}
	id, err := KeyID(pub)
	if err != nil {
		return err
	}
	if f.Alg != SigAlg || f.KeyID != id {
		return errors.New("the signature names another key or algorithm")
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(f.Sig)
	if err != nil || len(sig) != sigBytes {
		return errors.New("the signature's sig is not 64 bytes of base64url")
	}
	digest := manifestDigest(manifest)
	r := new(big.Int).SetBytes(sig[:scalarBytes])
	s := new(big.Int).SetBytes(sig[scalarBytes:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return errors.New("the signature does not match the manifest")
	}
	return nil
}

func manifestDigest(manifest []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(ManifestSigContext))
	h.Write(manifest)
	var d [sha256.Size]byte
	h.Sum(d[:0])
	return d
}
