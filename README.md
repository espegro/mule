# mule

`mule` is an encrypted TCP port forwarder over QUIC/UDP.

It is meant for controlled service access between two hosts, for example exposing a local port on one machine and forwarding it through an authenticated QUIC tunnel to a fixed service on another machine.

```text
client
  -> Host B: mule forward
  -> encrypted QUIC over UDP
  -> Host A: mule exit
  -> fixed TCP target
```

`mule` is not a VPN. It does not create TUN/TAP interfaces, change routes, forward arbitrary IP packets, implement SOCKS/HTTP CONNECT, or let clients choose destinations.

## Common Uses

- Forward Ollama from a remote machine to a local-only port.
- Reach SSH on a private host through one UDP opening.
- Forward HTTPS to a fixed internal service.
- Put PacketPony in front of `mule forward` for ingress ACLs, rate limiting, logging, and metrics.

## Build

```bash
make build
```

The binary is written to:

```text
bin/mule
```

## Create A Shared Secret

Generate one high-entropy secret and install the same file on both sides:

```bash
install -d -m 0700 /etc/mule
umask 077
mule keygen --out /etc/mule/b-to-a.key
mule check --secret-file /etc/mule/b-to-a.key
```

OpenSSL works too:

```bash
install -d -m 0700 /etc/mule
umask 077
openssl rand -base64 32 > /etc/mule/b-to-a.key
chmod 0600 /etc/mule/b-to-a.key
```

Secret file requirements:

- Must be read from a local file.
- Must decode as base64 or hex.
- Must decode to at least 32 bytes.
- On Unix, must not be group/world-readable.
- Must be random. Do not use passwords or human-written strings.

## Simple One-Port Tunnel

On Host A, where the target service is reachable:

```bash
mule exit \
  --listen-udp :4400 \
  --secret-file /etc/mule/b-to-a.key \
  --target 127.0.0.1:11434
```

On Host B, where clients connect:

```bash
mule forward \
  --listen-tcp 127.0.0.1:11434 \
  --peer host-a.example.org:4400 \
  --secret-file /etc/mule/b-to-a.key
```

Then clients on Host B can connect to:

```text
127.0.0.1:11434
```

`--listen-tcp` and `--target` are shorthand for the built-in `default` route.

## Multiple Ports In One Tunnel

For Ollama, SSH, and HTTPS over the same QUIC connection:

Host A:

```bash
mule exit \
  --listen-udp :4400 \
  --secret-file /etc/mule/b-to-a.key \
  --route ollama=127.0.0.1:11434 \
  --route ssh=127.0.0.1:22 \
  --route https=127.0.0.1:443 \
  --idle-timeout 1h \
  --keepalive 20s \
  --max-streams 200
```

Host B:

```bash
mule forward \
  --peer host-a.example.org:4400 \
  --secret-file /etc/mule/b-to-a.key \
  --forward-id host-b \
  --listen ollama=127.0.0.1:11434 \
  --listen ssh=127.0.0.1:2222 \
  --listen https=127.0.0.1:8443 \
  --idle-timeout 1h \
  --keepalive 20s \
  --max-connections 200
```

Route IDs may contain letters, numbers, `_`, `.`, and `-`, up to 64 characters.

## Logging

`mule` logs to stderr and is intended to run in the foreground under systemd or another supervisor.

Useful flags:

```bash
--log-format text
--log-format json
--log-level info
--log-level debug
```

`forward` sends non-sensitive connection metadata to `exit` in the `OPEN` control frame:

- `route`
- `forward_id`
- `forward_listener`
- `connection_id`
- optional `source_addr`

Set a stable forward identity:

```bash
--forward-id host-b
```

Include the original TCP client address seen by `forward`:

```bash
--send-client-addr
```

Example exit log:

```text
event=connection_closed role=exit route=ssh connection_id=084ec5fb83b8b2c1 forward_id=host-b forward_listener=127.0.0.1:2222 source_addr=192.0.2.10:53144 duration_ms=918 bytes_client_to_target=4201 bytes_target_to_client=8192
```

`source_addr` is reported by the authenticated forward instance. It is useful for operations logs, but should not be used as a security boundary on `exit`.

## Timeouts And Limits

Defaults are conservative:

```text
connect-timeout    10s
handshake-timeout  10s
idle-timeout       5m
keepalive          20s
max-connections    100
max-streams        100
max-pending-dials  20
```

For SSH and Ollama, long-lived idle sessions are common. Consider:

```bash
--idle-timeout 1h
--keepalive 20s
```

For HTTPS with many clients, raise both sides consistently:

```bash
--max-connections 500
--max-streams 500
```

`exit --max-pending-dials` limits concurrent target dials and protects the target during bursts.

## Metrics

Prometheus metrics are disabled by default. Enable them with:

```bash
--metrics-listen 127.0.0.1:9100
```

The endpoint exports counters and gauges without high-cardinality labels such as client IP, route, stream ID, or connection ID.

## Systemd

Example unit files are included:

```text
deployment/systemd/mule-forward.service
deployment/systemd/mule-exit.service
```

`mule` does not daemonize itself. It runs in the foreground so systemd can supervise it, collect logs, restart it, and send shutdown signals.

## PacketPony In Front

Use PacketPony as the public TCP ingress, then forward accepted traffic to a loopback-only Mule listener:

```text
external clients
  -> PacketPony :3000
  -> mule forward 127.0.0.1:3100
  -> QUIC/UDP to Host A
  -> fixed target route
```

Example:

```bash
mule forward \
  --peer host-a.example.org:4400 \
  --secret-file /etc/mule/b-to-a.key \
  --forward-id host-b \
  --listen web=127.0.0.1:3100
```

## Firewall

Typical firewall rules:

- Allow UDP `4400` from Host B to Host A.
- Allow TCP ingress to PacketPony or `mule forward` only where clients should connect.
- Keep `mule forward` bound to `127.0.0.1` unless it must be exposed directly.
- The target service only needs to be reachable from Host A.

## Security Model

The shared secret deterministically derives:

- an internal Ed25519 CA
- a `forward` identity
- an `exit` identity

QUIC uses TLS 1.3 with mutual authentication. `mule` does not use the system CA store for tunnel authentication. Both sides verify that the peer certificate contains the expected derived Ed25519 public key.

If the secret is wrong:

- QUIC/TLS authentication fails.
- No stream is accepted.
- `exit` does not dial the target.

The forward side cannot request arbitrary destinations. It can only send a route ID, and `exit` maps that route ID to a target configured locally with `--target` or `--route`.

## Limitations

- TCP only.
- Fixed target routes only.
- No client-selected destinations.
- Original client IP is not preserved at the target.
- Original client address can optionally be logged with `--send-client-addr`.
- No stream/session resume after QUIC failure.
- No SOCKS, HTTP CONNECT, UDP forwarding, TUN/TAP, routing, mesh, or automatic NAT traversal.
- One shared secret currently means one shared trust domain. If many independent clients should connect to one exit, per-client secrets and route ACLs should be added.

## Troubleshooting

`secret file permissions are too open`
: Run `chmod 0600 /etc/mule/b-to-a.key`.

Authentication fails
: Make sure both sides have identical secret file contents.

Clients connect and then close immediately
: Check that `exit` can dial the configured `--target` or `--route` target.

QUIC does not connect
: Check UDP firewall/NAT rules from Host B to Host A.

SSH sessions drop when idle
: Increase `--idle-timeout` and keep `--keepalive` enabled.

Too many clients are rejected
: Raise `--max-connections` on `forward` and `--max-streams` on `exit`.
