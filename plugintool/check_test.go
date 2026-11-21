package plugintool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	moduleRoot = ".."
	checkWait  = 20 * time.Second
)

// build compiles one of the module's commands into dir/bin/<name>, as an
// author would.
func build(t *testing.T, pkg, dir, name string) {
	t.Helper()
	out := filepath.Join(dir, "bin", name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
}

// pluginFolder builds the starter template (a feed observer, which takes
// -transport) or the notifier example into a folder, with a manifest for the
// transport, and fills in the hashes.
func pluginFolder(t *testing.T, pkg, name, hooks, transport string) string {
	t.Helper()
	dir := t.TempDir()
	build(t, pkg, dir, name)
	args := ""
	if pkg == "./template" && transport != transportStdio {
		args = `, "args": ["-transport", "` + transport + `"]`
	}
	manifest := `{
  "manifestVersion": 1,
  "name": "` + name + `",
  "version": "0.1.0",
  "publisher": "Test",
  "protocol": ["basalt-plugin/1"],
  "hooks": ` + hooks + `,
  "endpoint": { "local": { "transport": "` + transport + `", "command": "bin/` + name + `"` + args + ` } },
  "network": { "egress": [] }
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644))
	_, err := FillManifest(dir)
	require.NoError(t, err)
	return dir
}

const (
	feedHook   = `{ "feed.observe": { "contract": "feed/1" } }`
	bothHooks  = `{ "feed.observe": { "contract": "feed/1" }, "approval.deliver": { "contract": "deliver/1" } }`
	skipReason = "builds a plugin"
)

func TestCheckEveryTransport(t *testing.T) {
	if testing.Short() {
		t.Skip(skipReason)
	}
	for _, transport := range []string{transportStdio, transportUnix, transportConnect} {
		t.Run(transport, func(t *testing.T) {
			dir := pluginFolder(t, "./template", "hello-feed", feedHook, transport)
			ctx, cancel := context.WithTimeout(context.Background(), checkWait)
			defer cancel()
			var lines []string
			res, err := Check(ctx, dir, CheckOptions{Say: func(f string, a ...any) { lines = append(lines, strings.TrimSpace(fmt.Sprintf(f, a...))) }})
			require.NoError(t, err)
			require.Equal(t, "READY", res.HealthStatus)
			require.Len(t, res.Hooks, 1)
			require.Equal(t, "feed.observe", res.Hooks[0].Hook)
			require.NotNil(t, res.Hooks[0].LastSeq)
			require.EqualValues(t, 2, *res.Hooks[0].LastSeq)
			require.Len(t, lines, 2)
			require.Contains(t, lines[0], "initialize")
			require.Contains(t, lines[1], "feed.observe lastSeq 2")
		})
	}
}

func TestCheckSendsBothNotifierHooks(t *testing.T) {
	if testing.Short() {
		t.Skip(skipReason)
	}
	dir := pluginFolder(t, "./examples/notifier", "hello-notify", bothHooks, transportStdio)
	ctx, cancel := context.WithTimeout(context.Background(), checkWait)
	defer cancel()
	res, err := Check(ctx, dir, CheckOptions{})
	require.NoError(t, err)
	names := make([]string, 0, len(res.Hooks))
	for _, h := range res.Hooks {
		names = append(names, h.Hook)
	}
	require.Equal(t, []string{"approval.deliver", "feed.observe"}, names)
}

func TestCheckRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip(skipReason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkWait)
	defer cancel()

	t.Run("a binary that changed after the manifest was written", func(t *testing.T) {
		dir := pluginFolder(t, "./template", "hello-feed", feedHook, transportStdio)
		f, err := os.OpenFile(filepath.Join(dir, "bin", "hello-feed"), os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString("x")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		_, err = Check(ctx, dir, CheckOptions{})
		require.ErrorContains(t, err, "run basalt-plugin manifest again")
	})
	t.Run("a file the manifest does not list", func(t *testing.T) {
		dir := pluginFolder(t, "./template", "hello-feed", feedHook, transportStdio)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0o644))
		_, err := Check(ctx, dir, CheckOptions{})
		require.ErrorContains(t, err, "is not in the manifest's files")
	})
	t.Run("a manifest that names another version", func(t *testing.T) {
		dir := pluginFolder(t, "./template", "hello-feed", feedHook, transportStdio)
		b := readFile(t, dir, ManifestFile)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFile), bytes.Replace(b, []byte(`"0.1.0"`), []byte(`"9.9.9"`), 1), 0o644))
		_, err := Check(ctx, dir, CheckOptions{})
		require.ErrorContains(t, err, "the manifest says hello-feed 9.9.9")
	})
	t.Run("a manifest that declares a hook the plugin lacks", func(t *testing.T) {
		dir := pluginFolder(t, "./template", "hello-feed", bothHooks, transportStdio)
		_, err := Check(ctx, dir, CheckOptions{})
		require.ErrorContains(t, err, "does not implement")
	})
	t.Run("a remote plugin", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFile), []byte(`{"endpoint": {"remote": {"url": "https://a.example"}}}`), 0o644))
		_, err := Check(ctx, dir, CheckOptions{})
		require.ErrorContains(t, err, "remote plugin")
	})
	t.Run("a socket plugin that exits without listening", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "quiet"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
		manifest := `{"name": "quiet", "version": "1.0.0", "hooks": {"feed.observe": {"contract": "feed/1"}},
  "endpoint": {"local": {"transport": "jsonrpc-unix", "command": "bin/quiet"}}}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644))
		_, err := FillManifest(dir)
		require.NoError(t, err)
		_, err = Check(ctx, dir, CheckOptions{})
		require.ErrorContains(t, err, "exited before it listened")
	})
}
