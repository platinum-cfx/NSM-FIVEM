#!/usr/bin/env python3
"""
Regression tests for the NSM masterlist service.

Covers the client-facing contract that the NSM client (nui:gsclient) relies on:

  * "gsclient:url" performs the request with followLocation = false and hands
    the UI the response's Location header.  If /api/servers/streamRedir/
    answers 200 instead of 302, the UI receives an empty string and the
    server browser can never load a server list.
  * The protobuf stream framing the cfx-ui worker expects:
    [4-byte LE length][protobuf master.Server] repeated.

Run:  python3 tools/masterlist/tests/test_masterlist.py
"""

import json
import os
import struct
import sys
import threading
import unittest
import urllib.error
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

import masterlist  # noqa: E402


def _decode_varint(buf, pos):
    result = 0
    shift = 0
    while True:
        b = buf[pos]
        pos += 1
        result |= (b & 0x7F) << shift
        if not (b & 0x80):
            return result, pos
        shift += 7


def decode_server(payload):
    """Decode one master.Server protobuf frame into (EndPoint, ServerData-ish)."""
    end_point = ""
    data = {}
    pos = 0
    while pos < len(payload):
        key, pos = _decode_varint(payload, pos)
        field, wire = key >> 3, key & 7
        if wire == 2:
            length, pos = _decode_varint(payload, pos)
            chunk = payload[pos:pos + length]
            pos += length
            if field == 1:
                end_point = chunk.decode("utf-8")
            elif field == 2:
                data = _decode_server_data(chunk)
        elif wire == 0:
            value, pos = _decode_varint(payload, pos)
        elif wire == 5:
            pos += 4
        elif wire == 1:
            pos += 8
        else:
            raise AssertionError("unexpected wire type %d" % wire)
    return end_point, data


def _decode_server_data(payload):
    out = {}
    pos = 0
    while pos < len(payload):
        key, pos = _decode_varint(payload, pos)
        field, wire = key >> 3, key & 7
        if wire == 0:
            value, pos = _decode_varint(payload, pos)
            out[field] = value
        elif wire == 2:
            length, pos = _decode_varint(payload, pos)
            chunk = payload[pos:pos + length]
            pos += length
            if field == 4:
                out["hostname"] = chunk.decode("utf-8")
            elif field == 5:
                out["gametype"] = chunk.decode("utf-8")
            elif field == 6:
                out["mapname"] = chunk.decode("utf-8")
            elif field == 12:
                out.setdefault("vars", {})  # map entry, decoded below
                k, v = _decode_map_entry(chunk)
                out["vars"][k] = v
        elif wire == 5:
            pos += 4
        elif wire == 1:
            pos += 8
    return out


def _decode_map_entry(chunk):
    pos = 0
    key = value = ""
    while pos < len(chunk):
        k, pos = _decode_varint(chunk, pos)
        field, wire = k >> 3, k & 7
        if wire == 2:
            length, pos = _decode_varint(chunk, pos)
            data = chunk[pos:pos + length].decode("utf-8")
            pos += length
            if field == 1:
                key = data
            else:
                value = data
    return key, value


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Mirrors the client's HttpRequestOptions.followLocation = false."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class MasterlistTestCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        from http.server import ThreadingHTTPServer

        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), masterlist.Handler)
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.base = "http://127.0.0.1:%d" % cls.port

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        masterlist.REGISTRY._servers.clear()

    # -- helpers ---------------------------------------------------------
    def get(self, path, method="GET", data=None, headers=None, follow=True):
        req = urllib.request.Request(self.base + path, method=method,
                                     data=data, headers=headers or {})
        opener = urllib.request.build_opener() if follow \
            else urllib.request.build_opener(_NoRedirect)
        try:
            with opener.open(req) as resp:
                return resp.status, dict(resp.headers), resp.read()
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), e.read()

    def heartbeat(self, ip="10.0.0.5", port=30120, hostname="NSM Test Server",
                  clients=3, maxclients=48):
        payload = {
            "port": port,
            "ipOverride": ip,
            "fallbackData": {
                "dynamic": {"clients": clients, "svMaxclients": maxclients,
                            "hostname": hostname, "gametype": "Deathmatch",
                            "mapname": "TestMap"},
                "info": {"version": "1.0.0", "resources": ["script:foo"]},
            },
        }
        status, _, body = self.get("/api/serverlist/ingress", method="POST",
                                    data=json.dumps(payload).encode(),
                                    headers={"Content-Type": "application/json"})
        self.assertEqual(200, status)
        self.assertEqual({}, json.loads(body))

    # -- tests -----------------------------------------------------------
    def test_health(self):
        status, _, body = self.get("/health")
        self.assertEqual(200, status)
        self.assertIn(b"NSM masterlist", body)

    def test_stream_redir_returns_a_redirect_with_location(self):
        """The client resolves streamRedir via a no-follow GET and reads Location.

        This is the contract the nui:gsclient 'gsclient:url' RPC depends on;
        a 200 here means the server browser gets an empty list URL.
        """
        status, headers, _ = self.get("/api/servers/streamRedir/", follow=False)
        self.assertEqual(302, status)
        self.assertIn("location", {k.lower(): v for k, v in headers.items()})
        location = {k.lower(): v for k, v in headers.items()}["location"]
        self.assertTrue(location.endswith("/api/servers/stream/"),
                        "unexpected Location: %s" % location)

    def test_stream_redir_location_is_absolute(self):
        _, headers, _ = self.get("/api/servers/streamRedir/", follow=False)
        location = {k.lower(): v for k, v in headers.items()}["location"]
        self.assertTrue(location.startswith("http://"),
                        "Location must be absolute, got %s" % location)

    def test_stream_redir_public_base_override(self):
        masterlist.PUBLIC_BASE = "http://nsm.example:8080"
        try:
            _, headers, _ = self.get("/api/servers/streamRedir/", follow=False)
            location = {k.lower(): v for k, v in headers.items()}["location"]
            self.assertEqual("http://nsm.example:8080/api/servers/stream/", location)
        finally:
            masterlist.PUBLIC_BASE = ""

    def test_stream_redir_is_followable(self):
        """A client that follows redirects natively must still land on a
        working stream response."""
        status, headers, body = self.get("/api/servers/streamRedir/", follow=True)
        self.assertEqual(200, status)
        self.assertIn("application/octet-stream", headers.get("Content-Type", ""))
        self.assertIsInstance(body, bytes)

    def test_stream_endpoint_serves_protobuf_frames(self):
        self.heartbeat()
        status, headers, body = self.get("/api/servers/stream/")
        self.assertEqual(200, status)
        self.assertIn("application/octet-stream", headers.get("Content-Type", ""))

        # [4-byte LE length][frame] repeated
        pos = 0
        frames = []
        while pos < len(body):
            (length,) = struct.unpack_from("<I", body, pos)
            pos += 4
            frames.append(body[pos:pos + length])
            pos += length
        self.assertEqual(1, len(frames))

        end_point, data = decode_server(frames[0])
        self.assertEqual("10.0.0.5:30120", end_point)
        self.assertEqual("NSM Test Server", data.get("hostname"))
        self.assertEqual(3, data.get(2))        # clients
        self.assertEqual(48, data.get(1))       # svMaxclients
        # the client browser filter is init(["gta5"]) - gamename must be gta5
        self.assertEqual("gta5", data.get("vars", {}).get("gamename"))

    def test_stream_endpoint_empty_when_no_servers(self):
        status, _, body = self.get("/api/servers/stream/")
        self.assertEqual(200, status)
        self.assertEqual(b"", body)

    def test_single_server(self):
        self.heartbeat()
        status, _, body = self.get("/api/servers/single/10.0.0.5:30120")
        self.assertEqual(200, status)
        doc = json.loads(body)
        self.assertEqual("10.0.0.5:30120", doc["EndPoint"])
        self.assertEqual("NSM Test Server", doc["Data"]["hostname"])

    def test_single_server_unknown_is_404(self):
        status, _, _ = self.get("/api/servers/single/1.2.3.4:9999")
        self.assertEqual(404, status)

    def test_counts_json(self):
        self.heartbeat(clients=7)
        status, _, body = self.get("/runtime/counts.json")
        self.assertEqual(200, status)
        self.assertEqual([7, 0, 7], json.loads(body))

    def test_heartbeat_expiry(self):
        self.heartbeat()
        masterlist.HEARTBEAT_TTL = 0.0
        try:
            _, _, body = self.get("/api/servers/stream/")
            self.assertEqual(b"", body)
        finally:
            masterlist.HEARTBEAT_TTL = 300.0

    def test_cors_headers_present(self):
        _, headers, _ = self.get("/api/servers/stream/")
        self.assertEqual("*", headers.get("Access-Control-Allow-Origin"))

    def test_unknown_route_is_404(self):
        status, _, _ = self.get("/api/does-not-exist")
        self.assertEqual(404, status)

    def test_entitlement_routes_are_not_faked(self):
        """The client must not be able to pass an entitlement check by hitting
        an NSM endpoint that always says 'yes'.  NSM has no entitlement store,
        so the route must not exist (404) rather than claim success."""
        status, _, _ = self.get("/api/validate/entitlement")
        self.assertEqual(404, status)
        status, _, _ = self.get("/api/validate/entitlement/rosfive")
        self.assertEqual(404, status)


if __name__ == "__main__":
    unittest.main(verbosity=2)
