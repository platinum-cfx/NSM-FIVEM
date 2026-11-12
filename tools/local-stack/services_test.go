package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withMode(t *testing.T, mode string) {
	t.Helper()
	oldMode, oldRO := runMode, accountsReadOnly
	runMode = mode
	accountsReadOnly = mode == "master"
	t.Cleanup(func() { runMode, accountsReadOnly = oldMode, oldRO })
}

func withServiceURLs(t *testing.T, master, web, status string) {
	t.Helper()
	oldM, oldW, oldS := masterBaseURL, webBaseURL, statusBaseURL
	masterBaseURL, webBaseURL, statusBaseURL = master, web, status
	t.Cleanup(func() { masterBaseURL, webBaseURL, statusBaseURL = oldM, oldW, oldS })
}

func TestNormalizeRow(t *testing.T) {
	row := map[string]interface{}{
		"Clients":    float64(650),
		"MaxClients": float64(2048),
		"Upvotes":    float64(12),
		"Tags":       []interface{}{"roleplay", "vMenu"},
		"Private":    true,
	}
	normalizeRow(row)
	if got := row["Clients"].(int64); got != 650 {
		t.Fatalf("Clients = %v (%T), want int64 650", got, got)
	}
	if got := row["Upvotes"].(int); got != 12 {
		t.Fatalf("Upvotes = %v (%T), want int 12", got, got)
	}
	if tags := row["Tags"].([]string); len(tags) != 2 || tags[0] != "roleplay" {
		t.Fatalf("Tags = %#v", tags)
	}
	if row["Private"].(bool) != true {
		t.Fatal("Private lost")
	}
}

func TestInternalBindEndpoint(t *testing.T) {
	setupTest(t)
	withMode(t, "web")
	oldTok := internalToken
	internalToken = ""
	t.Cleanup(func() { internalToken = oldTok })

	req := httptest.NewRequest("POST", "/internal/accounts/bind", strings.NewReader(`{"endpoint":"203.0.113.9:30120"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	internalBindHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("loopback bind: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK || out.Code == "" {
		t.Fatalf("bind response %q err=%v", rec.Body.String(), err)
	}
	if ep := joinCodeForEndpointLookup("203.0.113.9:30120"); ep != out.Code {
		t.Fatalf("stored code %q, want %q", ep, out.Code)
	}

	// non-loopback caller is rejected
	req = httptest.NewRequest("POST", "/internal/accounts/bind", strings.NewReader(`{"endpoint":"203.0.113.9:30120"}`))
	req.RemoteAddr = "192.0.2.55:4444"
	rec = httptest.NewRecorder()
	internalBindHandler(rec, req)
	if rec.Code != 403 {
		t.Fatalf("external bind: %d, want 403", rec.Code)
	}

	// token mode: wrong token rejected, right token accepted
	internalToken = "sekrit"
	req = httptest.NewRequest("POST", "/internal/accounts/bind", strings.NewReader(`{"endpoint":"203.0.113.10:30120"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec = httptest.NewRecorder()
	internalBindHandler(rec, req)
	if rec.Code != 403 {
		t.Fatalf("wrong token: %d, want 403", rec.Code)
	}
	req = httptest.NewRequest("POST", "/internal/accounts/bind", strings.NewReader(`{"endpoint":"203.0.113.10:30120"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-NSM-Internal-Token", "sekrit")
	rec = httptest.NewRecorder()
	internalBindHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("right token: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBindHeartbeatAccounts(t *testing.T) {
	setupTest(t)

	// master mode: the update travels to the web service
	var got atomic.Value
	fakeWeb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/accounts/bind" {
			http.NotFound(w, r)
			return
		}
		var req internalBindRequest
		json.NewDecoder(r.Body).Decode(&req)
		got.Store(req)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "code": "abc123"})
	}))
	defer fakeWeb.Close()

	withMode(t, "master")
	withServiceURLs(t, "", fakeWeb.URL, "")
	bindHeartbeatAccounts("key-1", "203.0.113.5:30120")
	req, _ := got.Load().(internalBindRequest)
	if req.KeyID != "key-1" || req.Endpoint != "203.0.113.5:30120" {
		t.Fatalf("web got %#v", req)
	}

	// serve mode: applied in-process
	withMode(t, "serve")
	bindHeartbeatAccounts("", "203.0.113.6:30120")
	if c := joinCodeForEndpointLookup("203.0.113.6:30120"); c == "" {
		t.Fatal("serve-mode bind did not mint a join code")
	}
}

func TestWebRowsFromMaster(t *testing.T) {
	setupTest(t)
	var hits atomic.Int32
	fakeMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/servers/live" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"servers": []map[string]interface{}{
				{
					"EndPoint": "203.0.113.7:30120", "JoinCode": "ab12xyz",
					"Hostname": "Test Server", "CleanName": "Test Server", "Clients": int64(42),
					"MaxClients": int64(64), "Upvotes": 7, "Private": true, "Tags": []string{"roleplay"},
				},
			},
			"players": 42,
			"count":   1,
		})
	}))
	defer fakeMaster.Close()

	withMode(t, "web")
	withServiceURLs(t, fakeMaster.URL, "", "")
	remoteRowsMu.Lock()
	remoteRows, remoteRowsAt = nil, time.Time{}
	remoteRowsMu.Unlock()

	rows := liveServers()
	if len(rows) != 1 || rows[0]["EndPoint"] != "203.0.113.7:30120" {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0]["Clients"].(int64) != 42 {
		t.Fatalf("Clients type/value = %#v", rows[0]["Clients"])
	}
	if totalPlayers() != 42 {
		t.Fatalf("totalPlayers = %d", totalPlayers())
	}
	if d, owner, ok := serverView("203.0.113.7:30120"); !ok || d.Clients != 42 || d.Vars["nsm_private"] != "1" || owner != "" {
		t.Fatalf("serverView = %#v owner=%q ok=%v", d, owner, ok)
	}
	if _, _, ok := serverView("203.0.113.99:30120"); ok {
		t.Fatal("serverView found a server that is not listed")
	}
	// the 3s cache collapses repeated reads into one fetch
	n := hits.Load()
	liveServers()
	liveServers()
	if hits.Load() != n {
		t.Fatal("cache missed: master hit twice within 3s")
	}
}

func TestMasterRoutesUpvoteToWeb(t *testing.T) {
	setupTest(t)
	var got atomic.Int32
	fakeWeb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(200)
	}))
	defer fakeWeb.Close()

	withMode(t, "master")
	withServiceURLs(t, "", fakeWeb.URL, "")
	h := rootHandler()

	req := httptest.NewRequest("GET", "/api/upvote/203.0.113.7:30120", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got.Load() != 1 {
		t.Fatalf("/api/upvote was not proxied to web (hits=%d)", got.Load())
	}
}
