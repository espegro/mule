# Mule: kryptert TCP-portforwarding over QUIC/UDP

## Oppgave

Lag et nytt, separat Go-program kalt **`mule`** (eventuelt `packetmule` dersom navnekollisjon oppstår).

Programmet skal lage en kryptert userspace-tunnel mellom to hoster. Det primære bruksområdet er kontrollert TCP-portforwarding:

```text
Klient
  |
  | TCP til Host B:3000
  v
Host B: mule forward
  |
  | kryptert QUIC over UDP
  v
Host A: mule exit
  |
  | TCP til fast konfigurert tjeneste
  v
target-host:target-port
```

Dette skal **ikke** være en generell VPN-løsning og skal ikke opprette TUN-interface, endre routing eller forsøke å reimplementere WireGuard.

## Plassering og avgrensning

Lag dette som et **eget repository/program**, ikke som en del av PacketPony i første versjon.

PacketPony skal fortsatt kunne brukes foran `mule forward` som TCP-ingress for ACL, rate limiting, logging og metrics:

```text
Eksterne klienter
  |
  v
PacketPony :3000
  |
  v
mule forward 127.0.0.1:3100
  |
  v
QUIC/UDP til Host A
```

`mule` skal fokusere på:

- kryptert transport mellom Host B og Host A
- gjensidig autentisering
- én TCP-forbindelse per QUIC bidirectional stream
- kontrollert og fast target på exit-siden
- enkel drift med CLI og systemd

## V1-funksjonalitet

Implementer to subcommands:

```bash
mule forward ...
mule exit ...
```

### `mule exit`

Kjører på Host A. Lytter på UDP/QUIC og kobler autentiserte streams videre til én fast TCP-target.

Eksempel:

```bash
mule exit \
  --listen-udp ":4400" \
  --secret-file /etc/mule/b-to-a.key \
  --target "10.20.30.40:443" \
  --max-streams 100 \
  --dial-timeout 10s \
  --idle-timeout 5m
```

Krav:

- Lytt på QUIC over UDP på `--listen-udp`.
- Krev kryptografisk autentisering før target-dial.
- `--target` er obligatorisk i v1.
- Hver akseptert QUIC bidirectional stream oppretter nøyaktig én TCP-dial til `--target`.
- Etter vellykket dial kopieres data full-duplex mellom TCP-conn og QUIC-stream.
- Ved dial-feil returneres en liten kontrollmelding til peer og streamen lukkes.
- Exit skal aldri akseptere en klientstyrt target-host eller target-port i v1.
- Lukk aktive streams ved graceful shutdown etter angitt timeout.

### `mule forward`

Kjører på Host B. Lytter på lokal eller eksponert TCP-port, og sender hver klientforbindelse som en egen QUIC-stream til Host A.

Eksempel:

```bash
mule forward \
  --listen-tcp "0.0.0.0:3000" \
  --peer "host-a.example.org:4400" \
  --secret-file /etc/mule/b-to-a.key \
  --connect-timeout 10s \
  --idle-timeout 5m
```

Krav:

- Lytt på `--listen-tcp`.
- Opprett eller gjenbruk én QUIC-connection mot `--peer`.
- For hver innkommende TCP-forbindelse: åpne én bidirectional QUIC-stream.
- Send kontrollmelding `OPEN` før ordinære payload-bytes.
- Vent på `OK` fra exit før data kopieres.
- Ved `ERROR` eller failure lukkes lokal TCP-forbindelse.
- Når QUIC-connection faller bort, lukk streams/TCP-forbindelser tilknyttet den forbindelsen.
- Programmet kan forsøke reconnect for nye klientforbindelser, men skal ikke forsøke å resume eksisterende TCP-streams.

## Transport og kryptografi

Bruk **QUIC over UDP**.

Anbefalt bibliotek:

```text
github.com/quic-go/quic-go
```

Bruk TLS 1.3 som QUICs sikkerhetslag.

### Nøkkelmodell i v1

Bruk en delt hemmelig fil (`--secret-file`) på begge sider.

Ikke ta hemmeligheten direkte som CLI-argument, for eksempel ikke:

```bash
mule -k "pre-shared-secret"
```

Årsak: argumenter kan lekke via prosessliste, shell-historikk, audit, systemd-status og overvåkingsverktøy.

Krav til `--secret-file`:

- Les kun fra lokal fil.
- Filen skal inneholde minst 32 tilfeldige byte, enten base64- eller hex-kodet.
- Avvis svak, ugyldig eller for kort nøkkel.
- På Unix: advar eller avvis dersom filen er group/world-readable.
- Ikke logg hemmeligheten, hash av hemmeligheten eller sertifikatprivate nøkler.
- Dokumenter opprettelse:

```bash
install -d -m 0700 /etc/mule
umask 077
openssl rand -base64 32 > /etc/mule/b-to-a.key
chmod 0600 /etc/mule/b-to-a.key
```

### Autentisering

Implementer gjensidig TLS-autentisering basert på hemmeligheten.

For v1 er det akseptabelt å deterministisk avlede separate Ed25519-identiteter/certifikater fra den delte hemmeligheten, for eksempel gjennom HKDF-SHA-256 med rolle- og kontekstspesifikke labels:

```text
mule/v1/forward identity
mule/v1/exit identity
mule/v1/tls certificate
```

Viktige krav:

- Ikke bruk den rå hemmeligheten direkte som TLS private key.
- Bruk HKDF-SHA-256 med tydelig context separation.
- Peer-verifikasjon skal kontrollere forventet, avledet peer-identitet.
- Ikke stol på vanlig system-CA for tunnelautentisering.
- Ikke tillat anonyme klienter.
- Ikke logg TLS- eller autentiseringsdetaljer som kan hjelpe en angriper.
- Ikke aktiver 0-RTT i v1.
- Sett eksplisitte handshake-timeouts.

Alternativt kan Codex velge en enklere, men like sikker modell med en selvsignert CA/certifikat generert fra hemmeligheten, forutsatt at mutual TLS og peer pinning beholdes.

### Ikke lag egen UDP-protokoll

Ikke implementer:

- egen retransmisjon
- egen congestion control
- egen packet fragmentation
- egen anti-replay-mekanisme
- egen krypteringsramme
- en WireGuard-klone

QUIC skal eie disse transportegenskapene.

## Kontrollprotokoll over QUIC-stream

Hver TCP-connection fra forward-siden blir én QUIC bidirectional stream.

Før payload-data sendes skal det sendes en kort, eksplisitt kontrollframe.

V1 kan bruke en enkel, lengdeprefikset binærprotokoll eller JSON med hard grense for størrelse. Foretrekk binær eller veldig enkel JSON.

Minimum:

```text
Forward -> Exit:
  OPEN
  version: 1

Exit -> Forward:
  OK

eller:

Exit -> Forward:
  ERROR
  code: dial_failed | overloaded | unauthorized | internal_error
```

Krav:

- Kontrollframe skal være begrenset til liten størrelse, for eksempel maksimum 4 KiB.
- Les med deadline under handshake.
- Ikke aksepter ekstra felt som styrer destination i v1.
- Når `OK` er sendt, blir resten av streamen rå TCP-payload.
- Definer og test hvordan half-close håndteres.
- Bruk `CloseWrite` eller tilsvarende der QUIC-biblioteket tillater det, slik at TCP EOF propagates korrekt begge veier.

## CLI-design

Bruk subcommands og lange flagg. Unngå tvetydige korte flagg i første versjon.

### Felles flagg

```text
--secret-file PATH             påkrevd
--log-format text|json         default: text
--log-level debug|info|warn|error
--metrics-listen ADDRESS       optional, disabled by default
--shutdown-timeout DURATION    default: 30s
```

### `mule forward`

```text
--listen-tcp ADDRESS           påkrevd, eksempel 127.0.0.1:3000
--peer HOST:PORT               påkrevd, UDP/QUIC-endepunkt for exit
--connect-timeout DURATION     default: 10s
--handshake-timeout DURATION   default: 10s
--idle-timeout DURATION        default: 5m, 0 = disabled
--max-connections N            default: 100
--keepalive DURATION           default: 20s
```

### `mule exit`

```text
--listen-udp ADDRESS           påkrevd, eksempel :4400
--target HOST:PORT             påkrevd
--dial-timeout DURATION        default: 10s
--handshake-timeout DURATION   default: 10s
--idle-timeout DURATION        default: 5m, 0 = disabled
--max-streams N                default: 100
--max-pending-dials N          default: 20
--keepalive DURATION           default: 20s
```

### Kommandoer som også skal finnes

```bash
mule version
mule keygen --out /etc/mule/b-to-a.key
mule check --secret-file /etc/mule/b-to-a.key
```

`mule check` skal kontrollere at hemmelig fil kan leses, har gyldig format og tilstrekkelig entropilengde, uten å skrive sensitive verdier.

## Standardverdier og sikkerhet

Sikre standardverdier:

- `forward --listen-tcp` skal dokumenteres med anbefalt loopback-bind:
  `127.0.0.1:3000`.
- Ikke bind automatisk offentlig uten at operatøren har angitt dette eksplisitt.
- `exit --listen-udp` kan bruke `:4400` når angitt av bruker, men ikke gjett port.
- Begrens antall samtidige TCP-connections/streams.
- Begrens antall samtidige target-dials.
- Bruk deadlines for dial, stream handshake og idle.
- Avvis forbindelser ved overbelastning før expensive arbeid.
- Ikke logg payload.
- Ikke logg secrets.
- Sanitér peer-feil i standardlogg; detaljert feil kan ligge på debug-nivå uten sensitiv nøkkeldata.
- Valider alle `host:port`-verdier med `net.SplitHostPort` eller tilsvarende.
- Valider portområde 1–65535.
- Støtt IPv4 og IPv6, inkludert bracket-syntaks:
  `[2001:db8::1]:4400`.
- Beskytt mot ressurslekkasje ved avbrutt handshake og feilende dials.
- Ikke bruk `InsecureSkipVerify: true` uten egen korrekt peer-pinning/verifikasjon. Foretrekk eksplisitt `VerifyPeerCertificate` / `VerifyConnection` kombinert med korrekt TLS-konfigurasjon.
- Ikke tillat refleksjonsvennlig oppførsel eller uautentiserte store svar over UDP.

## Logging

Støtt tekst og JSON.

Hendelser:

```text
startup
shutdown
quic_connected
quic_disconnected
tcp_accepted
stream_opened
stream_rejected
target_dial_succeeded
target_dial_failed
connection_closed
authentication_failed
rate_or_limit_rejected
```

Felt som bør være med når relevant:

```text
role=forward|exit
local_address
peer_address
listener_address
target_address
connection_id (kort tilfeldig/ikke-sensitiv intern ID)
stream_id
duration_ms
bytes_client_to_target
bytes_target_to_client
reason
```

Ikke logg:

```text
secret
secret hash
private key
full TLS certificate material
payload
HTTP headers
applikasjonsdata
```

## Metrics

Prometheus-metrics er ønskelig, men kan komme etter minimal fungerende tunnel. Dersom det implementeres i v1, bruk en enkel egen HTTP-server som er deaktivert som standard.

Minimum metrics:

```text
mule_active_tcp_connections
mule_active_quic_streams
mule_quic_connections
mule_tcp_connections_total
mule_streams_total
mule_stream_errors_total
mule_target_dial_errors_total
mule_auth_failures_total
mule_bytes_client_to_target_total
mule_bytes_target_to_client_total
```

Unngå labels med høy kardinalitet som klient-IP, target-host, stream-ID eller connection-ID.

## Systemd

Lag eksempler for begge roller.

### `/etc/mule/mule-forward.service`

```ini
[Unit]
Description=Mule encrypted TCP forwarder
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=mule
Group=mule
ExecStart=/usr/local/bin/mule forward \
  --listen-tcp 127.0.0.1:3100 \
  --peer host-a.example.org:4400 \
  --secret-file /etc/mule/b-to-a.key \
  --log-format json
Restart=on-failure
RestartSec=3

NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/run/mule
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictSUIDSGID=yes

[Install]
WantedBy=multi-user.target
```

Lag tilsvarende `mule-exit.service`. Juster hardening dersom QUIC-bibliotek eller runtime krever noe annet, og dokumenter begrunnelsen.

## Repository-struktur

```text
mule/
├── cmd/
│   └── mule/
│       └── main.go
├── internal/
│   ├── auth/
│   │   ├── secret.go
│   │   ├── derive.go
│   │   └── tls.go
│   ├── config/
│   │   └── config.go
│   ├── protocol/
│   │   └── control.go
│   ├── transport/
│   │   └── quic.go
│   ├── forward/
│   │   └── forward.go
│   ├── exit/
│   │   └── exit.go
│   ├── logging/
│   │   └── logging.go
│   └── metrics/
│       └── metrics.go
├── deployment/
│   └── systemd/
├── examples/
│   ├── packetpony-ingress.yaml
│   └── README.md
├── README.md
├── SECURITY.md
├── go.mod
└── Makefile
```

Bruk Go-modulnavn:

```text
github.com/espegro/mule
```

## Implementasjonsdetaljer

- Bruk Go-context konsekvent.
- Alle goroutines skal ha tydelig eier og avslutningsvei.
- Ikke start ubegrenset antall goroutines fra uautentiserte UDP-pakker.
- Bruk semafor eller bounded worker pool for samtidige dials og streams.
- Bruk `errgroup` eller tilsvarende koordinering for full-duplex kopiering.
- Returner første reelle feil, men sørg for at begge sider av en kopiering lukkes.
- Bruk `io.CopyBuffer` med gjenbrukbare, begrensede buffere dersom det er nødvendig; unngå per-byte eller ukontrollert buffervekst.
- Ikke bruk globale mutable maps uten mutex eller kanal-eierskap.
- Kjør `go test -race ./...`.
- Kjør `go vet ./...`.
- Kjør `staticcheck ./...` dersom tilgjengelig.
- Bruk `gofmt`.
- Legg til dependency scanning i CI dersom enkelt mulig.

## Tester

Skriv reelle tester, ikke bare compile-tester.

### Enhetstester

- nøkkelfil: gyldig base64
- nøkkelfil: gyldig hex
- nøkkelfil: tom
- nøkkelfil: for kort
- nøkkelfil: ugyldig encoding
- Unix permissions-check når plattformen støtter det
- deterministisk HKDF-avledning
- forward og exit får forskjellige avledede identiteter
- kontrollframe encode/decode
- kontrollframe med feil versjon
- kontrollframe som er for stor
- address validation for IPv4, IPv6 og ugyldig port

### Integrasjonstester

Start:

1. lokal TCP echo-server som target
2. `mule exit` på tilfeldig lokal UDP-port
3. `mule forward` på tilfeldig lokal TCP-port
4. TCP-klient mot forward

Verifiser:

- bytes går begge veier
- flere samtidige klienter blir separate streams
- feil target gir kontrollert feil og lukker klientforbindelse
- feil secret gir ingen funksjonell tunnel
- over `--max-streams` avvises nye streams
- shutdown lukker listeners og terminerer streams kontrollert
- reconnect muliggjør nye forbindelser etter at exit kommer tilbake
- eksisterende forbindelse blir ikke feilaktig “resumert” etter QUIC-brudd

## Dokumentasjon

README skal inneholde:

1. Hva verktøyet gjør og ikke gjør.
2. Trusselmodell.
3. Hvorfor QUIC brukes i stedet for egen UDP-protokoll.
4. Hvorfor dette ikke er en WireGuard-erstatning.
5. Eksempel på keygen.
6. Komplett forward/exit-oppsett.
7. PacketPony foran `mule forward`.
8. Firewall-eksempel:
   - TCP 3000 inn mot PacketPony eller mule forward.
   - UDP 4400 fra Host B til Host A.
9. Begrensninger:
   - Kun TCP i v1.
   - Fast target på exit.
   - Original klient-IP bevares ikke på target.
   - Ingen stream resume.
   - Ikke egnet som generell VPN.
10. Drift og feilsøking.

## Ikke-mål for v1

Ikke implementer dette i første versjon:

- TUN/TAP
- IP-routing
- generell proxy med klientstyrt destination
- SOCKS
- HTTP CONNECT
- UDP-over-QUIC forwarding
- multi-peer mesh
- key rotation over nettverket
- automatisk sertifikatdistribusjon
- 0-RTT
- stream/session resume
- traffic obfuscation
- NAT traversal utover vanlig utgående UDP fra forward til exit
- støtte for å bruke verktøyet som anonymitets- eller evasion-mekanisme

## Leveransekrav

Lever første fungerende versjon med:

- komplett Go-kilde
- `mule forward`
- `mule exit`
- `mule keygen`
- `mule check`
- QUIC/TLS 1.3 med gjensidig autentisering
- fast target på exit
- enhetstester og integrasjonstest
- Makefile med `build`, `test`, `test-race`, `lint`, `release`
- README
- systemd-eksempler
- eksempel på PacketPony-integrasjon

Arbeid iterativt:

1. Sett opp prosjekt og CLI.
2. Implementer sikker håndtering/validering av secret-fil.
3. Implementer TLS/QUIC og mutual authentication.
4. Implementer kontrollprotokollen.
5. Implementer exit.
6. Implementer forward.
7. Legg til begrensninger, deadlines, logging og shutdown.
8. Skriv tester.
9. Skriv dokumentasjon og systemd-filer.

Ikke lever utestet kryptografi, egen UDP-reliability eller en “minimal” implementasjon som mangler autentisering, ressursgrenser eller kontrollert target-policy.
