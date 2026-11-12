// hosting.go — free NSM server hosting: deploy, console, files, and the
// 500-port allocation pool. Users deploy a hosted city from /hosting and
// manage it in the ZAPX-style panel at /dashboard/<id>.
//
// What is real here:
//   - deploy creates the instance directory + generated server.cfg
//   - start/stop runs NSM-Server.exe on this machine when the binary is
//     present (NSM_SERVER_EXE env or next to this exe) and streams its real
//     stdout/stderr into the web console; stdin accepts console commands
//   - join links use the real /join/<code> system
//   - FileZilla connects over a real FTP endpoint (hosting_ftp.go), sandboxed
//     to the instance directory
//   - ports come from a real 500-port pool (30100-30599), tracked here
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// model + persistence
// ---------------------------------------------------------------------------

const (
	hostingPortFirst = 30100
	hostingPortLast  = 30599 // inclusive — exactly 500 ports
	hostingDirEnv    = "NSM_HOSTING_DIR"
	serverExeEnv     = "NSM_SERVER_EXE"
)

// HostingTier describes one free hosting package (ZAPX-style spec rows).
type HostingTier struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Slots    int    `json:"slots"`
	MemoryGB int    `json:"memoryGB"`
	Ports    int    `json:"ports"` // total ports granted from the pool
	Blurb    string `json:"blurb"`
}

var hostingTiers = []HostingTier{
	{"citizen", "Citizen", 32, 1, 4, "Perfect for a first city with friends."},
	{"enforcer", "Enforcer", 256, 4, 6, "Growing communities with custom gamemodes."},
	{"mayor", "Mayor", 2048, 16, 8, "Full-scale cities with the full 2048 slots."},
}

// HostedServer is one deployed instance.
type HostedServer struct {
	ID           string   `json:"id"`
	Owner        int64    `json:"owner"`
	OwnerName    string   `json:"ownerName"`
	Name         string   `json:"name"`
	Tier         string   `json:"tier"`
	Region       string   `json:"region"`
	GamePort     int      `json:"gamePort"`
	FTPPort      int      `json:"ftpPort"`
	PassivePort  int      `json:"passivePort"`
	ExtraPorts   []int    `json:"extraPorts"`
	Endpoint     string   `json:"endpoint"`
	JoinCode     string   `json:"joinCode"`
	FTPUser      string   `json:"ftpUser"`
	FTPPass      string   `json:"ftpPass"`
	Status       string   `json:"status"` // deploying | running | stopped | missing
	Note         string   `json:"note,omitempty"`
	CreatedAt    int64    `json:"createdAt"`
	LastStarted  int64    `json:"lastStarted,omitempty"`
	Console      []string `json:"console,omitempty"`
	ConsoleSeq   int64    `json:"consoleSeq"`
}

type hostingState struct {
	Servers  []*HostedServer `json:"servers"`
	NextPort int             `json:"nextPort"`
}

// hostingManager owns hosting.json (single writer), processes and FTP listeners.
type hostingManager struct {
	mu       sync.Mutex
	st       hostingState
	path     string
	root     string
	procs    map[string]*hostedProc
	ftp      map[string]*ftpServer
	firewall bool
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
			path:  "hosting.json",
			root:  root,
			procs: map[string]*hostedProc{},
			ftp:   map[string]*ftpServer{},
		}
		hostMgrV = m
		m.st.NextPort = hostingPortFirst
		if b, err := os.ReadFile(m.path); err == nil {
			_ = json.Unmarshal(b, &m.st)
		}
		if m.st.NextPort < hostingPortFirst {
			m.st.NextPort = hostingPortFirst
		}
		_ = os.MkdirAll(m.root, 0o755)
		// bring back FTP listeners for existing records
		for _, s := range m.st.Servers {
			if s.FTPPort > 0 {
				m.startFTP(s)
			}
			if s.Status == "running" {
				// process did not survive the restart — be honest
				s.Status = "stopped"
				m.log(s, "[system] panel restarted — server process is stopped, press Start")
			}
		}
		m.saveLocked()
	})
	return hostMgrV
}

func (m *hostingManager) saveLocked() {
	b, _ := json.MarshalIndent(&m.st, "", "  ")
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.path)
	}
}

func (m *hostingManager) log(s *HostedServer, line string) {
	s.ConsoleSeq++
	s.Console = append(s.Console, fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line))
	if len(s.Console) > 400 {
		s.Console = s.Console[len(s.Console)-400:]
	}
}

func (m *hostingManager) find(id string) *HostedServer {
	for _, s := range m.st.Servers {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ports — the 500-port pool
// ---------------------------------------------------------------------------

func (m *hostingManager) allocPortLocked() (int, error) {
	if m.st.NextPort > hostingPortLast {
		return 0, fmt.Errorf("the hosting port pool (%d-%d) is fully allocated", hostingPortFirst, hostingPortLast)
	}
	p := m.st.NextPort
	m.st.NextPort++
	return p, nil
}

func (m *hostingManager) portsInUseLocked() map[int]string {
	used := map[int]string{}
	for _, s := range m.st.Servers {
		used[s.GamePort] = s.Name + " (game)"
		used[s.FTPPort] = s.Name + " (files)"
		if s.PassivePort > 0 {
			used[s.PassivePort] = s.Name + " (files data)"
		}
		for _, p := range s.ExtraPorts {
			used[p] = s.Name + " (extra)"
		}
	}
	return used
}

// hostingFirewallOnce best-effort opens the pool on Windows so players can
// join hosted servers. Failure is logged, never fatal — the operator can open
// the range manually.
func (m *hostingManager) hostingFirewallOnce() {
	if m.firewall {
		return
	}
	m.firewall = true
	if _, err := exec.LookPath("netsh"); err != nil {
		log.Printf("[hosting] netsh not available — open TCP %d-%d in the firewall manually so hosted cities are joinable", hostingPortFirst, hostingPortLast)
		return
	}
	args := []string{"advfirewall", "firewall", "add", "rule", "name=NSM Hosting (500 ports)",
		"dir=in", "action=allow", "protocol=TCP",
		fmt.Sprintf("localport=%d-%d", hostingPortFirst, hostingPortLast)}
	out, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		log.Printf("[hosting] firewall rule failed (%v): %s — open TCP %d-%d manually", err, strings.TrimSpace(string(out)), hostingPortFirst, hostingPortLast)
		return
	}
	log.Printf("[hosting] firewall: allowed TCP %d-%d (%d ports) for hosted cities", hostingPortFirst, hostingPortLast, hostingPortLast-hostingPortFirst+1)
}

// ---------------------------------------------------------------------------
// provisioning
// ---------------------------------------------------------------------------

func (m *hostingManager) dirOf(s *HostedServer) string {
	return filepath.Join(m.root, s.ID)
}

func serverExePath() string {
	if p := os.Getenv(serverExeEnv); p != "" {
		return p
	}
	for _, cand := range []string{"NSM-Server.exe", "nsm-server.exe", "NSM-Server", "nsm-server"} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// deploy creates a new hosted city for the signed-in user.
func (m *hostingManager) deploy(u *User, name, tierID, region string) (*HostedServer, error) {
	tier := hostingTiers[0]
	for _, t := range hostingTiers {
		if t.ID == tierID {
			tier = t
		}
	}
	if region == "" {
		region = "us-east"
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = u.Username + "-city"
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostingFirewallOnce()

	s := &HostedServer{
		ID:        fmt.Sprintf("h%d", time.Now().UnixNano()%1e9),
		Owner:     u.ID,
		OwnerName: u.Username,
		Name:      name,
		Tier:      tier.ID,
		Region:    region,
		CreatedAt: time.Now().Unix(),
		Status:    "deploying",
		FTPUser:   "nsm_" + strconv.FormatInt(u.ID, 36) + "_" + strconv.Itoa(len(m.st.Servers)),
		FTPPass:   hostRandToken(14),
	}
	var err error
	if s.GamePort, err = m.allocPortLocked(); err != nil {
		return nil, err
	}
	if s.FTPPort, err = m.allocPortLocked(); err != nil {
		return nil, err
	}
	if s.PassivePort, err = m.allocPortLocked(); err != nil {
		return nil, err
	}
	granted := tier.Ports - 3
	for i := 0; i < granted; i++ {
		p, err := m.allocPortLocked()
		if err != nil {
			break
		}
		s.ExtraPorts = append(s.ExtraPorts, p)
	}

	host := os.Getenv("NSM_PUBLIC_IP")
	if host == "" {
		host = "127.0.0.1"
	}
	s.Endpoint = fmt.Sprintf("%s:%d", host, s.GamePort)
	s.JoinCode = joinCodeForEndpoint(s.Endpoint)

	dir := m.dirOf(s)
	if err := os.MkdirAll(filepath.Join(dir, "resources"), 0o755); err != nil {
		return nil, err
	}
	cfg := m.generateCfg(s, tier)
	if err := os.WriteFile(filepath.Join(dir, "server.cfg"), []byte(cfg), 0o600); err != nil {
		return nil, err
	}
	m.st.Servers = append(m.st.Servers, s)
	m.log(s, "[system] deployed city '"+name+"' — tier "+tier.Name+", region "+region)
	m.log(s, fmt.Sprintf("[system] allocated ports: game %d, files %d-%d, extra %v", s.GamePort, s.FTPPort, s.PassivePort, s.ExtraPorts))
	m.log(s, "[system] wrote server.cfg — edit it in the Files tab")
	m.startFTP(s)
	m.saveLocked()
	return s, nil
}

func (m *hostingManager) generateCfg(s *HostedServer, tier HostingTier) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — generated by the NSM hosting panel\n", s.Name)
	fmt.Fprintf(&b, "endpoint_add_tcp \"0.0.0.0:%d\"\n", s.GamePort)
	fmt.Fprintf(&b, "endpoint_add_udp \"0.0.0.0:%d\"\n", s.GamePort)
	fmt.Fprintf(&b, "sv_hostname \"%s\"\n", strings.ReplaceAll(s.Name, `"`, ""))
	fmt.Fprintf(&b, "sv_maxclients %d\n", tier.Slots)
	fmt.Fprintf(&b, "sets sv_region \"%s\"\n", s.Region)
	fmt.Fprintf(&b, "sets sv_nsmHosted \"1\"\n")
	b.WriteString("# paste the server key from AFX Portal (it is shown only when you create it):\n")
	b.WriteString("# sets sv_nsmServerKey \"nsm_srv_...\"\n")
	b.WriteString("# add your resources under resources/ and start them here\n")
	b.WriteString("ensure chat\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// process control (real NSM-Server.exe when present)
// ---------------------------------------------------------------------------

type hostedProc struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	done chan struct{}
}

func (m *hostingManager) start(s *HostedServer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[s.ID]; ok {
		select {
		case <-p.done:
		default:
			m.log(s, "[system] already running")
			m.saveLocked()
			return nil
		}
	}
	exe := serverExePath()
	if exe == "" {
		s.Status = "missing"
		s.Note = "NSM-Server.exe is not on this machine yet — place it next to the panel exe or set NSM_SERVER_EXE"
		m.log(s, "[warn] NSM-Server.exe not found on this host — server binary missing")
		m.log(s, "[system] everything else is ready: config, ports, join link and files")
		m.saveLocked()
		return fmt.Errorf("server binary missing")
	}
	cmd := exec.Command(exe, "+exec", "server.cfg")
	cmd.Dir = m.dirOf(s)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		s.Status = "stopped"
		m.log(s, "[error] failed to start server process: "+err.Error())
		m.saveLocked()
		return err
	}
	p := &hostedProc{cmd: cmd, in: stdin, done: make(chan struct{})}
	m.procs[s.ID] = p
	s.Status = "running"
	s.LastStarted = time.Now().Unix()
	m.log(s, "[system] server process started (pid "+strconv.Itoa(cmd.Process.Pid)+")")
	m.saveLocked()
	go m.pump(s.ID, stdout)
	go m.pump(s.ID, stderr)
	go func() {
		_ = cmd.Wait()
		close(p.done)
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.procs[s.ID]; ok && cur == p {
			delete(m.procs, s.ID)
			if s.Status == "running" {
				s.Status = "stopped"
				m.log(s, "[system] server process exited")
				m.saveLocked()
			}
		}
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
			m.log(s, line)
			m.saveLocked()
		}
		m.mu.Unlock()
	}
}

func (m *hostingManager) stop(s *HostedServer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[s.ID]
	if !ok {
		if s.Status == "running" {
			s.Status = "stopped"
		}
		m.log(s, "[system] not running")
		m.saveLocked()
		return
	}
	_, _ = io.WriteString(p.in, "quit\n")
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		_ = p.cmd.Process.Kill()
	}
	s.Status = "stopped"
	m.log(s, "[system] server stopped")
	m.saveLocked()
}

func (m *hostingManager) command(s *HostedServer, cmdText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[s.ID]
	if !ok {
		m.log(s, "[error] console command ignored — server is not running")
		m.saveLocked()
		return fmt.Errorf("server is not running")
	}
	_, err := io.WriteString(p.in, cmdText+"\n")
	m.log(s, "> "+cmdText)
	m.saveLocked()
	return err
}

// ---------------------------------------------------------------------------
// HTTP API + pages
// ---------------------------------------------------------------------------

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

func hostingPage(w http.ResponseWriter, r *http.Request) {
	renderPage(w, r, "hosting.tmpl", map[string]interface{}{
		"Title": "Free NSM Server Hosting - NSM",
		"Tiers": hostingTiers,
		"First": hostingPortFirst,
		"Last":  hostingPortLast,
		"Count": hostingPortLast - hostingPortFirst + 1,
	})
}

func hostingDeploy(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	_ = r.ParseForm()
	s, err := hostMgr().deploy(u, r.FormValue("name"), r.FormValue("tier"), r.FormValue("region"))
	if err != nil {
		renderPage(w, r, "hosting.tmpl", map[string]interface{}{
			"Title": "Free NSM Server Hosting - NSM", "Tiers": hostingTiers,
			"Error": "Could not deploy: " + err.Error(),
		})
		return
	}
	http.Redirect(w, r, "/dashboard/"+s.ID, http.StatusSeeOther)
}

func dashboardPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	m := hostMgr()
	m.mu.Lock()
	var mine []*HostedServer
	for _, s := range m.st.Servers {
		if s.Owner == u.ID {
			mine = append(mine, s)
		}
	}
	m.mu.Unlock()
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreatedAt > mine[j].CreatedAt })
	renderPage(w, r, "dashboard.tmpl", map[string]interface{}{
		"Title": "Dashboard - NSM", "Servers": mine, "Tiers": hostingTiers,
	})
}

func dashboardServerPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
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
	// live player count from the server list registry
	players := int64(0)
	if d := reg.get(snap.Endpoint); d != nil {
		players = d.Clients
	}
	tier := hostingTiers[0]
	for _, t := range hostingTiers {
		if t.ID == snap.Tier {
			tier = t
		}
	}
	renderPage(w, r, "dashboard_server.tmpl", map[string]interface{}{
		"Title": snap.Name + " - Dashboard - NSM", "S": snap, "Tier": tier,
		"Tab": r.URL.Query().Get("tab"), "Players": players,
		"First": hostingPortFirst, "Last": hostingPortLast,
		"PublicIP": os.Getenv("NSM_PUBLIC_IP"),
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
	if s == nil || s.Owner != u.ID {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	m.mu.Unlock()
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
	if s == nil || s.Owner != u.ID {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	m.mu.Unlock()
	action := r.PathValue("action")
	switch action {
	case "start":
		if err := m.start(s); err != nil {
			sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "status": s.Status})
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
	sendJSON(w, http.StatusOK, map[string]string{"ok": "true", "status": s.Status})
}

func apiHostFiles(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	if s == nil || s.Owner != u.ID {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	root := m.dirOf(s)
	m.mu.Unlock()

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
		var sz int64
		var mt int64
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
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	if s == nil || s.Owner != u.ID {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	root := m.dirOf(s)
	m.mu.Unlock()
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
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	if s == nil || s.Owner != u.ID {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	root := m.dirOf(s)
	m.mu.Unlock()
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
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiHostFileOp(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	var body struct {
		Op   string `json:"op"` // delete | mkdir
		Path string `json:"path"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	if s == nil || s.Owner != u.ID {
		m.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	root := m.dirOf(s)
	m.mu.Unlock()
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
	default:
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "unknown op"})
		return
	}
	if err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiHostUpload(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}
	m := hostMgr()
	m.mu.Lock()
	s := m.find(r.PathValue("id"))
	if s == nil || s.Owner != u.ID {
		m.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	root := m.dirOf(s)
	m.mu.Unlock()
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
	_, err = io.Copy(dst, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/dashboard/"+s.ID+"?tab=files", http.StatusSeeOther)
}

// hostSafePath resolves rel inside root and rejects escapes.
func hostSafePath(root, rel string) (string, bool) {
	clean := filepath.Clean(root)
	full := filepath.Join(clean, filepath.FromSlash(strings.Trim(rel, "/")))
	if full != clean && !strings.HasPrefix(full, clean+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
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
	host := os.Getenv("NSM_PUBLIC_IP")
	if host == "" {
		host = "127.0.0.1"
	}
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
	m.log(s, fmt.Sprintf("[system] extra port allocated: %d", p))
	m.saveLocked()
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "port": p, "extra": s.ExtraPorts})
}

// b64 encodes the FileZilla profile password.
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// startFTP brings up the FileZilla endpoint for an instance. Callers hold
// m.mu (same convention as m.log).
func (m *hostingManager) startFTP(s *HostedServer) {
	if _, ok := m.ftp[s.ID]; ok || s.FTPPort <= 0 {
		return
	}
	fs, err := startFTPServer(s.FTPPort, s.PassivePort, s.FTPUser, s.FTPPass, m.dirOf(s), func(msg string) {
		m.mu.Lock()
		if cur := m.find(s.ID); cur != nil {
			m.log(cur, msg)
			m.saveLocked()
		}
		m.mu.Unlock()
	})
	if err != nil {
		m.log(s, "[warn] files endpoint failed to start: "+err.Error())
		return
	}
	m.ftp[s.ID] = fs
	host := os.Getenv("NSM_PUBLIC_IP")
	if host == "" {
		host = "127.0.0.1"
	}
	m.log(s, fmt.Sprintf("FileZilla: ftp://%s:%d (user %s) — see the Files tab for the profile", host, s.FTPPort, s.FTPUser))
}

func registerHostingRoutes(mux *http.ServeMux) {
	// bring up hosted-city state + FileZilla endpoints at boot so FTP survives
	// panel restarts without waiting for the first dashboard visit
	hostMgr()
	mux.HandleFunc("GET /hosting", hostingPage)
	mux.HandleFunc("POST /hosting/deploy", hostingDeploy)
	mux.HandleFunc("GET /dashboard", dashboardPage)
	mux.HandleFunc("GET /dashboard/{id}", dashboardServerPage)
	mux.HandleFunc("GET /api/host/{id}/console", apiHostConsole)
	mux.HandleFunc("POST /api/host/{id}/command", apiHostCommand)
	mux.HandleFunc("POST /api/host/{id}/power/{action}", apiHostPower)
	mux.HandleFunc("GET /api/host/{id}/files", apiHostFiles)
	mux.HandleFunc("GET /api/host/{id}/file", apiHostFileRead)
	mux.HandleFunc("POST /api/host/{id}/file/write", apiHostFileWrite)
	mux.HandleFunc("POST /api/host/{id}/file/op", apiHostFileOp)
	mux.HandleFunc("POST /api/host/{id}/upload", apiHostUpload)
	mux.HandleFunc("GET /api/host/{id}/filezilla.xml", apiHostFilezilla)
	mux.HandleFunc("POST /api/host/{id}/ports", apiHostPorts)
}
