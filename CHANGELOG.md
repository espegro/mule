# Changelog

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
