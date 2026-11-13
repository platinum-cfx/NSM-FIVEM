// mysqlwire.go — a minimal MySQL/MariaDB text-protocol client, stdlib only.
//
// Why not a driver dependency: tools/local-stack/go.mod and go.sum are
// v0.7.7-LOCKED paths (docs/nsm-audit-v077) — the dependency surface of the
// launch/entitlement code must not grow. The database provisioner only needs
// the text protocol (COM_QUERY for DDL, COM_PING, COM_QUIT) with
// mysql_native_password auth, which is ~200 lines of well-specified wire
// format (https://dev.mysql.com/doc/dev/mysql-server/latest/Packet.Protocol.html).
//
// The client is verified against the wire-protocol stub in mysqlstub_test.go
// (which real clients — go-sql-driver and HeidiSQL — also speak to), plus a
// known-answer test for the scramble.
package main

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// client capabilities for the handshake response.
const (
	capClientLongPassword     = 0x00000001
	capClientLongFlag         = 0x00000004
	capClientConnectWithDB    = 0x00000008
	capClientProtocol41       = 0x00000200
	capClientTransactions     = 0x00002000
	capClientSecureConnection = 0x00008000
	capClientMultiResults     = 0x00020000
	capClientPluginAuth       = 0x00080000
)

type mysqlClient struct {
	conn net.Conn
	seq  byte
}

// mysqlDSN is a parsed connection string: user:pass@tcp(host:port)/db?opts.
type mysqlDSN struct {
	user, pass, db, addr string
	timeout              time.Duration
}

// parseMySQLDSN understands the exact DSN shape the panel generates and the
// NSM_DB_ADMIN_DSN shape operators set:  user:pass@tcp(host:port)/dbname
// with optional ?timeout=/readTimeout=/writeTimeout= seconds (suffixes s/ms).
func parseMySQLDSN(dsn string) (mysqlDSN, error) {
	var d mysqlDSN
	at := strings.LastIndex(dsn, "@tcp(")
	if at < 0 {
		return d, errors.New("dsn must look like user:pass@tcp(host:port)/db")
	}
	cred := dsn[:at]
	if i := strings.Index(cred, ":"); i >= 0 {
		d.user, d.pass = cred[:i], cred[i+1:]
	} else {
		d.user = cred
	}
	rest := dsn[at+len("@tcp("):]
	end := strings.Index(rest, ")")
	if end < 0 {
		return d, errors.New("dsn: missing ) after host:port")
	}
	d.addr = rest[:end]
	rest = rest[end+1:]
	if !strings.HasPrefix(rest, "/") {
		return d, errors.New("dsn: expected /db after host:port")
	}
	rest = rest[1:]
	if i := strings.Index(rest, "?"); i >= 0 {
		for _, kv := range strings.Split(rest[i+1:], "&") {
			p := strings.SplitN(kv, "=", 2)
			if len(p) != 2 {
				continue
			}
			switch p[0] {
			case "timeout", "readTimeout", "writeTimeout":
				if t := parseDur(p[1]); t > 0 && d.timeout == 0 {
					d.timeout = t
				}
			}
		}
		rest = rest[:i]
	}
	d.db = rest
	if d.addr == "" {
		return d, errors.New("dsn: empty host:port")
	}
	return d, nil
}

func parseDur(s string) time.Duration {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "ms") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "ms")); err == nil {
			return time.Duration(n) * time.Millisecond
		}
		return 0
	}
	s = strings.TrimSuffix(s, "s")
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second
	}
	return 0
}

// mysqlNativePassword computes the mysql_native_password auth response:
// SHA1(pwd) XOR SHA1(salt + SHA1(SHA1(pwd))).
func mysqlNativePassword(password string, salt []byte) []byte {
	if password == "" {
		return nil
	}
	h1 := sha1.Sum([]byte(password))
	h2 := sha1.Sum(h1[:])
	h := sha1.New()
	h.Write(salt)
	h.Write(h2[:])
	scramble := h.Sum(nil)
	for i := range scramble {
		scramble[i] ^= h1[i]
	}
	return scramble
}

// writePkt sends one packet with the running sequence id.
func (c *mysqlClient) writePkt(payload []byte) error {
	var hdr [4]byte
	n := len(payload)
	hdr[0], hdr[1], hdr[2] = byte(n), byte(n>>8), byte(n>>16)
	hdr[3] = c.seq
	c.seq++
	if _, err := c.conn.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

// readPkt reads one packet payload and advances the sequence id.
func (c *mysqlClient) readPkt() ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return nil, err
	}
	c.seq = hdr[3] + 1
	return buf, nil
}

// mysqlDial opens, authenticates and selects the schema.
func mysqlDial(dsn string) (*mysqlClient, error) {
	d, err := parseMySQLDSN(dsn)
	if err != nil {
		return nil, err
	}
	t := d.timeout
	if t == 0 {
		t = 10 * time.Second
	}
	conn, err := net.DialTimeout("tcp", d.addr, t)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(t))
	c := &mysqlClient{conn: conn, seq: 0}

	greet, err := c.readPkt()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mysql greeting: %w", err)
	}
	// protocol 10 greeting: [10][version\0][4 thread][8 salt1][1 0x00]
	// [2 caps-lo][1 charset][2 status][2 caps-hi][1 authLen][10 reserved]
	// [12 salt2][1 0x00][plugin name\0]
	vi := 1
	for vi < len(greet) && greet[vi] != 0 {
		vi++
	}
	base := vi + 1
	if len(greet) < 10 || greet[0] != 10 || base+31+12 > len(greet) {
		conn.Close()
		return nil, errors.New("mysql: unsupported greeting")
	}
	salt := make([]byte, 0, 20)
	salt = append(salt, greet[base+4:base+12]...)  // auth-plugin-data-part-1
	salt = append(salt, greet[base+31:base+43]...) // auth-plugin-data-part-2 (12 bytes)

	caps := uint32(capClientLongPassword | capClientLongFlag | capClientProtocol41 |
		capClientTransactions | capClientSecureConnection | capClientMultiResults |
		capClientPluginAuth)
	var resp []byte
	var cb [4]byte
	binary.LittleEndian.PutUint32(cb[:], caps)
	resp = append(resp, cb[:]...)
	resp = append(resp, 0, 0, 0, 1) // max packet
	resp = append(resp, 33)         // charset utf8_general_ci
	resp = append(resp, make([]byte, 23)...)
	resp = append(resp, d.user...)
	resp = append(resp, 0)
	if d.db != "" {
		caps |= capClientConnectWithDB
		binary.LittleEndian.PutUint32(cb[:], caps)
		copy(resp[:4], cb[:])
	}
	auth := mysqlNativePassword(d.pass, salt)
	resp = append(resp, byte(len(auth)))
	resp = append(resp, auth...)
	if d.db != "" {
		resp = append(resp, d.db...)
		resp = append(resp, 0)
	}
	resp = append(resp, []byte("mysql_native_password\x00")...)
	c.seq = 1
	if err := c.writePkt(resp); err != nil {
		conn.Close()
		return nil, err
	}
	ok, err := c.readPkt()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mysql auth: %w", err)
	}
	if len(ok) > 0 && ok[0] == 0xff {
		conn.Close()
		return nil, errors.New("mysql auth failed: " + string(ok[min(9, len(ok)):]))
	}
	_ = conn.SetDeadline(time.Now().Add(t))
	return c, nil
}

func (c *mysqlClient) close() {
	_ = c.writePkt([]byte{0x01}) // COM_QUIT
	_ = c.conn.Close()
}

// exec runs one text-protocol statement and returns the server error, if any.
func (c *mysqlClient) exec(sql string) error {
	c.seq = 0
	if err := c.writePkt(append([]byte{0x03}, []byte(sql)...)); err != nil {
		return err
	}
	first, err := c.readPkt()
	if err != nil {
		return err
	}
	if len(first) == 0 {
		return errors.New("mysql: empty response")
	}
	switch {
	case first[0] == 0x00: // OK
		return nil
	case first[0] == 0xff: // ERR
		msg := string(first[min(9, len(first)):])
		code := 0
		if len(first) >= 3 {
			code = int(first[1]) | int(first[2])<<8
		}
		return fmt.Errorf("mysql error %d: %s", code, msg)
	case first[0] == 0xfe && len(first) < 9: // EOF
		return nil
	}
	// result set: drain columns + rows until EOF/OK terminator
	return c.drainResult()
}

// drainResult consumes a result set until the trailing EOF/OK packet.
func (c *mysqlClient) drainResult() error {
	for {
		p, err := c.readPkt()
		if err != nil {
			return err
		}
		if len(p) == 0 {
			continue
		}
		if p[0] == 0xff {
			return errors.New("mysql error in result set: " + string(p[min(9, len(p)):]))
		}
		if p[0] == 0xfe && len(p) < 9 {
			return nil // EOF terminator
		}
		if p[0] == 0x00 && len(p) < 9 {
			return nil // OK terminator (DEPRECATE_EOF servers)
		}
	}
}

func (c *mysqlClient) ping() error {
	c.seq = 0
	if err := c.writePkt([]byte{0x0e}); err != nil {
		return err
	}
	p, err := c.readPkt()
	if err != nil {
		return err
	}
	if len(p) > 0 && p[0] == 0xff {
		return errors.New("mysql ping failed")
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
