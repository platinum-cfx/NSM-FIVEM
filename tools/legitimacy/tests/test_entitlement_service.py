#!/usr/bin/env python3
"""Regression tests for NSM's local entitlement service.

The closed-source `legitimacy` component asks an HTTP endpoint whether the
signed in Rockstar account has an entitlement. For Cfx that endpoint is
Cfx's own service; NSM points it at its own local service instead (see
`tools/legitimacy/patch_entitlement_endpoint.py`).

The service has to answer with the payload Cfx's service returns for an
account without an entitlement record - that is the honest answer for an NSM
account and it keeps the component on the same code path it used before the
endpoint was moved.

These tests make sure the three parts stay in sync:

* the DLL that ships with the client,
* the service in `code/client/launcher/EntitlementService.cpp`,
* the payload Cfx's service is known to return.
"""

import os
import re
import unittest

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
SERVICE_CPP = os.path.join(REPO, "code", "client", "launcher", "EntitlementService.cpp")
RELEASE_DLL = os.path.join(REPO, "code", "components", "legitimacy", "release", "legitimacy.dll")
DEBUG_DLL = os.path.join(REPO, "code", "components", "legitimacy", "debug", "legitimacy.dll")

# The payload Cfx's entitlement service returns for an account that has no
# entitlement record. It was observed verbatim in the launcher's log
# ("Obtained error from CPR: - 403 - Error contacting Rockstar Online
# Services: -> <?xml ...><Response><Status>0</Status><Error
# Code="DoesNotExist" /></Response>") and that answer is what the launcher
# used to get before the endpoint was moved to NSM.
EXPECTED_PAYLOAD = (
    b'<?xml version="1.0" encoding="utf-8"?>'
    b'<Response><Status>0</Status><Error Code="DoesNotExist" /></Response>'
)

# The endpoint the component is patched to use.
EXPECTED_PREFIX = b"http://127.0.0.1:18080/l/api/validate/entitlement"


def read(path):
    with open(path, "rb") as fh:
        return fh.read()


class TestEntitlementService(unittest.TestCase):
    def setUp(self):
        self.service = read(SERVICE_CPP).decode("utf-8")

    def test_service_binds_the_patched_endpoint(self):
        self.assertIn('constexpr const char* kBindAddress = "127.0.0.1";', self.service)
        self.assertIn("constexpr unsigned short kBindPort = 18080;", self.service)
        # it must only ever listen on the loopback interface
        self.assertIn("htonl(INADDR_LOOPBACK)", self.service)

    def test_service_answers_the_entitlement_paths(self):
        self.assertIn('const std::string prefix = "/l/api/validate/entitlement";', self.service)

    def test_service_payload_is_the_cfx_payload(self):
        match = re.search(r'kNoEntitlementRecord\s*=\s*\n?\s*"(.*?)";', self.service, re.S)
        self.assertIsNotNone(match, "kNoEntitlementRecord not found")

        payload = match.group(1)
        payload = payload.replace('\\"', '"')

        self.assertEqual(payload, EXPECTED_PAYLOAD.decode("utf-8"))

    def test_service_never_claims_an_entitlement(self):
        """The service must not answer "entitled" - that would fake the
        ownership check instead of reporting that there is no record."""
        self.assertNotIn("<Status>1</Status>", self.service)
        self.assertNotIn("EntitlementCode", self.service)

    def test_shipped_dll_points_at_the_service(self):
        for dll in (RELEASE_DLL, DEBUG_DLL):
            if not os.path.isfile(dll):
                continue

            self.assertIn(EXPECTED_PREFIX, read(dll),
                          "%s does not point at the local entitlement service" % dll)

    def test_shipped_dll_does_not_point_at_cfx(self):
        for dll in (RELEASE_DLL, DEBUG_DLL):
            if not os.path.isfile(dll):
                continue

            self.assertNotIn(b"https://lambda.fivem.net/", read(dll),
                             "%s still sends the Rockstar session to Cfx" % dll)

    def test_service_is_started_by_the_master_process(self):
        main = read(os.path.join(REPO, "code", "client", "launcher", "Main.cpp")).decode("utf-8")
        self.assertIn("nsm::StartEntitlementService();", main)
        self.assertIn("initState->IsMasterProcess()", main)


if __name__ == "__main__":
    unittest.main()
