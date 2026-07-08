# Examples

## PacketPony Ingress

Bind PacketPony publicly, then forward accepted traffic to a loopback-only Mule listener:

```text
client -> PacketPony :3000 -> mule agent 127.0.0.1:3100 -> mule server -> target
```

Run `mule agent`:

```bash
mule agent \
  --server host-a.example.org:4400 \
  --agent-id host-b \
  --secret-file /etc/mule/host-b.key \
  --forward web=127.0.0.1:3100
```
