// NSM Keymaster: authorized NSM-owned server key issuance, revocation,
// and heartbeat validation for NSM's server list.
//
// Security properties:
// - Keys are shaped: nsm_srv_<16-hex-id>.<48-hex-secret>
// - Only a per-key 128-bit random salt and an iterated HMAC-SHA256 digest
//   (100,000 rounds) are stored in accounts.json; the plaintext secret is
//   returned ONCE at creation and never persisted or logged.
// - Validation is fail-closed: missing (when requireKeys is set), malformed,
//   unknown, wrong-secret, expired, or revoked keys return 401/403 and NEVER
//   list a server.
// - Strict account ownership (anti-IDOR): users can only view/revoke their own
//   keys (unless admin/owner), and one account's valid key cannot hijack an
//   active server endpoint registered by another account.
// - Never touches or bypasses Rockstar/GTA entitlement validation or Cfx escrow.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	nsmKeyPrefix       = "nsm_srv_"
	nsmKeyIDHexLen     = 16
	nsmKeyHashRounds   = 100_000
	defaultKeyValidity = 90 * 24 * time.Hour
)

var (
	keyClock      = time.Now
	requireKeys   bool
	keyRedactExpr = regexp.MustCompile(`nsm_srv_[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)
)

type ServerKey struct {
	ID            string `json:"id"`
	UserID        int64  `json:"userId"`
	Username      string `json:"username"`
	Label         string `json:"label"`
	BoundEndpoint string `json:"boundEndpoint,omitempty"`
	JoinCode      string `json:"joinCode,omitempty"`
	SaltHex       string `json:"saltHex"`
	SecretHashHex string `json:"secretHashHex"`
	CreatedAt     int64  `json:"createdAt"`
	ExpiresAt     int64  `json:"expiresAt"`
	Revoked       bool   `json:"revoked"`
	RevokedAt     int64  `json:"revokedAt,omitempty"`
	LastUsedAt    int64  `json:"lastUsedAt,omitempty"`
	LastEndpoint  string `json:"lastEndpoint,omitempty"`
}

func (k ServerKey) PublicView() map[string]interface{} {
	now := keyClock().Unix()
	status := "active"
	if k.Revoked {
		status = "revoked"
	} else if now >= k.ExpiresAt {
		status = "expired"
	}
	return map[string]interface{}{
		"id":            k.ID,
		"userId":        k.UserID,
		"username":      k.Username,
		"label":         k.Label,
		"boundEndpoint": k.BoundEndpoint,
		"joinCode":      k.JoinCode,
		"status":        status,
		"createdAt":     k.CreatedAt,
		"expiresAt":     k.ExpiresAt,
		"revoked":       k.Revoked,
		"revokedAt":     k.RevokedAt,
		"lastUsedAt":    k.LastUsedAt,
		"lastEndpoint":  k.LastEndpoint,
	}
}

func redactServerKeys(s string) string {
	if s == "" {
		return ""
	}
	return keyRedactExpr.ReplaceAllString(s, "nsm_srv_[REDACTED]")
}

func deriveKeyDigest(secret string, salt []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(secret))
	sum := mac.Sum(nil)
	for i := 1; i < nsmKeyHashRounds; i++ {
		h := sha256.Sum256(append(salt, sum...))
		sum = h[:]
	}
	return sum
}

func parseNSMServerKey(raw string) (keyID string, secret string, ok bool) {
	token := strings.TrimSpace(raw)
	if !strings.HasPrefix(token, nsmKeyPrefix) {
		return "", "", false
	}
	dot := strings.IndexByte(token, '.')
	if dot <= len(nsmKeyPrefix) {
		return "", "", false
	}
	keyID = token[:dot]
	idHex := token[len(nsmKeyPrefix):dot]
	secret = token[dot+1:]
	if len(idHex) != nsmKeyIDHexLen {
		return "", "", false
	}
	for i := 0; i < len(idHex); i++ {
		c := idHex[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", "", false
		}
	}
	if len(secret) < 24 || len(secret) > 128 {
		return "", "", false
	}
	return keyID, secret, true
}

func ensureAccountsInitializedLocked() {
	if acc == nil {
		acc = &Accounts{Sessions: map[string]Session{}, Upvotes: map[string][]int64{}, NextID: 1}
	}
}

func issueServerKey(u *User, label, boundEndpoint string, ttl time.Duration) (string, ServerKey, error) {
	if u == nil {
		return "", ServerKey{}, errors.New("authentication required")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "NSM Server"
	}
	if len(label) > 100 {
		return "", ServerKey{}, errors.New("label must be 100 characters or fewer")
	}
	boundEndpoint = strings.TrimSpace(boundEndpoint)
	if ttl <= 0 {
		ttl = defaultKeyValidity
	}

	idBytes := make([]byte, nsmKeyIDHexLen/2)
	secBytes := make([]byte, 24)
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", ServerKey{}, err
	}
	if _, err := rand.Read(secBytes); err != nil {
		return "", ServerKey{}, err
	}
	if _, err := rand.Read(saltBytes); err != nil {
		return "", ServerKey{}, err
	}

	keyID := nsmKeyPrefix + hex.EncodeToString(idBytes)
	secret := hex.EncodeToString(secBytes)
	digest := deriveKeyDigest(secret, saltBytes)
	now := keyClock()

	rec := ServerKey{
		ID:            keyID,
		UserID:        u.ID,
		Username:      u.Username,
		Label:         label,
		BoundEndpoint: boundEndpoint,
		SaltHex:       hex.EncodeToString(saltBytes),
		SecretHashHex: hex.EncodeToString(digest),
		CreatedAt:     now.Unix(),
		ExpiresAt:     now.Add(ttl).Unix(),
		Revoked:       false,
	}

	accMu.Lock()
	ensureAccountsInitializedLocked()
	rec.JoinCode = ensureJoinCodeForKeyLocked(&rec)
	acc.ServerKeys = append(acc.ServerKeys, rec)
	saveAccountsLocked()
	accMu.Unlock()

	fullKey := keyID + "." + secret
	log.Printf("[keymaster] issued key %s for user %s (id %d)", keyID, u.Username, u.ID)
	return fullKey, rec, nil
}

// validateServerKey validates a raw NSM key fail-closed.
func validateServerKey(rawKey, endpoint string) (*ServerKey, int, string) {
	if strings.TrimSpace(rawKey) == "" {
		return nil, http.StatusUnauthorized, "missing_key"
	}
	keyID, secret, ok := parseNSMServerKey(rawKey)
	if !ok {
		return nil, http.StatusUnauthorized, "malformed_key"
	}

	now := keyClock().Unix()
	accMu.Lock()
	defer accMu.Unlock()
	ensureAccountsInitializedLocked()

	for i := range acc.ServerKeys {
		rec := &acc.ServerKeys[i]
		if rec.ID != keyID {
			continue
		}
		salt, err1 := hex.DecodeString(rec.SaltHex)
		wantHash, err2 := hex.DecodeString(rec.SecretHashHex)
		if err1 != nil || err2 != nil {
			return nil, http.StatusForbidden, "invalid_key"
		}
		gotHash := deriveKeyDigest(secret, salt)
		if subtle.ConstantTimeCompare(wantHash, gotHash) != 1 {
			return nil, http.StatusForbidden, "invalid_key"
		}
		if rec.Revoked {
			cp := *rec
			return &cp, http.StatusForbidden, "revoked_key"
		}
		if now >= rec.ExpiresAt {
			cp := *rec
			return &cp, http.StatusForbidden, "expired_key"
		}
		if rec.BoundEndpoint != "" && endpoint != "" && rec.BoundEndpoint != endpoint {
			cp := *rec
			return &cp, http.StatusForbidden, "endpoint_mismatch"
		}
		rec.LastUsedAt = now
		if endpoint != "" {
			rec.LastEndpoint = endpoint
		}
		saveAccountsLocked()
		cp := *rec
		return &cp, http.StatusOK, ""
	}
	return nil, http.StatusForbidden, "invalid_key"
}

func isServerKeyActive(keyID string) bool {
	if keyID == "" {
		return true
	}
	now := keyClock().Unix()
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil {
		return false
	}
	for i := range acc.ServerKeys {
		rec := &acc.ServerKeys[i]
		if rec.ID == keyID {
			return !rec.Revoked && now < rec.ExpiresAt
		}
	}
	return false
}

func revokeServerKey(actor *User, keyID string) (int, int, string) {
	if actor == nil {
		return 0, http.StatusUnauthorized, "authentication_required"
	}
	keyID = strings.TrimSpace(keyID)
	now := keyClock().Unix()

	accMu.Lock()
	ensureAccountsInitializedLocked()
	var found *ServerKey
	for i := range acc.ServerKeys {
		if acc.ServerKeys[i].ID == keyID {
			found = &acc.ServerKeys[i]
			break
		}
	}
	if found == nil {
		accMu.Unlock()
		return 0, http.StatusNotFound, "key_not_found"
	}
	if found.UserID != actor.ID && !roleAtLeast(actor.Role, "admin") {
		accMu.Unlock()
		return 0, http.StatusForbidden, "forbidden_not_key_owner"
	}
	found.Revoked = true
	found.RevokedAt = now
	revokeJoinCodesForKeyLocked(keyID)
	saveAccountsLocked()
	accMu.Unlock()

	evicted := reg.evictByKey(keyID)
	log.Printf("[keymaster] revoked key %s by %s (evicted %d listings)", keyID, actor.Username, evicted)
	return evicted, http.StatusOK, ""
}

func listServerKeysForUser(actor *User) []map[string]interface{} {
	if actor == nil {
		return nil
	}
	accMu.Lock()
	defer accMu.Unlock()
	if acc == nil {
		return nil
	}
	out := []map[string]interface{}{}
	for i := range acc.ServerKeys {
		k := &acc.ServerKeys[i]
		// PRIVACY: a key is visible ONLY to the account that issued it —
		// never to other members (and not even to staff browsing Keymaster).
		if k.UserID == actor.ID {
			ensureJoinCodeForKeyLocked(k)
			out = append(out, k.PublicView())
		}
	}
	if len(out) > 0 {
		saveAccountsLocked()
	}
	return out
}

func extractHeartbeatServerKey(r *http.Request, hb *heartbeat) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		if cand := strings.TrimSpace(auth[7:]); cand != "" {
			return cand
		}
	}
	if hdr := strings.TrimSpace(r.Header.Get("X-NSM-Server-Key")); hdr != "" {
		return hdr
	}
	if hb != nil {
		if tok := strings.TrimSpace(hb.ListingToken); tok != "" {
			return tok
		}
		for _, section := range []map[string]interface{}{
			unwrapData(hb.FallbackData.Info, "info"),
			unwrapData(hb.FallbackData.Dynamic, "dynamic"),
		} {
			if vars, ok := section["vars"].(map[string]interface{}); ok {
				for _, varName := range []string{"sv_nsmServerKey", "sv_licenseKey", "sv_licenseKeyToken"} {
					if v, exists := vars[varName]; exists {
						if s := strings.TrimSpace(asString(v)); s != "" {
							return s
						}
					}
				}
			}
		}
	}
	return ""
}

// ---- Website & JSON API Handlers ----

func serveKeymasterPage(w http.ResponseWriter, r *http.Request, extra map[string]interface{}) {
	u := currentUser(r)
	title := "AFX Portal — NSM Keymaster"
	if u == nil {
		title = "Login - AFX Portal"
	}
	data := map[string]interface{}{
		"Title":       title,
		"Page":        "/keymaster",
		"User":        u,
		"RequireKeys": requireKeys,
	}
	if u != nil {
		data["Keys"] = listServerKeysForUser(u)
		data["UpvoteBalance"] = u.UpvoteAllowance()
		upEp := ""
		accMu.Lock()
		if t := userByID(u.ID); t != nil {
			data["UpvoteAllocated"] = t.UpvoteAllocation
		}
		for _, k := range acc.ServerKeys {
			if k.UserID == u.ID && !k.Revoked {
				if k.BoundEndpoint != "" {
					upEp = k.BoundEndpoint
					break
				}
				if k.LastEndpoint != "" {
					upEp = k.LastEndpoint
				}
			}
		}
		accMu.Unlock()
		data["UpvoteServerEp"] = upEp
	}
	for k, v := range extra {
		data[k] = v
	}
	renderPage(w, r, "keymaster.tmpl", data)
}

func keymasterCreateForm(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	label := r.FormValue("label")
	bound := r.FormValue("boundEndpoint")
	days, _ := strconv.Atoi(r.FormValue("days"))
	if days <= 0 || days > 365 {
		days = 90
	}
	fullKey, rec, err := issueServerKey(u, label, bound, time.Duration(days)*24*time.Hour)
	if err != nil {
		serveKeymasterPage(w, r, map[string]interface{}{"Error": err.Error()})
		return
	}
	serveKeymasterPage(w, r, map[string]interface{}{
		"NewKey":   fullKey,
		"NewKeyID": rec.ID,
		"Msg":      "Server key created. Copy the secret below now — it is shown only once.",
	})
}

func keymasterRevokeForm(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	keyID := r.PathValue("id")
	if _, code, reason := revokeServerKey(u, keyID); code != http.StatusOK {
		serveKeymasterPage(w, r, map[string]interface{}{"Error": reason})
		return
	}
	http.Redirect(w, r, "/keymaster", http.StatusSeeOther)
}

func keymasterUpvotesForm(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?next=/keymaster", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("upvotes")))
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	accMu.Lock()
	if t := userByID(u.ID); t != nil {
		t.UpvoteAllocation = n
	}
	saveAccountsLocked()
	accMu.Unlock()
	serveKeymasterPage(w, r, map[string]interface{}{
		"Msg": fmt.Sprintf("Saved — %d of your free upvotes now boost your server's listing.", n),
	})
}

func apiKeymasterList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]interface{}{"ok": false, "error": "authentication_required"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "keys": listServerKeysForUser(u)})
}

func apiKeymasterIssue(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]interface{}{"ok": false, "error": "authentication_required"})
		return
	}
	var req struct {
		Label         string `json:"label"`
		BoundEndpoint string `json:"boundEndpoint"`
		TTLSeconds    int64  `json:"ttlSeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "invalid_json"})
		return
	}
	ttl := defaultKeyValidity
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	fullKey, rec, err := issueServerKey(u, req.Label, req.BoundEndpoint, ttl)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, http.StatusCreated, map[string]interface{}{
		"ok":        true,
		"serverKey": fullKey,
		"key":       rec.PublicView(),
	})
}

func apiKeymasterRevoke(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		sendJSON(w, http.StatusUnauthorized, map[string]interface{}{"ok": false, "error": "authentication_required"})
		return
	}
	var req struct {
		KeyID string `json:"keyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.KeyID) == "" {
		sendJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "keyId required"})
		return
	}
	evicted, code, reason := revokeServerKey(u, req.KeyID)
	if code != http.StatusOK {
		sendJSON(w, code, map[string]interface{}{"ok": false, "error": reason})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"keyId":           req.KeyID,
		"evictedListings": evicted,
	})
}

func apiServerRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key        string `json:"key"`
		Port       int64  `json:"port"`
		IPOverride string `json:"ipOverride"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	rawKey := extractHeartbeatServerKey(r, nil)
	if rawKey == "" {
		rawKey = strings.TrimSpace(req.Key)
	}
	rec, code, reason := validateServerKey(rawKey, "")
	if code != http.StatusOK || rec == nil {
		sendJSON(w, code, map[string]interface{}{"ok": false, "lastError": reason})
		return
	}
	sendJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"keyId":   rec.ID,
		"userId":  rec.UserID,
		"owner":   rec.Username,
		"message": "NSM server key verified",
	})
}
