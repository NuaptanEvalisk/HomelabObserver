# HomelabObserver

**HomelabObserver** is a small, self-hosted, read-only homelab telemetry
collector with a Model Context Protocol (MCP) interface. It is designed for
AI assistants that need timestamped observations rather than administrative
control.

**License:** GPL-3.0-only. **Target:** Linux/amd64.

## Features

- Reuses an existing status-page JSON snapshot, including intentionally
  powered-off nodes versus unexpected outages.
- Samples the collector host's CPU, MemAvailable and filesystem free capacity
  directly from Linux procfs and statfs.
- Connects to TrueNAS SCALE 25.10 through its official JSON-RPC WebSocket API,
  using TLS certificate fingerprint verification and a separate read-only key.
  It gathers CPU, RAM, ARC, pool health/capacity, apps and system alerts.
- Saves time-series measurements and service transitions in SQLite. Retains
  30 days by default.
- Serves authenticated, loopback-only Streamable HTTP MCP with six read-only
  tools: **get_overview**, **list_nodes**, **get_node_details**,
  **get_alerts**, **get_recent_changes**, **get_metric_history**.
- Marks unavailable or old data as stale; never replaces a missing metric
  with zero. The timestamp of an old retained TrueNAS CRITICAL alert stays
  visible, and its existence is not treated as evidence of a live incident.

## Architecture

    Existing local status snapshot --+
    Collector host procfs -----------+--> HomelabObserver --> SQLite history
    TrueNAS WSS API (read-only) ------+             |
                                             127.0.0.1:8765/mcp
                                                   |
                                        authenticated secure tunnel
                                                   |
                                               MCP client

No SSH access to TrueNAS is required for monitoring. TrueNAS does not need an
additional agent. This is not a remote administration server and provides no
shell, BMC control, power control, write, delete or other modification tools.

## Build and test

    go test -race ./...
    go vet ./...
    CGO_ENABLED=0 go build -trimpath -o build/homelab-observer .

The binary bundles the Go runtime, WebSocket client and pure-Go SQLite driver.

## Configuration

Copy config.example.json to a private, host-specific configuration file.
Do not commit local settings or secret files. The relevant options are:

- **status_path**: local snapshot containing checked_epoch, machines and
  services.
- **database_path**: private directory writable by the service user.
- **truenas_url**: a WSS URL, for example wss://NAS_HOST/api/current.
- **truenas_api_key_file**: a chmod-0600 file containing only the read-only
  TrueNAS API key.
- **truenas_certificate_sha256**: the current TrueNAS TLS leaf certificate's
  SHA-256 fingerprint (64 hexadecimal digits). The connection fails closed if
  the fingerprint changes or the certificate expires.
- **mcp_bearer_file**: an independent random secret for MCP clients. **This
  is not the TrueNAS key.**
- **poll_seconds**: default 60, configurable between 15 and 3600.
- **retention_days**: default 30.

The stock TrueNAS self-signed certificate may identify localhost instead of
the NAS IP. Exact SHA-256 certificate pinning explicitly authenticates the
certificate without silently disabling identity verification. Verify the
initial certificate fingerprint from a trusted management environment:

    openssl s_client -connect NAS_HOST:443 -servername NAS_HOST </dev/null 2>/dev/null |
      openssl x509 -noout -fingerprint -sha256

Certificate rotation requires updating the pin. Missing credentials do not
prevent collection of the local host or existing status snapshot. TrueNAS
errors are exposed as errors/stale state, not fabricated telemetry.

## systemd deployment

The file deploy/homelab-observer.service runs as a dedicated unprivileged
service user with a private systemd-managed state directory. Install the
compiled binary to /usr/local/bin/homelab-observer, configure it at
/etc/homelab-observer/config.json, and create two user-only credential files:

- /etc/homelab-observer/truenas-api-key (starts empty until provisioned)
- /etc/homelab-observer/mcp-bearer (random, unrelated to TrueNAS)

The files should be owned by the service account with mode 0600. The
configuration directory should be accessible only to root and the service
account. Then enable the unit:

    sudo systemctl daemon-reload
    sudo systemctl enable --now homelab-observer.service
    sudo systemctl status homelab-observer.service

### Entering your TrueNAS API key safely

After preparing the key file, run the following **interactively on the host**
(not through a command tool or chat), without echoing the secret to the shell
history. This writes only the key to the file and removes the shell variable:

    sudo bash -c 'IFS= read -r -s -p "TrueNAS read-only API key: " KEY; printf "\n"; printf "%s\n" "$KEY" > /etc/homelab-observer/truenas-api-key; unset KEY'

The existing service will attempt to authenticate on its next polling cycle.
The key is never passed in a URL, command line or HTTP response. Use a
dedicated account with only the TrueNAS read privileges needed for reporting,
pools, apps and alerts.

### Checking the local service

    curl -sS http://127.0.0.1:8765/healthz

MCP uses POST /mcp with application/json and Authorization: Bearer
MCP_BEARER_TOKEN. The server explicitly refuses non-loopback bind addresses
and rejects requests carrying an Origin header. The API key is never used
as MCP authentication.

Use a **Secure MCP Tunnel** or authenticated, TLS-protected gateway to connect
a remote client. Do not expose the loopback endpoint through a public,
unauthenticated proxy. Deployment does not automatically connect anyone's
ChatGPT or Dot account; that connection requires authorization in the
client's plugin interface.

## Data interpretation and limits

- Metrics have source, sample time and freshness information; missing
  samples are not interpolated. Historical queries are bounded to 7 days and
  at most 1,000 points per call.
- TrueNAS reporting can return trailing zero placeholders. The collector
  selects the latest recent meaningful sample rather than blindly using
  the last returned row.
- Stored old alerts are explicitly labeled historical, while fresh ZFS pool
  status is reported independently.
- Network outages preserve last-known values but mark them stale.
- This repository intentionally contains no personal hostnames, addresses,
  keys, certificates or other site-specific secrets.

## License

GPL-3.0-only. See [LICENSE](LICENSE).
