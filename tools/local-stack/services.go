package main

// Service modes — NSM-Masterlist.exe [serve|master|web|status]
//
// The platform can run as one process (serve, the default — everything in
// one cmd window exactly as before) or as separate commands so each piece
// lives in its own process:
//
//	NSM-Masterlist.exe master -bind 127.0.0.1 -port 8081
//	NSM-Masterlist.exe web    -bind 0.0.0.0 -port 8080 -master-url http://127.0.0.1:8081 -status-url http://127.0.0.1:8090
//	NSM-Masterlist.exe status -bind 127.0.0.1 -port 8090 -master-url http://127.0.0.1:8081 -web-url http://127.0.0.1:8080
//
// Splitting matters for the status page: a monitor that lives inside the
// process it watches cannot report that process as down. The status command
// runs in its own cmd window and probes the other two over HTTP.
//
// State ownership stays single-writer per file: accounts.json is written by
// web (master sends its heartbeat bind updates over the internal endpoint),
// state.json is written by master, status.json by status. Readers refresh on
// mtime. Public URLs never change: web proxies the master-owned API paths.

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
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	runMode          = "serve" // serve | master | web | status
	masterBaseURL    = ""      // where the master service listens
	webBaseURL       = ""      // where the web service listens
	statusBaseURL    = ""      // where the status service listens
	internalToken    = os.Getenv("NSM_INTERNAL_TOKEN")
	accountsReadOnly bool // master mode never writes accounts.json
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// parseRunMode strips a leading subcommand before flag parsing.
func parseRunMode() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "serve", "all", "master", "web", "status":
			runMode = os.Args[1]
			if runMode == "all" {
				runMode = "serve"
			}
			accountsReadOnly = runMode == "master"
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}
}

// ---------------------------------------------------------------------------
// accounts freshness (master reads the web-owned accounts.json)
// ---------------------------------------------------------------------------

var accountsLoadedAt time.Time

func maybeReloadAccounts() {
	if runMode != "master" {
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
// internal endpoint: master -> web account binds (join codes, last endpoint)
// ---------------------------------------------------------------------------

// internalAllowed authenticates internal service calls: a shared token when
// configured, otherwise loopback callers only.
func internalAllowed(r *http.Request) bool {
	if internalToken != "" {
		return r.Header.Get("X-NSM-Internal-Token") == internalToken
	}
	host, _, err := netSplitHostPort(r.RemoteAddr)
	return err == nil && isLoopbackHost(host)
}

func netSplitHostPort(addr string) (string, string, error) {
	h, p, err := splitHostPortCompat(addr)
	return h, p, err
}

type internalBindRequest struct {
	KeyID    string `json:"keyID"`
	Endpoint string `json:"endpoint"`
}

// internalBindHandler (web mode) applies the heartbeat's account-side
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
// endpoint. serve mode applies it directly; master mode posts it to the web
// service (the accounts.json writer) and fails open — the next heartbeat
// retries.
func bindHeartbeatAccounts(keyID, endPoint string) {
	if runMode != "master" {
		if keyID != "" {
			bindJoinCodeEndpoint(keyID, endPoint)
		}
		joinCodeForEndpoint(endPoint)
		return
	}
	if strings.TrimSpace(webBaseURL) == "" {
		return
	}
	payload, _ := json.Marshal(internalBindRequest{KeyID: keyID, Endpoint: endPoint})
	req, err := http.NewRequest("POST", strings.TrimRight(webBaseURL, "/")+"/internal/accounts/bind", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if internalToken != "" {
		req.Header.Set("X-NSM-Internal-Token", internalToken)
	}
	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		log.Printf("[services] heartbeat bind via web failed (will retry): %v", err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	maybeReloadAccounts()
}

// ---------------------------------------------------------------------------
// web mode: server rows fetched from the master service
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

// startRemoteListLoop keeps the web service's view of the world warm and
// also refreshes state.json (owner feed) which master owns.
func startRemoteListLoop() {
	maybeReloadState()
	fetchRemoteRows()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			fetchRemoteRows()
			maybeReloadState()
		}
	}()
}

var stateLoadedAt time.Time

func maybeReloadState() {
	if runMode != "web" {
		return
	}
	st, err := os.Stat(statePath())
	if err != nil || !st.ModTime().After(stateLoadedAt) {
		return
	}
	loadState()
	stateLoadedAt = st.ModTime()
}

// serverView resolves a server for the detail page: registry entry in
// serve/master mode, cached master row in web mode.
func serverView(ep string) (*ServerData, string, bool) {
	if runMode != "web" {
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
		return d, owner, true
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

// masterOwnedPrefixes are the API paths served by the master service. The
// web service proxies them so public URLs stay identical in split mode.
var masterOwnedPrefixes = []string{
	"/api/serverlist/",
	"/api/servers/live",
	"/api/servers/single/",
	"/api/servers/icon/",
	"/api/servers/streamRedir",
	"/runtime/",
	"/v1/public/",
	"/admin/",
	"/health",
}

func isMasterOwned(path string) bool {
	for _, p := range masterOwnedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return path == "/api/servers/live" || path == "/health"
}

// rootHandler assembles the process handler for the selected mode.
func rootHandler() http.Handler {
	switch runMode {
	case "master":
		mux := http.NewServeMux()
		registerUIRoutes(mux) // /health, streamRedir, ingress, counts, ...
		mux.HandleFunc("GET /api/servers/live", apiServersLive)
		webProxy := reverseProxy(webBaseURL)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// account writes belong to the web service
			if strings.HasPrefix(r.URL.Path, "/api/upvote") {
				webProxy.ServeHTTP(w, r)
				return
			}
			maybeReloadAccounts()
			mux.ServeHTTP(w, r)
		})
	case "web":
		site := buildMux()
		muxProxy := reverseProxy(masterBaseURL)
		statProxy := reverseProxy(statusBaseURL)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if p == "/internal/accounts/bind" {
				internalBindHandler(w, r)
				return
			}
			if p == "/status" || p == "/api/status" {
				if statusBaseURL == "" {
					http.NotFound(w, r)
					return
				}
				statProxy.ServeHTTP(w, r)
				return
			}
			if isMasterOwned(p) {
				muxProxy.ServeHTTP(w, r)
				return
			}
			site.ServeHTTP(w, r)
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
// loopback address of this process (for the serve-mode self-monitor).
func startModeServices(probeAddr string) {
	switch runMode {
	case "serve":
		startShowcaseServers()
		startStatusMonitor("http://"+probeAddr, "http://"+probeAddr)
	case "master":
		startShowcaseServers()
	case "web":
		startRemoteListLoop()
	case "status":
		startStatusMonitor(masterBaseURL, webBaseURL)
	}
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

// atoiSafe is a tiny helper for port defaults in tests.
func atoiSafe(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
