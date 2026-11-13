package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Framework installation: complete recipes, idempotence, managed config.
// The fixture test runs everywhere; the real-download test exercises the
// actual GitHub recipes (run with NSM_NET_TESTS=1).
// ---------------------------------------------------------------------------

func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveFixtures swaps fwHTTPGet for one test: URLs containing a key return
// the fixture bytes.
func serveFixtures(t *testing.T, served map[string][]byte) {
	t.Helper()
	old := fwHTTPGet
	t.Cleanup(func() { fwHTTPGet = old })
	fwHTTPGet = func(ctx context.Context, rawURL string) (*http.Response, error) {
		for key, body := range served {
			if strings.Contains(rawURL, key) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
			}
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}
}

func fixtureRecipe() fwRecipe {
	return fwRecipe{
		ID: "fixturefw", Name: "FixtureFW", Version: "1.0",
		Detect: "[fx]/fx-core",
		Parts: []fwPart{
			{Repo: "example/fx-core", Ref: "main", Dest: "[fx]/fx-core", SQL: []string{"fx-core.sql"}},
			{Repo: "example/fx-ui", Ref: "main", Dest: "[fx]/fx-ui"},
			{Repo: "example/fx-tools", Ref: "main", Dest: "[standalone]/fx-tools", AssetRE: `^fx-tools\.zip$`},
			{Repo: "example/fx-recipe", Ref: "main", SQL: []string{"schema.sql"}},
		},
		Ensures:  []string{"fx-tools", "fx-core", "fx-ui"},
		CfgLines: []string{"set onesync on"},
	}
}

func TestFrameworkInstallIsCompleteAndIdempotent(t *testing.T) {
	h := setupTest(t)
	_ = h
	resetHostMgr(t)
	fake := newFakeDBProv()
	dbProvOverride = fake
	defer func() { dbProvOverride = nil }()

	serveFixtures(t, map[string][]byte{
		"api.github.com/repos/example/fx-tools/releases/latest": []byte(
			`{"assets":[{"name":"fx-tools.zip","url":"https://example.invalid/fx-tools.zip"}]}`),
		"example.invalid/fx-tools.zip": buildZip(t, map[string]string{
			"fx-tools/fxmanifest.lua": `fx_version 'cerulean'`,
			"fx-tools/init.lua":       "-- tools",
		}),
		"codeload.github.com/example/fx-core": buildZip(t, map[string]string{
			"fx-core-main/fxmanifest.lua": `fx_version 'cerulean'`,
			"fx-core-main/server.lua":     "-- core",
			"fx-core-main/fx-core.sql":    "CREATE TABLE fx_core (id INT);",
		}),
		"codeload.github.com/example/fx-ui": buildZip(t, map[string]string{
			"fx-ui-main/fxmanifest.lua": `fx_version 'cerulean'`,
			"fx-ui-main/client.lua":    "-- ui",
		}),
		"codeload.github.com/example/fx-recipe": buildZip(t, map[string]string{
			"fx-recipe-main/schema.sql": "CREATE TABLE fx_extra (id INT);",
		}),
	})

	m := hostMgr()
	m.mu.Lock()
	s := &HostedServer{ID: "hfwfix", Owner: 401, OwnerName: "fwuser", Name: "fw-city", CreatedAt: time.Now().Unix(), Status: hostOffline}
	_ = os.MkdirAll(m.dirOf(s), 0o755)
	m.st.Servers = append(m.st.Servers, s)
	m.saveLocked()
	m.mu.Unlock()

	rec := fixtureRecipe()
	rep := m.installFramework(context.Background(), s, rec)
	if rep.Failed != "" {
		t.Fatalf("install failed: %+v", rep)
	}
	if len(rep.Installed) != 3 {
		t.Fatalf("all 3 resources must install, got %+v", rep)
	}
	root := m.serverRoot(s)
	for _, d := range []string{"[fx]/fx-core", "[fx]/fx-ui", "[standalone]/fx-tools"} {
		if _, err := os.Stat(filepath.Join(root, "resources", filepath.FromSlash(d), "fxmanifest.lua")); err != nil {
			t.Fatalf("resource %s must be installed with its fxmanifest: %v", d, err)
		}
	}
	// SQL reached the database with the customer-scoped schema.
	if len(fake.imports["nsm_401_main"]) != 2 {
		t.Fatalf("both schemas must import into the framework db: %v", fake.imports)
	}
	// server.cfg managed block: ordered ensures + onesync + real connection string.
	cfg, _ := os.ReadFile(filepath.Join(root, "server.cfg"))
	cfgS := string(cfg)
	for _, want := range []string{
		"# >>> nsm-framework:fixturefw", "set onesync on",
		"set mysql_connection_string \"mysql://nsmu_401_main:",
		"ensure fx-tools\nensure fx-core\nensure fx-ui\n",
		"# <<< nsm-framework <<<",
	} {
		if !strings.Contains(cfgS, want) {
			t.Fatalf("server.cfg missing %q:\n%s", want, cfgS)
		}
	}

	// user config outside the managed block survives, and a re-run is
	// idempotent: existing resources are kept, nothing duplicates.
	_ = os.WriteFile(filepath.Join(root, "server.cfg"), append([]byte("# my own line\nset sv_hostname \"x\"\n"), cfg...), 0o644)
	// user edits an existing resource file
	userFile := filepath.Join(root, "resources", "[fx]", "fx-core", "server.lua")
	_ = os.WriteFile(userFile, []byte("-- USER EDITED"), 0o644)
	rep2 := m.installFramework(context.Background(), s, rec)
	if rep2.Failed != "" {
		t.Fatalf("reinstall failed: %+v", rep2)
	}
	if len(rep2.Installed) != 0 || len(rep2.Skipped) != 3 {
		t.Fatalf("reinstall must skip existing resources: %+v", rep2)
	}
	b, _ := os.ReadFile(userFile)
	if string(b) != "-- USER EDITED" {
		t.Fatal("reinstall overwrote a user-modified resource file")
	}
	cfg2, _ := os.ReadFile(filepath.Join(root, "server.cfg"))
	if !strings.Contains(string(cfg2), "# my own line") || strings.Count(string(cfg2), "ensure fx-core") != 1 {
		t.Fatalf("managed cfg rewrite must keep user lines and not duplicate ensures:\n%s", cfg2)
	}
}

func TestFrameworkCfgBlockSwitchesBetweenFrameworks(t *testing.T) {
	cfgA := "# >>> nsm-framework:a — managed by the NSM Hosting panel >>>\nensure a1\n# <<< nsm-framework <<<\n"
	cfgB := "# >>> nsm-framework:b — managed by the NSM Hosting panel >>>\nensure b1\n# <<< nsm-framework <<<\n"
	p := filepath.Join(t.TempDir(), "server.cfg")
	if err := fwApplyCfg(p, "a", []string{"ensure a1"}); err != nil {
		t.Fatal(err)
	}
	if err := fwApplyCfg(p, "b", []string{"ensure b1"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if strings.Contains(s, "ensure a1") || strings.Count(s, "ensure b1") != 1 {
		t.Fatalf("switching frameworks must replace the managed block, got:\n%s", s)
	}
	_ = cfgA
	_ = cfgB
}

func TestFrameworkDetectsMissingDependencies(t *testing.T) {
	h := setupTest(t)
	_ = h
	resetHostMgr(t)
	dbProvOverride = nil
	serveFixtures(t, map[string][]byte{
		"codeload.github.com/example/fx-core": buildZip(t, map[string]string{
			"fx-core-main/fxmanifest.lua": `fx_version 'cerulean'`,
		}),
	})
	m := hostMgr()
	m.mu.Lock()
	s := &HostedServer{ID: "hfwmiss", Owner: 402, OwnerName: "fwuser2", Name: "fw-miss", CreatedAt: time.Now().Unix(), Status: hostOffline}
	_ = os.MkdirAll(m.dirOf(s), 0o755)
	m.st.Servers = append(m.st.Servers, s)
	m.saveLocked()
	m.mu.Unlock()

	rec := fwRecipe{
		ID: "partialfw", Parts: []fwPart{{Repo: "example/fx-core", Ref: "main", Dest: "[fx]/fx-core"}},
		Ensures: []string{"fx-core", "missing-dep"},
	}
	rep := m.installFramework(context.Background(), s, rec)
	if len(rep.Missing) != 1 || rep.Missing[0] != "missing-dep" {
		t.Fatalf("missing dependency must be detected and reported: %+v", rep)
	}
	// no database service: schemas are saved to disk, nothing is faked.
	rec2 := fwRecipe{
		ID: "schemafw", Parts: []fwPart{{Repo: "example/fx-core", Ref: "main", Dest: "[fx]/fx-core", SQL: []string{"fx-core.sql"}}},
		Ensures: []string{"fx-core"},
	}
	serveFixtures(t, map[string][]byte{
		"codeload.github.com/example/fx-core": buildZip(t, map[string]string{
			"fx-core-main/fxmanifest.lua": `fx_version 'cerulean'`,
			"fx-core-main/fx-core.sql":    "CREATE TABLE t (id INT);",
		}),
	})
	rep2 := m.installFramework(context.Background(), s, rec2)
	joined := strings.Join(rep2.SQL, " ")
	if !strings.Contains(joined, "nsm-db-import") {
		t.Fatalf("unconfigured database must leave the schema for manual import: %+v", rep2)
	}
	if _, err := os.Stat(filepath.Join(m.serverRoot(s), "nsm-db-import")); err != nil {
		t.Fatalf("nsm-db-import/ must exist for HeidiSQL manual import: %v", err)
	}
}

func TestFrameworkInstallEndpoint(t *testing.T) {
	h := setupTest(t)
	resetHostMgr(t)
	dbProvOverride = nil
	serveFixtures(t, map[string][]byte{
		"codeload.github.com/example/fx-core": buildZip(t, map[string]string{
			"fx-core-main/fxmanifest.lua": `fx_version 'cerulean'`,
		}),
	})
	sess := "fw-sess-1"
	accMu.Lock()
	acc.Sessions[sess] = Session{UserID: 501, Expires: time.Now().Add(time.Hour).Unix()}
	acc.Users = append(acc.Users, User{ID: 501, Username: "fwapi", Role: "user"})
	accMu.Unlock()
	m := hostMgr()
	m.mu.Lock()
	s := &HostedServer{ID: "hfwapi", Owner: 501, OwnerName: "fwapi", Name: "fw-api", CreatedAt: time.Now().Unix(), Status: hostOffline}
	_ = os.MkdirAll(m.serverRoot(s), 0o755)
	m.st.Servers = append(m.st.Servers, s)
	m.saveLocked()
	m.mu.Unlock()
	auth := map[string]string{"X-NSM-Session": sess}

	// unknown app
	w := requestAuth(h, "POST", "/api/host/hfwapi/apps/nope/install", "", auth)
	if w.Code != 404 {
		t.Fatalf("unknown app must 404: %d", w.Code)
	}
	// busy guard
	m.mu.Lock()
	fwInstallState.busy["hfwapi"] = true
	m.mu.Unlock()
	w = requestAuth(h, "POST", "/api/host/hfwapi/apps/qbcore/install", "", auth)
	if w.Code != 409 {
		t.Fatalf("busy install must 409: %d %s", w.Code, w.Body.String())
	}
	m.mu.Lock()
	delete(fwInstallState.busy, "hfwapi")
	m.mu.Unlock()
	// valid start returns 202 and the recipe id
	w = requestAuth(h, "POST", "/api/host/hfwapi/apps/qbcore/install", "", auth)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"recipe":"qbcore"`) {
		t.Fatalf("install must start async with the recipe id: %d %s", w.Code, w.Body.String())
	}
	// apps listing reports progress counts + busy flag
	deadline := time.Now().Add(30 * time.Second)
	for {
		w = requestAuth(h, "GET", "/api/host/hfwapi/apps", "", auth)
		if w.Code == 200 && strings.Contains(w.Body.String(), `"busy":false`) && strings.Contains(w.Body.String(), `"installedResources"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("install did not finish: %s", w.Body.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestFrameworkRealRecipeDownloads runs the REAL recipes against the REAL
// GitHub repos (source zips). Release-asset parts (oxmysql, vMenu) are
// stubbed through the same seam because the sandbox blocks the asset host —
// on the production VPS they download for real. Gated by NSM_NET_TESTS=1 so
// CI stays fast; run locally before shipping recipe changes.
func TestFrameworkRealRecipeDownloads(t *testing.T) {
	if os.Getenv("NSM_NET_TESTS") != "1" {
		t.Skip("set NSM_NET_TESTS=1 to run real GitHub recipe downloads")
	}
	h := setupTest(t)
	_ = h
	resetHostMgr(t)
	dbProvOverride = nil

	// minimal release-asset fixture for the oxmysql zip (sandbox has no
	// access to release-assets.githubusercontent.com)
	oxmysqlZip := buildZip(t, map[string]string{
		"oxmysql/fxmanifest.lua": `fx_version 'cerulean'`,
		"oxmysql/dist/oxmysql.js": "-- built driver",
	})
	old := fwHTTPGet
	defer func() { fwHTTPGet = old }()
	fwHTTPGet = func(ctx context.Context, rawURL string) (*http.Response, error) {
		if strings.Contains(rawURL, "releases/latest") && strings.Contains(rawURL, "oxmysql") {
			// minimal releases API answer with one matching asset
			body := `{"assets":[{"name":"oxmysql.zip","url":"https://example.invalid/oxmysql.zip"}]}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		if strings.Contains(rawURL, "example.invalid/oxmysql.zip") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(oxmysqlZip))}, nil
		}
		return old(ctx, rawURL) // everything else: real codeload downloads
	}

	m := hostMgr()
	m.mu.Lock()
	s := &HostedServer{ID: "hfwreal", Owner: 601, OwnerName: "fwreal", Name: "fw-real", CreatedAt: time.Now().Unix(), Status: hostOffline}
	_ = os.MkdirAll(m.serverRoot(s), 0o755)
	m.st.Servers = append(m.st.Servers, s)
	m.saveLocked()
	m.mu.Unlock()

	for _, id := range []string{"esx", "qbox", "qbcore"} {
		rec := frameworkRecipes[id]
		rep := m.installFramework(context.Background(), s, rec)
		if rep.Failed != "" {
			t.Fatalf("%s: real install failed: %+v", id, rep)
		}
		root := m.serverRoot(s)
		for _, p := range rec.Parts {
			if p.Dest == "" {
				continue
			}
			dest := filepath.Join(root, "resources", filepath.FromSlash(p.Dest))
			if p.Group {
				ents, err := os.ReadDir(dest)
				if err != nil || len(ents) == 0 {
					t.Fatalf("%s: group %s empty: %v", id, p.Dest, err)
				}
				continue
			}
			if _, err := os.Stat(filepath.Join(dest, "fxmanifest.lua")); err != nil {
				t.Fatalf("%s: %s missing fxmanifest after install: %v", id, p.Dest, err)
			}
		}
		if len(rep.Missing) > 0 {
			t.Fatalf("%s: missing deps after install: %v", id, rep.Missing)
		}
		if !rep.Cfg {
			t.Fatalf("%s: cfg not written", id)
		}
		if len(rep.SQL) == 0 && id != "vmenu" {
			t.Fatalf("%s: schemas not collected: %+v", id, rep)
		}
		// each framework replaces the previous managed block
		cfg, _ := os.ReadFile(filepath.Join(m.serverRoot(s), "server.cfg"))
		if strings.Count(string(cfg), "# >>> nsm-framework:") != 1 {
			t.Fatalf("%s: exactly one managed block expected:\n%s", id, cfg)
		}
	}
}

// TestGenerateCfgPreservesFrameworkBlock pins the server-file reinstall path:
// generateCfg must carry the managed framework block over instead of wiping
// the framework's startup order and database connection.
func TestGenerateCfgPreservesFrameworkBlock(t *testing.T) {
	h := setupTest(t)
	_ = h
	resetHostMgr(t)
	m := hostMgr()
	m.mu.Lock()
	s := &HostedServer{ID: "hcfgkeep", Owner: 701, OwnerName: "cfgu", Name: "cfg-city", CreatedAt: time.Now().Unix(), Status: hostOffline}
	root := m.serverRoot(s)
	_ = os.MkdirAll(root, 0o755)
	m.st.Servers = append(m.st.Servers, s)
	m.mu.Unlock()
	block := "# >>> nsm-framework:qbcore — managed by the NSM Hosting panel >>>\nset onesync on\nensure oxmysql\nensure qb-core\n# <<< nsm-framework <<<\n"
	_ = os.WriteFile(filepath.Join(root, "server.cfg"), []byte("# user line\n"+block), 0o644)
	cfg := m.generateCfg(s, root)
	if !strings.Contains(cfg, "# >>> nsm-framework:qbcore") || !strings.Contains(cfg, "ensure qb-core") {
		t.Fatalf("regenerated cfg must keep the framework block:\n%s", cfg)
	}
	if !strings.Contains(cfg, "ensure qb-core\n# <<< nsm-framework <<<") {
		t.Fatalf("block must be complete and uncorrupted:\n%s", cfg)
	}
}
