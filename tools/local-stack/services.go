package main

// Service modes — one exe per platform component.
//
// NSM-Masterlist.exe runs ONLY the master list (Server List API + Server
// Ingestion). Each of the website-side components gets its own exe:
//
//	NSM-Masterlist.exe                master list (list API + ingest + showcase)
//	NSM-Website.exe                   Website & Dashboard (front proxy + pages + status)
//	NSM-ClientFeed.exe                NSM.exe Client Feed (binary list feed)
//	NSM-JoinLinks.exe                 Join Links (/join/<code>)
//	NSM-Accounts.exe                  Accounts & Sign-In (auth, profiles, upvotes, keys)
//
// Each exe is built from this package with its mode baked in:
//
//	go build -ldflags "-X main.defaultRunMode=master"  -o NSM-Masterlist.exe .
//	go build -ldflags "-X main.defaultRunMode=website" -o NSM-Website.exe .
//	go build -ldflags "-X main.defaultRunMode=feed"    -o NSM-ClientFeed.exe .
//	go build -ldflags "-X main.defaultRunMode=join"    -o NSM-JoinLinks.exe .
//	go build -ldflags "-X main.defaultRunMode=accounts" -o NSM-Accounts.exe .
//
// The web-facing exe (NSM-Website.exe) is the public front: it answers the
// dashboard itself and reverse-proxies the other services' paths, so public
// URLs never change. The status monitor runs with the website and probes
// every service over its own URL — that is what makes the status page able
// to report one dead process without the others going dark.
//
// State ownership is single-writer per file: accounts.json = accounts exe
// (master sends heartbeat bind updates to it over the internal endpoint),
// state.json = master, forum.json = website, status.json = website (monitor).
// Readers refresh on mtime. Run all exes from the same directory so they
// share the state files. "serve" (default when no mode is baked in) keeps
// the old all-in-one behaviour for development and tests.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	// defaultRunMode is baked per-exe via -ldflags -X main.defaultRunMode=...
	defaultRunMode = "serve"
	runMode        = "serve" // serve | master | accounts | join | feed | website | status

	masterBaseURL   string // where the master list service listens
	feedBaseURL     string // where the client feed service listens
	joinBaseURL     string // where the join links service listens
	accountsBaseURL string // where the accounts service listens

	internalToken    = os.Getenv("NSM_INTERNAL_TOKEN")
	accountsReadOnly bool // only serve/accounts write accounts.json
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// parseRunMode strips a leading subcommand before flag parsing and falls
// back to the mode baked into this exe.
func parseRunMode() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "serve", "all", "master", "accounts", "join", "feed", "website", "web", "status":
			runMode = os.Args[1]
			switch runMode {
			case "all":
				runMode = "serve"
			case "web":
				runMode = "website"
			}
			os.Args = append(os.Args[:1], os.Args[2:]...)
			accountsReadOnly = !(runMode == "serve" || runMode == "accounts")
			return
		}
	}
	runMode = defaultRunMode
	if runMode == "web" {
		runMode = "website"
	}
	accountsReadOnly = !(runMode == "serve" || runMode == "accounts")
}

// remoteRowsMode reports whether this process reads live server rows from
// the master service over HTTP instead of owning the registry.
func remoteRowsMode() bool {
	return runMode == "website" || runMode == "feed" || runMode == "join" || runMode == "accounts"
}

// ---------------------------------------------------------------------------
// path ownership — which service answers which public path
// ---------------------------------------------------------------------------

// pathOwner maps a request path to the service that owns it. The website
// front proxies everything it does not own, and each service rejects (404)
// paths owned by someone else so "who runs what" stays explicit.
func pathOwner(p string) string {
	// --- NSM.exe Client Feed ---
	if strings.HasPrefix(p, "/api/servers/streamRedir") {
		return "feed"
	}
	// --- Join Links ---
	if p == "/join" || strings.HasPrefix(p, "/join/") {
		return "join"
	}
	// --- master list: Server List API + Server Ingestion ---
	if strings.HasPrefix(p, "/api/serverlist/") ||
		p == "/api/servers/live" ||
		strings.HasPrefix(p, "/api/servers/single/") ||
		strings.HasPrefix(p, "/api/servers/icon/") ||
		strings.HasPrefix(p, "/runtime/") ||
		strings.HasPrefix(p, "/v1/public/") {
		return "master"
	}
	if strings.HasPrefix(p, "/admin/") && !strings.HasPrefix(p, "/admin/users") {
		return "master"
	}
	// --- Accounts & Sign-In (everything that writes accounts.json) ---
	if strings.HasPrefix(p, "/internal/") {
		return "accounts"
	}
	if strings.HasPrefix(p, "/login") || strings.HasPrefix(p, "/register") ||
		strings.HasPrefix(p, "/signup") || strings.HasPrefix(p, "/f/signup") ||
		strings.HasPrefix(p, "/logout") || strings.HasPrefix(p, "/account") ||
		strings.HasPrefix(p, "/verify-email") {
		return "accounts"
	}
	if p == "/u" || strings.HasPrefix(p, "/u/") ||
		strings.HasPrefix(p, "/users") ||
		strings.HasPrefix(p, "/avatars/") || strings.HasPrefix(p, "/f/avatars/") ||
		strings.HasPrefix(p, "/banners/") {
		return "accounts"
	}
	if strings.HasPrefix(p, "/support") || strings.HasPrefix(p, "/hc/") ||
		strings.HasPrefix(p, "/marketplace") || strings.HasPrefix(p, "/portal") ||
		strings.HasPrefix(p, "/keymaster") || strings.HasPrefix(p, "/auth/") ||
		strings.HasPrefix(p, "/user-api-key") || strings.HasPrefix(p, "/f/user-api-key") {
		return "accounts"
	}
	if strings.HasPrefix(p, "/api/auth/") || strings.HasPrefix(p, "/api/upvote") ||
		p == "/api/servers/upvote" || p == "/api/servers/feature" ||
		strings.HasPrefix(p, "/api/identities") || strings.HasPrefix(p, "/api/ticket/") {
		return "accounts"
	}
	if strings.HasPrefix(p, "/servers/") && (strings.HasSuffix(p, "/upvote") || strings.HasSuffix(p, "/feature")) {
		return "accounts"
	}
	if strings.HasPrefix(p, "/session") || strings.HasPrefix(p, "/f/session") ||
		p == "/site" || strings.HasPrefix(p, "/site.") ||
		strings.HasPrefix(p, "/f/site") || p == "/posts.json" || p == "/f/posts.json" ||
		strings.HasPrefix(p, "/f/u") || strings.HasPrefix(p, "/l/api/validate") {
		return "accounts"
	}
	if strings.HasPrefix(p, "/admin/users") {
		return "accounts"
	}
	// --- Website & Dashboard (default: pages, forum, downloads, static) ---
	return "website"
}

// serviceGuard rejects paths owned by another service (used by every mode
// except serve and the website front, which proxies instead).
func serviceGuard(w http.ResponseWriter, r *http.Request, mine string) bool {
	if owner := pathOwner(r.URL.Path); owner != mine {
		sendJSON(w, http.StatusNotFound, map[string]string{
			"error": "this path is served by the " + owner + " service, not " + mine,
		})
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// accounts freshness (every account reader except the accounts exe)
// ---------------------------------------------------------------------------

var accountsLoadedAt time.Time

func maybeReloadAccounts() {
	if runMode == "serve" || runMode == "accounts" {
		return
	}
	st, err := os.Stat(accPath)
	if err != nil || !st.ModTime().After(accountsLoadedAt) {
		return
	}
	accMu.Lock()
	defer accMu.Unlock()
	if b, err := os.ReadFile(accPath); err == nil {
		var fresh Accounts
		if json.Unmarshal(b, &fresh) == nil {
			if fresh.Sessions == nil {
				fresh.Sessions = map[string]Session{}
			}
			if fresh.ClientTokens == nil {
				fresh.ClientTokens = map[string]ClientToken{}
			}
			if fresh.Upvotes == nil {
				fresh.Upvotes = map[string][]int64{}
			}
			if fresh.JoinCodes == nil {
				fresh.JoinCodes = map[string]JoinTarget{}
			}
			acc = &fresh
			accountsLoadedAt = st.ModTime()
			log.Printf("[services] reloaded accounts.json (%d users, %d keys)", len(acc.Users), len(acc.ServerKeys))
		}
	}
}

// ---------------------------------------------------------------------------
// internal endpoint: master -> accounts account binds (join codes, last endpoint)
// ---------------------------------------------------------------------------

func internalAllowed(r *http.Request) bool {
	if internalToken != "" {
		return r.Header.Get("X-NSM-Internal-Token") == internalToken
	}
	host, _, err := splitHostPortCompat(r.RemoteAddr)
	return err == nil && isLoopbackHost(host)
}

type internalBindRequest struct {
	KeyID    string `json:"keyID"`
	Endpoint string `json:"endpoint"`
}

// internalBindHandler (accounts mode) applies the heartbeat's account-side
// effects: join-code binding and the key's last-endpoint marker.
func internalBindHandler(w http.ResponseWriter, r *http.Request) {
	if !internalAllowed(r) {
		sendJSON(w, http.StatusForbidden, map[string]string{"error": "internal only"})
		return
	}
	var req internalBindRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	req.KeyID = strings.TrimSpace(req.KeyID)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	if req.Endpoint == "" {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint required"})
		return
	}
	// bindJoinCodeEndpoint / joinCodeForEndpoint lock accMu internally —
	// do not hold it here.
	if req.KeyID != "" {
		accMu.Lock()
		ensureAccountsInitializedLocked()
		for i := range acc.ServerKeys {
			if acc.ServerKeys[i].ID == req.KeyID {
				acc.ServerKeys[i].LastEndpoint = req.Endpoint
				acc.ServerKeys[i].LastUsedAt = time.Now().Unix()
				break
			}
		}
		saveAccountsLocked()
		accMu.Unlock()
		bindJoinCodeEndpoint(req.KeyID, req.Endpoint)
	}
	code := joinCodeForEndpoint(req.Endpoint)
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "code": code})
}

// bindHeartbeatAccounts keeps shareable join codes pointed at a server's live
// endpoint. serve mode applies it directly; master mode posts it to the
// accounts service (the accounts.json writer) and fails open — the next
// heartbeat retries.
func bindHeartbeatAccounts(keyID, endPoint string) {
	if runMode != "master" {
		if keyID != "" {
			bindJoinCodeEndpoint(keyID, endPoint)
		}
		joinCodeForEndpoint(endPoint)
		return
	}
	if strings.TrimSpace(accountsBaseURL) == "" {
		return
	}
	payload, _ := json.Marshal(internalBindRequest{KeyID: keyID, Endpoint: endPoint})
	req, err := http.NewRequest("POST", strings.TrimRight(accountsBaseURL, "/")+"/internal/accounts/bind", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if internalToken != "" {
		req.Header.Set("X-NSM-Internal-Token", internalToken)
	}
	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		log.Printf("[services] heartbeat bind via accounts failed (will retry): %v", err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	maybeReloadAccounts()
}

// ---------------------------------------------------------------------------
// live server rows fetched from the master service
// ---------------------------------------------------------------------------

var (
	remoteRowsMu  sync.Mutex
	remoteRows    []map[string]interface{}
	remoteRowsAt  time.Time
	remoteRowsErr error
)

// normalizeRow restores the typed fields after a JSON round-trip (numbers
// decode as float64, which the row sort and templates do not expect).
func normalizeRow(m map[string]interface{}) {
	for _, k := range []string{"Clients", "MaxClients", "IconVersion", "Resources", "players", "maxPlayers", "iconVersion", "resourcesCount", "serverVersion"} {
		if v, ok := m[k].(float64); ok {
			m[k] = int64(v)
		}
	}
	for _, k := range []string{"Upvotes", "BurstPower", "upvotes", "upvotePower", "burstPower"} {
		if v, ok := m[k].(float64); ok {
			m[k] = int(v)
		}
	}
	for _, k := range []string{"Tags", "ResourceList", "resources", "PlayersList", "playersList"} {
		if arr, ok := m[k].([]interface{}); ok {
			out := make([]string, 0, len(arr))
			for _, e := range arr {
				out = append(out, fmt.Sprintf("%v", e))
			}
			m[k] = out
		}
	}
	for _, k := range []string{"Featured", "Private", "featured", "private"} {
		if v, ok := m[k].(bool); ok {
			m[k] = v
		}
	}
}

func fetchRemoteRows() []map[string]interface{} {
	remoteRowsMu.Lock()
	defer remoteRowsMu.Unlock()
	if time.Since(remoteRowsAt) < 3*time.Second {
		return remoteRows
	}
	remoteRowsAt = time.Now()
	resp, err := statusHTTPClient.Get(strings.TrimRight(masterBaseURL, "/") + "/api/servers/live")
	if err != nil {
		remoteRowsErr = err
		return remoteRows
	}
	defer resp.Body.Close()
	var body struct {
		Servers []map[string]interface{} `json:"servers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		remoteRowsErr = err
		return remoteRows
	}
	for _, row := range body.Servers {
		normalizeRow(row)
	}
	remoteRows = body.Servers
	remoteRowsErr = nil
	return remoteRows
}

// startRemoteListLoop keeps this service's view of the server list warm.
// The website also refreshes state.json (owner feed) which master owns.
func startRemoteListLoop() {
	maybeReloadAccounts()
	fetchRemoteRows()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			fetchRemoteRows()
			maybeReloadAccounts()
			if runMode == "website" {
				maybeReloadState()
			}
		}
	}()
}

var stateLoadedAt time.Time

func maybeReloadState() {
	if runMode != "website" {
		return
	}
	st, err := os.Stat(statePath())
	if err != nil || !st.ModTime().After(stateLoadedAt) {
		return
	}
	loadState()
	stateLoadedAt = st.ModTime()
}

// serverDataFromRow rebuilds a ServerData from a cached list row (used by
// the detail page, the join service and the client feed).
func serverDataFromRow(row map[string]interface{}) *ServerData {
	d := &ServerData{
		Hostname:     fmt.Sprintf("%v", row["Hostname"]),
		Gametype:     fmt.Sprintf("%v", row["GameType"]),
		Mapname:      fmt.Sprintf("%v", row["MapName"]),
		Clients:      rowInt64(row, "Clients"),
		SVMaxclients: rowInt64(row, "MaxClients"),
		UpvotePower:  rowInt64(row, "Upvotes"),
		BurstPower:   rowInt64(row, "BurstPower"),
		IconVersion:  rowInt64(row, "IconVersion"),
		Server:       fmt.Sprintf("%v", row["ServerVersion"]),
		Vars: map[string]string{
			"sv_projectDesc":       fmt.Sprintf("%v", row["Desc"]),
			"tags":                 strings.Join(rowStrings(row, "Tags"), ","),
			"locale":               fmt.Sprintf("%v", row["Locale"]),
			"onesync_enabled":      fmt.Sprintf("%v", row["OneSync"]),
			"sv_enforceGameBuild":  fmt.Sprintf("%v", row["GameBuild"]),
			"sv_pureLevel":         fmt.Sprintf("%v", row["PureLevel"]),
			"sv_scriptHookAllowed": fmt.Sprintf("%v", row["ScriptHook"]),
			"banner_detail":        fmt.Sprintf("%v", row["BannerDetail"]),
			"banner_connecting":    fmt.Sprintf("%v", row["BannerConnecting"]),
		},
	}
	if priv, _ := row["Private"].(bool); priv {
		d.Vars["nsm_private"] = "1"
	}
	return d
}

// serverView resolves a server for the detail page: registry entry in
// serve/master mode, cached master row otherwise.
func serverView(ep string) (*ServerData, string, bool) {
	if !remoteRowsMode() {
		d := reg.get(ep)
		if d == nil {
			return nil, "", false
		}
		applyMods(ep, d)
		owner := ""
		reg.mu.RLock()
		if ent, ok := reg.servers[ep]; ok && ent.ownerID != 0 {
			accMu.Lock()
			if ou := userByID(ent.ownerID); ou != nil {
				owner = ou.Username
			}
			accMu.Unlock()
		}
		reg.mu.RUnlock()
		return d, owner, true
	}
	for _, row := range fetchRemoteRows() {
		if row["EndPoint"] != ep {
			continue
		}
		owner, _ := row["OwnerUsername"].(string)
		return serverDataFromRow(row), owner, true
	}
	return nil, "", false
}

func rowInt64(m map[string]interface{}, k string) int64 {
	if v, ok := m[k].(int64); ok {
		return v
	}
	if v, ok := m[k].(float64); ok {
		return int64(v)
	}
	return 0
}

func rowStrings(m map[string]interface{}, k string) []string {
	if v, ok := m[k].([]string); ok {
		return v
	}
	return nil
}

// feedEntries builds the client-feed entry set: from the registry in
// serve/master mode, from cached rows in the feed exe.
func feedEntries() map[string]*entry {
	if !remoteRowsMode() {
		live := reg.live()
		out := make(map[string]*entry, len(live))
		for ep, e := range live {
			d := e.data
			applyMods(ep, &d)
			decorateListVars(ep, &d)
			out[ep] = &entry{data: d, last: e.last}
		}
		return out
	}
	rows := fetchRemoteRows()
	out := make(map[string]*entry, len(rows))
	for _, row := range rows {
		ep, _ := row["EndPoint"].(string)
		if ep == "" {
			continue
		}
		d := serverDataFromRow(row)
		decorateListVars(ep, d)
		out[ep] = &entry{data: *d, last: time.Now()}
	}
	return out
}

// ---------------------------------------------------------------------------
// proxies and per-mode handlers
// ---------------------------------------------------------------------------

func reverseProxy(target string) http.Handler {
	u, err := url.Parse(target)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sendJSON(w, http.StatusBadGateway, map[string]string{"error": "bad proxy target"})
		})
	}
	return httputil.NewSingleHostReverseProxy(u)
}

// rootHandler assembles the process handler for the selected mode.
func rootHandler() http.Handler {
	switch runMode {
	case "master":
		mux := http.NewServeMux()
		registerUIRoutes(mux) // /health, streamRedir (blocked below), ingress, counts
		mux.HandleFunc("GET /api/servers/live", apiServersLive)
		accountsProxy := reverseProxy(accountsBaseURL)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			// account writes belong to the accounts service
			if strings.HasPrefix(p, "/api/upvote") {
				accountsProxy.ServeHTTP(w, r)
				return
			}
			if p == "/health" || strings.HasPrefix(p, "/api/serverlist/") ||
				p == "/api/servers/live" || strings.HasPrefix(p, "/api/servers/single/") ||
				strings.HasPrefix(p, "/api/servers/icon/") || strings.HasPrefix(p, "/runtime/") ||
				strings.HasPrefix(p, "/v1/public/") || strings.HasPrefix(p, "/admin/") {
				maybeReloadAccounts()
				mux.ServeHTTP(w, r)
				return
			}
			sendJSON(w, http.StatusNotFound, map[string]string{
				"error": "NSM-Masterlist runs the master list only; " + pathOwner(p) + " paths are served by their own exe",
			})
		})
	case "accounts":
		site := buildMux()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/internal/accounts/bind" {
				internalBindHandler(w, r)
				return
			}
			if r.URL.Path == "/health" {
				site.ServeHTTP(w, r)
				return
			}
			if !serviceGuard(w, r, "accounts") {
				return
			}
			site.ServeHTTP(w, r)
		})
	case "join":
		mux := http.NewServeMux()
		mux.HandleFunc("GET /join/{ep}", handleJoinRoute)
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
			sendJSON(w, 200, map[string]string{"ok": "join"})
		})
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			maybeReloadAccounts()
			mux.ServeHTTP(w, r)
		})
	case "feed":
		mux := http.NewServeMux()
		mux.HandleFunc("/api/servers/streamRedir", serveStreamRedir)
		mux.HandleFunc("/api/servers/streamRedir/", serveStreamRedir)
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
			sendJSON(w, 200, map[string]string{"ok": "feed"})
		})
		return mux
	case "website":
		site := buildMux()
		proxies := map[string]http.Handler{
			"master":   reverseProxy(masterBaseURL),
			"feed":     reverseProxy(feedBaseURL),
			"join":     reverseProxy(joinBaseURL),
			"accounts": reverseProxy(accountsBaseURL),
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			owner := pathOwner(r.URL.Path)
			if owner == "website" {
				site.ServeHTTP(w, r)
				return
			}
			if ph := proxies[owner]; ph != nil {
				ph.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		})
	case "status":
		mux := http.NewServeMux()
		mux.HandleFunc("GET /status", statusPage)
		mux.HandleFunc("GET /api/status", apiStatus)
		return mux
	default: // serve — everything in one process, exactly as before
		return localHandler()
	}
}

// startModeServices runs the per-mode background work. probeAddr is the
// loopback address of this process (for self-monitoring).
func startModeServices(probeAddr string) {
	switch runMode {
	case "serve":
		startShowcaseServers()
		startStatusMonitor(map[string]string{
			"website": "http://" + probeAddr, "master": "http://" + probeAddr,
			"feed": "http://" + probeAddr, "join": "http://" + probeAddr,
			"accounts": "http://" + probeAddr,
		})
	case "master":
		startShowcaseServers()
	case "accounts", "join", "feed":
		startRemoteListLoop()
	case "website":
		startRemoteListLoop()
		startStatusMonitor(map[string]string{
			"website":  "http://" + probeAddr,
			"master":   masterBaseURL,
			"feed":     feedBaseURL,
			"join":     joinBaseURL,
			"accounts": accountsBaseURL,
		})
	case "status":
		startStatusMonitor(map[string]string{
			"website":  webProbeBase(),
			"master":   masterBaseURL,
			"feed":     feedBaseURL,
			"join":     joinBaseURL,
			"accounts": accountsBaseURL,
		})
	}
}

// webProbeBase is the website URL the standalone status mode probes.
func webProbeBase() string {
	return envOr("NSM_WEB_URL", "http://127.0.0.1:8080")
}

// splitHostPortCompat avoids importing net here just for SplitHostPort.
func splitHostPortCompat(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", nil
	}
	return addr[:i], addr[i+1:], nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	return host == "127.0.0.1" || host == "::1" || host == "localhost" || strings.HasPrefix(host, "127.")
}
