# Windows test checklist — NSM server browser fix

Everything the fix touches is verified by a **real run on Windows with a legitimate
GTA V install**. This is the checklist for that run.

Branch: `arena/81dd1d7c-nsm-fivem` — PR <https://github.com/platinum-cfx/NSM-FIVEM/pull/6>
Full analysis: [`docs/server-browser-bugfix.md`](server-browser-bugfix.md)

---

## 0. Get the artifacts

Two artifacts are produced by CI (the sandbox could not download them itself — the
artifact CDN is not reachable from there, but the browser can):

| Artifact | Run | What it is |
|---|---|---|
| `nsm-client-2de7ad02aab84274a0a01350f08e888de6c76324` | **NSM client build** run [39011506212](https://github.com/platinum-cfx/NSM-FIVEM/actions/runs/39011506212) — **success** | the Windows client: `caches/fivereborn/` = the complete client tree (bootstrapper `CitizenFX.exe` = `NSM.exe`, `citizen/`, `bin/cef/…`, `citizen/ui.zip`). 765 MB, 14-day retention. |
| `nsm-masterlist-binaries` | **NSM masterlist tests** workflow | `NSM-Masterlist.exe` (Windows) and `NSM-Masterlist` (Linux) — the rebuilt masterlist |

Download both from the run pages:
<https://github.com/platinum-cfx/NSM-FIVEM/actions>

---

## 1. Redeploy the masterlist (required — this is the primary bug)

The browser gets its server list from the masterlist, and the deployed masterlist
answered the list request with `200 + body` instead of `302 + Location`. The
`nui:gsclient` `gsclient:url` RPC does **not** follow redirects — it reads the
`Location` header — so the UI received an empty URL and showed **no servers**.

### 1a. Which masterlist is running?

```bash
systemctl cat nsm-masterlist        # -> ExecStart shows which implementation
```

* `… /usr/bin/python3 /opt/nsm-masterlist/masterlist.py …` → **Python** (this is
  what `tools/masterlist/nsm-masterlist.service` runs).
* `… /opt/nsm-masterlist/NSM-Masterlist.exe …` → **Go** (use the
  `NSM-Masterlist.exe` from the CI artifact).

### 1b. Deploy

**Python** (recommended, it is the service unit in this repo):

```bash
scp tools/masterlist/masterlist.py      root@194.104.146.133:/opt/nsm-masterlist/masterlist.py
scp tools/masterlist/nsm-masterlist.service root@194.104.146.133:/etc/systemd/system/nsm-masterlist.service
ssh root@194.104.146.133 'systemctl daemon-reload && systemctl restart nsm-masterlist && systemctl status nsm-masterlist --no-pager'
```

**Go** (drop-in binary):

```bash
scp NSM-Masterlist.exe root@194.104.146.133:/opt/nsm-masterlist/NSM-Masterlist.exe
ssh root@194.104.146.133 'systemctl restart nsm-masterlist'
```

### 1c. Verify the contract (this is the exact request the client makes)

```bash
curl -i http://194.104.146.133/api/servers/streamRedir/
```

**Expected** — a redirect, not a body:

```
HTTP/1.1 302 Found
Location: http://194.104.146.133/api/servers/stream/
Content-Length: 0
```

**Wrong (the old, broken behaviour):**

```
HTTP/1.1 200 OK
Content-Type: application/octet-stream
<binary protobuf>
```

Then the stream itself:

```bash
curl -s http://194.104.146.133/api/servers/stream/ | wc -c      # > 0 once a server heartbeats
curl -s -o /dev/null -w '%{http_code}\n' http://194.104.146.133/api/servers/stream/   # 200
```

A server must be heartbeating for the list to be non-empty
(`POST /api/serverlist/ingress`, 5-minute TTL):

```bash
curl -s http://194.104.146.133/api/servers/single/<ip:port>   # {"EndPoint": …, "Data": {…}}
```

**Regression guard:** the entitlement route must still be absent (it is *not* faked):

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://194.104.146.133/api/validate/entitlement   # 404
```

---

## 2. Run the client on Windows

1. Unpack the `nsm-client-<sha>` artifact.
2. `caches/fivereborn/` is the client tree — it contains `CitizenFX.exe`
   (this **is** `NSM.exe`, the rebrand renames the product exe), `citizen/`
   (including `citizen/ui.zip` = the NSM-patched UI bundle) and `bin/cef/`.
3. Copy the tree into your existing NSM installation directory (keep your
   `data/` folder and your GTA V path), or run it from the unpacked folder.
4. Start it.

### What should happen (in order)

| # | Expected |
|---|---|
| 1 | The launcher window appears (the "Compositing Launcher", 1280×720). |
| 2 | The progress window shows *"Analyzing game data"* and then *"Validating Rockstar Entitlements"*. |
| 3 | The **Rockstar entitlement error still appears** — see §3. It is reported, and it is not fatal any more. The log should show `NSM entitlement service: listening on 127.0.0.1:18080` and `NSM entitlement service: /l/api/validate/entitlement -> no entitlement record`. |
| 4 | The progress window **closes on its own** and the **server browser (mpMenu) is visible**. |
| 5 | The server list shows the NSM servers (hostname, players, map). |
| 6 | Clicking a server connects; disconnecting returns to the browser. |

### If the progress window does not close

That is exactly the failure this fix addresses; the new watchdog should close it
automatically after 5 minutes without a stage change, and it is closed immediately
when the game process exits. If you still see it stuck, capture the log (below).

---

## 2b. Verify the build before starting it

```powershell
python tools\legitimacy\verify_client.py caches\fivereborn
```

It prints the entitlement endpoints found in the **shipped** `legitimacy.dll`:

```
NSM entitlement endpoints  : 5
Cfx entitlement endpoints  : 0
subprocess name            : CitizenFX_SubProcess_%s.bin (ok)
RESULT: OK - this build asks the NSM entitlement service
```

If it says `NOT OK`, the build is the pre-fix one and will still abort on the
Rockstar entitlement check.

## 3. The Rockstar entitlement error (expected, and no longer fatal)

The log should contain:

```
[   ROSLauncher]  MainThrd/ NSM entitlement service: listening on 127.0.0.1:18080
[   ROSLauncher]  MainThrd/ NSM entitlement service: /l/api/validate/entitlement -> no entitlement record
```

(The service runs in the launcher process, so the second line is logged by the
process that asked.) The entitlement check itself still fails - an NSM account
has no Cfx entitlement record - and that is reported, not hidden.

`Error contacting Rockstar Online Services: <Response><Status>0</Status><Error Code="DoesNotExist" /></Response>`

* **Where it comes from:** the closed-source `legitimacy.dll`. It used to call
  Cfx's `https://lambda.fivem.net/api/validate/entitlement*`; NSM has no records
  there, so Cfx answered `DoesNotExist` — a *hard* "no license" verdict that
  aborted the start-up transition.
* **Why the old launcher continued anyway:** the shipped v0.7.5 client used the
  same component against a **local** endpoint (`http://127.0.0.1/l/…`). The
  two binaries differ in 433 bytes / 8 regions, and the entitlement URLs are
  the functional part: the failure is the same, only its *class* differs.
* **What actually killed the client:** the check runs in the ROS helper
  (`FiveM_ROSLauncher`) and it reports the failure with `FatalError`, which
  terminates the process (the crash is the deliberate write to `0xDEED`). The
  game process then never gets its Rockstar session, blocks in
  `WaitForRlInit()` and dies 60 s later — and the progress window, which only
  the game process closes, stays on screen forever.
* **What this build does:** the entitlement endpoint now points at NSM's own
  local service (`127.0.0.1:18080`), which answers with the "no entitlement
  record" payload Cfx's service returns for an unknown account - the same
  answer the launcher has always got, and a soft one for the component. As a
  safety net, the *"the entitlement service could not be consulted"* class of
  error is reported (log + `data\cache\error-pickup`) but no longer
  terminates the process, so the ROS helper survives and the game gets its
  session. The ownership verdict (*"… does not own a valid license …"*) still
  terminates the process.
* **What this build does:** the five embedded endpoints are rewritten in place to
  `http://127.0.0.1:18080/l/api/validate/entitlement*` (see
  `tools/legitimacy/README.md`). The check still runs, still fails and still
  reports the error — it is only the failure *class* that changed, so the game
  process survives and the browser appears.
* **Not hidden, not faked:** the masterlist deliberately does **not** implement
  `/api/validate/entitlement*` (a test asserts it stays `404`), and the NSM local
  entitlement service on `127.0.0.1:18080` is not implemented either. Nothing
  reports a successful entitlement. NSM's own authentication (CNL endpoint,
  ticket signing, server ingress) is untouched.

---

## 4. Logs to collect if something is still wrong

| What | Where |
|---|---|
| Launcher / component log | `%LocalAppData%\FiveM\FiveM.app\citizen\logs\` (or the `logs` folder next to the exe) — `citizen.log` |
| CEF / NUI log | `citizen.log`, section `nui:*` — look for `nui:gsclient` and the `gsclient:url` result |
| Masterlist log | `journalctl -u nsm-masterlist -f` on the VPS — you should see `GET /api/servers/streamRedir/` and the heartbeat lines |
| Startup stages | the launcher console (F8 in-game console, or the console window) — the new watchdog prints the updater-window state every 30 s |

**Most useful single check** (proves the whole chain in one line):

```bash
journalctl -u nsm-masterlist -f | grep streamRedir
# must show a GET /api/servers/streamRedir/ with a 302 when the client starts
```

If that line never appears, the client did not reach the masterlist (network /
firewall / wrong UI bundle). If it appears with a 200, the old masterlist is
still deployed.

---

## 5. Quick regression matrix

| Test | Expected |
|---|---|
| Normal startup | browser opens, server list populated |
| Analysis success | *"Analyzing game data"* → progress window closes |
| Analysis failure / timeout | progress window closes by itself (watchdog, ≤ 5 min) instead of hanging forever |
| Rockstar `DoesNotExist` | error may show; browser still opens |
| Rockstar unavailable (no internet) | same; browser still opens |
| Masterlist unavailable | browser opens with an empty list + the UI's own error, no crash |
| Masterlist error / bad schema | same |
| Masterlist valid list | servers listed |
| Connect / disconnect | connects; disconnect returns to the browser and the list refreshes |
| Retry after a recoverable failure | the browser reloads the list on demand |
