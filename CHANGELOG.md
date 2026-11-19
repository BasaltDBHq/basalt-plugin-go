# Changelog

All notable changes to this module are recorded in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Package `basaltplugin`, the Go SDK for BasaltDB plugins. `Serve` answers the handshake, health and shutdown, holds the 4 MiB message cap and the per-call deadline, and runs at most four hook calls at once, on three transports: JSON-RPC on stdio, JSON-RPC on a Unix socket, and ConnectRPC (gRPC works too), local or over TLS.
- The notifier hooks: `FeedObserver` (`feed.observe`, contract `feed/1`) and `ApprovalDeliverer` (`approval.deliver`, contract `deliver/1`), with typed events and notices and their exact bytes in `Raw`.
- Package `protocol`: the protocol's JSON message types, method names and limits.
- Packages `proto/basalt/plugin/v1` and `pluginv1connect`: the generated `basalt.plugin.v1.PluginService` and its `.proto` file.
- A starter plugin in `template/` and a complete notifier in `examples/notifier`.
