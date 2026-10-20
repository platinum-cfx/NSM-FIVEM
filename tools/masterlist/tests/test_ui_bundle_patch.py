#!/usr/bin/env python3
"""
Regression tests for the NSM UI bundle URL rewriting.

The server browser is the closed-source cfx-ui bundle. The bundle is fetched
at build time and patched in place (ext/ui-build/patch_ui_urls.ps1), because
there is no other way to re-point it at the NSM services.

These tests guard the replacement table and the endpoint map against drift:

  * every Cfx service host the client needs must be rewritten to the NSM base
  * the rewritten base must be absolute and must match the endpoint map
  * the bundle must keep working for the endpoints the browser resolves
    through the nui:gsclient "gsclient:url" RPC (redirect + Location)

Run:  python3 tools/masterlist/tests/test_ui_bundle_patch.py
"""

import os
import re
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))

PATCHER = os.path.join(REPO, "ext", "ui-build", "patch_ui_urls.ps1")
PATCHER_REBRAND = os.path.join(REPO, "ext", "ui-build", "patch_ui_bundle.ps1")
ENDPOINT_MAP = os.path.join(REPO, "docs", "nsm-endpoints.md")


def read(path):
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def parse_replacements(text):
    """Extract the (old, new) pairs from the PowerShell replacement table.

    Handles both table styles used in this repo:
        'old' = 'new'                 (hashtable)
        'old' = $NewBase              (hashtable, variable)
        'old' = "$NewBase/some/path"  (hashtable, interpolated)
        @('old', 'new'),              (array of pairs)
    """
    # resolve the script's parameters so that variable references become
    # concrete URLs
    variables = {}
    for m in re.finditer(r"\$(\w+)\s*=\s*\"([^\"]*)\"", text):
        variables[m.group(1)] = m.group(2)

    def resolve(value):
        for name, val in variables.items():
            value = value.replace("$" + name, val)
        return value

    pairs = {}

    for m in re.finditer(r"'((?:[^'\\]|\\.)*)'\s*=\s*'((?:[^'\\]|\\.)*)'", text):
        pairs[m.group(1)] = resolve(m.group(2))

    for m in re.finditer(r"'((?:[^'\\]|\\.)*)'\s*=\s*\$(\w+)", text):
        pairs[m.group(1)] = resolve("$" + m.group(2))

    for m in re.finditer(r"'((?:[^'\\]|\\.)*)'\s*=\s*\"([^\"]*)\"", text):
        pairs[m.group(1)] = resolve(m.group(2))

    for m in re.finditer(r"@\(\s*'((?:[^'\\]|\\.)*)'\s*,\s*'((?:[^'\\]|\\.)*)'\s*\)", text):
        pairs[m.group(1)] = resolve(m.group(2))

    return pairs


class UiBundlePatchTestCase(unittest.TestCase):
    def setUp(self):
        self.assertTrue(os.path.exists(PATCHER),
                        "missing %s" % PATCHER)
        self.text = read(PATCHER)
        self.pairs = parse_replacements(self.text)

    def test_table_is_parsed(self):
        self.assertGreater(len(self.pairs), 0,
                           "no replacements parsed out of the patcher")

    def test_cfx_service_hosts_are_rewritten(self):
        """Every Cfx service host the client uses has to be redirected,
        otherwise the browser keeps talking to Cfx."""
        required = [
            "https://frontend.cfx-services.net",
            "https://lambda.fivem.net",
            "https://servers-frontend.fivem.net",
            "https://gss.cfx-services.net",
        ]

        for host in required:
            self.assertIn(host, self.pairs,
                          "%s is not rewritten by the UI bundle patcher" % host)
            self.assertNotIn("cfx", self.pairs[host].replace("cfx.re", ""),
                             "%s still points at a Cfx host: %s"
                             % (host, self.pairs[host]))

    def test_rewritten_base_is_absolute(self):
        for old, new in self.pairs.items():
            if old.startswith("http"):
                self.assertTrue(new.startswith("http://") or new.startswith("https://"),
                                "replacement for %s is not absolute: %s" % (old, new))

    def test_all_replacements_use_the_same_base(self):
        """Everything that used to be a Cfx service host must now point at
        the same NSM base, so the endpoint map stays truthful."""
        bases = set()
        for old, new in self.pairs.items():
            if old.startswith("http") and "cfx" in old:
                m = re.match(r"(https?://[^/]+)", new)
                self.assertIsNotNone(m, "cannot parse base of %s" % new)
                bases.add(m.group(1))

        self.assertEqual(1, len(bases),
                         "rewrites point at different bases: %s" % sorted(bases))

    def test_counts_url_keeps_its_path(self):
        old = "https://static.cfx.re/runtime/counts.json"
        self.assertIn(old, self.pairs)
        self.assertTrue(self.pairs[old].endswith("/runtime/counts.json"),
                        "counts.json path was lost: %s" % self.pairs[old])

    def test_patch_is_idempotent_on_already_patched_content(self):
        """Re-running the patcher on an already patched bundle must not
        corrupt it (the build runs it on a freshly downloaded bundle, but the
        script is also run by hand)."""
        for old, new in self.pairs.items():
            once = new
            twice = once.replace(old, new)
            self.assertEqual(once, twice,
                             "replacement for %s is not idempotent" % old)

    def test_endpoint_map_documents_the_browser_endpoints(self):
        doc = read(ENDPOINT_MAP)
        for route in [
            "/api/servers/streamRedir/",
            "/api/servers/stream/",
            "/api/servers/single/",
            "/api/serverlist/ingress",
            "/api/ticket/pubkey",
        ]:
            self.assertIn(route, doc,
                          "%s is not documented in the endpoint map" % route)

    def test_endpoint_map_documents_the_redirect_contract(self):
        doc = read(ENDPOINT_MAP)
        self.assertIn("Location", doc,
                      "the endpoint map must document the Location header contract")
        self.assertIn("followLocation = false", doc)

    def test_rebrand_patcher_if_present_uses_the_same_hosts(self):
        if not os.path.exists(PATCHER_REBRAND):
            self.skipTest("rebrand patcher not present on this branch")

        pairs = parse_replacements(read(PATCHER_REBRAND))
        for host in ["https://frontend.cfx-services.net",
                     "https://servers-frontend.fivem.net"]:
            self.assertIn(host, pairs,
                          "%s missing from the rebrand patcher" % host)


if __name__ == "__main__":
    unittest.main(verbosity=2)
