#!/usr/bin/env python3
"""Verify an NSM client build (the folder produced by the CI artifact).

Usage:
    python3 tools/legitimacy/verify_client.py <path-to-client-folder>

The client folder is the ``caches/fivereborn`` directory of the
``nsm-client-<sha>`` CI artifact (or an installed NSM client). The script
checks the *shipped* ``legitimacy.dll`` for the entitlement endpoints, so a
build can be validated before it is started.

Exit code 0 = the build is the expected one, 1 = it is not.
"""

import os
import sys

NSM_ENTITLEMENT_PREFIX = b"http://127.0.0.1:18080/l/"
CFX_ENTITLEMENT_PREFIX = b"https://lambda.fivem.net/"

ENTITLEMENT_PATHS = (
    b"api/validate/entitlement",
    b"api/validate/entitlement/rosfive",
    b"api/validate/entitlement/ros2",
    b"api/validate/discourse",
    b"api/validate/discord",
)

# the component looks the game subprocess up under this name; it must stay
# "CitizenFX_SubProcess_%s.bin" because that is what the build emits
EXPECTED_SUBPROCESS = b"C\x00i\x00t\x00i\x00z\x00e\x00n\x00F\x00X\x00_\x00S\x00u\x00b\x00P\x00r\x00o\x00c\x00e\x00s\x00s\x00_\x00%\x00s\x00.\x00b\x00i\x00n"

CANDIDATES = (
    "legitimacy.dll",
    os.path.join("citizen", "legitimacy.dll"),
    os.path.join("components", "legitimacy.dll"),
    os.path.join("citizen", "components", "legitimacy.dll"),
)


def find_component(root):
    for rel in CANDIDATES:
        path = os.path.join(root, rel)
        if os.path.isfile(path):
            return path
    return None


def check(path):
    with open(path, "rb") as fh:
        data = fh.read()

    ok = True
    print("component : %s" % path)
    print("size      : %d bytes" % len(data))

    nsm = data.count(NSM_ENTITLEMENT_PREFIX)
    cfx = data.count(CFX_ENTITLEMENT_PREFIX)

    print("NSM entitlement endpoints  : %d" % nsm)
    print("Cfx entitlement endpoints  : %d" % cfx)

    if nsm == 0:
        print("FAIL: the NSM entitlement endpoint is not in this component -")
        print("      this build still asks Cfx and will abort the startup")
        print("      transition on the Rockstar entitlement check.")
        ok = False

    if cfx:
        print("FAIL: %d Cfx entitlement endpoint(s) left in the component." % cfx)
        ok = False

    for suffix in ENTITLEMENT_PATHS:
        if NSM_ENTITLEMENT_PREFIX + suffix not in data:
            print("FAIL: missing endpoint %s" % (NSM_ENTITLEMENT_PREFIX + suffix).decode())
            ok = False

    if EXPECTED_SUBPROCESS not in data:
        print("FAIL: the CitizenFX_SubProcess_%%s.bin string is missing - the")
        print("      component would not find the game subprocess.")
        ok = False
    else:
        print("subprocess name           : CitizenFX_SubProcess_%s.bin (ok)")

    if b"Validating Rockstar Entitlements" not in data:
        print("WARN: the entitlement progress text is missing - is this the")
        print("      right component?")

    print("")
    print("RESULT: %s" % ("OK - this build asks the NSM entitlement service" if ok
                          else "NOT OK"))
    return ok


def main(argv):
    if len(argv) != 2:
        print(__doc__)
        return 2

    root = argv[1]
    if not os.path.isdir(root):
        print("not a directory: %s" % root)
        return 2

    path = find_component(root)
    if not path:
        print("no legitimacy.dll found under %s" % root)
        print("looked for: %s" % ", ".join(CANDIDATES))
        return 1

    return 0 if check(path) else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
