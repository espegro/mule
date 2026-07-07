# Examples

## PacketPony Ingress

Bind PacketPony publicly, then forward accepted traffic to a loopback-only Mule listener:

```text
client -> PacketPony :3000 -> mule forward 127.0.0.1:3100 -> mule exit -> target
```

Run `mule forward`:

```bash
mule forward \
  --peer host-a.example.org:4400 \
  --forward-id host-b \
  --listen web=127.0.0.1:3100 \
  --secret-file /etc/mule/b-to-a.key
```
