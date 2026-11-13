// hosting_db.go — per-customer MySQL/MariaDB databases for hosted servers.
//
// Every hosted server can create its own databases from the panel. Each
// database gets its own MySQL user whose grants cover exactly that one
// schema — customers can never read or write another customer's data.
// Admin credentials live ONLY in NSM_DB_ADMIN_DSN on the server and are
// never returned by any endpoint. Passwords are returned only to the owner
// through the single-database endpoint (list responses never carry them).
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// hostedDatabase is one database provisioned for a hosted server.
type hostedDatabase struct {
	ID      string `json:"id"`
	Name    string `json:"name"`    // user label ("main", "shop", ...)
	DBName  string `json:"dbName"`  // schema name: nsm_<uid>_<label>
	DBUser  string `json:"dbUser"`  // dedicated MySQL user (grants: this schema only)
	DBPass  string `json:"dbPass"`  // secret — owner-only responses, never list/stats
	Created int64  `json:"created"`
}

var dbNameRE = regexp.MustCompile(`^[a-z0-9_]{2,20}$`)

// dbProvisioner abstracts the MySQL/MariaDB server that stores the data.
// Production wires mysqlProvisioner; tests inject fakes. Every method must
// fail when the service is not configured — never fake success.
type dbProvisioner interface {
	Ping() error
	EnsureDatabase(dbName, dbUser, dbPass string) error
	DropDatabase(dbName, dbUser string) error
	SetPassword(dbUser, dbPass string) error
	ImportSQL(dbName, sqlText string) error
}

// mysqlProvisioner drives a real MySQL/MariaDB server through its admin DSN.
type mysqlProvisioner struct{ adminDSN string }

func (p *mysqlProvisioner) open() (*sql.DB, error) {
	db, err := sql.Open("mysql", p.adminDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

func (p *mysqlProvisioner) Ping() error {
	db, err := p.open()
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Ping()
}

func (p *mysqlProvisioner) EnsureDatabase(dbName, dbUser, dbPass string) error {
	if err := validateDBIdent(dbName, dbUser); err != nil {
		return err
	}
	db, err := p.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	// Dedicated user with grants on exactly one schema. IDENTIFIED BY keeps
	// the password out of the general log; the value is escaped by quoteDB.
	if _, err := db.Exec("CREATE USER IF NOT EXISTS " + quoteDB(dbUser) + "@'%' IDENTIFIED BY " + quoteDB(dbPass)); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	if _, err := db.Exec("ALTER USER " + quoteDB(dbUser) + "@'%' IDENTIFIED BY " + quoteDB(dbPass)); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if _, err := db.Exec("GRANT ALL PRIVILEGES ON `" + dbName + "`.* TO " + quoteDB(dbUser) + "@'%'"); err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	if _, err := db.Exec("FLUSH PRIVILEGES"); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

func (p *mysqlProvisioner) DropDatabase(dbName, dbUser string) error {
	if err := validateDBIdent(dbName, dbUser); err != nil {
		return err
	}
	db, err := p.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	if _, err := db.Exec("DROP USER IF EXISTS " + quoteDB(dbUser) + "@'%'"); err != nil {
		return fmt.Errorf("drop user: %w", err)
	}
	if _, err := db.Exec("FLUSH PRIVILEGES"); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

func (p *mysqlProvisioner) SetPassword(dbUser, dbPass string) error {
	if err := validateDBIdent("nsm_x", dbUser); err != nil {
		return err
	}
	db, err := p.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("ALTER USER " + quoteDB(dbUser) + "@'%' IDENTIFIED BY " + quoteDB(dbPass)); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if _, err := db.Exec("FLUSH PRIVILEGES"); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

func (p *mysqlProvisioner) ImportSQL(dbName, sqlText string) error {
	if err := validateDBIdent(dbName, "nsm_x"); err != nil {
		return err
	}
	db, err := p.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("USE `" + dbName + "`"); err != nil {
		return fmt.Errorf("use database: %w", err)
	}
	// Split on semicolons at statement boundaries. Framework schema files
	// are plain DDL — no stored procedures — so this is safe and keeps the
	// driver happy (it does not accept multi-statements without multiQueries).
	for _, stmt := range splitSQLStatements(sqlText) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("import: %w", err)
		}
	}
	return nil
}

// splitSQLStatements splits a schema file into single statements. It honors
// '--' line comments, '# ' line comments and '/* */' block comments, and
// does not split inside single/double/backtick-quoted strings.
func splitSQLStatements(s string) []string {
	var out []string
	var b strings.Builder
	inS, inD, inB, inLine, inBlock := false, false, false, false, false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		next := func() rune { if i+1 < len(rs) { return rs[i+1] }; return 0 }
		switch {
		case inLine:
			if c == '\n' { inLine = false; b.WriteRune(c) }
		case inBlock:
			if c == '*' && next() == '/' { inBlock = false; i++ }
		case inS:
			b.WriteRune(c)
			if c == '\'' && next() == '\'' { b.WriteRune('\''); i++; continue }
			if c == '\'' { inS = false }
		case inD:
			b.WriteRune(c)
			if c == '"' && next() == '"' { b.WriteRune('"'); i++; continue }
			if c == '"' { inD = false }
		case inB:
			b.WriteRune(c)
			if c == '`' { inB = false }
		default:
			switch {
			case c == '-' && next() == '-':
				inLine = true; i++
			case c == '#':
				inLine = true
			case c == '/' && next() == '*':
				inBlock = true; i++
			case c == '\'':
				inS = true; b.WriteRune(c)
			case c == '"':
				inD = true; b.WriteRune(c)
			case c == '`':
				inB = true; b.WriteRune(c)
			case c == ';':
				if t := strings.TrimSpace(b.String()); t != "" && !isOnlyComments(t) {
					out = append(out, t+";")
				}
				b.Reset()
			default:
				b.WriteRune(c)
			}
		}
	}
	if t := strings.TrimSpace(b.String()); t != "" && !isOnlyComments(t) {
		out = append(out, t+";")
	}
	return out
}

func isOnlyComments(s string) bool {
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "--") || strings.HasPrefix(t, "#") {
			continue
		}
		return false
	}
	return true
}

// quoteDB quotes a SQL string/identifier value for an admin connection.
func quoteDB(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// validateDBIdent rejects anything that is not a plain generated identifier
// before it is ever placed into a DDL statement.
func validateDBIdent(dbName, dbUser string) error {
	ident := regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
	if !ident.MatchString(dbName) {
		return errors.New("invalid database name")
	}
	if !ident.MatchString(dbUser) {
		return errors.New("invalid database user")
	}
	return nil
}

// ---------------------------------------------------------------------------

// dbService holds the (optional) provisioner plus connection display info.
type dbService struct {
	prov dbProvisioner // nil when no database service is configured
	host string       // host frameworks should dial (usually loopback)
	port int          // MySQL port
	pub  string       // address users type into HeidiSQL
}

// dbProvOverride lets tests inject a provisioner without a live database.
// Production leaves this nil and the mysqlProvisioner below does the work.
var dbProvOverride dbProvisioner

// dbServiceFor returns the configured database service. When
// NSM_DB_ADMIN_DSN is unset the provisioner is nil and every API answers an
// honest "not configured" — nothing is faked.
func dbServiceFor() dbService {
	svc := dbService{
		host: envOr("NSM_DB_HOST", "127.0.0.1"),
		port: 3306,
		pub:  serverHostIP(),
	}
	if p := strings.TrimSpace(os.Getenv("NSM_DB_PORT")); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			svc.port = n
		}
	}
	switch {
	case dbProvOverride != nil:
		svc.prov = dbProvOverride
	case strings.TrimSpace(os.Getenv("NSM_DB_ADMIN_DSN")) != "":
		svc.prov = &mysqlProvisioner{adminDSN: strings.TrimSpace(os.Getenv("NSM_DB_ADMIN_DSN"))}
	}
	return svc
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func dbRandSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)[:n]
}

// mysqlConnString renders the connection string frameworks get in
// server.cfg. The password is URL-escaped so special characters are safe.
func mysqlConnString(d *hostedDatabase, svc dbService) string {
	return fmt.Sprintf("mysql://%s:%s@%s:%d/%s?charset=utf8mb4",
		d.DBUser, url.QueryEscape(d.DBPass), svc.host, svc.port, d.DBName)
}

// dbSafe is the wire form of a database for LIST responses — no secret.
func dbSafe(d *hostedDatabase, svc dbService) map[string]interface{} {
	return map[string]interface{}{
		"id": d.ID, "name": d.Name, "dbName": d.DBName, "dbUser": d.DBUser,
		"created": d.Created, "host": svc.pub, "port": svc.port,
		"hostFromServer": svc.host,
	}
}

// dbDetail is the owner-only wire form: includes the password + HeidiSQL block.
func dbDetail(d *hostedDatabase, svc dbService) map[string]interface{} {
	m := dbSafe(d, svc)
	m["dbPass"] = d.DBPass
	m["heidi"] = map[string]interface{}{
		"network": "MySQL (TCP/IP)", "host": svc.pub, "port": svc.port,
		"user": d.DBUser, "password": d.DBPass, "database": d.DBName,
	}
	return m
}

// findDB returns the index of a database on a server (by id or label).
func findDB(s *HostedServer, key string) (int, *hostedDatabase) {
	for i := range s.DBs {
		if s.DBs[i].ID == key || s.DBs[i].Name == key {
			return i, &s.DBs[i]
		}
	}
	return -1, nil
}

// ensureServerDatabase finds the framework database ("main") or creates it.
// Returns nil when the database service is not configured.
func (m *hostingManager) ensureServerDatabase(s *HostedServer, label string) *hostedDatabase {
	svc := dbServiceFor()
	if svc.prov == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.find(s.ID)
	if cur == nil {
		return nil
	}
	if _, d := findDB(cur, label); d != nil {
		return d
	}
	d := &hostedDatabase{
		ID:      "db" + dbRandSecret(10),
		Name:    label,
		DBName:  fmt.Sprintf("nsm_%d_%s", cur.Owner, label),
		DBUser:  fmt.Sprintf("nsmu_%d_%s", cur.Owner, label),
		DBPass:  dbRandSecret(24),
		Created: time.Now().Unix(),
	}
	if err := svc.prov.EnsureDatabase(d.DBName, d.DBUser, d.DBPass); err != nil {
		m.logNL(cur, "[databases] provisioning failed: "+err.Error())
		return nil
	}
	cur.DBs = append(cur.DBs, *d)
	m.saveLocked()
	m.logNL(cur, "[databases] created database "+d.DBName+" for user "+d.DBUser)
	return d
}

// importServerSQL imports a schema into the given database. When no
// database service is configured (or db is nil), the SQL is written into
// nsm-db-import/ inside the server folder so the owner can import it via
// HeidiSQL — the panel never claims success it cannot guarantee.
func (m *hostingManager) importServerSQL(s *HostedServer, d *hostedDatabase, fileLabel, sqlText string) string {
	svc := dbServiceFor()
	if d != nil && svc.prov != nil {
		if err := svc.prov.ImportSQL(d.DBName, sqlText); err != nil {
			m.appendLog(s, "[databases] schema import failed for "+d.DBName+": "+err.Error())
			return "schema import failed: " + err.Error()
		}
		m.appendLog(s, "[databases] imported "+fileLabel+" into "+d.DBName)
		return "imported " + fileLabel + " into " + d.DBName
	}
	// No database service: leave the schema on disk for manual import.
	dir := filepath.Join(m.serverRoot(s), "nsm-db-import")
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, fileLabel)
	if err := os.WriteFile(p, []byte(sqlText), 0o644); err != nil {
		return "could not save " + fileLabel + ": " + err.Error()
	}
	m.appendLog(s, "[databases] no database service configured — saved "+fileLabel+" to nsm-db-import/ (import it with HeidiSQL)")
	return "saved " + fileLabel + " to nsm-db-import/ — create a database in the Databases tab and import it with HeidiSQL"
}

// ---------------------------------------------------------------------------
// HTTP endpoints (all behind panelOwned: signed in + own server only)
// ---------------------------------------------------------------------------

func apiHostDBs(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	svc := dbServiceFor()
	m.mu.Lock()
	cur := m.find(snap.ID)
	list := []map[string]interface{}{}
	if cur != nil {
		for i := range cur.DBs {
			list = append(list, dbSafe(&cur.DBs[i], svc))
		}
	}
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"configured": svc.prov != nil,
		"dbs":        list,
		"host":       svc.pub, "port": svc.port, "hostFromServer": svc.host,
	})
}

var errDBNotConfigured = errors.New("database service not configured on this host")

func apiHostDBCreate(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	svc := dbServiceFor()
	if svc.prov == nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]string{"error": errDBNotConfigured.Error()})
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	if name == "" {
		// accepts both {"name":"..."} JSON bodies and plain form posts
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err == nil {
			name = strings.ToLower(strings.TrimSpace(body.Name))
		}
	}
	if !dbNameRE.MatchString(name) {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "database name must be 2-20 characters: letters, numbers, underscore"})
		return
	}
	if err := m.checkDiskAllowance(snap, 1); err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	m.mu.Lock()
	cur := m.find(snap.ID)
	if cur == nil {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "server not found"})
		return
	}
	if _, d := findDB(cur, name); d != nil {
		m.mu.Unlock()
		sendJSON(w, http.StatusConflict, map[string]string{"error": "a database named " + name + " already exists"})
		return
	}
	if len(cur.DBs) >= 8 {
		m.mu.Unlock()
		sendJSON(w, http.StatusConflict, map[string]string{"error": "database limit reached (8 per server)"})
		return
	}
	nd := hostedDatabase{
		ID:      "db" + dbRandSecret(10),
		Name:    name,
		DBName:  fmt.Sprintf("nsm_%d_%s", cur.Owner, name),
		DBUser:  fmt.Sprintf("nsmu_%d_%s", cur.Owner, name),
		DBPass:  dbRandSecret(24),
		Created: time.Now().Unix(),
	}
	uid := cur.Owner
	m.mu.Unlock()

	if err := validateDBIdent(nd.DBName, nd.DBUser); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := svc.prov.EnsureDatabase(nd.DBName, nd.DBUser, nd.DBPass); err != nil {
		sendJSON(w, http.StatusBadGateway, map[string]string{"error": "database service error: " + err.Error()})
		return
	}
	m.mu.Lock()
	cur = m.find(snap.ID)
	if cur == nil || cur.Owner != uid {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "server not found"})
		return
	}
	cur.DBs = append(cur.DBs, nd)
	m.saveLocked()
	m.logNL(cur, "[databases] created database "+nd.DBName+" for user "+nd.DBUser)
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "db": dbDetail(&nd, svc)})
}

func apiHostDBOne(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	svc := dbServiceFor()
	m.mu.Lock()
	cur := m.find(snap.ID)
	var d *hostedDatabase
	var snapDB hostedDatabase
	if cur != nil {
		_, d = findDB(cur, r.PathValue("db"))
	}
	if d != nil {
		snapDB = *d
	}
	m.mu.Unlock()
	if d == nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "database not found"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"configured": svc.prov != nil, "db": dbDetail(&snapDB, svc)})
}

func apiHostDBRotate(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	svc := dbServiceFor()
	if svc.prov == nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]string{"error": errDBNotConfigured.Error()})
		return
	}
	newPass := dbRandSecret(24)
	m.mu.Lock()
	cur := m.find(snap.ID)
	var d *hostedDatabase
	if cur != nil {
		_, d = findDB(cur, r.PathValue("db"))
	}
	if d != nil {
		d.DBPass = newPass
		m.saveLocked()
	}
	user := ""
	if d != nil {
		user = d.DBUser
	}
	m.mu.Unlock()
	if d == nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "database not found"})
		return
	}
	if err := svc.prov.SetPassword(user, newPass); err != nil {
		sendJSON(w, http.StatusBadGateway, map[string]string{"error": "database service error: " + err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "dbPass": newPass})
}

func apiHostDBDrop(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	svc := dbServiceFor()
	if svc.prov == nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]string{"error": errDBNotConfigured.Error()})
		return
	}
	m.mu.Lock()
	cur := m.find(snap.ID)
	var d *hostedDatabase
	if cur != nil {
		_, d = findDB(cur, r.PathValue("db"))
	}
	m.mu.Unlock()
	if d == nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "database not found"})
		return
	}
	if err := svc.prov.DropDatabase(d.DBName, d.DBUser); err != nil {
		sendJSON(w, http.StatusBadGateway, map[string]string{"error": "database service error: " + err.Error()})
		return
	}
	m.mu.Lock()
	cur = m.find(snap.ID)
	if cur != nil {
		i, d2 := findDB(cur, r.PathValue("db"))
		if d2 != nil {
			cur.DBs = append(cur.DBs[:i], cur.DBs[i+1:]...)
			m.saveLocked()
			m.logNL(cur, "[databases] dropped database "+d2.DBName)
		}
	}
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// registerHostingDBs wires the database endpoints.
func registerHostingDBs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/host/{id}/dbs", apiHostDBs)
	mux.HandleFunc("POST /api/host/{id}/dbs", apiHostDBCreate)
	mux.HandleFunc("GET /api/host/{id}/dbs/{db}", apiHostDBOne)
	mux.HandleFunc("POST /api/host/{id}/dbs/{db}/rotate", apiHostDBRotate)
	mux.HandleFunc("POST /api/host/{id}/dbs/{db}/drop", apiHostDBDrop)
	mux.HandleFunc("DELETE /api/host/{id}/dbs/{db}", apiHostDBDrop)
}

// sortDBs keeps panel output stable (creation order is already stable; this
// exists for deterministic tests).
func sortDBs(dbs []hostedDatabase) {
	sort.SliceStable(dbs, func(i, j int) bool { return dbs[i].Created < dbs[j].Created })
}
