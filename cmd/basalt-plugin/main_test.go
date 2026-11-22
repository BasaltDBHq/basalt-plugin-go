package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// keygen, manifest and sign, end to end, as an author types them, with the
// flag after the folder as the guide writes it.
func TestKeygenManifestSign(t *testing.T) {
	t.Parallel()
	keys := filepath.Join(t.TempDir(), "keys")
	code, out, errOut := runCLI(t, "keygen", "--out", keys)
	require.Equal(t, exitOK, code, errOut)
	id := regexp.MustCompile(`key id (\S+)`).FindStringSubmatch(out)[1]
	require.FileExists(t, filepath.Join(keys, id+".pub"))
	require.NotContains(t, out+errOut, "BEGIN")

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "acme"), []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "basalt-plugin.json"), []byte(`{"name": "acme"}`), 0o644))

	code, out, errOut = runCLI(t, "manifest", dir)
	require.Equal(t, exitOK, code, errOut)
	require.Contains(t, out, "lists 1 files")

	code, out, errOut = runCLI(t, "sign", dir, "--key", filepath.Join(keys, id+".key"))
	require.Equal(t, exitOK, code, errOut)
	require.Contains(t, out, "signed by key "+id)
	require.FileExists(t, filepath.Join(dir, "basalt-plugin.json.sig"))
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"keygen"},
		{"manifest"},
		{"sign", t.TempDir()},
		{"check", "a", "b"},
		{"keygen", "--out"},
	} {
		code, _, errOut := runCLI(t, args...)
		require.Equal(t, exitUsage, code, "%v: %s", args, errOut)
	}
}

func TestFailureIsNotUsage(t *testing.T) {
	t.Parallel()
	code, _, errOut := runCLI(t, "manifest", filepath.Join(t.TempDir(), "missing"))
	require.Equal(t, exitFail, code)
	require.Contains(t, errOut, "basalt-plugin:")
}
