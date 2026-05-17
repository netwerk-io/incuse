# Incus access

`incuse` reaches the Incus daemon two ways. The MVP path is the Unix
socket on the same host; HTTPS+cert is supported for the day we move
the orchestrator off-box.

## Unix socket (MVP)

Incus exposes `/var/lib/incus/unix.socket` to members of the
`incus-admin` group. Add the service user to that group and incuse
talks to the daemon with no extra credentials.

```bash
# As root on the Incus host (rocket).
sudo groupadd -f incus-admin           # already exists from the incus install
sudo useradd --system --no-create-home --shell /usr/sbin/nologin incuse
sudo usermod -aG incus-admin incuse
```

Reference it from `/etc/incuse/config.yaml`:

```yaml
incus:
  # Empty url + socket_path → Unix socket transport.
  socket_path: /var/lib/incus/unix.socket
  project: incuse
  default_profile: incuse-runner
```

The systemd unit (phase 6) wires `User=incuse` and
`SupplementaryGroups=incus-admin` so the service inherits the right
group at startup. Running as a `DynamicUser` would not work here —
group membership has to be persistent.

## HTTPS + client cert (alternative)

Use this when the orchestrator runs on a different host than the
Incus daemon, or when you want every administrative call audited
against a named cert.

### 1. Enable the daemon's HTTPS listener

```bash
sudo incus config set core.https_address :8443
```

`incus config show` afterwards reports the listener; the daemon
auto-generates `/var/lib/incus/server.crt` if it does not already
exist.

### 2. Generate a client cert

```bash
mkdir -p /etc/incuse
openssl req -x509 -newkey rsa:4096 -nodes -days 825 \
    -keyout /etc/incuse/client.key \
    -out /etc/incuse/client.crt \
    -subj "/CN=incuse@$(hostname)"
chmod 600 /etc/incuse/client.{crt,key}
chown incuse:incuse /etc/incuse/client.{crt,key}
```

825 days is the maximum CA/Browser-Forum validity for a non-CA cert;
shorter is fine, just plan for renewal.

### 3. Trust the cert on the daemon

```bash
sudo incus config trust add-certificate /etc/incuse/client.crt \
    --name incuse --restricted --projects incuse
```

`--restricted --projects incuse` scopes the cert to the `incuse`
Incus project so a stolen credential cannot create instances anywhere
else on the host.

### 4. Pin the daemon's cert (recommended)

Copy the daemon's TLS cert to the orchestrator host so the upstream
client validates the connection without falling back to the system
trust store:

```bash
sudo cp /var/lib/incus/server.crt /etc/incuse/server.crt
sudo chmod 644 /etc/incuse/server.crt
```

### 5. Configure incuse

```yaml
incus:
  url: https://rocket.lkv.netwerk.io:8443
  cert_file: /etc/incuse/client.crt
  key_file: /etc/incuse/client.key
  server_cert_file: /etc/incuse/server.crt
  project: incuse
  default_profile: incuse-runner
```

`insecure_skip_verify: true` is supported but only ever appropriate
for local development. The systemd unit's `--validate` preflight
(phase 6) refuses to start when both `insecure_skip_verify` and
`url` are set in production deployments.

## Picking a transport

| Use case                             | Transport            |
| ------------------------------------ | -------------------- |
| MVP, orchestrator on the Incus host  | Unix socket          |
| Orchestrator on a different host     | HTTPS + client cert  |
| Local dev against a remote dev box   | HTTPS, insecure ok   |

`Config.URL` and `Config.SocketPath` are mutually exclusive — when
`URL` is set the wrapper picks HTTPS, otherwise Unix socket.
