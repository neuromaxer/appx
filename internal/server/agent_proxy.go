package server

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/neuromaxer/appx/internal/project"
)

const (
	// agentProjectIDHeader carries the resolved agent-server project id (== appx
	// project name) from the proxy handler to the reverse-proxy Director. It is
	// consumed and stripped before the request leaves appx; it is never sent to
	// agent-server, which derives the project directory from its own registry.
	agentProjectIDHeader = "X-Appx-Project-Id"
)

// agentServerProxyHandler proxies Appx-authenticated project agent requests to
// a loopback Pi agent-server instance. The browser only sees same-origin Appx
// URLs; cookies and optional agent-server bearer credentials stay server-side.
//
// appx addresses projects by its own UUID, but agent-server keys projects by
// slug id (== the appx project name). We resolve the project here and hand the
// agent-server id to the Director via a short-lived internal header.
func agentServerProxyHandler(pm *project.Manager, backendURL string, token string) http.Handler {
	proxy := agentServerReverseProxy(backendURL, token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("id")
		if projectID == "" {
			http.Error(w, "project id required", http.StatusBadRequest)
			return
		}
		proj, err := pm.Get(projectID)
		if err != nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}

		// SSE streams can live much longer than the server write timeout.
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
		r = r.Clone(r.Context())
		r.Header.Set(agentProjectIDHeader, proj.Name)
		proxy.ServeHTTP(w, r)
	})
}

func agentServerReverseProxy(backendURL string, token string) http.Handler {
	target, err := url.Parse(backendURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf("invalid agent-server URL %q", backendURL), http.StatusInternalServerError)
		})
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			agentPath := strings.TrimPrefix(req.PathValue("agentPath"), "/")
			// Use the resolved agent-server id (== project name), not appx's
			// UUID path value, then strip the internal header.
			agentProjectID := req.Header.Get(agentProjectIDHeader)
			proxyPrefix := "/v1/projects/" + url.PathEscape(agentProjectID)
			req.URL.Path = cleanAgentServerPath(proxyPrefix, agentPath)
			req.URL.RawPath = ""
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Header.Del("Cookie")
			// Internal headers never leave appx; agent-server resolves the project
			// directory from its own persisted registry.
			req.Header.Del(agentProjectIDHeader)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("agent-server proxy error path=%s: %v", r.URL.Path, err)
			http.Error(w, "agent-server unavailable", http.StatusBadGateway)
		},
		FlushInterval: -1,
	}

	return proxy
}

func cleanAgentServerPath(prefix string, agentPath string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(agentPath, "/"))
	if cleaned == "/" {
		return prefix
	}
	return prefix + cleaned
}

// agentServerMirrorHandler proxies the agent-server `/v1` contract 1:1 under a
// single same-origin appx mount, so the agent-client SDK can talk to agent-server
// through appx without per-operation URL rewriting (the SDK is configured with
// one baseUrl + the native `/v1` prefix). The browser sees only same-origin
// appx URLs; the agent-server bearer token and the appx cookie stay
// server-side.
//
// Access control (OWASP A01 — broken access control): appx is a per-project
// control plane, so the mirror only forwards a deliberately narrow slice of the
// contract:
//   - GET /v1/sessions/...             session-independent, read-only globals
//                                      (e.g. the model catalogue); project-agnostic.
//   - /v1/auth/...   /v1/custom/...     runtime-global credential + custom-provider
//                                      management (provider API keys, subscription
//                                      logins, models.json providers). These are not
//                                      project-scoped: they configure the single
//                                      agent-server process appx fronts. Exposed only
//                                      to authenticated dashboard users (the SameSite
//                                      cookie + auth middleware run before the proxy).
//   - /v1/projects/{slug}/sessions...  session traffic, only when the
//                                      authenticated user owns a project whose
//                                      slug (== appx project name) is registered.
//
// Project lifecycle routes (`POST/GET /v1/projects`, `GET/DELETE
// /v1/projects/{slug}`) are intentionally NOT exposed: project creation and
// deletion go through appx's own `/api/projects` surface, which also assigns
// ports/subdomains and owns the control-plane record. This prevents a logged-in
// user from reaching unregistered agent-server projects (e.g. another tenant's
// or another product's) that happen to share the backend.
func agentServerMirrorHandler(pm *project.Manager, backendURL string, token string) http.Handler {
	target, err := url.Parse(backendURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf("invalid agent-server URL %q", backendURL), http.StatusInternalServerError)
		})
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// The contract path is forwarded verbatim (it already includes `/v1`);
			// it is set on the request by the wrapping handler below.
			req.Header.Del("Cookie")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("agent-server mirror proxy error path=%s: %v", r.URL.Path, err)
			http.Error(w, "agent-server unavailable", http.StatusBadGateway)
		},
		FlushInterval: -1,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve the contract path from the wildcard and normalise it so `..`
		// segments cannot escape the `/v1` namespace (defence in depth on top of
		// the allow-list below).
		mirrorPath := path.Clean("/" + strings.TrimPrefix(r.PathValue("piPath"), "/"))
		if !mirrorAccessAllowed(pm, r.Method, mirrorPath) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// SSE streams can live much longer than the server write timeout.
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
		r = r.Clone(r.Context())
		r.URL.Path = mirrorPath
		r.URL.RawPath = ""
		proxy.ServeHTTP(w, r)
	})
}

// mirrorAccessAllowed enforces the mirror's narrow allow-list (see
// agentServerMirrorHandler). It receives the already-cleaned contract path
// (leading slash, no `..`).
func mirrorAccessAllowed(pm *project.Manager, method string, mirrorPath string) bool {
	segments := strings.Split(strings.TrimPrefix(mirrorPath, "/"), "/")
	if len(segments) < 2 || segments[0] != "v1" {
		return false
	}

	switch segments[1] {
	case "sessions":
		// Session-independent globals (e.g. /v1/sessions/models). Read-only.
		return method == http.MethodGet
	case "auth", "custom":
		// Runtime-global credential + custom-provider management. Not
		// project-scoped: these configure the single agent-server process appx
		// fronts. Any authenticated dashboard user may manage them (the same
		// access the now-removed /api/agent global proxy granted).
		return true
	case "projects":
		// Only project-scoped *session* traffic is allowed, and only for a
		// project registered with appx. Bare project lifecycle is never exposed.
		if len(segments) < 4 || segments[3] != "sessions" {
			return false
		}
		slug := segments[2]
		if slug == "" {
			return false
		}
		_, err := pm.Store.GetByName(slug)
		return err == nil
	default:
		return false
	}
}
