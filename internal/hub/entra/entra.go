// Package entra signs people in through Microsoft Entra ID with OpenID
// Connect: the authorization code flow with PKCE, a client secret, and an ID
// token whose signature, issuer, audience, tenant, expiry and nonce are all
// checked here rather than trusted. It knows nothing about users or roles:
// it answers "this is the verified e-mail of someone in your tenant", and the
// hub decides whether that e-mail is allowed in.
package entra

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultAuthority is Entra's public cloud. Tests point Provider.Authority at
// a fake.
const DefaultAuthority = "https://login.microsoftonline.com"

// Getter and Setter are the settings table, as for the other configurations
// edited from the browser.
type Getter interface {
	GetSetting(key string) (string, bool, error)
}
type Setter interface {
	SetSetting(key, value string) error
}

// Config is the app registration. The secret is write-only through the API,
// like the SMTP password and the Azure client secret.
type Config struct {
	Tenant       string
	ClientID     string
	ClientSecret string
}

// Enabled says whether the Microsoft button is offered at all.
func (c Config) Enabled() bool {
	return c.Tenant != "" && c.ClientID != "" && c.ClientSecret != ""
}

func get(g Getter, k string) string {
	v, ok, err := g.GetSetting(k)
	if err != nil || !ok {
		return ""
	}
	return v
}

func Load(g Getter) Config {
	return Config{Tenant: get(g, "entra_tenant"), ClientID: get(g, "entra_client_id"), ClientSecret: get(g, "entra_client_secret")}
}

func Save(s Setter, c Config) error {
	for k, v := range map[string]string{"entra_tenant": c.Tenant, "entra_client_id": c.ClientID, "entra_client_secret": c.ClientSecret} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// Validate refuses a tenant that is not a GUID. "common" and "organizations"
// are refused with their own sentence: they accept any tenant, and the tenant
// check in Verify is what keeps a stranger's Microsoft account out.
func (c Config) Validate() error {
	if c.Tenant == "" && c.ClientID == "" {
		return nil // Entra is off
	}
	t := strings.ToLower(c.Tenant)
	if t == "common" || t == "organizations" || t == "consumers" {
		return errors.New("the tenant must be your tenant id or domain; common, organizations and consumers would let other tenants in")
	}
	if c.Tenant == "" || c.ClientID == "" {
		return errors.New("Entra needs both the tenant and the client id")
	}
	if !guid.MatchString(c.Tenant) {
		return errors.New("the tenant must be the directory (tenant) id, a GUID shown on the app registration's overview")
	}
	return nil
}

var guid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Provider runs the flow. One per hub; the pending logins live in memory,
// because a login that spans a hub restart is simply started again.
type Provider struct {
	Authority string
	HTTP      *http.Client
	Now       func() time.Time

	mu      sync.Mutex
	pending map[string]pending
	keys    map[string]*rsa.PublicKey
	keysAt  time.Time
}

type pending struct {
	verifier, nonce string
	at              time.Time
}

// loginTTL bounds how long a person may take on Microsoft's page.
const loginTTL = 10 * time.Minute

func NewProvider() *Provider {
	return &Provider{Authority: DefaultAuthority, HTTP: &http.Client{Timeout: 15 * time.Second}, Now: time.Now,
		pending: map[string]pending{}}
}

func (p *Provider) base(tenant string) string {
	return strings.TrimSuffix(p.Authority, "/") + "/" + url.PathEscape(tenant)
}

func random() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Start returns the URL to send the browser to, and the state the browser
// must bring back in a cookie as well as in the query: the query alone would
// let someone else's browser finish a login it never began.
func (p *Provider) Start(c Config, redirectURI string) (authURL, state string, err error) {
	state, err = random()
	if err != nil {
		return "", "", err
	}
	verifier, err := random()
	if err != nil {
		return "", "", err
	}
	nonce, err := random()
	if err != nil {
		return "", "", err
	}
	now := p.Now()
	p.mu.Lock()
	for k, v := range p.pending {
		if now.Sub(v.at) > loginTTL {
			delete(p.pending, k)
		}
	}
	p.pending[state] = pending{verifier: verifier, nonce: nonce, at: now}
	p.mu.Unlock()

	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {c.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"response_mode":         {"query"},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return p.base(c.Tenant) + "/oauth2/v2.0/authorize?" + q.Encode(), state, nil
}

// Identity is what a verified ID token says about the person.
type Identity struct {
	Email string
	Name  string
}

// Finish exchanges the code and verifies the ID token. A state is used once:
// it is removed before anything else, so a replayed callback finds nothing.
func (p *Provider) Finish(ctx context.Context, c Config, redirectURI, state, code string) (Identity, error) {
	p.mu.Lock()
	pd, ok := p.pending[state]
	delete(p.pending, state)
	p.mu.Unlock()
	if !ok || p.Now().Sub(pd.at) > loginTTL {
		return Identity{}, errors.New("this sign-in was not started here, or took too long; start again")
	}
	form := url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {pd.verifier},
		"scope":         {"openid email profile"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base(c.Tenant)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("entra token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tok struct {
		IDToken     string `json:"id_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.Unmarshal(body, &tok)
	if resp.StatusCode != http.StatusOK || tok.IDToken == "" {
		// Entra's own words: AADSTS codes are what a person can search for.
		msg := tok.Description
		if msg == "" {
			msg = resp.Status
		}
		return Identity{}, fmt.Errorf("entra refused the code: %s", firstLine(msg))
	}
	return p.Verify(ctx, c, tok.IDToken, pd.nonce)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

type claims struct {
	Iss               string          `json:"iss"`
	Aud               json.RawMessage `json:"aud"`
	Exp               float64         `json:"exp"`
	Nbf               float64         `json:"nbf"`
	Nonce             string          `json:"nonce"`
	Tid               string          `json:"tid"`
	Email             string          `json:"email"`
	PreferredUsername string          `json:"preferred_username"`
	Name              string          `json:"name"`
}

// Verify checks an ID token the way the OIDC spec asks, with nothing taken on
// trust: an RS256 signature by a key from the tenant's own JWKS, the issuer
// of that tenant, this app as the audience, the time window, the nonce this
// login was started with, and the tenant id against the issuer's.
func (p *Provider) Verify(ctx context.Context, c Config, idToken, nonce string) (Identity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("the ID token is not a JWT")
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &head); err != nil {
		return Identity{}, err
	}
	if head.Alg != "RS256" {
		return Identity{}, fmt.Errorf("the ID token is signed with %q, only RS256 is accepted", head.Alg)
	}
	key, err := p.key(ctx, c.Tenant, head.Kid)
	if err != nil {
		return Identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, errors.New("the ID token signature is not base64url")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Identity{}, errors.New("the ID token signature does not verify")
	}
	var cl claims
	if err := decodeSegment(parts[1], &cl); err != nil {
		return Identity{}, err
	}
	now := float64(p.Now().Unix())
	// Five minutes of leeway for the clocks, the usual allowance.
	switch {
	case cl.Exp == 0 || now > cl.Exp+300:
		return Identity{}, errors.New("the ID token has expired")
	case cl.Nbf != 0 && now+300 < cl.Nbf:
		return Identity{}, errors.New("the ID token is not valid yet")
	case cl.Nonce == "" || cl.Nonce != nonce:
		return Identity{}, errors.New("the ID token was not issued for this sign-in")
	case !audienceIs(cl.Aud, c.ClientID):
		return Identity{}, errors.New("the ID token was issued for another application")
	case cl.Tid == "" || cl.Iss != strings.TrimSuffix(p.Authority, "/")+"/"+cl.Tid+"/v2.0":
		return Identity{}, errors.New("the ID token issuer is not an Entra tenant")
	case !tenantMatches(c.Tenant, cl.Tid, cl.Iss):
		return Identity{}, errors.New("the ID token comes from another tenant")
	}
	email := cl.Email
	if email == "" {
		email = cl.PreferredUsername
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return Identity{}, errors.New("the ID token carries no e-mail address")
	}
	return Identity{Email: email, Name: cl.Name}, nil
}

// tenantMatches compares the token's tid with the configured tenant id. It
// has to be the GUID: Entra signs every tenant's tokens with the same keys,
// so the signature alone says nothing about which tenant issued one, and a
// domain name cannot be compared with tid.
func tenantMatches(configured, tid, _ string) bool {
	return strings.EqualFold(configured, tid)
}

func audienceIs(raw json.RawMessage, clientID string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == clientID
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == clientID {
				return true
			}
		}
	}
	return false
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("the ID token is not base64url")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("the ID token is not JSON")
	}
	return nil
}

// key finds a signing key by id in the tenant's JWKS, cached for an hour and
// re-read at once for an unknown kid, which is how Entra's key rotation shows.
func (p *Provider) key(ctx context.Context, tenant, kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	k, ok := p.keys[kid]
	fresh := p.Now().Sub(p.keysAt) < time.Hour
	p.mu.Unlock()
	if ok && fresh {
		return k, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base(tenant)+"/discovery/v2.0/keys", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("entra signing keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("entra signing keys: %s", resp.Status)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("entra signing keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jk := range set.Keys {
		if jk.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(jk.N)
		e, err2 := base64.RawURLEncoding.DecodeString(jk.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[jk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	p.mu.Lock()
	p.keys, p.keysAt = keys, p.Now()
	p.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("the ID token is signed with a key this tenant does not publish")
}
