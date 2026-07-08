# mule

`mule` is an encrypted TCP service tunnel over QUIC/UDP.

It connects one or more outbound `mule agent` processes to a `mule server`. Each TCP connection becomes one QUIC bidirectional stream. Targets are always configured locally on the side that dials them; neither side can command the other side to dial an arbitrary host or port.

## Model

There are two roles:

```text
mule server
  listens for QUIC/UDP
  authenticates agents
  owns policy
  owns reverse listeners

mule agent
  connects outbound to the server
  owns forward listeners
  owns reverse targets
```

There are two directions:

```text
forward:
  listener lives on agent
  target lives on server

reverse:
  listener lives on server
  target lives on agent
```

## Install

Download the right release asset from:

```text
https://github.com/espegro/mule/releases
```

For Linux ARM64/AArch64:

```bash
wget -O mule https://github.com/espegro/mule/releases/download/v2.0.0/mule-linux-arm64
chmod +x mule
./mule version
```

Build from source:

```bash
make build
```

## Keys

Each agent should have its own secret:

```bash
install -d -m 0700 /etc/mule
umask 077
mule keygen --out /etc/mule/dgx.key
mule check --secret-file /etc/mule/dgx.key
```

Copy the same secret file to the server and the matching agent.

Secret requirements:

- local file only
- base64 or hex encoded
- at least 32 decoded bytes
- Unix permissions must not be group/world-readable
- generated randomly, not human-written

## Simple Forward Example

Expose Ollama on the agent side while the Ollama service runs on the server side.

Server:

```bash
mule server \
  --listen-udp :4400 \
  --agent dgx=/etc/mule/dgx.key \
  --forward ollama=127.0.0.1:11434
```

Agent:

```bash
mule agent \
  --server server.example.org:4400 \
  --agent-id dgx \
  --secret-file /etc/mule/dgx.key \
  --forward ollama=127.0.0.1:10000
```

Clients on the agent machine can now use:

```text
127.0.0.1:10000 -> QUIC tunnel -> server 127.0.0.1:11434
```

## Simple Reverse Example

Expose SSH on the server side while SSH runs on the agent side.

Server:

```bash
mule server \
  --listen-udp :4400 \
  --agent dgx=/etc/mule/dgx.key \
  --reverse ssh=127.0.0.1:2222
```

Agent:

```bash
mule agent \
  --server server.example.org:4400 \
  --agent-id dgx \
  --secret-file /etc/mule/dgx.key \
  --reverse ssh=127.0.0.1:22
```

Then on the server:

```bash
ssh -p 2222 user@127.0.0.1
```

Flow:

```text
server 127.0.0.1:2222
  -> existing QUIC connection
  -> agent 127.0.0.1:22
```

The agent only needs outbound UDP access to the server.

## Forward And Reverse Together

Server:

```bash
mule server \
  --listen-udp :4400 \
  --agent dgx=/etc/mule/dgx.key \
  --forward ollama=127.0.0.1:11434 \
  --reverse ssh=127.0.0.1:2222
```

Agent:

```bash
mule agent \
  --server server.example.org:4400 \
  --agent-id dgx \
  --secret-file /etc/mule/dgx.key \
  --forward ollama=127.0.0.1:10000 \
  --reverse ssh=127.0.0.1:22
```

## Multiple Agents

When the server has multiple agents, prefix service mappings with `agent-id:`.

```bash
mule server \
  --listen-udp :4400 \
  --agent dgx-1=/etc/mule/dgx-1.key \
  --agent dgx-2=/etc/mule/dgx-2.key \
  --reverse dgx-1:ssh=127.0.0.1:2201 \
  --reverse dgx-2:ssh=127.0.0.1:2202 \
  --forward dgx-1:ollama=127.0.0.1:11434 \
  --forward dgx-2:ollama=127.0.0.1:11434
```

Each agent uses its own secret:

```bash
mule agent \
  --server server.example.org:4400 \
  --agent-id dgx-1 \
  --secret-file /etc/mule/dgx-1.key \
  --reverse ssh=127.0.0.1:22 \
  --forward ollama=127.0.0.1:10001
```

Only one active connection per `agent-id` is accepted.

## Config Files

CLI is good for simple cases. Config files are better for multiple services.

Server config:

```yaml
listen_udp: ":4400"
idle_timeout: 1h
keepalive: 20s

agents:
  dgx:
    secret_file: /etc/mule/dgx.key
    forward:
      ollama: 127.0.0.1:11434
    reverse:
      ssh: 127.0.0.1:2222
```

Run:

```bash
mule server --config /etc/mule/server.yaml
```

Agent config:

```yaml
server: server.example.org:4400
agent_id: dgx
secret_file: /etc/mule/dgx.key
send_client_addr: true

forward:
  ollama: 127.0.0.1:10000

reverse:
  ssh: 127.0.0.1:22
```

Run:

```bash
mule agent --config /etc/mule/agent.yaml
```

## Probe

Probe tests QUIC authentication and service policy.

Forward probe also checks that the server can dial the configured forward target:

```bash
mule probe \
  --server server.example.org:4400 \
  --agent-id dgx \
  --secret-file /etc/mule/dgx.key \
  --direction forward \
  --service ollama
```

Reverse probe checks that the reverse service is authorized for the agent:

```bash
mule probe \
  --server server.example.org:4400 \
  --agent-id dgx \
  --secret-file /etc/mule/dgx.key \
  --direction reverse \
  --service ssh
```

## Timeouts And Limits

Useful defaults:

```text
connect-timeout    10s
handshake-timeout  10s
idle-timeout       5m
keepalive          20s
max-connections    100
max-streams        100
max-pending-dials  20
```

For SSH and Ollama, consider:

```bash
--idle-timeout 1h
--keepalive 20s
```

## Logging

Logs go to stderr and are intended for systemd/journald or another supervisor.

```bash
--log-format text|json
--log-level debug|info|warn|error
```

Connection logs include:

- `agent_id`
- `service`
- `direction`
- `connection_id`
- byte counters
- duration

The agent can optionally send the original TCP client address for forward connections:

```bash
--send-client-addr
```

This is logging metadata only, not an authorization signal.

## Systemd

System service examples:

```text
deployment/systemd/mule-server.service
deployment/systemd/mule-agent.service
```

User-mode agent example:

```text
deployment/systemd/mule-agent.user.service
```

Install a user-mode agent unit as:

```bash
mkdir -p ~/.config/systemd/user ~/.config/mule
cp deployment/systemd/mule-agent.user.service ~/.config/systemd/user/mule-agent.service
systemctl --user daemon-reload
systemctl --user enable --now mule-agent.service
```

To keep the user service running after logout:

```bash
loginctl enable-linger "$USER"
```

## Metrics

Prometheus metrics are disabled by default:

```bash
--metrics-listen 127.0.0.1:9100
```

Metrics avoid high-cardinality labels such as client IP and connection ID.

## Security Model

Each agent secret deterministically derives:

- an internal Ed25519 CA
- an agent identity
- a server identity for that agent

QUIC uses TLS 1.3 with mutual authentication. The system CA store is not used. The verified TLS identity maps to `agent_id`, and policy is enforced as:

```text
verified agent_id + direction + service -> allowed?
```

The control protocol never carries arbitrary target host/port. Service targets are configured locally on the side that dials them:

- forward target is configured on the server
- reverse target is configured on the agent

If the secret is wrong, QUIC/TLS authentication fails before any target dial.

## Limitations

- TCP only.
- No client-selected destinations.
- No stream/session resume after QUIC failure.
- No SOCKS, HTTP CONNECT, UDP forwarding, TUN/TAP, routing, mesh, or automatic NAT traversal.
- One active agent connection per `agent_id`.

## Troubleshooting

`secret file permissions are too open`
: Run `chmod 0600 /etc/mule/*.key`.

Authentication fails
: Check that the server and agent have matching secret files and matching `agent_id`.

Reverse listener accepts and immediately closes
: The agent is offline, the service is not configured on the agent, or the agent target cannot be dialed.

Forward listener accepts and immediately closes
: The server target cannot be dialed or the service is not authorized.

QUIC does not connect
: Check UDP firewall/NAT rules from agent to server.
