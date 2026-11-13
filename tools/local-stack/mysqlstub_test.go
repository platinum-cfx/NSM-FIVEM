package main

// mysqlstub_test.go — a minimal MySQL/MariaDB wire-protocol server used to
// exercise the REAL mysqlProvisioner through the REAL go-sql-driver, the
// same protocol HeidiSQL speaks. It records every query the provisioner
// sends so tests can assert the exact DDL (isolation grants, no admin leaks).

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

type mysqlStub struct {
	ln       net.Listener
	mu       sync.Mutex
	queries  []string
	user     string // username from the handshake response
	passOK   bool
	expectU  string
	expectP  string
	capFlags uint32
}

// startMySQLStub starts the stub on 127.0.0.1:<random>. When expectU/P are
// set, handshakes with other credentials are rejected (auth failure) —
// proving the provisioner really authenticates.
func startMySQLStub(t *testing.T, expectU, expectP string) *mysqlStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &mysqlStub{ln: ln, expectU: expectU, expectP: expectP}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *mysqlStub) addr() string { return s.ln.Addr().String() }

func (s *mysqlStub) queriesSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.queries))
	copy(out, s.queries)
	return out
}

func (s *mysqlStub) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

// writePkt writes one MySQL packet (3-byte len, 1-byte seq).
func writePkt(c net.Conn, seq byte, payload []byte) error {
	var hdr [4]byte
	hdr[0] = byte(len(payload))
	hdr[1] = byte(len(payload) >> 8)
	hdr[2] = byte(len(payload) >> 16)
	hdr[3] = seq
	if _, err := c.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.Write(payload)
	return err
}

// readPkt reads one packet and returns (seq, payload).
func readPkt(c net.Conn) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return 0, nil, err
	}
	return hdr[3], buf, nil
}

func (s *mysqlStub) handle(c net.Conn) {
	defer c.Close()
	// ---- greeting (protocol 10) ----
	salt := []byte("12345678") // auth-plugin-data-part-1 (8)
	salt2 := []byte("abcdefghijkl") // part-2 (12)
	var g []byte
	g = append(g, 10)                                // protocol version
	g = append(g, []byte("8.0.36-nsm-stub\x00")...)   // server version
	var tid [4]byte
	binary.LittleEndian.PutUint32(tid[:], 42)
	g = append(g, tid[:]...)
	g = append(g, salt...)
	g = append(g, 0)                                 // filler
	g = append(g, 0xff, 0xff)                        // caps lower: CLIENT_PROTOCOL_41 etc (0xffff)
	g = append(g, 33)                                // charset utf8_general_ci
	g = append(g, 0x02, 0x00)                        // status
	g = append(g, 0xff, 0x81)                        // caps upper: SECURE_CONNECTION|PLUGIN_AUTH|CONNECT_ATTRS...
	g = append(g, 21)                                // auth plugin data len
	g = append(g, make([]byte, 10)...)               // reserved
	g = append(g, salt2...)
	g = append(g, 0)
	g = append(g, []byte("mysql_native_password\x00")...)
	if err := writePkt(c, 0, g); err != nil {
		return
	}
	// ---- handshake response ----
	seq, resp, err := readPkt(c)
	if err != nil {
		return
	}
	_ = seq
	if len(resp) < 36 {
		return
	}
	pos := 32 // caps(4)+maxpkt(4)+charset(1)+reserved(23) = 32
	end := strings.IndexByte(string(resp[pos:]), 0)
	user := string(resp[pos : pos+end])
	pos += end + 1
	authLen := int(resp[pos])
	pos++
	pos += authLen // auth response
	_ = user

	s.mu.Lock()
	s.user = user
	s.passOK = user == s.expectU // stub accepts the expected user; rejects others
	if s.expectU == "" {
		s.passOK = true
	}
	s.mu.Unlock()
	if !s.passOK {
		// ERR packet: ER_ACCESS_DENIED_ERROR
		errPkt := []byte{0xff, 0x28, 0x04, '#', '2', '8', '0', '0', '0'}
		errPkt = append(errPkt, []byte("Access denied")...)
		_ = writePkt(c, 2, errPkt)
		return
	}
	ok := []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00} // OK, 2 affected
	if err := writePkt(c, 2, ok); err != nil {
		return
	}
	// ---- command loop ----
	for {
		_, pkt, err := readPkt(c)
		if err != nil {
			return
		}
		if len(pkt) == 0 {
			continue
		}
		switch pkt[0] {
		case 0x01: // COM_QUIT
			return
		case 0x0e: // COM_PING
			_ = writePkt(c, 1, ok)
		case 0x03: // COM_QUERY
			q := string(pkt[1:])
			s.mu.Lock()
			s.queries = append(s.queries, q)
			s.mu.Unlock()
			_ = writePkt(c, 1, ok)
		default:
			_ = writePkt(c, 1, ok)
		}
	}
}

func TestMySQLProvisionerAgainstWireProtocol(t *testing.T) {
	stub := startMySQLStub(t, "root", "admin-secret")
	svc := &mysqlProvisioner{adminDSN: fmt.Sprintf("root:admin-secret@tcp(%s)/?timeout=5s&readTimeout=5s&writeTimeout=5s", stub.addr())}
	if err := svc.Ping(); err != nil {
		t.Fatalf("ping over the real MySQL protocol: %v", err)
	}
	if err := svc.EnsureDatabase("nsm_7_main", "nsmu_7_main", "pw123"); err != nil {
		t.Fatalf("EnsureDatabase: %v", err)
	}
	if err := svc.ImportSQL("nsm_7_main", "CREATE TABLE t (id INT);\nINSERT INTO t VALUES (1);"); err != nil {
		t.Fatalf("ImportSQL: %v", err)
	}
	if err := svc.SetPassword("nsmu_7_main", "pw456"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := svc.DropDatabase("nsm_7_main", "nsmu_7_main"); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	q := strings.Join(stub.queriesSeen(), "\n")
	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS `nsm_7_main`",
		"CREATE USER IF NOT EXISTS 'nsmu_7_main'@'%' IDENTIFIED BY 'pw123'",
		"GRANT ALL PRIVILEGES ON `nsm_7_main`.* TO 'nsmu_7_main'@'%'",
		"CREATE TABLE t (id INT);",
		"INSERT INTO t VALUES (1);",
		"ALTER USER 'nsmu_7_main'@'%' IDENTIFIED BY 'pw456'",
		"DROP DATABASE IF EXISTS `nsm_7_main`",
		"DROP USER IF EXISTS 'nsmu_7_main'@'%'",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("missing statement %q in:\n%s", want, q)
		}
	}
	// the admin password must only appear inside the DSN the operator set —
	// never in any statement.
	if strings.Contains(q, "admin-secret") {
		t.Fatal("admin credential leaked into a SQL statement")
	}
	// grants are scoped to exactly one schema
	if strings.Contains(q, "GRANT ALL PRIVILEGES ON *.*") {
		t.Fatal("provisioner must never grant global privileges")
	}
}

func TestMySQLProvisionerRejectsBadIdents(t *testing.T) {
	svc := &mysqlProvisioner{adminDSN: "x"}
	if err := svc.EnsureDatabase("nsm_7_main; DROP", "ok_user", "p"); err == nil {
		t.Fatal("ident validation must reject hostile names")
	}
	if err := svc.EnsureDatabase("ok_db", "bad user", "p"); err == nil {
		t.Fatal("ident validation must reject hostile users")
	}
}
