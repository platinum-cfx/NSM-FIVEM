#!/usr/bin/env python3
"""
Regression tests for the `legitimacy` entitlement-endpoint patch.

Run: python3 tools/legitimacy/tests/test_entitlement_patch.py
"""

import importlib.util
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))

PATCH_SCRIPT = os.path.join(REPO, "tools", "legitimacy", "patch_entitlement_endpoint.py")

spec = importlib.util.spec_from_file_location("patch_entitlement_endpoint", PATCH_SCRIPT)
patch_mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(patch_mod)

TARGETS = [
    os.path.join(REPO, "code", "components", "legitimacy", "release", "legitimacy.dll"),
    os.path.join(REPO, "code", "components", "legitimacy", "debug", "legitimacy.dll"),
]

EXPECTED_URLS = {
    "http://127.0.0.1:18080/l/api/validate/entitlement",
    "http://127.0.0.1:18080/l/api/validate/entitlement/rosfive",
    "http://127.0.0.1:18080/l/api/validate/entitlement/ros2",
    "http://127.0.0.1:18080/l/api/validate/discourse",
    "http://127.0.0.1:18080/l/api/validate/discord",
}


def read(path):
    with open(path, "rb") as f:
        return f.read()


class TestEntitlementEndpointPatch(unittest.TestCase):
    def test_prefixes_have_the_same_length(self):
        # A same-length rewrite guarantees that no byte after the patched
        # region moves, so no pointer inside the PE can break.
        self.assertEqual(len(patch_mod.CFX_PREFIX), len(patch_mod.NSM_PREFIX))

    def test_committed_components_use_the_nsm_endpoint(self):
        for path in TARGETS:
            with self.subTest(variant=os.path.basename(os.path.dirname(path))):
                self.assertTrue(os.path.isfile(path), "%s is missing" % path)
                data = read(path)
                self.assertFalse(patch_mod.CFX_PREFIX in data,
                                 "%s still points at the Cfx entitlement service" % path)
                self.assertEqual(set(patch_mod.describe(data)), EXPECTED_URLS)

    def test_patch_is_idempotent(self):
        for path in TARGETS:
            with self.subTest(variant=os.path.basename(os.path.dirname(path))):
                data = read(path)
                again, count = patch_mod.patch_bytes(data)
                self.assertEqual(count, 0)
                self.assertEqual(again, data)

    def test_patch_reverts_exactly(self):
        for path in TARGETS:
            with self.subTest(variant=os.path.basename(os.path.dirname(path))):
                patched = read(path)
                reverted, count = patch_mod.revert_bytes(patched)
                self.assertEqual(count, 5)
                self.assertEqual(len(reverted), len(patched))
                # re-patching the reverted bytes gives the original file back
                again, count2 = patch_mod.patch_bytes(reverted)
                self.assertEqual(count2, 5)
                self.assertEqual(again, patched)

    def test_patch_touches_nothing_but_the_urls(self):
        for path in TARGETS:
            with self.subTest(variant=os.path.basename(os.path.dirname(path))):
                patched = read(path)
                reverted, _ = patch_mod.revert_bytes(patched)
                diffs = [i for i in range(len(patched)) if patched[i] != reverted[i]]
                self.assertTrue(diffs, "expected the endpoints to differ")
                # every differing byte must lie inside one of the five prefixes
                regions = []
                for url in EXPECTED_URLS:
                    start = patched.find(url.encode("ascii"))
                    self.assertNotEqual(start, -1, "missing %s" % url)
                    regions.append((start, start + len(patch_mod.CFX_PREFIX)))
                outside = [i for i in diffs
                           if not any(a <= i < b for (a, b) in regions)]
                self.assertEqual(outside, [],
                                 "bytes outside of an endpoint prefix changed")

    def test_subprocess_name_is_preserved(self):
        # The component uses this string to find the game subprocess; the
        # endpoint patch must not touch it.
        for path in TARGETS:
            with self.subTest(variant=os.path.basename(os.path.dirname(path))):
                data = read(path)
                found = (b"CitizenFX_SubProcess_%s.bin" in data
                         or "CitizenFX_SubProcess_%s.bin".encode("utf-16-le") in data)
                self.assertTrue(found, "the subprocess name is gone")

    def test_entitlement_check_is_not_faked(self):
        # The component must still report the failure - the patch only moves
        # the endpoint, it never makes the check succeed.
        for path in TARGETS:
            with self.subTest(variant=os.path.basename(os.path.dirname(path))):
                data = read(path)
                for needle in (b"Error contacting Rockstar Online Services",
                               b"Validating Rockstar Entitlements",
                               b"<Status>0</Status>"):
                    found = (needle in data
                             or needle.decode("ascii").encode("utf-16-le") in data)
                    self.assertTrue(found, "%r is missing" % needle)

    def test_no_cfx_entitlement_url_anywhere(self):
        for path in TARGETS:
            with self.subTest(variant=os.path.basename(os.path.dirname(path))):
                data = read(path)
                self.assertFalse(b"lambda.fivem.net/api/validate" in data)


if __name__ == "__main__":
    unittest.main(verbosity=2)
