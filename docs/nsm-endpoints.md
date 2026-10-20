# NSM endpoint map

Every network endpoint the NSM client and server use, where it is configured,
who calls it, and what it must return. Generated while fixing the missing
server browser (see `docs/server-browser-bugfix.md`).

Legend: **masterlist** = the NSM service on the NSM host (default port 80).

## 1. Server browser (client → masterlist)

The server browser is the `cfx-ui` application, served to the launcher from
`citizen:/ui.zip` and hosted in the `mpMenu` NUI frame
(`code/components/citizen-game-main/src/Game.cpp`).

The UI does **not** talk to the masterlist directly. It calls the
`nui:gsclient` NUI RPC `gsclient:url`, which performs the HTTP request in the
game process with `HttpRequestOptions::followLocation = false` and returns the
response's **`Location`** header to the UI
(`code/components/nui-gsclient/src/ServerList.cpp`).

> **Consequence:** an endpoint that the browser resolves through
> `gsclient:url` **must** answer with a redirect and a `Location` header.
> Answering `200` directly makes the RPC return an empty string, and the
> browser never receives a server list.

| Endpoint | Caller | Purpose | Expected response |
|---|---|---|---|
| `GET /api/servers/streamRedir/` | cfx-ui via `gsclient:url` | resolve the server-list stream URL | `302` + `Location: <base>/api/servers/stream/` |
| `GET /api/servers/stream/` | cfx-ui server-list worker | full server list | `200`, `application/octet-stream`, repeated `[4-byte LE length][protobuf master.Server]` |
| `GET /api/servers/single/<ip:port>` | cfx-ui (direct connect, favorites) | one server | `200` `{ "EndPoint": …, "Data": {…} }`, `404` when unknown |
| `GET /api/servers/icon/<ep>/<v>.png` | cfx-ui | server icon | `404` (client falls back to the default icon) |
| `GET /api/identities` | cfx-ui | linked identities | `200` `[]` |
| `GET /api/ticket/identities` | cfx-ui | ticket identities | `200` `[]` |
| `GET /v1/public/featured-servers` | cfx-ui | featured servers | `200` `[]` |
| `GET /v1/public/notices/fivem` | cfx-ui | notices | `200` `{ "notices": [] }` |
| `GET /v1/public/tweets` | cfx-ui | tweets | `200` `{ "tweets": [] }` |
| `GET /runtime/counts.json` | cfx-ui | player counts | `200` `[players, 0, players]` |

The `gamename` variable of every listed server is forced to `gta5`, because the
client browser filters on `init(["gta5"])`.

## 2. Server → masterlist

| Endpoint | Caller | Purpose | Expected response |
|---|---|---|---|
| `POST /api/serverlist/ingress` | NSM server heartbeat (`citizen-server-impl/src/GameServer.cpp`) | register / refresh a server (5 min TTL) | `200` `{}` (empty `lastError` = success) |
| `GET /api/ticket/pubkey` | NSM server with `sv_lan false` | RSA public key for ticket validation | `200` `{ "key": "<base64 PKCS#1>" }` |
| `GET /v1/public/pool-size-limits/fivem` | client + server | pool size limits | `200` `{}` (empty = unlimited) |
| `GET /v1/public/pool-size-limits/redm` | client + server | pool size limits | `200` `{}` |

## 3. Other NSM-hosted endpoints

| URL | Configured in | Purpose |
|---|---|---|
| `http://194.104.146.133/` | `code/client/shared/CnlEndpointOverride.h` (`CNL_ENDPOINT`) | connect tickets / identity lookups |
| `http://194.104.146.133/updates` | `code/client/launcher/LauncherConfig.h` (`CFX_UPDATER_URL`) | component updates |

## 4. Local services inside the client

There is **no** hard-coded localhost API port in this tree. `9987` and `9988`
do not appear anywhere in the sources (verified by grep over `code/`).

| Service | Transport | Notes |
|---|---|---|
| NUI / server browser | CEF custom scheme `nui:` (`https://nui-game-internal/…`) | registered with `CEF_SCHEME_OPTION_SECURE`; content comes from `citizen:/ui.zip` and `citizen:/ui-big.zip` |
| NUI → HTTP bridge | NUI RPC `gsclient:url`, `gsclient:dynamic` | runs in the game process through the `http-client` component |
| Launcher ↔ game IPC | `citizen:game:ipc` (nng push/pull, `launcherTalk`) | no TCP port |
| Updater/progress window | shared memory `CfxUUIState` | no socket; `finalized` / `waitForExpiration` drive the window |
| Shared init state | shared memory `CfxInitState` | `initialLauncherPid`, `initialGamePid`, `gamePid` |
| NSM entitlement service | `http://127.0.0.1:18080/l/api/validate/entitlement*` | the closed-source `legitimacy` component's entitlement endpoints, rewritten in place by `tools/legitimacy/patch_entitlement_endpoint.py`. **Implemented**: the master process hosts it (`code/client/launcher/EntitlementService.cpp`), bound to the loopback interface only, and answers with the "no entitlement record" payload Cfx's own service returns for an unknown account (`<Response><Status>0</Status><Error Code="DoesNotExist" /></Response>`). The check still runs, still fails and is still reported. The Rockstar ownership verdict stays fatal - see `code/client/shared/EntitlementError.h`. |

## 5. Cfx endpoints that are still referenced (not rebranded)

| URL | Where | Impact |
|---|---|---|
| `https://lambda.fivem.net/api/validate/entitlement` | `code/components/legitimacy/release/legitimacy.dll` (closed source) | **Rewritten** to `http://127.0.0.1:18080/l/api/validate/entitlement` (see above). Pointing it at Cfx is what produced `Error contacting Rockstar Online Services: <Response><Status>0</Status><Error Code="DoesNotExist" /></Response>` and aborted the start-up transition. |
| `https://lambda.fivem.net/api/validate/entitlement/rosfive` | same | same, rewritten |
| `https://lambda.fivem.net/api/validate/entitlement/ros2` | same | same, rewritten |
| `https://lambda.fivem.net/api/validate/discord` / `/discourse` | same | same, rewritten |
| `https://forum.cfx.re/…` | same | only used in crash-report links |
| `https://content.cfx.re/mirrors/…` | `code/client/launcher/GameCache.cpp` | game cache / component downloads |
| `https://native-collector-live.fivem.net/upload` | `code/components/citizen-scripting-core/src/NativeResourceLogging.cpp` | native resource logging |

> The `legitimacy` component is a closed-source Cfx binary, so its service URLs
> can only be changed by patching the binary. The patch
> (`tools/legitimacy/patch_entitlement_endpoint.py`) rewrites **only** the URL
> prefixes, in place and with the same length, so the file size does not change
> and no byte moves. The check itself is not faked: it still runs, still fails
> and still reports the error. The NSM service deliberately does **not**
> implement `/api/validate/entitlement`: returning a synthetic "entitled"
> response would be faking an entitlement check, which is not acceptable.

## 6. UI bundle URL rewriting

The prebuilt, closed-source `cfx-ui` bundle is downloaded at build time and
patched in place:

* `ext/ui-build/patch_ui_urls.ps1` (this branch) / `patch_ui_bundle.ps1`
  (rebrand branch) — rewrites the service hosts inside the bundle.
* `ext/ui-build/build.cmd` — downloads the bundle, patches it, unpacks it into
  `citizen:/ui.zip` and `citizen:/ui-big.zip`.

The rewritten hosts must stay in sync with the table above - see
`tools/masterlist/tests/test_ui_bundle_patch.py`.
