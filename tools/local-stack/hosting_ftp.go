// hosting_ftp.go — a minimal FTP service for hosted city instances so users
// can manage their server files with FileZilla. Each instance gets its own
// control port from the hosting pool and one passive data port; the session
// is sandboxed to the instance directory. Plain FTP (no TLS) — FileZilla's
// "Plain FTP" site type connects directly.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"sync"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ftpTLSConfig returns the FTPS configuration (explicit TLS: AUTH TLS),
// creating a self-signed certificate on first use. FileZilla accepts it after
// its usual certificate prompt; Plain FTP also keeps working.
var (
	ftpTLSOnce sync.Once
	ftpTLSConf *tls.Config
)

func ftpTLSConfig() *tls.Config {
	ftpTLSOnce.Do(func() {
		cert, err := loadOrCreateFTPCert()
		if err != nil {
			log.Printf("[hosting] FTPS certificate unavailable: %v — Plain FTP only", err)
			return
		}
		ftpTLSConf = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	})
	return ftpTLSConf
}

func loadOrCreateFTPCert() (tls.Certificate, error) {
	const certFile, keyFile = "ftp-tls.crt", "ftp-tls.key"
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		// accept only certificates with a real subject — earlier builds wrote
		// an empty-DN cert that GnuTLS (FileZilla) refuses to parse
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && leaf.Subject.CommonName != "" {
			return c, nil
		}
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	sans := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if h := os.Getenv("NSM_PUBLIC_IP"); h != "" {
		if ip := net.ParseIP(h); ip != nil {
			sans = append(sans, ip)
		}
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   "NSM Hosting FTP",
			Organization: []string{"NSM"},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost", "nsm-hosting"},
		IPAddresses: sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	_ = os.WriteFile(certFile, certPEM, 0o600)
	_ = os.WriteFile(keyFile, keyPEM, 0o600)
	return tls.X509KeyPair(certPEM, keyPEM)
}

type ftpServer struct {
	ln           net.Listener
	port         int
	passivePort  int
	user, pass   string
	root         string
	logf         func(string)
}

// startFTPServer binds the control port and serves connections until Close.
func startFTPServer(port int, passivePort int, user, pass, root string, logf func(string)) (*ftpServer, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	fs := &ftpServer{ln: ln, port: port, passivePort: passivePort, user: user, pass: pass, root: filepath.Clean(root), logf: logf}
	go fs.acceptLoop()
	return fs, nil
}

func (fs *ftpServer) Close() { _ = fs.ln.Close() }

func (fs *ftpServer) acceptLoop() {
	for {
		conn, err := fs.ln.Accept()
		if err != nil {
			return
		}
		go fs.handle(conn)
	}
}

type ftpConn struct {
	fs         *ftpServer
	c          net.Conn
	r          *bufio.Reader
	user       string
	authed     bool
	cwd        string
	passive    net.Listener
	renameFrom string
	rest       int64
	protP      bool // data connections use TLS (PROT P)
}

// upgradeTLS wraps the control connection in TLS (explicit FTPS).
func (fc *ftpConn) upgradeTLS() error {
	conf := ftpTLSConfig()
	if conf == nil {
		return fmt.Errorf("TLS unavailable")
	}
	tconn := tls.Server(fc.c, conf)
	_ = tconn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tconn.Handshake(); err != nil {
		return err
	}
	_ = tconn.SetDeadline(time.Now().Add(10 * time.Minute))
	fc.c = tconn
	fc.r = bufio.NewReader(tconn)
	return nil
}

func (fs *ftpServer) handle(c net.Conn) {
	fc := &ftpConn{fs: fs, c: c, r: bufio.NewReader(c), cwd: "/"}
	defer func() {
		if fc.passive != nil {
			_ = fc.passive.Close()
		}
		_ = c.Close()
	}()
	_ = c.SetDeadline(time.Now().Add(10 * time.Minute))
	fc.reply("220 NSM Hosting FTP ready")
	for {
		_ = c.SetDeadline(time.Now().Add(10 * time.Minute))
		line, err := fc.r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		verb := line
		arg := ""
		if i := strings.IndexByte(line, ' '); i >= 0 {
			verb = line[:i]
			arg = line[i+1:]
		}
		cmd := strings.ToUpper(verb)
		if !fc.authed {
			switch cmd {
			case "AUTH":
				if ftpTLSConfig() != nil && (strings.EqualFold(arg, "TLS") || strings.EqualFold(arg, "SSL")) {
					fc.reply("234 AUTH TLS OK")
					if err := fc.upgradeTLS(); err != nil {
						return
					}
				} else {
					fc.reply("502 AUTH not supported — use Plain FTP in FileZilla")
				}
			case "USER":
				fc.user = arg
				fc.reply("331 password required")
			case "PASS":
				if fc.user == fc.fs.user && arg == fc.fs.pass {
					fc.authed = true
					fc.reply("230 logged in")
					if fc.fs.logf != nil {
						fc.fs.logf("FileZilla session opened for " + fc.user)
					}
				} else {
					fc.reply("530 login incorrect")
				}
			case "QUIT":
				fc.reply("221 goodbye")
				return
			default:
				fc.reply("530 please log in")
			}
			continue
		}
		switch cmd {
		case "USER":
			fc.reply("331 password required")
		case "PASS":
			fc.reply("230 logged in")
		case "QUIT":
			fc.reply("221 goodbye")
			return
		case "NOOP", "CLNT":
			fc.reply("200 ok")
		case "SYST":
			fc.reply("215 UNIX Type: L8")
		case "FEAT":
			fc.raw("211-Features:\r\n UTF8\r\n MLST type*;size*;modify*;\r\n AUTH TLS\r\n PBSZ\r\n PROT\r\n PASV\r\n EPSV\r\n SIZE\r\n MDTM\r\n REST STREAM\r\n211 End\r\n")
		case "OPTS":
			fc.reply("200 ok")
		case "TYPE", "STRU", "MODE":
			fc.reply("200 ok")
		case "PWD", "XPWD":
			fc.reply(fmt.Sprintf("257 \"%s\" is current directory", fc.cwd))
		case "CWD", "XCWD":
			if fc.chdir(arg) {
				fc.reply("250 changed directory")
			} else {
				fc.reply("550 no such directory")
			}
		case "CDUP":
			fc.chdir("..")
			fc.reply("250 changed directory")
		case "PASV":
			if fc.openPassive() {
				ip := fc.pasvIP()
				p := fc.fs.passivePort
				fc.reply(fmt.Sprintf("227 Entering Passive Mode (%d,%d,%d,%d,%d,%d)", ip[0], ip[1], ip[2], ip[3], p/256, p%256))
			} else {
				fc.reply("425 cannot open data connection")
			}
		case "EPSV":
			if fc.openPassive() {
				fc.reply(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", fc.fs.passivePort))
			} else {
				fc.reply("425 cannot open data connection")
			}
		case "LIST":
			fc.sendList(arg, false)
		case "NLST":
			fc.sendList(arg, true)
		case "MLSD":
			fc.sendList(arg, false)
		case "MLST":
			name := filepath.Base(strings.Trim(fc.cwd, "/"))
			fc.raw(fmt.Sprintf("250-Listing %s\r\n type=file;size=0; %s\r\n250 End\r\n", name, name))
		case "RETR":
			fc.sendFile(arg)
		case "STOR":
			fc.recvFile(arg, false)
		case "APPE":
			fc.recvFile(arg, true)
		case "DELE":
			if p, ok := fc.resolve(arg); ok && fileExists(p) && os.Remove(p) == nil {
				fc.reply("250 deleted")
			} else {
				fc.reply("550 cannot delete")
			}
		case "MKD", "XMKD":
			if p, ok := fc.resolve(arg); ok && os.MkdirAll(p, 0o755) == nil {
				fc.reply(fmt.Sprintf("257 \"%s\" created", arg))
			} else {
				fc.reply("550 cannot create")
			}
		case "RMD", "XRMD":
			if p, ok := fc.resolve(arg); ok && os.Remove(p) == nil {
				fc.reply("250 removed")
			} else {
				fc.reply("550 cannot remove")
			}
		case "RNFR":
			if p, ok := fc.resolve(arg); ok && fileExists(p) {
				fc.renameFrom = p
				fc.reply("350 ready for RNTO")
			} else {
				fc.reply("550 not found")
			}
		case "RNTO":
			if fc.renameFrom != "" {
				if p, ok := fc.resolve(arg); ok && os.Rename(fc.renameFrom, p) == nil {
					fc.reply("250 renamed")
				} else {
					fc.reply("550 rename failed")
				}
				fc.renameFrom = ""
			} else {
				fc.reply("503 RNFR first")
			}
		case "SIZE":
			if p, ok := fc.resolve(arg); ok {
				if st, err := os.Stat(p); err == nil && !st.IsDir() {
					fc.reply(fmt.Sprintf("213 %d", st.Size()))
					continue
				}
			}
			fc.reply("550 not a file")
		case "MDTM":
			if p, ok := fc.resolve(arg); ok {
				if st, err := os.Stat(p); err == nil {
					fc.reply("213 " + st.ModTime().UTC().Format("20060102150405"))
					continue
				}
			}
			fc.reply("550 not found")
		case "REST":
			fc.rest, _ = strconv.ParseInt(arg, 10, 64)
			fc.reply("350 restarting at " + arg)
		case "AUTH":
			if ftpTLSConfig() != nil && (strings.EqualFold(arg, "TLS") || strings.EqualFold(arg, "SSL")) {
				fc.reply("234 AUTH TLS OK")
				if err := fc.upgradeTLS(); err != nil {
					return
				}
			} else {
				fc.reply("502 AUTH not supported — use Plain FTP in FileZilla")
			}
		case "PBSZ":
			fc.reply("200 OK")
		case "PROT":
			if strings.EqualFold(arg, "P") {
				fc.protP = true
				fc.reply("200 data connections will use TLS")
			} else if strings.EqualFold(arg, "C") {
				fc.protP = false
				fc.reply("200 data connections are clear")
			} else {
				fc.reply("504 unsupported protection level")
			}
		default:
			fc.reply("502 command not implemented")
		}
	}
}

func (fc *ftpConn) reply(s string)  { fc.raw(s + "\r\n") }
func (fc *ftpConn) raw(s string)    { _, _ = io.WriteString(fc.c, s) }

func (fc *ftpConn) resolve(rel string) (string, bool) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	base := filepath.Join(fc.fs.root, filepath.FromSlash(strings.Trim(fc.cwd, "/")))
	full := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
	root := fc.fs.root
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

func (fc *ftpConn) chdir(rel string) bool {
	p, ok := fc.resolve(rel)
	if !ok {
		return false
	}
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return false
	}
	rel2 := strings.Trim(strings.TrimPrefix(p, fc.fs.root), string(os.PathSeparator))
	fc.cwd = "/" + filepath.ToSlash(rel2)
	if fc.cwd == "/" {
		fc.cwd = "/"
	}
	return true
}

func (fc *ftpConn) pasvIP() [4]byte {
	var out [4]byte
	host, _, err := net.SplitHostPort(fc.c.LocalAddr().String())
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				copy(out[:], v4)
				return out
			}
		}
	}
	out = [4]byte{127, 0, 0, 1}
	return out
}

func (fc *ftpConn) openPassive() bool {
	if fc.passive != nil {
		_ = fc.passive.Close()
		fc.passive = nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", fc.fs.passivePort))
	if err != nil {
		return false
	}
	fc.passive = ln
	return true
}

func (fc *ftpConn) acceptData() net.Conn {
	if fc.passive == nil {
		return nil
	}
	ln := fc.passive
	fc.passive = nil
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Second))
	c, err := ln.Accept()
	_ = ln.Close()
	if err != nil {
		return nil
	}
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	return c
}

// wrapData upgrades a data connection to TLS (PROT P). It must run AFTER the
// 150 reply — clients start their side of the handshake only after that.
func (fc *ftpConn) wrapData(d net.Conn) net.Conn {
	if !fc.protP {
		return d
	}
	conf := ftpTLSConfig()
	if conf == nil {
		return d
	}
	tconn := tls.Server(d, conf)
	_ = tconn.SetDeadline(time.Now().Add(2 * time.Minute))
	if err := tconn.Handshake(); err != nil {
		_ = d.Close()
		return nil
	}
	return tconn
}

func (fc *ftpConn) sendList(arg string, namesOnly bool) {
	p, ok := fc.resolve(arg)
	if !ok {
		fc.reply("550 bad path")
		return
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		fc.reply("550 cannot list")
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	d := fc.acceptData()
	if d == nil {
		fc.reply("425 cannot open data connection")
		return
	}
	fc.reply("150 opening data connection")
	if d = fc.wrapData(d); d == nil {
		fc.reply("425 data TLS handshake failed")
		return
	}
	defer d.Close()
	var sb strings.Builder
	for _, e := range entries {
		if namesOnly {
			sb.WriteString(e.Name() + "\r\n")
			continue
		}
		info, _ := e.Info()
		var size int64
		var mt = time.Now()
		if info != nil {
			size = info.Size()
			mt = info.ModTime()
		}
		typ := "file"
		if e.IsDir() {
			typ = "dir"
		}
		sb.WriteString(fmt.Sprintf("type=%s;size=%d;modify=%s; %s\r\n", typ, size, mt.UTC().Format("20060102150405"), e.Name()))
	}
	_, _ = io.WriteString(d, sb.String())
	fc.reply("226 transfer complete")
}

func (fc *ftpConn) sendFile(arg string) {
	p, ok := fc.resolve(arg)
	if !ok {
		fc.reply("550 bad path")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fc.reply("550 cannot open file")
		return
	}
	defer f.Close()
	if fc.rest > 0 {
		_, _ = f.Seek(fc.rest, io.SeekStart)
		fc.rest = 0
	}
	d := fc.acceptData()
	if d == nil {
		fc.reply("425 cannot open data connection")
		return
	}
	fc.reply("150 opening data connection")
	if d = fc.wrapData(d); d == nil {
		fc.reply("425 data TLS handshake failed")
		return
	}
	defer d.Close()
	_, _ = io.Copy(d, f)
	fc.reply("226 transfer complete")
}

func (fc *ftpConn) recvFile(arg string, appendMode bool) {
	p, ok := fc.resolve(arg)
	if !ok {
		fc.reply("550 bad path")
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(p, flag, 0o600)
	if err != nil {
		fc.reply("550 cannot write file")
		return
	}
	defer f.Close()
	d := fc.acceptData()
	if d == nil {
		fc.reply("425 cannot open data connection")
		return
	}
	fc.reply("150 opening data connection")
	if d = fc.wrapData(d); d == nil {
		fc.reply("425 data TLS handshake failed")
		return
	}
	defer d.Close()
	_, _ = io.Copy(f, d)
	fc.reply("226 transfer complete")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
