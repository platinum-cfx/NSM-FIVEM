package main

// NSM Status — live health monitoring for the platform's own services.
// Every component is measured with real probes (never hardcoded): the
// monitor checks the site, the server list API, the NSM.exe client feed,
// heartbeat ingestion, join links and accounts on an interval, keeps 90
// days of uptime samples, and opens/resolves incidents automatically.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// model
// ---------------------------------------------------------------------------

type statusSample struct {
	T  int64 `json:"t"`
	OK bool  `json:"ok"`
}

type statusDay struct {
	Up    int `json:"up"`
	Total int `json:"total"`
}

type statusIncidentUpdate struct {
	T     int64  `json:"t"`
	State string `json:"state"` // investigating / monitoring / resolved
	Text  string `json:"text"`
}

type statusIncident struct {
	Component string                 `json:"component"`
	Name      string                 `json:"name"`
	Started   int64                  `json:"started"`
	Resolved  int64                  `json:"resolved"` // 0 = ongoing
	Updates   []statusIncidentUpdate `json:"updates"`
}

type statusComponent struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Recent      []statusSample        `json:"recent"` // ring, newest last
	Days        map[string]*statusDay `json:"days"`   // "2006-01-02" UTC
	LastChecked int64                 `json:"lastChecked"`
	Fails       int                   `json:"fails"` // consecutive failures
	Passes      int                   `json:"passes"`
	Open        bool                  `json:"open"` // incident currently open
}

// ---------------------------------------------------------------------------
// probes — one per platform service
// ---------------------------------------------------------------------------

type statusProbe struct {
	id, name, desc string
	path           string
	accept         []int  // HTTP statuses counted as healthy
	bodyNeed       string // optional substring that must appear
}

var statusProbeList = []statusProbe{
	{"website", "Website & Dashboard", "The NSM website, pages and downloads", "/", []int{200}, ""},
	{"list-api", "Server List API", "Live server list data for the website and apps", "/api/servers/live", []int{200}, `"servers"`},
	{"client-feed", "NSM.exe Client Feed", "The binary list feed the NSM.exe browser reads", "/api/servers/streamRedir/", []int{200}, ""},
	{"ingest", "Server Ingestion", "Game server heartbeats and listing updates", "/api/serverlist/ingress", []int{405}, ""}, // GET must be 405: route mounted, POST-only
	{"join", "Join Links", "Shareable /join/ connect links", "/join/status-check", []int{200, 301, 302, 404}, ""},
	{"auth", "Accounts & Sign-In", "Login, registration and account sessions", "/login", []int{200}, ""},
}

// statusDoRequest performs one probe request and returns (statusCode, body).
// The default talks to the live listener over loopback; tests swap it for an
// in-process handler.
var statusDoRequest = func(path string) (int, string, error) {
	base := strings.TrimRight(statusProbeBase, "/")
	resp, err := statusHTTPClient.Get(base + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(b), nil
}

var statusHTTPClient = &http.Client{Timeout: 5 * time.Second}
var statusProbeBase = "http://127.0.0.1:80"

// ---------------------------------------------------------------------------
// state
// ---------------------------------------------------------------------------

var (
	statusMu        sync.Mutex
	statusMon       = map[string]*statusComponent{}
	statusIncidents []statusIncident
)

var statusFilePathOverride = "" // tests point this at a temp file

func statusPath() string {
	if statusFilePathOverride != "" {
		return statusFilePathOverride
	}
	return filepath.Join(filepath.Dir(statePath()), "status.json")
}

type statusFileShape struct {
	Components map[string]*statusComponent `json:"components"`
	Incidents  []statusIncident            `json:"incidents"`
}

func loadStatus() {
	b, err := os.ReadFile(statusPath())
	if err != nil {
		return
	}
	var s statusFileShape
	if json.Unmarshal(b, &s) != nil || s.Components == nil {
		return
	}
	statusMu.Lock()
	statusMon = s.Components
	statusIncidents = s.Incidents
	statusMu.Unlock()
}

func saveStatusLocked() {
	shape := statusFileShape{Components: statusMon, Incidents: statusIncidents}
	b, err := json.Marshal(&shape)
	if err != nil {
		return
	}
	tmp := statusPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, statusPath())
	}
}

// ---------------------------------------------------------------------------
// check cycle
// ---------------------------------------------------------------------------

const (
	statusRecentKeep    = 240 // samples kept per component (~2h at 60s)
	statusDayKeep       = 90  // days of uptime history
	statusIncidentKeep  = 20
	statusFailThreshold = 3 // consecutive failures open an incident
	statusPassThreshold = 3 // consecutive passes resolve it
)

func statusNow() int64 { return time.Now().Unix() }

// runStatusCycle probes every component once and updates samples, uptime
// days and incidents.
func runStatusCycle() {
	now := statusNow()
	day := time.Unix(now, 0).UTC().Format("2006-01-02")

	for _, p := range statusProbeList {
		code, body, err := statusDoRequest(p.path)
		ok := err == nil
		if ok {
			ok = false
			for _, a := range p.accept {
				if code == a {
					ok = true
					break
				}
			}
			if ok && p.bodyNeed != "" && !strings.Contains(body, p.bodyNeed) {
				ok = false
			}
		}

		statusMu.Lock()
		c := statusMon[p.id]
		if c == nil {
			c = &statusComponent{ID: p.id, Name: p.name, Description: p.desc, Days: map[string]*statusDay{}}
			statusMon[p.id] = c
		}
		c.Name, c.Description = p.name, p.desc
		c.LastChecked = now
		c.Recent = append(c.Recent, statusSample{T: now, OK: ok})
		if len(c.Recent) > statusRecentKeep {
			c.Recent = c.Recent[len(c.Recent)-statusRecentKeep:]
		}
		if c.Days == nil {
			c.Days = map[string]*statusDay{}
		}
		d := c.Days[day]
		if d == nil {
			d = &statusDay{}
			c.Days[day] = d
		}
		d.Total++
		if ok {
			d.Up++
		}
		for k := range c.Days {
			if len(c.Days) > statusDayKeep {
				delete(c.Days, k)
			}
		}

		if ok {
			c.Fails = 0
			if c.Open {
				c.Passes++
				if c.Passes == 1 {
					statusAddUpdateLocked(p.id, p.name, "monitoring", "Service has recovered; monitoring.")
				}
				if c.Passes >= statusPassThreshold {
					c.Open = false
					c.Passes = 0
					for i := range statusIncidents {
						if statusIncidents[i].Component == p.id && statusIncidents[i].Resolved == 0 {
							statusIncidents[i].Resolved = now
							statusIncidents[i].Updates = append(statusIncidents[i].Updates, statusIncidentUpdate{
								T: now, State: "resolved", Text: "Service is operating normally.",
							})
						}
					}
				}
			}
		} else {
			c.Passes = 0
			c.Fails++
			if c.Fails == statusFailThreshold && !c.Open {
				c.Open = true
				statusIncidents = append([]statusIncident{{
					Component: p.id, Name: p.name, Started: now,
					Updates: []statusIncidentUpdate{{
						T: now, State: "investigating",
						Text: "Automated monitoring detected failures on " + p.name + ".",
					}},
				}}, statusIncidents...)
				if len(statusIncidents) > statusIncidentKeep {
					statusIncidents = statusIncidents[:statusIncidentKeep]
				}
			}
		}
		saveStatusLocked()
		statusMu.Unlock()
	}
}

// statusAddUpdateLocked appends an update to the newest open incident for a
// component. Caller holds statusMu.
func statusAddUpdateLocked(id, name, state, text string) {
	for i := range statusIncidents {
		if statusIncidents[i].Component == id && statusIncidents[i].Resolved == 0 {
			statusIncidents[i].Updates = append(statusIncidents[i].Updates, statusIncidentUpdate{
				T: statusNow(), State: state, Text: text,
			})
			return
		}
	}
}

// startStatusMonitor loads history and runs check cycles forever.
// Only called from main(); tests drive runStatusCycle directly.
func startStatusMonitor(probeBase string) {
	statusProbeBase = probeBase
	loadStatus()
	go func() {
		// give the HTTP listener a moment to come up before the first probe
		time.Sleep(2 * time.Second)
		runStatusCycle()
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			runStatusCycle()
		}
	}()
}

// ---------------------------------------------------------------------------
// snapshot for the page + JSON API
// ---------------------------------------------------------------------------

type statusComponentView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Uptime      struct {
		Today float64 `json:"today"`
		D7    float64 `json:"d7"`
		D30   float64 `json:"d30"`
		D90   float64 `json:"d90"`
	} `json:"uptime"`
	LastChecked int64          `json:"lastChecked"`
	DayBars     []statusDayBar `json:"dayBars"`
	Recent      []int          `json:"recent"`
}

type statusDayBar struct {
	D string `json:"d"`
	P int    `json:"p"` // percent 0-100, -1 = no data
}

type statusView struct {
	Overall     string                `json:"overall"`
	OverallText string                `json:"overallText"`
	Updated     int64                 `json:"updated"`
	Components  []statusComponentView `json:"components"`
	Incidents   []statusIncident      `json:"incidents"`
}

// statusHealthLabel classifies a component from its recent samples.
func statusHealthLabel(c *statusComponent) string {
	n := len(c.Recent)
	if n == 0 {
		return "operational"
	}
	window := 30
	if n < window {
		window = n
	}
	fails := 0
	for _, s := range c.Recent[n-window:] {
		if !s.OK {
			fails++
		}
	}
	switch {
	case fails == 0:
		return "operational"
	case fails <= 2:
		return "degraded"
	case fails < window/2:
		return "partial-outage"
	default:
		return "major-outage"
	}
}

var statusSeverity = map[string]int{
	"operational": 0, "degraded": 1, "partial-outage": 2, "major-outage": 3,
}

var statusOverallText = map[string]string{
	"operational":    "All Systems Operational",
	"degraded":       "Minor Service Degradation",
	"partial-outage": "Partial System Outage",
	"major-outage":   "Major System Outage",
}

func statusDayPct(d *statusDay) int {
	if d == nil || d.Total == 0 {
		return -1
	}
	return int(float64(d.Up) / float64(d.Total) * 100)
}

func statusUptime(c *statusComponent, days int) float64 {
	since := time.Now().UTC().AddDate(0, 0, -days+1)
	up, total := 0, 0
	for k, d := range c.Days {
		t, err := time.Parse("2006-01-02", k)
		if err != nil || t.Before(since) {
			continue
		}
		up += d.Up
		total += d.Total
	}
	if total == 0 {
		return 100
	}
	return float64(up) / float64(total) * 100
}

func statusSnapshot() statusView {
	statusMu.Lock()
	defer statusMu.Unlock()

	v := statusView{Updated: statusNow(), Overall: "operational", Incidents: statusIncidents}
	if v.Incidents == nil {
		v.Incidents = []statusIncident{}
	}
	ids := make([]string, 0, len(statusMon))
	for id := range statusMon {
		ids = append(ids, id)
	}
	// stable order: the probe list order
	order := map[string]int{}
	for i, p := range statusProbeList {
		order[p.id] = i
	}
	sort.Slice(ids, func(i, j int) bool { return order[ids[i]] < order[ids[j]] })

	for _, id := range ids {
		c := statusMon[id]
		health := statusHealthLabel(c)
		if statusSeverity[health] > statusSeverity[v.Overall] {
			v.Overall = health
		}
		cv := statusComponentView{
			ID: c.ID, Name: c.Name, Description: c.Description,
			Status: health, LastChecked: c.LastChecked,
		}
		cv.Uptime.Today = statusUptime(c, 1)
		cv.Uptime.D7 = statusUptime(c, 7)
		cv.Uptime.D30 = statusUptime(c, 30)
		cv.Uptime.D90 = statusUptime(c, 90)
		for i := 89; i >= 0; i-- {
			k := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
			cv.DayBars = append(cv.DayBars, statusDayBar{D: k, P: statusDayPct(c.Days[k])})
		}
		n := len(c.Recent)
		window := 60
		if n < window {
			window = n
		}
		for _, s := range c.Recent[n-window:] {
			val := 0
			if s.OK {
				val = 1
			}
			cv.Recent = append(cv.Recent, val)
		}
		if cv.Recent == nil {
			cv.Recent = []int{}
		}
		v.Components = append(v.Components, cv)
	}
	v.OverallText = statusOverallText[v.Overall]
	return v
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

type statusIncidentUpdateView struct {
	State string
	TText string
	Text  string
}

type statusIncidentPageView struct {
	Name         string
	StartedText  string
	ResolvedText string
	Ongoing      bool
	Updates      []statusIncidentUpdateView
}

func statusPage(w http.ResponseWriter, r *http.Request) {
	v := statusSnapshot()
	views := make([]statusIncidentPageView, 0, len(v.Incidents))
	for _, in := range v.Incidents {
		pv := statusIncidentPageView{
			Name:        in.Name,
			StartedText: time.Unix(in.Started, 0).UTC().Format("Jan 2, 2006 15:04 UTC"),
			Ongoing:     in.Resolved == 0,
		}
		if in.Resolved != 0 {
			pv.ResolvedText = time.Unix(in.Resolved, 0).UTC().Format("Jan 2, 2006 15:04 UTC")
		} else {
			pv.ResolvedText = "ongoing"
		}
		for _, u := range in.Updates {
			pv.Updates = append(pv.Updates, statusIncidentUpdateView{
				State: u.State,
				TText: time.Unix(u.T, 0).UTC().Format("Jan 2, 2006 15:04 UTC"),
				Text:  u.Text,
			})
		}
		views = append(views, pv)
	}
	renderPage(w, r, "status.tmpl", map[string]interface{}{
		"Title":         "NSM Status - System Uptime",
		"Page":          "/status",
		"User":          currentUser(r),
		"Status":        v,
		"IncidentViews": views,
	})
}

func apiStatus(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, http.StatusOK, statusSnapshot())
}
