// NSM website: embedded pages served by the same exe.
// Templates in site/templates, static in site/static. Go 1.22 mux patterns.
package main

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed site
var siteFS embed.FS

var tmpl *template.Template

var colorCodeRe = regexp.MustCompile(`\^[0-9]`)

func cleanHostname(s string) string {
	s = strings.TrimSpace(strings.TrimRight(colorCodeRe.ReplaceAllString(s, ""), "\x00"))
	if s == "" || strings.EqualFold(s, "default FXServer") {
		return "AFXserver"
	}
	return s
}

func initSite() {
	if forum == nil {
		loadForum()
	}
	tmpl = template.Must(template.ParseFS(siteFS, "site/templates/*.tmpl"))
}

func accountCount() int64 {
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil {
		return 0
	}
	return int64(len(acc.Users))
}

func renderPage(w http.ResponseWriter, r *http.Request, name string, data map[string]interface{}) {
	if data == nil {
		data = map[string]interface{}{}
	}
	if _, hasUser := data["User"]; !hasUser {
		data["User"] = currentUser(r)
	}
	if _, hasPage := data["Page"]; !hasPage {
		data["Page"] = r.URL.Path
	}
	if _, hasStats := data["Stats"]; !hasStats {
		data["Stats"] = map[string]int64{
			"Players":  totalPlayers(),
			"Servers":  int64(len(reg.live())),
			"Accounts": accountCount(),
			"Posts":    totalForumPosts(),
		}
	}
	data["HasPendingClientAuth"] = hasPendingClientAuth()
	data["GoogleEnabled"] = googleEnabled()
	data["Year"] = time.Now().Year()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

// liveServers returns the current masterlist registry entries for the site and JSON API.
func liveServers() []map[string]interface{} {
	out := []map[string]interface{}{}
	for ep, e := range reg.live() {
		d := e.data
		applyMods(ep, &d)
		clean := cleanHostname(d.Hostname)
		initial := "S"
		if len(clean) > 0 {
			initial = strings.ToUpper(clean[:1])
		}
		desc := d.Vars["sv_projectDesc"]
		if desc == "" {
			desc = d.Vars["sv_projectName"]
		}
		var tags []string
		if rawTags := d.Vars["tags"]; rawTags != "" {
			for _, t := range strings.Split(rawTags, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tags = append(tags, t)
				}
			}
		}
		locale := strings.TrimSpace(d.Vars["locale"])
		if locale == "" || locale == "root-AQ" {
			locale = "en-US"
		}
		country := "US"
		if parts := strings.Split(strings.ReplaceAll(locale, "_", "-"), "-"); len(parts) >= 2 && len(parts[len(parts)-1]) == 2 {
			country = strings.ToUpper(parts[len(parts)-1])
		}
		onesync := d.Vars["onesync_enabled"]
		gameBuild := d.Vars["sv_enforceGameBuild"]
		pureLevel := d.Vars["sv_pureLevel"]
		scriptHook := d.Vars["sv_scriptHookAllowed"]
		bannerDetail := d.Vars["banner_detail"]
		bannerConnecting := d.Vars["banner_connecting"]
		upv := int(d.UpvotePower)
		burst := int(d.BurstPower)
		featured := isFeaturedServer(ep)
		joinCode := joinCodeForEndpointLookup(ep)
		joinID := joinCode
		if joinID == "" {
			joinID = ep
		}
		out = append(out, map[string]interface{}{
			// Template fields
			"EndPoint":         ep,
			"JoinCode":         joinCode,
			"Hostname":         d.Hostname,
			"CleanName":        clean,
			"Initial":          initial,
			"Desc":             desc,
			"Clients":          d.Clients,
			"MaxClients":       d.SVMaxclients,
			"GameType":         d.Gametype,
			"MapName":          d.Mapname,
			"Upvotes":          upv,
			"BurstPower":       burst,
			"Featured":         featured,
			"Tags":             tags,
			"Locale":           locale,
			"Country":          country,
			"OneSync":          onesync,
			"GameBuild":        gameBuild,
			"PureLevel":        pureLevel,
			"ScriptHook":       scriptHook,
			"BannerDetail":     bannerDetail,
			"BannerConnecting": bannerConnecting,
			"IconVersion":      d.IconVersion,
			"Resources":        len(d.Resources),
			"ResourceList":       d.Resources,
			"PlayersList":        d.Players,
			"ServerVersion":      d.Server,
			// Client JS fields (matching NSM.exe cfx-ui IServerView)
			"id":               ep,
			"joinId":           joinID,
			"rawName":          d.Hostname,
			"hostname":         d.Hostname,
			"name":             clean,
			"desc":             desc,
			"projectDescription": desc,
			"players":          d.Clients,
			"maxPlayers":       d.SVMaxclients,
			"mode":             d.Gametype,
			"map":              d.Mapname,
			"tags":             tags,
			"locale":           locale,
			"country":          country,
			"onesync":          onesync,
			"gameBuild":        gameBuild,
			"pureLevel":        pureLevel,
			"scriptHook":       scriptHook,
			"bannerDetail":     bannerDetail,
			"bannerConnecting": bannerConnecting,
			"iconVersion":      d.IconVersion,
			"resourcesCount":   len(d.Resources),
			"resources":        d.Resources,
			"playersList":      d.Players,
			"serverVersion":    d.Server,
			"upvotes":          upv,
			"upvotePower":      upv,
			"burstPower":       burst,
			"featured":         featured,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		fi, _ := out[i]["Featured"].(bool)
		fj, _ := out[j]["Featured"].(bool)
		if fi != fj {
			return fi
		}
		ui, uj := out[i]["Upvotes"].(int), out[j]["Upvotes"].(int)
		if ui != uj {
			return ui > uj
		}
		ci, cj := out[i]["Clients"].(int64), out[j]["Clients"].(int64)
		if ci != cj {
			return ci > cj
		}
		return out[i]["CleanName"].(string) < out[j]["CleanName"].(string)
	})
	return out
}

func totalPlayers() int64 {
	var n int64
	for _, e := range reg.live() {
		d := e.data
		applyMods("", &d)
		n += d.Clients
	}
	return n
}

// ---- pages ----

func homePage(w http.ResponseWriter, r *http.Request) {
	stateMu.RLock()
	feedItems := make([]feedItem, len(state.Feed))
	copy(feedItems, state.Feed)
	stateMu.RUnlock()
	sort.Slice(feedItems, func(i, j int) bool { return feedItems[i].ID > feedItems[j].ID })
	if len(feedItems) > 5 {
		feedItems = feedItems[:5]
	}
	allServers := liveServers()
	tagSet := map[string]bool{}
	var allTags []string
	for _, s := range allServers {
		if tags, ok := s["Tags"].([]string); ok {
			for _, t := range tags {
				if !tagSet[t] {
					tagSet[t] = true
					allTags = append(allTags, t)
				}
			}
		}
	}
	sort.Strings(allTags)
	renderPage(w, r, "index.tmpl", map[string]interface{}{
		"Title":       "NSM — the GTA V multiplayer modification you have dreamt of",
		"Feed":        feedItems,
		"Servers":     allServers,
		"Tags":        allTags,
		"ForumTopics": recentForumTopics(4),
		"Stats": map[string]int64{
			"Players":  totalPlayers(),
			"Servers":  int64(len(allServers)),
			"Accounts": accountCount(),
			"Posts":    totalForumPosts(),
		},
	})
}

func serversPage(w http.ResponseWriter, r *http.Request) {
	srvs := liveServers()
	tagSet := map[string]bool{}
	var allTags []string
	var featuredSrvs []map[string]interface{}
	for _, s := range srvs {
		if f, _ := s["Featured"].(bool); f {
			featuredSrvs = append(featuredSrvs, s)
		}
		if tags, ok := s["Tags"].([]string); ok {
			for _, t := range tags {
				if !tagSet[t] {
					tagSet[t] = true
					allTags = append(allTags, t)
				}
			}
		}
	}
	sort.Strings(allTags)
	renderPage(w, r, "servers.tmpl", map[string]interface{}{
		"Title":           "Server List - NSM",
		"Servers":         srvs,
		"FeaturedServers": featuredSrvs,
		"Tags":            allTags,
	})
}

func serverDetailPage(w http.ResponseWriter, r *http.Request) {
	ep := r.PathValue("ep")
	if unesc, err := url.PathUnescape(ep); err == nil && unesc != "" {
		ep = strings.TrimSpace(unesc)
	}
	renderServerDetail(w, r, ep, joinCodeForEndpointLookup(ep))
}

// renderServerDetail renders the server landing page. joinCode (when set) is
// the opaque share code shown instead of the raw endpoint so public pages
// never display server IPs.
func renderServerDetail(w http.ResponseWriter, r *http.Request, ep, joinCode string) {
	d := reg.get(ep)
	if d == nil {
		missDisplay := ep
		if joinCode != "" {
			missDisplay = joinCode
		}
		renderPage(w, r, "server_missing.tmpl", map[string]interface{}{"Title": "Server Offline - NSM", "EndPoint": missDisplay, "JoinCode": joinCode, "JoinID": missDisplay})
		return
	}
	applyMods(ep, d)
	u := currentUser(r)
	voted := false
	if u != nil {
		accMu.Lock()
		for _, id := range acc.Upvotes[ep] {
			if id == u.ID {
				voted = true
			}
		}
		accMu.Unlock()
	}
	var ownerUsername string
	reg.mu.RLock()
	if ent, ok := reg.servers[ep]; ok && ent.ownerID != 0 {
		accMu.Lock()
		if ou := userByID(ent.ownerID); ou != nil {
			ownerUsername = ou.Username
		}
		accMu.Unlock()
	}
	reg.mu.RUnlock()

	joinID := joinCode
	if joinID == "" {
		joinID = ep
	}
	renderPage(w, r, "server.tmpl", map[string]interface{}{
		"Title":         cleanHostname(d.Hostname) + " - Server List - NSM",
		"CleanName":     cleanHostname(d.Hostname),
		"EndPoint":      ep,
		"JoinCode":      joinCode,
		"JoinID":        joinID,
		"Srv":           d,
		"Upvotes":       int(d.UpvotePower),
		"Voted":         voted,
		"Featured":      isFeaturedServer(ep),
		"OwnerUsername": ownerUsername,
	})
}

func loginPage(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	renderPage(w, r, "login.tmpl", map[string]interface{}{
		"Title":          "Login - AFX Portal",
		"Next":           next,
		"ShowOwnerSetup": ownerNeedsPasswordSetup(),
	})
}

func registerPage(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	renderPage(w, r, "register.tmpl", map[string]interface{}{"Title": "Create Account - AFX Portal", "Next": next})
}

func logoutPage(w http.ResponseWriter, r *http.Request) {
	apiLogout(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func accountPage(w http.ResponseWriter, r *http.Request) {
	renderAccountPageWithMsg(w, r, currentUser(r), "", "")
}

func supportPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	isStaff := u != nil && u.IsStaffOrHigher()
	var myTickets []map[string]interface{}
	var allTickets []map[string]interface{}
	var closedLog []map[string]interface{}
	accMu.Lock()
	if acc != nil && u != nil {
		for i := len(acc.SupportTickets) - 1; i >= 0; i-- {
			tk := acc.SupportTickets[i]
			isMine := tk.RequesterID == u.ID || strings.EqualFold(tk.RequesterUsername, u.Username)
			if tk.Status == "Closed" {
				// Closed tickets vanish from the working lists and are kept as
				// a read-only log entry of what the request was about.
				if isMine || isStaff {
					closedLog = append(closedLog, buildSupportTicketView(&tk))
				}
				continue
			}
			if isMine {
				myTickets = append(myTickets, buildSupportTicketView(&tk))
			}
			if isStaff {
				allTickets = append(allTickets, buildSupportTicketView(&tk))
			}
		}
	}
	accMu.Unlock()
	renderPage(w, r, "support.tmpl", map[string]interface{}{
		"Title":      "AFX Support",
		"Page":       "/support",
		"Invite":     "https://discord.gg/UXj5QfCX88",
		"Tickets":    myTickets,
		"MyTickets":  myTickets,
		"AllTickets": allTickets,
		"ClosedLog":  closedLog,
		"IsStaff":    isStaff,
		"CanSubmit":  u != nil,
		"Error":      r.URL.Query().Get("error"),
		"Msg":        r.URL.Query().Get("msg"),
	})
}

func marketplacePage(w http.ResponseWriter, r *http.Request) {
	renderPage(w, r, "marketplace.tmpl", map[string]interface{}{
		"Title":  "AFX Marketplace",
		"Topics": recentForumTopics(8),
	})
}

func keymasterPage(w http.ResponseWriter, r *http.Request) {
	serveKeymasterPage(w, r, nil)
}

func docsPage(w http.ResponseWriter, r *http.Request) {
	renderPage(w, r, "docs.tmpl", map[string]interface{}{"Title": "Home - NSM Documentation"})
}

func serveClientDownload(w http.ResponseWriter, r *http.Request) {
	for _, candidate := range []string{"NSM.exe", "downloads/NSM.exe", "site/static/NSM.exe"} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="NSM.exe"`)
			http.ServeFile(w, r, candidate)
			return
		}
	}
	http.Redirect(w, r, "https://github.com/platinum-cfx/NSM-FIVEM/releases/download/nsm-v0.8.0-candidate/NSM.exe", http.StatusFound)
}

func serveServerDownload(w http.ResponseWriter, r *http.Request) {
	for _, candidate := range []string{"NSM-Server.zip", "downloads/NSM-Server.zip", "site/static/NSM-Server.zip"} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", `attachment; filename="NSM-Server.zip"`)
			http.ServeFile(w, r, candidate)
			return
		}
	}
	http.Redirect(w, r, "https://github.com/platinum-cfx/NSM-FIVEM/releases/download/nsm-v0.8.0-candidate/NSM-Server.zip", http.StatusFound)
}

func serveLocalPolicy(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, http.StatusOK, []string{
		"subdir_file_mapping",
		"onesync",
		"onesync_plus",
		"onesync_medium",
		"onesync_big",
		"onesync_platinum",
		"local_evaluation",
	})
}

func apiServersLive(w http.ResponseWriter, r *http.Request) {
	srvs := liveServers()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"servers":         srvs,
		"featuredServers": featuredServerEndpoints(),
		"players":         totalPlayers(),
		"count":           len(srvs),
	})
}

// ---- router assembly (Go 1.22 patterns) ----

func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerUIRoutes(mux)

	// static
	static, _ := fs.Sub(siteFS, "site/static")
	mux.Handle("/css/", http.StripPrefix("/css/", http.FileServer(http.FS(static))))
	mux.Handle("/js/", http.StripPrefix("/js/", http.FileServer(http.FS(static))))
	mux.Handle("/img/", http.StripPrefix("/img/", http.FileServer(http.FS(static))))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	// pages
	mux.HandleFunc("GET /{$}", homePage)
	mux.HandleFunc("GET /servers", serversPage)
	mux.HandleFunc("GET /servers/{ep}", serverDetailPage)
	mux.HandleFunc("GET /join/{ep}", func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSpace(r.PathValue("ep"))
		if unesc, err := url.PathUnescape(raw); err == nil && unesc != "" {
			raw = strings.TrimSpace(unesc)
		}
		// Opaque join codes (like cfx.re/join/zjjkkky) hide the server IP.
		code := strings.ToLower(raw)
		if isJoinCode(code) {
			ep, ok := resolveJoinCode(code)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				renderPage(w, r, "server_missing.tmpl", map[string]interface{}{"Title": "Server Not Found - NSM", "EndPoint": code})
				return
			}
			w.Header().Set("X-CitizenFX-Url", "http://"+ep+"/")
			w.Header().Set("Location", "https://nsmultiplayer.com/join/"+code)
			renderServerDetail(w, r, ep, code)
			return
		}
		// Legacy /join/<ip:port> form — kept for compatibility.
		ep := raw
		if ep != "" {
			w.Header().Set("X-CitizenFX-Url", "http://"+ep+"/")
			w.Header().Set("Location", "https://nsmultiplayer.com/join/"+ep)
		}
		renderServerDetail(w, r, ep, joinCodeForEndpointLookup(ep))
	})
	mux.HandleFunc("POST /servers/{ep}/upvote", func(w http.ResponseWriter, r *http.Request) { upvoteServer(w, r, r.PathValue("ep")) })
	mux.HandleFunc("POST /servers/{ep}/feature", func(w http.ResponseWriter, r *http.Request) { featureServer(w, r, r.PathValue("ep")) })
	mux.HandleFunc("POST /api/servers/upvote", func(w http.ResponseWriter, r *http.Request) { upvoteServer(w, r, "") })
	mux.HandleFunc("POST /api/servers/feature", func(w http.ResponseWriter, r *http.Request) { featureServer(w, r, "") })
	mux.HandleFunc("GET /login", loginPage)
	mux.HandleFunc("POST /login", apiLogin)
	mux.HandleFunc("GET /register", registerPage)
	mux.HandleFunc("POST /register", apiRegister)
	mux.HandleFunc("GET /signup", registerPage)
	mux.HandleFunc("POST /signup", apiRegister)
	mux.HandleFunc("/f/signup", registerPage)
	mux.HandleFunc("GET /logout", logoutPage)
	mux.HandleFunc("GET /account", accountPage)
	mux.HandleFunc("GET /verify-email", verifyEmailPage)
	mux.HandleFunc("POST /account/verify-email", accountVerifyEmail)
	mux.HandleFunc("POST /account/resend-verify", accountResendVerifyEmail)
	mux.HandleFunc("POST /account/2fa", accountConfigure2FA)
	mux.HandleFunc("POST /account/friends", accountFriendAction)
	mux.HandleFunc("POST /account/password", accountChangePassword)
	mux.HandleFunc("POST /account/profile", accountUpdateProfile)
	mux.HandleFunc("POST /account/pulse", accountSubscribePulse)
	mux.HandleFunc("POST /account/messages", sendDirectMessage)
	mux.HandleFunc("POST /account/connect-client", accountConnectClient)
	mux.HandleFunc("POST /account/claim-owner", accountClaimOwner)
	mux.HandleFunc("GET /u/check_email.json", serveDiscourseCheckEmail)
	mux.HandleFunc("GET /u/check_username.json", serveDiscourseCheckUsername)
	mux.HandleFunc("GET /u", usersDirectoryPage)
	mux.HandleFunc("GET /users", usersDirectoryPage)
	mux.HandleFunc("POST /u", serveDiscourseCreateUser)
	mux.HandleFunc("POST /u/action/send_activation_email", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, http.StatusOK, map[string]string{"success": "OK"})
	})
	mux.HandleFunc("GET /u/{username}", userProfilePage)
	mux.HandleFunc("GET /u/{username}/{tab}", userProfilePage)
	mux.HandleFunc("GET /users/{username}", userProfilePage)
	mux.HandleFunc("GET /users/{username}/{tab}", userProfilePage)
	mux.HandleFunc("GET /avatars/u/{username}/{size}", serveUserAvatar)
	mux.HandleFunc("/f/avatars/u/{username}/{size}", serveUserAvatar)
	mux.HandleFunc("GET /banners/u/{username}", serveUserBanner)

	mux.HandleFunc("GET /docs", docsPage)
	mux.HandleFunc("GET /support", supportPage)
	mux.HandleFunc("GET /support/requests", supportPage)
	mux.HandleFunc("GET /hc/en-us", supportPage)
	mux.HandleFunc("GET /hc/en-us/requests", supportPage)
	mux.HandleFunc("POST /support/requests", supportCreateRequest)
	mux.HandleFunc("GET /support/requests/{id}", supportViewRequest)
	mux.HandleFunc("GET /hc/en-us/requests/{id}", supportViewRequest)
	mux.HandleFunc("POST /support/requests/{id}/reply", supportReplyRequest)
	mux.HandleFunc("POST /support/requests/{id}/status", supportStatusRequest)
	mux.HandleFunc("GET /marketplace", marketplacePage)
	mux.HandleFunc("GET /portal", keymasterPage)
	mux.HandleFunc("GET /download", serveClientDownload)
	mux.HandleFunc("GET /NSM.exe", serveClientDownload)
	mux.HandleFunc("GET /download-server", serveServerDownload)
	mux.HandleFunc("GET /NSM-Server.zip", serveServerDownload)

	// Forum (full Discourse-style forum at /forum + /f/ compatibility)
	mux.HandleFunc("GET /forum", forumPage)
	mux.HandleFunc("GET /forum/c/{cat}", forumPage)
	mux.HandleFunc("POST /forum/new", forumCreateTopic)
	mux.HandleFunc("GET /forum/t/{id}", forumTopicPage)
	mux.HandleFunc("POST /forum/t/{id}/reply", forumReplyTopic)
	mux.HandleFunc("POST /forum/t/{id}/like/{postId}", forumLikePost)
	mux.HandleFunc("POST /forum/t/{id}/mod/{action}", forumModerateTopic)
	mux.HandleFunc("GET /api/forum/realtime", serveForumRealtimeJSON)

	// Keymaster
	mux.HandleFunc("GET /keymaster", keymasterPage)
	mux.HandleFunc("POST /keymaster/create", keymasterCreateForm)
	mux.HandleFunc("POST /keymaster/upvotes", keymasterUpvotesForm)
	mux.HandleFunc("POST /keymaster/{id}/revoke", keymasterRevokeForm)
	mux.HandleFunc("GET /auth/google", googleAuthStart)
	mux.HandleFunc("GET /auth/google/callback", googleAuthCallback)

	// In-App Client Login & Discourse API Bridge
	mux.HandleFunc("/user-api-key/new", serveUserAPIKeyNew)
	mux.HandleFunc("/f/user-api-key/new", serveUserAPIKeyNew)
	mux.HandleFunc("/user-api-key", serveUserAPIKeyCreate)
	mux.HandleFunc("/f/user-api-key", serveUserAPIKeyCreate)
	mux.HandleFunc("/api/auth/complete", serveClientAuthComplete)
	mux.HandleFunc("/api/auth/client-poll", apiClientAuthPoll)
	mux.HandleFunc("/session/csrf.json", serveDiscourseCSRF)
	mux.HandleFunc("/f/session/csrf.json", serveDiscourseCSRF)
	mux.HandleFunc("/session/hp.json", serveDiscourseHoneypot)
	mux.HandleFunc("/f/session/hp.json", serveDiscourseHoneypot)
	mux.HandleFunc("/session/current.json", serveDiscourseCurrentSession)
	mux.HandleFunc("/f/session/current.json", serveDiscourseCurrentSession)
	mux.HandleFunc("/session", serveDiscourseCreateSession)
	mux.HandleFunc("/f/session", serveDiscourseCreateSession)
	mux.HandleFunc("/session/{username}", serveDiscourseCreateSession)
	mux.HandleFunc("/f/session/{username}", serveDiscourseCreateSession)
	mux.HandleFunc("/f/u/check_email.json", serveDiscourseCheckEmail)
	mux.HandleFunc("/f/u/check_username.json", serveDiscourseCheckUsername)
	mux.HandleFunc("/f/u/action/send_activation_email", func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, http.StatusOK, map[string]string{"success": "OK"})
	})
	mux.HandleFunc("/f/u", serveDiscourseCreateUser)
	mux.HandleFunc("/f/u/{username}", func(w http.ResponseWriter, r *http.Request) {
		serveDiscourseUserJSON(w, r, r.PathValue("username"))
	})
	mux.HandleFunc("/site", serveDiscourseSiteJSON)
	mux.HandleFunc("/site.json", serveDiscourseSiteJSON)
	mux.HandleFunc("/f/site", serveDiscourseSiteJSON)
	mux.HandleFunc("/f/site.json", serveDiscourseSiteJSON)
	mux.HandleFunc("/posts.json", serveDiscoursePostsJSON)
	mux.HandleFunc("/f/posts.json", serveDiscoursePostsJSON)
	mux.HandleFunc("/f/t/{id}", forumTopicPage)
	mux.HandleFunc("/l/api/validate/discourse", serveValidateDiscourse)

	// staff admin
	mux.HandleFunc("GET /admin/users", adminUsersPage)
	mux.HandleFunc("POST /admin/users/{id}/role", adminSetRole)
	mux.HandleFunc("POST /admin/users/{id}/partner", adminTogglePartner)
	mux.HandleFunc("POST /admin/users/{id}/ban", adminBanUser)

	// JSON API (client + site + keymaster)
	mux.HandleFunc("GET /api/auth/me", apiMe)
	mux.HandleFunc("POST /api/auth/login", apiLogin)
	mux.HandleFunc("POST /api/auth/register", apiRegister)
	mux.HandleFunc("POST /api/auth/logout", apiLogout)
	mux.HandleFunc("GET /api/servers/live", apiServersLive)
	mux.HandleFunc("GET /api/keymaster/keys", apiKeymasterList)
	mux.HandleFunc("POST /api/keymaster/issue", apiKeymasterIssue)
	mux.HandleFunc("POST /api/keymaster/revoke", apiKeymasterRevoke)
	mux.HandleFunc("POST /api/nsm/servers/register", apiServerRegister)
	mux.HandleFunc("POST /api/nsm/internal-restart", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
		go func() {
			time.Sleep(40 * time.Millisecond)
			log.Printf("[restart] New NSM-Masterlist instance requested port takeover — exiting old instance.")
			closeActiveServer()
			os.Exit(0)
		}()
	})

	// updater: no updates in local mode
	mux.HandleFunc("/updates/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("UPDATE %s %s -> 404 (keeping current build)", r.Method, r.URL.Path)
		http.Error(w, "no updates (NSM local stack - keeping current build)", http.StatusNotFound)
	})

	// independent NSM server feature policy & root mirror/patch cache routes
	mux.HandleFunc("/p/api/policy/", serveLocalPolicy)
	mux.HandleFunc("/api/policy/", serveLocalPolicy)
	mux.HandleFunc("/mirrors/", func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/c" + r.URL.Path
		serveProxy(w, r2, "c", proxyRoutes["c"])
	})
	mux.HandleFunc("/patches/", func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/r" + r.URL.Path
		serveProxy(w, r2, "r", proxyRoutes["r"])
	})

	// game-cache proxy routes (/f/ and /e/ are handled locally by our Discourse & Forum service)
	for route, upstream := range proxyRoutes {
		rt, up := route, upstream
		if rt == "f" || rt == "e" {
			mux.HandleFunc("/"+rt+"/", serveDiscourseRouter)
			continue
		}
		if rt == "p" {
			mux.HandleFunc("/p/", serveLocalPolicy)
			continue
		}
		mux.HandleFunc("/"+rt+"/", func(w http.ResponseWriter, r *http.Request) {
			serveProxy(w, r, rt, up)
		})
	}

	// everything else -> masterlist v2 handler (api/servers, ingress, ticket, /v1/*, counts, /admin/* bearer)
	mux.Handle("/", &handler{})

	return mux
}
