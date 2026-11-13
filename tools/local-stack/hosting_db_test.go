package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Database provisioning: per-customer isolation, credential handling and
// honest failure when no database service is configured.
// ---------------------------------------------------------------------------

type fakeDBProv struct {
	dbs      map[string]bool // dbName -> exists
	users    map[string]string
	imports  map[string][]string
	calls    []string
	failWith error
}

func newFakeDBProv() *fakeDBProv {
	return &fakeDBProv{dbs: map[string]bool{}, users: map[string]string{}, imports: map[string][]string{}}
}

func (f *fakeDBProv) Ping() error { return f.failWith }

func (f *fakeDBProv) EnsureDatabase(dbName, dbUser, dbPass string) error {
	f.calls = append(f.calls, "ensure "+dbName+" "+dbUser)
	if f.failWith != nil {
		return f.failWith
	}
	f.dbs[dbName] = true
	f.users[dbUser] = dbPass
	return nil
}

func (f *fakeDBProv) DropDatabase(dbName, dbUser string) error {
	f.calls = append(f.calls, "drop "+dbName+" "+dbUser)
	if f.failWith != nil {
		return f.failWith
	}
	delete(f.dbs, dbName)
	delete(f.users, dbUser)
	return nil
}

func (f *fakeDBProv) SetPassword(dbUser, dbPass string) error {
	f.calls = append(f.calls, "setpass "+dbUser)
	if f.failWith != nil {
		return f.failWith
	}
	if _, ok := f.users[dbUser]; !ok {
		return errDBNotConfigured
	}
	f.users[dbUser] = dbPass
	return nil
}

func (f *fakeDBProv) ImportSQL(dbName, sqlText string) error {
	f.calls = append(f.calls, "import "+dbName)
	if f.failWith != nil {
		return f.failWith
	}
	f.imports[dbName] = append(f.imports[dbName], sqlText)
	return nil
}

// seedTwoAccounts creates two accounts with ready sessions and one owned
// server each; returns their session tokens and server ids.
func seedTwoAccounts(t *testing.T, h http.Handler) (sessA, idA, sessB, idB string) {
	t.Helper()
	accMu.Lock()
	acc.Sessions["db-sess-a"] = Session{UserID: 301, Expires: time.Now().Add(time.Hour).Unix()}
	acc.Sessions["db-sess-b"] = Session{UserID: 302, Expires: time.Now().Add(time.Hour).Unix()}
	acc.Users = append(acc.Users,
		User{ID: 301, Username: "dbusera", Role: "user"},
		User{ID: 302, Username: "dbuserb", Role: "user"})
	accMu.Unlock()
	m := hostMgr()
	m.mu.Lock()
	for _, fx := range []*HostedServer{
		{ID: "hdbsrv-a", Owner: 301, OwnerName: "dbusera", Name: "city-a", CreatedAt: time.Now().Unix(), Status: hostOffline},
		{ID: "hdbsrv-b", Owner: 302, OwnerName: "dbuserb", Name: "city-b", CreatedAt: time.Now().Unix(), Status: hostOffline},
	} {
		if err := os.MkdirAll(m.dirOf(fx), 0o755); err != nil {
			m.mu.Unlock()
			t.Fatal(err)
		}
		m.st.Servers = append(m.st.Servers, fx)
	}
	m.saveLocked()
	m.mu.Unlock()
	return "db-sess-a", "hdbsrv-a", "db-sess-b", "hdbsrv-b"
}

func TestDatabaseIsolationAndCredentialHandling(t *testing.T) {
	h := setupTest(t)
	resetHostMgr(t)
	fake := newFakeDBProv()
	dbProvOverride = fake
	defer func() { dbProvOverride = nil }()
	sessA, idA, sessB, idB := seedTwoAccounts(t, h)
	auth := func(s string) map[string]string { return map[string]string{"X-NSM-Session": s} }

	// 1. owner creates a database — schema + user names are customer-scoped.
	w := requestAuth(h, "POST", "/api/host/"+idA+"/dbs", `{"name":"main"}`, auth(sessA))
	if w.Code != 200 {
		t.Fatalf("create db: %d %s", w.Code, w.Body.String())
	}
	var cr struct {
		OK bool           `json:"ok"`
		DB map[string]any `json:"db"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cr); err != nil || !cr.OK {
		t.Fatalf("bad create response: %s", w.Body.String())
	}
	dbName, _ := cr.DB["dbName"].(string)
	dbUser, _ := cr.DB["dbUser"].(string)
	dbPass, _ := cr.DB["dbPass"].(string)
	if dbName != "nsm_301_main" || dbUser != "nsmu_301_main" {
		t.Fatalf("customer-scoped names expected, got %s / %s", dbName, dbUser)
	}
	if len(dbPass) < 16 {
		t.Fatalf("password too short: %q", dbPass)
	}
	if !fake.dbs[dbName] || fake.users[dbUser] != dbPass {
		t.Fatal("provisioner must receive the created database/user/password")
	}

	// 2. LIST responses never carry the password.
	w = requestAuth(h, "GET", "/api/host/"+idA+"/dbs", "", auth(sessA))
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), dbPass) {
		t.Fatal("list response leaked the database password")
	}

	// 3. other customers cannot see, rotate or drop this database.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/host/" + idA + "/dbs/" + "main"},
		{"POST", "/api/host/" + idA + "/dbs/" + "main" + "/rotate"},
		{"POST", "/api/host/" + idA + "/dbs/" + "main" + "/drop"},
		{"GET", "/api/host/" + idA + "/dbs"},
	} {
		w = requestAuth(h, tc.method, tc.path, "", auth(sessB))
		if w.Code != 404 {
			t.Fatalf("cross-customer %s %s must 404, got %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// 4. second customer creating the same label gets an isolated namespace.
	w = requestAuth(h, "POST", "/api/host/"+idB+"/dbs", `{"name":"main"}`, auth(sessB))
	if w.Code != 200 {
		t.Fatalf("create db B: %d %s", w.Code, w.Body.String())
	}
	var crB struct {
		DB map[string]any `json:"db"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &crB)
	if bn, _ := crB.DB["dbName"].(string); bn != "nsm_302_main" {
		t.Fatalf("customer B must get its own namespace, got %v", crB.DB["dbName"])
	}

	// 5. duplicate label on the same server is refused.
	w = requestAuth(h, "POST", "/api/host/"+idA+"/dbs", `{"name":"main"}`, auth(sessA))
	if w.Code != 409 {
		t.Fatalf("duplicate db name must 409, got %d", w.Code)
	}

	// 6. owner detail view carries the password + HeidiSQL block.
	w = requestAuth(h, "GET", "/api/host/"+idA+"/dbs/main", "", auth(sessA))
	if w.Code != 200 || !strings.Contains(w.Body.String(), dbPass) {
		t.Fatalf("owner detail must include the password: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"heidi"`) || !strings.Contains(w.Body.String(), `"network":"MySQL (TCP/IP)"`) {
		t.Fatalf("detail must include HeidiSQL connection info: %s", w.Body.String())
	}

	// 7. password rotation reaches the provisioner and the owner only.
	w = requestAuth(h, "POST", "/api/host/"+idA+"/dbs/main/rotate", "", auth(sessA))
	if w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	var rr struct {
		Pass string `json:"dbPass"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &rr)
	if rr.Pass == "" || rr.Pass == dbPass || fake.users[dbUser] != rr.Pass {
		t.Fatalf("rotate must change the stored password: %+v", rr)
	}

	// 8. drop removes it at the service and locally.
	w = requestAuth(h, "POST", "/api/host/"+idA+"/dbs/main/drop", "", auth(sessA))
	if w.Code != 200 {
		t.Fatalf("drop: %d %s", w.Code, w.Body.String())
	}
	if fake.dbs["nsm_301_main"] {
		t.Fatal("drop must remove the schema at the database service")
	}
	w = requestAuth(h, "GET", "/api/host/"+idA+"/dbs", "", auth(sessA))
	if strings.Contains(w.Body.String(), `"dbName"`) {
		t.Fatalf("dropped db must disappear from the list: %s", w.Body.String())
	}

	// 9. invalid labels are refused.
	for _, bad := range []string{"x", "has space", "up.per", "toolongtoolongtoolongtoolong"} {
		w = requestAuth(h, "POST", "/api/host/"+idA+"/dbs", `{"name":"`+bad+`"}`, auth(sessA))
		if w.Code != 400 {
			t.Fatalf("bad name %q must 400, got %d", bad, w.Code)
		}
	}
}

func TestDatabaseUnconfiguredIsHonest(t *testing.T) {
	h := setupTest(t)
	resetHostMgr(t)
	dbProvOverride = nil
	os.Unsetenv("NSM_DB_ADMIN_DSN")
	sessA, idA, _, _ := seedTwoAccounts(t, h)
	auth := func(s string) map[string]string { return map[string]string{"X-NSM-Session": s} }

	w := requestAuth(h, "GET", "/api/host/"+idA+"/dbs", "", auth(sessA))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("list must report configured:false: %d %s", w.Code, w.Body.String())
	}
	w = requestAuth(h, "POST", "/api/host/"+idA+"/dbs", `{"name":"main"}`, auth(sessA))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Fatalf("create without a database service must be an honest 503: %d %s", w.Code, w.Body.String())
	}
}

func TestSQLStatementSplitting(t *testing.T) {
	in := "-- comment\nCREATE TABLE a (id INT);\n/* block ; comment */\nINSERT INTO a VALUES ('x;y', \"a;b\", `c;d`); # trailing\nSELECT 1\n"
	got := splitSQLStatements(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 statements, got %d: %q", len(got), got)
	}
	if !strings.HasSuffix(got[0], ";") || strings.Contains(got[0], "--") {
		t.Fatalf("statement 1 wrong: %q", got[0])
	}
	if strings.Contains(got[1], "INSERT") && !strings.Contains(got[1], "'x;y'") {
		t.Fatalf("quoted semicolons must survive: %q", got[1])
	}
}

func TestDBConnectionStringEscaping(t *testing.T) {
	d := &hostedDatabase{DBUser: "nsmu_1_x", DBPass: "p@ss/w:rd", DBName: "nsm_1_x"}
	s := mysqlConnString(d, dbService{host: "127.0.0.1", port: 3306})
	if strings.Contains(s, "p@ss/w:rd") {
		t.Fatal("password must be URL-escaped in the connection string")
	}
	if !strings.Contains(s, "nsm_1_x") {
		t.Fatalf("connection string incomplete: %s", s)
	}
}

