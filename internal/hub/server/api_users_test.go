package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/notify"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

const (
	testTenant = "11111111-2222-3333-4444-555555555555"
	testClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// signInAs gives the rig's client a session for an account, the way a
// successful sign-in would, without going through either login route.
func (r *rig) signInAs(t *testing.T, u store.User) {
	t.Helper()
	tok, err := r.st.CreateSession(u.ID, r.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(r.srv.URL)
	r.client.Jar.SetCookies(base, []*http.Cookie{{Name: sessionCookie, Value: tok, Path: "/"}})
}

func TestAViewerOnlyReads(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	viewer, err := r.st.AddUser("ann@example.com", "", store.RoleViewer, store.ProviderEntra, r.now)
	if err != nil {
		t.Fatal(err)
	}
	r.signInAs(t, viewer)

	for _, path := range []string{"/api/v1/me", "/api/v1/hosts", "/api/v1/alerts", "/api/v1/users", "/api/v1/settings"} {
		if resp, data := r.do(t, "GET", path, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d %s", path, resp.StatusCode, data)
		}
	}
	for _, w := range []struct{ method, path string }{
		{"POST", "/api/v1/hosts"}, {"PUT", "/api/v1/settings"}, {"POST", "/api/v1/alerts/rules"},
		{"POST", "/api/v1/azure/actions"}, {"POST", "/api/v1/azure/vms/delete"}, {"POST", "/api/v1/users"},
		{"PUT", "/api/v1/notifications"}, {"POST", "/api/v1/notifications/test"},
	} {
		resp, data := r.do(t, w.method, w.path, map[string]any{})
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(data), "read-only") {
			t.Errorf("%s %s = %d %s, want 403", w.method, w.path, resp.StatusCode, data)
		}
	}
	_, me := r.do(t, "GET", "/api/v1/me", nil)
	if !strings.Contains(string(me), `"role":"viewer"`) || !strings.Contains(string(me), `"provider":"entra"`) {
		t.Fatalf("me = %s", me)
	}
	// Signing out is the one write a viewer keeps.
	if resp, _ := r.do(t, "POST", "/api/v1/logout", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
}

// An Entra account has no password. Before lot 11 an empty hash was replaced
// by the dummy hash of "placeholder" for timing, so that word would have
// opened it.
func TestAnEntraAccountCannotUseThePasswordRoute(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	r.st.AddUser("ann@example.com", "", store.RoleAdmin, store.ProviderEntra, r.now)
	for _, pw := range []string{"placeholder", ""} {
		if resp, _ := r.do(t, "POST", "/api/v1/login", map[string]string{"email": "ann@example.com", "password": pw}); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("password %q = %d, want 401", pw, resp.StatusCode)
		}
	}
}

func TestUsersAPI(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	resp, data := r.do(t, "POST", "/api/v1/users", map[string]string{"email": " Ann@Example.com ", "role": "viewer"})
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(data), `"email":"ann@example.com"`) {
		t.Fatalf("add = %d %s", resp.StatusCode, data)
	}
	var ann userView
	json.Unmarshal(data, &ann)
	if resp, _ := r.do(t, "POST", "/api/v1/users", map[string]string{"email": "ann@example.com", "role": "viewer"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "POST", "/api/v1/users", map[string]string{"email": "bob@example.com", "role": "owner"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad role = %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "PUT", "/api/v1/users/"+itoa(ann.ID), map[string]string{"role": "admin"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("promote = %d", resp.StatusCode)
	}

	_, data = r.do(t, "GET", "/api/v1/users", nil)
	var list struct{ Users []userView }
	json.Unmarshal(data, &list)
	var self userView
	for _, u := range list.Users {
		if u.Self {
			self = u
		}
	}
	if len(list.Users) != 2 || self.Email != "v@example.com" || self.Provider != "local" {
		t.Fatalf("users = %+v", list.Users)
	}
	resp, data = r.do(t, "DELETE", "/api/v1/users/"+itoa(self.ID), nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), "last local admin") {
		t.Fatalf("delete the last local admin = %d %s", resp.StatusCode, data)
	}
	if resp, _ := r.do(t, "DELETE", "/api/v1/users/"+itoa(ann.ID), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete ann = %d", resp.StatusCode)
	}
}

func TestEntraSettings(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	if resp, data := r.do(t, "PUT", "/api/v1/auth/entra/settings", map[string]string{"tenant": "common", "client_id": testClient}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("common = %d %s", resp.StatusCode, data)
	}
	if resp, data := r.do(t, "PUT", "/api/v1/auth/entra/settings", map[string]string{"tenant": testTenant, "client_id": testClient, "client_secret": "s"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	// Absent secret keeps the stored one; it never comes back.
	r.do(t, "PUT", "/api/v1/auth/entra/settings", map[string]string{"tenant": testTenant, "client_id": testClient})
	_, data := r.do(t, "GET", "/api/v1/auth/entra/settings", nil)
	if !strings.Contains(string(data), `"client_secret_set":true`) || strings.Contains(string(data), `"s"`) {
		t.Fatalf("settings = %s", data)
	}
}

// fakeAuthority is Entra for one sign-in: it remembers the nonce of the
// authorize URL the hub built and signs an ID token carrying it.
type fakeAuthority struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	nonce string
	email string
}

func newAuthority(t *testing.T) *fakeAuthority {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := &fakeAuthority{key: key, email: "ann@example.com"}
	enc := base64.RawURLEncoding.EncodeToString
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/discovery/v2.0/keys"):
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k",
				"n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes())}}})
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k"})
			body, _ := json.Marshal(map[string]any{"iss": a.srv.URL + "/" + testTenant + "/v2.0", "aud": testClient,
				"tid": testTenant, "exp": time.Now().Add(time.Hour).Unix(), "nonce": a.nonce, "preferred_username": a.email})
			signing := enc(head) + "." + enc(body)
			sum := sha256.Sum256([]byte(signing))
			sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
			json.NewEncoder(w).Encode(map[string]string{"id_token": signing + "." + enc(sig)})
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// A full Microsoft sign-in through the hub's two routes: the start sets the
// state cookie and sends the browser away, the callback finishes it.
func TestAnEntraSignIn(t *testing.T) {
	r := newRig(t)
	r.setupAndLogin(t)
	auth := newAuthority(t)
	r.entra.Authority = auth.srv.URL
	notify.SaveConfig(r.st, notify.Config{Public: "https://hub.example"})
	r.do(t, "PUT", "/api/v1/auth/entra/settings", map[string]string{"tenant": testTenant, "client_id": testClient, "client_secret": "s"})
	r.do(t, "POST", "/api/v1/users", map[string]string{"email": "ann@example.com", "role": "viewer"})
	r.do(t, "POST", "/api/v1/logout", nil)

	_, data := r.do(t, "GET", "/api/v1/me", nil)
	if !strings.Contains(string(data), `"entra":true`) {
		t.Fatalf("the login page must offer Microsoft: %s", data)
	}

	noFollow := &http.Client{Jar: r.client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signIn := func(t *testing.T) string {
		resp, err := noFollow.Get(r.srv.URL + "/api/v1/auth/entra/start")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || loc.Query().Get("redirect_uri") != "https://hub.example/api/v1/auth/entra/callback" {
			t.Fatalf("start = %d %s", resp.StatusCode, loc)
		}
		auth.nonce = loc.Query().Get("nonce")
		cb, err := noFollow.Get(r.srv.URL + "/api/v1/auth/entra/callback?code=c&state=" + url.QueryEscape(loc.Query().Get("state")))
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 512)
		n, _ := cb.Body.Read(b)
		cb.Body.Close()
		return string(b[:n])
	}

	page := signIn(t)
	if strings.Contains(page, "error=") {
		t.Fatalf("callback = %s", page)
	}
	_, data = r.do(t, "GET", "/api/v1/me", nil)
	if !strings.Contains(string(data), `"email":"ann@example.com"`) || !strings.Contains(string(data), `"role":"viewer"`) {
		t.Fatalf("after sign-in: %s", data)
	}

	// An address of the tenant that nobody added is refused.
	r.do(t, "POST", "/api/v1/logout", nil)
	auth.email = "stranger@example.com"
	if page := signIn(t); !strings.Contains(page, "not+allowed") {
		t.Fatalf("stranger = %s", page)
	}
	if resp, _ := r.do(t, "GET", "/api/v1/hosts", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("a refused sign-in must not leave a session")
	}

	// The local admin's address signing in through Microsoft is refused too:
	// the local account is only reachable with its password.
	auth.email = "v@example.com"
	if page := signIn(t); !strings.Contains(page, "not+allowed") {
		t.Fatalf("local address through Entra = %s", page)
	}
}

func TestACallbackWithoutTheStateCookieIsRefused(t *testing.T) {
	r := newRig(t)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).
		Get(r.srv.URL + "/api/v1/auth/entra/callback?code=c&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 512)
	n, _ := resp.Body.Read(b)
	resp.Body.Close()
	if !strings.Contains(string(b[:n]), "error=") {
		t.Fatalf("callback = %s", b[:n])
	}
}
