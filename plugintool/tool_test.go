package plugintool

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// vector is a signed plugin the engine and this module both hold, byte for
// byte: the manifest, the file it lists, and one signature by each side (the
// engine's own code, and this module's). ECDSA signatures are not
// reproducible, so the vector fixes the inputs and a signature each side
// made; each side verifies both. The same directory is in the engine's
// repository, where a test runs it through the real trust store and verifier.
const vector = "testdata/sign-vector"

func readFile(t *testing.T, elem ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(elem...))
	require.NoError(t, err)
	return b
}

func TestVectorSignaturesVerify(t *testing.T) {
	t.Parallel()
	manifest := readFile(t, vector, "plugin", ManifestFile)
	for _, who := range []string{"engine", "sdk"} {
		pub, id, err := ParsePublicKey(readFile(t, vector, who+".pub"))
		require.NoError(t, err)
		sig := readFile(t, vector, who+".sig")
		require.NoError(t, VerifySignature(pub, manifest, sig), who)
		require.Contains(t, string(sig), `"keyId": "`+id+`"`)
		// One byte of the manifest breaks it.
		require.Error(t, VerifySignature(pub, append([]byte(" "), manifest...), sig), who)
	}
}

func TestVectorEngineKeyID(t *testing.T) {
	t.Parallel()
	_, id, err := ParsePublicKey(readFile(t, vector, "engine.pub"))
	require.NoError(t, err)
	require.Equal(t, "y61TWsStBtJapfQqmdT9GQ", id, "the engine computed this id from the same key")
}

var sigLayout = regexp.MustCompile(`\A\{\n  "keyId": "[A-Za-z0-9_-]{22}",\n  "alg": "ES256",\n  "sig": "[A-Za-z0-9_-]{86}"\n\}\n\z`)

// The signature file this module writes has the engine's layout: two-space
// indent, keyId, alg and sig in that order, a trailing newline, and 64 bytes
// of base64url without padding.
func TestSignatureFileLayout(t *testing.T) {
	t.Parallel()
	require.Regexp(t, sigLayout, string(readFile(t, vector, "engine.sig")))
	require.Regexp(t, sigLayout, string(readFile(t, vector, "sdk.sig")))
	dir := plugin(t)
	keys, err := KeyGen(t.TempDir())
	require.NoError(t, err)
	priv, err := ReadSigningKey(keys.PrivatePath)
	require.NoError(t, err)
	sigPath, id, err := SignDir(dir, priv)
	require.NoError(t, err)
	require.Equal(t, keys.KeyID, id)
	require.Equal(t, filepath.Join(dir, SignatureFile), sigPath)
	require.Regexp(t, sigLayout, string(readFile(t, sigPath)))
	pub, _, err := ParsePublicKey(readFile(t, keys.PublicPath))
	require.NoError(t, err)
	require.NoError(t, VerifySignature(pub, readFile(t, dir, ManifestFile), readFile(t, sigPath)))
}

// plugin is a folder with a manifest, filled in, and one file.
func plugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "acme"), []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifestText), 0o644))
	_, err := FillManifest(dir)
	require.NoError(t, err)
	return dir
}

const manifestText = `{
  "manifestVersion": 1,
  "name": "acme",
  "version": "1.0.0",
  "endpoint": { "local": { "transport": "jsonrpc-stdio", "command": "bin/acme" } },
  "hooks": { "feed.observe": { "contract": "feed/1" } },
  "network": { "egress": [] }
}
`

func TestKeyGen(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "keys")
	keys, err := KeyGen(dir)
	require.NoError(t, err)
	require.Len(t, keys.KeyID, 22)
	require.Equal(t, filepath.Join(dir, keys.KeyID+".key"), keys.PrivatePath)
	fi, err := os.Stat(keys.PrivatePath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	priv, err := ReadSigningKey(keys.PrivatePath)
	require.NoError(t, err)
	id, err := KeyID(&priv.PublicKey)
	require.NoError(t, err)
	require.Equal(t, keys.KeyID, id)
	_, pubID, err := ParsePublicKey(readFile(t, keys.PublicPath))
	require.NoError(t, err)
	require.Equal(t, keys.KeyID, pubID)
}

func TestKeyGenNeverOverwrites(t *testing.T) {
	t.Parallel()
	keys, err := KeyGen(t.TempDir())
	require.NoError(t, err)
	before := readFile(t, keys.PrivatePath)
	// The exclusive create is what keeps a key id that already exists safe.
	require.ErrorContains(t, writeExclusive(keys.PrivatePath, []byte("x"), 0o600), "never overwrites")
	require.Equal(t, before, readFile(t, keys.PrivatePath))
}

func TestReadSigningKeyRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keys, err := KeyGen(dir)
	require.NoError(t, err)

	require.NoError(t, os.Chmod(keys.PrivatePath, 0o644))
	_, err = ReadSigningKey(keys.PrivatePath)
	require.ErrorContains(t, err, "chmod 600")
	require.NoError(t, os.Chmod(keys.PrivatePath, 0o600))

	link := filepath.Join(dir, "link.key")
	require.NoError(t, os.Symlink(keys.PrivatePath, link))
	_, err = ReadSigningKey(link)
	require.Error(t, err)

	notKey := filepath.Join(dir, "no.key")
	require.NoError(t, os.WriteFile(notKey, []byte("hello"), 0o600))
	_, err = ReadSigningKey(notKey)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "hello")

	_, err = ReadSigningKey(keys.PublicPath)
	require.Error(t, err)
}

func TestFillManifestKeepsEveryOtherByte(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "acme"), []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "model.bin"), []byte("weights"), 0o644))
	// Key order, an unknown field and odd spacing stay; a stale files goes.
	in := "{ \"zeta\": 1,\n \"files\": {\"old\": \"sha256:00\"},\n  \"name\":   \"acme\", \"endpoint\": {\"local\": {\"transport\": \"jsonrpc-stdio\", \"command\": \"bin/acme\"}} }\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFile), []byte(in), 0o644))
	paths, err := FillManifest(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"bin/acme", "model.bin"}, paths)
	got := string(readFile(t, dir, ManifestFile))
	require.Contains(t, got, "{ \"zeta\": 1,\n \"files\": {\n    \"bin/acme\": \"sha256:")
	require.Contains(t, got, "  },\n  \"name\":   \"acme\", \"endpoint\"")
	require.NotContains(t, got, "old")
	// The result is what SignDir accepts, and a second run changes nothing.
	again, err := FillManifest(dir)
	require.NoError(t, err)
	require.Equal(t, paths, again)
	require.Equal(t, got, string(readFile(t, dir, ManifestFile)))
}

func TestFillManifestAddsTheKeyWhenMissing(t *testing.T) {
	t.Parallel()
	dir := plugin(t)
	got := readFile(t, dir, ManifestFile)
	require.Regexp(t, `"network": \{ "egress": \[\] \},\n  "files": \{\n    "bin/acme": "sha256:[0-9a-f]{64}"\n  \}\n\}\n\z`, string(got))
	_, head, err := readManifest(dir)
	require.NoError(t, err)
	require.Len(t, head.Files, 1)
	h, err := hashFile(filepath.Join(dir, "bin", "acme"))
	require.NoError(t, err)
	require.Equal(t, h, head.Files["bin/acme"])
}

func TestFillManifestRemovesAStaleSignature(t *testing.T) {
	t.Parallel()
	dir := plugin(t)
	keys, err := KeyGen(t.TempDir())
	require.NoError(t, err)
	priv, err := ReadSigningKey(keys.PrivatePath)
	require.NoError(t, err)
	_, _, err = SignDir(dir, priv)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "acme"), []byte("changed"), 0o755))
	_, err = FillManifest(dir)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dir, SignatureFile))
	// Nothing changed: the signature stays.
	_, _, err = SignDir(dir, priv)
	require.NoError(t, err)
	_, err = FillManifest(dir)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, SignatureFile))
}

func TestFillManifestRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup func(dir string)
		want  string
	}{
		"empty directory": {func(dir string) { _ = os.Mkdir(filepath.Join(dir, "empty"), 0o755) }, "empty directory"},
		"private key": {func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, "k.pem"), []byte("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"), 0o644)
		}, "PEM private key"},
		"dot file": {func(dir string) { _ = os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644) }, "cannot be listed"},
		"symlink": {func(dir string) {
			_ = os.Symlink(filepath.Join(dir, "bin", "acme"), filepath.Join(dir, "link"))
		}, "regular files only"},
		"remote": {func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, ManifestFile), []byte(`{"endpoint": {"remote": {"url": "https://a.example"}}}`), 0o644)
		}, "remote plugin"},
		"no manifest": {func(dir string) { _ = os.Remove(filepath.Join(dir, ManifestFile)) }, "cannot be opened"},
		"not json":    {func(dir string) { _ = os.WriteFile(filepath.Join(dir, ManifestFile), []byte("nope"), 0o644) }, "not valid JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := plugin(t)
			tc.setup(dir)
			_, err := FillManifest(dir)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestSignDirRefusals(t *testing.T) {
	t.Parallel()
	keys, err := KeyGen(t.TempDir())
	require.NoError(t, err)
	priv, err := ReadSigningKey(keys.PrivatePath)
	require.NoError(t, err)

	dir := plugin(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "acme"), []byte("changed"), 0o755))
	_, _, err = SignDir(dir, priv)
	require.ErrorContains(t, err, "run basalt-plugin manifest again")
	require.NoFileExists(t, filepath.Join(dir, SignatureFile))

	bare := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bare, ManifestFile), []byte(manifestText), 0o644))
	_, _, err = SignDir(bare, priv)
	require.ErrorContains(t, err, "lists no files")
}

// The embedded samples are the contract's, as the module's testdata holds
// them; a change there is a change here.
func TestEmbeddedSamplesAreTheContractsSamples(t *testing.T) {
	t.Parallel()
	for embedded, contract := range map[string]string{
		"feed-check-sample.json":    "valid-feed-request-check-sample.json",
		"deliver-check-sample.json": "valid-deliver-request-check-sample.json",
	} {
		got, err := sampleFS.ReadFile("samples/" + embedded)
		require.NoError(t, err)
		require.Equal(t, readFile(t, "..", "testdata", contract), got, embedded)
	}
}
