// Command basalt-plugin is what a plugin author runs: make a signing key,
// fill in the manifest's file hashes, sign a plugin folder, and check the
// folder by running it the way the engine does.
//
//	basalt-plugin keygen --out DIR
//	basalt-plugin manifest DIR
//	basalt-plugin sign DIR --key FILE
//	basalt-plugin check DIR
//
// It reads no engine.json, trust store or database, and sends the engine
// nothing. Install it with:
//
//	go install github.com/basaltdbhq/basalt-plugin-go/cmd/basalt-plugin@latest
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/basaltdbhq/basalt-plugin-go/plugintool"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const usage = "usage: basalt-plugin keygen --out DIR | manifest DIR | sign DIR --key FILE | check DIR"

var errUsage = errors.New(usage)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	cmds := map[string]func(context.Context, []string, io.Writer, io.Writer) error{
		"keygen": keygen, "manifest": manifest, "sign": sign, "check": check,
	}
	f, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if err := f(ctx, args[1:], stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "basalt-plugin:", err)
		if errors.Is(err, errUsage) {
			return exitUsage
		}
		return exitFail
	}
	return exitOK
}

// parse reads flags that may come before or after the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %w", errUsage, err)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func oneDir(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parse(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", fmt.Errorf("%w: give the plugin's folder", errUsage)
	}
	return filepath.Abs(pos[0])
}

func keygen(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "the directory to write <keyId>.key and <keyId>.pub to (required)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *out == "" || len(pos) != 0 {
		return fmt.Errorf("%w: --out DIR is required", errUsage)
	}
	dir, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	keys, err := plugintool.KeyGen(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "key id %s\npublic key %s\n", keys.KeyID, keys.PublicPath)
	fmt.Fprintf(stderr, "the private key is %s (mode 0600); keep it secret and never share it. Sign a plugin with: basalt-plugin sign <dir> --key %q\n", keys.PrivatePath, keys.PrivatePath)
	return nil
}

func manifest(_ context.Context, args []string, stdout, _ io.Writer) error {
	dir, err := oneDir(flag.NewFlagSet("manifest", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	paths, err := plugintool.FillManifest(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s lists %d files\n", filepath.Join(dir, plugintool.ManifestFile), len(paths))
	return nil
}

func sign(_ context.Context, args []string, stdout, _ io.Writer) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	key := fs.String("key", "", "the private key file to sign with, mode 0600 (required)")
	dir, err := oneDir(fs, args)
	if err != nil {
		return err
	}
	if *key == "" {
		return fmt.Errorf("%w: --key FILE is required", errUsage)
	}
	priv, err := plugintool.ReadSigningKey(*key)
	if err != nil {
		return err
	}
	sigPath, id, err := plugintool.SignDir(dir, priv)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed by key %s\nsignature %s\n", id, sigPath)
	return nil
}

func check(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	dir, err := oneDir(flag.NewFlagSet("check", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	_, err = plugintool.Check(ctx, dir, plugintool.CheckOptions{
		Stderr: stderr,
		Say:    func(format string, a ...any) { fmt.Fprintf(stdout, format, a...) },
	})
	return err
}
