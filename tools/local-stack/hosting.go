// hosting.go — NSM Hosting: the game-server hosting department of the NSM
// website. One standard package, one server per NSM account, fully automatic
// provisioning on this machine (the VPS):
//
//	official nsm-server files -> isolated city directory -> unique TCP/UDP
//	ports from the 500-port pool -> generated server.cfg -> registered in the
//	store -> managed from /dashboard/<id> (console, overview, files, ports).
//
// Every status, metric and file operation reflects the real machine: the
// process is really spawned, console lines are its real output, CPU/RAM come
// from the OS, disk usage is walked from the city directory, and ports are
// tracked in hosting.json (the store) so they can never collide.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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

// ---------------------------------------------------------------------------
// package + model
// ---------------------------------------------------------------------------

const (
	hostingPortFirst = 30100
	hostingPortLast  = 30599 // inclusive — exactly 500 ports
	hostingDirEnv    = "NSM_HOSTING_DIR"
	serverExeEnv     = "NSM_SERVER_EXE"
)

// HostingPackage is the single standard NSM Hosting package. Every customer
// receives exactly this; there are no tiers.
type HostingPackage struct {
	Name     string
	Slots    int
	RAMGB    int
	DiskGB   int
	Ports    int
	Blurb    string
}

var hostingPackage = HostingPackage{
	Name:   "NSM Standard",
	Slots:  2048,
	RAMGB:  8,
	DiskGB: 50,
	Ports:  5,
	Blurb:  "The one and only NSM hosting package — full-power cities for everyone.",
}

// Hosting status values (provisioning lifecycle).
const (
	hostPending    = "pending"
	hostInstalling = "installing"
	hostStarting   = "starting"
	hostOnline     = "online"
	hostStopping   = "stopping"
	hostOffline    = "offline"
	hostError      = "error"
)

// HostedServer is one customer city.
type HostedServer struct {
	KeyID  string `json:"keyId,omitempty"`  // keymaster server key id (auto-issued)
	KeyRaw string `json:"keyRaw,omitempty"` // raw key written into server.cfg (owner-only)
	ID          string   `json:"id"`
	Owner       int64    `json:"owner"`
	OwnerName   string   `json:"ownerName"`
	Name        string   `json:"name"`
	Region      string   `json:"region"`
	GameTCPPort int      `json:"gameTcpPort"`
	GameUDPPort int      `json:"gameUdpPort"`
	FTPPort     int      `json:"ftpPort"`
	PassivePort int      `json:"passivePort"`
	ExtraPorts  []int    `json:"extraPorts"`
	Endpoint    string   `json:"endpoint"`
	JoinCode    string   `json:"joinCode"`
	FTPUser     string   `json:"ftpUser"`
	FTPPass     string   `json:"ftpPass"`
	Status      string   `json:"status"`
	Note        string   `json:"note,omitempty"`
	ExeRel      string   `json:"exeRel,omitempty"`
	CreatedAt   int64    `json:"createdAt"`
	LastStarted int64    `json:"lastStarted,omitempty"`
	Console     []string `json:"console,omitempty"`
	ConsoleSeq  int64    `json:"consoleSeq"`
}

type hostingState struct {
	Servers []*HostedServer `json:"servers"`
}

var errAlreadyOwns = errors.New("this NSM account already owns a server")

type hostedProc struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	done     chan struct{}
	lastCPU  float64
	lastTime time.Time
}

type hostingManager struct {
	mu       sync.Mutex
	st       hostingState
	path     string
	root     string
	procs    map[string]*hostedProc
	ftp      map[string]*ftpServer
	firewall      bool
	firewallNote  string
	diskMemo map[string]diskMemo
}

type diskMemo struct {
	mb  int64
	at  time.Time
}

var (
	hostOnce sync.Once
	hostMgrV *hostingManager
)

func hostMgr() *hostingManager {
	hostOnce.Do(func() {
		root := os.Getenv(hostingDirEnv)
		if root == "" {
			root = filepath.Join(".", "hosted")
		}
		m := &hostingManager{
			path:     "hosting.json",
			root:     root,
			procs:    map[string]*hostedProc{},
			ftp:      map[string]*ftpServer{},
			diskMemo: map[string]diskMemo{},
		}
		hostMgrV = m
		if b, err := os.ReadFile(m.path); err == nil {
			_ = json.Unmarshal(b, &m.st)
		}
		m.normalizeLocked()
		_ = os.MkdirAll(m.root, 0o755)
		for _, s := range m.st.Servers {
			if s.FTPPort > 0 {
				m.startFTP(s)
			}
			if s.Status == hostOnline || s.Status == hostStarting {
				s.Status = hostOffline
				m.logNL(s, "[system] panel restarted — server is offline, press Start")
			}
		}
		m.saveLocked()
	})
	return hostMgrV
}

// normalizeLocked enforces the store-level invariants: one server per
// account (keep the oldest) and well-formed records.
func (m *hostingManager) normalizeLocked() {
	seen := map[int64]bool{}
	kept := m.st.Servers[:0]
	for _, s := range m.st.Servers {
		if s.Owner == 0 || seen[s.Owner] {
			continue
		}
		seen[s.Owner] = true
		if s.Status == "" {
			s.Status = hostOffline
		}
		kept = append(kept, s)
	}
	m.st.Servers = kept
}

func (m *hostingManager) saveLocked() {
	b, _ := json.MarshalIndent(&m.st, "", "  ")
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.path)
	}
}

// stripANSI removes terminal color codes so the web console stays readable
// (the text itself is preserved exactly).
func stripANSI(line string) string {
	var b strings.Builder
	i := 0
	for i < len(line) {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) {
				c := line[j]
				j++
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
					break
				}
			}
			i = j
			continue
		}
		b.WriteByte(line[i])
		i++
	}
	return strings.TrimRight(b.String(), "\r")
}

// logNL appends a console line. Caller must hold m.mu.
func (m *hostingManager) logNL(s *HostedServer, line string) {
	line = stripANSI(line)
	s.ConsoleSeq++
	s.Console = append(s.Console, fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line))
	if len(s.Console) > 500 {
		s.Console = s.Console[len(s.Console)-500:]
	}
}

// appendLog is the locking variant of logNL.
func (m *hostingManager) appendLog(s *HostedServer, line string) {
	m.mu.Lock()
	if cur := m.find(s.ID); cur != nil {
		m.logNL(cur, line)
		m.saveLocked()
	}
	m.mu.Unlock()
}

func (m *hostingManager) setStatus(s *HostedServer, status string) {
	m.mu.Lock()
	if cur := m.find(s.ID); cur != nil {
		cur.Status = status
		m.saveLocked()
	}
	m.mu.Unlock()
}

func (m *hostingManager) find(id string) *HostedServer {
	for _, s := range m.st.Servers {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (m *hostingManager) findOwnerLocked(owner int64) *HostedServer {
	for _, s := range m.st.Servers {
		if s.Owner == owner {
			return s
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ports — the 500-port pool, reserved in the store
// ---------------------------------------------------------------------------

func (m *hostingManager) portsInUseLocked() map[int]string {
	used := map[int]string{}
	for _, s := range m.st.Servers {
		if s.GameTCPPort > 0 {
			used[s.GameTCPPort] = s.Name + " (game TCP)"
		}
		if s.GameUDPPort > 0 {
			used[s.GameUDPPort] = s.Name + " (game UDP)"
		}
		if s.FTPPort > 0 {
			used[s.FTPPort] = s.Name + " (files)"
		}
		if s.PassivePort > 0 {
			used[s.PassivePort] = s.Name + " (files data)"
		}
		for _, p := range s.ExtraPorts {
			used[p] = s.Name + " (extra)"
		}
	}
	return used
}

// allocPortLocked reserves the lowest port that is free in the store AND
// actually bindable on this machine (other services on the VPS may hold
// ports inside the range). Ports are released only when a server is
// permanently deleted — never when it is stopped.
func (m *hostingManager) allocPortLocked() (int, error) {
	used := m.portsInUseLocked()
	for p := hostingPortFirst; p <= hostingPortLast; p++ {
		if _, busy := used[p]; busy {
			continue
		}
		if !portBindable(p) {
			log.Printf("[hosting] port %d is held by another process on this machine — skipping it", p)
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("the hosting port pool (%d-%d) is fully allocated", hostingPortFirst, hostingPortLast)
}

// portBindable reports whether TCP and UDP on p are free right now.
func portBindable(p int) bool {
	addr := fmt.Sprintf(":%d", p)
	tl, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = tl.Close()
	ul, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	_ = ul.Close()
	return true
}

// killInstanceProcs terminates leftover processes that were started from this
// city's directory (orphans from a previous panel run). Only processes whose
// command line contains the city's own path are touched.
func killInstanceProcs(instDir string) int {
	abs, err := filepath.Abs(instDir)
	if err != nil {
		return 0
	}
	killed := 0
	if runtime.GOOS == "linux" {
		procs, _ := os.ReadDir("/proc")
		for _, pe := range procs {
			pid, err := strconv.Atoi(pe.Name())
			if err != nil {
				continue
			}
			b, err := os.ReadFile("/proc/" + pe.Name() + "/cmdline")
			if err != nil {
				continue
			}
			if strings.Contains(string(b), abs) {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
					killed++
				}
			}
		}
		return killed
	}
	out, err := exec.Command("wmic", "process", "get", "ProcessId,CommandLine", "/format:csv").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, abs) {
			continue
		}
		parts := strings.SplitN(strings.TrimSpace(line), ",", 3)
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
			killed++
		}
	}
	return killed
}

var errPortsBusy = errors.New("assigned ports are held by another process on this machine")

// hostingFirewallOnce opens the hosting port range (TCP and UDP) in the VPS
// firewall so players can reach hosted cities. Failure is logged, never
// hidden — the operator can open the range manually.
func (m *hostingManager) hostingFirewallOnce() {
	if m.firewall {
		return
	}
	m.firewall = true
	rangeSpec := fmt.Sprintf("%d-%d", hostingPortFirst, hostingPortLast)
	manual := "allow TCP and UDP " + rangeSpec + " through the Windows firewall AND your provider's firewall (VPS control panel), otherwise players see \"Failed to get info from server\" / connection timeouts"
	if netsh := findWindowsTool("netsh.exe"); netsh != "" {
		ok := true
		for _, proto := range []string{"TCP", "UDP"} {
			args := []string{"advfirewall", "firewall", "add", "rule",
				"name=NSM Hosting " + proto + " " + rangeSpec,
				"dir=in", "action=allow", "protocol=" + proto, "localport=" + rangeSpec}
			out, err := exec.Command(netsh, args...).CombinedOutput()
			if err != nil {
				ok = false
				log.Printf("[hosting] firewall %s rule failed (%v): %s", proto, err, strings.TrimSpace(string(out)))
			}
		}
		if ok {
			m.firewallNote = "this machine's firewall allows TCP and UDP " + rangeSpec + " for hosted cities. If players still cannot connect, also allow the range in your provider's firewall (VPS control panel)."
			log.Printf("[hosting] firewall: allowed TCP and UDP %s (%d ports) for hosted cities", rangeSpec, hostingPortLast-hostingPortFirst+1)
			return
		}
	}
	if ps := findWindowsTool("powershell.exe"); ps != "" {
		ok := true
		for _, proto := range []string{"TCP", "UDP"} {
			rule := fmt.Sprintf("New-NetFirewallRule -DisplayName 'NSM Hosting %s %s' -Direction Inbound -Action Allow -Protocol %s -LocalPort %s", proto, rangeSpec, proto, rangeSpec)
			out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", rule).CombinedOutput()
			if err != nil {
				ok = false
				log.Printf("[hosting] firewall %s rule via PowerShell failed (%v): %s", proto, err, strings.TrimSpace(string(out)))
			}
		}
		if ok {
			m.firewallNote = "this machine's firewall allows TCP and UDP " + rangeSpec + " for hosted cities (PowerShell rule). If players still cannot connect, also allow the range in your provider's firewall (VPS control panel)."
			log.Printf("[hosting] firewall: allowed TCP and UDP %s via PowerShell", rangeSpec)
			return
		}
	}
	m.firewallNote = "could not open the firewall automatically - " + manual
	log.Printf("[hosting] firewall: %s", m.firewallNote)
}

// findWindowsTool locates a system tool even when the service PATH is minimal.
func findWindowsTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, base := range []string{os.Getenv("SystemRoot"), `C:\Windows`} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, "System32", name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// provisioning
// ---------------------------------------------------------------------------

var errBinaryMissing = errors.New("server binary missing")

func (m *hostingManager) dirOf(s *HostedServer) string {
	return filepath.Join(m.root, s.ID)
}

// serverRoot is the folder holding the game server itself (server.cfg,
// resources/, the server exe). It is the root users see over FTP and in the
// Files tab - panel-internal files (ftppass.txt and the config mirror) live
// one level up, outside it.
func (m *hostingManager) serverRoot(s *HostedServer) string {
	if s.ExeRel == "" {
		return m.dirOf(s)
	}
	return filepath.Dir(filepath.Join(m.dirOf(s), s.ExeRel))
}

// deploy provisions a new city for the signed-in user. It is safe against
// double-clicks and simultaneous requests: the one-server-per-account check
// and the record creation happen atomically under the manager lock.
func (m *hostingManager) deploy(u *User, name, region, reqHost string) (*HostedServer, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = u.Username + "-city"
	}
	if region == "" {
		region = "us-east"
	}

	// ---- Phase A: atomic claim (one server per account, no duplicates) ----
	m.mu.Lock()
	if existing := m.findOwnerLocked(u.ID); existing != nil {
		m.mu.Unlock()
		return existing, errAlreadyOwns
	}
	m.hostingFirewallOnce()
	s := &HostedServer{
		ID:        fmt.Sprintf("h%d", time.Now().UnixNano()%1e9),
		Owner:     u.ID,
		OwnerName: u.Username,
		Name:      name,
		Region:    region,
		CreatedAt: time.Now().Unix(),
		Status:    hostPending,
		FTPUser:   sanitizeFTPUser(u.Username),
		FTPPass:   hostRandToken(14),
	}
	// register first so each allocation below sees the previous ones
	m.st.Servers = append(m.st.Servers, s)
	// the city's directory exists from this moment (files + FTP usable even
	// while the server software is still installing)
	_ = os.MkdirAll(m.dirOf(s), 0o755)
	var err error
	if s.GameTCPPort, err = m.allocPortLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if s.GameUDPPort, err = m.allocPortLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if s.FTPPort, err = m.allocPortLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if s.PassivePort, err = m.allocPortLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// the 5th package port goes to the city itself as an extra service port
	if p, err := m.allocPortLocked(); err == nil {
		s.ExtraPorts = append(s.ExtraPorts, p)
	}
	host := os.Getenv("NSM_PUBLIC_IP")
	if host == "" {
		host = reqHost
		if h, _, err := net.SplitHostPort(reqHost); err == nil {
			host = h
		}
	}
	if host == "" || host == "localhost" {
		host = "127.0.0.1"
	}
	s.Endpoint = fmt.Sprintf("%s:%d", host, s.GameUDPPort)
	s.JoinCode = joinCodeForEndpoint(s.Endpoint)
	m.logNL(s, "[system] provisioning city '"+name+"' ("+hostingPackage.Name+")")
	m.logNL(s, "[system] network: "+m.firewallNote)
	m.logNL(s, fmt.Sprintf("[system] ports reserved: game TCP %d, game UDP %d, files %d-%d, extra %v", s.GameTCPPort, s.GameUDPPort, s.FTPPort, s.PassivePort, s.ExtraPorts))
	m.logNL(s, "[system] status: pending")
	m.saveLocked()
	m.startFTP(s)
	m.mu.Unlock()

	// ---- Phase B: install the official server files (slow, no lock held) ----
	if err := m.install(s); err != nil {
		m.mu.Lock()
		if cur := m.find(s.ID); cur != nil {
			cur.Status = hostError
			cur.Note = "install failed: " + err.Error()
			m.logNL(cur, "[error] "+cur.Note)
			m.logNL(cur, "[system] press Start to retry the installation")
			m.saveLocked()
			s = cur
		}
		m.mu.Unlock()
		return s, err
	}

	// ---- Phase C: start it for real ----
	m.mu.Lock()
	if cur := m.find(s.ID); cur != nil {
		s = cur
	}
	m.mu.Unlock()
	if err := m.start(s); err != nil {
		return s, err
	}
	return s, nil
}

// ensureServerKey issues this hosted server's keymaster key on first install
// so the game server can authenticate its heartbeat. One active key per
// account: issuing revokes prior keys (see issueServerKey).
func (m *hostingManager) ensureServerKey(s *HostedServer) {
	if s.KeyRaw != "" {
		return
	}
	own := userByID(s.Owner)
	if own == nil {
		return
	}
	raw, rec, err := issueServerKey(own, "Hosted: "+s.Name, "", 0)
	if err != nil {
		return
	}
	s.KeyID, s.KeyRaw = rec.ID, raw
	m.mu.Lock()
	m.saveLocked()
	m.mu.Unlock()
}

// healCfg upgrades configs written by older panel versions in place: sv_lan 1
// (stock auth tickets off - players were rejected with "No authentication
// ticket was specified") and the server's own keymaster key for heartbeats.
// Never rewrites user content, only appends what is missing.
func (m *hostingManager) healCfg(s *HostedServer) {
	p := filepath.Join(m.serverRoot(s), "server.cfg")
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	text := string(b)
	add := ""
	if !strings.Contains(text, "sv_lan") {
		add += "\n# added by the NSM Hosting panel: accept players without stock auth tickets\nsv_lan 1\n"
	}
	if s.KeyRaw != "" && !strings.Contains(text, "sv_nsmServerKey \""+s.KeyRaw+"\"") {
		add += "sets sv_nsmServerKey \"" + s.KeyRaw + "\"\n"
	}
	if add == "" {
		return
	}
	if err := os.WriteFile(p, []byte(text+add), 0o600); err == nil {
		m.appendLog(s, "[system] server.cfg updated with connection settings (sv_lan / server key)")
	}
}

// hostingPublicHostForPort maps a game port to the public host recorded for
// the hosted server that owns it, so heartbeats arriving over loopback still
// publish a reachable endpoint.
func hostingPublicHostForPort(port int) string {
	m := hostMgrV
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, srv := range m.st.Servers {
		if srv.GameUDPPort == port || srv.GameTCPPort == port {
			return publicHostOf(srv)
		}
	}
	return ""
}

// joinBaseURL is the site origin a shareable join link should carry.
func joinBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xp := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); xp == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// install downloads/locates the official server files and copies them into
// the city's isolated directory, verifying the result.
func (m *hostingManager) install(s *HostedServer) error {
	m.setStatus(s, hostInstalling)
	// issue this server's keymaster key first so the generated config can
	// carry it even when file installation fails and is retried later
	m.ensureServerKey(s)
	m.appendLog(s, "[system] status: installing")
	m.appendLog(s, "[system] installing official NSM server files")

	templateDir, err := ensureServerFiles(func(line string) { m.appendLog(s, line) })
	if err != nil {
		return err
	}
	count, relExe, err := installInstance(templateDir, m.dirOf(s))
	if err != nil {
		return err
	}
	s.ExeRel = relExe
	m.mu.Lock()
	if cur := m.find(s.ID); cur != nil {
		cur.ExeRel = relExe
		m.logNL(cur, fmt.Sprintf("[system] installed %d files into this server's directory", count))
		m.logNL(cur, "[system] install verified: "+relExe)
		m.saveLocked()
	}
	m.mu.Unlock()

	// server.cfg with the REAL allocated ports, written into the server's own
	// directory (where the binary looks for it) and mirrored at instance root
	serverRoot := m.serverRoot(s)
	cfg := m.generateCfg(s, serverRoot)
	if err := os.WriteFile(filepath.Join(serverRoot, "server.cfg"), []byte(cfg), 0o600); err != nil {
		return err
	}
	if serverRoot != m.dirOf(s) {
		_ = os.WriteFile(filepath.Join(m.dirOf(s), "server.cfg"), []byte(cfg), 0o600)
	}
	m.appendLog(s, "[system] server.cfg generated with this server's ports (TCP "+itoa(s.GameTCPPort)+", UDP "+itoa(s.GameUDPPort)+")")
	m.appendLog(s, "[system] config installed at server.cfg (your server folder root over FTP)")
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }

// discoverResources lists the resource names actually shipped in the city's
// resources/ directory (resources/<name> and resources/[cat]/<name>).
func discoverResources(instDir string) []string {
	names := map[string]bool{}
	base := filepath.Join(instDir, "resources")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "[") {
			sub, err := os.ReadDir(filepath.Join(base, e.Name()))
			if err != nil {
				continue
			}
			for _, se := range sub {
				if se.IsDir() {
					names[se.Name()] = true
				}
			}
			continue
		}
		names[e.Name()] = true
	}
	out := []string{}
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (m *hostingManager) generateCfg(s *HostedServer, instDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — generated by the NSM Hosting panel\n", s.Name)
	fmt.Fprintf(&b, "# package: %s (%d slots, %d GB RAM allocation)\n", hostingPackage.Name, hostingPackage.Slots, hostingPackage.RAMGB)
	fmt.Fprintf(&b, "endpoint_add_tcp \"0.0.0.0:%d\"\n", s.GameTCPPort)
	fmt.Fprintf(&b, "endpoint_add_udp \"0.0.0.0:%d\"\n", s.GameUDPPort)
	fmt.Fprintf(&b, "sv_hostname \"%s\"\n", strings.ReplaceAll(s.Name, `"`, ""))
	fmt.Fprintf(&b, "sv_maxclients %d\n", hostingPackage.Slots)
	fmt.Fprintf(&b, "sets sv_region \"%s\"\n", s.Region)
	b.WriteString("sets sv_nsmHosted \"1\"\n")
	// sv_lan 1 turns off the stock auth-ticket requirement - our stack runs
	// ticketless and the server impl keeps internet play working in lan mode.
	// Without it players are rejected with "No authentication ticket was specified".
	b.WriteString("sv_lan 1\n")
	if s.KeyRaw != "" {
		fmt.Fprintf(&b, "sets sv_nsmServerKey %q\n", s.KeyRaw)
	} else {
		b.WriteString("# paste the server key from AFX Portal (shown only when you create it):\n")
		b.WriteString("# sets sv_nsmServerKey \"nsm_srv_...\"\n")
	}
	// only ensure resources that really exist in this server package
	res := discoverResources(instDir)
	if len(res) == 0 {
		b.WriteString("# no resources found in resources/ yet — add yours and list them below\n")
	}
	// sessionmanager first when present (the server impl looks for it)
	for _, name := range res {
		if name == "sessionmanager" {
			fmt.Fprintf(&b, "ensure %s\n", name)
		}
	}
	for _, name := range res {
		if name != "sessionmanager" {
			fmt.Fprintf(&b, "ensure %s\n", name)
		}
	}
	b.WriteString("# add your resources under resources/ and start them here\n")
	return b.String()
}

func sanitizeFTPUser(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "nsmuser"
	}
	return b.String()
}

func hostRandToken(n int) string {
	const abc = "abcdefghjkmnpqrstuvwxyz23456789"
	rb := make([]byte, n)
	_, _ = rand.Read(rb)
	b := make([]byte, n)
	for i := range b {
		b[i] = abc[int(rb[i])%len(abc)]
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// process control — real NSM-Server processes, real output
// ---------------------------------------------------------------------------

func serverExePath(dir, rel string) string {
	if rel != "" {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if exe := findServerExe(dir); exe != "" {
		return exe
	}
	return ""
}

func (m *hostingManager) start(s *HostedServer) error {
	m.mu.Lock()
	if p, ok := m.procs[s.ID]; ok {
		select {
		case <-p.done:
		default:
			m.logNL(s, "[system] already running")
			m.saveLocked()
			m.mu.Unlock()
			return nil
		}
	}
	dir := m.dirOf(s)
	s.Status = hostStarting
	s.Note = ""
	m.logNL(s, "[system] status: starting")
	m.saveLocked()
	m.mu.Unlock()

	// ensure the server files are really installed (also the retry path)
	exe := serverExePath(dir, s.ExeRel)
	if exe == "" {
		if err := m.install(s); err != nil {
			m.setStatus(s, hostError)
			m.appendLog(s, "[error] server files unavailable: "+err.Error())
			return errBinaryMissing
		}
		exe = serverExePath(dir, "")
	}
	if exe == "" {
		m.setStatus(s, hostError)
		m.appendLog(s, "[error] "+errBinaryMissing.Error())
		return errBinaryMissing
	}

	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	// preflight: the assigned ports must be free before we boot, otherwise the
	// server reports EADDRINUSE. If a leftover process from a previous panel
	// run is holding them, reap it (only processes from THIS city's directory).
	if !portBindable(s.GameTCPPort) || !portBindable(s.GameUDPPort) {
		reaped := killInstanceProcs(dir)
		if reaped > 0 {
			m.appendLog(s, fmt.Sprintf("[system] stopped %d leftover server process(es) from an earlier run", reaped))
		}
		if !portBindable(s.GameTCPPort) || !portBindable(s.GameUDPPort) {
			m.setStatus(s, hostError)
			m.appendLog(s, fmt.Sprintf("[error] ports %d/%d are held by another process on this machine — free them or delete and re-create the server to get new ports", s.GameTCPPort, s.GameUDPPort))
			return errPortsBusy
		}
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	m.healCfg(s)
	cmd := exec.Command(exe, "+exec", "server.cfg")
	cmd.Dir = filepath.Dir(exe)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		m.setStatus(s, hostError)
		m.appendLog(s, "[error] failed to start server process: "+err.Error())
		return err
	}
	m.mu.Lock()
	p := &hostedProc{cmd: cmd, in: stdin, done: make(chan struct{}), lastTime: time.Now()}
	m.procs[s.ID] = p
	if cur := m.find(s.ID); cur != nil {
		cur.Status = hostOnline
		cur.LastStarted = time.Now().Unix()
		m.logNL(cur, "[system] status: online (pid "+strconv.Itoa(cmd.Process.Pid)+")")
		m.saveLocked()
	}
	m.mu.Unlock()
	go m.pump(s.ID, stdout)
	go m.pump(s.ID, stderr)
	go func() {
		_ = cmd.Wait()
		close(p.done)
		m.mu.Lock()
		if cur, ok := m.procs[s.ID]; ok && cur == p {
			delete(m.procs, s.ID)
			if s2 := m.find(s.ID); s2 != nil && (s2.Status == hostOnline || s2.Status == hostStarting) {
				s2.Status = hostOffline
				m.logNL(s2, "[system] status: offline (server process exited)")
				m.saveLocked()
			}
		}
		m.mu.Unlock()
	}()
	return nil
}

func (m *hostingManager) pump(id string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 256<<10)
	for sc.Scan() {
		line := sc.Text()
		m.mu.Lock()
		if s := m.find(id); s != nil {
			m.logNL(s, line)
			m.saveLocked()
		}
		m.mu.Unlock()
	}
}

func (m *hostingManager) stop(s *HostedServer) {
	m.mu.Lock()
	s2 := m.find(s.ID)
	if s2 == nil {
		m.mu.Unlock()
		return
	}
	s2.Status = hostStopping
	p, ok := m.procs[s.ID]
	m.saveLocked()
	m.mu.Unlock()

	if !ok {
		m.setStatus(s, hostOffline)
		m.appendLog(s, "[system] status: offline")
		return
	}
	_, _ = io.WriteString(p.in, "quit\n")
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		_ = p.cmd.Process.Kill()
	}
	m.setStatus(s, hostOffline)
	m.appendLog(s, "[system] status: offline (stopped)")
}

func (m *hostingManager) command(s *HostedServer, cmdText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[s.ID]
	if !ok {
		m.logNL(s, "[error] command ignored — server is not running")
		m.saveLocked()
		return fmt.Errorf("server is not running")
	}
	_, err := io.WriteString(p.in, cmdText+"\n")
	m.logNL(s, "> "+cmdText)
	m.saveLocked()
	return err
}

// delete removes the city permanently: process, files, FTP endpoint and all
// of its reserved ports.
func (m *hostingManager) delete(s *HostedServer) {
	m.stop(s)
	m.mu.Lock()
	defer m.mu.Unlock()
	if fs, ok := m.ftp[s.ID]; ok {
		fs.Close()
		delete(m.ftp, s.ID)
	}
	_ = os.RemoveAll(m.dirOf(s))
	kept := m.st.Servers[:0]
	for _, cur := range m.st.Servers {
		if cur.ID != s.ID {
			kept = append(kept, cur)
		}
	}
	m.st.Servers = kept
	m.saveLocked()
	log.Printf("[hosting] city %s (%s) deleted — its ports were released", s.Name, s.ID)
}

// ---------------------------------------------------------------------------
// real metrics (CPU/RAM from the OS, disk walked from the city directory)
// ---------------------------------------------------------------------------

type hostUsage struct {
	Online    bool  `json:"online"`
	CPUPct    float64 `json:"cpu"` // -1 when the OS will not report it
	RAMMB     int64 `json:"ramMB"` // -1 when the OS will not report it
	DiskMB    int64 `json:"diskMB"`
	UptimeSec int64 `json:"uptimeSec"`
}

func (m *hostingManager) usage(s *HostedServer) hostUsage {
	u := hostUsage{CPUPct: -1, RAMMB: -1}
	m.mu.Lock()
	p, running := m.procs[s.ID]
	status := s.Status
	last := s.LastStarted
	m.mu.Unlock()
	u.Online = running && status == hostOnline
	if u.Online && last > 0 {
		u.UptimeSec = time.Now().Unix() - last
	}
	u.DiskMB = m.diskUsage(s)
	if running && p.cmd.Process != nil {
		cpu, ram := procUsage(p.cmd.Process.Pid, p)
		u.CPUPct, u.RAMMB = cpu, ram
		m.mu.Lock()
		if cur, ok := m.procs[s.ID]; ok && cur == p {
			if cpu >= 0 {
				cur.lastCPU = cpu
			}
			cur.lastTime = time.Now()
		}
		m.mu.Unlock()
	}
	return u
}

func (m *hostingManager) diskUsage(s *HostedServer) int64 {
	m.mu.Lock()
	if memo, ok := m.diskMemo[s.ID]; ok && time.Since(memo.at) < 15*time.Second {
		m.mu.Unlock()
		return memo.mb
	}
	m.mu.Unlock()
	var total int64
	_ = filepath.Walk(m.dirOf(s), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	mb := total / (1 << 20)
	m.mu.Lock()
	m.diskMemo[s.ID] = diskMemo{mb: mb, at: time.Now()}
	m.mu.Unlock()
	return mb
}

// procUsage returns CPU percent and RSS in MB for a pid. Linux reads /proc;
// Windows reads tasklist/wmic. Anything the OS refuses to report comes back
// as -1 ("n/a") — never invented.
func procUsage(pid int, p *hostedProc) (float64, int64) {
	if runtime.GOOS == "linux" {
		cpu := -1.0
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			fields := strings.Fields(string(b))
			if len(fields) > 15 {
				ut, _ := strconv.ParseFloat(fields[13], 64)
				stt, _ := strconv.ParseFloat(fields[14], 64)
				ticks := ut + stt
				hz := float64(100)
				elapsed := time.Since(p.lastTime).Seconds()
				if p.lastCPU >= 0 && elapsed > 0.5 {
					cpu = ((ticks - p.lastCPU) / hz) / elapsed * 100
					if cpu < 0 {
						cpu = 0
					}
				}
				p.lastCPU = ticks
			}
		}
		ram := int64(-1)
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid)); err == nil {
			fields := strings.Fields(string(b))
			if len(fields) > 1 {
				pages, _ := strconv.ParseInt(fields[1], 10, 64)
				ram = pages * int64(os.Getpagesize()) / (1 << 20)
			}
		}
		return cpu, ram
	}
	// Windows: real numbers from tasklist / wmic, best effort
	ram := int64(-1)
	if out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output(); err == nil {
		line := strings.TrimSpace(string(out))
		if i := strings.LastIndex(line, `","`); i > 0 {
			mem := strings.Trim(line[i+3:], `"\r`)
			mem = strings.ReplaceAll(mem, ",", "")
			mem = strings.ReplaceAll(mem, " K", "")
			if kb, err := strconv.ParseInt(strings.TrimSpace(mem), 10, 64); err == nil {
				ram = kb / 1024
			}
		}
	}
	cpu := -1.0
	if out, err := exec.Command("wmic", "process", "where", fmt.Sprintf("processid=%d", pid), "get", "UserModeTime,KernelModeTime", "/format:csv").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			parts := strings.Split(strings.TrimSpace(lines[len(lines)-1]), ",")
			if len(parts) >= 3 {
				ut, _ := strconv.ParseFloat(parts[1], 64)
				kt, _ := strconv.ParseFloat(parts[2], 64)
				ticks := (ut + kt) / 1e7 // 100ns -> seconds
				elapsed := time.Since(p.lastTime).Seconds()
				if p.lastCPU >= 0 && elapsed > 0.5 {
					cpu = (ticks - p.lastCPU) / elapsed * 100
					if cpu < 0 {
						cpu = 0
					}
				}
				p.lastCPU = ticks
			}
		}
	}
	return cpu, ram
}

// hostFacts reports the real hardware of this machine (the VPS). Used to
// present the infrastructure honestly instead of inventing specifications.
func hostFacts() map[string]interface{} {
	facts := map[string]interface{}{
		"cpuCores": runtime.NumCPU(),
		"ramMB":    int64(-1),
		"goos":     runtime.GOOS,
	}
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						kb, _ := strconv.ParseInt(fields[1], 10, 64)
						facts["ramMB"] = kb / 1024
					}
				}
			}
		}
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "model name") {
					if i := strings.Index(line, ":"); i > 0 {
						facts["cpuModel"] = strings.TrimSpace(line[i+1:])
					}
					break
				}
			}
		}
	} else if out, err := exec.Command("wmic", "computersystem", "get", "TotalPhysicalMemory", "/format:csv").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			parts := strings.Split(strings.TrimSpace(lines[len(lines)-1]), ",")
			if len(parts) >= 2 {
				if b, _ := strconv.ParseInt(parts[1], 10, 64); b > 0 {
					facts["ramMB"] = b / (1 << 20)
				}
			}
		}
	}
	return facts
}

// ---------------------------------------------------------------------------
// pages
// ---------------------------------------------------------------------------

func hostingPage(w http.ResponseWriter, r *http.Request) {
	facts := hostFacts()
	cores, _ := facts["cpuCores"].(int)
	ramText := ""
	if mb, ok := facts["ramMB"].(int64); ok && mb > 0 {
		ramText = fmt.Sprintf("%d GB", mb/1024)
	}
	cpuModel, _ := facts["cpuModel"].(string)
	renderPage(w, r, "hosting.tmpl", map[string]interface{}{
		"Title":    "NSM Hosting - Free Game Servers",
		"PKG":      hostingPackage,
		"Cores":    cores,
		"RAMText":  ramText,
		"CPUModel": cpuModel,
		"First":    hostingPortFirst,
		"Last":     hostingPortLast,
	})
}

func hostingCreatePage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/hosting/create"), http.StatusFound)
		return
	}
	m := hostMgr()
	m.mu.Lock()
	existing := m.findOwnerLocked(u.ID)
	m.mu.Unlock()
	if existing != nil {
		http.Redirect(w, r, "/dashboard/"+existing.ID, http.StatusSeeOther)
		return
	}
	renderPage(w, r, "hosting_create.tmpl", map[string]interface{}{
		"Title":  "Create a Server - NSM Hosting",
		"PKG":    hostingPackage,
		"Error":  r.URL.Query().Get("error"),
		"First":  hostingPortFirst,
		"Last":   hostingPortLast,
	})
}

func hostingCreateSubmit(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/hosting/create"), http.StatusFound)
		return
	}
	_ = r.ParseForm()
	s, err := hostMgr().deploy(u, r.FormValue("name"), r.FormValue("region"), r.Host)
	if s != nil && s.Owner == u.ID {
		// one-per-account double-click protection: always land on their city
		http.Redirect(w, r, "/dashboard/"+s.ID, http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Redirect(w, r, "/hosting/create?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/hosting/manage", http.StatusSeeOther)
}

func hostingManagePage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/hosting/manage"), http.StatusFound)
		return
	}
	m := hostMgr()
	m.mu.Lock()
	s := m.findOwnerLocked(u.ID)
	var snap *HostedServer
	if s != nil {
		cp := *s
		snap = &cp
	}
	m.mu.Unlock()
	data := map[string]interface{}{"Title": "My Server - NSM Hosting", "PKG": hostingPackage}
	if snap != nil {
		players := int64(0)
		if d := reg.get(snap.Endpoint); d != nil {
			players = d.Clients
		}
		data["S"] = snap
		data["Players"] = players
	}
	renderPage(w, r, "hosting_manage.tmpl", data)
}

// ---------------------------------------------------------------------------
// dashboard (panel pages + API)
// ---------------------------------------------------------------------------

// panelTab defaults to the console — the console is the panel home.
func panelTab(t string) string {
	switch t {
	case "overview", "files", "ports", "console", "config":
		return t
	}
	return "console"
}

func dashboardServerPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	id := r.PathValue("id")
	m := hostMgr()
	m.mu.Lock()
	s := m.find(id)
	var snap *HostedServer
	if s != nil && s.Owner == u.ID {
		cp := *s
		snap = &cp
	}
	m.mu.Unlock()
	if snap == nil {
		http.NotFound(w, r)
		return
	}
	players := int64(0)
	if d := reg.get(snap.Endpoint); d != nil {
		players = d.Clients
	}
	renderPage(w, r, "dashboard_server.tmpl", map[string]interface{}{
		"Title": snap.Name + " - NSM Hosting", "S": snap, "PKG": hostingPackage,
		"Tab": panelTab(r.URL.Query().Get("tab")), "Players": players,
		"First": hostingPortFirst, "Last": hostingPortLast,
		"Host": publicHostOf(snap), "JoinBase": joinBaseURL(r),
		"Firewall": m.firewallNote, "PkgWarn": serverFilesWarning(),
	})
}

func apiHostConsole(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	m := hostMgr()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.find(r.PathValue("id"))
	if s == nil || s.Owner != u.ID {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	lines := []map[string]interface{}{}
	for i, l := range s.Console {
		seq := s.ConsoleSeq - int64(len(s.Console)) + int64(i) + 1
		if seq > since {
			lines = append(lines, map[string]interface{}{"seq": seq, "line": l})
		}
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"seq": s.ConsoleSeq, "lines": lines, "status": s.Status})
}

func apiHostCommand(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	var body struct {
		Cmd string `json:"cmd"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	m.mu.Unlock()
	if s == nil || s.Owner != u.ID {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err := m.command(s, strings.TrimSpace(body.Cmd)); err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiHostPower(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	m.mu.Unlock()
	if s == nil || s.Owner != u.ID {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	switch r.PathValue("action") {
	case "start":
		if err := m.start(s); err != nil {
			sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "status": hostError})
			return
		}
	case "stop":
		m.stop(s)
	case "restart":
		m.stop(s)
		_ = m.start(s)
	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "unknown action"})
		return
	}
	m.mu.Lock()
	status := s.Status
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]string{"ok": "true", "status": status})
}

func apiHostUsage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	m.mu.Unlock()
	if s == nil || s.Owner != u.ID {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	sendJSON(w, http.StatusOK, m.usage(s))
}

func apiHostDelete(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	m.mu.Unlock()
	if s == nil || s.Owner != u.ID {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	revokeServerKeyAndEvict(s.KeyID)
	m.delete(s)
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// file manager API (the real city directory on this machine)
// ---------------------------------------------------------------------------

// hostSafePath resolves rel inside root and rejects escapes.
func hostSafePath(root, rel string) (string, bool) {
	clean := filepath.Clean(root)
	full := filepath.Join(clean, filepath.FromSlash(strings.Trim(rel, "/")))
	if full != clean && !strings.HasPrefix(full, clean+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

func (m *hostingManager) owned(id string, u *User) (*HostedServer, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.find(id)
	if s == nil || s.Owner != u.ID {
		return nil, "", false
	}
	return s, m.serverRoot(s), true
}

func apiHostFiles(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	_, root, ok := hostMgr().owned(r.PathValue("id"), u)
	if !ok {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	rel := strings.Trim(filepath.Clean("/"+r.URL.Query().Get("path")), "/")
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !strings.HasPrefix(full, filepath.Clean(root)) {
		sendJSON(w, http.StatusForbidden, map[string]string{"error": "path outside instance"})
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "cannot list: " + err.Error()})
		return
	}
	type fe struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Dir   bool   `json:"dir"`
		Size  int64  `json:"size"`
		MTime int64  `json:"mtime"`
	}
	out := []fe{}
	for _, e := range entries {
		info, _ := e.Info()
		var sz, mt int64
		if info != nil {
			sz = info.Size()
			mt = info.ModTime().Unix()
		}
		p := strings.TrimPrefix(filepath.ToSlash(filepath.Join(rel, e.Name())), "/")
		out = append(out, fe{e.Name(), p, e.IsDir(), sz, mt})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	sendJSON(w, http.StatusOK, map[string]interface{}{"path": rel, "entries": out})
}

func apiHostFileRead(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	_, root, ok := hostMgr().owned(r.PathValue("id"), u)
	if !ok {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	full, ok := hostSafePath(root, r.URL.Query().Get("path"))
	if !ok {
		sendJSON(w, http.StatusForbidden, map[string]string{"error": "path outside instance"})
		return
	}
	b, err := os.ReadFile(full)
	if err != nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]string{"content": string(b)})
}

func apiHostFileDownload(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	_, root, ok := hostMgr().owned(r.PathValue("id"), u)
	if !ok {
		http.NotFound(w, r)
		return
	}
	full, ok := hostSafePath(root, r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "path outside instance", http.StatusForbidden)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(full)+`"`)
	if st != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

func apiHostFileWrite(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body)
	s, root, ok := hostMgr().owned(r.PathValue("id"), u)
	if !ok {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	full, ok := hostSafePath(root, body.Path)
	if !ok {
		sendJSON(w, http.StatusForbidden, map[string]string{"error": "path outside instance"})
		return
	}
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(body.Content), 0o600); err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	hostMgr().diskMemo[s.ID] = diskMemo{} // invalidate
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiHostFileOp(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	var body struct {
		Op   string `json:"op"` // delete | mkdir | rename
		Path string `json:"path"`
		To   string `json:"to"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	s, root, ok := hostMgr().owned(r.PathValue("id"), u)
	if !ok {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	full, ok := hostSafePath(root, body.Path)
	if !ok {
		sendJSON(w, http.StatusForbidden, map[string]string{"error": "path outside instance"})
		return
	}
	var err error
	switch body.Op {
	case "delete":
		err = os.RemoveAll(full)
	case "mkdir":
		err = os.MkdirAll(full, 0o755)
	case "rename":
		var dst string
		if dst, ok = hostSafePath(root, body.To); !ok {
			sendJSON(w, http.StatusForbidden, map[string]string{"error": "path outside instance"})
			return
		}
		err = os.Rename(full, dst)
	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "unknown op"})
		return
	}
	if err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	hostMgr().diskMemo[s.ID] = diskMemo{}
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiHostUpload(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	s, root, ok := hostMgr().owned(r.PathValue("id"), u)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dirRel := r.FormValue("path")
	f, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer f.Close()
	full, ok := hostSafePath(root, filepath.ToSlash(filepath.Join(dirRel, filepath.Base(hdr.Filename))))
	if !ok {
		http.Error(w, "path outside instance", http.StatusForbidden)
		return
	}
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	dst, err := os.Create(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer dst.Close()
	if _, err = io.Copy(dst, f); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	hostMgr().diskMemo[s.ID] = diskMemo{}
	http.Redirect(w, r, "/dashboard/"+s.ID+"?tab=files", http.StatusSeeOther)
}

func apiHostFilezilla(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	var snap *HostedServer
	if s != nil && s.Owner == u.ID {
		cp := *s
		snap = &cp
	}
	m.mu.Unlock()
	if snap == nil {
		http.NotFound(w, r)
		return
	}
	host := publicHostOf(snap)
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<FileZilla3 version="3.65.0" platform="windows">
  <Servers>
    <Server>
      <Host>%s</Host>
      <Port>%d</Port>
      <Protocol>0</Protocol>
      <Type>0</Type>
      <User>%s</User>
      <Pass encoding="base64">%s</Pass>
      <Logontype>1</Logontype>
      <Name>%s</Name>
    </Server>
  </Servers>
</FileZilla3>
`, host, snap.FTPPort, snap.FTPUser, b64(snap.FTPPass), snap.Name)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(snap.Name, `"`, "")+`-filezilla.xml"`)
	_, _ = io.WriteString(w, xml)
}

// b64 encodes the FileZilla profile password.
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func apiHostPorts(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	m := hostMgr()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.find(r.PathValue("id"))
	if s == nil || s.Owner != u.ID {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	p, err := m.allocPortLocked()
	if err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.ExtraPorts = append(s.ExtraPorts, p)
	m.logNL(s, fmt.Sprintf("[system] extra port allocated: %d", p))
	m.saveLocked()
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "port": p, "extra": s.ExtraPorts})
}

// publicHostOf returns the host this server is reachable at: NSM_PUBLIC_IP
// when set, otherwise the host derived at deploy time (the site's own host).
func publicHostOf(s *HostedServer) string {
	if h := os.Getenv("NSM_PUBLIC_IP"); h != "" {
		return h
	}
	if s.Endpoint != "" {
		if host, _, err := net.SplitHostPort(s.Endpoint); err == nil && host != "" {
			return host
		}
	}
	return "127.0.0.1"
}

// startFTP brings up the FileZilla endpoint for an instance. Callers hold
// m.mu (same convention as logNL).
func (m *hostingManager) startFTP(s *HostedServer) {
	if _, ok := m.ftp[s.ID]; ok || s.FTPPort <= 0 {
		return
	}
	fs, err := startFTPServer(s.FTPPort, s.PassivePort, s.FTPUser, s.FTPPass, func() string {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.serverRoot(s)
		}, func(msg string) {
		m.appendLog(s, msg)
	})
	if err != nil {
		m.logNL(s, "[warn] files endpoint failed to start: "+err.Error())
		return
	}
	m.ftp[s.ID] = fs
	m.logNL(s, fmt.Sprintf("FileZilla: ftp://%s:%d (user %s) — full address on the Files tab", publicHostOf(s), s.FTPPort, s.FTPUser))
}

// ---------------------------------------------------------------------------
// routes
// ---------------------------------------------------------------------------

func registerHostingRoutes(mux *http.ServeMux) {
	// bring up hosted-city state + FileZilla endpoints at boot
	hostMgr()
	mux.HandleFunc("GET /hosting", hostingPage)
	mux.HandleFunc("GET /hosting/create", hostingCreatePage)
	mux.HandleFunc("POST /hosting/create", hostingCreateSubmit)
	mux.HandleFunc("GET /hosting/manage", hostingManagePage)
	// legacy URLs from earlier builds keep working
	mux.HandleFunc("GET /deploy", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hosting/create", http.StatusFound)
	})
	mux.HandleFunc("POST /hosting/deploy", hostingCreateSubmit)
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hosting/manage", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard/{id}", dashboardServerPage)
	mux.HandleFunc("GET /api/host/{id}/console", apiHostConsole)
	mux.HandleFunc("POST /api/host/{id}/command", apiHostCommand)
	mux.HandleFunc("POST /api/host/{id}/power/{action}", apiHostPower)
	mux.HandleFunc("GET /api/host/{id}/usage", apiHostUsage)
	mux.HandleFunc("POST /api/host/{id}/delete", apiHostDelete)
	mux.HandleFunc("GET /api/host/{id}/files", apiHostFiles)
	mux.HandleFunc("GET /api/host/{id}/file", apiHostFileRead)
	mux.HandleFunc("GET /api/host/{id}/file/download", apiHostFileDownload)
	mux.HandleFunc("POST /api/host/{id}/file/write", apiHostFileWrite)
	mux.HandleFunc("POST /api/host/{id}/file/op", apiHostFileOp)
	mux.HandleFunc("POST /api/host/{id}/upload", apiHostUpload)
	mux.HandleFunc("GET /api/host/{id}/filezilla.xml", apiHostFilezilla)
	mux.HandleFunc("POST /api/host/{id}/ports", apiHostPorts)
}
