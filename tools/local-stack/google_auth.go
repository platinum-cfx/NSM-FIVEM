package main

// Google "Sign in with Google" for NSM. Credentials live ONLY in the server
// environment — never in files:
//
//	NSM_GOOGLE_CLIENT_ID      OAuth client id (unset = feature hidden & disabled)
//	NSM_GOOGLE_CLIENT_SECRET  OAuth client secret
//	NSM_PUBLIC_URL            public base URL for the OAuth callback
//	                            (default https://nsmultiplayer.com)
//	NSM_GOOGLE_TOKEN_URL / NSM_GOOGLE_USERINFO_URL
//	                        optional overrides (used by tests)
//
// When Google is not configured the endpoints answer with a plain 503 and the
// buttons are hidden — success is never faked.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func googleEnabled() bool {
	return strings.TrimSpace(os.Getenv("NSM_GOOGLE_CLIENT_ID")) != "" &&
		strings.TrimSpace(os.Getenv("NSM_GOOGLE_CLIENT_SECRET")) != ""
}

func publicBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("NSM_PUBLIC_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://nsmultiplayer.com"
}

func googleTokenURL() string {
	if v := strings.TrimSpace(os.Getenv("NSM_GOOGLE_TOKEN_URL")); v != "" {
		return v
	}
	return "https://oauth2.googleapis.com/token"
}

func googleUserinfoURL() string {
	if v := strings.TrimSpace(os.Getenv("NSM_GOOGLE_USERINFO_URL")); v != "" {
		return v
	}
	return "https://openidconnect.googleapis.com/v1/userinfo"
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func googleAuthStart(w http.ResponseWriter, r *http.Request) {
	if !googleEnabled() {
		http.Error(w, "Google Sign-In is not configured on this server. Set NSM_GOOGLE_CLIENT_ID and NSM_GOOGLE_CLIENT_SECRET to enable it.", http.StatusServiceUnavailable)
		return
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{Name: "nsm_g_state", Value: state, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	next := r.URL.Query().Get("next")
	q := url.Values{
		"client_id":     {os.Getenv("NSM_GOOGLE_CLIENT_ID")},
		"redirect_uri":  {publicBaseURL() + "/auth/google/callback"},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		"prompt":        {"select_account"},
	}
	if next != "" {
		q.Set("next", next)
	}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode(), http.StatusFound)
}

type googleUserinfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func googleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !googleEnabled() {
		http.Error(w, "Google Sign-In is not configured on this server.", http.StatusServiceUnavailable)
		return
	}
	_ = r.ParseForm()
	if errMsg := r.FormValue("error"); errMsg != "" {
		http.Error(w, "Google sign-in was cancelled or denied ("+errMsg+").", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	state := strings.TrimSpace(r.FormValue("state"))
	c, err := r.Cookie("nsm_g_state")
	if code == "" || state == "" || err != nil || c.Value != state {
		http.Error(w, "Google sign-in state mismatch — please try again.", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "nsm_g_state", Value: "", Path: "/", MaxAge: -1})

	// Exchange the authorization code for tokens.
	form := url.Values{
		"code":          {code},
		"client_id":     {os.Getenv("NSM_GOOGLE_CLIENT_ID")},
		"client_secret": {os.Getenv("NSM_GOOGLE_CLIENT_SECRET")},
		"redirect_uri":  {publicBaseURL() + "/auth/google/callback"},
		"grant_type":    {"authorization_code"},
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm(googleTokenURL(), form)
	if err != nil {
		log.Printf("[google-auth] token exchange failed: %v", err)
		http.Error(w, "Could not reach Google to complete sign-in. Please try again.", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(body, &tok)
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		log.Printf("[google-auth] token exchange rejected: HTTP %d %s", resp.StatusCode, tok.Error)
		http.Error(w, "Google rejected the sign-in attempt. Please try again.", http.StatusBadGateway)
		return
	}

	// Fetch the verified identity.
	req, _ := http.NewRequest("GET", googleUserinfoURL(), nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uResp, err := client.Do(req)
	if err != nil {
		log.Printf("[google-auth] userinfo failed: %v", err)
		http.Error(w, "Could not read your Google profile. Please try again.", http.StatusBadGateway)
		return
	}
	defer uResp.Body.Close()
	uBody, _ := io.ReadAll(io.LimitReader(uResp.Body, 1<<20))
	var gu googleUserinfo
	_ = json.Unmarshal(uBody, &gu)
	if uResp.StatusCode != http.StatusOK || gu.Email == "" {
		log.Printf("[google-auth] userinfo rejected: HTTP %d", uResp.StatusCode)
		http.Error(w, "Google did not share an email address for this account.", http.StatusBadGateway)
		return
	}
	if !gu.EmailVerified {
		http.Error(w, "Your Google account's email address is not verified by Google.", http.StatusForbidden)
		return
	}

	accMu.Lock()
	var user *User
	for i := range acc.Users {
		if (gu.Sub != "" && acc.Users[i].GoogleID == gu.Sub) || strings.EqualFold(acc.Users[i].Email, gu.Email) {
			user = &acc.Users[i]
			break
		}
	}
	now := time.Now().UnixMilli()
	if user == nil {
		un := uniqueUsernameFromEmail(gu.Email)
		disp := gu.Name
		if disp == "" {
			disp = un
		}
		acc.Users = append(acc.Users, User{
			ID:            acc.NextID,
			Username:      un,
			DisplayName:   disp,
			Email:         gu.Email,
			GoogleID:      gu.Sub,
			EmailVerified: true,
			Role:          "user",
			CreatedAt:     now,
			UpdatedAt:     now,
			LastLogin:     now,
		})
		acc.NextID++
		user = &acc.Users[len(acc.Users)-1]
		log.Printf("[google-auth] created account %s (id %d) via Google", user.Username, user.ID)
	} else {
		user.GoogleID = gu.Sub
		user.EmailVerified = true
		user.LastLogin = now
		user.UpdatedAt = now
	}
	tokSess := newSession(user.ID)
	userCopy := *user
	saveAccountsLocked()
	accMu.Unlock()

	http.SetCookie(w, &http.Cookie{Name: "nsm_session", Value: tokSess, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	recordWebsiteLoginForClientAuth(userCopy.ID)
	next := r.FormValue("next")
	if next == "" {
		next = r.URL.Query().Get("next")
	}
	http.Redirect(w, r, safeRedirectTarget(next), http.StatusSeeOther)
}

// uniqueUsernameFromEmail builds a free AFX username from a Google email.
func uniqueUsernameFromEmail(email string) string {
	base := strings.ToLower(strings.SplitN(email, "@", 2)[0])
	base = strings.Map(func(ch rune) rune {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			return ch
		}
		return -1
	}, base)
	if len(base) < 3 {
		base = "afx_" + base
	}
	if len(base) > 20 {
		base = base[:20]
	}
	name := base
	for i := 2; findUser(name) != nil; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
		if i > 500 {
			name = base + "_" + randomHex(3)
			break
		}
	}
	return name
}
