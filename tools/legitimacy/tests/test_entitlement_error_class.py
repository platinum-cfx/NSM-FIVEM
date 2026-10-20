#!/usr/bin/env python3
"""Regression tests for the NSM entitlement error classification.

The closed-source ``legitimacy`` component reports the Rockstar entitlement
validation through ``FatalError``, which terminates the process. Two different
things are reported that way and they must not be treated the same:

* "the entitlement service could not be consulted" - expected for every NSM
  account (there is no Cfx entitlement record for it). Recoverable: the
  process has to survive, or the game never gets its Rockstar session and the
  server browser never appears.
* "this account does not own the game" - the real ownership verdict. Stays
  fatal.

The classification lives in ``code/client/shared/EntitlementError.h`` and is
used by ``code/client/common/Error.cpp``. These tests check that the
classifier and the strings that are actually compiled into the component agree.
"""

import os
import re
import unittest

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
HEADER = os.path.join(REPO, "code", "client", "shared", "EntitlementError.h")
ERROR_CPP = os.path.join(REPO, "code", "client", "common", "Error.cpp")
RELEASE_DLL = os.path.join(REPO, "code", "components", "legitimacy", "release", "legitimacy.dll")
DEBUG_DLL = os.path.join(REPO, "code", "components", "legitimacy", "debug", "legitimacy.dll")

# The messages the component can report. The expectations mirror
# EntitlementError.h: the first group is the "service could not be consulted"
# class, the second one is the ownership verdict / Rockstar session failure.
CONTACT_MESSAGES = [
    "Could not contact entitlement service. Status code: %d, error message: %d/%s, response body: %s",
    "Error contacting Rockstar Online Services.",
    "Error contacting Rockstar Online Services: %s -> %s.",
]

OWNERSHIP_MESSAGES = [
    "Could not validate game ownership!",
    "The Social Club account specified does not own a valid license.",
    "The Social Club account specified does not own a valid license to Grand Theft Auto V.",
    "Could not sign on to the Social Club. Error code: %s/%s",
    "Could not get title access token from the Social Club. Error code: %s/%s",
    "Could not get entitlement block from the Social Club. Error code: %s/%s",
]


def read(path):
    with open(path, "rb") as fh:
        return fh.read()


def header_patterns(function_name):
    """Extract the string literals used by one of the classifier functions."""
    source = read(HEADER).decode("utf-8")
    start = source.index(function_name)
    end = source.index("\n}", start)
    body = source[start:end]
    return re.findall(r'"((?:[^"\\]|\\.)*)"', body)


def classify(message):
    """Re-implement the C++ classification from the header patterns."""
    contact = header_patterns("IsEntitlementServiceContactError")
    ownership = header_patterns("IsEntitlementOwnershipError")
    if any(pattern in message for pattern in ownership):
        return "fatal"
    if any(pattern in message for pattern in contact):
        return "recoverable"
    return "fatal"


class TestEntitlementErrorClassification(unittest.TestCase):
    def test_header_exists(self):
        self.assertTrue(os.path.isfile(HEADER), HEADER)

    def test_contact_messages_are_recoverable(self):
        for message in CONTACT_MESSAGES:
            self.assertEqual(classify(message), "recoverable", message)

    def test_ownership_messages_stay_fatal(self):
        for message in OWNERSHIP_MESSAGES:
            self.assertEqual(classify(message), "fatal", message)

    def test_classifier_covers_the_component_messages(self):
        """Every entitlement error the component can print is classified."""
        for dll in (RELEASE_DLL, DEBUG_DLL):
            if not os.path.isfile(dll):
                continue

            data = read(dll)
            strings = {m.group().decode("ascii")
                       for m in re.finditer(rb"[\x20-\x7e]{8,}", data)}

            for message in CONTACT_MESSAGES + OWNERSHIP_MESSAGES:
                self.assertIn(message, strings,
                              "%s is not compiled into %s" % (message, dll))

    def test_error_handler_uses_the_classifier(self):
        """The error handler has to honour the classification."""
        source = read(ERROR_CPP).decode("utf-8")
        self.assertIn("IsEntitlementServiceContactError(buffer)", source)
        self.assertIn("IsEntitlementOwnershipError(buffer)", source)
        self.assertIn("ReportRecoverableEntitlementError(buffer)", source)

    def test_recoverable_path_does_not_terminate(self):
        """The recoverable branch must return 0 - the FatalError macro only
        crashes the process when the error function returns a negative value."""
        source = read(ERROR_CPP).decode("utf-8")
        branch = source[source.index("IsEntitlementServiceContactError(buffer)"):]
        branch = branch[:branch.index("return noReport")]
        self.assertIn("return 0;", branch)


if __name__ == "__main__":
    unittest.main()
