package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/auth"
	"github.com/vincentlauriat/serversmonitor/internal/hub/entra"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

const sessionCookie = "sm_session"

// AgentHandler is the ingest side seen from here: a handler plus the two
// controls the API needs.
type AgentHandler interface {
	http.Handler
	Reconfigure(intervalSec int)
	Connected() []int64
}

type Deps struct {
	Store         *store.Store
	Agents        AgentHandler
	Bus           *Broadcaster
	Notify        Notifier
	Azure         Azurer
	Static        fs.FS
	InstallScript []byte
	Now           func() time.Time
	Version       string
	Log           *slog.Logger
	SessionTTL    time.Duration
	Secure        bool // set the Secure flag on the cookie (behind TLS)
	// Entra runs the Microsoft sign-in. Nil builds a default one.
	Entra *entra.Provider
}

type server struct {
	Deps
	limiter *auth.Limiter
}

func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.SessionTTL == 0 {
		d.SessionTTL = 30 * 24 * time.Hour
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Entra == nil {
		d.Entra = entra.NewProvider()
	}
	s := &server{Deps: d, limiter: auth.NewLimiter(5, time.Minute)}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/me", s.handleMe)
	mux.HandleFunc("POST /api/v1/setup", s.handleSetup)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.Handle("POST /api/v1/logout", s.auth(s.handleLogout))
	mux.Handle("PUT /api/v1/password", s.auth(s.handlePassword))

	mux.HandleFunc("GET /api/v1/auth/entra/start", s.handleEntraStart)
	mux.HandleFunc("GET /api/v1/auth/entra/callback", s.handleEntraCallback)
	mux.Handle("GET /api/v1/auth/entra/settings", s.auth(s.handleGetEntraSettings))
	mux.Handle("PUT /api/v1/auth/entra/settings", s.auth(s.handlePutEntraSettings))
	mux.Handle("GET /api/v1/users", s.auth(s.handleListUsers))
	mux.Handle("POST /api/v1/users", s.auth(s.handleAddUser))
	mux.Handle("PUT /api/v1/users/{id}", s.auth(s.handleSetUserRole))
	mux.Handle("DELETE /api/v1/users/{id}", s.auth(s.handleDeleteUser))

	mux.Handle("GET /api/v1/hosts", s.auth(s.handleListHosts))
	mux.Handle("POST /api/v1/hosts", s.auth(s.handleCreateHost))
	mux.Handle("GET /api/v1/hosts/{id}", s.auth(s.handleGetHost))
	mux.Handle("PATCH /api/v1/hosts/{id}", s.auth(s.handlePatchHost))
	mux.Handle("DELETE /api/v1/hosts/{id}", s.auth(s.handleDeleteHost))
	mux.Handle("POST /api/v1/hosts/{id}/token", s.auth(s.handleRegenerateToken))
	mux.Handle("GET /api/v1/hosts/{id}/series", s.auth(s.handleSeries))
	mux.Handle("GET /api/v1/hosts/{id}/containers", s.auth(s.handleContainers))

	mux.Handle("GET /api/v1/alerts", s.auth(s.handleAlerts))
	mux.Handle("GET /api/v1/alerts/events", s.auth(s.handleAlertEvents))
	mux.Handle("POST /api/v1/alerts/rules", s.auth(s.handleCreateRule))
	mux.Handle("PUT /api/v1/alerts/rules/{id}", s.auth(s.handleUpdateRule))
	mux.Handle("DELETE /api/v1/alerts/rules/{id}", s.auth(s.handleDeleteRule))

	mux.Handle("GET /api/v1/notifications", s.auth(s.handleGetNotifications))
	mux.Handle("PUT /api/v1/notifications", s.auth(s.handlePutNotifications))
	mux.Handle("POST /api/v1/notifications/test", s.auth(s.handleTestNotification))
	mux.Handle("GET /api/v1/notifications/deliveries", s.auth(s.handleDeliveries))

	mux.Handle("GET /api/v1/azure", s.auth(s.handleGetAzure))
	mux.Handle("GET /api/v1/azure/settings", s.auth(s.handleGetAzureSettings))
	mux.Handle("PUT /api/v1/azure/settings", s.auth(s.handlePutAzureSettings))
	mux.Handle("POST /api/v1/azure/test", s.auth(s.handleTestAzure))
	mux.Handle("POST /api/v1/azure/actions", s.auth(s.handleStartAzureAction))
	mux.Handle("GET /api/v1/azure/actions", s.auth(s.handleAzureActions))
	// The provision id travels in the body for the delete, like an action's
	// resource id: nothing here goes in a path segment that a ServeMux
	// wildcard would have to match.
	mux.Handle("POST /api/v1/azure/vms", s.auth(s.handleStartProvision))
	mux.Handle("GET /api/v1/azure/vms", s.auth(s.handleProvisions))
	mux.Handle("GET /api/v1/azure/vms/outbound", s.auth(s.handleOutbound))
	mux.Handle("POST /api/v1/azure/vms/delete", s.auth(s.handleDeleteProvision))

	mux.Handle("GET /api/v1/azure/guardrails", s.auth(s.handleGetGuardrails))
	mux.Handle("GET /api/v1/azure/guardrails/settings", s.auth(s.handleGetGuardrailSettings))
	mux.Handle("PUT /api/v1/azure/guardrails/settings", s.auth(s.handlePutGuardrailSettings))
	mux.Handle("GET /api/v1/azure/schedules", s.auth(s.handleGetSchedules))
	// The resource id travels in the body for both, like every other Azure
	// route that names an ARM resource — see handleStartAzureAction.
	mux.Handle("PUT /api/v1/azure/schedules", s.auth(s.handlePutSchedule))
	mux.Handle("POST /api/v1/azure/schedules/delete", s.auth(s.handleDeleteSchedule))
	mux.Handle("POST /api/v1/azure/orphans/delete", s.auth(s.handleDeleteOrphan))

	mux.Handle("GET /api/v1/settings", s.auth(s.handleGetSettings))
	mux.Handle("PUT /api/v1/settings", s.auth(s.handlePutSettings))
	mux.Handle("GET /api/v1/events", s.auth(s.handleEvents))

	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.Handle("/agent/ws", d.Agents)
	mux.Handle("/", s.staticHandler())
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// auth wraps a handler so it only runs with a valid session, and a viewer
// only ever reads. The role check lives here, once, rather than in each
// handler: a write route added later is refused to viewers without anybody
// having to remember to say so. Signing out is the one write a viewer keeps.
func (s *server) auth(next func(http.ResponseWriter, *http.Request, store.User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if u.Role != store.RoleAdmin && !readOnlyAllowed(r) {
			writeErr(w, http.StatusForbidden, "read-only account: ask an admin to make this change")
			return
		}
		next(w, r, u)
	})
}

func readOnlyAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return true
	}
	return r.URL.Path == "/api/v1/logout"
}

func (s *server) currentUser(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.User{}, false
	}
	u, err := s.Store.SessionUser(c.Value, s.Now())
	if err != nil {
		return store.User{}, false
	}
	return u, true
}

func (s *server) setSession(w http.ResponseWriter, userID int64) error {
	tok, err := s.Store.CreateSession(userID, s.Now().Add(s.SessionTTL))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: s.Secure, SameSite: http.SameSiteStrictMode, MaxAge: int(s.SessionTTL / time.Second)})
	return nil
}

func notFoundOr500(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}
