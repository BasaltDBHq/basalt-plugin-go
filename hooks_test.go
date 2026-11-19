package basaltplugin

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// probeHook stands in for a hook the SDK learns later: an optional interface
// found by type assertion.
type probeHook interface {
	Probe(ctx context.Context, n int) (int, error)
}

type probePlugin struct{ fullPlugin }

func (*probePlugin) Probe(_ context.Context, n int) (int, error) { return n + 1, nil }

func withProbeHook(t *testing.T) {
	t.Helper()
	old := hookSpecs
	t.Cleanup(func() { hookSpecs = old })
	hookSpecs = append(hookSpecs[:len(hookSpecs):len(hookSpecs)], hookSpec{
		name: "probe.hook",
		declare: func(p Plugin) (HookDecl, bool) {
			_, ok := p.(probeHook)
			return HookDecl{Contract: "probe/1"}, ok
		},
		methods: map[string]func(Plugin) methodFunc{
			"probe": func(p Plugin) methodFunc {
				h := p.(probeHook)
				return func(ctx context.Context, params json.RawMessage) (any, *rpcError) {
					var in struct{ N int }
					if json.Unmarshal(params, &in) != nil {
						return nil, errBadParams
					}
					n, err := h.Probe(ctx, in.N)
					if err != nil {
						return nil, errInternal
					}
					return map[string]int{"n": n}, nil
				}
			},
		},
	})
}

// TestAHookAddedLaterLeavesOtherPluginsAlone: a plugin that implements the new
// hook's interface declares it and gets its methods; one written before the
// hook existed is unchanged and answers "method not found" for it.
func TestAHookAddedLaterLeavesOtherPluginsAlone(t *testing.T) {
	withProbeHook(t)
	req := `{"protocols":["basalt-plugin/1"],"hooks":["probe.hook"],"config":{},"limits":{"timeoutMs":1000}}`

	p := &probePlugin{fullPlugin: *newFull()}
	_, c := serveStdio(t, p)
	r := c.call("initialize", req)
	require.Nil(t, r.Error)
	require.Contains(t, string(r.Result), `"probe.hook":{"contract":"probe/1"}`)
	r = c.call("probe", `{"N":41}`)
	require.JSONEq(t, `{"n":42}`, string(r.Result))

	old := newFull() // implements no probe interface
	_, c = serveStdio(t, old)
	r = c.call("initialize", req)
	require.NotNil(t, r.Error, "it does not implement the hook the engine lists")
	r = c.call("initialize", initParams)
	require.Nil(t, r.Error)
	require.NotContains(t, string(r.Result), "probe.hook")
	require.Equal(t, codeNoMethod, c.call("probe", `{"N":1}`).Error.Code)
}

// TestWithHooksReplacesDiscovery: declaring hooks explicitly is exactly what
// the plugin says, even when it implements more.
func TestWithHooksReplacesDiscovery(t *testing.T) {
	withProbeHook(t)
	p := &probePlugin{fullPlugin: *newFull()}
	_, c := serveStdio(t, p, WithHooks(map[string]HookDecl{}))
	r := c.call("initialize", initParams)
	require.Nil(t, r.Error)
	require.JSONEq(t, `{}`, mustField(t, r.Result, "hooks"))
}

// TestWithHooksServesOnlyTheHooksItWasGiven: a hook the plugin implements but
// WithHooks leaves out answers "method not found"; one it names is served.
func TestWithHooksServesOnlyTheHooksItWasGiven(t *testing.T) {
	withProbeHook(t)
	p := &probePlugin{fullPlugin: *newFull()}

	_, c := serveStdio(t, p, WithHooks(map[string]HookDecl{}))
	require.Equal(t, codeNoMethod, c.call("probe", `{"N":1}`).Error.Code)

	_, c = serveStdio(t, p, WithHooks(map[string]HookDecl{"other.hook": {Contract: "other/1"}}))
	require.Equal(t, codeNoMethod, c.call("probe", `{"N":1}`).Error.Code)

	_, c = serveStdio(t, p, WithHooks(map[string]HookDecl{"probe.hook": {Contract: "probe/1"}}))
	require.JSONEq(t, `{"n":2}`, string(c.call("probe", `{"N":1}`).Result))
}

func mustField(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	return string(m[key])
}

// TestImportsNothingOfTheEngine: the module stands on its own. No file in it,
// tests included, imports a package of the engine's module, so the engine can
// depend on this module and never the other way round.
func TestImportsNothingOfTheEngine(t *testing.T) {
	const engine = "github.com/basaltdbhq/basaltdb/"
	const module = "github.com/basaltdbhq/basalt-plugin-go"
	checked := 0
	require.NoError(t, filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		require.NoError(t, err)
		checked++
		for _, imp := range parsed.Imports {
			got := strings.Trim(imp.Path.Value, `"`)
			require.False(t, strings.HasPrefix(strings.ToLower(got), engine), "%s imports %s", path, got)
		}
		return nil
	}))
	require.Greater(t, checked, 10, "the walk saw the module's files")

	// The template is an author's file: it imports the SDK and the standard library.
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join("template", "main.go"), nil, parser.ImportsOnly)
	require.NoError(t, err)
	found := false
	for _, imp := range parsed.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, ".") {
			require.Equal(t, module, path)
			found = true
		}
	}
	require.True(t, found)
}

// TestSDKReadsNoEnvironmentItself: the only environment access is the injected
// getenv, which Serve calls for SocketEnv alone.
func TestSDKReadsNoEnvironmentItself(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, call := range []string{"os.LookupEnv", "os.Environ", "os.ExpandEnv", "syscall.Getenv"} {
			require.NotContains(t, string(src), call, f)
		}
		if f != "options.go" { // os.Getenv appears once, as the default of getenv
			require.NotContains(t, string(src), "os.Getenv", f)
		}
	}
}
