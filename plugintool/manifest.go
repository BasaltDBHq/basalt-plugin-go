package plugintool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// manifestFileMode is the mode of a rewritten manifest.
const manifestFileMode = 0o644

var errNotAnObject = errors.New("the manifest is not a JSON object")

// manifestHead is the part of the manifest the tools read.
type manifestHead struct {
	Hooks    map[string]json.RawMessage `json:"hooks"`
	Files    map[string]string          `json:"files"`
	Endpoint struct {
		Local *struct {
			Transport string   `json:"transport"`
			Command   string   `json:"command"`
			Args      []string `json:"args"`
		} `json:"local"`
		Remote json.RawMessage `json:"remote"`
	} `json:"endpoint"`
}

func readManifest(dir string) ([]byte, manifestHead, error) {
	p := filepath.Join(dir, ManifestFile)
	b, err := readRegular(p, maxManifestBytes)
	if err != nil {
		return nil, manifestHead{}, err
	}
	var h manifestHead
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, manifestHead{}, fmt.Errorf("%s is not valid JSON of the manifest's shape: %w", p, err)
	}
	return b, h, nil
}

// FillManifest writes the manifest's files object from what the folder
// holds: every file but the manifest and its signature, each with its SHA-256.
// Every other byte of the manifest stays as the author wrote it, in the same
// order. When the manifest changes it removes the old signature, which no
// longer matches. A remote plugin ships no files and is refused. It returns
// the paths listed.
func FillManifest(dir string) ([]string, error) {
	b, head, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	if len(head.Endpoint.Remote) > 0 {
		return nil, errors.New("this is a remote plugin: it ships no files, so there is nothing to hash")
	}
	paths, err := folderFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s holds no file to list besides the manifest", dir)
	}
	files, err := filesObject(dir, paths)
	if err != nil {
		return nil, err
	}
	out, err := replaceFiles(b, files)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, ManifestFile), err)
	}
	if bytes.Equal(out, b) {
		return paths, nil
	}
	if err := writeFileAtomic(dir, ManifestFile, out, manifestFileMode); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(dir, SignatureFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("the old signature %s cannot be removed", filepath.Join(dir, SignatureFile))
	}
	return paths, nil
}

// filesObject is the JSON text of the files object, indented to sit at the
// manifest's top level.
func filesObject(dir string, paths []string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, rel := range paths {
		h, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		buf.WriteString("    " + strconv.Quote(rel) + ": " + strconv.Quote(h))
		if i < len(paths)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("  }")
	return buf.Bytes(), nil
}

// replaceFiles swaps the value of the top-level "files" key for files, or adds
// the key before the closing brace. It touches no other byte.
func replaceFiles(manifest, files []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(manifest))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errNotAnObject
	}
	hasKeys := false
	for dec.More() {
		hasKeys = true
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		if key, _ := kt.(string); key != "files" {
			continue
		}
		end := int(dec.InputOffset())
		start := end - len(value)
		out := append([]byte{}, manifest[:start]...)
		out = append(out, files...)
		return append(out, manifest[end:]...), nil
	}
	closing := bytes.LastIndexByte(manifest, '}')
	if closing < 0 {
		return nil, errNotAnObject
	}
	sep := ","
	if !hasKeys {
		sep = ""
	}
	out := append([]byte{}, bytes.TrimRight(manifest[:closing], " \t\r\n")...)
	out = append(out, []byte(sep+"\n  \"files\": ")...)
	out = append(out, files...)
	out = append(out, []byte("\n}")...)
	return append(out, manifest[closing+1:]...), nil
}
