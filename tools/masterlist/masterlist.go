// NSM masterlist service
// ======================
// Standalone replacement for the Cfx masterlist endpoints used by NSM
// clients (v3+) and servers (v6+). Single file, standard library only.
//
// Build:
//   go build -o nsm-masterlist .
//   GOOS=windows GOARCH=amd64 go build -o NSM-Masterlist.exe .
//
// Endpoints (all on one port, default 80):
//   GET  /api/servers/streamRedir/     302 redirect to /api/servers/stream/
//   GET  /api/servers/stream/          full list: [4-byte LE length][protobuf master.Server] frames
//   GET  /api/servers/single/<ip:port> single server JSON {EndPoint, Data}
//   GET  /api/servers/icon/...         server icon (404 -> client uses default)
//   POST /api/serverlist/ingress       server heartbeat (GameServer.cpp)
//   GET  /api/ticket/pubkey            RSA public key (unused with sv_lan true)
//   GET  /api/identities               player identities (empty)
//   GET  /api/ticket/identities        ticket identities (empty)
//   GET  /v1/public/featured-servers   featured servers (empty)
//   GET  /v1/public/notices/fivem      notices (empty)
//   GET  /v1/public/tweets             tweets (empty)
//   GET  /v1/public/pool-size-limits/* pool limits (empty = unlimited)
//   GET  /runtime/counts.json          player counts [n, 0, n]
//
// Entries expire 5 minutes after their last heartbeat.
// vars.gamename is forced to "gta5" so the client browser filter shows them.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	heartbeatTTL = 300 * time.Second
	gamename     = "gta5"
)

// publicBaseOverride is the absolute base URL advertised to clients. Empty
// means "derive it from the request".
var publicBaseOverride = ""

// ---------------------------------------------------------------------------
// protobuf encoding (schema extracted from the cfx-ui worker bundle:
// master.Server { string EndPoint = 1; master.ServerData Data = 2; }
// master.ServerData {
//   int32 svMaxclients = 1; int32 clients = 2; int32 protocol = 3;
//   string hostname = 4; string gametype = 5; string mapname = 6;
//   repeated string resources = 8; string server = 9;
//   map<string,string> vars = 12; bool enhancedHostSupport = 16;
//   int32 upvotePower = 17; repeated string connectEndPoints = 18;
//   int32 burstPower = 19; })
// ---------------------------------------------------------------------------

func pbVarint(n uint64) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

func pbTag(field, wire int) []byte { return pbVarint(uint64(field<<3 | wire)) }

func pbString(field int, s string) []byte {
	b := []byte(s)
	out := pbTag(field, 2)
	out = append(out, pbVarint(uint64(len(b)))...)
	return append(out, b...)
}

func pbInt(field int, v int64) []byte {
	if v == 0 {
		return nil // protobuf default, decoder yields 0
	}
	out := pbTag(field, 0)
	return append(out, pbVarint(uint64(v))...)
}

func pbMessage(field int, payload []byte) []byte {
	out := pbTag(field, 2)
	out = append(out, pbVarint(uint64(len(payload)))...)
	return append(out, payload...)
}

// ServerData is the per-server payload; JSON tags mirror the protobufjs
// camelCase naming used by the /api/servers/single/ JSON endpoint.
type ServerData struct {
	SVMaxclients        int64             `json:"svMaxclients"`
	Clients             int64             `json:"clients"`
	Protocol            int64             `json:"protocol,omitempty"`
	Hostname            string            `json:"hostname"`
	Gametype            string            `json:"gametype"`
	Mapname             string            `json:"mapname"`
	Resources           []string          `json:"resources"`
	Server              string            `json:"server"`
	IconVersion         int64             `json:"iconVersion,omitempty"`
	Vars                map[string]string `json:"vars"`
	EnhancedHostSupport bool              `json:"enhancedHostSupport"`
	UpvotePower         int64             `json:"upvotePower"`
	ConnectEndPoints    []string          `json:"connectEndPoints"`
	BurstPower          int64             `json:"burstPower"`
}

func encodeServerData(d *ServerData) []byte {
	var out []byte
	out = append(out, pbInt(1, d.SVMaxclients)...)
	out = append(out, pbInt(2, d.Clients)...)
	out = append(out, pbInt(3, d.Protocol)...)
	if d.Hostname != "" {
		out = append(out, pbString(4, d.Hostname)...)
	}
	if d.Gametype != "" {
		out = append(out, pbString(5, d.Gametype)...)
	}
	if d.Mapname != "" {
		out = append(out, pbString(6, d.Mapname)...)
	}
	for _, r := range d.Resources {
		out = append(out, pbString(8, r)...)
	}
	if d.Server != "" {
		out = append(out, pbString(9, d.Server)...)
	}
	if d.IconVersion != 0 {
		out = append(out, pbInt(11, d.IconVersion)...)
	}
	keys := make([]string, 0, len(d.Vars))
	for k := range d.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry := append(pbString(1, k), pbString(2, d.Vars[k])...)
		out = append(out, pbMessage(12, entry)...)
	}
	if d.EnhancedHostSupport {
		out = append(out, append(pbTag(16, 0), 1)...)
	}
	out = append(out, pbInt(17, d.UpvotePower)...)
	for _, c := range d.ConnectEndPoints {
		out = append(out, pbString(18, c)...)
	}
	out = append(out, pbInt(19, d.BurstPower)...)
	return out
}

func encodeServer(endPoint string, d *ServerData) []byte {
	var out []byte
	out = append(out, pbString(1, endPoint)...)
	out = append(out, pbMessage(2, encodeServerData(d))...)
	return out
}

func streamBytes(entries map[string]*entry) []byte {
	eps := make([]string, 0, len(entries))
	for ep := range entries {
		eps = append(eps, ep)
	}
	sort.Strings(eps)
	var out []byte
	var frame [4]byte
	for _, ep := range eps {
		d := entries[ep].data
		d.ConnectEndPoints = []string{ep}
		payload := encodeServer(ep, &d)
		binary.LittleEndian.PutUint32(frame[:], uint32(len(payload)))
		out = append(out, frame[:]...)
		out = append(out, payload...)
	}
	return out
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

type entry struct {
	data ServerData
	last time.Time
}

type registry struct {
	mu      sync.RWMutex
	servers map[string]*entry
}

func newRegistry() *registry {
	return &registry{servers: map[string]*entry{}}
}

func (r *registry) upsert(endPoint string, data ServerData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.servers[endPoint] = &entry{data: data, last: time.Now()}
}

func (r *registry) get(endPoint string) *ServerData {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.servers[endPoint]
	if !ok || time.Since(e.last) > heartbeatTTL {
		return nil
	}
	d := e.data
	d.ConnectEndPoints = []string{endPoint}
	return &d
}

func (r *registry) live() map[string]*entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ep, e := range r.servers {
		if time.Since(e.last) > heartbeatTTL {
			delete(r.servers, ep)
		}
	}
	out := make(map[string]*entry, len(r.servers))
	for ep, e := range r.servers {
		out[ep] = e
	}
	return out
}

var reg = newRegistry()

// ---------------------------------------------------------------------------
// heartbeat -> ServerData mapping
// ---------------------------------------------------------------------------

type heartbeat struct {
	Port        int64  `json:"port"`
	ListingToken string `json:"listingToken"`
	IPOverride  string `json:"ipOverride"`
	Private     bool   `json:"private"`
	FallbackData struct {
		Players json.RawMessage        `json:"players"`
		Info    map[string]interface{} `json:"info"`
		Dynamic map[string]interface{} `json:"dynamic"`
	} `json:"fallbackData"`
}

func asString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func asInt(v interface{}, def int64) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n
		}
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
	}
	return def
}

func asStringSlice(v interface{}) []string {
	if arr, ok := v.([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			out = append(out, asString(e))
		}
		return out
	}
	return nil
}

func asStringMap(v interface{}) map[string]string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, e := range m {
		out[k] = asString(e)
	}
	return out
}

func serverDataFromHeartbeat(hb *heartbeat) ServerData {
	info := hb.FallbackData.Info
	dyn := hb.FallbackData.Dynamic

	vars := asStringMap(info["vars"])
	for k, v := range asStringMap(dyn["vars"]) {
		vars[k] = v
	}
	vars["gamename"] = gamename // client browser filter requires this

	var clients int64
	if c, ok := dyn["clients"]; ok {
		clients = asInt(c, 0)
	} else if len(hb.FallbackData.Players) > 0 && hb.FallbackData.Players[0] == '[' {
		var arr []interface{}
		if json.Unmarshal(hb.FallbackData.Players, &arr) == nil {
			clients = int64(len(arr))
		}
	}

	hostname := asString(dyn["hostname"])
	if hostname == "" {
		hostname = asString(info["hostname"])
	}
	if hostname == "" {
		hostname = "NSM Server"
	}

	return ServerData{
		SVMaxclients:        asInt(dyn["svMaxclients"], 48),
		Clients:             clients,
		Hostname:            hostname,
		Gametype:            asString(dyn["gametype"]),
		Mapname:             asString(dyn["mapname"]),
		Resources:           asStringSlice(info["resources"]),
		Server:              asString(info["version"]),
		Vars:                vars,
		EnhancedHostSupport: asInt(info["enhancedHostSupport"], 0) != 0,
		UpvotePower:         0,
		BurstPower:          0,
	}
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// Public key served at /api/ticket/pubkey (PKCS#1 RSAPublicKey, base64 DER).
// Matches tools/masterlist/ticket-signing-key.pem. Only used when a server
// runs with sv_lan false; NSM servers should set `sv_lan "true"` so connect
// tickets are not required at all.
const ticketPubkeyB64 = "MIIBCgKCAQEAwleR7LYjK6zv5GPU4+go+4u42JI8fA/hX4wykkwhxG8fLpdGTA1kjQm+NWrWi7Xg" +
	"OMOAVzeiMCpr5cLlcTabf1fJpexyN1XKmQLvnIxVgofc/N/ZvzVbnqTV5RGJ7ZfnXDd//r+bBuxg" +
	"DWJIEAiXnYAwTSZq5crxgXorya7gVTcD2P/RQQUCCzuniFaXO2mROxj28UDSihVCKriTd1p6StxX" +
	"SnsnekH6av5uuCENf1KHL85n7bl4gKZQoG0qoYQxohigjfUXvvO93AbN7M10r12rvgLcLTCHhMci" +
	"y0xEmbj2pI5b9wOjPbx/oHqn5rtAhqYae4803I80aZRweafWzQIDAQAB"

type handler struct{}

func setCors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

func sendJSON(w http.ResponseWriter, code int, v interface{}) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	setCors(w)
	w.WriteHeader(code)
	w.Write(body)
}

func clientIP(r *http.Request) string {
	if ff := r.Header.Get("X-Forwarded-For"); ff != "" {
		return strings.TrimSpace(strings.Split(ff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// publicBase returns the absolute base URL of this service, used for the
// streamRedir redirect. publicBaseOverride wins; otherwise the base is
// derived from the request (Host header + scheme).
func publicBase(r *http.Request) string {
	if publicBaseOverride != "" {
		return strings.TrimRight(publicBaseOverride, "/")
	}

	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	if host == "" {
		host = "127.0.0.1"
	}

	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}

	return scheme + "://" + host
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := r.URL.Path

	switch {
	case path == "/" || path == "/health":
		body := "NSM masterlist service\n"
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)

	case path == "/api/servers/streamRedir/":
		// The client resolves this endpoint through the nui:gsclient
		// "gsclient:url" RPC, which issues the request with
		// followLocation = false and returns the Location header. If we
		// answer with 200 instead, the RPC hands the UI an empty string
		// and the server browser can never load a server list.
		http.Redirect(w, r, publicBase(r)+"/api/servers/stream/", http.StatusFound)

	case path == "/api/servers/stream/":
		live := reg.live()
		body := streamBytes(live)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)

	case strings.HasPrefix(path, "/api/servers/single/"):
		ep := strings.TrimPrefix(path, "/api/servers/single/")
		if d := reg.get(ep); d != nil {
			sendJSON(w, http.StatusOK, map[string]interface{}{"EndPoint": ep, "Data": d})
		} else {
			sendJSON(w, http.StatusNotFound, map[string]interface{}{})
		}

	case strings.HasPrefix(path, "/api/servers/icon/"):
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusNotFound)

	case path == "/api/ticket/pubkey":
		sendJSON(w, http.StatusOK, map[string]string{"key": ticketPubkeyB64})

	case path == "/api/identities" || path == "/api/ticket/identities":
		sendJSON(w, http.StatusOK, []string{})

	case path == "/v1/public/featured-servers":
		sendJSON(w, http.StatusOK, []string{})

	case path == "/v1/public/notices/fivem":
		sendJSON(w, http.StatusOK, map[string][]string{"notices": {}})

	case path == "/v1/public/tweets":
		sendJSON(w, http.StatusOK, map[string][]string{"tweets": {}})

	case strings.HasPrefix(path, "/v1/public/pool-size-limits/"):
		sendJSON(w, http.StatusOK, map[string]int{})

	case path == "/runtime/counts.json":
		players := int64(0)
		for _, e := range reg.live() {
			players += e.data.Clients
		}
		sendJSON(w, http.StatusOK, []int64{players, 0, players})

	case path == "/api/serverlist/ingress" && r.Method == http.MethodPost:
		var hb heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]string{"lastError": "bad json: " + err.Error()})
			return
		}
		ip := strings.TrimSpace(hb.IPOverride)
		if ip == "" {
			ip = clientIP(r)
		}
		port := hb.Port
		if port == 0 {
			port = 30120
		}
		endPoint := net.JoinHostPort(ip, strconv.FormatInt(port, 10))
		// JoinHostPort adds brackets for IPv6; the masterlist format is plain ip:port
		endPoint = strings.ReplaceAll(endPoint, "[", "")
		endPoint = strings.ReplaceAll(endPoint, "]", "")
		reg.upsert(endPoint, serverDataFromHeartbeat(&hb))
		log.Printf("[heartbeat] %s clients=%d", endPoint, reg.get(endPoint).Clients)
		sendJSON(w, http.StatusOK, map[string]interface{}{}) // empty lastError = success

	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "path": path})
	}
}

func main() {
	bind := flag.String("bind", "0.0.0.0", "address to bind")
	port := flag.Int("port", 80, "port to listen on")
	publicBaseFlag := flag.String("public-base", "",
		"absolute base URL advertised to clients (default: derived from the request Host header)")
	flag.Parse()

	publicBaseOverride = strings.TrimRight(*publicBaseFlag, "/")

	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           &handler{},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("NSM masterlist listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("listen error: %v", err)
		os.Exit(1)
	}
}
