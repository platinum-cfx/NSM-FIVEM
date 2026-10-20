package main

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noRedirect mirrors the client: the nui:gsclient "gsclient:url" RPC issues
// the request with followLocation = false and reads the Location header.
var noRedirect = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func TestStreamRedirRedirects(t *testing.T) {
	srv := httptest.NewServer(&handler{})
	defer srv.Close()

	resp, err := noRedirect.Get(srv.URL + "/api/servers/streamRedir/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d - the client resolves this endpoint "+
			"through a no-follow request and needs the Location header",
			resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("no Location header - the server browser would get an empty list URL")
	}
	if !strings.HasSuffix(loc, "/api/servers/stream/") {
		t.Fatalf("unexpected Location: %s", loc)
	}
	if !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "https://") {
		t.Fatalf("Location must be absolute, got %s", loc)
	}
}

func TestStreamEndpointServesFrames(t *testing.T) {
	srv := httptest.NewServer(&handler{})
	defer srv.Close()

	resp, err := noRedirect.Get(srv.URL + "/api/servers/stream/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("unexpected content type: %s", ct)
	}

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := buf[:n]

	if len(body) == 0 {
		// no servers registered - an empty stream is valid
		return
	}

	length := binary.LittleEndian.Uint32(body[0:4])
	if int(length) > len(body)-4 {
		t.Fatalf("frame length %d exceeds payload %d", length, len(body)-4)
	}
}

func TestPublicBaseOverride(t *testing.T) {
	old := publicBaseOverride
	publicBaseOverride = "http://nsm.example:8080"
	defer func() { publicBaseOverride = old }()

	req := httptest.NewRequest(http.MethodGet, "/api/servers/streamRedir/", nil)
	req.Host = "internal.example"

	if got := publicBase(req); got != "http://nsm.example:8080" {
		t.Fatalf("expected the override to win, got %s", got)
	}
}

func TestPublicBaseDerivedFromRequest(t *testing.T) {
	old := publicBaseOverride
	publicBaseOverride = ""
	defer func() { publicBaseOverride = old }()

	req := httptest.NewRequest(http.MethodGet, "/api/servers/streamRedir/", nil)
	req.Host = "nsm.example"

	if got := publicBase(req); got != "http://nsm.example" {
		t.Fatalf("unexpected base: %s", got)
	}
}

func TestIngressAndSingle(t *testing.T) {
	srv := httptest.NewServer(&handler{})
	defer srv.Close()

	body := strings.NewReader(`{"port":30120,"ipOverride":"10.0.0.9",` +
		`"fallbackData":{"dynamic":{"clients":2,"svMaxclients":48,"hostname":"Test"}}}`)
	resp, err := http.Post(srv.URL+"/api/serverlist/ingress", "application/json", body)
	if err != nil {
		t.Fatalf("ingress failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from ingress, got %d", resp.StatusCode)
	}

	sresp, err := noRedirect.Get(srv.URL + "/api/servers/single/10.0.0.9:30120")
	if err != nil {
		t.Fatalf("single failed: %v", err)
	}
	defer sresp.Body.Close()

	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from single, got %d", sresp.StatusCode)
	}
}

// The service must not claim an entitlement that it cannot verify.
func TestEntitlementRoutesAreNotFaked(t *testing.T) {
	srv := httptest.NewServer(&handler{})
	defer srv.Close()

	for _, path := range []string{"/api/validate/entitlement", "/api/validate/entitlement/rosfive"} {
		resp, err := noRedirect.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s returned %d - the service has no entitlement store and "+
				"must not answer with a synthetic success", path, resp.StatusCode)
		}
	}
}
