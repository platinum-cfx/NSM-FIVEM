#!/usr/bin/env python3
"""
NSM masterlist service
======================
Drop-in replacement for the Cfx masterlist endpoints used by NSM clients
and servers. Single file, Python 3.8+ standard library only.

Serves (all on one port, default 80):

  Server browser (UI bundle, after patch_ui_urls.ps1):
    GET  /api/servers/streamRedir/            302 redirect to the stream
                                              endpoint (the client resolves
                                              this via the nui:gsclient
                                              "gsclient:url" RPC, which
                                              does NOT follow redirects and
                                              returns the Location header)
    GET  /api/servers/stream/                 full list as a stream of
                                              [4-byte LE length][protobuf master.Server]
    GET  /api/servers/single/<ip:port>        single server as JSON {EndPoint, Data}
    GET  /api/servers/icon/<ep>/<v>.png       server icon (404 -> default icon)
    GET  /api/identities                      player identities (empty)
    GET  /api/ticket/identities               ticket identities (empty)
    GET  /v1/public/featured-servers          featured servers (empty)
    GET  /v1/public/notices/fivem             notices (empty)
    GET  /v1/public/tweets                    tweets (empty)
    GET  /runtime/counts.json                 player counts [0, 0, 0]

  Server side:
    POST /api/serverlist/ingress              server heartbeat (GameServer.cpp)
    GET  /api/ticket/pubkey                   RSA public key (unused with sv_lan true)
    GET  /v1/public/pool-size-limits/fivem    pool size limits (empty = unlimited)

Entries expire 5 minutes after their last heartbeat.
vars.gamename is forced to "gta5" so the client browser filter (init(["gta5"]))
shows the servers.

Run:  sudo python3 masterlist.py            (port 80)
      python3 masterlist.py --port 8080
"""

import argparse
import base64
import json
import socket
import struct
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HEARTBEAT_TTL = 300.0
GAMENAME = "gta5"
MAX_HEARTBEAT_BYTES = 1024 * 1024

# Absolute base URL advertised to clients. Empty = derive from the request
# (Host header + scheme), which is correct for a single-host deployment.
PUBLIC_BASE = ""

# ---------------------------------------------------------------------------
# protobuf encoding (schema extracted from the cfx-ui worker bundle:
# master.Server { string EndPoint = 1; master.ServerData Data = 2; }
# master.ServerData {
#   int32 svMaxclients = 1; int32 clients = 2; int32 protocol = 3;
#   string hostname = 4; string gametype = 5; string mapname = 6;
#   repeated string resources = 8; string server = 9;
#   map<string,string> vars = 12; bool enhancedHostSupport = 16;
#   int32 upvotePower = 17; repeated string connectEndPoints = 18;
#   int32 burstPower = 19; })
# ---------------------------------------------------------------------------

def _varint(n):
    # Protobuf signed integers use a sign-extended, unsigned 64-bit varint.
    # Right-shifting a negative Python integer otherwise never reaches zero.
    if not -(1 << 63) <= n < (1 << 64):
        raise ValueError("integer outside protobuf 64-bit range")
    n &= (1 << 64) - 1
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def _tag(field, wire):
    return _varint((field << 3) | wire)


def _pb_str(field, s):
    if s is None:
        return b""
    b = s.encode("utf-8") if isinstance(s, str) else bytes(s)
    return _tag(field, 2) + _varint(len(b)) + b


def _pb_int(field, v):
    if v is None:
        return b""
    return _tag(field, 0) + _varint(int(v))


def _pb_msg(field, payload):
    return _tag(field, 2) + _varint(len(payload)) + payload


def encode_server_data(d):
    out = b""
    out += _pb_int(1, d.get("svMaxclients"))
    out += _pb_int(2, d.get("clients"))
    out += _pb_int(3, d.get("protocol"))
    out += _pb_str(4, d.get("hostname", ""))
    out += _pb_str(5, d.get("gametype", ""))
    out += _pb_str(6, d.get("mapname", ""))
    for r in d.get("resources") or []:
        out += _pb_str(8, r)
    out += _pb_str(9, d.get("server", ""))
    for k, v in (d.get("vars") or {}).items():
        out += _pb_msg(12, _pb_str(1, k) + _pb_str(2, "" if v is None else str(v)))
    if d.get("enhancedHostSupport"):
        out += _tag(16, 0) + b"\x01"
    out += _pb_int(17, d.get("upvotePower", 0))
    for c in d.get("connectEndPoints") or []:
        out += _pb_str(18, c)
    out += _pb_int(19, d.get("burstPower", 0))
    return out


def encode_server(end_point, data):
    return _pb_str(1, end_point) + _pb_msg(2, encode_server_data(data))


def stream_bytes(entries):
    out = bytearray()
    for ep, data in entries:
        payload = encode_server(ep, data)
        out += struct.pack("<I", len(payload)) + payload
    return bytes(out)


# ---------------------------------------------------------------------------
# registry
# ---------------------------------------------------------------------------

class Registry:
    def __init__(self):
        self._lock = threading.Lock()
        self._servers = {}  # EndPoint -> {"data": {...}, "last": ts}

    def upsert(self, end_point, data):
        with self._lock:
            self._servers[end_point] = {"data": data, "last": time.time()}

    def entries(self):
        now = time.time()
        with self._lock:
            dead = [ep for ep, s in self._servers.items() if now - s["last"] > HEARTBEAT_TTL]
            for ep in dead:
                del self._servers[ep]
            return [(ep, s["data"]) for ep, s in sorted(self._servers.items())]

    def get(self, end_point):
        now = time.time()
        with self._lock:
            s = self._servers.get(end_point)
            if not s or now - s["last"] > HEARTBEAT_TTL:
                return None
            return s["data"]


REGISTRY = Registry()


# ---------------------------------------------------------------------------
# heartbeat -> ServerData mapping
# ---------------------------------------------------------------------------

def _num(x, default=0):
    try:
        return int(x)
    except (TypeError, ValueError, OverflowError):
        return default


def _client_count(value, default):
    value = _num(value, default)
    if not 0 <= value <= 2147483647:
        raise ValueError("client count must be a non-negative int32")
    return value


def server_data_from_heartbeat(hb):
    if not isinstance(hb, dict):
        raise ValueError("heartbeat must be an object")
    fb = hb.get("fallbackData") or {}
    if not isinstance(fb, dict):
        raise ValueError("fallbackData must be an object")
    info = fb.get("info") or {}
    dynamic = fb.get("dynamic") or {}
    if not isinstance(info, dict) or not isinstance(dynamic, dict):
        raise ValueError("info and dynamic must be objects")
    info = info.get("info", info) if isinstance(info.get("info", None), dict) else info
    dynamic = dynamic.get("dynamic", dynamic) if isinstance(dynamic.get("dynamic", None), dict) else dynamic

    # Validate untrusted shapes before storing data used by the protobuf encoder.
    for obj in (info, dynamic):
        if not isinstance(obj.get("vars") or {}, dict):
            raise ValueError("vars must be an object")
    resources = info.get("resources") or []
    if not isinstance(resources, list) or any(not isinstance(r, str) for r in resources):
        raise ValueError("resources must be an array of strings")
    for obj, fields in ((dynamic, ("hostname", "gametype", "mapname")),
                        (info, ("hostname",))):
        for field in fields:
            if obj.get(field) is not None and not isinstance(obj[field], str):
                raise ValueError(field + " must be a string")

    # vars live in both info.vars and dynamic.vars; merge them
    vars_ = {}
    vars_.update(info.get("vars") or {})
    vars_.update(dynamic.get("vars") or {})
    vars_["gamename"] = GAMENAME  # client browser filter requires this

    return {
        "svMaxclients": _client_count(dynamic.get("svMaxclients", hb.get("maxClients")), 48),
        "clients": _client_count(dynamic.get("clients", len(fb.get("players") or [])), 0),
        "hostname": dynamic.get("hostname") or info.get("hostname") or "NSM Server",
        "gametype": dynamic.get("gametype") or "",
        "mapname": dynamic.get("mapname") or "",
        "resources": info.get("resources") or [],
        "server": str(info.get("version") or ""),
        "vars": vars_,
        "enhancedHostSupport": bool(info.get("enhancedHostSupport")),
        "connectEndPoints": [],
        "upvotePower": 0,
        "burstPower": 0,
    }


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

# Public key served at /api/ticket/pubkey (PKCS#1 RSAPublicKey, base64 DER).
# Matches the NSM ticket-signing key (tools/masterlist/ticket-signing-key.pem).
# Only used when a server runs with sv_lan false; NSM servers should set
# `sv_lan true` so connect tickets are not required at all.
TICKET_PUBKEY_B64 = (
    "MIIBCgKCAQEAwleR7LYjK6zv5GPU4+go+4u42JI8fA/hX4wykkwhxG8fLpdGTA1kjQm+NWrWi7Xg"
    "OMOAVzeiMCpr5cLlcTabf1fJpexyN1XKmQLvnIxVgofc/N/ZvzVbnqTV5RGJ7ZfnXDd//r+bBuxg"
    "DWJIEAiXnYAwTSZq5crxgXorya7gVTcD2P/RQQUCCzuniFaXO2mROxj28UDSihVCKriTd1p6StxX"
    "SnsnekH6av5uuCENf1KHL85n7bl4gKZQoG0qoYQxohigjfUXvvO93AbN7M10r12rvgLcLTCHhMci"
    "y0xEmbj2pI5b9wOjPbx/oHqn5rtAhqYae4803I80aZRweafWzQIDAQAB"
)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "NSM-Masterlist/1.0"

    # -- helpers ------------------------------------------------------------
    def _cors(self):
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "*")

    def _send(self, code, body, ctype="application/json"):
        if isinstance(body, str):
            body = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self._cors()
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _json(self, obj, code=200):
        self._send(code, json.dumps(obj), "application/json")

    def _client_ip(self):
        ff = self.headers.get("X-Forwarded-For")
        if ff:
            return ff.split(",")[0].strip()
        return self.client_address[0]

    def _public_base(self):
        """Absolute base URL of this service, used for the streamRedir redirect."""
        if PUBLIC_BASE:
            return PUBLIC_BASE.rstrip("/")

        host = self.headers.get("Host") or ("%s:%d" % self.server.server_address)
        scheme = self.headers.get("X-Forwarded-Proto") or "http"
        return "%s://%s" % (scheme, host)

    def _redirect(self, location):
        body = b""
        self.send_response(302)
        self.send_header("Location", location)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", "0")
        self._cors()
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def log_message(self, fmt, *args):
        print("[%s] %s %s" % (time.strftime("%Y-%m-%d %H:%M:%S"),
                              self.command, fmt % args), flush=True)

    # -- routing ------------------------------------------------------------
    def do_OPTIONS(self):
        self._send(204, b"")

    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        path = self.path.split("?", 1)[0].rstrip("?")
        # strip duplicate trailing slashes, but keep the canonical
        # streamRedir/ and stream/ forms
        if path.endswith("/") and not path.endswith("streamRedir/") and not path.endswith("/stream/"):
            path = path.rstrip("/") or "/"

        if path in ("/", "/health"):
            self._send(200, "NSM masterlist service\n", "text/plain")

        elif path == "/api/servers/streamRedir/":
            # The client resolves this endpoint through the nui:gsclient
            # "gsclient:url" RPC, which issues the request with
            # followLocation = false and returns the Location header. If we
            # answer with 200 instead, the RPC hands the UI an empty string
            # and the server browser can never load a server list.
            self._redirect("%s/api/servers/stream/" % self._public_base())

        elif path == "/api/servers/stream/":
            entries = REGISTRY.entries()
            # fill connectEndPoints now that we know the EndPoints
            fixed = [(ep, dict(d, connectEndPoints=[ep])) for ep, d in entries]
            self._send(200, stream_bytes(fixed), "application/octet-stream")

        elif path.startswith("/api/servers/single/"):
            ep = path[len("/api/servers/single/"):]
            data = REGISTRY.get(ep)
            if data:
                data = dict(data, connectEndPoints=[ep])
                self._json({"EndPoint": ep, "Data": data})
            else:
                self._json({}, 404)

        elif path.startswith("/api/servers/icon/"):
            self._send(404, b"", "image/png")

        elif path == "/api/ticket/pubkey":
            self._json({"key": TICKET_PUBKEY_B64})

        elif path == "/api/identities" or path == "/api/ticket/identities":
            self._json([])

        elif path == "/v1/public/featured-servers":
            self._json([])

        elif path == "/v1/public/notices/fivem":
            self._json({"notices": []})

        elif path == "/v1/public/tweets":
            self._json({"tweets": []})

        elif path.startswith("/v1/public/pool-size-limits/"):
            self._json({})

        elif path == "/runtime/counts.json":
            entries = REGISTRY.entries()
            players = sum(_num(d.get("clients")) for _, d in entries)
            self._json([players, 0, players])

        else:
            self._json({"error": "not found", "path": path}, 404)

    def do_POST(self):
        path = self.path.split("?", 1)[0]

        if path == "/api/serverlist/ingress":
            # This handler only supports fixed-length bodies. Reject ambiguous
            # framing and close the connection so unread bytes cannot be reused.
            if self.headers.get("Transfer-Encoding") or len(self.headers.get_all("Content-Length", [])) > 1:
                self.close_connection = True
                self._json({"lastError": "unsupported request framing"}, 400)
                return
            try:
                length = int(self.headers.get("Content-Length") or 0)
                if length < 0 or length > MAX_HEARTBEAT_BYTES:
                    self.close_connection = True
                    self._json({"lastError": "heartbeat body too large or invalid length"}, 413)
                    return
                hb = json.loads(self.rfile.read(length) or b"{}")
                data = server_data_from_heartbeat(hb)
                ip_override = hb.get("ipOverride") or ""
                if not isinstance(ip_override, str):
                    raise ValueError("ipOverride must be a string")
                port = int(hb.get("port", 30120))
                if not 1 <= port <= 65535:
                    raise ValueError("port must be between 1 and 65535")
                # Validate serialization before accepting an entry into the registry.
                encode_server_data(data)
            except (ValueError, TypeError, OverflowError, AttributeError) as e:
                self.close_connection = True
                self._json({"lastError": "invalid heartbeat: %s" % e}, 400)
                return

            ip = (ip_override or "").strip() or self._client_ip()
            end_point = "%s:%d" % (ip, port)
            REGISTRY.upsert(end_point, data)
            print("[heartbeat] %s clients=%s" % (end_point, data["clients"]), flush=True)
            self._json({})  # empty lastError = success
        else:
            self.close_connection = True
            self._json({"error": "not found", "path": path}, 404)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=80)
    ap.add_argument("--bind", default="0.0.0.0")
    ap.add_argument("--public-base", default="",
                    help="absolute base URL advertised to clients "
                         "(default: derived from the request Host header)")
    args = ap.parse_args()

    global PUBLIC_BASE
    PUBLIC_BASE = args.public_base.rstrip("/")

    srv = ThreadingHTTPServer((args.bind, args.port), Handler)
    srv.daemon_threads = True
    print("NSM masterlist listening on %s:%d" % (args.bind, args.port), flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
