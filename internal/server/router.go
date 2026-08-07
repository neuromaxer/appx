package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/neuromaxer/appx/internal/auth"
	"github.com/neuromaxer/appx/internal/egress"
	"github.com/neuromaxer/appx/internal/project"
	"github.com/neuromaxer/appx/internal/terminal"
)

// RouterConfig holds runtime configuration that affects routing behaviour.
// Passed to NewRouter so middleware can adapt to the deployment mode.
type RouterConfig struct {
	HTTPMode         bool     // true = plain HTTP dev mode, affects security headers
	BaseDomain       string   // base domain for subdomain routing
	HostAliases      []string // additional hostnames/IPs that also serve the dashboard (e.g. server IP)
	AgentServerURL   string   // URL of the Pi agent-server (default "http://127.0.0.1:4001")
	AgentServerToken string   // optional bearer token for Pi agent-server
}

// NewRouter builds the top-level HTTP handler. All requests go through auth
// middleware (except POST /api/login which is public and rate-limited).
// es must not be nil; pass egress.NewStore(db) from the caller.
// lm must not be nil; pass terminal.NewLocalManager(bufSize) from the caller.
func NewRouter(a *auth.Auth, pm *project.Manager, webFS fs.FS, rcfg RouterConfig, es *egress.Store, ep *egress.PendingRegistry, lm *terminal.LocalManager) http.Handler {
	mux := http.NewServeMux()

	// Public API routes (no auth) — rate limited
	loginLimiter := newRateLimiter(5*time.Minute, 10)
	mux.Handle("POST /api/login", limitBody(requireJSON(http.HandlerFunc(loginLimiter.middleware(handleLogin(a))))))

	// Protected API routes
	hc := project.NewHealthChecker()
	api := http.NewServeMux()
	api.HandleFunc("GET /api/projects", handleListProjects(pm, hc))
	api.HandleFunc("POST /api/projects", handleCreateProject(pm))
	api.HandleFunc("GET /api/projects/{id}", handleGetProject(pm, hc))
	api.HandleFunc("DELETE /api/projects/{id}", handleDeleteProject(pm))
	api.HandleFunc("PUT /api/settings/password", handleChangePassword(a))
	api.HandleFunc("GET /api/settings/terminal-buffer-size", handleGetTerminalBufferSize(a.Store))
	api.HandleFunc("PUT /api/settings/terminal-buffer-size", handleSetTerminalBufferSize(a.Store))
	api.HandleFunc("GET /api/config", handleGetConfig(rcfg.BaseDomain))
	api.HandleFunc("DELETE /api/session", handleLogout(a))
	api.HandleFunc("GET /api/egress/log", handleGetEgressLog(es))
	api.HandleFunc("GET /api/egress/allowlist", handleGetAllowlist(es))
	api.HandleFunc("PUT /api/egress/allowlist", handleSetAllowlist(es))
	if ep != nil {
		api.HandleFunc("GET /api/egress/pending", handleGetEgressPending(ep))
		api.HandleFunc("POST /api/egress/pending/{id}/approve", handleApproveEgressRequest(ep))
		api.HandleFunc("POST /api/egress/pending/{id}/deny", handleDenyEgressRequest(ep))
	}
	agentServerURL := rcfg.AgentServerURL
	if agentServerURL == "" {
		agentServerURL = "http://127.0.0.1:4001"
	}
	agentProxy := agentServerProxyHandler(pm, agentServerURL, rcfg.AgentServerToken)
	api.Handle("GET /api/projects/{id}/agent/{agentPath...}", agentProxy)
	api.Handle("POST /api/projects/{id}/agent/{agentPath...}", agentProxy)
	api.Handle("PATCH /api/projects/{id}/agent/{agentPath...}", agentProxy)
	api.Handle("DELETE /api/projects/{id}/agent/{agentPath...}", agentProxy)
	mux.Handle("/api/", limitBody(a.Middleware(requireJSON(api))))

	// Shell (local PTY) routes — outside the requireJSON api mux because the
	// WebSocket connect endpoint is a GET that must not be blocked by requireJSON.
	mux.Handle("POST /api/shell", a.Middleware(limitBody(http.HandlerFunc(handleShellCreate(lm)))))
	mux.Handle("PUT /api/shell/{id}", a.Middleware(limitBody(requireJSON(http.HandlerFunc(handleShellResize(lm))))))
	mux.Handle("GET /api/shell/{id}/connect", a.Middleware(http.HandlerFunc(handleShellConnect(lm))))

	// agent-client SDK gateway: same-origin 1:1 mirror of the agent-server /v1
	// contract. Mounted on the top-level mux (auth + body limit) rather than the
	// requireJSON-wrapped api mux, because the SDK issues legitimate *bodyless*
	// POSTs (create session, abort) that requireJSON would reject with 415. The
	// route is still CSRF-safe: state-changing requests carry the SameSite=Lax
	// session cookie (not sent on cross-site POST) and auth runs first, so a
	// forged cross-origin request is rejected with 401 before reaching the proxy.
	// This is the single same-origin surface the agent-client SDK uses for
	// sessions, the model catalogue, and runtime credential management
	// (/v1/auth, /v1/custom). More-specific patterns win over the "/api/" mux below.
	agentMirror := agentServerMirrorHandler(pm, agentServerURL, rcfg.AgentServerToken)
	mux.Handle("GET /api/pi/{piPath...}", a.Middleware(agentMirror))
	mux.Handle("POST /api/pi/{piPath...}", a.Middleware(limitBody(agentMirror)))
	mux.Handle("PUT /api/pi/{piPath...}", a.Middleware(limitBody(agentMirror)))
	mux.Handle("PATCH /api/pi/{piPath...}", a.Middleware(limitBody(agentMirror)))
	mux.Handle("DELETE /api/pi/{piPath...}", a.Middleware(agentMirror))

	// React SPA fallback
	fileServer := http.FileServerFS(webFS)
	mux.Handle("/", spaHandler(fileServer, webFS))

	// Build the dashboard handler (base domain requests).
	dashboard := securityHeaders(mux, rcfg.HTTPMode, rcfg.BaseDomain)

	// If no BaseDomain configured, skip subdomain dispatch.
	if rcfg.BaseDomain == "" {
		return dashboard
	}

	// Shared transport for subdomain reverse proxies. Reusing a single
	// transport across requests enables connection pooling and prevents
	// file descriptor exhaustion under load.
	subdomainTransport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	// Build alias set for O(1) lookup. Aliases serve the dashboard just like BaseDomain.
	aliasSet := make(map[string]bool, len(rcfg.HostAliases))
	for _, a := range rcfg.HostAliases {
		aliasSet[a] = true
	}

	// Subdomain dispatcher: inspect Host header to route requests.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := stripPort(r.Host)

		// Base domain or registered alias — serve dashboard.
		if host == rcfg.BaseDomain || aliasSet[host] {
			dashboard.ServeHTTP(w, r)
			return
		}

		// Check for subdomain: <name>.<baseDomain>
		suffix := "." + rcfg.BaseDomain
		if !strings.HasSuffix(host, suffix) {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		projectName := strings.TrimSuffix(host, suffix)
		if projectName == "" {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}

		// A `-dev` label addresses the project's DEV environment; everything else
		// is PROD. The `-dev` reserved-suffix guard on project names (ValidateName)
		// means `<name>-dev` is unambiguously project `<name>`'s dev URL.
		lookupName := projectName
		wantDev := strings.HasSuffix(projectName, "-dev")
		if wantDev {
			lookupName = strings.TrimSuffix(projectName, "-dev")
		}

		// Look up the project.
		proj, err := pm.Store.GetByName(lookupName)
		if err != nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}

		// Select DEV vs PROD port from the label. The proxy target is still
		// 127.0.0.1:<port>; only the port choice changes.
		targetPort := proj.AssignedPort
		if wantDev {
			targetPort = proj.DevPort
		}
		if targetPort == 0 {
			http.Error(w, "environment not available", http.StatusNotFound)
			return
		}

		// Auth middleware wraps the reverse proxy for subdomain requests.
		a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Minimal security headers for proxied content. We intentionally
			// omit a strict CSP (it would break user apps) but still prevent
			// content-type sniffing and enforce HSTS in production.
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if !rcfg.HTTPMode {
				w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}

			// Remove write deadline so long-lived connections (SSE, WebSocket)
			// are not killed by the server's WriteTimeout.
			http.NewResponseController(w).SetWriteDeadline(time.Time{})

			target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", targetPort))
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			proxy := &httputil.ReverseProxy{
				Director: func(req *http.Request) {
					req.URL.Scheme = target.Scheme
					req.URL.Host = target.Host
					req.Host = target.Host
					req.Header.Del("Cookie")
				},
				Transport:     subdomainTransport,
				FlushInterval: -1,
			}
			proxy.ServeHTTP(w, r)
		})).ServeHTTP(w, r)
	})
}

// stripPort removes the port suffix from a host:port string. Uses
// net.SplitHostPort so IPv6 addresses in brackets are handled correctly
// (e.g. "[::1]:8080" → "::1"). Returns the host unchanged if no port is present.
func stripPort(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	return h
}

// spaHandler wraps a file server to support single-page application routing.
// If the requested path matches a real file in webFS it is served directly;
// otherwise the request is rewritten to "/" so the React app handles client-side
// routing.
func spaHandler(fileServer http.Handler, webFS fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "index.html"
		} else {
			path = path[1:]
		}
		if _, err := fs.Stat(webFS, path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

// writeJSON encodes v as JSON and writes it to the response with the
// appropriate Content-Type header. Used by all API handlers to send responses.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
