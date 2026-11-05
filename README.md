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

## Secure MCP Tunnel (private ChatGPT and dots access)

HomelabObserver supports OpenAI's **Secure MCP Tunnel**, which requires only
an outbound connection to OpenAI's API. It does **not** require DNS, a
public hostname, an incoming firewall rule, or a Cloudflare Tunnel.

1. In [Platform Tunnel settings](https://platform.openai.com/settings/organization/tunnels),
   create a private tunnel and associate it with the target ChatGPT workspace
   and owning Platform organization. Save its tunnel ID.
2. Create a restricted **runtime API key** with Tunnels Read + Use in that
   organization. Tunnel creation requires separate Tunnels Read + Manage
   permissions. Do not give the long-running daemon an Admin API key.
3. Download and SHA-256-verify the official Linux binary from
   [openai/tunnel-client releases](https://github.com/openai/tunnel-client/releases/latest).
   Install it at /usr/local/bin/tunnel-client. The upstream Apache-licensed
   binary is separate from this GPL repository.
4. Install deploy/homelab-observer-tunnel.service as a systemd unit and
   deploy/homelab-observer-tunnel-setup to /usr/local/sbin/. Create the
   dedicated unprivileged homelab-tunnel account. Its directory
   /etc/homelab-tunnel must be mode 0700 and owned by homelab-tunnel.
   Do not start the tunnel service without credentials.
5. SSH to the host and run the setup command **interactively**:

       sudo /usr/local/sbin/homelab-observer-tunnel-setup

   Enter the tunnel ID and hidden OpenAI runtime key in the prompts.
   The script validates the values and stores the key in the 0600 file
   /etc/homelab-tunnel/openai-api-key. Neither value is printed or placed
   in shell history, command arguments or Git. The script enables and starts
   the service.
6. Verify the service and readiness:

       systemctl status homelab-observer-tunnel.service
       curl -fsS http://127.0.0.1:18766/healthz
       curl -fsS http://127.0.0.1:18766/readyz

The official client uses file-based secret references and forwards to the
loopback MCP server at http://127.0.0.1:8765/mcp. It injects the separate
MCP Bearer token from another protected file, leaving the MCP endpoint
authenticated. The tunnel identity does not have permission to read the
TrueNAS API key. The outbound OpenAI transport uses a host-specific proxy,
but loopback MCP calls are not proxied.

In ChatGPT Plugins, choose **Add custom MCP server**, Connection **Tunnel**,
select the newly created tunnel, and create a private HomelabObserver plugin.
Select **No authentication** for the ChatGPT-facing plugin only after
confirming that the tunnel is restricted to your intended workspace; the
daemon itself injects the local MCP Bearer credential. This does not disable
authentication on the actual MCP origin. Install the plugin to your ChatGPT
account and allow your dot to use it through shared Plugins permissions.

A local installation without an OpenAI API key or tunnel ID is intentionally
not an active OpenAI connection. Confirm /readyz and run a real plugin tool
call after provisioning before considering the integration complete.

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
