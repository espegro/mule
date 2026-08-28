# Examples

Generate a separate secret for each agent before using these configurations:

```bash
sudo install -d -m 0700 /etc/mule
sudo mule keygen --out /etc/mule/lab.key
```

Replace hostnames, secret paths, and target addresses for your lab. Keep listeners on `127.0.0.1` unless other machines must connect directly.

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

`deployment/install-systemd.sh` installs the binary, creates a locked-down `mule` system user, validates and installs an existing YAML config, and enables the service. The generated unit reads `/etc/mule/agent.yaml` or `/etc/mule/server.yaml`; the daemon never runs as root. Config-file mode is recommended; referenced secrets should use absolute paths and must already exist with safe permissions. During installation, every secret is checked as the `mule` service user. Review the result first with `--dry-run`:

```bash
./deployment/install-systemd.sh agent \
  --binary ./bin/mule \
  --config ./combined-agent.yaml \
  --dry-run
```

Remove `--dry-run` and run with `sudo` to install. The installer refuses to replace existing config or units unless `--force` is given, and backs them up before replacement. For small setups it can still generate YAML from inline `--server`, `--agent-id`, `--forward`, and `--reverse` options; run `install-systemd.sh --help` for that compatibility mode.

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
