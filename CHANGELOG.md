# Changelog

## v2.0.1 - 2026-07-10

- Rename systemd examples from `mule-forward`/`mule-exit` to `mule-agent`/`mule-server`.
- Add a user-mode systemd agent example.
- Prefer Go 1.26.5 or newer for builds and require it for releases.
- Bound concurrent probe connections and include probe dials in the global dial limit.
- Prevent `keygen` from overwriting existing files and validate secret file metadata on the opened file.

## v2.0.0

- Replace the old `forward`/`exit` model with `server`/`agent`.
- Add first-class forward and reverse directions:
  - forward: listener on agent, target on server
  - reverse: listener on server, target on agent
- Add per-agent secrets and policy keyed by verified TLS identity.
- Add service-based control protocol with explicit `direction` and `service`.
- Add `mule probe --direction forward|reverse --service SERVICE`.
- Add server and agent YAML config support.
- Drop backward compatibility with the experimental 1.x CLI and config model.
- Add cross-platform release builds for Linux, macOS, and Windows.

## v1.1.0

- Add multi-client `exit` mode with per-client secret files and route ACLs.
- Add YAML config support for `mule exit --config`.
- Add CLI bridge flags for multi-client mode: `--client id=secret-file` and `--route id:route=target`.
- Add `mule probe` to verify QUIC authentication and route authorization from the forward side.
- Log verified `client_id` on exit connections and stream events.
- Document multi-client operation, route ACLs, and probe usage.

## v1.0.1

- Lower Go module requirement to Go 1.24 for source builds on platforms where Go 1.26 toolchain download is unavailable.
- Use `golang.org/x/crypto/hkdf` for broader toolchain compatibility.
- Document one exit accepting multiple forwarders.

## v1.0.0

- Initial release.
- Add encrypted TCP forwarding over QUIC/UDP.
- Add `forward`, `exit`, `keygen`, `check`, and `version` commands.
- Add static multi-route forwarding, mutual TLS authentication from shared secret files, metrics, systemd examples, and integration tests.
