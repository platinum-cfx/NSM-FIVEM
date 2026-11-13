.# # NSM — Powered by AxiomFX (`AFX`)

**NSM (New State Multiplayer)** is a standalone, self-hosted GTA V multiplayer platform and full client/server/web ecosystem powered by the **AxiomFX (`AFX`)** runtime.

All core platform networks — including the **Masterlist**, **Server Browser**, **Server Policy & OneSync/EUP Unlock Service**, **Keymaster**, **In-Game Client Authentication**, **Upvotes & Featured Servers**, **Forum**, **Docs**, **Support**, and **Marketplace** — run independently on NSM infrastructure (**`https://nsmultiplayer.com`** / **`147.189.174.173`**) while maintaining **100% real-world runtime compatibility** with FiveM scripts, frameworks, and custom streaming assets, and preserving legitimate Rockstar Games GTA V ownership verification.

---

## Status: Confirmed Working in Production (`nsm-v0.8.0-candidate`)

| Component | Artifact | Status | Summary |
| :--- | :--- | :--- | :--- |
| **NSM Client Installer & Launcher** | `NSM.exe` | **Confirmed Working** | Single-file Windows GUI installer & launcher (`%LOCALAPPDATA%\NSM\NSM.app`), branded as `NSM - Powered by AFX`, `AxiomFX/` runtime directory, Steam App ID `218` (`Source SDK Base 2007`), fast edge game-storage updates, and in-app `nsmultiplayer.com` sign-in. |
| **NSM Dedicated Server** | `NSM-Server.zip` (`nsm-server.exe`) | **Confirmed Working** | Dedicated server runtime with `AxiomFX-*.dll` binaries, `afx>` console prompt, default `AFXserver` hostname, baked-in NSM masterlist ingress (no `sv_master1` URL needed in `server.cfg`), and automatic EUP + 2048-slot OneSync support. |
| **NSM Masterlist & Web Platform** | `NSM-Masterlist.exe` | **Confirmed Working** | All-in-one Go service powering `https://nsmultiplayer.com`, live server ingress/egress, in-app Discourse/RSA client auth, `/p/api/policy/` server policy unlock, Keymaster (`nsmk_...`), Server Upvotes (`1,000,000` Founder allowance for `nta~`), Featured Servers, Forum, Custom Profiles, Support, Marketplace, and Docs. |

---

## Release Downloads

- **Latest Release (`nsm-v0.8.0-candidate`)**:
  `https://github.com/platinum-cfx/NSM-FIVEM/releases/tag/nsm-v0.8.0-candidate`
  - **Client Installer (`NSM.exe`)**: `https://github.com/platinum-cfx/NSM-FIVEM/releases/download/nsm-v0.8.0-candidate/NSM.exe` (or `https://nsmultiplayer.com/download`)
  - **Dedicated Server (`NSM-Server.zip`)**: `https://github.com/platinum-cfx/NSM-FIVEM/releases/download/nsm-v0.8.0-candidate/NSM-Server.zip` (or `https://nsmultiplayer.com/download-server`)
  - **Masterlist & Website Server (`NSM-Masterlist.exe`)**: `https://github.com/platinum-cfx/NSM-FIVEM/releases/download/nsm-v0.8.0-candidate/NSM-Masterlist.exe`
- **Previous Stable Fallback (`nsm-v0.7.8-candidate`)**:
  `https://github.com/platinum-cfx/NSM-FIVEM/releases/tag/nsm-v0.7.8-candidate`

---

## Architecture & What Works

### 1. 100% Independent NSM Network Infrastructure
All NSM platform, account, policy, and server-discovery routes operate directly through the NSM stack (`https://nsmultiplayer.com` / `147.189.174.173`):

- **Masterlist Ingress & Server Browser (`/api/serverlist/ingress`, `/api/servers/live`, `/api/servers/single/{ep}`, `/api/servers/streamRedir/`)**:
  - `nsm-server.exe` (`AxiomFX-server-impl.dll`) has `http://147.189.174.173/api/serverlist/ingress` baked directly into the binary (`kDefaultServerList`), so server owners **do not need any `sv_master1` URL in `server.cfg`**.
  - Live servers appear simultaneously in the **`NSM.exe` in-game server browser** and on the **`https://nsmultiplayer.com/servers`** web server list.
- **Local Server Policy & Feature Unlock Service (`/p/api/policy/*`, `/api/policy/*`)**:
  - `NSM-Masterlist.exe` and `AxiomFX-net-five.dll` automatically grant:
    - `subdir_file_mapping` — unlocks **EUP custom clothing packs** and `stream/` subfolder organization (`^` prefix assets).
    - `onesync`, `onesync_plus`, `onesync_medium`, `onesync_big`, `onesync_platinum` — unlocks **OneSync Infinity up to 2048 player slots** (`maxClients <= 4096`) for every NSM server without external subscriptions.
    - `local_evaluation` — enables offline/local policy evaluation without external checks.
- **Pool-Size Limits & Ticket Public Keys (`/v1/public/pool-size-limits`, `/l/api/ticket/pubkey`)**:
  - Served directly by `NSM-Masterlist.exe` so neither client nor server depends on external Cfx endpoints for pool configuration or ticket key retrieval.
- **Fast Edge Game-Cache Updates (`c, r, s`) + Legitimate Rockstar GTA V Entitlement**:
  - Static GTA V DLC `.rpf` byte-range requests (`HTTP 206 Partial Content`) stream directly from high-speed edge CDNs so `"Updating game storage..."` completes in seconds.
  - Legitimate Rockstar Games / GTA V ownership verification (`legitimacy.dll` entitlement checks and ROS/EOS headers) is preserved intact.

---

### 2. Full Real-World FiveM Script & Resource Compatibility (How It Works Under the Hood)
NSM is engineered so existing FiveM servers, frameworks, scripts, and custom assets run **out of the box without a single line of code changed**. Below is how the **AxiomFX (`AFX`)** engine executes FiveM resources end-to-end:

1. **Dual-Layer Architecture (External `AxiomFX` Branding vs. Internal `citizen:` Registry)**:
   - On disk, all client and server folders and DLLs are rebranded to **`AxiomFX/`** and **`AxiomFX-*.dll`** (`AxiomFX-resources-core.dll`, `AxiomFX-scripting-lua.dll`, `AxiomFX-scripting-v8.dll`, `AxiomFX-scripting-mono.dll`).
   - Inside the engine's memory, internal `FXCOMPONENT` dependency identifiers (`citizen:resources:core`, `citizen:scripting:lua`, `citizen:resources:gta`) and FNV-1a component hash buckets are preserved verbatim so every engine subsystem initializes in the exact order FiveM scripts expect.

2. **Manifest Parsing (`fxmanifest.lua` & `__resource.lua`)**:
   - When `nsm-server.exe` starts a resource (`ensure <resource>`), `AxiomFX-resources-core.dll` runs the Lua manifest parser (`fx_version 'cerulean'`, `'bodacious'`, `'adamant'`, or legacy `resource_manifest_version`).
   - All standard manifest directives work identically: `client_scripts`, `server_scripts`, `shared_scripts`, `files`, `ui_page`, `data_file`, `dependencies`, `lua54 'yes'`, `escrow_ignore`, and `provides`.

3. **Identical Script Globals, Events, Exports & Convars**:
   - Every script runtime exposes the exact globals FiveM scripts call:
     - **Threading & Tick Loops**: `Citizen.CreateThread`, `CreateThread`, `Citizen.Wait`, `Wait`, `Citizen.SetTimeout`, `SetInterval`.
     - **Client/Server Networking**: `RegisterNetEvent`, `AddEventHandler`, `TriggerEvent`, `TriggerServerEvent`, `TriggerClientEvent`, `TriggerLatentClientEvent`, and `RegisterCommand`.
     - **Cross-Resource Exports**: `exports['resource_name']:functionName(...)` and `AddEventHandler('__cfx_export_...', ...)`.
     - **State Bags (OneSync)**: `Entity(veh).state`, `Player(src).state`, and `GlobalState` replication between `nsm-server.exe` and `NSM.exe`.

4. **All Three Scripting Languages (Lua 5.4, JavaScript/Node.js V8, C# Mono)**:
   - **Lua (`AxiomFX-scripting-lua.dll` / `AxiomFX-scripting-lua54.dll`)**: Full Cfx Lua 5.3 / 5.4 runtime including vector types (`vector2`, `vector3`, `vector4`, `quat`), `msgpack`, `json`, `promise`/`Citizen.Await`, and `debug` hooks.
   - **JavaScript / Node.js (`AxiomFX-scripting-v8*.dll`)**: Full V8 + Node.js server/client runtime, allowing scripts like **`oxmysql`**, **`webpack`**, **`yarn`**, Discord bots, and HTTP/WebSocket modules to run natively on the server.
   - **C# / Mono (`AxiomFX-scripting-mono.dll`)**: Full `.NET` / `CitizenFX.Core.dll` client & server assembly support (used by **`vMenu`** and C# roleplay frameworks).

5. **GTA V Native Execution (`Citizen.InvokeNative` & `scrbind`)**:
   - Client scripts invoke GTA V RAGE engine functions through the exact 64-bit universal native crossmap table (`0x...` hashes mapped to human-readable functions like `PlayerPedId()`, `GetEntityCoords()`, `CreateVehicle()`, `SetPedComponentVariation()`).
   - Server scripts execute server-side natives (`GetPlayerIdentifiers()`, `GetPlayerEndpoint()`, `DropPlayer()`, `PerformHttpRequest()`, `SetRoutingBucketPopulationEnabled()`, etc.) inside `AxiomFX-server-impl.dll`.

6. **NUI Chromium Browser (`nui://`) & Callbacks**:
   - Resources with a `ui_page` render inside `AxiomFX_SubProcess_chrome.bin` (CEF/Chromium).
   - Lua/JS/C# scripts send data to the UI via `SendNUIMessage({...})`, and the HTML/React/Vue UI talks back to the script via `fetch('https://resource_name/callback', ...)` -> `RegisterNUICallback('callback', ...)`.

7. **Custom Streaming Assets (`stream/`), EUP Clothing, Cars & MLOs**:
   - Any `.yft`, `.ydr`, `.ytd`, `.ydd`, `.ymap`, `.ytyp`, `.ymf`, `.awc`, or `.meta` file placed inside a resource's `stream/` folder (including nested `stream/[subfolder]/` directories and `^` prefix overrides) is hashed and streamed by `nsm-server.exe` to connecting `NSM.exe` clients.
   - Because `NSM-Masterlist.exe` (`/p/api/policy/*`) and `AxiomFX-net-five.dll` automatically unlock **`subdir_file_mapping`** and **`onesync_platinum`** (up to 2048 slots), EUP clothing packs, custom addon cars, custom weapons, and MLO interiors stream without needing any Cfx Patreon subscription.

8. **Confirmed Compatible Frameworks & Resources**:
   - **QBCore**, **Qbox**, **ESX Legacy**, **vMenu**, **ox_lib**, **ox_inventory**, **ox_target**, **ox_doorlock**, **oxmysql**, **pma-voice** (built-in Mumble voice server), custom phones/HUDs, custom addon vehicles, weapons, MLO interiors, and EUP clothing packs.
   - Supports all GTA V game builds via `sv_enforceGameBuild` (`b2802`, `b2944`, `b3095`, `b3258`, etc.).

---

### 3. NSM Client (`NSM.exe`) Features
- **Single-File AppData Self-Extractor & Launcher (`tools/nsm-installer`)**:
  - Downloading and running `NSM.exe` automatically installs the client runtime into `%LOCALAPPDATA%\NSM\NSM.app`, creates Desktop and Start Menu shortcuts (`NSM.lnk`), configures `%LOCALAPPDATA%\NSM\NSM.app\desktop.ini` with the blue NSM icon (`NSM Application Data`), and launches the client.
- **Full AxiomFX (`AFX`) Rebrand**:
  - Window title: **`NSM - Powered by AFX`**.
  - Runtime folder renamed from `citizen/` to **`AxiomFX/`** (including `legitimacy.dll` runtime TLS path trampolines and MBA constant patches so `AxiomFX/ros/...` certificates load seamlessly).
  - Client & subprocess binaries renamed: `afx_curl_x86_64.dll`, `AxiomFX-*.dll`, `AxiomFXGame.dll`, `AfxLaunch_TLSDummy.dll`, `AxiomFX.ini`, `AxiomFX_log`, `AxiomFX_SubProcess_*.bin`, and `AFXCrashDump`.
  - Custom blue NSM icons across `NSM.exe`, `NSM Application Data`, and `NSM_Diag.exe` (no FiveM snail icon).
- **Steam Presence (`Source SDK Base 2007` — App ID `218`)**:
  - `steam_api64.dll` initializes `SteamAppId=218`, `SteamGameId=218`, `SteamOverlayGameId=218`, and `steam_appid.txt` prior to `steamclient64.dll` connection so Steam displays **Source SDK Base 2007** while playing NSM.
- **In-App Account Sign-In (`/user-api-key/new`)**:
  - Clicking **Sign In** inside `NSM.exe` authenticates directly against `https://nsmultiplayer.com` using RSA-2048 OAEP encrypted Discourse user API keys (`client_auth.go`) with zero `"Discourse Auth service unavailable"` errors.

---

### 4. NSM Dedicated Server (`NSM-Server.zip`) Features
- **Ready-to-Run Dedicated Server (`nsm-server.exe`)**:
  - Double-clicking `nsm-server.exe` (or running `START-NSM-SERVER.bat`) automatically detects and executes `server.cfg`.
- **AxiomFX Server Binaries & Console**:
  - Uses `AxiomFX-*.dll` components (`AxiomFX-server-impl.dll`, `AxiomFX-resources-core.dll`, etc.) and `afx_curl_x86_64.dll`.
  - Interactive server console uses the **`afx>`** prompt (`afx> status`, `afx> refresh`, `afx> ensure <resource>`).
  - Default fallback server hostname is **`AFXserver`** (replacing `default FXServer`).
- **Direct Player Connections & Automatic Masterlist Heartbeats**:
  - Bypasses external `cfxTicket2` and `sv_lan` restrictions in `AxiomFX-server-impl.dll` so players can connect over the internet or LAN without Cfx Nucleus tokens.
  - Sends live heartbeats directly to `http://147.189.174.173/api/serverlist/ingress` out of the box — **no `sv_master1` URL needed in `server.cfg`**.
  - Ships with all standard `cfx-server-data` resources (`basic-gamemode`, `chat`, `spawnmanager`, `mapmanager`, `sessionmanager`, `hardcap`, `baseevents`, `yarn`, `webpack`, etc.).
- **Custom Server Logo in the Server List (`load_server_icon`)**:
  - Drop a **96x96 PNG** (exact size, e.g. `server-icon.png`) next to `server.cfg` and add `load_server_icon server-icon.png` to `server.cfg` (or run it live in the `afx>` console).
  - The logo is broadcast in every heartbeat and displayed automatically in the `NSM.exe` server browser and on `https://nsmultiplayer.com/servers` — servers without a logo keep the generated letter avatar.

---

### 5. NSM Website, Masterlist, Keymaster & Community Stack (`NSM-Masterlist.exe`)
`NSM-Masterlist.exe` (`tools/local-stack/`) is a self-contained binary serving the entire NSM web and API ecosystem:

- **Main Website (`https://nsmultiplayer.com`)**:
  - Landing page with live player/server metrics and direct `/download` (`NSM.exe`) and `/download-server` (`NSM-Server.zip`) endpoints.
- **Unified Blue Server List (`/servers` & `/join/{endpoint}`)**:
  - Single, unified blue Server Browser UI matching `NSM.exe` with real-time search, tags, player lists, ping/location details, and **`https://nsmultiplayer.com/join/<endpoint>`** direct connect links (`X-CitizenFX-Url` header support).
- **Persistent Server Upvotes & Founder Controls**:
  - **Founder (`nta~`)** automatically holds **`1,000,000` Upvote Power** (`User.UpvoteAllowance()`), with persistent upvote storage across `/servers` and `NSM.exe` (`UpvotePower` & `BurstPower`).
  - **Featured Servers (`/v1/public/featured-servers`)**: Exclusively controllable by **`nta~`** (`POST /api/servers/feature`) and showcased at the top of `/servers`.
- **AFX Keymaster (`/keymaster`)**:
  - Generate, manage, and toggle NSM server license keys (`nsmk_...`) tied to user accounts.
- **AFX Community Forum (`/forum`), Custom Profiles (`/u/{username}`) & Support (`/support`)**:
  - Full Discourse-style forum with categories, staff-only `announcements` topic creation, real-time view counters, two-way friend requests & DMs, custom AFX profile themes, animated avatar decorations, 18 hard-to-earn badges, email verification, 2FA, Staff Moderation Panel, and Zendesk-style Support Tickets with strict per-user privacy (`MyTickets` vs staff-only `AllTickets`).
- **Account Security & Keymaster Hardening**:
  - Registration now **requires a real email address** and sends a genuine 6-digit verification code through SMTP (configure `NSM_SMTP_HOST` / `NSM_SMTP_PORT` / `NSM_SMTP_USER` / `NSM_SMTP_PASS` / `NSM_MAIL_FROM` in the server environment; credentials never live in files), plus optional **Sign in with Google** (`NSM_GOOGLE_CLIENT_ID` / `NSM_GOOGLE_CLIENT_SECRET`).
  - Server keys are **visible only to the account that issued them** (not even other staff can browse them in Keymaster), profile notifications/DMs are strictly private, and support tickets require a signed-in account — closed tickets leave the working queues and are archived in a **Closed Ticket Log**.
  - Keymaster shows your **10 free upvotes** with a 1–10 slider to boost your own server's listing, and advertises the **free 2048 player slots** included with every key.

---

## Quick Start Guide

### 1. Running the Masterlist & Website (`NSM-Masterlist.exe`)
On your Windows VPS (`147.189.174.173` / `nsmultiplayer.com`):
```powershell
.\NSM-Masterlist.exe
```
- Binds to `0.0.0.0:80` by default with public URL `https://nsmultiplayer.com`.
- Automatically configures Windows Firewall rules for TCP port `80` and persists all accounts, forum posts, keys, and server upvotes in its data directory.

### 2. Installing & Playing NSM (`NSM.exe`)
1. Download **`NSM.exe`** from `https://nsmultiplayer.com/download` (or the GitHub release).
2. Run **`NSM.exe`** — it installs into `%LOCALAPPDATA%\NSM\NSM.app`, creates Desktop/Start Menu shortcuts, verifies your GTA V installation, updates game storage from fast edge mirrors, and opens the NSM launcher.

### 3. Hosting an NSM Server (`NSM-Server.zip`)
1. Download **`NSM-Server.zip`** from `https://nsmultiplayer.com/download-server` and extract it.
2. *(Optional)* Edit `NSM-Server\server.cfg` to set your `sv_hostname`, `tags`, `sv_maxclients`, and `ensure` your scripts/resources in `NSM-Server\resources\`. (No `sv_master1` URL is required.)
3. Run **`START-NSM-SERVER.bat`** or double-click **`NSM-Server\nsm-server.exe`**.
4. Open port `30120` (TCP & UDP) in your firewall — your server will automatically appear on `https://nsmultiplayer.com/servers` and inside `NSM.exe`.

---

## Repository Layout

- `code/` — C++ client (`launcher`, `diag`, `shared`, `citicore`) and server (`citizen-server-impl`, `net`, `pool-sizes-state`, etc.) source code.
- `data/client/` — Client runtime data and patched `steam_api64.dll` (Steam App ID `218`).
- `tools/local-stack/` — Go source code, HTML templates (`site/templates/`), static assets (`site/static/`), and test suite (`local_api_test.go`) for `NSM-Masterlist.exe`.
- `tools/nsm-installer/` — Windows GUI self-extracting AppData installer (`main_windows.go`) for `NSM.exe` and the server artifact builder/patcher (`build_server_artifact.py`) for `NSM-Server.zip`.
- `.github/workflows/build-v078-release.yml` — Automated CI/CD pipeline that verifies locked paths, runs `go test -race ./...`, patches client and server PE binaries, builds `NSM.exe`, `NSM-Server.zip`, and `NSM-Masterlist.exe`, and publishes GitHub releases.
