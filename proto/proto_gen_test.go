package proto_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	pluginv1 "github.com/BasaltDBHq/basalt-plugin-go/proto/basalt/plugin/v1"
)

var (
	protoMsgBlock = regexp.MustCompile(`(?m)^message (\w+) \{([^}]*)\}`)
	protoFieldNum = regexp.MustCompile(`(?m)^\s*(repeated\s+)?(map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*(\d+);`)
	protoRPC      = regexp.MustCompile(`(?m)^\s*rpc (\w+)\((\w+)\) returns \((\w+)\);`)
	protoService  = regexp.MustCompile(`(?m)^service (\w+) \{`)
	protoPackage  = regexp.MustCompile(`(?m)^package ([\w.]+);`)
)

// TestGeneratedCodeMatchesProto: the checked-in Go code in this directory was
// generated from the current plugin.proto. It compares the generated file
// descriptor with the .proto read as text: the package, every message with
// its fields (name, number, type, repeated), and the service's methods. CI
// has no protoc, so this is the drift guard; regenerate with `make proto`.
func TestGeneratedCodeMatchesProto(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("basalt", "plugin", "v1", "plugin.proto"))
	require.NoError(t, err)
	text := string(raw)
	fd := pluginv1.File_basalt_plugin_v1_plugin_proto

	pkg := protoPackage.FindStringSubmatch(text)
	require.NotNil(t, pkg)
	require.Equal(t, pkg[1], string(fd.Package()), "package")

	var want, got []string
	for _, m := range protoMsgBlock.FindAllStringSubmatch(text, -1) {
		want = append(want, "message "+m[1])
		for _, f := range protoFieldNum.FindAllStringSubmatch(m[2], -1) {
			want = append(want, fmt.Sprintf("%s.%s=%s %s%s", m[1], f[3], f[4], strings.TrimSpace(f[1]), strings.ReplaceAll(f[2], " ", "")))
		}
	}
	msgs := fd.Messages()
	for i := range msgs.Len() {
		md := msgs.Get(i)
		got = append(got, "message "+string(md.Name()))
		fields := md.Fields()
		for j := range fields.Len() {
			f := fields.Get(j)
			got = append(got, fmt.Sprintf("%s.%s=%d %s", md.Name(), f.Name(), f.Number(), protoTypeName(f)))
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	require.Equal(t, want, got, "the generated messages differ from plugin.proto: run make proto")

	want, got = make([]string, 0, 4), make([]string, 0, 4)
	svc := protoService.FindStringSubmatch(text)
	require.NotNil(t, svc)
	for _, r := range protoRPC.FindAllStringSubmatch(text, -1) {
		want = append(want, fmt.Sprintf("%s.%s(%s) %s", svc[1], r[1], r[2], r[3]))
	}
	svcs := fd.Services()
	for i := range svcs.Len() {
		sd := svcs.Get(i)
		methods := sd.Methods()
		for j := range methods.Len() {
			m := methods.Get(j)
			got = append(got, fmt.Sprintf("%s.%s(%s) %s", sd.Name(), m.Name(), m.Input().Name(), m.Output().Name()))
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	require.Equal(t, want, got, "the generated service differs from plugin.proto: run make proto")
}

// protoTypeName writes a field's type as the .proto spells it.
func protoTypeName(f protoreflect.FieldDescriptor) string {
	if f.IsMap() {
		return "map<" + scalarOrMessage(f.MapKey()) + "," + scalarOrMessage(f.MapValue()) + ">"
	}
	prefix := ""
	if f.Cardinality() == protoreflect.Repeated {
		prefix = "repeated"
	}
	return prefix + scalarOrMessage(f)
}

func scalarOrMessage(f protoreflect.FieldDescriptor) string {
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		md := f.Message()
		if md.ParentFile() != nil && md.ParentFile().Package() != f.ParentFile().Package() {
			return string(md.FullName())
		}
		return string(md.Name())
	case protoreflect.EnumKind:
		return string(f.Enum().Name())
	default:
		return f.Kind().String()
	}
}
