# NSM masterlist service

Minimal replacement for the Cfx masterlist/identity endpoints, pointed at by
NSM clients (v3+) and servers (v6+).

Two implementations with identical behaviour:

| File | Runs on |
|---|---|
| `masterlist.py` | anything with Python 3.8+ (standard library only) |
| `masterlist.go` | any platform with a Go toolchain — builds the standalone `NSM-Masterlist.exe` / `nsm-masterlist-linux-*` binaries |

> The canonical source of the service lives in `platinum-cfx/NSM-masterlist`.
> The Go implementation is kept in this repository as well so the deployed
> binary can be rebuilt and redeployed from here; the two must stay in sync.

## Upgrading an existing deployment (this is the fix for the server browser)

A deployment that answers `GET /api/servers/streamRedir/` with **`200` and a
body instead of `302` + `Location`** is the pre-fix build, and the client
browser will show **no servers** - the `gsclient:url` RPC returns the
`Location` header only, so it hands the UI an empty URL.

Check which build is running:

```bash
curl -i http://127.0.0.1/api/servers/streamRedir/ | head -3
# fixed:  HTTP/1.1 302 Found
#         Location: http://194.104.146.133/api/servers/stream/
# broken: HTTP/1.1 200 OK   (no Location header)
```

Upgrade:

```bash
# as root
cp masterlist.py /opt/nsm-masterlist/masterlist.py
cp nsm-masterlist.service /etc/systemd/system/
systemctl daemon-reload
systemctl restart nsm-masterlist
curl -i http://127.0.0.1/api/servers/streamRedir/ | head -3
```

The unit file passes `--public-base http://194.104.146.133`, so the
`Location` header is deterministic instead of being derived from the request's
`Host` header.

Deploy on the VPS (194.104.146.133):

```bash
# as root
mkdir -p /opt/nsm-masterlist
cp masterlist.py /opt/nsm-masterlist/

# --- option A: systemd (recommended) ---
cp nsm-masterlist.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now nsm-masterlist

# --- option B: quick test ---
python3 /opt/nsm-masterlist/masterlist.py --port 8080
```

Check it:

```bash
curl http://127.0.0.1/health
# -> NSM masterlist service

curl -X POST http://127.0.0.1/api/serverlist/ingress \
  -H 'Content-Type: application/json' -d '{"port":30120,"fallbackData":{"dynamic":{"clients":0,"hostname":"test","svMaxclients":48}}}'
curl http://127.0.0.1/api/servers/single/127.0.0.1:30120

# the browser resolves the list through the redirect:
curl -i http://127.0.0.1/api/servers/streamRedir/
# -> HTTP/1.1 302 Found
#    Location: http://127.0.0.1/api/servers/stream/
curl -o /dev/null -w '%{content_type}\n' http://127.0.0.1/api/servers/stream/
# -> application/octet-stream
```

Run the regression tests:

```bash
python3 tools/masterlist/tests/test_masterlist.py
python3 tools/masterlist/tests/test_ui_bundle_patch.py
go test ./...            # in tools/masterlist/
```

## Firewall

Open TCP 80 inbound (the client UI, and the servers' heartbeats, both hit it).

## How it fits together

| Endpoint | Consumer | Purpose |
|---|---|---|
| `GET /api/servers/streamRedir/` | client UI (server browser) | **302 redirect** to `/api/servers/stream/` |
| `GET /api/servers/stream/` | client UI (server browser) | full list, 4-byte LE length-prefixed protobuf `master.Server` frames |
| `GET /api/servers/single/<ep>` | client UI | single server JSON for direct connect / favorites |
| `GET /api/servers/icon/...` | client UI | server icons (404 = default icon) |
| `POST /api/serverlist/ingress` | NSM server heartbeat | registers/refreshes a server (5 min TTL) |
| `GET /api/ticket/pubkey` | server (when `sv_lan false`) | RSA pubkey for ticket checks |
| `GET /api/identities`, `/api/ticket/identities` | client UI | linked identities (empty) |
| `GET /v1/public/*`, `/runtime/counts.json` | client UI / server | news, featured, pool limits (empty defaults) |

Servers appear in the browser only while they heartbeat; entries expire after
5 minutes. The service forces `vars.gamename = gta5` so the client's browser
filter (`init(["gta5"])`) shows them.

### Why `streamRedir` has to redirect

The browser does not fetch the server list itself. It asks the `nui:gsclient`
component for it through the `gsclient:url` NUI RPC, and that RPC performs the
request with `HttpRequestOptions::followLocation = false` and hands the UI the
response's **`Location`** header
(`code/components/nui-gsclient/src/ServerList.cpp`).

If `streamRedir` answers `200` with the list in the body, the RPC returns an
empty string, the UI never gets a server-list URL, and the server browser comes
up without any servers. So `streamRedir` must answer `302` with a `Location`
header pointing at `/api/servers/stream/`, which serves the protobuf frames.

The `Location` value is built from the request's `Host` header (or from
`--public-base` when the service sits behind a proxy that rewrites `Host`).

## Server configuration (on the NSM server)

```
sv_lan "true"        # no Cfx connect tickets — required for direct connect
sv_hostname "..."    # shown in the browser
```

`sv_master1` already defaults to `http://194.104.146.133/api/serverlist/ingress`
in NSM server builds; the heartbeat is sent regardless of license key.

## Notes

- `ticket-signing-key.pem` is the RSA key whose public half is served at
  `/api/ticket/pubkey`. Only relevant if a server runs `sv_lan false`; the
  ticket-issuing flow is not implemented, so keep `sv_lan true`.
- Heartbeats are accepted from any source; protect the port or trust your
  network if listing garbage entries is a concern.

## Regression tests

Run from the repository root (Python standard library only):

```bash
python -m unittest discover -s tools/masterlist -v
```

Heartbeat bodies are limited to 1 MiB. Malformed data, invalid ports, negative
client counts, and unsupported HTTP transfer framing return an error without
registering an entry. Protobuf negative integers are encoded as 64-bit varints.
