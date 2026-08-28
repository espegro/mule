# Examples

The default installer creates users, directories, keys, configs, and systemd
units. The YAML files here are for manual or advanced setups. Replace
hostnames, secret paths, and target addresses for your lab. Keep listeners on
`127.0.0.1` unless other machines must connect directly.

## Default Setup

The default setup is one server-side service shared with multiple client
machines. Both clients listen on their own `127.0.0.1:10000` and reach the
same server-side Ollama service at `127.0.0.1:11434`:

```text
default-client-a :10000 ─┐
                         ├─ QUIC/UDP → default-server → Ollama :11434
default-client-b :10000 ─┘
```

Use these files:

```text
default-server.yaml
default-client-a.yaml
default-client-b.yaml
```

`client-a` and `client-b` require different keys. Each client key must match
the corresponding `secret_file` entry on the server. See the complete
walkthrough under “Default Setup” in the main README.

## Systemd Installer

For the shortest default setup, install each agent first. Its key is generated
at `/etc/mule/ID.key`. After securely collecting the keys on the server, pass
all of them to one server command:

```bash
sudo ./deployment/install-systemd.sh server \
  --binary ./bin/mule \
  --listen-udp :4400 \
  --agent client-a=./client-a.key \
  --agent client-b=./client-b.key \
  --forward ollama=127.0.0.1:11434
```

The installer imports keys safely, generates the multi-agent config, and runs
the daemon as the locked `mule` account. Existing YAML remains available via
`--config FILE`. Use `--dry-run` to inspect generated files, and run
`install-systemd.sh --help` for all options.

## Forward

Expose a server-side web service on the agent at `127.0.0.1:8080`:

```text
client -> agent 127.0.0.1:8080 -> QUIC -> server 127.0.0.1:3000
```

```bash
mule server --config examples/forward-server.yaml
mule agent --config examples/forward-agent.yaml
curl http://127.0.0.1:8080
```

## Reverse

Expose agent-side SSH on the server at `127.0.0.1:2222`:

```text
client -> server 127.0.0.1:2222 -> QUIC -> agent 127.0.0.1:22
```

```bash
mule server --config examples/reverse-server.yaml
mule agent --config examples/reverse-agent.yaml
ssh -p 2222 user@127.0.0.1
```

## Forward And Reverse

Run both directions over one agent connection:

```bash
mule server --config examples/combined-server.yaml
mule agent --config examples/combined-agent.yaml
```

## Multiple Agents

`multi-agent-server.yaml` assigns independent secrets and services to `gpu-1` and `gpu-2`. Start each agent with its matching file:

```bash
mule server --config examples/multi-agent-server.yaml
mule agent --config examples/multi-agent-gpu-1.yaml
mule agent --config examples/multi-agent-gpu-2.yaml
```

The server exposes SSH for the agents on ports 2201 and 2202. Each agent exposes its server-side Ollama target on local port 10001 or 10002.

## Agent To Agent

Mule has no implicit cross-agent routing. `agent-to-agent-server.yaml` explicitly connects Agent A's forward target to Agent B's loopback-only reverse listener:

```text
client -> Agent A :9000 -> QUIC -> server :2202 -> QUIC -> Agent B :8080
```

```bash
mule server --config examples/agent-to-agent-server.yaml
mule agent --config examples/agent-to-agent-a.yaml
mule agent --config examples/agent-to-agent-b.yaml
curl http://127.0.0.1:9000
```

This uses two QUIC streams and a local TCP connection on the server. The `127.0.0.1:2202` listener is intentionally not exposed to the network.

## PacketPony Ingress

Bind PacketPony publicly, then forward accepted traffic to a loopback-only Mule listener using `packetpony-ingress.yaml`:

```text
client -> PacketPony :3000 -> mule agent 127.0.0.1:3100 -> mule server -> target
```

```bash
mule agent \
  --server host-a.example.org:4400 \
  --agent-id host-b \
  --secret-file /etc/mule/host-b.key \
  --forward web=127.0.0.1:3100
```
