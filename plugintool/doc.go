// Package plugintool is the tooling a plugin author runs, as code: make a
// signing key, fill in the manifest's file hashes, sign a plugin folder, and
// check a folder by starting its program and talking to it the way the engine
// does. The basalt-plugin command is a thin shell over it.
//
// Nothing here reads engine.json, the engine's trust store or a database, and
// nothing is sent to the engine. A private key's contents are never printed
// or put in an error.
//
// The signature format is the engine's contract: ECDSA P-256 with SHA-256
// over the context line "basalt-plugin-manifest.1\n" followed by the
// manifest file's exact bytes, written to basalt-plugin.json.sig as
// {"keyId", "alg", "sig"}.
package plugintool
