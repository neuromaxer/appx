package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/neuromaxer/appx/internal/auth"
	"github.com/neuromaxer/appx/internal/egress"
	"github.com/neuromaxer/appx/internal/project"
	"github.com/neuromaxer/appx/internal/terminal"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// testSchema is the minimal in-memory SQLite schema used by all server tests.
// It includes the project columns read by handlers while omitting legacy Docker
// columns that are no longer used.
const testSchema = `
	CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
	CREATE TABLE sessions (token TEXT PRIMARY KEY, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, expires_at DATETIME);
	CREATE TABLE projects (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		status TEXT DEFAULT 'stopped',
		assigned_port INTEGER,
		dev_port INTEGER,
		last_error TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE egress_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id TEXT,
		destination TEXT,
		port INTEGER,
		allowed BOOLEAN NOT NULL DEFAULT 1,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);
`

// setupTest creates an in-memory SQLite database with the full schema,
// sets up auth, creates a project Manager backed by the store, and returns
// the router handler, auth store, and raw DB connection for test use.
func setupTest(t *testing.T) (http.Handler, *auth.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err = db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}

	store := auth.NewStore(db)
	store.SetBcryptCost(bcrypt.MinCost)
	store.SetPassword("testpassword1")
	a := auth.New(store)

	ps := project.NewStore(db)
	pm := project.NewManager(ps, t.TempDir())
	es := egress.NewStore(db)

	webFS := fstest.MapFS{
		"index.html":          {Data: []byte("<html>app</html>")},
		"assets/index-abc.js": {Data: []byte("console.log('hi')")},
	}

	return NewRouter(a, pm, webFS, RouterConfig{}, es, nil, terminal.NewLocalManager(65536)), store, db
}

// setupTestWithHTTPMode creates a test handler configured for HTTP dev mode
// (HTTPMode=true, BaseDomain="localhost"). Used by tests that verify HTTP-mode
// behaviour such as absent HSTS headers and non-Secure cookies.
func setupTestWithHTTPMode(t *testing.T) (http.Handler, *auth.Store, *sql.DB) {
	return setupTestWithConfig(t, RouterConfig{HTTPMode: true, BaseDomain: "localhost"})
}

// setupTestWithConfig creates a test handler with the given RouterConfig. It
// mirrors setupTest but allows callers to control HTTPMode, BaseDomain, and
// other routing options. Cookie attributes on the Auth instance are derived
// from the provided RouterConfig so that cookie tests can assert Domain and
// Secure values.
func setupTestWithConfig(t *testing.T, rcfg RouterConfig) (http.Handler, *auth.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err = db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}

	store := auth.NewStore(db)
	store.SetBcryptCost(bcrypt.MinCost)
	store.SetPassword("testpassword1")
	a := auth.New(store)
	if rcfg.BaseDomain != "" && net.ParseIP(rcfg.BaseDomain) == nil {
		a.Cookie.Domain = "." + rcfg.BaseDomain
	}
	a.Cookie.Secure = !rcfg.HTTPMode

	ps := project.NewStore(db)
	pm := project.NewManager(ps, t.TempDir())
	es := egress.NewStore(db)

	webFS := fstest.MapFS{
		"index.html":          {Data: []byte("<html>app</html>")},
		"assets/index-abc.js": {Data: []byte("console.log('hi')")},
	}

	return NewRouter(a, pm, webFS, rcfg, es, nil, terminal.NewLocalManager(65536)), store, db
}

// authedRequest creates an HTTP request with a valid session cookie.
func authedRequest(t *testing.T, store *auth.Store, method, path string, body string) *http.Request {
	t.Helper()
	token, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "appx_session", Value: token})
	return req
}

func TestLogin_Success(t *testing.T) {
	handler, _, _ := setupTest(t)

	body := strings.NewReader(`{"password":"testpassword1"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should set session cookie
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "appx_session" {
			found = true
			if !c.HttpOnly {
				t.Error("session cookie should be HttpOnly")
			}
			if !c.Secure {
				t.Error("session cookie should be Secure")
			}
		}
	}
	if !found {
		t.Error("expected appx_session cookie")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	handler, _, _ := setupTest(t)

	body := strings.NewReader(`{"password":"wrong-password-but-long-enough"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestLogin_BadJSON(t *testing.T) {
	handler, _, _ := setupTest(t)

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestProtectedRoute_NoAuth(t *testing.T) {
	handler, _, _ := setupTest(t)

	req := httptest.NewRequest("GET", "/api/projects", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestProtectedRoute_WithAuth(t *testing.T) {
	handler, store, _ := setupTest(t)

	req := authedRequest(t, store, "GET", "/api/projects", "")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var projects []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&projects); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("expected empty projects, got %d", len(projects))
	}
}

func TestProtectedRoute_InvalidSession(t *testing.T) {
	handler, _, _ := setupTest(t)

	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "appx_session", Value: "bogus-token"})
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSPA_ServesIndexHTML(t *testing.T) {
	handler, _, _ := setupTest(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html>app</html>") {
		t.Error("expected index.html content")
	}
}

func TestSPA_ServesStaticAsset(t *testing.T) {
	handler, _, _ := setupTest(t)

	req := httptest.NewRequest("GET", "/assets/index-abc.js", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "console.log") {
		t.Error("expected JS content")
	}
}

func TestLogout_ClearsSession(t *testing.T) {
	handler, store, _ := setupTest(t)

	token, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}

	// Confirm session is valid
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "appx_session", Value: token})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 before logout, got %d", w.Code)
	}

	// Logout
	req = httptest.NewRequest("DELETE", "/api/session", nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "appx_session", Value: token})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from logout, got %d: %s", w.Code, w.Body.String())
	}

	// Session should now be invalid
	req = httptest.NewRequest("GET", "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "appx_session", Value: token})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 after logout, got %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler, _, _ := setupTest(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	headers := map[string]string{
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
	}
	for header, want := range headers {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s: got %q, want %q", header, got, want)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("Content-Security-Policy header missing")
	}
}

func TestRateLimit_Login(t *testing.T) {
	handler, _, _ := setupTest(t)

	// Send 11 failed login attempts from the same IP (max is 10 per window)
	var lastCode int
	for i := 0; i < 11; i++ {
		body := strings.NewReader(`{"password":"wrong-password-but-long-enough"}`)
		req := httptest.NewRequest("POST", "/api/login", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		lastCode = w.Code
	}

	if lastCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 after exceeding rate limit, got %d", lastCode)
	}
}

func TestRateLimit_Login_XForwardedFor(t *testing.T) {
	handler, _, _ := setupTest(t)

	var lastCode int
	for i := 0; i < 11; i++ {
		body := strings.NewReader(`{"password":"wrong-password-but-long-enough"}`)
		req := httptest.NewRequest("POST", "/api/login", body)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", "192.168.1.100, 203.0.113.1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		lastCode = w.Code
	}

	if lastCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 after exceeding rate limit, got %d", lastCode)
	}

	body := strings.NewReader(`{"password":"wrong-password-but-long-enough"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "192.168.1.101")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 (allowed to guess password), got %d", w.Code)
	}
}

func TestSPA_FallbackToIndex(t *testing.T) {
	handler, _, _ := setupTest(t)

	// SPA route like /login should return index.html
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html>app</html>") {
		t.Error("expected index.html fallback for SPA route")
	}
}

// --- Project endpoint tests ---

func TestCreateProject_Success(t *testing.T) {
	handler, store, _ := setupTest(t)

	req := authedRequest(t, store, "POST", "/api/projects", `{"name":"my-app"}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var p map[string]any
	json.NewDecoder(w.Body).Decode(&p)
	if p["name"] != "my-app" {
		t.Errorf("expected name my-app, got %v", p["name"])
	}
	if p["status"] != "stopped" {
		t.Errorf("expected status stopped, got %v", p["status"])
	}
	// Auto-assigned port should be in range 10000-10999.
	if port, ok := p["assignedPort"].(float64); !ok || port < 10000 || port > 10999 {
		t.Errorf("expected assignedPort in range 10000-10999, got %v", p["assignedPort"])
	}
}

func TestCreateProject_InvalidName(t *testing.T) {
	handler, store, _ := setupTest(t)

	req := authedRequest(t, store, "POST", "/api/projects", `{"name":"Bad Name"}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateProject_DuplicateName(t *testing.T) {
	handler, store, _ := setupTest(t)

	req := authedRequest(t, store, "POST", "/api/projects", `{"name":"my-app"}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d", w.Code)
	}

	req = authedRequest(t, store, "POST", "/api/projects", `{"name":"my-app"}`)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateProject_NoAuth(t *testing.T) {
	handler, _, _ := setupTest(t)

	req := httptest.NewRequest("POST", "/api/projects", strings.NewReader(`{"name":"my-app"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestGetProject_Success(t *testing.T) {
	handler, store, _ := setupTest(t)

	// Create a project first
	req := authedRequest(t, store, "POST", "/api/projects", `{"name":"my-app"}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var created map[string]any
	json.NewDecoder(w.Body).Decode(&created)
	id := created["id"].(string)

	// Get it
	req = authedRequest(t, store, "GET", "/api/projects/"+id, "")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	json.NewDecoder(w.Body).Decode(&got)
	if got["name"] != "my-app" {
		t.Errorf("expected name my-app, got %v", got["name"])
	}
}

func TestGetProject_NotFound(t *testing.T) {
	handler, store, _ := setupTest(t)

	req := authedRequest(t, store, "GET", "/api/projects/nonexistent", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDeleteProject_Success(t *testing.T) {
	handler, store, _ := setupTest(t)

	// Create
	req := authedRequest(t, store, "POST", "/api/projects", `{"name":"my-app"}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var created map[string]any
	json.NewDecoder(w.Body).Decode(&created)
	id := created["id"].(string)

	// Delete
	req = authedRequest(t, store, "DELETE", "/api/projects/"+id, "")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify gone
	req = authedRequest(t, store, "GET", "/api/projects/"+id, "")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", w.Code)
	}
}

func TestDeleteProject_NotFound(t *testing.T) {
	handler, store, _ := setupTest(t)

	req := authedRequest(t, store, "DELETE", "/api/projects/nonexistent", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// --- Settings endpoint tests ---

func TestSettingsEndpoints_NoAuth(t *testing.T) {
	handler, _, _ := setupTest(t)

	req := httptest.NewRequest("GET", "/api/settings/terminal-buffer-size", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/settings/terminal-buffer-size: expected 401, got %d", w.Code)
	}
}

// --- Terminal buffer size setting tests ---

func TestGetTerminalBufferSize_Default(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "GET", "/api/settings/terminal-buffer-size", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]int
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["value"] != 512 {
		t.Errorf("expected default 512, got %d", resp["value"])
	}
}

func TestSetTerminalBufferSize(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "PUT", "/api/settings/terminal-buffer-size", `{"value":1024}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Verify readback.
	req = authedRequest(t, store, "GET", "/api/settings/terminal-buffer-size", "")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var resp map[string]int
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["value"] != 1024 {
		t.Errorf("expected 1024, got %d", resp["value"])
	}
}

func TestSetTerminalBufferSize_TooSmall(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "PUT", "/api/settings/terminal-buffer-size", `{"value":32}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSetTerminalBufferSize_TooLarge(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "PUT", "/api/settings/terminal-buffer-size", `{"value":8192}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// loginAndGetCookie performs a login request and returns the session cookie.
// Used by tests that need to simulate authenticated requests via cookies.
func loginAndGetCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	body := strings.NewReader(`{"password":"testpassword1"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "appx_session" {
			return c
		}
	}
	t.Fatal("no session cookie in login response")
	return nil
}

func TestSecurityHeaders_NoHSTS_HTTPMode(t *testing.T) {
	handler, _, _ := setupTestWithHTTPMode(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if hsts := w.Header().Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("expected no HSTS header in HTTP mode, got %q", hsts)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff, got %q", got)
	}
}

func TestLogin_CookieHasDomainAttribute(t *testing.T) {
	handler, _, _ := setupTestWithConfig(t, RouterConfig{
		BaseDomain: "localhost",
	})

	body := strings.NewReader(`{"password":"testpassword1"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "appx_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected appx_session cookie")
	}
	// Go's http.ReadSetCookies normalises the Domain by stripping the leading
	// dot, so the parsed cookie has "localhost" even though the Set-Cookie
	// header was emitted with Domain=localhost (set via a.Cookie.Domain=".localhost").
	if sessionCookie.Domain != "localhost" {
		t.Errorf("expected Domain=localhost, got %q", sessionCookie.Domain)
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("expected SameSite=Lax, got %v", sessionCookie.SameSite)
	}
}

func TestLogin_CookieNotSecureInHTTP(t *testing.T) {
	handler, _, _ := setupTestWithConfig(t, RouterConfig{
		BaseDomain: "localhost",
		HTTPMode:   true,
	})

	body := strings.NewReader(`{"password":"testpassword1"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "appx_session" {
			if c.Secure {
				t.Error("expected Secure=false in HTTP mode")
			}
			return
		}
	}
	t.Fatal("expected appx_session cookie")
}

func TestLogin_CookieHasDomainAttributeForHostname(t *testing.T) {
	// Non-localhost hostname should also get a Domain attribute (same code path,
	// confirms it's not special-casing "localhost").
	handler, _, _ := setupTestWithConfig(t, RouterConfig{
		BaseDomain: "myserver.example.com",
	})

	body := strings.NewReader(`{"password":"testpassword1"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Host = "myserver.example.com"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "appx_session" {
			if c.Domain != "myserver.example.com" {
				t.Errorf("expected Domain=myserver.example.com, got %q", c.Domain)
			}
			return
		}
	}
	t.Fatal("expected appx_session cookie")
}

func TestLogin_CookieNoDomainForIP(t *testing.T) {
	// When BaseDomain is an IP address the cookie must have no Domain attribute.
	// Browsers reject Domain=.<ip> cookies per RFC 6265, which silently breaks
	// login when the server is accessed by IP (the cookie is dropped and the next
	// request fails auth, causing an immediate redirect back to /login).
	handler, _, _ := setupTestWithConfig(t, RouterConfig{
		BaseDomain: "192.0.2.1",
		HTTPMode:   false,
	})

	body := strings.NewReader(`{"password":"testpassword1"}`)
	req := httptest.NewRequest("POST", "/api/login", body)
	req.Host = "192.0.2.1"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "appx_session" {
			if c.Domain != "" {
				t.Errorf("expected empty Domain for IP-based access, got %q", c.Domain)
			}
			return
		}
	}
	t.Fatal("expected appx_session cookie")
}

func TestDashboardRouteHasStrictCSP(t *testing.T) {
	h, _, _ := setupTest(t)
	req := httptest.NewRequest("GET", "/api/projects", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if xfo := rr.Header().Get("X-Frame-Options"); xfo != "DENY" {
		t.Errorf("expected X-Frame-Options: DENY, got %q", xfo)
	}
	csp := rr.Header().Get("Content-Security-Policy")
	// script-src must not allow unsafe-inline; style-src may have it for Google Fonts fallback.
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("unexpected unsafe-inline in script-src of dashboard CSP: %q", csp)
	}
	if strings.Contains(csp, "worker-src") {
		t.Errorf("unexpected worker-src in dashboard CSP: %q", csp)
	}
	// With no BaseDomain configured there are no app subdomains to embed, so
	// frame-src stays locked to 'self'.
	if !strings.Contains(csp, "frame-src 'self'") {
		t.Errorf("expected frame-src directive in dashboard CSP, got %q", csp)
	}
}

// TestDashboardCSPAllowsAppSubdomains verifies that when a BaseDomain is
// configured, the dashboard CSP frame-src permits embedding agent-built apps
// served on `<project>.<baseDomain>` subdomains. This is what lets the project
// view preview a running app in an iframe.
func TestDashboardCSPAllowsAppSubdomains(t *testing.T) {
	h, _, _ := setupTestWithConfig(t, RouterConfig{HTTPMode: true, BaseDomain: "localhost"})
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Host = "localhost" // base-domain host → dashboard
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	csp := rr.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-src 'self' http://*.localhost:*") {
		t.Errorf("expected frame-src to allow app subdomains, got %q", csp)
	}
}

// deadlineRecorder wraps httptest.ResponseRecorder and records whether
// SetWriteDeadline was called with the zero time (meaning "no deadline").
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadlineCleared bool
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		r.writeDeadlineCleared = true
	}
	return nil
}

// setupTestWithAgentServerBackend creates a test handler configured to proxy
// /api/projects/{id}/agent/* requests to the given agent-server URL.
func setupTestWithAgentServerBackend(t *testing.T, agentServerURL string, token string) (http.Handler, *auth.Store, *sql.DB) {
	t.Helper()
	rcfg := RouterConfig{AgentServerURL: agentServerURL, AgentServerToken: token}
	return setupTestWithConfig(t, rcfg)
}

func insertProject(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO projects (id, name, status, assigned_port) VALUES (?, ?, 'running', 3000)",
		id,
		"proj-"+id,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentServerProxy_RequiresAuth(t *testing.T) {
	handler, _, db := setupTestWithAgentServerBackend(t, "http://127.0.0.1:4001", "")
	insertProject(t, db, "p1")

	req := httptest.NewRequest("GET", "/api/projects/p1/agent/sessions", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAgentServerProxy_Authed_ForwardsRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"path":   r.URL.Path,
			"query":  r.URL.RawQuery,
			"method": r.Method,
			// agent-server resolves the project from its own registry; appx must
			// not leak any internal project headers to it.
			"projectId": r.Header.Get(agentProjectIDHeader),
		})
	}))
	defer backend.Close()

	handler, store, db := setupTestWithAgentServerBackend(t, backend.URL, "")
	insertProject(t, db, "p1")

	req := authedRequest(t, store, "GET", "/api/projects/p1/agent/sessions?limit=10", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// The proxy addresses agent-server by the project *name* (its slug id),
	// not appx's UUID. insertProject names project p1 "proj-p1".
	if resp["path"] != "/v1/projects/proj-p1/sessions" {
		t.Errorf("expected path /v1/projects/proj-p1/sessions after prefix rewrite, got %q", resp["path"])
	}
	if resp["query"] != "limit=10" {
		t.Errorf("expected query string preserved, got %q", resp["query"])
	}
	if resp["projectId"] != "" {
		t.Errorf("expected internal project headers stripped, got %q", resp["projectId"])
	}
}

func TestAgentServerProxy_StripsCookieAndAddsBearer(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"cookie":        r.Header.Get("Cookie"),
			"authorization": r.Header.Get("Authorization"),
		})
	}))
	defer backend.Close()

	handler, store, db := setupTestWithAgentServerBackend(t, backend.URL, "secret-token")
	insertProject(t, db, "p1")

	req := authedRequest(t, store, "GET", "/api/projects/p1/agent/sessions", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["cookie"] != "" {
		t.Errorf("expected appx cookie to be stripped, got %q", resp["cookie"])
	}
	if resp["authorization"] != "Bearer secret-token" {
		t.Errorf("expected bearer token forwarded, got %q", resp["authorization"])
	}
}

func TestAgentServerProxy_UnknownProjectReturns404(t *testing.T) {
	handler, store, _ := setupTestWithAgentServerBackend(t, "http://127.0.0.1:4001", "")

	req := authedRequest(t, store, "GET", "/api/projects/nope/agent/sessions", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAgentServerProxy_ClearsWriteDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler, store, db := setupTestWithAgentServerBackend(t, backend.URL, "")
	insertProject(t, db, "p1")

	req := authedRequest(t, store, "GET", "/api/projects/p1/agent/sessions/s1/events", "")
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(rec, req)

	if !rec.writeDeadlineCleared {
		t.Error("expected write deadline to be cleared for agent-server proxy requests")
	}
}

func TestSubdomainDispatch_BaseDomain_ServesDashboard(t *testing.T) {
	handler, _, _ := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost"})

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for base domain, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html>app</html>") {
		t.Error("expected SPA content for base domain")
	}
}

func TestSubdomainDispatch_BaseDomainWithPort_ServesDashboard(t *testing.T) {
	handler, _, _ := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost"})

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestSubdomainDispatch_UnknownProject_Returns404(t *testing.T) {
	handler, _, _ := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost"})

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "nonexistent.localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubdomainDispatch_ExistingProject_RequiresAuth(t *testing.T) {
	handler, store, _ := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost"})

	// Create a project.
	req := authedRequest(t, store, "POST", "/api/projects", `{"name":"myapp"}`)
	req.Host = "localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Access subdomain without auth.
	req = httptest.NewRequest("GET", "/", nil)
	req.Host = "myapp.localhost"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSubdomainDispatch_ExistingProject_ProxiesToPort(t *testing.T) {
	// Start a fake app backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello from app")
	}))
	defer backend.Close()

	// Extract port from backend URL.
	backendPort := strings.TrimPrefix(backend.URL, "http://127.0.0.1:")
	port, _ := strconv.Atoi(backendPort)

	handler, store, db := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost"})

	// Insert the project directly into the DB with the backend's port as assigned_port,
	// bypassing Manager.Create (which would scaffold a directory and run git).
	_, err := db.Exec(
		`INSERT INTO projects (id, name, status, assigned_port) VALUES (?, ?, ?, ?)`,
		"test-proxy-id", "myapp", "stopped", port,
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Access subdomain with auth.
	req := authedRequest(t, store, "GET", "/", "")
	req.Host = "myapp.localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello from app") {
		t.Errorf("expected proxied content, got %q", w.Body.String())
	}
}

func TestListProjects_AppRunningField(t *testing.T) {
	handler, store, db := setupTest(t)

	// Insert directly to avoid git dependency in tests.
	_, err := db.Exec("INSERT INTO projects (id, name, status, assigned_port) VALUES ('hid', 'healthtest', 'stopped', 59999)")
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	req := authedRequest(t, store, "GET", "/api/projects", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var projects []struct {
		ID         string `json:"id"`
		AppRunning bool   `json:"appRunning"`
	}
	if err := json.NewDecoder(w.Body).Decode(&projects); err != nil {
		t.Fatal(err)
	}
	if len(projects) == 0 {
		t.Fatal("expected at least one project")
	}
	found := false
	for _, p := range projects {
		if p.ID == "hid" {
			found = true
			if p.AppRunning {
				t.Error("expected appRunning=false for port with no listener")
			}
		}
	}
	if !found {
		t.Error("project 'hid' not found in response")
	}
}

func TestSubdomainDispatch_NoAppxSecurityHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>app</html>")
	}))
	defer backend.Close()

	backendPort := strings.TrimPrefix(backend.URL, "http://127.0.0.1:")
	port, _ := strconv.Atoi(backendPort)

	handler, store, db := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost"})

	// Insert project directly with the backend's port.
	_, err := db.Exec(
		`INSERT INTO projects (id, name, status, assigned_port) VALUES (?, ?, ?, ?)`,
		"test-csp-id", "csptest", "stopped", port,
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	req := authedRequest(t, store, "GET", "/", "")
	req.Host = "csptest.localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	csp := w.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "script-src 'self'") {
		t.Errorf("subdomain response should not have appx strict CSP, got: %q", csp)
	}
}

func TestSubdomainDispatch_HasMinimalSecurityHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	backendPort := strings.TrimPrefix(backend.URL, "http://127.0.0.1:")
	port, _ := strconv.Atoi(backendPort)

	handler, store, db := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost"})

	_, err := db.Exec(
		`INSERT INTO projects (id, name, status, assigned_port) VALUES (?, ?, ?, ?)`,
		"test-hdr-id", "hdrtest", "stopped", port,
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	req := authedRequest(t, store, "GET", "/", "")
	req.Host = "hdrtest.localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Subdomain responses must have X-Content-Type-Options and HSTS.
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("subdomain: expected X-Content-Type-Options: nosniff, got %q", got)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got != "max-age=63072000; includeSubDomains" {
		t.Errorf("subdomain: expected HSTS header, got %q", got)
	}

	// Should NOT have the dashboard-only headers.
	if got := w.Header().Get("X-Frame-Options"); got == "DENY" {
		t.Error("subdomain should not have X-Frame-Options: DENY")
	}
}

func TestSubdomainDispatch_NoHSTS_HTTPMode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	backendPort := strings.TrimPrefix(backend.URL, "http://127.0.0.1:")
	port, _ := strconv.Atoi(backendPort)

	handler, store, db := setupTestWithConfig(t, RouterConfig{BaseDomain: "localhost", HTTPMode: true})

	_, err := db.Exec(
		`INSERT INTO projects (id, name, status, assigned_port) VALUES (?, ?, ?, ?)`,
		"test-nohs-id", "nohstest", "stopped", port,
	)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	req := authedRequest(t, store, "GET", "/", "")
	req.Host = "nohstest.localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("subdomain in HTTP mode should not have HSTS, got %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("subdomain in HTTP mode should still have X-Content-Type-Options, got %q", got)
	}
}

// --- Egress endpoint tests ---

func TestGetEgressLog_Empty(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "GET", "/api/egress/log", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Entries []any `json:"entries"`
		Total   int   `json:"total"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Total != 0 {
		t.Errorf("expected total 0, got %d", resp.Total)
	}
}

func TestGetEgressLog_Unauthenticated(t *testing.T) {
	handler, _, _ := setupTest(t)
	req := httptest.NewRequest("GET", "/api/egress/log", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestGetAllowlist(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "GET", "/api/egress/allowlist", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Entries []string `json:"entries"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Entries) != len(egress.DefaultAllowlist) {
		t.Errorf("expected %d default entries, got %d", len(egress.DefaultAllowlist), len(resp.Entries))
	}
}

func TestPutAllowlist(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "PUT", "/api/egress/allowlist",
		`{"entries":["api.anthropic.com:443","custom.example.com:8080"]}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	req2 := authedRequest(t, store, "GET", "/api/egress/allowlist", "")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	var resp struct {
		Entries []string `json:"entries"`
	}
	json.NewDecoder(w2.Body).Decode(&resp)
	if len(resp.Entries) != 2 {
		t.Errorf("expected 2 entries after update, got %d", len(resp.Entries))
	}
}

func TestPutAllowlist_EmptyArray(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "PUT", "/api/egress/allowlist", `{"entries":[]}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty allowlist, got %d", w.Code)
	}
}

func TestPutAllowlist_InvalidFormat(t *testing.T) {
	handler, store, _ := setupTest(t)
	req := authedRequest(t, store, "PUT", "/api/egress/allowlist",
		`{"entries":["api.anthropic.com"]}`)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing port, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Config endpoint tests ---

func TestGetConfig_ReturnsRuntimeConfig(t *testing.T) {
	handler, store, _ := setupTestWithConfig(t, RouterConfig{BaseDomain: "example.com"})
	req := authedRequest(t, store, "GET", "/api/config", "")
	req.Host = "example.com"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		BaseDomain string `json:"baseDomain"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.BaseDomain != "example.com" {
		t.Errorf("expected example.com, got %q", resp.BaseDomain)
	}
}

func TestGetConfig_RequiresAuth(t *testing.T) {
	handler, _, _ := setupTestWithConfig(t, RouterConfig{BaseDomain: "example.com"})
	req := httptest.NewRequest("GET", "/api/config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// --- ProjectDir field tests ---

func TestGetProject_HasAppRunning(t *testing.T) {
	handler, store, db := setupTest(t)
	// Start a TCP listener to simulate a running app.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	db.Exec("INSERT INTO projects (id, name, status, assigned_port) VALUES ('apprun1', 'runapp', 'stopped', ?)", port)

	req := authedRequest(t, store, "GET", "/api/projects/apprun1", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		AppRunning bool `json:"appRunning"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.AppRunning {
		t.Error("expected appRunning=true when listener is active on assigned port")
	}
}

func TestGetProject_HasProjectDir(t *testing.T) {
	handler, store, db := setupTest(t)
	db.Exec("INSERT INTO projects (id, name, status, assigned_port) VALUES ('p1', 'myapp', 'stopped', 10000)")
	req := authedRequest(t, store, "GET", "/api/projects/p1", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		ProjectDir string `json:"projectDir"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.ProjectDir == "" {
		t.Error("expected non-empty projectDir in response")
	}
	if !strings.Contains(resp.ProjectDir, "myapp") {
		t.Errorf("expected projectDir to contain project name, got %q", resp.ProjectDir)
	}
}

func TestListProjects_HasProjectDir(t *testing.T) {
	handler, store, db := setupTest(t)
	db.Exec("INSERT INTO projects (id, name, status, assigned_port) VALUES ('p2', 'listapp', 'stopped', 10001)")
	req := authedRequest(t, store, "GET", "/api/projects", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var projects []struct {
		Name       string `json:"name"`
		ProjectDir string `json:"projectDir"`
	}
	json.NewDecoder(w.Body).Decode(&projects)
	if len(projects) == 0 {
		t.Fatal("expected at least one project")
	}
	for _, p := range projects {
		if p.Name == "listapp" {
			if p.ProjectDir == "" {
				t.Error("expected non-empty projectDir in list response")
			}
			if !strings.Contains(p.ProjectDir, "listapp") {
				t.Errorf("expected projectDir to contain project name, got %q", p.ProjectDir)
			}
			return
		}
	}
	t.Error("project 'listapp' not found in list response")
}

func TestStripPort(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"localhost:8080", "localhost"},
		{"localhost", "localhost"},
		{"example.com:443", "example.com"},
		{"[::1]:8080", "::1"},
		{"[::1]", "[::1]"}, // no port — returned as-is
		{"127.0.0.1:443", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1"},
	}
	for _, tt := range tests {
		got := stripPort(tt.input)
		if got != tt.want {
			t.Errorf("stripPort(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPutAllowlist_BlocksLoopback(t *testing.T) {
	loopbackEntries := []string{
		`{"entries":["localhost:4096"]}`,
		`{"entries":["127.0.0.1:9080"]}`,
		`{"entries":["::1:443"]}`,
		`{"entries":["myapp.localhost:3000"]}`,
	}
	handler, store, _ := setupTest(t)
	for _, body := range loopbackEntries {
		req := authedRequest(t, store, "PUT", "/api/egress/allowlist", body)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %s, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

func TestPutAllowlist_BlocksPrivateIPs(t *testing.T) {
	privateEntries := []string{
		`{"entries":["10.0.0.5:8080"]}`,
		`{"entries":["172.16.1.1:443"]}`,
		`{"entries":["192.168.1.100:3000"]}`,
		`{"entries":["169.254.0.1:80"]}`,
	}
	handler, store, _ := setupTest(t)
	for _, body := range privateEntries {
		req := authedRequest(t, store, "PUT", "/api/egress/allowlist", body)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %s, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

func TestChangePassword_Success(t *testing.T) {
	handler, store, _ := setupTest(t)

	body := `{"currentPassword":"testpassword1","newPassword":"newpassword12345"}`
	req := authedRequest(t, store, "PUT", "/api/settings/password", body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Old password should no longer work.
	ok, _ := store.CheckPassword("testpassword1")
	if ok {
		t.Error("old password should no longer be valid")
	}

	// New password should work.
	ok, _ = store.CheckPassword("newpassword12345")
	if !ok {
		t.Error("new password should be valid")
	}

	// Response should set a fresh session cookie.
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "appx_session" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("expected fresh appx_session cookie after password change")
	}
}

func TestChangePassword_WrongCurrentPassword(t *testing.T) {
	handler, store, _ := setupTest(t)

	body := `{"currentPassword":"wrong-password-here","newPassword":"newpassword12345"}`
	req := authedRequest(t, store, "PUT", "/api/settings/password", body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestChangePassword_TooShort(t *testing.T) {
	handler, store, _ := setupTest(t)

	body := `{"currentPassword":"testpassword1","newPassword":"short"}`
	req := authedRequest(t, store, "PUT", "/api/settings/password", body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestChangePassword_InvalidatesOtherSessions(t *testing.T) {
	handler, store, _ := setupTest(t)

	// Create a session that should be invalidated.
	oldToken, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}

	body := `{"currentPassword":"testpassword1","newPassword":"newpassword12345"}`
	req := authedRequest(t, store, "PUT", "/api/settings/password", body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The old session should be invalid.
	if store.ValidSession(oldToken) {
		t.Error("old session should have been invalidated after password change")
	}
}

// --- agent-server /v1 mirror (agent-client SDK gateway) ---------------------

func TestAgentMirror_RequiresAuth(t *testing.T) {
	handler, _, db := setupTestWithAgentServerBackend(t, "http://127.0.0.1:4001", "")
	insertProject(t, db, "p1")

	req := httptest.NewRequest("GET", "/api/pi/v1/projects/proj-p1/sessions", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAgentMirror_ForwardsProjectSessionTraffic(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"path":          r.URL.Path,
			"query":         r.URL.RawQuery,
			"cookie":        r.Header.Get("Cookie"),
			"authorization": r.Header.Get("Authorization"),
		})
	}))
	defer backend.Close()

	handler, store, db := setupTestWithAgentServerBackend(t, backend.URL, "secret-token")
	insertProject(t, db, "p1") // name == "proj-p1"

	req := authedRequest(t, store, "GET", "/api/pi/v1/projects/proj-p1/sessions?limit=10", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	// The /v1 contract path is forwarded verbatim (1:1 mirror), not rewritten.
	if resp["path"] != "/v1/projects/proj-p1/sessions" {
		t.Errorf("expected verbatim /v1 path, got %q", resp["path"])
	}
	if resp["query"] != "limit=10" {
		t.Errorf("expected query preserved, got %q", resp["query"])
	}
	if resp["cookie"] != "" {
		t.Errorf("expected appx cookie stripped, got %q", resp["cookie"])
	}
	if resp["authorization"] != "Bearer secret-token" {
		t.Errorf("expected bearer token forwarded, got %q", resp["authorization"])
	}
}

func TestAgentMirror_AllowsGlobalModelCatalogue(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path})
	}))
	defer backend.Close()

	handler, store, _ := setupTestWithAgentServerBackend(t, backend.URL, "")

	req := authedRequest(t, store, "GET", "/api/pi/v1/sessions/models", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["path"] != "/v1/sessions/models" {
		t.Errorf("expected /v1/sessions/models, got %q", resp["path"])
	}
}

// TestAgentMirror_AllowsCredentialRoutes confirms the mirror forwards the
// runtime-global credential + custom-provider management routes (/v1/auth,
// /v1/custom) for authenticated users. agent-client's AgentSettings panel uses
// these through the same /api/pi gateway as session traffic.
func TestAgentMirror_AllowsCredentialRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"path":          r.URL.Path,
			"method":        r.Method,
			"cookie":        r.Header.Get("Cookie"),
			"authorization": r.Header.Get("Authorization"),
		})
	}))
	defer backend.Close()

	handler, store, _ := setupTestWithAgentServerBackend(t, backend.URL, "secret-token")

	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/api/pi/v1/auth/providers"},
		{"PUT", "/api/pi/v1/auth/providers/anthropic/api-key"},
		{"DELETE", "/api/pi/v1/auth/providers/anthropic"},
		{"POST", "/api/pi/v1/auth/providers/openai-codex/subscription/start"},
		{"GET", "/api/pi/v1/custom/providers"},
		{"PUT", "/api/pi/v1/custom/providers"},
		{"DELETE", "/api/pi/v1/custom/providers/litellm"},
	}
	for _, tc := range cases {
		req := authedRequest(t, store, tc.method, tc.path, "{}")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: expected 200, got %d: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		// The /v1 contract path is forwarded verbatim (1:1 mirror).
		wantPath := strings.TrimPrefix(tc.path, "/api/pi")
		if resp["path"] != wantPath {
			t.Errorf("%s %s: expected forwarded path %q, got %q", tc.method, tc.path, wantPath, resp["path"])
		}
		if resp["cookie"] != "" {
			t.Errorf("%s %s: expected appx cookie stripped, got %q", tc.method, tc.path, resp["cookie"])
		}
		if resp["authorization"] != "Bearer secret-token" {
			t.Errorf("%s %s: expected bearer token forwarded, got %q", tc.method, tc.path, resp["authorization"])
		}
	}
}

func TestAgentMirror_CredentialRoutesRequireAuth(t *testing.T) {
	handler, _, _ := setupTestWithAgentServerBackend(t, "http://127.0.0.1:4001", "")

	req := httptest.NewRequest("GET", "/api/pi/v1/auth/providers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated credential request, got %d", w.Code)
	}
}

func TestAgentMirror_UnknownProjectForbidden(t *testing.T) {
	handler, store, _ := setupTestWithAgentServerBackend(t, "http://127.0.0.1:4001", "")

	req := authedRequest(t, store, "GET", "/api/pi/v1/projects/ghost/sessions", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for unregistered project, got %d", w.Code)
	}
}

func TestAgentMirror_ProjectLifecycleRoutesForbidden(t *testing.T) {
	handler, store, db := setupTestWithAgentServerBackend(t, "http://127.0.0.1:4001", "")
	insertProject(t, db, "p1")

	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/api/pi/v1/projects"},                  // list all projects
		{"POST", "/api/pi/v1/projects"},                 // create project
		{"GET", "/api/pi/v1/projects/proj-p1"},          // bare project metadata
		{"DELETE", "/api/pi/v1/projects/proj-p1"},       // delete project
		{"GET", "/api/pi/v1/projects/proj-p1/settings"}, // non-session subresource
		{"GET", "/api/pi/v1/healthz"},                   // runtime liveness (not a session/credential route)
	}
	for _, tc := range cases {
		req := authedRequest(t, store, tc.method, tc.path, "")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected 403, got %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestAgentMirror_ClearsWriteDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler, store, db := setupTestWithAgentServerBackend(t, backend.URL, "")
	insertProject(t, db, "p1")

	req := authedRequest(t, store, "GET", "/api/pi/v1/projects/proj-p1/sessions/s1/events", "")
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(rec, req)

	if !rec.writeDeadlineCleared {
		t.Error("expected write deadline cleared for SSE mirror requests")
	}
}

// TestAgentMirror_AllowsBodylessPost reproduces the agent-client SDK's
// createSession/abort calls: a POST with no body and no Content-Type. These
// must be forwarded, not rejected with 415 by requireJSON (regression: the
// mirror is mounted outside the requireJSON-wrapped api mux for this reason).
func TestAgentMirror_AllowsBodylessPost(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "method": r.Method})
	}))
	defer backend.Close()

	handler, store, db := setupTestWithAgentServerBackend(t, backend.URL, "")
	insertProject(t, db, "p1") // name == "proj-p1"

	// Authenticated, but deliberately NO Content-Type header and NO body.
	token, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/pi/v1/projects/proj-p1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: "appx_session", Value: token})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code == http.StatusUnsupportedMediaType {
		t.Fatal("bodyless POST was rejected with 415; mirror must bypass requireJSON")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["path"] != "/v1/projects/proj-p1/sessions" || resp["method"] != "POST" {
		t.Errorf("unexpected forwarded request: %v", resp)
	}
}

// TestAgentMirror_BodylessPostRequiresAuth confirms dropping requireJSON did not
// drop authentication: an unauthenticated bodyless POST is still rejected.
func TestAgentMirror_BodylessPostRequiresAuth(t *testing.T) {
	handler, _, db := setupTestWithAgentServerBackend(t, "http://127.0.0.1:4001", "")
	insertProject(t, db, "p1")

	req := httptest.NewRequest("POST", "/api/pi/v1/projects/proj-p1/sessions", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d", w.Code)
	}
}
