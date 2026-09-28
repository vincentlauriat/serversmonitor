package entra

import (
	"context"
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
)

const (
	tenant   = "11111111-2222-3333-4444-555555555555"
	clientID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// fakeEntra is an authority with one signing key, a token endpoint that
// checks the PKCE verifier, and whatever claims a test asks it to sign.
type fakeEntra struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	claims   func(nonce string) map[string]any
	lastForm url.Values
	nonce    string // the nonce the authorize URL carried
	verifier string // the challenge the authorize URL carried, to check the verifier against
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newFake(t *testing.T) *fakeEntra {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEntra{key: key, kid: "k1"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/discovery/v2.0/keys"):
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": f.kid, "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
			}}})
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			r.ParseForm()
			f.lastForm = r.PostForm
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if b64(sum[:]) != f.verifier || r.PostForm.Get("client_secret") != "s3cret" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "AADSTS50148: bad verifier\r\nTrace ID: x"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"id_token": f.sign(t, f.claims(f.nonce))})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.claims = func(nonce string) map[string]any { return f.good(nonce) }
	return f
}

func (f *fakeEntra) good(nonce string) map[string]any {
	return map[string]any{
		"iss": f.srv.URL + "/" + tenant + "/v2.0", "aud": clientID, "tid": tenant,
		"exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
		"nonce": nonce, "preferred_username": "Ann@Example.com", "name": "Ann",
	}
}

func (f *fakeEntra) sign(t *testing.T, claims map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": f.kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	signing := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func (f *fakeEntra) provider() *Provider {
	p := NewProvider()
	p.Authority = f.srv.URL
	return p
}

var cfg = Config{Tenant: tenant, ClientID: clientID, ClientSecret: "s3cret"}

// start runs Start and reads back what the authorize URL carried, the way
// Entra would.
func (f *fakeEntra) start(t *testing.T, p *Provider) string {
	t.Helper()
	u, state, err := p.Start(cfg, "https://hub.example/cb")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if !strings.HasPrefix(u, f.srv.URL+"/"+tenant+"/oauth2/v2.0/authorize?") || q.Get("state") != state ||
		q.Get("code_challenge_method") != "S256" || q.Get("client_id") != clientID {
		t.Fatalf("authorize URL = %s", u)
	}
	f.nonce, f.verifier = q.Get("nonce"), q.Get("code_challenge")
	return state
}

func TestAFullSignIn(t *testing.T) {
	f := newFake(t)
	p := f.provider()
	state := f.start(t, p)
	id, err := p.Finish(context.Background(), cfg, "https://hub.example/cb", state, "code")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "ann@example.com" || id.Name != "Ann" {
		t.Fatalf("identity = %+v", id)
	}
	if f.lastForm.Get("redirect_uri") != "https://hub.example/cb" {
		t.Fatalf("token form = %v", f.lastForm)
	}
	// A state is used once: replaying the callback finds nothing.
	if _, err := p.Finish(context.Background(), cfg, "https://hub.example/cb", state, "code"); err == nil {
		t.Fatal("a replayed callback must be refused")
	}
}

func TestAnUnknownStateIsRefused(t *testing.T) {
	p := newFake(t).provider()
	if _, err := p.Finish(context.Background(), cfg, "https://hub.example/cb", "made-up", "code"); err == nil {
		t.Fatal("a state this hub never issued must be refused")
	}
}

func TestEntrasOwnWordsComeThrough(t *testing.T) {
	f := newFake(t)
	p := f.provider()
	state := f.start(t, p)
	f.verifier = "something else" // the verifier will not match
	_, err := p.Finish(context.Background(), cfg, "https://hub.example/cb", state, "code")
	if err == nil || !strings.Contains(err.Error(), "AADSTS50148") || strings.Contains(err.Error(), "Trace ID") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheIDTokenIsCheckedClaimByClaim(t *testing.T) {
	for name, mutate := range map[string]func(c map[string]any){
		"expired":       func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"another nonce": func(c map[string]any) { c["nonce"] = "not ours" },
		"another app":   func(c map[string]any) { c["aud"] = "someone-else" },
		"another tenant": func(c map[string]any) {
			c["tid"] = "99999999-2222-3333-4444-555555555555"
			c["iss"] = strings.Replace(c["iss"].(string), tenant, "99999999-2222-3333-4444-555555555555", 1)
		},
		"a forged issuer": func(c map[string]any) { c["iss"] = "https://evil.example/" + tenant + "/v2.0" },
		"no e-mail":       func(c map[string]any) { delete(c, "preferred_username") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.claims = func(nonce string) map[string]any { c := f.good(nonce); mutate(c); return c }
			p := f.provider()
			state := f.start(t, p)
			if _, err := p.Finish(context.Background(), cfg, "https://hub.example/cb", state, "code"); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
}

func TestASignatureByAnotherKeyIsRefused(t *testing.T) {
	f := newFake(t)
	p := f.provider()
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	token := func() string {
		saved := f.key
		f.key = other
		defer func() { f.key = saved }()
		return f.sign(t, f.good("n"))
	}()
	if _, err := p.Verify(context.Background(), cfg, token, "n"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v", err)
	}
	// And an alg other than RS256 is refused before any key is looked at.
	head := b64([]byte(`{"alg":"none","kid":"k1"}`))
	body := b64([]byte(`{}`))
	if _, err := p.Verify(context.Background(), cfg, head+"."+body+".", "n"); err == nil || !strings.Contains(err.Error(), "RS256") {
		t.Fatalf("alg none: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for c, ok := range map[Config]bool{
		{}:                                     true, // off
		{Tenant: tenant, ClientID: clientID}:   true,
		{Tenant: "common", ClientID: clientID}: false,
		{Tenant: "contoso.com", ClientID: clientID}: false,
		{Tenant: tenant}: false,
	} {
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("%+v: %v, want ok = %v", c, err, ok)
		}
	}
	if (Config{Tenant: tenant, ClientID: clientID}).Enabled() {
		t.Error("no secret means no button")
	}
}
