package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTest(t *testing.T) http.Handler {
	t.Helper()
	reg = newRegistry()
	state = ownerState{Servers: map[string]serverMods{}, FeedIDs: map[string]bool{}}
	ticketPublicKey = ""
	requireKeys = false
	keyClock = time.Now
	tmp := t.TempDir()
	accPath = filepath.Join(tmp, "accounts.json")
	forumPath = filepath.Join(tmp, "forum.json")
	loadAccounts()
	loadForum()
	initSite()
	return localHandler()
}
func request(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestRouteContracts(t *testing.T) {
	h := setupTest(t)
	cases := []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/api/identities", 200, `{"identities":[]}`},
		{"POST", "/api/ticket/identities", 200, `{"identities":[]}`},
		{"GET", "/api/ticket/identities", 200, `{"identities":[]}`},
		{"POST", "/api/identities", 503, `identity linking`},
		{"GET", "/v1/public/featured-servers", 200, `"pinnedServers":[]`},
		{"GET", "/v1/public/tweets", 200, `[]`},
		{"GET", "/v1/public/notices/fivem", 200, `[]`},
		{"GET", "/v1/public/pool-size-limits/fivem", 200, `{}`},
		{"GET", "/runtime/counts.json", 200, `[0,null,null]`},
		{"GET", "/api/servers/streamRedir/", 200, ``},
		{"GET", "/api/servers/streamRedir", 200, ``},
		{"GET", "/api/servers/single/127.0.0.1:30120", 404, `{}`},
		{"GET", "/api/servers/icon/127.0.0.1:30120/0.png", 404, ``},
		{"GET", "/api/ticket/pubkey", 503, `no trusted public key`},
		{"GET", "/api/upvote/", 403, `unavailable`},
		{"POST", "/api/upvote/", 403, `unavailable`},
		{"GET", "/api/serverlist/ingress", 405, `POST required`},
		{"POST", "/v1/public/tweets", 405, `method not allowed`},
		{"OPTIONS", "/api/identities", 204, ``},
		{"GET", "/health", 200, `"version":"0.7.7-local"`},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			w := request(h, c.method, c.path, "")
			if w.Code != c.code || !strings.Contains(w.Body.String(), c.body) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatal("CORS missing")
			}
			if w.Header().Get("X-NSM-Backend") != backendVersion {
				t.Fatal("version missing")
			}
		})
	}
}

const validHeartbeat = `{"port":30120,"listingToken":"","ipOverride":"","private":true,"fallbackData":{"players":[],"info":{"info":{"resources":["chat"],"version":"test-fixture","vars":{"sv_projectName":"Integration test fixture"}}},"dynamic":{"dynamic":{"clients":2,"svMaxclients":48,"hostname":"TEST FIXTURE - NOT A LIVE GAME SERVER"}}}}`

func TestHeartbeatStreamAndExpiry(t *testing.T) {
	h := setupTest(t)
	if w := request(h, "POST", "/api/serverlist/ingress", validHeartbeat); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := request(h, "GET", "/api/servers/streamRedir/", "")
	b := w.Body.Bytes()
	if len(b) < 4 || int(binary.LittleEndian.Uint32(b)) != len(b)-4 {
		t.Fatal("invalid frame length")
	}
	if !bytes.Contains(b, []byte("127.0.0.1:30120")) || !bytes.Contains(b, []byte("gta5")) {
		t.Fatal("frame fields missing")
	}
	w = request(h, "GET", "/api/servers/single/127.0.0.1:30120", "")
	var server struct {
		EndPoint string
		Data     ServerData
	}
	if json.Unmarshal(w.Body.Bytes(), &server) != nil || server.Data.Clients != 2 || len(server.Data.ConnectEndPoints) != 1 {
		t.Fatal(w.Body.String())
	}
	if w = request(h, "GET", "/runtime/counts.json", ""); w.Body.String() != "[2,null,null]" {
		t.Fatal(w.Body.String())
	}
	reg.mu.Lock()
	reg.servers[server.EndPoint].last = time.Now().Add(-heartbeatTTL - time.Second)
	reg.mu.Unlock()
	if w = request(h, "GET", "/api/servers/single/127.0.0.1:30120", ""); w.Code != 404 {
		t.Fatal("stale single")
	}
	if w = request(h, "GET", "/api/servers/streamRedir/", ""); w.Body.Len() != 0 {
		t.Fatal("stale stream")
	}
	if len(reg.servers) != 0 {
		t.Fatal("stale record not pruned")
	}
}
func makePNG(t *testing.T, w, h int, tint uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), tint, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func withIcon(body, iconB64 string) string {
	return strings.Replace(body, `"resources":["chat"]`, `"resources":["chat"],"icon":"`+iconB64+`"`, 1)
}

func TestServerIconLoadAndServe(t *testing.T) {
	h := setupTest(t)
	ep := "127.0.0.1:30120"
	single := func() int64 {
		t.Helper()
		w := request(h, "GET", "/api/servers/single/"+ep, "")
		var server struct {
			EndPoint string
			Data     ServerData
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &server) != nil {
			t.Fatalf("single: %d %s", w.Code, w.Body.String())
		}
		return server.Data.IconVersion
	}
	iconURL := func(ver int64) string {
		return "/api/servers/icon/" + ep + "/" + fmt.Sprintf("%d", ver) + ".png"
	}

	// 1. Non-96x96 PNGs and garbage are rejected but never break the heartbeat.
	if w := request(h, "POST", "/api/serverlist/ingress", withIcon(validHeartbeat, base64.StdEncoding.EncodeToString(makePNG(t, 100, 100, 1)))); w.Code != 200 {
		t.Fatalf("heartbeat with 100x100 icon: %d %s", w.Code, w.Body.String())
	}
	if v := single(); v != 0 {
		t.Fatalf("expected no iconVersion for 100x100 icon, got %d", v)
	}
	if w := request(h, "POST", "/api/serverlist/ingress", withIcon(validHeartbeat, "!!!not-base64!!!")); w.Code != 200 {
		t.Fatalf("heartbeat with invalid icon must stay up: %d %s", w.Code, w.Body.String())
	}

	// 2. A 96x96 PNG is accepted and served at the icon URL.
	pngA := makePNG(t, 96, 96, 10)
	b64A := base64.StdEncoding.EncodeToString(pngA)
	if w := request(h, "POST", "/api/serverlist/ingress", withIcon(validHeartbeat, b64A)); w.Code != 200 {
		t.Fatalf("heartbeat with icon: %d %s", w.Code, w.Body.String())
	}
	verA := single()
	if verA == 0 {
		t.Fatal("expected iconVersion for 96x96 icon")
	}
	wIcon := request(h, "GET", iconURL(verA), "")
	if wIcon.Code != 200 || wIcon.Header().Get("Content-Type") != "image/png" || !bytes.Equal(wIcon.Body.Bytes(), pngA) {
		t.Fatalf("icon GET: %d %s", wIcon.Code, wIcon.Body.String())
	}

	// 3. Wrong or missing versions 404 (contract: client falls back to avatar).
	if w := request(h, "GET", iconURL(0), ""); w.Code != 404 {
		t.Fatalf("version 0 icon: %d", w.Code)
	}
	if w := request(h, "GET", iconURL(verA+1), ""); w.Code != 404 {
		t.Fatalf("stale version icon: %d", w.Code)
	}
	if w := request(h, "GET", "/api/servers/icon/203.0.113.9:30120/1.png", ""); w.Code != 404 {
		t.Fatalf("unknown endpoint icon: %d", w.Code)
	}

	// 4. Replacing the logo bumps the version and orphans the old URL.
	pngB := makePNG(t, 96, 96, 200)
	if w := request(h, "POST", "/api/serverlist/ingress", withIcon(validHeartbeat, base64.StdEncoding.EncodeToString(pngB))); w.Code != 200 {
		t.Fatalf("heartbeat with new icon: %d %s", w.Code, w.Body.String())
	}
	verB := single()
	if verB == 0 || verB == verA {
		t.Fatalf("icon version should change: %d -> %d", verA, verB)
	}
	if w := request(h, "GET", iconURL(verB), ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pngB) {
		t.Fatalf("new icon GET: %d", w.Code)
	}
	if w := request(h, "GET", iconURL(verA), ""); w.Code != 404 {
		t.Fatalf("old icon URL should 404 after replacement: %d", w.Code)
	}

	// 5. A heartbeat without icon clears the logo (load_server_icon removed).
	if w := request(h, "POST", "/api/serverlist/ingress", validHeartbeat); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if v := single(); v != 0 {
		t.Fatalf("iconVersion should clear, got %d", v)
	}
	if w := request(h, "GET", iconURL(verB), ""); w.Code != 404 {
		t.Fatalf("cleared icon should 404: %d", w.Code)
	}

	// 6. Icons expire with their server entry after the heartbeat TTL.
	if w := request(h, "POST", "/api/serverlist/ingress", withIcon(validHeartbeat, b64A)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	verC := single()
	if verC == 0 {
		t.Fatal("expected iconVersion")
	}
	reg.mu.Lock()
	reg.servers[ep].last = time.Now().Add(-heartbeatTTL - time.Second)
	reg.mu.Unlock()
	if w := request(h, "GET", iconURL(verC), ""); w.Code != 404 {
		t.Fatalf("icon for expired server should 404: %d", w.Code)
	}
}

func TestMalformedHeartbeats(t *testing.T) {
	h := setupTest(t)
	for _, body := range []string{`{}`, `null`, `[]`, `{`, `{} {}`, `{"port":-1}`, `{"port":65536}`, `{"ipOverride":"8.8.8.8"}`, `{"fallbackData":{"info":[]}}`, `{"fallbackData":{"dynamic":{"clients":-1}}}`, `{"fallbackData":{"dynamic":{"clients":1.5}}}`, `{"fallbackData":{"dynamic":{"clients":2147483648}}}`, `{"fallbackData":{"info":{"resources":[42]}}}`, `{"fallbackData":{"info":{"vars":[]}}}`, `{"fallbackData":{"players":{}}}`} {
		w := request(h, "POST", "/api/serverlist/ingress", body)
		if w.Code < 400 {
			t.Errorf("accepted %s", body)
		}
	}
	if len(reg.live()) != 0 {
		t.Fatal("invalid entry stored")
	}
	w := request(h, "POST", "/api/serverlist/ingress", `{"x":"`+strings.Repeat("a", 1<<20)+`"}`)
	if w.Code != 413 {
		t.Fatalf("oversize %d", w.Code)
	}
}
func TestRealCountsNotFabricated(t *testing.T) {
	h := setupTest(t)
	request(h, "POST", "/api/serverlist/ingress", validHeartbeat)
	state.Servers["127.0.0.1:30120"] = serverMods{FakePlayerCnt: 9000}
	w := request(h, "GET", "/runtime/counts.json", "")
	if w.Body.String() != "[2,null,null]" {
		t.Fatal(w.Body.String())
	}
}
func TestProxyDoesNotCacheCredentials(t *testing.T) {
	setupTest(t)
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("credential-response")) }))
	defer upstream.Close()
	r := httptest.NewRequest("GET", "/l/api/validate/entitlement/rosfive", nil)
	w := httptest.NewRecorder()
	serveProxy(w, r, "l", upstream.URL)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if _, err := os.Stat(cachePath("l", "api/validate/entitlement/rosfive", "")); !os.IsNotExist(err) {
		t.Fatal("credentials cached")
	}
}
func TestExistingCacheStillWorks(t *testing.T) {
	setupTest(t)
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("game-file")) }))
	for i := 0; i < 2; i++ {
		if i == 1 {
			upstream.Close()
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/c/mirrors/file.bin", nil)
		serveProxy(w, r, "c", upstream.URL)
		if w.Code != 200 || w.Body.String() != "game-file" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	files, _ := filepath.Glob("mirror-cache/c/*")
	if len(files) != 1 {
		t.Fatal(files)
	}
}
func TestUnavailableUpstream(t *testing.T) {
	setupTest(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/l/test", nil)
	serveProxy(w, r, "l", "http://127.0.0.1:1")
	if w.Code != 502 {
		t.Fatal(w.Code)
	}
}
func TestSpoofedForwardingIgnored(t *testing.T) {
	h := setupTest(t)
	r := httptest.NewRequest("POST", "/api/serverlist/ingress", strings.NewReader(validHeartbeat))
	r.RemoteAddr = "127.0.0.1:20"
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if reg.get("127.0.0.1:30120") == nil {
		t.Fatal("spoofed endpoint")
	}
}
func TestFullHTTPPipeline(t *testing.T) {
	h := setupTest(t)
	s := httptest.NewServer(h)
	defer s.Close()
	resp, err := http.Post(s.URL+"/api/serverlist/ingress", "application/json", strings.NewReader(validHeartbeat))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = http.Get(s.URL + "/api/servers/streamRedir/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if len(b) < 4 {
		t.Fatal("empty HTTP stream")
	}
}

func TestTrustedPublicKeyOnly(t *testing.T) {
	h := setupTest(t)
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	path := filepath.Join(t.TempDir(), "public.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der}), 0600)
	ticketPublicKey, err = readTicketPublicKey(path)
	if err != nil {
		t.Fatal(err)
	}
	w := request(h, "GET", "/api/ticket/pubkey", "")
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result["key"] != base64.StdEncoding.EncodeToString(der) {
		t.Fatal(w.Code, w.Body.String())
	}
	os.WriteFile(path, []byte("invalid key"), 0600)
	if _, err = readTicketPublicKey(path); err == nil {
		t.Fatal("invalid public key accepted")
	}
}
func TestNonLocalRequestRejected(t *testing.T) {
	h := setupTest(t)
	r := httptest.NewRequest("GET", "/api/servers/streamRedir/", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func requestAuth(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func registerTestUser(t *testing.T, h http.Handler, username string) string {
	t.Helper()
	w := requestAuth(h, "POST", "/api/auth/register",
		`{"Username":"`+username+`","Password":"Password123!","Email":"`+username+`@nsm.local"}`, nil)
	if w.Code != 200 {
		t.Fatalf("register %s failed: %d %s", username, w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "nsm_session" {
			return c.Value
		}
	}
	t.Fatalf("missing nsm_session cookie for %s", username)
	return ""
}

func TestKeymasterFullMatrix(t *testing.T) {
	h := setupTest(t)
	now := time.Unix(1700000000, 0)
	keyClock = func() time.Time { return now }
	defer func() { keyClock = time.Now; requireKeys = false }()

	aliceSess := registerTestUser(t, h, "alice_srv")
	bobSess := registerTestUser(t, h, "bob_srv")

	// 1. Keymaster portal HTML is live (no longer a stub)
	wPage := requestAuth(h, "GET", "/keymaster", "", map[string]string{"X-NSM-Session": aliceSess})
	if wPage.Code != 200 || !strings.Contains(wPage.Body.String(), "Issue server key") {
		t.Fatalf("expected live keymaster portal, got %d %s", wPage.Code, wPage.Body.String())
	}

	// 2. Issue key for Alice and Bob
	wIssueA := requestAuth(h, "POST", "/api/keymaster/issue",
		`{"label":"Alice Main","ttlSeconds":60}`, map[string]string{"X-NSM-Session": aliceSess})
	if wIssueA.Code != 201 {
		t.Fatalf("issue Alice key: %d %s", wIssueA.Code, wIssueA.Body.String())
	}
	var issueRespA struct {
		ServerKey string                 `json:"serverKey"`
		Key       map[string]interface{} `json:"key"`
	}
	json.Unmarshal(wIssueA.Body.Bytes(), &issueRespA)
	aliceKey := issueRespA.ServerKey
	aliceKeyID := issueRespA.Key["id"].(string)

	wIssueB := requestAuth(h, "POST", "/api/keymaster/issue",
		`{"label":"Bob Main","ttlSeconds":3600}`, map[string]string{"X-NSM-Session": bobSess})
	var issueRespB struct {
		ServerKey string `json:"serverKey"`
	}
	json.Unmarshal(wIssueB.Body.Bytes(), &issueRespB)
	bobKey := issueRespB.ServerKey

	// 3. Valid key via sets sv_nsmServerKey in vars authenticates & strips secret from public listing
	hbWithVarsKey := `{"port":30120,"fallbackData":{"players":[],"info":{"resources":["chat"],"vars":{"sv_nsmServerKey":"` + aliceKey + `","sv_projectName":"Alice RP"}},"dynamic":{"clients":2,"svMaxclients":32,"hostname":"Alice RP Server"}}}`
	wHB := request(h, "POST", "/api/serverlist/ingress", hbWithVarsKey)
	if wHB.Code != 200 {
		t.Fatalf("valid key heartbeat failed: %d %s", wHB.Code, wHB.Body.String())
	}
	wSingle := request(h, "GET", "/api/servers/single/127.0.0.1:30120", "")
	if wSingle.Code != 200 || strings.Contains(wSingle.Body.String(), aliceKey) {
		t.Fatalf("single server missing or leaked secret: %d %s", wSingle.Code, wSingle.Body.String())
	}

	// 4. Unknown, malformed, and wrong-secret keys rejected (fail-closed)
	badKeys := []struct {
		key  string
		code int
	}{
		{"cfx_bad_prefix", 401},
		{"nsm_srv_0011223344556677.short", 401},
		{"nsm_srv_0011223344556677." + strings.Repeat("a", 48), 403},
		{aliceKeyID + "." + strings.Repeat("f", 48), 403},
	}
	for _, bk := range badKeys {
		wBad := requestAuth(h, "POST", "/api/serverlist/ingress", validHeartbeat,
			map[string]string{"Authorization": "Bearer " + bk.key})
		if wBad.Code != bk.code {
			t.Fatalf("bad key %s expected %d got %d (%s)", bk.key, bk.code, wBad.Code, wBad.Body.String())
		}
	}

	// 5. Cross-account IDOR revoke & endpoint hijack prevented
	wIDOR := requestAuth(h, "POST", "/api/keymaster/revoke",
		`{"keyId":"`+aliceKeyID+`"}`, map[string]string{"X-NSM-Session": bobSess})
	if wIDOR.Code != 403 {
		t.Fatalf("expected 403 on cross-account revoke, got %d", wIDOR.Code)
	}
	wHijack := requestAuth(h, "POST", "/api/serverlist/ingress", validHeartbeat,
		map[string]string{"Authorization": "Bearer " + bobKey})
	if wHijack.Code != 403 || !strings.Contains(wHijack.Body.String(), "endpoint_owned_by_another_account") {
		t.Fatalf("expected 403 on endpoint hijack, got %d %s", wHijack.Code, wHijack.Body.String())
	}

	// 6. Expired key rejected and active listing pruned
	now = now.Add(61 * time.Second)
	wExp := requestAuth(h, "POST", "/api/serverlist/ingress", validHeartbeat,
		map[string]string{"Authorization": "Bearer " + aliceKey})
	if wExp.Code != 403 || !strings.Contains(wExp.Body.String(), "expired_key") {
		t.Fatalf("expected 403 expired_key, got %d %s", wExp.Code, wExp.Body.String())
	}
	if wAfterExp := request(h, "GET", "/api/servers/single/127.0.0.1:30120", ""); wAfterExp.Code != 404 {
		t.Fatalf("expired key server was not pruned: %d", wAfterExp.Code)
	}

	// 7. Revoked key immediately evicts active server and rejects future heartbeats
	hbBob := strings.Replace(validHeartbeat, `"port":30120`, `"port":30125`, 1)
	if w := requestAuth(h, "POST", "/api/serverlist/ingress", hbBob, map[string]string{"Authorization": "Bearer " + bobKey}); w.Code != 200 {
		t.Fatalf("bob heartbeat: %d %s", w.Code, w.Body.String())
	}
	var bobKeyMeta struct {
		Key map[string]interface{} `json:"key"`
	}
	json.Unmarshal(wIssueB.Body.Bytes(), &bobKeyMeta)
	bobKeyID := bobKeyMeta.Key["id"].(string)
	wRev := requestAuth(h, "POST", "/api/keymaster/revoke",
		`{"keyId":"`+bobKeyID+`"}`, map[string]string{"X-NSM-Session": bobSess})
	if wRev.Code != 200 || !strings.Contains(wRev.Body.String(), `"evictedListings":1`) {
		t.Fatalf("revoke bob key failed: %d %s", wRev.Code, wRev.Body.String())
	}
	if w := request(h, "GET", "/api/servers/single/127.0.0.1:30125", ""); w.Code != 404 {
		t.Fatalf("revoked key server still listed: %d", w.Code)
	}
	if w := requestAuth(h, "POST", "/api/serverlist/ingress", hbBob, map[string]string{"Authorization": "Bearer " + bobKey}); w.Code != 403 {
		t.Fatalf("revoked key heartbeat should fail 403, got %d", w.Code)
	}

	// 8. requireKeys mode rejects keyless heartbeats fail-closed
	requireKeys = true
	if w := request(h, "POST", "/api/serverlist/ingress", validHeartbeat); w.Code != 401 {
		t.Fatalf("expected 401 when requireKeys=true and no key sent, got %d", w.Code)
	}
}

func TestForumProfilesAndInAppClientLogin(t *testing.T) {
	h := setupTest(t)

	// 1. HTML form registration redirects to /verify-email and sets nsm_session cookie
	regForm := url.Values{
		"username": {"cravenz_dev"},
		"email":    {"dev@nsm.local"},
		"password": {"secretPass123"},
	}
	rReg := httptest.NewRequest("POST", "/register", strings.NewReader(regForm.Encode()))
	rReg.RemoteAddr = "127.0.0.1:12345"
	rReg.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rReg.Header.Set("Accept", "text/html")
	wReg := httptest.NewRecorder()
	h.ServeHTTP(wReg, rReg)
	if wReg.Code != http.StatusSeeOther || !strings.HasPrefix(wReg.Header().Get("Location"), "/verify-email") {
		t.Fatalf("expected 303 redirect to /verify-email, got %d (%s)", wReg.Code, wReg.Header().Get("Location"))
	}
	var sessionCookie *http.Cookie
	for _, c := range wReg.Result().Cookies() {
		if c.Name == "nsm_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatalf("missing nsm_session cookie on HTML registration")
	}

	// Verify email using the issued verification code
	accMu.Lock()
	regUser := findUser("cravenz_dev")
	verifyCode := ""
	if regUser != nil {
		verifyCode = regUser.EmailVerifyCode
	}
	accMu.Unlock()
	if verifyCode == "" {
		t.Fatalf("expected registration to generate a 6-digit EmailVerifyCode")
	}
	rVer := httptest.NewRequest("POST", "/account/verify-email", strings.NewReader("code="+url.QueryEscape(verifyCode)+"&return_to=%2Faccount"))
	rVer.RemoteAddr = "127.0.0.1:12345"
	rVer.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rVer.AddCookie(sessionCookie)
	wVer := httptest.NewRecorder()
	h.ServeHTTP(wVer, rVer)
	if wVer.Code != http.StatusSeeOther {
		t.Fatalf("POST /account/verify-email failed: %d", wVer.Code)
	}

	// 2. Customize user profile (DisplayName, Title, Bio, colors, social metadata)
	profForm := url.Values{
		"display_name":    {"Cravenz Official"},
		"title":           {"Lead NSM Architect"},
		"bio":             {"Building the NSM multiplayer platform and Keymaster."},
		"avatar_color":    {"#e7064f"},
		"banner_color":    {"#1e293b"},
		"location":        {"Miami, FL"},
		"discord_tag":     {"cravenz#0001"},
		"favorite_server": {"NSM Official RP"},
	}
	rProf := httptest.NewRequest("POST", "/account/profile", strings.NewReader(profForm.Encode()))
	rProf.RemoteAddr = "127.0.0.1:12345"
	rProf.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rProf.AddCookie(sessionCookie)
	wProf := httptest.NewRecorder()
	h.ServeHTTP(wProf, rProf)
	if wProf.Code != 200 || !strings.Contains(wProf.Body.String(), "Cravenz Official") {
		t.Fatalf("profile update failed: %d", wProf.Code)
	}

	// Public profile page /u/cravenz_dev & PNG avatar /avatars/u/cravenz_dev/128.png
	if wU := request(h, "GET", "/u/cravenz_dev", ""); wU.Code != 200 || !strings.Contains(wU.Body.String(), "Lead NSM Architect") {
		t.Fatalf("public profile /u/cravenz_dev failed: %d", wU.Code)
	}
	if wAv := request(h, "GET", "/avatars/u/cravenz_dev/128.png", ""); wAv.Code != 200 || wAv.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("avatar PNG failed: %d (%s)", wAv.Code, wAv.Header().Get("Content-Type"))
	}

	// 3. Forum: view /forum, create topic, view topic, reply, and like
	if wF := request(h, "GET", "/forum", ""); wF.Code != 200 || !strings.Contains(wF.Body.String(), "NSM Community Forum") {
		t.Fatalf("GET /forum failed: %d", wF.Code)
	}
	topicForm := url.Values{
		"title":    {"My New NSM Resource Release v1.0"},
		"category": {"releases"},
		"tags":     {"release,hud"},
		"content":  {"Here is our brand new custom HUD resource for NSM servers!"},
	}
	rNewTopic := httptest.NewRequest("POST", "/forum/new", strings.NewReader(topicForm.Encode()))
	rNewTopic.RemoteAddr = "127.0.0.1:12345"
	rNewTopic.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rNewTopic.AddCookie(sessionCookie)
	wNewTopic := httptest.NewRecorder()
	h.ServeHTTP(wNewTopic, rNewTopic)
	if wNewTopic.Code != http.StatusSeeOther || !strings.HasPrefix(wNewTopic.Header().Get("Location"), "/forum/t/") {
		t.Fatalf("POST /forum/new failed: %d (%s)", wNewTopic.Code, wNewTopic.Header().Get("Location"))
	}
	topicLoc := wNewTopic.Header().Get("Location")
	if wT := request(h, "GET", topicLoc, ""); wT.Code != 200 || !strings.Contains(wT.Body.String(), "My New NSM Resource Release v1.0") {
		t.Fatalf("GET %s failed: %d", topicLoc, wT.Code)
	}

	// 4. NSM Client In-App Login (RSA User-Api-Key flow + /api/auth/client-poll + /l/api/validate/discourse)
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	nonce := "00112233445566778899aabbccddeeff"
	clientID := "deadbeefcafebabe0011223344556677"

	authForm := url.Values{
		"action":           {"authorize"},
		"client_id":        {clientID},
		"nonce":            {nonce},
		"public_key":       {pubPEM},
		"application_name": {"NSM Client"},
		"auth_redirect":    {"nsm://accept-auth"},
	}
	rAuth := httptest.NewRequest("POST", "/user-api-key/new", strings.NewReader(authForm.Encode()))
	rAuth.RemoteAddr = "127.0.0.1:12345"
	rAuth.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rAuth.AddCookie(sessionCookie)
	wAuth := httptest.NewRecorder()
	h.ServeHTTP(wAuth, rAuth)
	if wAuth.Code != 200 || !strings.Contains(wAuth.Body.String(), "nsm://accept-auth?payload=") {
		t.Fatalf("POST /user-api-key/new failed: %d", wAuth.Code)
	}

	// Client poller receives the RSA-encrypted payload
	wPoll := request(h, "POST", "/api/auth/client-poll", `{"nonce":"`+nonce+`","clientId":"`+clientID+`"}`)
	if wPoll.Code != 200 {
		t.Fatalf("POST /api/auth/client-poll failed: %d", wPoll.Code)
	}
	var pollResp struct {
		OK      bool   `json:"ok"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(wPoll.Body.Bytes(), &pollResp); err != nil || !pollResp.OK || pollResp.Payload == "" {
		t.Fatalf("unexpected poll response: %s", wPoll.Body.String())
	}

	cipherBytes, err := base64.StdEncoding.DecodeString(pollResp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	plainBytes, err := rsa.DecryptPKCS1v15(rand.Reader, privKey, cipherBytes)
	if err != nil {
		t.Fatalf("RSA decrypt failed: %v", err)
	}
	var decPayload struct {
		Key   string `json:"key"`
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(plainBytes, &decPayload); err != nil || decPayload.Nonce != nonce || !strings.HasPrefix(decPayload.Key, "nsm_uapi_") {
		t.Fatalf("unexpected decrypted payload: %s", string(plainBytes))
	}

	// Verify /session/current.json and /l/api/validate/discourse with User-Api-Key
	wSess := requestAuth(h, "GET", "/session/current.json", "", map[string]string{"User-Api-Key": decPayload.Key})
	if wSess.Code != 200 || !strings.Contains(wSess.Body.String(), `"username":"cravenz_dev"`) {
		t.Fatalf("GET /session/current.json failed: %d %s", wSess.Code, wSess.Body.String())
	}
	wVal := requestAuth(h, "POST", "/l/api/validate/discourse", "", map[string]string{"User-Api-Key": decPayload.Key})
	if wVal.Code != 200 || !strings.Contains(wVal.Body.String(), `"username":"cravenz_dev"`) {
		t.Fatalf("POST /l/api/validate/discourse failed: %d %s", wVal.Code, wVal.Body.String())
	}

	// 5. Verify patchUIBundleBytes rewrites Discord invite to http://127.0.0.1, keeps root.html clean, and injects strict-mode-safe auth poller + interceptor into app/index.html
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f1, _ := zw.Create("app/index.html")
	f1.Write([]byte(`<html><head><script defer="defer" src="static/js/main.js?v=nsm078b"></script></head><body><div id="root"></div></body></html>`))
	f2, _ := zw.Create("app/static/js/main.js")
	f2.Write([]byte(`const BASE_URL = "https://discord.gg/UXj5QfCX88";`))
	f3, _ := zw.Create("root.html")
	f3.Write([]byte(`<html><head>` + uiDiscourseHeadInterceptorV2 + `</head><body></body></html>`))
	zw.Close()

	patchedZip, changed, err := patchUIBundleBytes(zbuf.Bytes())
	if err != nil || !changed {
		t.Fatalf("patchUIBundleBytes failed: changed=%v err=%v", changed, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(patchedZip), int64(len(patchedZip)))
	if err != nil {
		t.Fatal(err)
	}
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if zf.Name == "app/static/js/main.js" && !strings.Contains(string(b), `http://127.0.0.1`) {
			t.Fatalf("app/static/js/main.js was not rewritten: %s", string(b))
		}
		if zf.Name == "app/index.html" {
			htmlStr := string(b)
			for _, m := range []string{`id="nsm-client-auth-poll-v4"`, `id="nsm-discourse-interceptor"`, `src="static/js/main.js"`} {
				if !strings.Contains(htmlStr, m) {
					t.Fatalf("app/index.html missing %s: %s", m, htmlStr)
				}
			}
			for _, bad := range []string{`origFetch.call`, `?v=nsm078b`} {
				if strings.Contains(htmlStr, bad) {
					t.Fatalf("app/index.html must not contain %s: %s", bad, htmlStr)
				}
			}
		}
		if zf.Name == "root.html" && strings.Contains(string(b), "nsm-discourse-interceptor") {
			t.Fatalf("root.html must not contain nsm-discourse-interceptor: %s", string(b))
		}
	}
}

func TestDiscourseInAppLoginAndCravenzOwnerClaim(t *testing.T) {
	h := setupTest(t)

	// 1. nta~ owner account is seeded with Pass == "" and can be claimed ONCE via POST /account/claim-owner
	wCSRF := request(h, "GET", "/session/csrf.json", "")
	if wCSRF.Code != 200 || !strings.Contains(wCSRF.Body.String(), `"csrf"`) {
		t.Fatalf("GET /session/csrf.json failed: %d %s", wCSRF.Code, wCSRF.Body.String())
	}

	// Verify /login shows one-time setup popup before owner password is set
	wLoginBefore := request(h, "GET", "/login", "")
	if !strings.Contains(wLoginBefore.Body.String(), "One-Time Founder Setup") {
		t.Fatalf("expected /login to show One-Time Founder Setup before nta~ password is set")
	}

	claimForm := url.Values{"password": {"OwnerChosenPass!2026"}}
	rClaim := httptest.NewRequest("POST", "/account/claim-owner", strings.NewReader(claimForm.Encode()))
	rClaim.RemoteAddr = "127.0.0.1:12345"
	rClaim.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rClaim.Header.Set("Accept", "application/json")
	wClaim := httptest.NewRecorder()
	h.ServeHTTP(wClaim, rClaim)
	if wClaim.Code != 200 || !strings.Contains(wClaim.Body.String(), `"role":"owner"`) || !strings.Contains(wClaim.Body.String(), `"username":"nta~"`) {
		t.Fatalf("POST /account/claim-owner failed: %d %s", wClaim.Code, wClaim.Body.String())
	}

	// Second attempt to claim owner password must fail with 403 (one-time only!)
	rClaim2 := httptest.NewRequest("POST", "/account/claim-owner", strings.NewReader(claimForm.Encode()))
	rClaim2.RemoteAddr = "127.0.0.1:12345"
	rClaim2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rClaim2.Header.Set("Accept", "application/json")
	wClaim2 := httptest.NewRecorder()
	h.ServeHTTP(wClaim2, rClaim2)
	if wClaim2.Code != http.StatusForbidden {
		t.Fatalf("second POST /account/claim-owner must return 403 Forbidden (one-time setup), got %d", wClaim2.Code)
	}
	wLoginAfter := request(h, "GET", "/login", "")
	if strings.Contains(wLoginAfter.Body.String(), "One-Time Founder Setup") {
		t.Fatalf("/login must not show One-Time Founder Setup after owner password is set")
	}

	sessForm := url.Values{
		"login":    {"nta~"},
		"password": {"OwnerChosenPass!2026"},
	}
	rLogin := httptest.NewRequest("POST", "/session", strings.NewReader(sessForm.Encode()))
	rLogin.RemoteAddr = "127.0.0.1:12345"
	rLogin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wLogin := httptest.NewRecorder()
	h.ServeHTTP(wLogin, rLogin)
	if wLogin.Code != 200 || !strings.Contains(wLogin.Body.String(), `"username":"nta~"`) {
		t.Fatalf("POST /session for nta~ failed: %d %s", wLogin.Code, wLogin.Body.String())
	}

	// 2. syntheticAuth() inside NSM.exe fetches /user-api-key/new and parses hidden inputs, then POSTs /user-api-key
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	nonce := "aabbccddeeff00112233445566778899"
	clientID := "11223344556677889900aabbccddeeff"

	qNew := url.Values{
		"scopes":           {"session_info,read,write"},
		"client_id":        {clientID},
		"nonce":            {nonce},
		"public_key":       {pubPEM},
		"application_name": {"NSM client"},
		"auth_redirect":    {"fivem://accept-auth"},
	}
	wNew := request(h, "GET", "/user-api-key/new?"+qNew.Encode(), "")
	if wNew.Code != 200 {
		t.Fatalf("GET /user-api-key/new failed: %d", wNew.Code)
	}
	for _, field := range []string{`name="authenticity_token"`, `name="application_name"`, `name="nonce"`, `name="client_id"`, `name="public_key"`, `name="scopes"`} {
		if !strings.Contains(wNew.Body.String(), field) {
			t.Fatalf("GET /user-api-key/new missing required DOM input %s", field)
		}
	}

	rKey := httptest.NewRequest("POST", "/user-api-key", strings.NewReader(qNew.Encode()))
	rKey.RemoteAddr = "127.0.0.1:12345"
	rKey.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wKey := httptest.NewRecorder()
	h.ServeHTTP(wKey, rKey)
	if wKey.Code != http.StatusFound {
		t.Fatalf("POST /user-api-key expected 302 Found, got %d (%s)", wKey.Code, wKey.Body.String())
	}
	loc := wKey.Header().Get("Location")
	uLoc, err := url.Parse(loc)
	if err != nil || uLoc.Query().Get("payload") == "" {
		t.Fatalf("POST /user-api-key redirect missing payload: %s", loc)
	}

	cipherBytes, err := base64.StdEncoding.DecodeString(uLoc.Query().Get("payload"))
	if err != nil {
		t.Fatal(err)
	}
	plainBytes, err := rsa.DecryptPKCS1v15(rand.Reader, privKey, cipherBytes)
	if err != nil {
		t.Fatalf("RSA decrypt failed: %v", err)
	}
	var dec struct {
		Key   string `json:"key"`
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(plainBytes, &dec); err != nil || dec.Nonce != nonce || !strings.HasPrefix(dec.Key, "nsm_uapi_") {
		t.Fatalf("unexpected decrypted payload: %s", string(plainBytes))
	}

	// 3. loadCurrentAccount(true) calls /session/current.json and /u/nta~.json (and /f/... variants)
	for _, p := range []string{"/session/current.json", "/f/session/current.json", "/u/nta~.json", "/f/u/nta~.json"} {
		w := requestAuth(h, "GET", p, "", map[string]string{"User-Api-Key": dec.Key, "User-Api-Client-Id": clientID})
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"nta~"`) {
			t.Fatalf("GET %s failed: %d %s", p, w.Code, w.Body.String())
		}
	}

	// 5. Verify NSM & AFX multi-site ecosystem, Private Network Access CORS, and NSM/AFX logos
	wOpt := request(h, "OPTIONS", "/session/current.json", "")
	if wOpt.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("OPTIONS /session/current.json missing Access-Control-Allow-Private-Network: true")
	}

	siteChecks := []struct {
		path   string
		marker string
	}{
		{"/", `DOWNLOAD CLIENT`},
		{"/servers", `id="nsm-mp-app"`},
		{"/login", `<title>Login - AFX Portal</title>`},
		{"/keymaster", `AFX <strong>Portal</strong>`},
		{"/support", `<title>AFX Support</title>`},
		{"/forum", `<title>AFX Forum - The home of NSM</title>`},
		{"/marketplace", `<title>AFX Marketplace</title>`},
		{"/docs", `<title>Home - NSM Documentation</title>`},
	}
	for _, sc := range siteChecks {
		wPage := request(h, "GET", sc.path, "")
		if wPage.Code != 200 {
			t.Fatalf("GET %s failed: %d", sc.path, wPage.Code)
		}
		body := wPage.Body.String()
		if !strings.Contains(body, sc.marker) {
			t.Fatalf("GET %s missing marker %q", sc.path, sc.marker)
		}
		for _, forbidden := range []string{"FiveM", "Cfx.re", "CitizenFX"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("GET %s contains forbidden string %q", sc.path, forbidden)
			}
		}
	}

	for _, imgPath := range []string{"/img/nsm-icon.png", "/img/afx-icon.png", "/img/nsm-afx-logo.png", "/img/hero-bg.jpg"} {
		wImg := request(h, "GET", imgPath, "")
		if wImg.Code != 200 || wImg.Body.Len() < 5000 {
			t.Fatalf("GET %s expected image bytes, got status %d len %d", imgPath, wImg.Code, wImg.Body.Len())
		}
	}

	// 11. Verify 1:1 Discourse Forum, Topic, and /u/nta~ Profile DOM structure & same-tab navigation
	wForum := request(h, "GET", "/forum", "")
	if wForum.Code != http.StatusOK {
		t.Fatalf("GET /forum returned %d", wForum.Code)
	}
	forumBody := wForum.Body.String()
	for _, marker := range []string{
		`class="d-header"`,
		`id="d-sidebar"`,
		`class="categories-and-latest"`,
		`class="category-list"`,
		`class="subcategories"`,
		`class="latest-topic-list"`,
		`class="badge-category-bg"`,
	} {
		if !strings.Contains(forumBody, marker) {
			t.Fatalf("GET /forum missing Discourse marker %q", marker)
		}
	}
	if strings.Contains(forumBody, `target="_blank"`) {
		t.Fatalf("GET /forum still contains target=_blank link")
	}

	wTopic := request(h, "GET", "/forum/t/1", "")
	if wTopic.Code != http.StatusOK {
		t.Fatalf("GET /forum/t/1 returned %d", wTopic.Code)
	}
	topicBody := wTopic.Body.String()
	for _, marker := range []string{
		`id="topic-title"`,
		`class="container posts"`,
		`class="post-stream"`,
		`class="topic-map"`,
		`class="timeline-container"`,
		`id="reply-control"`,
		`id="suggested-topics"`,
	} {
		if !strings.Contains(topicBody, marker) {
			t.Fatalf("GET /forum/t/1 missing Discourse marker %q", marker)
		}
	}

	wProf := request(h, "GET", "/u/nta~", "")
	if wProf.Code != http.StatusOK {
		t.Fatalf("GET /u/nta~ returned %d", wProf.Code)
	}
	profBody := wProf.Body.String()
	for _, marker := range []string{
		`user-full-themed-page fx-effect-aurora_waves`,
		`user-main user-themed-profile`,
		`class="about has-background"`,
		`user-profile-avatar deco-cat_ears`,
		`id="collapsed-info-panel"`,
		`class="user-primary-navigation"`,
		`class="top-section stats-section"`,
		`class="top-section replies-and-topics-section"`,
		`class="top-section top-categories-section"`,
		`class="top-section badges-section"`,
		`Badges (18/18)`,
		`AFX Pulse Eternal (Founder Forever)`,
		`d-social-community`,
		`d-social-github`,
		`github.com/platinum-cfx`,
	} {
		if !strings.Contains(profBody, marker) {
			t.Fatalf("GET /u/nta~ missing Discourse profile marker %q", marker)
		}
	}
	if strings.Contains(profBody, `target="_blank"`) {
		t.Fatalf("GET /u/nta~ still contains target=_blank link")
	}

	// 12. Verify every /u/nta~ profile tab renders its own distinct Discourse view (not Summary on every tab)
	tabExpectations := []struct {
		url         string
		mustContain string
		mustNotHave string
	}{
		{"/u/nta~?tab=activity", `All Activity by`, `class="top-section stats-section"`},
		{"/u/nta~/activity", `class="user-navigation user-secondary-navigation"`, `class="top-section stats-section"`},
		{"/u/nta~?tab=topics", `Topics Created by`, `class="top-section stats-section"`},
		{"/u/nta~?tab=replies", `Replies by`, `class="top-section stats-section"`},
		{"/u/nta~?tab=read", `Recently Read Topics`, `class="top-section stats-section"`},
		{"/u/nta~?tab=likes", `Liked Posts &amp; Appreciated Contributions`, `class="top-section stats-section"`},
		{"/u/nta~?tab=solved", `Accepted Solutions`, `class="top-section stats-section"`},
		{"/u/nta~?tab=votes", `NSM Server Upvotes &amp; Topic Votes`, `class="top-section stats-section"`},
		{"/u/nta~?tab=notifications", `Notifications for`, `class="top-section stats-section"`},
		{"/u/nta~?tab=messages", `Direct Chats`, `class="top-section stats-section"`},
		{"/u/nta~?tab=badges", `Badges Earned by`, `class="top-section stats-section"`},
		{"/u/nta~?tab=pulse", `AFX Pulse Subscriptions Are Currently Closed`, `class="top-section stats-section"`},
	}
	for _, tc := range tabExpectations {
		wTab := request(h, "GET", tc.url, "")
		if wTab.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d", tc.url, wTab.Code)
		}
		tb := wTab.Body.String()
		if !strings.Contains(tb, tc.mustContain) {
			t.Fatalf("GET %s missing expected tab marker %q", tc.url, tc.mustContain)
		}
		if tc.mustNotHave != "" && strings.Contains(tb, tc.mustNotHave) {
			t.Fatalf("GET %s unexpectedly rendered Summary marker %q", tc.url, tc.mustNotHave)
		}
	}

	// 13. Verify AFX Pulse is closed right now so users cannot purchase it yet
	customSess := registerTestUser(t, h, "theme_tester")

	reqPulseClosed := httptest.NewRequest("POST", "/account/pulse", strings.NewReader("tier=sovereign"))
	reqPulseClosed.RemoteAddr = "127.0.0.1:12345"
	reqPulseClosed.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqPulseClosed.Header.Set("X-NSM-Session", customSess)
	wPulseClosed := httptest.NewRecorder()
	h.ServeHTTP(wPulseClosed, reqPulseClosed)
	if !strings.Contains(wPulseClosed.Body.String(), "AFX Pulse is currently closed") {
		t.Fatalf("expected POST /account/pulse to state AFX Pulse is closed right now")
	}

	// Verify basic user (without AFX Pulse) CAN set both profile picture and profile banner
	formPayload := "display_name=Captain+Neon+AFX&title=Lead+Architect&avatar_url=%2Fimg%2Fafx-icon.png&banner_url=%2Fimg%2Fhero-bg.jpg&theme_primary=%23EC4899&theme_accent=%238B5CF6&banner_color=%233B0764&avatar_color=%23F472B6&avatar_decoration=cat_ears&profile_effect=cyber_matrix&squad_house=brilliance&return_tab=summary"
	reqUpdBasic := httptest.NewRequest("POST", "/account/profile", strings.NewReader(formPayload))
	reqUpdBasic.RemoteAddr = "127.0.0.1:12345"
	reqUpdBasic.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqUpdBasic.Header.Set("X-NSM-Session", customSess)
	wUpdBasic := httptest.NewRecorder()
	h.ServeHTTP(wUpdBasic, reqUpdBasic)
	if strings.Contains(wUpdBasic.Body.String(), "--prof-primary: #EC4899") {
		t.Fatalf("basic user without AFX Pulse Pro should not be able to change profile layout colors")
	}
	accMu.Lock()
	basicTester := findUser("theme_tester")
	basicAvatar := basicTester.AvatarPath()
	basicBanner := basicTester.BannerPath()
	accMu.Unlock()
	if basicAvatar != "/img/afx-icon.png" || basicBanner != "/img/hero-bg.jpg" {
		t.Fatalf("basic user should be able to set both avatar and banner: got avatar=%q banner=%q", basicAvatar, basicBanner)
	}

	// Grant Sovereign tier directly in store (as Founder/pre-existing subscriber) and verify profile customization & username rename
	accMu.Lock()
	if u := findUser("theme_tester"); u != nil {
		u.PulseTier = "sovereign"
	}
	accMu.Unlock()

	renamePayload := formPayload + "&new_username=neon_captain"
	reqUpd := httptest.NewRequest("POST", "/account/profile", strings.NewReader(renamePayload))
	reqUpd.RemoteAddr = "127.0.0.1:12345"
	reqUpd.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqUpd.Header.Set("X-NSM-Session", customSess)
	wUpd := httptest.NewRecorder()
	h.ServeHTTP(wUpd, reqUpd)
	if wUpd.Code != http.StatusOK {
		t.Fatalf("POST /account/profile expected 200 OK, got %d", wUpd.Code)
	}

	// 14. Verify Self-Views do NOT increment ProfileViews, while Another User viewing DOES increment once
	accMu.Lock()
	viewsBeforeSelf := findUser("neon_captain").ProfileViews
	accMu.Unlock()

	_ = requestAuth(h, "GET", "/u/neon_captain?tab=activity", "", map[string]string{"X-NSM-Session": customSess})
	_ = requestAuth(h, "GET", "/u/neon_captain?tab=summary", "", map[string]string{"X-NSM-Session": customSess})

	accMu.Lock()
	viewsAfterSelf := findUser("neon_captain").ProfileViews
	accMu.Unlock()
	if viewsAfterSelf != viewsBeforeSelf {
		t.Fatalf("clicking own profile tabs must not increment ProfileViews: before=%d after=%d", viewsBeforeSelf, viewsAfterSelf)
	}

	// 15. Verify Two-Way Friend Request & Acceptance Requirement before Direct Messaging:
	peerSess := registerTestUser(t, h, "friend_peer")

	// Step A: Attempting to DM friend_peer before sending a friend request must be blocked
	dmPayload := "recipient=friend_peer&subject=Server+Collab&body=Hey+peer+lets+test+1on1+chat&return_profile=neon_captain"
	reqDMBlocked := httptest.NewRequest("POST", "/account/messages", strings.NewReader(dmPayload))
	reqDMBlocked.RemoteAddr = "127.0.0.1:12345"
	reqDMBlocked.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqDMBlocked.Header.Set("X-NSM-Session", customSess)
	wDMBlocked := httptest.NewRecorder()
	h.ServeHTTP(wDMBlocked, reqDMBlocked)
	if !strings.Contains(wDMBlocked.Body.String(), "You must send a friend request to @friend_peer") {
		t.Fatalf("expected DM before sending friend request to be blocked, got status %d", wDMBlocked.Code)
	}

	// Step B: neon_captain sends a friend request to friend_peer -> still cannot message until friend_peer accepts!
	reqAddFriend := httptest.NewRequest("POST", "/account/friends", strings.NewReader("target=friend_peer&action=add&return_to=%2Fu%2Fneon_captain%3Ftab%3Dmessages%26with%3Dfriend_peer"))
	reqAddFriend.RemoteAddr = "127.0.0.1:12345"
	reqAddFriend.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqAddFriend.Header.Set("X-NSM-Session", customSess)
	wAddFriend := httptest.NewRecorder()
	h.ServeHTTP(wAddFriend, reqAddFriend)
	if wAddFriend.Code != http.StatusSeeOther {
		t.Fatalf("POST /account/friends (send request) failed: %d", wAddFriend.Code)
	}

	reqDMPending := httptest.NewRequest("POST", "/account/messages", strings.NewReader(dmPayload))
	reqDMPending.RemoteAddr = "127.0.0.1:12345"
	reqDMPending.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqDMPending.Header.Set("X-NSM-Session", customSess)
	wDMPending := httptest.NewRecorder()
	h.ServeHTTP(wDMPending, reqDMPending)
	if !strings.Contains(wDMPending.Body.String(), "Friend request pending: @friend_peer must accept your friend request before you can message each other.") {
		t.Fatalf("expected DM while friend request is pending to be blocked until recipient accepts")
	}

	// Step C: friend_peer logs in on their account and accepts neon_captain's friend request
	reqAcceptFriend := httptest.NewRequest("POST", "/account/friends", strings.NewReader("target=neon_captain&action=accept"))
	reqAcceptFriend.RemoteAddr = "127.0.0.1:12345"
	reqAcceptFriend.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqAcceptFriend.Header.Set("X-NSM-Session", peerSess)
	wAcceptFriend := httptest.NewRecorder()
	h.ServeHTTP(wAcceptFriend, reqAcceptFriend)
	if wAcceptFriend.Code != http.StatusSeeOther {
		t.Fatalf("POST /account/friends (accept request) failed: %d", wAcceptFriend.Code)
	}

	// Step D: Now neon_captain can send direct messages to friend_peer!
	reqDM := httptest.NewRequest("POST", "/account/messages", strings.NewReader(dmPayload))
	reqDM.RemoteAddr = "127.0.0.1:12345"
	reqDM.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqDM.Header.Set("X-NSM-Session", customSess)
	wDM := httptest.NewRecorder()
	h.ServeHTTP(wDM, reqDM)
	if wDM.Code != http.StatusSeeOther || !strings.Contains(wDM.Header().Get("Location"), "/u/neon_captain?tab=messages&with=friend_peer") {
		t.Fatalf("POST /account/messages after friend acceptance expected redirect to 1-on-1 chat, got %d (%s)", wDM.Code, wDM.Header().Get("Location"))
	}
	wChat := requestAuth(h, "GET", "/u/neon_captain?tab=messages&with=friend_peer", "", map[string]string{"X-NSM-Session": customSess})
	if !strings.Contains(wChat.Body.String(), "Hey peer lets test 1on1 chat") {
		t.Fatalf("1-on-1 chat thread did not render sent direct message")
	}

	// 16. Verify FiveM/Zendesk-Style Support Request Creation, Strict Per-User Privacy, and Staff Access
	supForm := url.Values{
		"email":       {"neon@nsm.local"},
		"category":    {"Technical / Client Issue"},
		"subject":     {"NSM.exe Audio Device Question"},
		"server_info": {"127.0.0.1:30120"},
		"description": {"Testing the dedicated support request page and staff replies."},
	}
	reqSup := httptest.NewRequest("POST", "/support/requests", strings.NewReader(supForm.Encode()))
	reqSup.RemoteAddr = "127.0.0.1:12345"
	reqSup.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqSup.Header.Set("X-NSM-Session", customSess)
	wSup := httptest.NewRecorder()
	h.ServeHTTP(wSup, reqSup)
	if wSup.Code != http.StatusSeeOther || !strings.HasPrefix(wSup.Header().Get("Location"), "/support/requests/") {
		t.Fatalf("POST /support/requests expected 303 redirect to /support/requests/{id}, got %d (%s)", wSup.Code, wSup.Header().Get("Location"))
	}
	ticketURL := wSup.Header().Get("Location")
	wTicket := requestAuth(h, "GET", ticketURL, "", map[string]string{"X-NSM-Session": customSess})
	if wTicket.Code != http.StatusOK || !strings.Contains(wTicket.Body.String(), "neon@nsm.local") || !strings.Contains(wTicket.Body.String(), "NSM.exe Audio Device Question") {
		t.Fatalf("GET %s did not display requester email and subject: %d", ticketURL, wTicket.Code)
	}

	// Another regular user (friend_peer) MUST NOT be able to view neon_captain's support request or see it on /support
	wTicketOther := requestAuth(h, "GET", ticketURL, "", map[string]string{"X-NSM-Session": peerSess})
	if wTicketOther.Code != http.StatusForbidden {
		t.Fatalf("other regular user must receive 403 Forbidden when attempting to view another user's support request, got %d", wTicketOther.Code)
	}
	wSupportOther := requestAuth(h, "GET", "/support", "", map[string]string{"X-NSM-Session": peerSess})
	if strings.Contains(wSupportOther.Body.String(), "NSM.exe Audio Device Question") {
		t.Fatalf("other regular user must not see neon_captain's support request on /support")
	}

	// 17. Verify Staff user can reply to support ticket, grant Partner role, and ban/unban a user
	staffSess := registerTestUser(t, h, "mod_staff")
	accMu.Lock()
	staffUser := findUser("mod_staff")
	targetUser := findUser("neon_captain")
	if staffUser == nil || targetUser == nil {
		accMu.Unlock()
		t.Fatalf("expected mod_staff and neon_captain users to exist")
	}
	staffUser.Role = "staff"
	targetID := targetUser.ID
	accMu.Unlock()

	reqStaffReply := httptest.NewRequest("POST", ticketURL+"/reply", strings.NewReader("body=Hello+from+AFX+Support+Staff!+We+have+resolved+your+question.&status=solved"))
	reqStaffReply.RemoteAddr = "127.0.0.1:12345"
	reqStaffReply.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqStaffReply.Header.Set("X-NSM-Session", staffSess)
	wStaffReply := httptest.NewRecorder()
	h.ServeHTTP(wStaffReply, reqStaffReply)
	if wStaffReply.Code != http.StatusSeeOther {
		t.Fatalf("staff reply to support ticket failed: %d", wStaffReply.Code)
	}
	wTicketAfter := requestAuth(h, "GET", ticketURL, "", map[string]string{"X-NSM-Session": customSess})
	if !strings.Contains(wTicketAfter.Body.String(), "Hello from AFX Support Staff!") || !strings.Contains(wTicketAfter.Body.String(), "AFX Support Staff") {
		t.Fatalf("support ticket page did not render staff reply and badge")
	}

	// Staff views neon_captain profile -> increments ProfileViews by 1
	_ = requestAuth(h, "GET", "/u/neon_captain", "", map[string]string{"X-NSM-Session": staffSess})
	accMu.Lock()
	viewsAfterOther := findUser("neon_captain").ProfileViews
	accMu.Unlock()
	if viewsAfterOther != viewsBeforeSelf+1 {
		t.Fatalf("other user viewing profile should increment ProfileViews by 1: got %d want %d", viewsAfterOther, viewsBeforeSelf+1)
	}

	// Staff bans and unbans user
	reqBan := httptest.NewRequest("POST", fmt.Sprintf("/admin/users/%d/ban", targetID), strings.NewReader("action=ban&reason=Rule+violation+test"))
	reqBan.RemoteAddr = "127.0.0.1:12345"
	reqBan.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqBan.Header.Set("X-NSM-Session", staffSess)
	wBan := httptest.NewRecorder()
	h.ServeHTTP(wBan, reqBan)
	if wBan.Code != http.StatusSeeOther || !strings.Contains(wBan.Header().Get("Location"), "Banned") {
		t.Fatalf("POST /admin/users/%d/ban failed: %d (%s)", targetID, wBan.Code, wBan.Header().Get("Location"))
	}
	reqUnban := httptest.NewRequest("POST", fmt.Sprintf("/admin/users/%d/ban", targetID), strings.NewReader("action=unban"))
	reqUnban.RemoteAddr = "127.0.0.1:12345"
	reqUnban.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqUnban.Header.Set("X-NSM-Session", staffSess)
	wUnban := httptest.NewRecorder()
	h.ServeHTTP(wUnban, reqUnban)
	if wUnban.Code != http.StatusSeeOther || !strings.Contains(wUnban.Header().Get("Location"), "Unbanned") {
		t.Fatalf("POST /admin/users/%d/ban unban failed: %d (%s)", targetID, wUnban.Code, wUnban.Header().Get("Location"))
	}

	reqPartner := httptest.NewRequest("POST", fmt.Sprintf("/admin/users/%d/partner", targetID), strings.NewReader("action=grant"))
	reqPartner.RemoteAddr = "127.0.0.1:12345"
	reqPartner.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqPartner.Header.Set("X-NSM-Session", staffSess)
	wPartner := httptest.NewRecorder()
	h.ServeHTTP(wPartner, reqPartner)
	if wPartner.Code != http.StatusOK {
		t.Fatalf("POST /admin/users/%d/partner by staff expected 200 OK, got %d: %s", targetID, wPartner.Code, wPartner.Body.String())
	}
	if !strings.Contains(wPartner.Body.String(), "Partnered Server Owner") {
		t.Fatalf("Partner role grant did not unlock Partnered Server Owner badge")
	}

	wRealtime := request(h, "GET", "/api/forum/realtime?profile=nta~", "")
	if wRealtime.Code != http.StatusOK || !strings.Contains(wRealtime.Body.String(), `"profileViews"`) {
		t.Fatalf("GET /api/forum/realtime failed: %d %s", wRealtime.Code, wRealtime.Body.String())
	}

	wUsersDir := request(h, "GET", "/u", "")
	if wUsersDir.Code != http.StatusOK || !strings.Contains(wUsersDir.Body.String(), "d-users-directory-table") {
		t.Fatalf("GET /u users directory failed: %d", wUsersDir.Code)
	}

	ln, err := listenWithReuseAddr("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenWithReuseAddr failed: %v", err)
	}
	_ = ln.Close()
}

func TestUpvotesFeaturedAndJoinLinks(t *testing.T) {
	h := setupTest(t)

	// 1. Register a live server via heartbeat
	wHB := request(h, "POST", "/api/serverlist/ingress", validHeartbeat)
	if wHB.Code != 200 {
		t.Fatalf("heartbeat failed: %d %s", wHB.Code, wHB.Body.String())
	}

	// 2. Verify nta~ has 1,000,000 upvote allowance & profile displays 1000000
	accMu.Lock()
	owner := findUser("nta~")
	if owner == nil {
		accMu.Unlock()
		t.Fatal("nta~ user missing")
	}
	if got := owner.UpvoteAllowance(); got != 1000000 {
		accMu.Unlock()
		t.Fatalf("expected nta~ UpvoteAllowance 1000000, got %d", got)
	}
	ownerSess := "test-owner-sess-upv"
	acc.Sessions[ownerSess] = Session{UserID: owner.ID, Expires: time.Now().Add(time.Hour).Unix()}
	// Create a non-owner staff user to test owner-only Featured Server guard
	acc.NextID++
	staffID := acc.NextID
	acc.Users = append(acc.Users, User{
		ID:       staffID,
		Username: "moduser",
		Email:    "mod@nsm.local",
		Role:     "admin",
	})
	staffSess := "test-staff-sess-upv"
	acc.Sessions[staffSess] = Session{UserID: staffID, Expires: time.Now().Add(time.Hour).Unix()}
	accMu.Unlock()

	wProfile := request(h, "GET", "/u/nta~", "")
	if wProfile.Code != 200 || !strings.Contains(wProfile.Body.String(), "1000000") {
		t.Fatalf("expected /u/nta~ to display 1000000 upvotes, got code %d", wProfile.Code)
	}

	// 3. Upvote server as nta~ via POST /api/servers/upvote
	reqUpv := httptest.NewRequest("POST", "/api/servers/upvote", strings.NewReader(`{"endpoint":"127.0.0.1:30120"}`))
	reqUpv.RemoteAddr = "127.0.0.1:12345"
	reqUpv.Header.Set("Accept", "application/json")
	reqUpv.Header.Set("Content-Type", "application/json")
	reqUpv.Header.Set("X-NSM-Session", ownerSess)
	wUpv := httptest.NewRecorder()
	h.ServeHTTP(wUpv, reqUpv)
	if wUpv.Code != 200 || !strings.Contains(wUpv.Body.String(), `"upvotes":1000000`) {
		t.Fatalf("POST /api/servers/upvote expected 1000000 upvotes, got %d: %s", wUpv.Code, wUpv.Body.String())
	}

	// 4. Verify /api/servers/single/127.0.0.1:30120 and /api/servers/live reflect 1,000,000 upvotes
	wSingle := request(h, "GET", "/api/servers/single/127.0.0.1:30120", "")
	if wSingle.Code != 200 || !strings.Contains(wSingle.Body.String(), `"upvotePower":1000000`) {
		t.Fatalf("GET /api/servers/single expected upvotePower 1000000, got %d: %s", wSingle.Code, wSingle.Body.String())
	}
	wLive := request(h, "GET", "/api/servers/live", "")
	if wLive.Code != 200 || !strings.Contains(wLive.Body.String(), `"upvotePower":1000000`) {
		t.Fatalf("GET /api/servers/live expected upvotePower 1000000, got %d: %s", wLive.Code, wLive.Body.String())
	}

	// 5. Verify non-owner cannot feature servers (even an admin)
	reqFeatDeny := httptest.NewRequest("POST", "/api/servers/feature", strings.NewReader(`{"endpoint":"127.0.0.1:30120"}`))
	reqFeatDeny.RemoteAddr = "127.0.0.1:12345"
	reqFeatDeny.Header.Set("Accept", "application/json")
	reqFeatDeny.Header.Set("Content-Type", "application/json")
	reqFeatDeny.Header.Set("X-NSM-Session", staffSess)
	wFeatDeny := httptest.NewRecorder()
	h.ServeHTTP(wFeatDeny, reqFeatDeny)
	if wFeatDeny.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-nta~ feature request, got %d: %s", wFeatDeny.Code, wFeatDeny.Body.String())
	}

	// 6. Verify nta~ can feature servers and /v1/public/featured-servers returns it
	reqFeat := httptest.NewRequest("POST", "/api/servers/feature", strings.NewReader(`{"endpoint":"127.0.0.1:30120"}`))
	reqFeat.RemoteAddr = "127.0.0.1:12345"
	reqFeat.Header.Set("Accept", "application/json")
	reqFeat.Header.Set("Content-Type", "application/json")
	reqFeat.Header.Set("X-NSM-Session", ownerSess)
	wFeat := httptest.NewRecorder()
	h.ServeHTTP(wFeat, reqFeat)
	if wFeat.Code != 200 || !strings.Contains(wFeat.Body.String(), `"featured":true`) {
		t.Fatalf("POST /api/servers/feature by nta~ failed: %d %s", wFeat.Code, wFeat.Body.String())
	}

	wFeaturedPub := request(h, "GET", "/v1/public/featured-servers", "")
	if wFeaturedPub.Code != 200 || !strings.Contains(wFeaturedPub.Body.String(), `"127.0.0.1:30120"`) {
		t.Fatalf("GET /v1/public/featured-servers missing featured server: %d %s", wFeaturedPub.Code, wFeaturedPub.Body.String())
	}

	// 7. Verify GET /join/{ep} sets X-CitizenFX-Url and renders the code-based
	//    share URL — raw endpoints must never appear in rendered join/console
	//    displays (join codes hide server IPs).
	wJoin := request(h, "GET", "/join/127.0.0.1:30120", "")
	joinBody := wJoin.Body.String()
	if wJoin.Code != 200 || wJoin.Header().Get("X-CitizenFX-Url") != "http://127.0.0.1:30120/" || !strings.Contains(joinBody, "https://nsmultiplayer.com/join/") {
		t.Fatalf("GET /join/127.0.0.1:30120 failed: code=%d hdr=%q", wJoin.Code, wJoin.Header().Get("X-CitizenFX-Url"))
	}
	if strings.Contains(joinBody, "join/127.0.0.1:30120") || strings.Contains(joinBody, "connect 127.0.0.1:30120") {
		t.Fatal("GET /join/{ep} leaked the raw endpoint into rendered join/console displays")
	}

	// 8. Verify default FXServer / empty hostname becomes AFXserver
	hbDefault := `{"port":30125,"listingToken":"","ipOverride":"","private":true,"fallbackData":{"players":[],"info":{"info":{"resources":["chat"],"version":"test-fixture","vars":{}}},"dynamic":{"dynamic":{"clients":0,"svMaxclients":48,"hostname":"default FXServer"}}}}`
	if wDef := request(h, "POST", "/api/serverlist/ingress", hbDefault); wDef.Code != 200 {
		t.Fatalf("default FXServer heartbeat failed: %d %s", wDef.Code, wDef.Body.String())
	}
	wSingleDef := request(h, "GET", "/api/servers/single/127.0.0.1:30125", "")
	if wSingleDef.Code != 200 || !strings.Contains(wSingleDef.Body.String(), `"hostname":"AFXserver"`) {
		t.Fatalf("expected default FXServer to become AFXserver, got %d: %s", wSingleDef.Code, wSingleDef.Body.String())
	}
}







// ---------------------------------------------------------------------------
// Keymaster key privacy, upvote allocation, support rules, email & Google auth
// ---------------------------------------------------------------------------

func sessionFor(t *testing.T, h http.Handler, username string) string {
	t.Helper()
	return registerTestUser(t, h, username)
}

func makeStaffSession(t *testing.T, username, role string) string {
	t.Helper()
	accMu.Lock()
	defer accMu.Unlock()
	var id int64
	for i := range acc.Users {
		if strings.EqualFold(acc.Users[i].Username, username) {
			acc.Users[i].Role = role
			id = acc.Users[i].ID
		}
	}
	if id == 0 {
		t.Fatalf("user %s not found", username)
	}
	sess := "test-sess-" + username + "-" + role
	acc.Sessions[sess] = Session{UserID: id, Expires: time.Now().Add(time.Hour).Unix()}
	return sess
}

func TestKeymasterKeysArePrivateToOwner(t *testing.T) {
	h := setupTest(t)
	sessA := sessionFor(t, h, "key_owner_a")
	sessB := sessionFor(t, h, "key_snooper_b")

	// A issues a key with a recognizable label.
	form := url.Values{"label": {"A-SECRET-KEY-LABEL"}, "boundEndpoint": {"127.0.0.1:30120"}, "days": {"90"}}
	req := httptest.NewRequest("POST", "/keymaster/create", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-NSM-Session", sessA)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "A-SECRET-KEY-LABEL") {
		t.Fatalf("owner could not issue key: %d", w.Code)
	}

	// B must not see A's key on the page or in the JSON API.
	wB := requestAuth(h, "GET", "/keymaster", "", map[string]string{"X-NSM-Session": sessB})
	if strings.Contains(wB.Body.String(), "A-SECRET-KEY-LABEL") {
		t.Fatal("another user's server key is visible on /keymaster")
	}
	wBJ := requestAuth(h, "GET", "/api/keymaster/keys", "", map[string]string{"X-NSM-Session": sessB})
	if strings.Contains(wBJ.Body.String(), "A-SECRET-KEY-LABEL") {
		t.Fatal("another user's server key is visible in /api/keymaster/keys")
	}

	// Even an admin must not see other people's keys in Keymaster.
	makeStaffSession(t, "key_snooper_b", "admin")
	wAdm := requestAuth(h, "GET", "/keymaster", "", map[string]string{"X-NSM-Session": sessB})
	if strings.Contains(wAdm.Body.String(), "A-SECRET-KEY-LABEL") {
		t.Fatal("admin sees another user's keys in Keymaster — keys must be owner-only")
	}

	// The owner still sees their own.
	wA := requestAuth(h, "GET", "/keymaster", "", map[string]string{"X-NSM-Session": sessA})
	if !strings.Contains(wA.Body.String(), "A-SECRET-KEY-LABEL") {
		t.Fatal("owner cannot see their own key on /keymaster")
	}
}

func TestRegisterRequiresEmailAndReportsMailTruthfully(t *testing.T) {
	h := setupTest(t)

	// 1. No email -> rejected, no account created.
	wNoMail := requestAuth(h, "POST", "/api/auth/register",
		`{"Username":"no_email_user","Password":"Password123!"}`, nil)
	if wNoMail.Code != 200 || !strings.Contains(wNoMail.Body.String(), "email") {
		t.Fatalf("registration without email must fail with an email message: %d %s", wNoMail.Code, wNoMail.Body.String())
	}
	accMu.Lock()
	created := findUser("no_email_user") != nil
	accMu.Unlock()
	if created {
		t.Fatal("account was created without an email address")
	}

	// 2. Fake local emails are rejected too.
	wFake := requestAuth(h, "POST", "/api/auth/register",
		`{"Username":"fake_email_user","Password":"Password123!","Email":"fake_email_user@users.nsm.local"}`, nil)
	if wFake.Code != 200 && wFake.Code != 400 {
		t.Fatalf("unexpected status for fake email: %d", wFake.Code)
	}
	accMu.Lock()
	created = findUser("fake_email_user") != nil
	accMu.Unlock()
	if created {
		t.Fatal("account was created with a @users.nsm.local placeholder email")
	}

	// 3. With SMTP configured, the real verification code is delivered.
	msgs := startSMTPCapture(t)
	host, port, _ := net.SplitHostPort(smtpCaptureAddr(t))
	t.Setenv("NSM_SMTP_HOST", host)
	t.Setenv("NSM_SMTP_PORT", port)
	t.Setenv("NSM_SMTP_USER", "support@nsmultiplayer.com")
	t.Setenv("NSM_SMTP_PASS", "test-app-password")
	wOK := requestAuth(h, "POST", "/api/auth/register",
		`{"Username":"real_email_user","Password":"Password123!","Email":"real_email_user@example.com"}`, nil)
	if wOK.Code != 200 {
		t.Fatalf("registration with valid email failed: %d %s", wOK.Code, wOK.Body.String())
	}
	accMu.Lock()
	u := findUser("real_email_user")
	code := ""
	if u != nil {
		code = u.EmailVerifyCode
	}
	accMu.Unlock()
	if code == "" {
		t.Fatal("no verification code generated")
	}
	if msgs.count() != 1 || !strings.Contains(msgs.body(0), code) || !strings.Contains(msgs.body(0), "real_email_user@example.com") {
		t.Fatalf("verification email was not delivered to the capture SMTP server: %v", msgs.all())
	}
	if !strings.Contains(wOK.Body.String(), "verificationRequired") {
		t.Fatalf("register JSON must report verificationRequired: %s", wOK.Body.String())
	}

	// 4. Without SMTP configured the page must say the code was NOT emailed.
	t.Setenv("NSM_SMTP_HOST", "")
	reqNoSMTP := httptest.NewRequest("POST", "/register", strings.NewReader("username=no_smtp_user&password=Password123!&email=no_smtp_user@example.com"))
	reqNoSMTP.RemoteAddr = "127.0.0.1:12345"
	reqNoSMTP.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wNoSMTP := httptest.NewRecorder()
	h.ServeHTTP(wNoSMTP, reqNoSMTP)
	if wNoSMTP.Code != 303 {
		t.Fatalf("register form: %d %s", wNoSMTP.Code, wNoSMTP.Body.String())
	}
	loc := wNoSMTP.Header().Get("Location")
	locDec, _ := url.QueryUnescape(loc)
	if !strings.HasPrefix(loc, "/verify-email") || !strings.Contains(locDec, "could not be emailed") {
		t.Fatalf("unconfigured SMTP must be reported honestly, got Location=%s", loc)
	}
}

func TestSupportTicketsRequireLoginAndClosedTicketsGoToLog(t *testing.T) {
	h := setupTest(t)
	postForm := func(sess, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if sess != "" {
			req.Header.Set("X-NSM-Session", sess)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// 1. Guests cannot open tickets.
	supForm := url.Values{"subject": {"Guest attempt"}, "description": {"Should be rejected."}}
	req := httptest.NewRequest("POST", "/support/requests", strings.NewReader(supForm.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wGuest := httptest.NewRecorder()
	h.ServeHTTP(wGuest, req)
	if wGuest.Code != 303 || !strings.HasPrefix(wGuest.Header().Get("Location"), "/login") {
		t.Fatalf("guest ticket creation must redirect to login: %d %s", wGuest.Code, wGuest.Header().Get("Location"))
	}

	// 2. A signed-in user opens a ticket.
	sess := sessionFor(t, h, "ticket_user")
	tf := url.Values{
		"email":       {"ticket_user@example.com"},
		"category":    {"Bug Report"},
		"subject":     {"SECRET-TICKET-SUBJECT-XYZ"},
		"description": {"TICKET-BODY-PRIVATE-DETAILS"},
	}
	wT := postForm(sess, "/support/requests", tf.Encode())
	if wT.Code != 303 || !strings.HasPrefix(wT.Header().Get("Location"), "/support/requests/") {
		t.Fatalf("signed-in ticket creation failed: %d %s", wT.Code, wT.Header().Get("Location"))
	}
	ticketURL := wT.Header().Get("Location")
	ticketID := strings.TrimPrefix(ticketURL, "/support/requests/")

	// 3. Staff closes it -> it leaves the working lists and lands in the log.
	staffSess := makeStaffSession(t, "ticket_user", "staff") // staff view of the queue
	wClose := postForm(staffSess, "/support/requests/"+ticketID+"/status", "status=Closed")
	if wClose.Code != 303 {
		t.Fatalf("close failed: %d %s", wClose.Code, wClose.Body.String())
	}
	wSup := requestAuth(h, "GET", "/support", "", map[string]string{"X-NSM-Session": sess})
	html := wSup.Body.String()
	logAt := strings.Index(html, `id="closed-log"`)
	if logAt < 0 {
		t.Fatal("closed ticket log section missing")
	}
	if strings.Contains(html[:logAt], "SECRET-TICKET-SUBJECT-XYZ") {
		t.Fatal("closed ticket still visible in the active staff/support lists")
	}
	rest := html[logAt:]
	if !strings.Contains(rest, "SECRET-TICKET-SUBJECT-XYZ") || !strings.Contains(rest, "TICKET-BODY-PRIVATE-DETAILS") || !strings.Contains(rest, "ticket_user") {
		t.Fatal("closed ticket log must record what the ticket was about")
	}
}

func TestProfileNotificationsAndMessagesArePrivate(t *testing.T) {
	h := setupTest(t)
	sessA := sessionFor(t, h, "private_alice")
	sessB := sessionFor(t, h, "private_bob")

	accMu.Lock()
	alice := findUser("private_alice")
	if alice == nil {
		accMu.Unlock()
		t.Fatal("alice missing")
	}
	aliceID := alice.ID
	addUserNotificationLocked(alice, "message", "SECRET-NOTIF-TITLE-ALICE", "SECRET-NOTIF-BODY-ALICE", "/account")
	acc.DirectMessages = append(acc.DirectMessages, DirectMessage{
		ID: 9001, FromUserID: aliceID, FromUser: "private_alice", ToUserID: aliceID, ToUser: "private_alice",
		Body: "SECRET-DM-BODY-ALICE", CreatedAt: time.Now().Unix(),
	})
	accMu.Unlock()

	// Owner sees their own inbox/notification feed.
	wOwn := requestAuth(h, "GET", "/u/private_alice?tab=notifications", "", map[string]string{"X-NSM-Session": sessA})
	if !strings.Contains(wOwn.Body.String(), "SECRET-NOTIF-BODY-ALICE") {
		t.Fatal("owner should see their own notifications")
	}

	// Another signed-in user must not (any tab).
	wB := requestAuth(h, "GET", "/u/private_alice?tab=notifications", "", map[string]string{"X-NSM-Session": sessB})
	bodyB := wB.Body.String()
	if strings.Contains(bodyB, "SECRET-NOTIF") || strings.Contains(bodyB, "SECRET-DM-BODY-ALICE") {
		t.Fatal("another user can see this profile's private notifications/messages")
	}

	// Guests must not either.
	wG := request(h, "GET", "/u/private_alice?tab=notifications", "")
	if strings.Contains(wG.Body.String(), "SECRET-NOTIF") || strings.Contains(wG.Body.String(), "SECRET-DM-BODY-ALICE") {
		t.Fatal("guest can see private notifications/messages")
	}

	// The messages tab of someone else's profile shows the VIEWER's inbox only.
	wB2 := requestAuth(h, "GET", "/u/private_alice?tab=messages", "", map[string]string{"X-NSM-Session": sessB})
	if strings.Contains(wB2.Body.String(), "SECRET-DM-BODY-ALICE") {
		t.Fatal("messages tab leaks the profile owner's DMs")
	}
}

func TestKeymasterUpvoteAllocationAndFreeTen(t *testing.T) {
	h := setupTest(t)
	sess := sessionFor(t, h, "upvote_owner")

	// Every account starts with 10 free upvotes.
	w := requestAuth(h, "GET", "/keymaster", "", map[string]string{"X-NSM-Session": sess})
	if !strings.Contains(w.Body.String(), "10 free upvotes") || !strings.Contains(w.Body.String(), "2048") {
		t.Fatal("keymaster must advertise the 10 free upvotes and free 2048 player slots")
	}

	// Issue a bound key so the allocation knows the owner's server endpoint.
	form := url.Values{"label": {"Upvote Server"}, "boundEndpoint": {"127.0.0.1:30120"}, "days": {"90"}}
	reqK := httptest.NewRequest("POST", "/keymaster/create", strings.NewReader(form.Encode()))
	reqK.RemoteAddr = "127.0.0.1:12345"
	reqK.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqK.Header.Set("X-NSM-Session", sess)
	wK := httptest.NewRecorder()
	h.ServeHTTP(wK, reqK)
	if wK.Code != 200 {
		t.Fatalf("key issue failed: %d", wK.Code)
	}

	// Slider: 7 upvotes on the server.
	postForm := func(sess, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if sess != "" {
			req.Header.Set("X-NSM-Session", sess)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := postForm(sess, "/keymaster/upvotes", "upvotes=7"); w.Code != 200 {
		t.Fatalf("upvote allocation failed: %d", w.Code)
	}
	if w := request(h, "POST", "/api/serverlist/ingress", validHeartbeat); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	checkPower := func(want int64) {
		t.Helper()
		w1 := request(h, "GET", "/api/servers/single/127.0.0.1:30120", "")
		var server struct {
			Data ServerData
		}
		if json.Unmarshal(w1.Body.Bytes(), &server) != nil {
			t.Fatal(w1.Body.String())
		}
		if server.Data.UpvotePower != want {
			t.Fatalf("UpvotePower = %d, want %d", server.Data.UpvotePower, want)
		}
	}
	checkPower(7)

	// Clamping: 99 -> 10, -5 -> 0.
	postForm(sess, "/keymaster/upvotes", "upvotes=99")
	checkPower(10)
	postForm(sess, "/keymaster/upvotes", "upvotes=-5")
	checkPower(0)

	// Guests are bounced to login.
	reqG := httptest.NewRequest("POST", "/keymaster/upvotes", strings.NewReader("upvotes=5"))
	reqG.RemoteAddr = "127.0.0.1:12345"
	reqG.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wG := httptest.NewRecorder()
	h.ServeHTTP(wG, reqG)
	if wG.Code != 303 || !strings.HasPrefix(wG.Header().Get("Location"), "/login") {
		t.Fatalf("guest upvote allocation must redirect to login: %d", wG.Code)
	}
}

func TestGoogleSignInFlow(t *testing.T) {
	h := setupTest(t)

	// 1. Not configured -> honest 503, no button on the login page.
	w503 := request(h, "GET", "/auth/google", "")
	if w503.Code != 503 || !strings.Contains(w503.Body.String(), "not configured") {
		t.Fatalf("unconfigured /auth/google must 503 clearly: %d %s", w503.Code, w503.Body.String())
	}
	wLogin := request(h, "GET", "/login", "")
	if strings.Contains(wLogin.Body.String(), "Sign in with Google") {
		t.Fatal("Google button must be hidden when not configured")
	}

	// 2. Configured with mock token+userinfo endpoints -> full flow works.
	var srvHits int
	var mu sync.Mutex
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		srvHits++
		mu.Unlock()
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-123"})
		case "/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"sub": "google-sub-1", "email": "google_user@example.com",
				"email_verified": true, "name": "Google User",
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer mock.Close()
	t.Setenv("NSM_GOOGLE_CLIENT_ID", "test-client-id")
	t.Setenv("NSM_GOOGLE_CLIENT_SECRET", "test-client-secret")
	t.Setenv("NSM_GOOGLE_TOKEN_URL", mock.URL+"/token")
	t.Setenv("NSM_GOOGLE_USERINFO_URL", mock.URL+"/userinfo")

	// Button is now shown.
	wLogin2 := request(h, "GET", "/login", "")
	if !strings.Contains(wLogin2.Body.String(), "Sign in with Google") {
		t.Fatal("Google button must appear when configured")
	}

	// Start the flow and capture state.
	wStart := request(h, "GET", "/auth/google", "")
	if wStart.Code != 302 {
		t.Fatalf("/auth/google: %d", wStart.Code)
	}
	loc, _ := url.Parse(wStart.Header().Get("Location"))
	if !strings.Contains(loc.Host, "accounts.google.com") {
		t.Fatalf("expected Google redirect, got %s", loc)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("missing oauth state")
	}
	var gCookie string
	for _, c := range wStart.Result().Cookies() {
		if c.Name == "nsm_g_state" {
			gCookie = c.Value
		}
	}
	if gCookie != state {
		t.Fatal("state cookie mismatch")
	}

	// Callback with valid state creates the account and signs in.
	reqC := httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state="+url.QueryEscape(state), nil)
	reqC.RemoteAddr = "127.0.0.1:12345"
	reqC.AddCookie(&http.Cookie{Name: "nsm_g_state", Value: state})
	wC := httptest.NewRecorder()
	h.ServeHTTP(wC, reqC)
	if wC.Code != 303 {
		t.Fatalf("callback: %d %s", wC.Code, wC.Body.String())
	}
	accMu.Lock()
	gu := findUserByLoginOrEmail("google_user@example.com")
	gu2 := false
	for i := range acc.Users {
		if acc.Users[i].GoogleID == "google-sub-1" {
			gu2 = true
		}
	}
	accMu.Unlock()
	if gu == nil || !gu2 {
		t.Fatal("Google account was not created/linked")
	}
	hasSession := false
	for _, c := range wC.Result().Cookies() {
		if c.Name == "nsm_session" && c.Value != "" {
			hasSession = true
		}
	}
	if !hasSession {
		t.Fatal("no session cookie after Google sign-in")
	}

	// Replay/denied state -> rejected.
	reqBad := httptest.NewRequest("GET", "/auth/google/callback?code=authcode&state=WRONG", nil)
	reqBad.RemoteAddr = "127.0.0.1:12345"
	reqBad.AddCookie(&http.Cookie{Name: "nsm_g_state", Value: state})
	wBad := httptest.NewRecorder()
	h.ServeHTTP(wBad, reqBad)
	if wBad.Code != 400 {
		t.Fatalf("state mismatch must 400: %d", wBad.Code)
	}
}

// ---- minimal SMTP capture server for delivery tests ----

var smtpCaptureMu sync.Mutex
var smtpCaptureServers = map[*testing.T]*smtpCapture{}

type smtpCapture struct {
	mu   sync.Mutex
	addr string
	msgs []string
}

func (c *smtpCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}
func (c *smtpCapture) body(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < len(c.msgs) {
		return c.msgs[i]
	}
	return ""
}
func (c *smtpCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.msgs...)
}

var lastSMTPCapture *smtpCapture

func smtpCaptureAddr(t *testing.T) string {
	t.Helper()
	smtpCaptureMu.Lock()
	defer smtpCaptureMu.Unlock()
	if lastSMTPCapture == nil {
		t.Fatal("no SMTP capture running")
	}
	return lastSMTPCapture.addr
}

func startSMTPCapture(t *testing.T) *smtpCapture {
	t.Helper()
	cap := &smtpCapture{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	smtpCaptureMu.Lock()
	lastSMTPCapture = cap
	smtpCaptureMu.Unlock()
	cap.addr = ln.Addr().String()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				fmt.Fprintf(c, "220 capture ESMTP\r\n")
				br := bufio.NewReader(c)
				inData := false
				var data strings.Builder
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					trim := strings.TrimRight(line, "\r\n")
					if inData {
						if trim == "." {
							inData = false
							cap.mu.Lock()
							cap.msgs = append(cap.msgs, data.String())
							cap.mu.Unlock()
							data.Reset()
							fmt.Fprintf(c, "250 OK\r\n")
							continue
						}
						data.WriteString(line)
						data.WriteString("\n")
						continue
					}
					upper := strings.ToUpper(trim)
					switch {
					case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
						fmt.Fprintf(c, "250-capture\r\n250 AUTH PLAIN LOGIN\r\n")
					case strings.HasPrefix(upper, "AUTH"):
						fmt.Fprintf(c, "235 ok\r\n")
					case strings.HasPrefix(upper, "DATA"):
						inData = true
						fmt.Fprintf(c, "354 end with .\r\n")
					case strings.HasPrefix(upper, "QUIT"):
						fmt.Fprintf(c, "221 bye\r\n")
						return
					default:
						fmt.Fprintf(c, "250 OK\r\n")
					}
				}
			}(conn)
		}
	}()
	return cap
}

func TestJoinCodes(t *testing.T) {
	h := setupTest(t)
	sess := registerTestUser(t, h, "joinowner")

	// 1. Each key gets its own join code ("changed per key").
	wIssueA := requestAuth(h, "POST", "/api/keymaster/issue",
		`{"label":"Join Alpha","boundEndpoint":"127.0.0.1:30120","ttlSeconds":3600}`, map[string]string{"X-NSM-Session": sess})
	if wIssueA.Code != 201 {
		t.Fatalf("issue key A: %d %s", wIssueA.Code, wIssueA.Body.String())
	}
	var issueA struct {
		ServerKey string                 `json:"serverKey"`
		Key       map[string]interface{} `json:"key"`
	}
	if err := json.Unmarshal(wIssueA.Body.Bytes(), &issueA); err != nil {
		t.Fatalf("issue A json: %v", err)
	}
	codeA, _ := issueA.Key["joinCode"].(string)
	idA, _ := issueA.Key["id"].(string)
	if len(codeA) != joinCodeLen || !isJoinCode(codeA) {
		t.Fatalf("key A join code %q is not a %d-char code", codeA, joinCodeLen)
	}

	wIssueB := requestAuth(h, "POST", "/api/keymaster/issue",
		`{"label":"Join Beta","boundEndpoint":"127.0.0.1:30120","ttlSeconds":3600}`, map[string]string{"X-NSM-Session": sess})
	if wIssueB.Code != 201 {
		t.Fatalf("issue key B: %d %s", wIssueB.Code, wIssueB.Body.String())
	}
	var issueB struct {
		Key map[string]interface{} `json:"key"`
	}
	json.Unmarshal(wIssueB.Body.Bytes(), &issueB)
	codeB, _ := issueB.Key["joinCode"].(string)
	if codeB == "" || codeB == codeA {
		t.Fatalf("each key must get its own join code, got %q and %q", codeA, codeB)
	}

	// 2. A keyed heartbeat binds the code to the live endpoint; /join/<code>
	//    resolves it WITHOUT exposing the IP in rendered displays.
	hbKeyed := `{"port":30120,"fallbackData":{"players":[],"info":{"resources":["chat"],"vars":{"sv_nsmServerKey":"` + issueA.ServerKey + `"}},"dynamic":{"clients":3,"svMaxclients":48,"hostname":"Join Code Test"}}}`
	if w := request(h, "POST", "/api/serverlist/ingress", hbKeyed); w.Code != 200 {
		t.Fatalf("keyed heartbeat: %d %s", w.Code, w.Body.String())
	}
	wJoin := request(h, "GET", "/join/"+codeA, "")
	if wJoin.Code != 200 {
		t.Fatalf("GET /join/%s: %d %s", codeA, wJoin.Code, wJoin.Body.String())
	}
	if got := wJoin.Header().Get("X-CitizenFX-Url"); got != "http://127.0.0.1:30120/" {
		t.Fatalf("join code must hand off the resolved endpoint to NSM.exe, got X-CitizenFX-Url=%q", got)
	}
	if got := wJoin.Header().Get("Location"); got != "https://nsmultiplayer.com/join/"+codeA {
		t.Fatalf("join code page must canonicalize to the code URL, got Location=%q", got)
	}
	body := wJoin.Body.String()
	if !strings.Contains(body, "https://nsmultiplayer.com/join/"+codeA) {
		t.Fatalf("join code page missing code share URL: %s", body)
	}
	if strings.Contains(body, "join/127.0.0.1:30120") || strings.Contains(body, "connect 127.0.0.1:30120") {
		t.Fatal("join code page leaked the raw endpoint into rendered displays")
	}

	// 3. Unknown codes 404.
	if w := request(h, "GET", "/join/zzzzzzz", ""); w.Code != 404 {
		t.Fatalf("unknown join code: want 404, got %d", w.Code)
	}

	// 4. Keymaster shows the shareable code URL to its owner.
	wKM := requestAuth(h, "GET", "/keymaster", "", map[string]string{"X-NSM-Session": sess})
	if wKM.Code != 200 || !strings.Contains(wKM.Body.String(), "nsmultiplayer.com/join/"+codeA) {
		t.Fatalf("keymaster missing join code link: %d", wKM.Code)
	}

	// 5. Revoking a key kills its code; other keys' codes stay alive.
	wRev := requestAuth(h, "POST", "/api/keymaster/revoke",
		`{"keyId":"`+idA+`"}`, map[string]string{"X-NSM-Session": sess})
	if wRev.Code != 200 {
		t.Fatalf("revoke key A: %d %s", wRev.Code, wRev.Body.String())
	}
	if w := request(h, "GET", "/join/"+codeA, ""); w.Code != 404 {
		t.Fatalf("revoked key's join code must die, got %d", w.Code)
	}
	if w := request(h, "GET", "/join/"+codeB, ""); w.Code != 200 {
		t.Fatalf("other key's join code must survive, got %d", w.Code)
	}

	// 6. Keyless servers get an auto join code so every owner can hide their IP.
	hbKeyless := `{"port":30125,"fallbackData":{"players":[],"info":{"resources":["chat"],"vars":{}},"dynamic":{"clients":1,"svMaxclients":48,"hostname":"Keyless Hide IP"}}}`
	if w := request(h, "POST", "/api/serverlist/ingress", hbKeyless); w.Code != 200 {
		t.Fatalf("keyless heartbeat: %d %s", w.Code, w.Body.String())
	}
	wList := request(h, "GET", "/servers", "")
	m := regexp.MustCompile(`data-join-code="([a-z0-9]+)"`).FindStringSubmatch(wList.Body.String())
	if m == nil {
		t.Fatal("server list rows must carry a join code")
	}
	codeK := m[1]
	wJoinK := request(h, "GET", "/join/"+codeK, "")
	if wJoinK.Code != 200 || wJoinK.Header().Get("X-CitizenFX-Url") != "http://127.0.0.1:30125/" {
		t.Fatalf("keyless join code: %d hdr=%q", wJoinK.Code, wJoinK.Header().Get("X-CitizenFX-Url"))
	}
	bodyK := wJoinK.Body.String()
	if !strings.Contains(bodyK, "https://nsmultiplayer.com/join/"+codeK) {
		t.Fatalf("keyless join page missing code share URL: %s", bodyK)
	}
	if strings.Contains(bodyK, "join/127.0.0.1:30125") || strings.Contains(bodyK, "connect 127.0.0.1:30125") {
		t.Fatal("keyless join page leaked the raw endpoint into rendered displays")
	}

	// 7. Legacy /join/<ip:port> keeps working for old links (compat).
	wLeg := request(h, "GET", "/join/127.0.0.1:30120", "")
	if wLeg.Code != 200 || wLeg.Header().Get("X-CitizenFX-Url") != "http://127.0.0.1:30120/" {
		t.Fatalf("legacy /join/{ep} broke: %d hdr=%q", wLeg.Code, wLeg.Header().Get("X-CitizenFX-Url"))
	}

	// 8. Per-server uniqueness like cfx.re: even ONE key running several
	//    servers gives every server its own distinct code resolving to its
	//    own endpoint.
	wIssueU := requestAuth(h, "POST", "/api/keymaster/issue",
		`{"label":"Multi Server","ttlSeconds":3600}`, map[string]string{"X-NSM-Session": sess})
	var issueU struct {
		ServerKey string `json:"serverKey"`
	}
	json.Unmarshal(wIssueU.Body.Bytes(), &issueU)
	for _, port := range []string{"30131", "30132"} {
		hbU := strings.Replace(validHeartbeat, `"port":30120`, `"port":`+port, 1)
		if w := requestAuth(h, "POST", "/api/serverlist/ingress", hbU, map[string]string{"X-NSM-Server-Key": issueU.ServerKey}); w.Code != 200 {
			t.Fatalf("multi-server heartbeat on %s: %d %s", port, w.Code, w.Body.String())
		}
	}
	wList2 := request(h, "GET", "/servers", "")
	pairs := regexp.MustCompile(`data-ep="(127\.0\.0\.1:3013[12])"\s+data-join-code="([a-z0-9]+)"`).FindAllStringSubmatch(wList2.Body.String(), -1)
	if len(pairs) != 2 {
		t.Fatalf("expected both multi-server rows to carry join codes, got %d: %s", len(pairs), wList2.Body.String()[:min(200, len(wList2.Body.String()))])
	}
	seen := map[string]string{}
	for _, p := range pairs {
		if !isJoinCode(p[2]) {
			t.Fatalf("row %s has invalid join code %q", p[1], p[2])
		}
		if other, dup := seen[p[2]]; dup {
			t.Fatalf("servers %s and %s share join code %q — every server needs its own", other, p[1], p[2])
		}
		seen[p[2]] = p[1]
		w := request(h, "GET", "/join/"+p[2], "")
		if w.Code != 200 || w.Header().Get("X-CitizenFX-Url") != "http://"+p[1]+"/" {
			t.Fatalf("join code %s must resolve to %s, got %d hdr=%q", p[2], p[1], w.Code, w.Header().Get("X-CitizenFX-Url"))
		}
	}
}
