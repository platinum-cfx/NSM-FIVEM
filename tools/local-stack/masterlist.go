// NSM masterlist service v2
// =========================
// Standalone replacement for the Cfx masterlist endpoints used by NSM
// clients (v3+) and servers (v6+). Single file, standard library only.
//
// Build:
//
//	go build -o nsm-masterlist .
//	GOOS=windows GOARCH=amd64 go build -o NSM-Masterlist.exe .
//	GOOS=linux GOARCH=amd64 go build -o nsm-masterlist-linux-amd64 .
//	GOOS=linux GOARCH=arm64 go build -o nsm-masterlist-linux-arm64 .
//
// Endpoints (all on one port, default 80):
//
//	GET  /api/servers/streamRedir/     full list: [4-byte LE length][protobuf master.Server] frames
//	GET  /api/servers/single/<ip:port> single server JSON {EndPoint, Data}
//	GET  /api/servers/icon/...         server icon (404 -> client uses default)
//	POST /api/serverlist/ingress       server heartbeat (GameServer.cpp)
//	GET  /api/ticket/pubkey            RSA public key (unused with sv_lan true)
//	GET  /api/identities               player identities (empty)
//	GET  /api/ticket/identities        ticket identities (empty)
//	GET  /v1/public/featured-servers   featured servers (empty)
//	GET  /v1/public/notices/fivem      notices (empty)
//	GET  /v1/public/tweets             official feed (Twitter-format JSON array;
//	                                   sources: owner posts + Discord poller)
//	GET  /v1/public/pool-size-limits/* pool limits (empty = unlimited)
//	GET  /runtime/counts.json          player counts [n, 0, n]
//	*    /l/api/validate/entitlement*  unchanged upstream validation; never fabricated
//	*    /f/...                        forum mimic (posts.json, upload sink)
//
// Owner admin API (Authorization: Bearer $OWNER_TOKEN, owner-only):
//
//	POST /admin/upvotes      {"endpoints":[...], "set":N} or {"add":N}
//	POST /admin/fakeplayers  {"endpoints":[...], "count":N}  (0..9000, 0=off)
//	POST /admin/feed         {"text":"...", "author":"...", "screenName":"...",
//	                          "avatar":"...", "url":"..."}   (owner post)
//	POST /admin/feed/import  {"items":[{id,text,author,screenName,avatar,url,
//	                          createdAt,media:[{url,w,h}]}]}  (poller, id-deduped)
//	POST /admin/feed/delete  {"id":"..."}
//	GET  /admin/state        current mods + feed
//
// Entries expire 5 minutes after their last heartbeat. Owner mods (upvotes,
// legacy fake-player settings are ignored in local-first mode.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// ---------------------------------------------------------------------------
// protobuf encoding (schema extracted from the cfx-ui worker bundle:
// master.Server { string EndPoint = 1; master.ServerData Data = 2; }
// master.ServerData {
//   int32 svMaxclients = 1; int32 clients = 2; int32 protocol = 3;
//   string hostname = 4; string gametype = 5; string mapname = 6;
//   repeated string resources = 8; string server = 9;
//   repeated master.Player players = 10; int32 iconVersion = 11;
//   map<string,string> vars = 12; bool enhancedHostSupport = 16;
//   int32 upvotePower = 17; repeated string connectEndPoints = 18;
//   int32 burstPower = 19; }
// master.Player { string name = 1; repeated string identifiers = 2;
//   string endpoint = 3; int32 ping = 4; int32 id = 5; })
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

// Player is the per-player entry served in ServerData.players (field 10).
// Identifiers are always omitted: we never fabricate player identifiers.
type Player struct {
	Name string `json:"name"`
	Ping int64  `json:"ping,omitempty"`
	ID   int64  `json:"id,omitempty"`
}

func encodePlayer(p *Player) []byte {
	var out []byte
	if p.Name != "" {
		out = append(out, pbString(1, p.Name)...)
	}
	if p.Ping != 0 {
		out = append(out, pbInt(4, p.Ping)...)
	}
	if p.ID != 0 {
		out = append(out, pbInt(5, p.ID)...)
	}
	return out
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
	Players             []Player          `json:"players,omitempty"`
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
	for i := range d.Players {
		out = append(out, pbMessage(10, encodePlayer(&d.Players[i]))...)
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
	data    ServerData
	last    time.Time
	keyID   string
	ownerID int64
}

// iconRecord holds one server's list logo: a 96x96 PNG that arrived in the
// heartbeat as info.icon (base64), plus a content-derived version used in
// /api/servers/icon/<endpoint>/<version>.png URLs (FiveM-compatible).
type iconRecord struct {
	png     []byte
	version int64
}

type registry struct {
	mu      sync.RWMutex
	servers map[string]*entry
	icons   map[string]iconRecord
}

func newRegistry() *registry {
	return &registry{servers: map[string]*entry{}, icons: map[string]iconRecord{}}
}

func (r *registry) upsert(endPoint string, data ServerData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.servers[endPoint] = &entry{data: data, last: time.Now()}
}

func (r *registry) upsertWithOwner(endPoint string, data ServerData, keyID string, ownerID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.servers[endPoint]; ok {
		if time.Since(existing.last) <= heartbeatTTL && existing.keyID != "" && isServerKeyActive(existing.keyID) {
			if existing.ownerID != 0 && existing.ownerID != ownerID {
				return false
			}
		}
	}
	r.servers[endPoint] = &entry{data: data, last: time.Now(), keyID: keyID, ownerID: ownerID}
	return true
}

// endpointByKeyID returns the live endpoint currently heartbeating with the
// given server key (used to resolve join codes with dynamic IPs).
func (r *registry) endpointByKeyID(keyID string) string {
	if keyID == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for ep, e := range r.servers {
		if e.keyID == keyID && time.Since(e.last) <= heartbeatTTL {
			return ep
		}
	}
	return ""
}

func (r *registry) evictByKey(keyID string) int {
	if keyID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	evicted := 0
	for ep, e := range r.servers {
		if e.keyID == keyID {
			delete(r.servers, ep)
			delete(r.icons, ep)
			evicted++
		}
	}
	return evicted
}

func (r *registry) get(endPoint string) *ServerData {
	r.mu.RLock()
	e, ok := r.servers[endPoint]
	r.mu.RUnlock()
	if !ok || time.Since(e.last) > heartbeatTTL || (e.keyID != "" && !isServerKeyActive(e.keyID)) {
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
		if time.Since(e.last) > heartbeatTTL || (e.keyID != "" && !isServerKeyActive(e.keyID)) {
			delete(r.servers, ep)
			delete(r.icons, ep)
		}
	}
	for ep := range r.icons {
		if _, ok := r.servers[ep]; !ok {
			delete(r.icons, ep)
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
// server list icons (load_server_icon -> heartbeat info.icon -> icon URL)
// ---------------------------------------------------------------------------

const maxServerIconBytes = 512 << 10

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// validServerIconPNG mirrors the server runtime's load_server_icon rules
// (InfoHttpHandler.cpp): the file must be a PNG of exactly 96x96 pixels.
func validServerIconPNG(raw []byte) bool {
	if len(raw) < 24 || len(raw) > maxServerIconBytes || !bytes.Equal(raw[:8], pngSignature) {
		return false
	}
	if string(raw[12:16]) != "IHDR" {
		return false
	}
	return binary.BigEndian.Uint32(raw[16:20]) == 96 && binary.BigEndian.Uint32(raw[20:24]) == 96
}

// serverIconVersion derives a positive 31-bit icon version from the icon
// content so /api/servers/icon/<ep>/<ver>.png URLs change when the logo does.
func serverIconVersion(png []byte) int64 {
	sum := sha256.Sum256(png)
	v := int64(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
	if v == 0 {
		v = 1
	}
	return v
}

// setIcon stores the base64 96x96 PNG icon carried in a heartbeat (info.icon)
// and returns the effective icon version to publish on the server entry.
// An empty value clears the icon; an invalid value keeps the previous one.
func (r *registry) setIcon(endPoint, iconB64 string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(iconB64) == "" {
		delete(r.icons, endPoint)
		return 0
	}
	raw, err := base64.StdEncoding.DecodeString(iconB64)
	if err != nil || !validServerIconPNG(raw) {
		if rec, ok := r.icons[endPoint]; ok {
			return rec.version
		}
		return 0
	}
	ver := serverIconVersion(raw)
	r.icons[endPoint] = iconRecord{png: raw, version: ver}
	return ver
}

// getIcon returns the icon for endPoint when wantVer matches the current
// icon version and the server entry is still live; stale URLs return false
// so clients fall back to the generated letter avatar.
func (r *registry) getIcon(endPoint string, wantVer int64) ([]byte, bool) {
	r.mu.RLock()
	rec, ok := r.icons[endPoint]
	e := r.servers[endPoint]
	r.mu.RUnlock()
	if !ok || rec.version != wantVer {
		return nil, false
	}
	if e == nil || time.Since(e.last) > heartbeatTTL || (e.keyID != "" && !isServerKeyActive(e.keyID)) {
		return nil, false
	}
	return rec.png, true
}

// ---------------------------------------------------------------------------
// owner mods (upvotes / fake players) + feed, persisted to state.json
// ---------------------------------------------------------------------------

type serverMods struct {
	Upvotes       *int64 `json:"upvotes,omitempty"`     // absolute override
	FakePlayerCnt int64  `json:"fakePlayers,omitempty"` // 1..9000, 0 = off
}

type feedMedia struct {
	URL string `json:"url"`
	W   int    `json:"w"`
	H   int    `json:"h"`
}

type feedItem struct {
	ID         string      `json:"id"`
	Text       string      `json:"text"`
	Author     string      `json:"author"`
	ScreenName string      `json:"screenName"`
	Avatar     string      `json:"avatar,omitempty"`
	URL        string      `json:"url,omitempty"`
	CreatedAt  time.Time   `json:"createdAt"`
	Source     string      `json:"source"`
	Media      []feedMedia `json:"media,omitempty"`
}

type ownerState struct {
	Servers map[string]serverMods `json:"servers"`
	Feed    []feedItem            `json:"feed"`
	FeedIDs map[string]bool       `json:"-"` // dedupe set (rebuilt on load)
}

var (
	stateMu  sync.RWMutex
	state    = ownerState{Servers: map[string]serverMods{}, FeedIDs: map[string]bool{}}
	ownerTok string
)

func statePath() string { return *stateFile }

func loadState() {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return
	}
	var s ownerState
	if json.Unmarshal(b, &s) != nil {
		log.Printf("state.json unreadable, starting fresh")
		return
	}
	if s.Servers == nil {
		s.Servers = map[string]serverMods{}
	}
	s.FeedIDs = map[string]bool{}
	for _, it := range s.Feed {
		s.FeedIDs[it.ID] = true
	}
	stateMu.Lock()
	state = s
	stateMu.Unlock()
	log.Printf("state loaded: %d server mods, %d feed items", len(s.Servers), len(s.Feed))
}

func saveStateLocked() {
	b, err := json.MarshalIndent(&state, "", " ")
	if err != nil {
		return
	}
	tmp := statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, statePath())
	}
}

// applyMods overlays owner settings onto live server data (caller holds copy).
func applyMods(ep string, d *ServerData) {
	stateMu.RLock()
	m, ok := state.Servers[ep]
	stateMu.RUnlock()
	if ok && m.Upvotes != nil {
		d.UpvotePower = *m.Upvotes
	}
	if uc := int64(upvoteCount(ep)); uc > 0 {
		d.UpvotePower += uc
	}
	if alloc := int64(allocationBoost(ep)); alloc > 0 {
		d.UpvotePower += alloc
	}
	// Local-first: never fabricate players or counts from persisted admin state.
}

// ---------------------------------------------------------------------------
// heartbeat -> ServerData mapping
// ---------------------------------------------------------------------------

type heartbeat struct {
	Port         int64  `json:"port"`
	ListingToken string `json:"listingToken"`
	IPOverride   string `json:"ipOverride"`
	Private      bool   `json:"private"`
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

func serverDataFromHeartbeat(hb *heartbeat) (ServerData, string) {
	info := unwrapData(hb.FallbackData.Info, "info")
	dyn := unwrapData(hb.FallbackData.Dynamic, "dynamic")

	vars := map[string]string{}
	for _, m := range []map[string]string{asStringMap(info["vars"]), asStringMap(dyn["vars"])} {
		for k, v := range m {
			lk := strings.ToLower(strings.TrimSpace(k))
			if lk == "sv_nsmserverkey" || lk == "sv_licensekey" || lk == "sv_licensekeytoken" || lk == "authorization" {
				continue
			}
			rv := strings.TrimSpace(strings.TrimRight(redactServerKeys(v), "\x00"))
			if strings.EqualFold(rv, "default FXServer") {
				rv = "AFXserver"
			}
			vars[k] = rv
		}
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

	hostname := strings.TrimSpace(strings.TrimRight(redactServerKeys(asString(dyn["hostname"])), "\x00"))
	if hostname == "" {
		hostname = strings.TrimSpace(strings.TrimRight(redactServerKeys(asString(info["hostname"])), "\x00"))
	}
	if hostname == "" || strings.EqualFold(hostname, "default FXServer") {
		hostname = "AFXserver"
	}

	maxClients := asInt(dyn["svMaxclients"], 0)
	if maxClients <= 0 {
		maxClients = asInt(dyn["sv_maxclients"], 0)
	}
	if maxClients <= 0 {
		if v, ok := vars["sv_maxClients"]; ok {
			maxClients = asInt(v, 48)
		} else {
			maxClients = 48
		}
	}
	serverVer := redactServerKeys(asString(info["version"]))
	if serverVer == "" {
		serverVer = redactServerKeys(asString(info["server"]))
	}

	return ServerData{
		SVMaxclients:        maxClients,
		Clients:             clients,
		Hostname:            hostname,
		Gametype:            redactServerKeys(asString(dyn["gametype"])),
		Mapname:             redactServerKeys(asString(dyn["mapname"])),
		Resources:           append([]string{}, asStringSlice(info["resources"])...),
		Server:              serverVer,
		Vars:                vars,
		EnhancedHostSupport: info["enhancedHostSupport"] == true || asInt(info["enhancedHostSupport"], 0) != 0,
		UpvotePower:         0,
		BurstPower:          0,
	}, strings.TrimSpace(asString(info["icon"]))
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// Ticket public keys are explicitly configured; the backend never issues tickets.
type handler struct{}

func setCors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Authorization, User-Api-Key, User-Api-Client-Id, X-CSRF-Token, X-Requested-With, Discourse-Present, Discourse-Logged-In, Discourse-Visible, Ownership-Ticket, Cfx-Entitlement-Ticket, X-NSM-Session, *")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
}

func sendJSON(w http.ResponseWriter, code int, v interface{}) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	setCors(w)
	w.WriteHeader(code)
	w.Write(body)
}

var (
	isPublicMode        bool
	configuredPublicURL string
	configuredPublicIP  string
)

func detectOutboundIPv4() string {
	conn, err := net.DialTimeout("udp4", "1.1.1.1:80", 500*time.Millisecond)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsLoopback() {
		return ua.IP.String()
	}
	return ""
}

func publicAwareHandler(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicMode {
			host, portStr, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
				portStr = "0"
			}
			realIP := strings.TrimSpace(host)
			isForwarded := false
			if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
				realIP = cfIP
				isForwarded = true
			} else if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
				parts := strings.Split(xff, ",")
				if first := strings.TrimSpace(parts[0]); first != "" {
					realIP = first
					isForwarded = true
				}
			}
			r.Header.Set("X-NSM-Real-IP", realIP)
			if isForwarded {
				r.Header.Set("X-NSM-Forwarded", "1")
			} else {
				r.Header.Del("X-NSM-Forwarded")
			}
			r.RemoteAddr = net.JoinHostPort("127.0.0.1", portStr)
		}
		inner.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if realIP := strings.TrimSpace(r.Header.Get("X-NSM-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isOwner(r *http.Request) bool {
	if ownerTok == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	ok := auth == "Bearer "+ownerTok
	if !ok {
		time.Sleep(500 * time.Millisecond) // blunt brute force
	}
	return ok
}

// twitterTime renders times the way the UI's tweet parser expects.
func twitterTime(t time.Time) string { return t.Format("Mon Jan 02 15:04:05 -0700 2006") }

// tweetShape converts a feed item into the raw Twitter-format object the
// client's rawTweetToActivityDataItem() consumes.
func tweetShape(it feedItem) map[string]interface{} {
	user := map[string]interface{}{
		"name":        it.Author,
		"screen_name": it.ScreenName,
	}
	if it.Avatar != "" {
		user["profile_image_url_https"] = it.Avatar
	}
	tw := map[string]interface{}{
		"id_str":     it.ID,
		"text":       it.Text,
		"created_at": twitterTime(it.CreatedAt),
		"user":       user,
		"entities":   map[string]interface{}{"urls": []interface{}{}},
	}
	if len(it.Media) > 0 {
		media := make([]interface{}, 0, len(it.Media))
		for _, m := range it.Media {
			w, h := m.W, m.H
			if w <= 0 || h <= 0 {
				w, h = 1280, 720
			}
			media = append(media, map[string]interface{}{
				"id_str":          it.ID + "_m",
				"type":            "photo",
				"media_url":       m.URL,
				"media_url_https": m.URL,
				"sizes":           map[string]interface{}{"large": map[string]int{"w": w, "h": h}},
			})
		}
		tw["extended_entities"] = map[string]interface{}{"media": media}
	}
	return tw
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	w.Header().Set("X-NSM-Backend", backendVersion)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := r.URL.Path
	if path == "/api/serverlist/ingress" && r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		sendJSON(w, 405, map[string]string{"lastError": "POST required"})
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(path, "/api/servers/") {
		if strings.HasSuffix(path, "/upvote") {
			ep := strings.TrimSuffix(strings.TrimPrefix(path, "/api/servers/"), "/upvote")
			upvoteServer(w, r, ep)
			return
		}
		if strings.HasSuffix(path, "/feature") {
			ep := strings.TrimSuffix(strings.TrimPrefix(path, "/api/servers/"), "/feature")
			featureServer(w, r, ep)
			return
		}
	}
	if path != "/api/serverlist/ingress" && !strings.HasPrefix(path, "/admin/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
		if path != "/api/ticket/identities" && path != "/api/identities" && path != "/api/upvote/" && path != "/api/upvote" {
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			sendJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
	}

	// ---- owner admin API ----
	if strings.HasPrefix(path, "/admin/") {
		if !isOwner(r) {
			sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "owner token required"})
			return
		}
		h.serveAdmin(w, r, path)
		return
	}

	switch {
	case path == "/health":
		sendJSON(w, 200, map[string]interface{}{"service": "nsm-local-first", "version": backendVersion,
			"pid": os.Getpid(), "auth": "external-validation-required", "telemetry": "disabled",
			"heartbeatTTLSeconds": 300, "servers": len(reg.live()), "routes": uiRoutes})
	case path == "/":
		body := "NSM masterlist service v2\n"
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)

	case path == "/api/servers/streamRedir/" || path == "/api/servers/streamRedir":
		live := reg.live()
		out := make(map[string]*entry, len(live))
		for ep, e := range live {
			d := e.data
			applyMods(ep, &d)
			out[ep] = &entry{data: d, last: e.last}
		}
		body := streamBytes(out)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)

	case strings.HasPrefix(path, "/api/servers/single/"):
		ep := strings.TrimPrefix(path, "/api/servers/single/")
		if d := reg.get(ep); d != nil {
			applyMods(ep, d)
			sendJSON(w, http.StatusOK, map[string]interface{}{"EndPoint": ep, "Data": d})
		} else {
			sendJSON(w, http.StatusNotFound, map[string]interface{}{})
		}

	case strings.HasPrefix(path, "/api/servers/icon/"):
		w.Header().Set("Content-Type", "image/png")
		rest := strings.TrimPrefix(path, "/api/servers/icon/")
		slash := strings.LastIndex(rest, "/")
		if slash <= 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ver, err := strconv.ParseInt(strings.TrimSuffix(rest[slash+1:], ".png"), 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		icon, ok := reg.getIcon(rest[:slash], ver)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(icon)))
		w.WriteHeader(http.StatusOK)
		w.Write(icon)

	case path == "/api/ticket/pubkey":
		if ticketPublicKey == "" {
			sendJSON(w, 503, map[string]string{"error": "ticket authentication unavailable: no trusted public key configured"})
		} else {
			sendJSON(w, 200, map[string]string{"key": ticketPublicKey})
		}

	case path == "/api/identities" || path == "/api/ticket/identities":
		if r.Method == http.MethodPost && path == "/api/identities" {
			sendJSON(w, 503, map[string]string{"error": "identity linking is not available in local mode"})
		} else if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPost && path == "/api/ticket/identities" {
			sendJSON(w, 200, map[string]interface{}{"identities": []interface{}{}})
		} else {
			sendJSON(w, 405, map[string]string{"error": "method not allowed"})
		}
	case path == "/api/upvote/" || path == "/api/upvote":
		handleUpvoteAPI(w, r)
	case path == "/v1/public/featured-servers":
		pinned := featuredServerEndpoints()
		if pinned == nil {
			pinned = []string{}
		}
		sendJSON(w, http.StatusOK, map[string]interface{}{"pinnedServers": pinned, "pinIfEmpty": len(pinned) > 0})

	case path == "/v1/public/notices/fivem":
		sendJSON(w, http.StatusOK, []interface{}{})

	case path == "/v1/public/tweets":
		stateMu.RLock()
		items := make([]feedItem, len(state.Feed))
		copy(items, state.Feed)
		stateMu.RUnlock()
		// newest first, highest id first
		sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
		out := make([]interface{}, 0, len(items))
		for _, it := range items {
			out = append(out, tweetShape(it))
		}
		sendJSON(w, http.StatusOK, out)

	case strings.HasPrefix(path, "/v1/public/pool-size-limits/"):
		sendJSON(w, http.StatusOK, map[string]int{})

	case path == "/runtime/counts.json":
		players := int64(0)
		for ep, e := range reg.live() {
			d := e.data
			applyMods(ep, &d)
			players += d.Clients
		}
		sendJSON(w, http.StatusOK, []interface{}{players, nil, nil})

	case path == "/api/serverlist/ingress" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		var hb *heartbeat
		if err := dec.Decode(&hb); err != nil || hb == nil {
			var max *http.MaxBytesError
			code := 400
			if errors.As(err, &max) {
				code = 413
			}
			sendJSON(w, code, map[string]string{"lastError": "invalid heartbeat JSON"})
			return
		}
		var extra interface{}
		if dec.Decode(&extra) != io.EOF {
			sendJSON(w, 400, map[string]string{"lastError": "unexpected trailing JSON"})
			return
		}
		// GameServer.cpp marks heartbeats to a custom master as "private".
		// This loopback-only service is that explicitly selected local master;
		// accept the record locally, never publish it to an upstream master.
		if hb.FallbackData.Info != nil {
			infoMap := unwrapData(hb.FallbackData.Info, "info")
			if v, ok := infoMap["version"]; ok {
				if num, isNum := v.(json.Number); isNum {
					if srv, okSrv := infoMap["server"].(string); okSrv && srv != "" {
						infoMap["version"] = srv
					} else {
						infoMap["version"] = num.String()
					}
				}
			}
		}
		if err := validateHeartbeat(hb); err != nil {
			sendJSON(w, 400, map[string]string{"lastError": err.Error()})
			return
		}
		ip := clientIP(r)
		if override := strings.TrimSpace(hb.IPOverride); override != "" {
			if net.ParseIP(override) == nil || (!isPublicMode && !net.ParseIP(override).IsLoopback()) {
				sendJSON(w, 400, map[string]string{"lastError": "ipOverride must be a loopback IP"})
				return
			}
			ip = override
		}
		if isPublicMode && configuredPublicIP != "" {
			if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
				ip = configuredPublicIP
			}
		}
		port := hb.Port
		if port == 0 {
			port = 30120
		}
		endPoint := net.JoinHostPort(ip, strconv.FormatInt(port, 10))
		rawKey := extractHeartbeatServerKey(r, hb)
		var keyID string
		var ownerID int64
		if rawKey != "" || requireKeys {
			keyRec, code, reason := validateServerKey(rawKey, endPoint)
			if code != http.StatusOK || keyRec == nil {
				sendJSON(w, code, map[string]string{"lastError": reason})
				return
			}
			keyID = keyRec.ID
			ownerID = keyRec.UserID
		}
		data, iconB64 := serverDataFromHeartbeat(hb)
		if iconVer := reg.setIcon(endPoint, iconB64); iconVer != 0 {
			data.IconVersion = iconVer
		}
		if !reg.upsertWithOwner(endPoint, data, keyID, ownerID) {
			sendJSON(w, http.StatusForbidden, map[string]string{"lastError": "endpoint_owned_by_another_account"})
			return
		}
		// Keep join codes (shareable /join/<code> links that hide the IP)
		// pointed at this server's live endpoint. Every listed server gets
		// its own unique code; a key's code additionally follows its server.
		if keyID != "" {
			bindJoinCodeEndpoint(keyID, endPoint)
		}
		joinCodeForEndpoint(endPoint)
		log.Printf("[heartbeat] %s clients=%d", endPoint, data.Clients)
		sendJSON(w, 200, map[string]interface{}{})

	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "path": path})
	}
}

func (h *handler) serveAdmin(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodPost && path != "/admin/state" {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	switch path {

	case "/admin/state":
		stateMu.RLock()
		snap := ownerState{Servers: state.Servers, Feed: state.Feed}
		stateMu.RUnlock()
		sendJSON(w, http.StatusOK, snap)

	case "/admin/upvotes":
		var req struct {
			Endpoints []string `json:"endpoints"`
			Set       *int64   `json:"set"`
			Add       *int64   `json:"add"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Endpoints) == 0 ||
			(req.Set == nil && req.Add == nil) {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "need endpoints and set or add"})
			return
		}
		stateMu.Lock()
		updated := map[string]int64{}
		for _, ep := range req.Endpoints {
			m := state.Servers[ep]
			switch {
			case req.Set != nil:
				v := *req.Set
				m.Upvotes = &v
			case req.Add != nil && m.Upvotes != nil:
				v := *m.Upvotes + *req.Add
				if v < 0 {
					v = 0
				}
				m.Upvotes = &v
			case req.Add != nil:
				v := *req.Add
				if v < 0 {
					v = 0
				}
				m.Upvotes = &v
			}
			state.Servers[ep] = m
			updated[ep] = *m.Upvotes
		}
		saveStateLocked()
		stateMu.Unlock()
		log.Printf("[admin] upvotes: %v", updated)
		sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "upvotes": updated})

	case "/admin/fakeplayers":
		sendJSON(w, 410, map[string]string{"error": "fabricated player counts are disabled"})
		return

	case "/admin/feed":
		var req struct {
			Text       string `json:"text"`
			Author     string `json:"author"`
			ScreenName string `json:"screenName"`
			Avatar     string `json:"avatar"`
			URL        string `json:"url"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Text) == "" {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "need text"})
			return
		}
		if req.Author == "" {
			req.Author = "NSM"
		}
		if req.ScreenName == "" {
			req.ScreenName = "NSMCommunity"
		}
		stateMu.Lock()
		id := strconv.FormatInt(time.Now().UnixNano()/1e6, 10)
		state.Feed = append(state.Feed, feedItem{
			ID: id, Text: req.Text, Author: req.Author, ScreenName: req.ScreenName,
			Avatar: req.Avatar, URL: req.URL, CreatedAt: time.Now(), Source: "owner",
		})
		state.FeedIDs[id] = true
		saveStateLocked()
		stateMu.Unlock()
		log.Printf("[admin] feed post %s", id)
		sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": id})

	case "/admin/feed/import":
		var req struct {
			Items []struct {
				ID         string      `json:"id"`
				Text       string      `json:"text"`
				Author     string      `json:"author"`
				ScreenName string      `json:"screenName"`
				Avatar     string      `json:"avatar"`
				URL        string      `json:"url"`
				CreatedAt  string      `json:"createdAt"` // RFC3339
				Source     string      `json:"source"`
				Media      []feedMedia `json:"media"`
			} `json:"items"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		added, skipped := 0, 0
		stateMu.Lock()
		for _, it := range req.Items {
			if it.ID == "" || it.Text == "" || state.FeedIDs[it.ID] {
				skipped++
				continue
			}
			ca := time.Now()
			if it.CreatedAt != "" {
				if t, err := time.Parse(time.RFC3339, it.CreatedAt); err == nil {
					ca = t
				}
			}
			src := it.Source
			if src == "" {
				src = "discord"
			}
			state.Feed = append(state.Feed, feedItem{
				ID: it.ID, Text: it.Text, Author: it.Author, ScreenName: it.ScreenName,
				Avatar: it.Avatar, URL: it.URL, CreatedAt: ca, Source: src, Media: it.Media,
			})
			state.FeedIDs[it.ID] = true
			added++
		}
		// keep newest 200
		if len(state.Feed) > 200 {
			sort.Slice(state.Feed, func(i, j int) bool { return state.Feed[i].ID > state.Feed[j].ID })
			state.Feed = state.Feed[:200]
		}
		saveStateLocked()
		stateMu.Unlock()
		log.Printf("[admin] feed import: %d added, %d skipped", added, skipped)
		sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "added": added, "skipped": skipped})

	case "/admin/feed/delete":
		var req struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.ID == "" {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "need id"})
			return
		}
		stateMu.Lock()
		out := state.Feed[:0]
		deleted := false
		for _, it := range state.Feed {
			if it.ID == req.ID {
				deleted = true
				delete(state.FeedIDs, it.ID)
				continue
			}
			out = append(out, it)
		}
		state.Feed = out
		saveStateLocked()
		stateMu.Unlock()
		if !deleted {
			sendJSON(w, http.StatusNotFound, map[string]string{"error": "no such id"})
			return
		}
		sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true})

	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "unknown admin path"})
	}
}

var stateFile = flag.String("state", "state.json", "path to persistent state file")

var ownerName = "nta~"

var (
	activeSrvMu    sync.Mutex
	activeListener net.Listener
	activeServer   *http.Server
)

func closeActiveServer() {
	activeSrvMu.Lock()
	ln := activeListener
	srv := activeServer
	activeSrvMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if srv != nil {
		_ = srv.Close()
	}
}

func sys32Bin(name string) string {
	if sr := os.Getenv("SystemRoot"); sr != "" {
		p := filepath.Join(sr, "System32", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

func findListeningPIDsOnPort(port int) []int {
	pid := os.Getpid()
	out, err := exec.Command(sys32Bin("netstat.exe"), "-ano", "-p", "tcp").Output()
	if err != nil {
		return nil
	}
	targetSuffix := ":" + strconv.Itoa(port)
	seen := map[int]bool{}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "TCP") {
			continue
		}
		localAddr := fields[1]
		foreignAddr := fields[2]
		stateStr := fields[3]
		isListening := strings.EqualFold(stateStr, "LISTENING") || foreignAddr == "0.0.0.0:0" || foreignAddr == "[::]:0" || strings.HasSuffix(foreignAddr, ":0")
		if isListening && strings.HasSuffix(localAddr, targetSuffix) {
			if otherPID, err := strconv.Atoi(fields[len(fields)-1]); err == nil && otherPID > 4 && otherPID != pid && !seen[otherPID] {
				seen[otherPID] = true
				pids = append(pids, otherPID)
			}
		}
	}
	return pids
}

func stopPreviousMasterlistInstances(addr string, port int) {
	pid := os.Getpid()
	// 1. Politely ask any running NSM-Masterlist on this address to close its listener and exit
	tr := &http.Transport{DisableKeepAlives: true}
	cl := &http.Client{Timeout: 350 * time.Millisecond, Transport: tr}
	if req, err := http.NewRequest("POST", "http://"+addr+"/api/nsm/internal-restart", strings.NewReader(`{}`)); err == nil {
		req.Close = true
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connection", "close")
		if resp, err := cl.Do(req); err == nil && resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	tr.CloseIdleConnections()

	if runtime.GOOS != "windows" {
		return
	}

	taskkillBin := sys32Bin("taskkill.exe")
	tasklistBin := sys32Bin("tasklist.exe")

	killPID := func(targetPID int) {
		if targetPID <= 4 || targetPID == pid {
			return
		}
		if proc, err := os.FindProcess(targetPID); err == nil && proc != nil {
			_ = proc.Kill()
		}
		_ = exec.Command(taskkillBin, "/F", "/T", "/PID", strconv.Itoa(targetPID)).Run()
	}

	// 2. Find any NSM-Masterlist*.exe process via tasklist CSV output and terminate it without touching the console window
	if out, err := exec.Command(tasklistBin, "/FO", "CSV", "/NH").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Split(line, `","`)
			if len(parts) >= 2 {
				imgName := strings.ToLower(strings.Trim(parts[0], `"`))
				pidStr := strings.Trim(parts[1], `"`)
				if strings.Contains(imgName, "nsm-masterlist") {
					if otherPID, err := strconv.Atoi(pidStr); err == nil {
						killPID(otherPID)
					}
				}
			}
		}
	}

	// 3. Also check netstat for any user process holding 127.0.0.1:<port>
	for _, otherPID := range findListeningPIDsOnPort(port) {
		killPID(otherPID)
	}

	// 4. If an elevated process is still holding 127.0.0.1:<port>, request elevated taskkill for that PID
	time.Sleep(150 * time.Millisecond)
	for _, remPID := range findListeningPIDsOnPort(port) {
		psCmd := fmt.Sprintf("Start-Process -FilePath 'taskkill.exe' -ArgumentList '/F /T /PID %d' -Verb RunAs -Wait", remPID)
		_ = exec.Command(sys32Bin("WindowsPowerShell\\v1.0\\powershell.exe"), "-NoProfile", "-NonInteractive", "-Command", psCmd).Run()
	}
}

func main() {
	// Ensure working directory is the executable's directory when launched by double-click on Windows
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		base := strings.ToLower(filepath.Base(exe))
		if exeDir != "" && !strings.Contains(base, ".test") {
			_ = os.Chdir(exeDir)
		}
	}

	defaultPubURL := os.Getenv("NSM_PUBLIC_URL")
	if defaultPubURL == "" {
		defaultPubURL = "https://nsmultiplayer.com"
	}
	bind := flag.String("bind", "0.0.0.0", "bind IP address (0.0.0.0 for VPS/Cloudflare/LAN, 127.0.0.1 for local-only)")
	port := flag.Int("port", 80, "port to listen on")
	pubURL := flag.String("public-url", defaultPubURL, "optional public base URL (e.g. https://nsmultiplayer.com)")
	pubIP := flag.String("public-ip", os.Getenv("NSM_PUBLIC_IP"), "optional public IPv4 for local server heartbeats")
	owner := flag.String("owner-token", os.Getenv("OWNER_TOKEN"), "owner admin API bearer token")
	oname := flag.String("owner-name", os.Getenv("OWNER_NAME"), "username that holds the owner role")
	opass := flag.String("owner-pass", os.Getenv("OWNER_PASS"), "set a new password for the owner account at startup")
	keyPath := flag.String("ticket-public-key", "", "optional trusted RSA public key PEM; no tickets are issued")
	reqKeys := flag.Bool("require-keys", os.Getenv("NSM_REQUIRE_KEYS") == "1", "require a valid NSM keymaster server key on /api/serverlist/ingress")
	flag.Parse()
	requireKeys = *reqKeys
	parsedBind := net.ParseIP(*bind)
	if parsedBind == nil {
		log.Fatalf("invalid -bind IP address: %q", *bind)
	}
	configuredPublicURL = strings.TrimRight(strings.TrimSpace(*pubURL), "/")
	configuredPublicIP = strings.TrimSpace(*pubIP)
	isPublicMode = !parsedBind.IsLoopback() || configuredPublicURL != "" || configuredPublicIP != ""
	if isPublicMode && configuredPublicIP == "" {
		configuredPublicIP = detectOutboundIPv4()
	}
	if *port < 1 || *port > 65535 {
		log.Fatal("invalid HTTP port")
	}
	if *keyPath != "" {
		var err error
		ticketPublicKey, err = readTicketPublicKey(*keyPath)
		if err != nil {
			log.Fatalf("ticket public key error: %v", err)
		}
	}

	if *oname != "" {
		ownerName = *oname
	}

	ownerTok = *owner
	if ownerTok == "" {
		log.Printf("NOTE: OWNER_TOKEN not set - bearer-token admin API is disabled (website owner login still works)")
	}

	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	probeHost := *bind
	if parsedBind.IsUnspecified() {
		probeHost = "127.0.0.1"
	}
	probeAddr := net.JoinHostPort(probeHost, strconv.Itoa(*port))
	if c, dialErr := net.DialTimeout("tcp", probeAddr, 120*time.Millisecond); dialErr == nil {
		_ = c.Close()
		log.Printf("Existing service detected on %s — stopping previous NSM-Masterlist instance...", addr)
		stopPreviousMasterlistInstances(addr, *port)
		time.Sleep(200 * time.Millisecond)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("Port %s is currently in use — stopping previous NSM-Masterlist instance...", addr)
		stopPreviousMasterlistInstances(addr, *port)
		for attempt := 0; attempt < 6; attempt++ {
			time.Sleep(150 * time.Millisecond)
			ln, err = net.Listen("tcp", addr)
			if err == nil {
				log.Printf("Successfully took over %s from previous instance.", addr)
				break
			}
		}
		if err != nil {
			// On Windows, Go's default net.Listen does not set SO_REUSEADDR, so lingering TIME_WAIT/FIN_WAIT_2
			// connections from Chrome or NSM.exe after stopping the old process cause WSAEADDRINUSE for up to 120s.
			// Binding with SO_REUSEADDR after stopping the old instance succeeds immediately.
			ln, err = listenWithReuseAddr("tcp", addr)
			if err == nil {
				log.Printf("Successfully bound %s (with SO_REUSEADDR socket takeover).", addr)
			}
		}
	}
	if err != nil {
		log.Fatalf("listen error on %s: %v", addr, err)
	}

	loadState()
	loadAccounts()
	if *opass != "" {
		setOwnerPassword(*opass)
	}
	loadForum()
	initSite()
	patchLocalUIBundlesOnDisk()

	srv := &http.Server{
		Addr:              addr,
		Handler:           publicAwareHandler(localHandler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	activeSrvMu.Lock()
	activeListener = ln
	activeServer = srv
	activeSrvMu.Unlock()

	displayBase := "http://" + addr
	if configuredPublicURL != "" {
		displayBase = configuredPublicURL
	}
	log.Printf("================================================================")
	log.Printf("NSM Masterlist + 1:1 Website + AFX Forum + Accounts (v0.7.8-n)")
	log.Printf("Listening on: http://%s (Public Mode: %v)", addr, isPublicMode)
	if configuredPublicIP != "" {
		log.Printf("Public Server IP: %s", configuredPublicIP)
	}
	log.Printf("  Homepage:    %s/", displayBase)
	log.Printf("  Server List: %s/servers", displayBase)
	log.Printf("  AFX Forum:   %s/forum", displayBase)
	log.Printf("  Your Profile:%s/u/nta~", displayBase)
	log.Printf("  Owner Setup: http://127.0.0.1:%d/login (from VPS desktop)", *port)
	log.Printf("================================================================")
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen error: %v", err)
	}
}
