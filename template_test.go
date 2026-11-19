package basaltplugin

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildCommand compiles one of the module's commands, as an author would.
func buildCommand(t *testing.T, pkg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plugin")
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
	return out
}

// TestTemplateAndExampleAnswerTheCheckSamples: the starter template and the
// README's notifier, built and run as their own processes, answer what basalt
// plugin check sends: initialize with the manifest's hooks, health, the
// sample of each hook they declare, and shutdown, after which they exit 0.
func TestTemplateAndExampleAnswerTheCheckSamples(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	for _, tc := range []struct {
		pkg, name string
		hooks     []string
	}{
		{"./template", "hello-feed", []string{HookFeedObserve}},
		{"./examples/notifier", "hello-notify", []string{HookFeedObserve, HookApprovalDeliver}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(buildCommand(t, tc.pkg))
			cmd.Env = []string{}
			in, err := cmd.StdinPipe()
			require.NoError(t, err)
			out, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			c := &stdioClient{t: t, w: in, r: bufio.NewReaderSize(out, MaxMessageBytes+4096)}

			r := c.call("initialize", `{"protocols":["basalt-plugin/1"],"hooks":["`+strings.Join(tc.hooks, `","`)+
				`"],"config":{},"limits":{"maxMessageBytes":4194304,"maxInFlight":4,"timeoutMs":2000}}`)
			require.Nil(t, r.Error)
			require.Contains(t, string(r.Result), `"name":"`+tc.name+`"`)
			for _, h := range tc.hooks {
				require.Contains(t, string(r.Result), `"`+h+`"`)
			}
			require.JSONEq(t, `{"status":"READY"}`, string(c.call("health", `{}`).Result))

			r = c.call("feed.observe", compact(t, readSample(t, "valid-feed-request-check-sample.json")))
			require.Nil(t, r.Error)
			require.JSONEq(t, `{"lastSeq":2}`, string(r.Result))
			if len(tc.hooks) > 1 {
				r = c.call("approval.deliver", compact(t, readSample(t, "valid-deliver-request-check-sample.json")))
				require.Nil(t, r.Error)
				require.JSONEq(t, `{}`, string(r.Result))
			}

			require.Nil(t, c.call("shutdown", `{}`).Error)
			require.NoError(t, cmd.Wait(), "exit status 0")
		})
	}
}

// TestREADMEShowsTheExample: the notifier in the README is the one in
// examples/notifier, which the build compiles.
func TestREADMEShowsTheExample(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	src, err := os.ReadFile(filepath.Join("examples", "notifier", "main.go"))
	require.NoError(t, err)
	_, program, ok := strings.Cut(string(src), "package main\n")
	require.True(t, ok)
	require.Contains(t, string(readme), "package main\n"+program)
}
