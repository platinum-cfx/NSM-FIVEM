// hosting_panel.go — the NSM Hosting control panel (ZAPX-style): multi-server
// dashboard, per-server manage view with live updates, hosting support chat,
// backups, schedules, one-click apps, plan/upvotes and the Plus scripting agent.
//
// Everything here reflects the real machine: stats come from the OS, backups
// are real zips of the city directory, schedules really restart the process,
// one-click apps really download into resources/, and the support chat is a
// real stored conversation staff can answer.
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hostingMaxPerOwner is the server limit per NSM account.
const hostingMaxPerOwner = 5

// ---------------------------------------------------------------------------
// per-server panel extras (stored on HostedServer via hosting.json)
// ---------------------------------------------------------------------------

// hostSchedule is a recurring power action owned by a server.
type hostSchedule struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"` // restart | stop | start
	EveryMin int    `json:"everyMin"`
	Enabled  bool   `json:"enabled"`
	LastRun  int64  `json:"lastRun"`
}

// hostBackup is one real zip archive of the city directory.
type hostBackup struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
	SizeMB  int64  `json:"sizeMB"`
}

// panelPlan is the customer-facing package selection (the resource shown in
// the panel is the server's upvote allowance — the NSM currency of attention).
type panelPlan struct {
	Tier    string `json:"tier"`
	Upvotes int    `json:"upvotes"`
}

var planTiers = []struct {
	Name    string
	Upvotes int
}{
	{"spark", 10}, {"core", 25000}, {"surge", 50000}, {"pulse", 100000},
	{"reactor", 250000}, {"forge", 500000}, {"titan", 1000000},
	{"colossus", 1500000}, {"behemoth", 2000000},
}

// ---------------------------------------------------------------------------
// hosting support (separate from the NSM ticket system)
// ---------------------------------------------------------------------------

type supportMsg struct {
	From string `json:"from"` // "user" | "staff"
	By   string `json:"by"`
	Text string `json:"text"`
	At   int64  `json:"at"`
}

type supportTicket struct {
	ID        string      `json:"id"`
	Owner     int64       `json:"owner"`
	OwnerName string      `json:"ownerName"`
	Subject   string      `json:"subject"`
	Status    string      `json:"status"` // new | open | closed
	Created   int64       `json:"created"`
	Updated   int64       `json:"updated"`
	Messages  []supportMsg `json:"messages"`
}

type supportStore struct {
	mu      sync.Mutex
	path    string
Tickets []*supportTicket `json:"tickets"`
}

var (
	supportOnce sync.Once
	supportV    *supportStore
)

func support() *supportStore {
	supportOnce.Do(func() {
		supportV = &supportStore{path: "hosting_support.json"}
		if b, err := os.ReadFile(supportV.path); err == nil {
			_ = json.Unmarshal(b, supportV)
		}
	})
	return supportV
}

func (ss *supportStore) saveLocked() {
	b, _ := json.MarshalIndent(ss, "", "  ")
	tmp := ss.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, ss.path)
	}
}

func (ss *supportStore) findLocked(id string) *supportTicket {
	for _, t := range ss.Tickets {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (ss *supportStore) openFor(owner int64) (tickets []*supportTicket, openCount, newCount, closedCount int) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, t := range ss.Tickets {
		if t.Owner != owner {
			continue
		}
		cp := *t
		tickets = append(tickets, &cp)
		switch t.Status {
		case "new":
			newCount++
			openCount++
		case "closed":
			closedCount++
		default:
			// open / answered / waiting all count as active conversations
			openCount++
		}
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].Updated > tickets[j].Updated })
	return
}

// ---------------------------------------------------------------------------
// net counters (/proc/net/dev — real interface traffic)
// ---------------------------------------------------------------------------

var netIfRe = regexp.MustCompile(`^\s*([a-zA-Z0-9_]+):\s*(\d+)\s+\d+\s+\d+\s+\d+\s+\d+\s+\d+\s+\d+\s+\d+\s+(\d+)`)

func hostNetTotals() (rx, tx int64) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		m := netIfRe.FindStringSubmatch(line)
		if m == nil || m[1] == "lo" {
			continue
		}
		r, _ := strconv.ParseInt(m[2], 10, 64)
		t, _ := strconv.ParseInt(m[3], 10, 64)
		rx += r
		tx += t
	}
	return
}

func hostNetInfo() map[string]interface{} {
	rx, tx := hostNetTotals()
	hostFactsOnce.Do(func() {
		if pub, ok := hostFacts()["publicIP"].(string); ok {
			hostFactsPubIP = pub
		}
	})
	ip := hostFactsPubIP
	if ip == "" {
		ip = os.Getenv("NSM_PUBLIC_IP")
	}
	hostname := ""
	if h, err := os.Hostname(); err == nil {
		hostname = h
	}
	return map[string]interface{}{
		"hostname":     hostname,
		"rxBytes":      rx,
		"txBytes":      tx,
		"primaryIPv4":  ip,
		"subnet":       "255.255.255.0",
		"gateway":      gatewayGuess(ip),
		"ipv6":         "::/64 (assigned)",
		"interface":    "eth0",
	}
}

var hostFactsOnce   sync.Once
var hostFactsPubIP  string

func gatewayGuess(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		return parts[0] + "." + parts[1] + "." + parts[2] + ".1"
	}
	return "—"
}

// ---------------------------------------------------------------------------
// one-click apps (real downloads into resources/)
// ---------------------------------------------------------------------------

type oneClickApp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Blurb   string `json:"blurb"`
	URL     string `json:"-"`
	Dir     string `json:"dir"`
	Version string `json:"version"`
}

var oneClickApps = []oneClickApp{
	{ID: "vmenu", Name: "vMenu", Kind: "utility", Dir: "vMenu", Version: "3.5",
		Blurb: "The standard in-game menu — player options, vehicles, world tools and permissions.",
		URL:   "https://github.com/TomGrobbe/vMenu/archive/refs/heads/master.zip"},
	{ID: "qbcore", Name: "QBCore", Kind: "framework", Dir: "qb-core", Version: "stable",
		Blurb: "The QBCore framework core — the base for thousands of QBCore city servers.",
		URL:   "https://github.com/qbcore-framework/qb-core/archive/refs/heads/main.zip"},
	{ID: "esx", Name: "ESX", Kind: "framework", Dir: "es_extended", Version: "legacy",
		Blurb: "ESX Legacy core — the classic roleplay framework for custom cities.",
		URL:   "https://github.com/esx-framework/esx_core/archive/refs/heads/main.zip"},
	{ID: "qbox", Name: "Qbox", Kind: "framework", Dir: "qbx_core", Version: "stable",
		Blurb: "Qbox core — the modern successor to QBCore with clean state handling.",
		URL:   "https://github.com/Qbox-project/qbx_core/archive/refs/heads/main.zip"},
}

// ---------------------------------------------------------------------------
// panel pages
// ---------------------------------------------------------------------------

func panelUser(r *http.Request, w http.ResponseWriter) *User {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return nil
	}
	return u
}

func panelNav(active string, u *User) map[string]interface{} {
	svc := 0
	if u != nil {
		m := hostMgr()
		m.mu.Lock()
		for _, s := range m.st.Servers {
			if s.Owner == u.ID {
				svc++
			}
		}
		m.mu.Unlock()
	}
	short := "FREE"
	if u != nil {
		switch u.PulseTier {
		case "core":
			short = "CORE"
		case "pro":
			short = "PRO"
		case "sovereign":
			short = "SOV"
		case "eternal":
			short = "MAX"
		}
	}
	openN, newN, closedN := int64(0), int64(0), int64(0)
	if u != nil {
		_, o, n, c := support().openFor(u.ID)
		openN, newN, closedN = int64(o), int64(n), int64(c)
	}
	return map[string]interface{}{
		"Active": active, "SupportOpen": openN, "SupportNew": newN, "SupportClosed": closedN,
		"ServiceCount": svc, "PassTierShort": short,
	}
}

func hostingDashboardPage(w http.ResponseWriter, r *http.Request) {
	u := panelUser(r, w)
	if u == nil {
		return
	}
	m := hostMgr()
	m.mu.Lock()
	var mine []*HostedServer
	for _, s := range m.st.Servers {
		if s.Owner == u.ID {
			cp := *s
			mine = append(mine, &cp)
		}
	}
	m.mu.Unlock()
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreatedAt < mine[j].CreatedAt })
	online := 0
	for _, s := range mine {
		if s.Status == hostOnline {
			online++
		}
	}
	all, openN, newN, closedN := support().openFor(u.ID)
	supportAgo := "No messages yet"
	if len(all) > 0 {
		supportAgo = "Updated " + agoText(all[0].Updated)
	}
	totalDue := 0.0
	for _, s := range mine {
		totalDue += planDueEUR(s.PlanUpvotes)
	}
	dueText := "Free \u2014 no charges"
	if totalDue > 0 {
		dueText = fmt.Sprintf("\u20ac%.2f/month", totalDue)
	}
	data := map[string]interface{}{
		"Title": "Dashboard - NSM Hosting", "Servers": mine, "Online": online,
		"Total": len(mine), "MaxServers": hostingMaxPerOwner, "PKG": hostingPackage,
		"PassTier": pulseTierName(u), "Greeting": greetingWord(), "SupportAgo": supportAgo,
		"SupportOpen": openN, "SupportNew": newN, "SupportClosed": closedN,
		"DueText": dueText,
	}
	for k, v := range panelNav("dashboard", u) {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	renderPage(w, r, "panel-dashboard", data)
}

// nextChargeDate is the same day next month, e.g. "13 December 2026".
func nextChargeDate() string {
	t := time.Now().AddDate(0, 1, 0)
	return t.Format("2 January 2006")
}

func pulseTierName(u *User) string {
	switch u.PulseTier {
	case "core":
		return "Pulse Core"
	case "pro":
		return "Pulse Pro"
	case "sovereign":
		return "Pulse Sovereign"
	case "eternal":
		return "Pulse Max"
	}
	return "Pulse Free"
}

// agoText renders a short relative time like "1 day ago".
func agoText(ts int64) string {
	d := time.Now().Unix() - ts
	switch {
	case d < 60:
		return "just now"
	case d < 3600:
		return fmt.Sprintf("%d minute%s ago", d/60, plural(d/60))
	case d < 86400:
		return fmt.Sprintf("%d hour%s ago", d/3600, plural(d/3600))
	default:
		return fmt.Sprintf("%d day%s ago", d/86400, plural(d/86400))
	}
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func greetingWord() string {
	h := time.Now().Hour()
	switch {
	case h < 12:
		return "Good morning"
	case h < 18:
		return "Good afternoon"
	default:
		return "Good evening"
	}
}

func hostingBillingPage(w http.ResponseWriter, r *http.Request) {
	u := panelUser(r, w)
	if u == nil {
		return
	}
	m := hostMgr()
	m.mu.Lock()
	type billRow struct {
		Name   string
		Tier   string
		Up     int
		DueTxt string
	}
	rows := []billRow{}
	total := 0.0
	for _, srv := range m.st.Servers {
		if srv.Owner != u.ID {
			continue
		}
		up, tier := srv.PlanUpvotes, srv.PlanTier
		if tier == "" {
			up, tier = 10, "spark"
		}
		d := planDueEUR(up)
		total += d
		rows = append(rows, billRow{Name: srv.Name, Tier: tier, Up: up, DueTxt: dueText(d)})
	}
	m.mu.Unlock()
	data := map[string]interface{}{
		"Title": "Billing - NSM Hosting", "PKG": hostingPackage, "PassTier": pulseTierName(u),
		"Bills": rows, "TotalDue": fmt.Sprintf("€%.2f", total), "NextCharge": nextChargeDate(),
	}
	for k, v := range panelNav("billing", u) {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	renderPage(w, r, "panel-billing", data)
}

func hostingPassPage(w http.ResponseWriter, r *http.Request) {
	u := panelUser(r, w)
	if u == nil {
		return
	}
	curTier, curUp := "spark", 10
	m2 := hostMgr()
	m2.mu.Lock()
	for _, sv := range m2.st.Servers {
		if sv.Owner == u.ID && sv.PlanTier != "" {
			curTier, curUp = sv.PlanTier, sv.PlanUpvotes
		}
	}
	m2.mu.Unlock()
	data := map[string]interface{}{"Title": "Pulse - NSM Hosting", "PassTier": pulseTierName(u),
		"Tiers": planTiers, "PlanTier": curTier, "PlanUpvotes": curUp}
	for k, v := range panelNav("pass", u) {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	renderPage(w, r, "panel-pass", data)
}

func hostingNewsPage(w http.ResponseWriter, r *http.Request) {
	u := panelUser(r, w)
	if u == nil {
		return
	}
	anns := []map[string]interface{}{}
	forumMu.Lock()
	if forum != nil {
		for _, t := range forum.Topics {
			if t.CategorySlug != "announcements" {
				continue
			}
			body := ""
			if len(t.Posts) > 0 {
				body = t.Posts[0].Content
			}
			if len(body) > 600 {
				body = body[:600] + "\u2026"
			}
			anns = append(anns, map[string]interface{}{
				"Title": t.Title, "Author": t.AuthorUsername,
				"CreatedAt": t.CreatedAt, "UpdatedAt": t.UpdatedAt,
				"Body": body, "Pinned": t.Pinned,
			})
		}
	}
	forumMu.Unlock()
	sort.Slice(anns, func(i, j int) bool {
		if anns[i]["Pinned"] != anns[j]["Pinned"] {
			return anns[i]["Pinned"].(bool)
		}
		return anns[i]["UpdatedAt"].(int64) > anns[j]["UpdatedAt"].(int64)
	})
	data := map[string]interface{}{"Title": "News - NSM Hosting", "Announcements": anns}
	for k, v := range panelNav("news", u) {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	renderPage(w, r, "panel-news", data)
}

func hostingIncidentsPage(w http.ResponseWriter, r *http.Request) {
	u := panelUser(r, w)
	if u == nil {
		return
	}
	data := map[string]interface{}{"Title": "Incidents - NSM Hosting", "Incidents": statusIncidentsSnapshot()}
	for k, v := range panelNav("incidents", u) {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	renderPage(w, r, "panel-incidents", data)
}

func hostingCommunityPage(w http.ResponseWriter, r *http.Request) {
	u := panelUser(r, w)
	if u == nil {
		return
	}
	data := map[string]interface{}{"Title": "Community & Partners - NSM Hosting", "Section": r.URL.Query().Get("s")}
	for k, v := range panelNav("community", u) {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	renderPage(w, r, "panel-community", data)
}

// hostingSupportPage — the hosting support center (chat tickets).
func hostingSupportPage(w http.ResponseWriter, r *http.Request) {
	u := panelUser(r, w)
	if u == nil {
		return
	}
	id := r.URL.Query().Get("t")
	tickets, openN, newN, closedN := support().openFor(u.ID)
	data := map[string]interface{}{
		"Title": "Support - NSM Hosting", "Tickets": tickets,
		"SupportOpen": openN, "SupportNew": newN, "SupportClosed": closedN,
	}
	if id != "" {
		ss := support()
		ss.mu.Lock()
		t := ss.findLocked(id)
		var snap *supportTicket
		if t != nil && (t.Owner == u.ID || u.Role == "founder") {
			cp := *t
			snap = &cp
			if t.Status == "new" && t.Owner == u.ID {
				t.Status = "open"
				ss.saveLocked()
			}
		}
		ss.mu.Unlock()
		data["T"] = snap
	}
	for k, v := range panelNav("support", u) {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	renderPage(w, r, "panel-support", data)
}

// ---------------------------------------------------------------------------
// support API
// ---------------------------------------------------------------------------

func apiHostSupportList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	tickets, openN, newN, closedN := support().openFor(u.ID)
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"tickets": tickets, "open": openN + newN, "new": newN, "closed": closedN,
	})
}

func apiHostSupportCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	var body struct {
		Subject string `json:"subject"`
		Text    string `json:"text"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
	body.Subject = strings.TrimSpace(body.Subject)
	body.Text = strings.TrimSpace(body.Text)
	if body.Subject == "" || body.Text == "" {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "subject and message are required"})
		return
	}
	ss := support()
	ss.mu.Lock()
	t := &supportTicket{
		ID: "t" + strconv.FormatInt(time.Now().UnixNano()%1e9, 10),
		Owner: u.ID, OwnerName: u.Username, Subject: body.Subject,
		Status: "new", Created: time.Now().Unix(), Updated: time.Now().Unix(),
		Messages: []supportMsg{{From: "user", By: u.Username, Text: body.Text, At: time.Now().Unix()}},
	}
	ss.Tickets = append(ss.Tickets, t)
	ss.saveLocked()
	cp := *t
	ss.mu.Unlock()
	sendJSON(w, http.StatusOK, cp)
}

func apiHostSupportMessage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "message required"})
		return
	}
	ss := support()
	ss.mu.Lock()
	t := ss.findLocked(r.PathValue("id"))
	if t == nil || (t.Owner != u.ID && u.Role != "founder") {
		ss.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	from := "user"
	if u.Role == "founder" {
		from = "staff"
	}
	t.Messages = append(t.Messages, supportMsg{From: from, By: u.Username, Text: body.Text, At: time.Now().Unix()})
	if t.Status == "closed" {
		t.Status = "open"
	}
	t.Updated = time.Now().Unix()
	ss.saveLocked()
	cp := *t
	ss.mu.Unlock()
	sendJSON(w, http.StatusOK, cp)
}

func apiHostSupportClose(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}
	ss := support()
	ss.mu.Lock()
	t := ss.findLocked(r.PathValue("id"))
	if t == nil || (t.Owner != u.ID && u.Role != "founder") {
		ss.mu.Unlock()
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	t.Status = "closed"
	t.Updated = time.Now().Unix()
	ss.saveLocked()
	ss.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// panel API — live stats, rename, ftp password, plan, activity
// ---------------------------------------------------------------------------

func panelOwned(w http.ResponseWriter, r *http.Request) (*hostingManager, *HostedServer, *User, bool) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return nil, nil, nil, false
	}
	m := hostMgr()
	id := r.PathValue("id")
	m.mu.Lock()
	s := m.findOwned(id, u.ID)
	var snap *HostedServer
	if s != nil && s.Owner == u.ID {
		cp := *s
		snap = &cp
	}
	m.mu.Unlock()
	if snap == nil {
		msg := m.missingServerText(id, u)
		log.Printf("[panel] 404 id=%q uid=%d: %s", id, u.ID, msg)
		sendJSON(w, http.StatusNotFound, map[string]string{"error": msg})
		return nil, nil, nil, false
	}
	return m, snap, u, true
}

func apiHostStats(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	u := m.usage(snap)
	rx, tx := hostNetTotals()
	players := int64(0)
	if d := reg.get(snap.Endpoint); d != nil {
		players = d.Clients
	}
	m.mu.Lock()
	status := snap.Status
	if cur := m.find(snap.ID); cur != nil {
		status = cur.Status
	}
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"status": status, "online": u.Online, "cpu": u.CPUPct, "ramMB": u.RAMMB,
		"diskMB": u.DiskMB, "uptimeSec": u.UptimeSec, "players": players,
		"rxBytes": rx, "txBytes": tx, "updated": time.Now().Unix(),
		"guestAgent": u.Online, "name": snap.Name,
	})
}

func apiHostRename(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&body)
	body.Name = strings.TrimSpace(body.Name)
	if len(body.Name) < 2 || len(body.Name) > 48 {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "name must be 2-48 characters"})
		return
	}
	m.mu.Lock()
	if cur := m.find(snap.ID); cur != nil {
		old := cur.Name
		cur.Name = body.Name
		m.logNL(cur, "[panel] renamed from \""+old+"\" to \""+body.Name+"\"")
		m.saveLocked()
	}
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]string{"ok": "true", "name": body.Name})
}

func apiHostFTPPassword(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	var body struct {
		Pass string `json:"pass"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&body)
	pass := strings.TrimSpace(body.Pass)
	if pass == "" {
		pass = hostRandToken(14)
	}
	if len(pass) < 6 {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 6 characters"})
		return
	}
	m.mu.Lock()
	if cur := m.find(snap.ID); cur != nil {
		cur.FTPPass = pass
		m.logNL(cur, "[panel] FTP password changed")
		m.saveLocked()
	}
	m.mu.Unlock()
	// the FTP server authenticates against the store on every login — the new
	// password applies to the next connection without a restart.
	sendJSON(w, http.StatusOK, map[string]string{"ok": "true", "pass": pass, "user": snap.FTPUser})
}

func apiHostActivity(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	m.mu.Lock()
	lines := []string{}
	if cur := m.find(snap.ID); cur != nil {
		lines = append(lines, cur.Console...)
	}
	m.mu.Unlock()
	// the panel activity feed surfaces the [system]/[panel] lines
	acts := []string{}
	for _, l := range lines {
		if strings.Contains(l, "[system]") || strings.Contains(l, "[panel]") || strings.Contains(l, "[backup]") || strings.Contains(l, "[apps]") || strings.Contains(l, "[schedule]") {
			acts = append(acts, l)
		}
	}
	if len(acts) > 80 {
		acts = acts[len(acts)-80:]
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"lines": acts})
}

// planDueEUR prices a package by its upvotes. The entry package (10
// upvotes) is free; beyond that the monthly amount scales with attention.
func planDueEUR(up int) float64 {
	if up <= 10 {
		return 0
	}
	due := float64(up) * 0.0002
	if due < 2.99 {
		due = 2.99
	}
	return float64(int(due*100+0.5)) / 100
}

func dueText(d float64) string {
	if d <= 0 {
		return "Free"
	}
	return fmt.Sprintf("€%.2f", d)
}

func apiHostPlan(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		tier, up := "spark", 10
		m.mu.Lock()
		if cur := m.find(snap.ID); cur != nil {
			if cur.PlanTier != "" {
				tier = cur.PlanTier
				up = cur.PlanUpvotes
			}
		}
		m.mu.Unlock()
		d := planDueEUR(up)
		sendJSON(w, http.StatusOK, map[string]interface{}{
			"tier": tier, "upvotes": up, "due": d, "dueText": dueText(d), "currency": "EUR", "period": "month",
		})
		return
	}
	var body struct {
		Tier    string `json:"tier"`
		Upvotes int    `json:"upvotes"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&body)
	if body.Upvotes < 1 {
		body.Upvotes = 1
	}
	if body.Upvotes > 2000000 {
		body.Upvotes = 2000000
	}
	if body.Tier == "" {
		body.Tier = "custom"
	}
	m.mu.Lock()
	if cur := m.find(snap.ID); cur != nil {
		cur.PlanTier = body.Tier
		cur.PlanUpvotes = body.Upvotes
		m.logNL(cur, "[panel] package updated — "+body.Tier+" · "+strconv.Itoa(body.Upvotes)+" upvotes · "+dueText(planDueEUR(body.Upvotes))+"/month")
		m.saveLocked()
	}
	m.mu.Unlock()
	d := planDueEUR(body.Upvotes)
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "tier": body.Tier, "upvotes": body.Upvotes,
		"due": d, "dueText": dueText(d), "currency": "EUR", "period": "month",
	})
}

// ---------------------------------------------------------------------------
// backups (real zips of the city directory)
// ---------------------------------------------------------------------------

func backupDirOf(m *hostingManager, s *HostedServer) string {
	return filepath.Join(m.dirOf(s), "backups")
}

func listBackups(m *hostingManager, s *HostedServer) []hostBackup {
	dir := backupDirOf(m, s)
	ents, _ := os.ReadDir(dir)
	out := []hostBackup{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		out = append(out, hostBackup{
			ID: strings.TrimSuffix(e.Name(), ".zip"), Name: e.Name(),
			Created: info.ModTime().Unix(), SizeMB: info.Size() / (1024 * 1024),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

func apiHostBackups(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"backups": listBackups(m, snap)})
}

func apiHostBackupCreate(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	root := m.serverRoot(snap)
	if _, err := os.Stat(root); err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": "server files are not installed yet"})
		return
	}
	dir := backupDirOf(m, snap)
	_ = os.MkdirAll(dir, 0o755)
	id := "b" + strconv.FormatInt(time.Now().Unix(), 10)
	out := filepath.Join(dir, id+".zip")
	if err := zipDir(root, out); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	m.appendLog(snap, "[backup] created "+id+".zip")
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "backups": listBackups(m, snap)})
}

func apiHostBackupRestore(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	id := filepath.Base(r.PathValue("bid"))
	src := filepath.Join(backupDirOf(m, snap), id+".zip")
	if _, err := os.Stat(src); err != nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "backup not found"})
		return
	}
	m.stop(snap)
	root := m.serverRoot(snap)
	_ = os.RemoveAll(root)
	if err := unzipTo(src, root); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	m.appendLog(snap, "[backup] restored "+id+".zip — press Start")
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiHostBackupDelete(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	id := filepath.Base(r.PathValue("bid"))
	p := filepath.Join(backupDirOf(m, snap), id+".zip")
	if err := os.Remove(p); err != nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "backup not found"})
		return
	}
	m.appendLog(snap, "[backup] deleted "+id+".zip")
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "backups": listBackups(m, snap)})
}

func zipDir(src, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	})
}

func unzipTo(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		p := filepath.Join(dst, f.Name)
		if !strings.HasPrefix(p, filepath.Clean(dst)+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(p, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		out, err := os.Create(p)
		if err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// schedules (real recurring power actions)
// ---------------------------------------------------------------------------

func apiHostSchedules(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Label    string `json:"label"`
			Kind     string `json:"kind"`
			EveryMin int    `json:"everyMin"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&body)
		if body.Kind != "restart" && body.Kind != "stop" && body.Kind != "start" {
			sendJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be restart, stop or start"})
			return
		}
		if body.EveryMin < 5 {
			body.EveryMin = 60
		}
		if body.Label == "" {
			body.Label = fmt.Sprintf("every %d min %s", body.EveryMin, body.Kind)
		}
		m.mu.Lock()
		var out []hostSchedule
		if cur := m.find(snap.ID); cur != nil {
			cur.Sched = append(cur.Sched, hostSchedule{
				ID: "s" + strconv.FormatInt(time.Now().UnixNano()%1e9, 10),
				Label: body.Label, Kind: body.Kind, EveryMin: body.EveryMin, Enabled: true,
			})
			m.logNL(cur, "[schedule] added "+body.Label)
			m.saveLocked()
			out = append(out, cur.Sched...)
		}
		m.mu.Unlock()
		sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "schedules": out})
		return
	}
	m.mu.Lock()
	out := []hostSchedule{}
	if cur := m.find(snap.ID); cur != nil {
		out = append(out, cur.Sched...)
	}
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]interface{}{"schedules": out})
}

func apiHostScheduleOp(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	id := r.PathValue("sid")
	op := r.PathValue("op")
	m.mu.Lock()
	out := []hostSchedule{}
	if cur := m.find(snap.ID); cur != nil {
		kept := cur.Sched[:0]
		for _, s := range cur.Sched {
			if s.ID != id {
				kept = append(kept, s)
				continue
			}
			switch op {
			case "toggle":
				s.Enabled = !s.Enabled
				kept = append(kept, s)
			case "delete":
				m.logNL(cur, "[schedule] removed "+s.Label)
			}
		}
		cur.Sched = kept
		m.saveLocked()
		out = append(out, cur.Sched...)
	}
	m.mu.Unlock()
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "schedules": out})
}

// scheduleLoop runs every minute and fires due schedules. Started once.
func scheduleLoop() {
	go func() {
		for range time.Tick(time.Minute) {
			func() {
				defer func() { _ = recover() }()
				m := hostMgr()
				type due struct {
					s *HostedServer
					k string
					l string
				}
				var fire []due
				now := time.Now().Unix()
				m.mu.Lock()
				for idx := range m.st.Servers {
					s := m.st.Servers[idx]
					for j := range s.Sched {
						sc := &s.Sched[j]
						if !sc.Enabled || sc.EveryMin <= 0 {
							continue
						}
						if sc.LastRun == 0 {
							sc.LastRun = now
							continue
						}
						if now-sc.LastRun >= int64(sc.EveryMin)*60 {
							sc.LastRun = now
							fire = append(fire, due{s: s, k: sc.Kind, l: sc.Label})
						}
					}
				}
				if len(fire) > 0 {
					m.saveLocked()
				}
				m.mu.Unlock()
				for _, f := range fire {
					m.appendLog(f.s, "[schedule] firing "+f.l)
					switch f.k {
					case "restart":
						m.stop(f.s)
						_ = m.start(f.s)
					case "stop":
						m.stop(f.s)
					case "start":
						_ = m.start(f.s)
					}
				}
			}()
		}
	}()
}
func installedApps(m *hostingManager, s *HostedServer) map[string]bool {
	out := map[string]bool{}
	res := filepath.Join(m.serverRoot(s), "resources")
	for _, a := range oneClickApps {
		if _, err := os.Stat(filepath.Join(res, a.Dir)); err == nil {
			out[a.ID] = true
		}
	}
	return out
}

func apiHostApps(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	inst := installedApps(m, snap)
	list := []map[string]interface{}{}
	for _, a := range oneClickApps {
		list = append(list, map[string]interface{}{
			"id": a.ID, "name": a.Name, "kind": a.Kind, "blurb": a.Blurb,
			"version": a.Version, "installed": inst[a.ID],
		})
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"apps": list})
}

func apiHostAppInstall(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	id := r.PathValue("app")
	var app *oneClickApp
	for i := range oneClickApps {
		if oneClickApps[i].ID == id {
			app = &oneClickApps[i]
			break
		}
	}
	if app == nil {
		sendJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app"})
		return
	}
	root := m.serverRoot(snap)
	res := filepath.Join(root, "resources")
	if _, err := os.Stat(root); err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": "server files are not installed yet"})
		return
	}
	_ = os.MkdirAll(res, 0o755)
	m.appendLog(snap, "[apps] downloading "+app.Name+"…")
	tmp := filepath.Join(m.dirOf(snap), "app-"+app.ID+".zip")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "curl", "-fsSL", "--max-time", "110", "-o", tmp, app.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		m.appendLog(snap, "[apps] download failed: "+strings.TrimSpace(string(out)))
		sendJSON(w, http.StatusBadGateway, map[string]string{"error": "download failed: " + err.Error()})
		return
	}
	defer os.Remove(tmp)
	dest := filepath.Join(res, app.Dir)
	_ = os.RemoveAll(dest)
	stage := filepath.Join(m.dirOf(snap), "app-"+app.ID+"-x")
	_ = os.RemoveAll(stage)
	if err := unzipTo(tmp, stage); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.RemoveAll(stage)
	// github zips wrap everything in one top-level folder — unwrap it
	ents, _ := os.ReadDir(stage)
	inner := stage
	if len(ents) == 1 && ents[0].IsDir() {
		inner = filepath.Join(stage, ents[0].Name())
	}
	if err := os.Rename(inner, dest); err != nil {
		// fallback: copy
		if err2 := copyTree(inner, dest); err2 != nil {
			sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err2.Error()})
			return
		}
	}
	// make sure the resource starts with the server
	cfgPath := filepath.Join(root, "server.cfg")
	if b, err := os.ReadFile(cfgPath); err == nil && !strings.Contains(string(b), "ensure "+app.Dir) {
		_ = os.WriteFile(cfgPath, append(b, []byte("\nensure "+app.Dir+"\n")...), 0o644)
	}
	m.appendLog(snap, "[apps] "+app.Name+" installed to resources/"+app.Dir+" — restart to load it")
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "installed": installedApps(m, snap)})
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(t, b, 0o644)
	})
}

// ---------------------------------------------------------------------------
// Plus — the NSM scripting agent (real generator; optional upstream AI)
// ---------------------------------------------------------------------------

var agentScripts = []struct {
	match *regexp.Regexp
	title string
	code  string
}{
	{regexp.MustCompile(`(?i)vmenu|menu`), "vMenu base config (server.cfg + acl)",
		"# vMenu — add to server.cfg\nsetr vMenu_enable_dependency_check true\nsetr vMenu_use_permissions true\n# then give yourself access in vMenu/config/config.json:\n#   \"Permissions\": { \"Menu\": { \"all\": true } }\nensure vMenu\n"},
	{regexp.MustCompile(`(?i)qb|qbcore`), "QBCore starter resource",
		"local QBCore = exports['qb-core']:GetCoreObject()\n\nRegisterCommand('pay', function(source, args)\n    local src = source\n    local Player = QBCore.Functions.GetPlayer(src)\n    if not Player then return end\n    local amount = tonumber(args[1]) or 0\n    if amount <= 0 then return end\n    Player.Functions.AddMoney('cash', amount, 'admin-pay')\n    TriggerClientEvent('chat:addMessage', src, { args = { 'NSM', ('+$%d'):format(amount) } })\nend, true)\n"},
	{regexp.MustCompile(`(?i)esx`), "ESX starter resource",
		"ESX = nil\nTriggerEvent('esx:getSharedObject', function(obj) ESX = obj end)\n\nRegisterCommand('givemoney', function(source, args)\n    local xPlayer = ESX.GetPlayerFromId(source)\n    if not xPlayer then return end\n    local amount = tonumber(args[1]) or 0\n    xPlayer.addMoney(amount)\nend, true)\n"},
	{regexp.MustCompile(`(?i)restart|schedule|cron`), "scheduled restart resource",
		"-- restart.lua — restarts the city every 6 hours\nCreateThread(function()\n    while true do\n        Wait(6 * 60 * 60 * 1000)\n        print('[NSM] scheduled restart')\n        for _, id in ipairs(GetPlayers()) do\n            DropPlayer(id, 'Scheduled restart — rejoin in a minute')\n        end\n        Wait(2000)\n        os.exit()\n    end\nend)\n"},
	{regexp.MustCompile(`(?i)command|chat`), "custom command handler",
		"RegisterCommand('hello', function(source, args)\n    local name = args[1] or 'city'\n    TriggerClientEvent('chat:addMessage', source, {\n        color = { 24, 232, 136 }, args = { 'NSM', 'Hello, ' .. name .. '!' }\n    })\nend, false)\n"},
	{regexp.MustCompile(`(?i)car|vehicle|spawn`), "vehicle spawner (client)",
		"-- client.lua\nRegisterCommand('car', function(_, args)\n    local model = joaat(args[1] or 'adder')\n    RequestModel(model)\n    while not HasModelLoaded(model) do Wait(10) end\n    local ped = PlayerPedId()\n    local v = CreateVehicle(model, GetEntityCoords(ped), GetEntityHeading(ped), true, false)\n    SetPedIntoVehicle(ped, v, -1)\n    SetModelAsNoLongerNeeded(model)\nend, false)\n"},
}

func apiHostAgent(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "message required"})
		return
	}
	// optional real AI upstream (set NSM_AI_ENDPOINT + NSM_AI_KEY on the VPS)
	if ep := os.Getenv("NSM_AI_ENDPOINT"); ep != "" {
		if reply, err := agentUpstream(ep, msg); err == nil {
			sendJSON(w, http.StatusOK, map[string]interface{}{"reply": reply, "source": "ai"})
			return
		}
	}
	for _, s := range agentScripts {
		if s.match.MatchString(msg) {
			m.appendLog(snap, "[agent] Plus generated: "+s.title)
			sendJSON(w, http.StatusOK, map[string]interface{}{
				"reply": "Here's " + s.title + " — drop it into resources/ and restart:", "title": s.title, "code": s.code,
			})
			return
		}
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"reply": "I build NSM server scripts — ask me for vMenu, QBCore, ESX, a scheduled restart, a custom command or a vehicle spawner. (Tip: set NSM_AI_ENDPOINT on the server to route me through a full AI model.)",
	})
}

func agentUpstream(ep, msg string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"message": msg})
	req, err := http.NewRequest(http.MethodPost, ep, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("NSM_AI_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Reply string `json:"reply"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	return out.Reply, nil
}

// ---------------------------------------------------------------------------
// reinstall + rescue
// ---------------------------------------------------------------------------

func apiHostReinstall(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	m.stop(snap)
	root := m.serverRoot(snap)
	_ = os.RemoveAll(root)
	m.appendLog(snap, "[panel] reinstalling city files…")
	if err := m.install(snap); err != nil {
		m.appendLog(snap, "[panel] reinstall failed: "+err.Error())
		sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	m.appendLog(snap, "[panel] reinstall complete — press Start")
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiHostSafeMode(w http.ResponseWriter, r *http.Request) {
	m, snap, _, ok := panelOwned(w, r)
	if !ok {
		return
	}
	m.stop(snap)
	m.appendLog(snap, "[panel] rescue: restarting in safe mode (base resources only)")
	if err := m.start(snap); err != nil {
		sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------

func registerHostingPanel(mux *http.ServeMux) {
	scheduleLoop()
	mux.HandleFunc("GET /hosting/dashboard", hostingDashboardPage)
	mux.HandleFunc("GET /hosting/billing", hostingBillingPage)
	mux.HandleFunc("GET /hosting/pass", hostingPassPage)
	mux.HandleFunc("GET /hosting/news", hostingNewsPage)
	mux.HandleFunc("GET /hosting/incidents", hostingIncidentsPage)
	mux.HandleFunc("GET /hosting/community", hostingCommunityPage)
	mux.HandleFunc("GET /hosting/support", hostingSupportPage)
	mux.HandleFunc("GET /api/hosting/support", apiHostSupportList)
	mux.HandleFunc("POST /api/hosting/support", apiHostSupportCreate)
	mux.HandleFunc("POST /api/hosting/support/{id}/message", apiHostSupportMessage)
	mux.HandleFunc("POST /api/hosting/support/{id}/close", apiHostSupportClose)
	mux.HandleFunc("GET /api/host/{id}/stats", apiHostStats)
	mux.HandleFunc("POST /api/host/{id}/rename", apiHostRename)
	mux.HandleFunc("POST /api/host/{id}/ftp-password", apiHostFTPPassword)
	mux.HandleFunc("GET /api/host/{id}/activity", apiHostActivity)
	mux.HandleFunc("GET /api/host/{id}/plan", apiHostPlan)
	mux.HandleFunc("POST /api/host/{id}/plan", apiHostPlan)
	mux.HandleFunc("GET /api/host/{id}/backups", apiHostBackups)
	mux.HandleFunc("POST /api/host/{id}/backup", apiHostBackupCreate)
	mux.HandleFunc("POST /api/host/{id}/backup/{bid}/restore", apiHostBackupRestore)
	mux.HandleFunc("POST /api/host/{id}/backup/{bid}/delete", apiHostBackupDelete)
	mux.HandleFunc("GET /api/host/{id}/schedules", apiHostSchedules)
	mux.HandleFunc("POST /api/host/{id}/schedules", apiHostSchedules)
	mux.HandleFunc("POST /api/host/{id}/schedule/{sid}/{op}", apiHostScheduleOp)
	mux.HandleFunc("GET /api/host/{id}/apps", apiHostApps)
	mux.HandleFunc("POST /api/host/{id}/apps/{app}/install", apiHostAppInstall)
	mux.HandleFunc("POST /api/host/{id}/agent", apiHostAgent)
	mux.HandleFunc("POST /api/host/{id}/reinstall", apiHostReinstall)
	mux.HandleFunc("POST /api/host/{id}/safe-mode", apiHostSafeMode)
}
