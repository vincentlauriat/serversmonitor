package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/vincentlauriat/serversmonitor/internal/hub/auth"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	if u, ok := s.currentUser(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"email": u.Email, "version": s.Version})
		return
	}
	n, err := s.Store.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"setup_required": n == 0})
}

func (s *server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n > 0 {
		writeErr(w, http.StatusForbidden, "already set up")
		return
	}
	var c credentials
	if err := readJSON(w, r, &c); err != nil || !strings.Contains(c.Email, "@") || len(c.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "email and a password of at least 8 characters are required")
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	u, err := s.Store.CreateUser(c.Email, hash, s.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.setSession(w, u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"email": u.Email})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	key := clientKey(r)
	if !s.limiter.Allowed(key, s.Now()) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, wait a minute")
		return
	}
	var c credentials
	if err := readJSON(w, r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	u, err := s.Store.UserByEmail(c.Email)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Verify even when the user is unknown, so timing does not reveal which
	// e-mail addresses exist.
	hash := u.PasswordHash
	if hash == "" {
		hash, _ = auth.HashPassword("placeholder")
	}
	if err != nil || !auth.VerifyPassword(hash, c.Password) {
		s.limiter.Fail(key, s.Now())
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	s.limiter.Reset(key)
	if err := s.setSession(w, u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request, _ store.User) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handlePassword(w http.ResponseWriter, r *http.Request, u store.User) {
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(w, r, &body); err != nil || len(body.New) < 8 {
		writeErr(w, http.StatusBadRequest, "new password must have at least 8 characters")
		return
	}
	if !auth.VerifyPassword(u.PasswordHash, body.Current) {
		writeErr(w, http.StatusForbidden, "current password is wrong")
		return
	}
	hash, err := auth.HashPassword(body.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.Store.UpdatePassword(u.ID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
