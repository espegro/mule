# mule

`mule` is an encrypted TCP service tunnel over QUIC/UDP.

It connects one or more outbound `mule agent` processes to a `mule server`. Each TCP connection becomes one QUIC bidirectional stream. Targets are always configured locally on the side that dials them; neither side can command the other side to dial an arbitrary host or port.


<img width="1693" height="929" alt="ChatGPT Image Jul 11, 2026, 08_46_05 PM" src="https://github.com/user-attachments/assets/a207027a-36d1-46fb-9f66-c112244cec70" />


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
wget -O mule https://github.com/espegro/mule/releases/download/v2.0.5/mule-linux-arm64
chmod +x mule
./mule version
```

Build from source:

```bash
make build
```

### Verify a release

Release checksums are signed with this OpenPGP key:

```text
AF14 E838 23D9 1A8D D2C7 C0A4 50AE 70D7 9132 0122
```

Import the public key from GitHub and verify its fingerprint before trusting it:

```bash
curl -fsSL https://github.com/espegro.gpg | gpg --import
gpg --fingerprint 50AE70D791320122
```

Download the release files, `checksums.txt`, and `checksums.txt.asc` into the same directory, then run:

```bash
gpg --verify checksums.txt.asc checksums.txt
sha256sum -c checksums.txt --ignore-missing
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

## Default Setup: One Server, Multiple Clients

This is the normal Mule deployment. A service such as Ollama listens only on
the server's loopback interface. Each client runs a Mule agent and gets the
same service on a local port:

```text
client-a 127.0.0.1:10000 ─┐
                           ├─ QUIC/UDP → server.example.org:4400
client-b 127.0.0.1:10000 ─┘                │
                                           └─ 127.0.0.1:11434 (Ollama)
```

This direction is called `forward`: the listener is on each agent, while the
target is dialed locally by the server. Both clients can use port `10000`
because they run on different machines.

Each client needs a unique agent ID and unique key. The server stores both
keys; each client receives only its matching key:

```text
server:   /etc/mule/client-a.key and /etc/mule/client-b.key
client-a: /etc/mule/client-a.key
client-b: /etc/mule/client-b.key
```

### Install the agents

Build Mule with `make`, then run this on client A:

```bash
sudo ./deployment/install-systemd.sh agent \
  --binary ./bin/mule \
  --server server.example.org:4400 \
  --agent-id client-a \
  --forward ollama=127.0.0.1:10000
```

Run the same command on client B with `--agent-id client-b`. Each command
creates the `mule` system user, `/etc/mule`, a unique key, `agent.yaml`, and
the systemd service. The agent may reconnect until the server is installed;
that is expected. After the server starts, an agent in maximum backoff can
take up to 60 seconds to reconnect.

Securely copy the generated key from each client to a temporary directory on
the server:

```text
client A /etc/mule/client-a.key -> server /root/mule-keys/client-a.key
client B /etc/mule/client-b.key -> server /root/mule-keys/client-b.key
```

The transfer method is up to you; use SSH/SCP or another secure channel. Do
not send keys through chat or email.

### Install the server

After collecting the keys, run one command on the server:

```bash
sudo ./deployment/install-systemd.sh server \
  --binary ./bin/mule \
  --listen-udp :4400 \
  --agent client-a=/root/mule-keys/client-a.key \
  --agent client-b=/root/mule-keys/client-b.key \
  --forward ollama=127.0.0.1:11434
```

This creates the `mule` user and directories, imports every key as
`mule:mule` with mode `0600`, generates `/etc/mule/server.yaml`, installs the
hardened unit, and starts it. The shared `--forward` policy is added to every
listed agent. Add another `--agent ID=KEYFILE` for each additional client.

No user, directory, key, YAML, or systemd unit needs to be created manually.
Only the secure collection of client keys is outside the installer.

Check the result with:

```bash
systemctl status mule-server
journalctl -u mule-server -f
curl http://127.0.0.1:10000/api/tags  # run on either client
```

Use `--dry-run` on either installer command to inspect generated YAML and the
unit without changing the machine. Existing YAML is still supported for
advanced or customized deployments:

```bash
sudo ./deployment/install-systemd.sh server \
  --binary ./bin/mule --config ./server.yaml
```

Ready-to-copy versions are in `examples/default-server.yaml`,
`examples/default-client-a.yaml`, and `examples/default-client-b.yaml`.

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

Agent reconnects use exponential backoff starting at `reconnect-delay` (1s by default) and capped at one minute. The delay resets after a connection has remained stable for at least one minute.

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

For the default system-wide installation, let the installer generate the
config. On an agent:

```bash
sudo ./deployment/install-systemd.sh agent \
  --binary ./bin/mule \
  --server server.example.org:4400 \
  --agent-id client-a \
  --forward ollama=127.0.0.1:10000
```

The complete multi-agent server command is shown in “Default Setup” above.
Use `--config FILE` when you need settings not covered by the simple flags.
Config secret paths should be absolute and readable by `mule`. Use `--dry-run`
to validate YAML and inspect the generated unit first.

The installer creates a locked system account named `mule`; the daemon does
not run as root. It installs the selected config as `/etc/mule/agent.yaml` or
`/etc/mule/server.yaml`. Referenced secret files must be readable by `mule`
and should be owned by `mule:mule` with mode `0600`.

The static system service examples use the same config paths:

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
cp examples/default-client-a.yaml ~/.config/mule/agent.yaml
# Edit ~/.config/mule/agent.yaml and use a secret path below your home directory.
systemctl --user daemon-reload
systemctl --user enable --now mule-agent.service
```

The user-mode unit runs as your login user, not as the system account `mule`.
For a system-wide installation, use `install-systemd.sh` instead.

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

The same local HTTP listener exposes connection status as JSON:

```bash
curl -s http://127.0.0.1:9100/status
```

Agent status includes `connected`, `agent_id`, and `server`. Server status includes a sorted `connected_agents` list and `configured_agent_count`.

The status and metrics listener has no authentication. Bind it to loopback unless the surrounding network is trusted; Mule logs a warning for non-loopback listeners.

Use `--log-level debug` to log connection attempts, authenticated QUIC peers, stream lifecycle, control frames, and target dials. Debug logs include service and connection identifiers, but never secrets or tunneled payloads.

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
