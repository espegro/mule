# Security

Report security issues privately to the repository owner. Do not include secrets, private keys, packet captures with payload, or production logs with application data in public issues.

## Secret Handling

`mule` never accepts a shared secret as a command-line argument. Use `--secret-file` with a file that is readable only by the service account:

```bash
chmod 0600 /etc/mule/dgx.key
```

Each agent should have its own secret. Rotate a secret by provisioning a new file on the server and matching agent, then restarting both processes. Mule does not implement networked key rotation.

## Authentication

Each shared secret derives an internal CA and role-specific Ed25519 identities through HKDF-SHA-256. TLS 1.3 mutual authentication is required, and peers are pinned to the expected derived identity. The operating system CA store is not used for tunnel authentication.

The server accepts only configured `agent_id` identities. A valid secret for one agent does not authorize another agent, and each opened stream is checked against the configured direction and service policy.

## Non-Goals

`mule` is not designed for anonymity, traffic obfuscation, generic proxying, VPN routing, or bypassing network policy.
