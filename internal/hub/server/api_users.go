package server

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/entra"
	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// --- users ------------------------------------------------------------------

type userView struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Provider string `json:"provider"`
	// Self lets the page say "you" and keep a person from removing their own
	// way in by accident.
	Self bool `json:"self"`
}

func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request, me store.User) {
	us, err := s.Store.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]userView, 0, len(us))
	for _, u := range us {
		out = append(out, userView{ID: u.ID, Email: u.Email, Role: u.Role, Provider: u.Provider, Self: u.ID == me.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func validRole(role string) bool { return role == store.RoleAdmin || role == store.RoleViewer }

// handleAddUser allows an Entra account in. It is the allow list: nobody from
// the tenant gets in unless an admin added their address here first. Local
// accounts are not added this way; the one made at setup is the way back in.
func (s *server) handleAddUser(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !strings.Contains(in.Email, "@") || strings.ContainsAny(in.Email, " \t") {
		writeErr(w, http.StatusBadRequest, "an e-mail address is required")
		return
	}
	if !validRole(in.Role) {
		writeErr(w, http.StatusBadRequest, "role must be admin or viewer")
		return
	}
	u, err := s.Store.AddUser(in.Email, "", in.Role, store.ProviderEntra, s.Now())
	if errors.Is(err, store.ErrUserExists) {
		writeErr(w, http.StatusConflict, "this address already has an account")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, userView{ID: u.ID, Email: u.Email, Role: u.Role, Provider: u.Provider})
}

func userErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrLastLocalAdmin):
		writeErr(w, http.StatusConflict, "the last local admin stays: it is the way in when Microsoft sign-in is down")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such user")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *server) handleSetUserRole(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := readJSON(w, r, &in); err != nil || !validRole(in.Role) {
		writeErr(w, http.StatusBadRequest, "role must be admin or viewer")
		return
	}
	if err := s.Store.SetUserRole(id, in.Role); err != nil {
		userErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteUser(id); err != nil {
		userErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Entra settings ---------------------------------------------------------

type entraView struct {
	Tenant    string `json:"tenant"`
	ClientID  string `json:"client_id"`
	SecretSet bool   `json:"client_secret_set"`
	// RedirectURI is what goes in the app registration, built from the public
	// URL; empty when that is not set, and then the button cannot work.
	RedirectURI string `json:"redirect_uri"`
}

func (s *server) redirectURI() string {
	pub := notify.LoadConfig(s.Store).Root()
	if pub == "" {
		return ""
	}
	return pub + "/api/v1/auth/entra/callback"
}

func (s *server) handleGetEntraSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	c := entra.Load(s.Store)
	writeJSON(w, http.StatusOK, entraView{Tenant: c.Tenant, ClientID: c.ClientID,
		SecretSet: c.ClientSecret != "", RedirectURI: s.redirectURI()})
}

func (s *server) handlePutEntraSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	var in struct {
		Tenant       string  `json:"tenant"`
		ClientID     string  `json:"client_id"`
		ClientSecret *string `json:"client_secret"` // absent keeps the stored one
	}
	if err := readJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	c := entra.Load(s.Store)
	c.Tenant, c.ClientID = strings.TrimSpace(in.Tenant), strings.TrimSpace(in.ClientID)
	if in.ClientSecret != nil {
		c.ClientSecret = *in.ClientSecret
	}
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := entra.Save(s.Store, c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Entra sign-in ----------------------------------------------------------

const entraStateCookie = "sm_entra_state"

// handleEntraStart sends the browser to Microsoft. The state goes both in the
// URL and in a cookie; the callback wants both to match, so a link crafted
// by someone else cannot finish a sign-in in this browser. The cookie is
// SameSite=Lax, not Strict like the session: the callback arrives from
// login.microsoftonline.com, and a Strict cookie is not sent on that hop.
func (s *server) handleEntraStart(w http.ResponseWriter, r *http.Request) {
	c := entra.Load(s.Store)
	redirect := s.redirectURI()
	if !c.Enabled() || redirect == "" {
		loginError(w, r, "Microsoft sign-in is not configured on this hub")
		return
	}
	u, state, err := s.Entra.Start(c, redirect)
	if err != nil {
		loginError(w, r, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: entraStateCookie, Value: state, Path: "/api/v1/auth/entra/",
		HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *server) handleEntraCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	http.SetCookie(w, &http.Cookie{Name: entraStateCookie, Value: "", Path: "/api/v1/auth/entra/", MaxAge: -1, HttpOnly: true})
	if e := q.Get("error"); e != "" {
		// A person who cancels on Microsoft's page lands here too.
		loginError(w, r, "Microsoft sign-in stopped: "+firstLine(q.Get("error_description"), e))
		return
	}
	cookie, err := r.Cookie(entraStateCookie)
	state := q.Get("state")
	if err != nil || state == "" || cookie.Value != state {
		loginError(w, r, "this sign-in was not started in this browser; start again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	id, err := s.Entra.Finish(ctx, entra.Load(s.Store), s.redirectURI(), state, q.Get("code"))
	if err != nil {
		s.Log.Warn("entra sign-in refused", "err", err)
		loginError(w, r, err.Error())
		return
	}
	u, err := s.Store.UserByEmail(id.Email)
	if err != nil || u.Provider != store.ProviderEntra {
		// Same words whether the address is unknown or belongs to the local
		// account: the page does not confirm which addresses exist.
		s.Log.Warn("entra sign-in for an address not allowed", "email", id.Email)
		loginError(w, r, id.Email+" is not allowed on this hub; ask an admin to add it")
		return
	}
	if err := s.setSession(w, u.ID); err != nil {
		loginError(w, r, err.Error())
		return
	}
	// A page rather than a 302: the session cookie is SameSite=Strict, and a
	// redirect chain that began on Microsoft's site would not carry it to the
	// first page load. A same-site navigation from here does.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Signing in</title>` +
		`<meta http-equiv="refresh" content="0;url=/"><a href="/">Continue</a>`))
}

func firstLine(s, fallback string) string {
	if s == "" {
		return fallback
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// loginError sends the person back to the login page with the reason, which
// the page shows. The reason is escaped by the page as text; it is also kept
// short so a URL never carries a stack of Entra diagnostics.
func loginError(w http.ResponseWriter, r *http.Request, msg string) {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	target := "/login?error=" + url.QueryEscape(msg)
	// Same same-site navigation as a success, for the same reason.
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Sign-in failed</title>` +
		`<meta http-equiv="refresh" content="0;url=` + html.EscapeString(target) + `"><a href="` +
		html.EscapeString(target) + `">Back to sign-in</a>`))
}
