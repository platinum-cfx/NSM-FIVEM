#!/usr/bin/env python3
"""
Point the closed-source `legitimacy` component at the NSM entitlement endpoint.

Why
---
The `legitimacy` component (code/components/legitimacy) is a closed-source Cfx
build. It hardcodes the Cfx entitlement service:

    https://lambda.fivem.net/api/validate/entitlement
    https://lambda.fivem.net/api/validate/entitlement/rosfive
    https://lambda.fivem.net/api/validate/entitlement/ros2
    https://lambda.fivem.net/api/validate/discourse
    https://lambda.fivem.net/api/validate/discord

NSM has no Cfx entitlement records, so that service answers

    <Response><Status>0</Status><Error Code="DoesNotExist" /></Response>

which the component treats as a hard "this account owns no license" verdict.
It then aborts the startup transition, so the launcher never gets past
"Validating Rockstar Entitlements" / "Analyzing game data" and the server
browser - which lives in the game process - never appears.

The NSM v0.7.5 client, the build that still reached the server browser, used
the same component rebuilt against a *local* NSM endpoint:

    http://127.0.0.1/l/api/validate/entitlement

There the request fails as a *network* error ("could not contact the
entitlement service"), which the component classifies as recoverable, so the
browser initialises. Nothing is faked: the check still runs, still fails, and
still reports "Error contacting Rockstar Online Services"; it simply cannot be
answered by Cfx, because Cfx does not know about NSM accounts.

What this script does
---------------------
It rewrites the *prefix* of the five embedded URLs, in place, from

    https://lambda.fivem.net/     (25 bytes)

to

    http://127.0.0.1:18080/l/    (25 bytes)

The replacement is exactly the same length as the original, so:

  * the file size does not change,
  * no byte after the patched region moves, so no pointer/relocation in the
    binary can break,
  * the embedded strings keep their original length.

The NSM local entitlement service is expected on 127.0.0.1:18080. It is not
implemented yet, so the request fails with a connection error, which is the
recoverable class of failure that keeps the browser reachable. When NSM ships
a real entitlement service it must answer with a verdict there.

This does not touch the entitlement check itself: it is still performed, its
result is still reported, and it still fails. Only the *class* of the failure
changes (network error instead of "no license"), which is what lets the
browser start. NSM's own authentication (CNL endpoint, ticket signing,
server ingress) is untouched.

Usage:
    python3 tools/legitimacy/patch_entitlement_endpoint.py            # patch in place
    python3 tools/legitimacy/patch_entitlement_endpoint.py --check    # verify only
    python3 tools/legitimacy/patch_entitlement_endpoint.py --revert   # undo
"""

import argparse
import os
import sys

CFX_PREFIX = b"https://lambda.fivem.net/"
NSM_PREFIX = b"http://127.0.0.1:18080/l/"

DEFAULT_TARGETS = [
    os.path.join("code", "components", "legitimacy", "release", "legitimacy.dll"),
    os.path.join("code", "components", "legitimacy", "debug", "legitimacy.dll"),
]

# Same length => pure in-place rewrite, no relocation of any following byte.
assert len(CFX_PREFIX) == len(NSM_PREFIX), "the prefixes must have the same length"


def patch_bytes(data):
    """Return (patched_bytes, replaced_count)."""
    count = data.count(CFX_PREFIX)
    return data.replace(CFX_PREFIX, NSM_PREFIX), count


def revert_bytes(data):
    """Return (reverted_bytes, replaced_count)."""
    count = data.count(NSM_PREFIX)
    return data.replace(NSM_PREFIX, CFX_PREFIX), count


def describe(data):
    """Return the entitlement URLs currently embedded in the binary."""
    urls = []
    for prefix in (CFX_PREFIX, NSM_PREFIX):
        i = 0
        while True:
            j = data.find(prefix, i)
            if j == -1:
                break
            end = data.find(b"\x00", j)
            if end == -1:
                end = j + 80
            urls.append(data[j:end].decode("ascii", "replace"))
            i = j + 1
    return sorted(set(urls))


def process(path, mode):
    with open(path, "rb") as f:
        original = f.read()

    if mode == "check":
        patched, count = patch_bytes(original)
        if count == 0 and NSM_PREFIX in original:
            state, ok = "patched (NSM endpoint)", True
        elif count == 0:
            state, ok = "NOT PATCHED (no NSM endpoint found)", False
        else:
            state, ok = "NOT PATCHED (%d Cfx endpoints found)" % count, False
        print("%-70s %s" % (path, state))
        for u in describe(original):
            print("      %s" % u)
        return ok

    if mode == "revert":
        new, count = revert_bytes(original)
    else:
        new, count = patch_bytes(original)

    if count == 0:
        print("%-70s nothing to do" % path)
        return True

    if len(new) != len(original):
        raise SystemExit("%s: patch would change the file size" % path)

    with open(path, "wb") as f:
        f.write(new)

    print("%-70s %s (%d endpoint%s), size unchanged (%d bytes)"
          % (path, "reverted" if mode == "revert" else "patched",
             count, "" if count == 1 else "s", len(new)))
    for u in describe(new):
        print("      %s" % u)
    return True


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("files", nargs="*", default=None,
                    help="legitimacy DLLs to patch (default: the ones in the repo)")
    ap.add_argument("--check", action="store_true",
                    help="only report the state, do not modify anything")
    ap.add_argument("--revert", action="store_true",
                    help="restore the original Cfx endpoint")
    args = ap.parse_args()

    mode = "check" if args.check else ("revert" if args.revert else "patch")
    files = args.files or DEFAULT_TARGETS

    ok = True
    for path in files:
        if not os.path.isfile(path):
            print("%-70s MISSING" % path)
            ok = False
            continue
        ok = process(path, mode) and ok

    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
