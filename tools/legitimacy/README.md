# NSM `legitimacy` component — entitlement endpoint

`code/components/legitimacy/` holds a **closed-source** Cfx component. It performs
the Rockstar entitlement check and drives the "Validating Rockstar Entitlements"
progress window.

## The problem

The component hardcodes the Cfx entitlement service:

```
https://lambda.fivem.net/api/validate/entitlement
https://lambda.fivem.net/api/validate/entitlement/rosfive
https://lambda.fivem.net/api/validate/entitlement/ros2
https://lambda.fivem.net/api/validate/discourse
https://lambda.fivem.net/api/validate/discord
```

NSM has no Cfx entitlement records, so Cfx answers

```xml
<Response><Status>0</Status><Error Code="DoesNotExist" /></Response>
```

The component treats that as a hard *"this account owns no license"* verdict and
aborts the startup transition. The launcher then never gets past
*"Validating Rockstar Entitlements"* / *"Analyzing game data"*, and the server
browser — which lives in the game process — never appears.

## Why the old client worked anyway

The NSM v0.7.5 client (the build that still reached the server browser) used the
same component rebuilt against a **local** endpoint:

```
http://127.0.0.1/l/api/validate/entitlement
```

There the request fails as a *network* error ("could not contact the entitlement
service"), which the component classifies as **recoverable**, so the startup
continues and the browser comes up. That is why the Rockstar entitlement error
was visible even in the working build — the error was always there, only the
*consequence* of it changed.

A byte-level comparison of the two binaries (`1f10baf6…` = shipped v0.7.5,
`b15f1101…` = `main`) shows **433 changed bytes** in 8 regions: the five
entitlement URLs, two `forum.cfx.re` help links, the subprocess name and the
error-message format.

## The fix

`patch_entitlement_endpoint.py` rewrites the URL **prefix** in place:

```
https://lambda.fivem.net/     (25 bytes)
http://127.0.0.1:18080/l/    (25 bytes)
```

The replacement is exactly the same length, so the file size does not change and
**no byte after the patched region moves** — no pointer inside the PE can break.
Nothing else in the binary is touched (the `CitizenFX_SubProcess_%s.bin` string
that the component uses to locate the game subprocess is preserved).

```bash
python3 tools/legitimacy/patch_entitlement_endpoint.py           # patch
python3 tools/legitimacy/patch_entitlement_endpoint.py --check   # verify
python3 tools/legitimacy/patch_entitlement_endpoint.py --revert  # undo
python3 tools/legitimacy/tests/test_entitlement_patch.py          # tests
```

## What this does *not* do

* It does **not** fake a successful entitlement check. The check still runs, it
  still fails, and the component still reports
  `Error contacting Rockstar Online Services`.
* It does **not** bypass NSM's own authentication. The CNL endpoint, the ticket
  signing key and the server ingress heartbeat are untouched.
* It does **not** make the Cfx ownership gate pass — it cannot. Cfx does not
  know about NSM accounts.

The NSM local entitlement service is expected on `127.0.0.1:18080` and is **not
implemented yet**; until it is, the request fails with a connection error, which
is the recoverable class of failure that keeps the browser reachable. When NSM
ships a real entitlement service it must answer with a verdict there, and the
ownership gate becomes meaningful again.
