package atproto

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/httpsrc"
)

// OAuth login under the atproto OAuth profile (docs/names.md D4, Mode A): a public
// client with PAR, PKCE S256 and DPoP (ES256), every HTTPS step under httpsrc's SSRF
// policy, sessions bound to the DID that was resolved before the flow started. Nu asks
// only for its own record collections (OAuthConfig.Scopes).
//
// This is Nu's own small client rather than indigo's: indigo's resolver and identity
// directory use their own HTTP client and TLS-only URLs, which would bypass httpsrc and
// the test fixtures, and pull in metrics dependencies.

// DefaultScopes are the scopes Nu requests: the atproto profile plus write access to its
// two record collections. The io.ipfs.nu.authPublish permission set replaces them once
// its lexicon is published under nu.ipfs.io.
var DefaultScopes = []string{"atproto", "repo:io.ipfs.nu.name", "repo:io.ipfs.nu.node"}

// CallbackPath is the path of the loopback redirect URI.
const CallbackPath = "/callback"

// OAuthConfig configures the client identity used for login.
type OAuthConfig struct {
	// ClientID is the client metadata URL; "" uses the localhost development client
	// (LocalhostClientID), which authorization servers accept without a hosted document
	// and which binds loopback redirect URIs with any port.
	ClientID string
	// Scopes are the requested scopes (DefaultScopes when empty).
	Scopes []string
}

// LocalhostClientID is the `http://localhost` client identity for the given scopes with
// the loopback callback path.
func LocalhostClientID(scopes []string) string {
	q := url.Values{"redirect_uri": {"http://127.0.0.1" + CallbackPath}, "scope": {strings.Join(scopes, " ")}}
	return "http://localhost?" + q.Encode()
}

func (o OAuthConfig) withDefaults() OAuthConfig {
	if len(o.Scopes) == 0 {
		o.Scopes = DefaultScopes
	}
	if o.ClientID == "" {
		o.ClientID = LocalhostClientID(o.Scopes)
	}
	return o
}

// Session is a logged-in account: the tokens and DPoP key of one OAuth session. It is
// stored under $NU_PATH/auth (0600) and never leaves the machine.
type Session struct {
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	// PDS is the resource server origin; Issuer the authorization server origin.
	PDS                string   `json:"pds"`
	Issuer             string   `json:"issuer"`
	TokenEndpoint      string   `json:"token_endpoint"`
	RevocationEndpoint string   `json:"revocation_endpoint,omitempty"`
	ClientID           string   `json:"client_id"`
	Scopes             []string `json:"scopes"`
	AccessToken        string   `json:"access_token"`
	RefreshToken       string   `json:"refresh_token"`
	// ASNonce and PDSNonce are the current DPoP nonces of the two servers.
	ASNonce  string `json:"as_nonce,omitempty"`
	PDSNonce string `json:"pds_nonce,omitempty"`
	// DPoPKey is the session's P-256 private key (base64url PKCS#8).
	DPoPKey string    `json:"dpop_key"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	key *ecdsa.PrivateKey
}

// HasScope reports whether the session was granted scope s.
func (s *Session) HasScope(scope string) bool {
	for _, x := range s.Scopes {
		if x == scope {
			return true
		}
	}
	return false
}

func (s *Session) privateKey() (*ecdsa.PrivateKey, error) {
	if s.key != nil {
		return s.key, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s.DPoPKey)
	if err != nil {
		return nil, fmt.Errorf("session key: %v", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("session key: %v", err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		return nil, errors.New("session key is not P-256")
	}
	s.key = ek
	return ek, nil
}

// LoginFlow is a started login: the user visits AuthorizeURL in a browser; the
// authorization server redirects to RedirectURI with code, state and iss, which
// FinishLogin exchanges for a Session.
type LoginFlow struct {
	DID, Handle, PDS string
	AuthorizeURL     string
	RedirectURI      string
	Scopes           []string

	issuer, tokenEndpoint, revocationEndpoint string
	clientID, state, verifier, requestURI     string
	asNonce                                   string
	key                                       *ecdsa.PrivateKey
}

// authServerMetadata is the subset of RFC 8414 metadata Nu validates.
type authServerMetadata struct {
	Issuer                           string   `json:"issuer"`
	AuthorizationEndpoint            string   `json:"authorization_endpoint"`
	TokenEndpoint                    string   `json:"token_endpoint"`
	PAREndpoint                      string   `json:"pushed_authorization_request_endpoint"`
	RevocationEndpoint               string   `json:"revocation_endpoint"`
	ResponseTypes                    []string `json:"response_types_supported"`
	GrantTypes                       []string `json:"grant_types_supported"`
	CodeChallengeMethods             []string `json:"code_challenge_methods_supported"`
	Scopes                           []string `json:"scopes_supported"`
	DPoPAlgs                         []string `json:"dpop_signing_alg_values_supported"`
	ISSParameter                     bool     `json:"authorization_response_iss_parameter_supported"`
	RequirePAR                       bool     `json:"require_pushed_authorization_requests"`
	ClientIDMetadataDocumentSupport  bool     `json:"client_id_metadata_document_supported"`
	TokenEndpointAuthMethods         []string `json:"token_endpoint_auth_methods_supported"`
	TokenEndpointAuthSigningAlgs     []string `json:"token_endpoint_auth_signing_alg_values_supported"`
	RequireRequestURIRegistrationPtr *bool    `json:"require_request_uri_registration"`
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// origin returns scheme://host of an origin URL, rejecting paths, credentials, query,
// fragments and default ports (the atproto OAuth server metadata rules).
func (c *Client) origin(raw, what string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		(u.Scheme != "https" && !(c.cfg.HTTP.AllowHTTP && u.Scheme == "http")) || u.Port() == "443" {
		return "", fmt.Errorf("%s %q is not an https origin", what, raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// endpoint checks an endpoint URL from server metadata before it is ever requested.
func (c *Client) endpoint(raw, what string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "https" && !(c.cfg.HTTP.AllowHTTP && u.Scheme == "http")) {
		return fmt.Errorf("%s %q is not an https URL", what, raw)
	}
	return nil
}

func (c *Client) smallOpts() httpsrc.Options {
	return httpsrc.Options{MaxBytes: c.cfg.Limits.DIDDocBytes, Timeout: c.cfg.Limits.Timeout, IdleTimeout: c.cfg.Limits.Timeout}
}

// getJSON fetches a small JSON document (exactly HTTP 200, application/json).
func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(ctx, req, c.smallOpts())
	if err != nil {
		return err
	}
	body, err := httpsrc.ReadAll(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &httpsrc.StatusError{Code: resp.StatusCode}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("%w: content type %q is not application/json", httpsrc.ErrResponse, ct)
	}
	return json.Unmarshal(body, v)
}

// authServer discovers and validates the authorization server of a PDS: the protected
// resource metadata names one server; its metadata must satisfy the atproto profile.
func (c *Client) authServer(ctx context.Context, pds string) (*authServerMetadata, error) {
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := c.getJSON(ctx, pds+"/.well-known/oauth-protected-resource", &prm); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "cannot read the OAuth resource metadata of %s: %v", pds, err).
			WithAction("the PDS must serve /.well-known/oauth-protected-resource; check the account's PDS and your network")
	}
	if len(prm.AuthorizationServers) != 1 {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s names %d authorization servers; the atproto profile requires exactly one", pds, len(prm.AuthorizationServers))
	}
	iss, err := c.origin(prm.AuthorizationServers[0], "authorization server")
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s: %v", pds, err)
	}
	var m authServerMetadata
	if err := c.getJSON(ctx, iss+"/.well-known/oauth-authorization-server", &m); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "cannot read the OAuth server metadata of %s: %v", iss, err)
	}
	bad := func(format string, args ...any) error {
		return apiv0.Errorf(apiv0.CodeFailed, "authorization server %s does not follow the atproto OAuth profile: "+format, append([]any{iss}, args...)...).
			WithAction("Nu logs in only through servers that implement the atproto OAuth profile (PAR, PKCE S256, DPoP ES256)")
	}
	if m.Issuer != iss {
		return nil, bad("issuer %q is not its origin", m.Issuer)
	}
	for what, ep := range map[string]string{"authorization_endpoint": m.AuthorizationEndpoint, "token_endpoint": m.TokenEndpoint, "pushed_authorization_request_endpoint": m.PAREndpoint} {
		if err := c.endpoint(ep, what); err != nil {
			return nil, bad("%v", err)
		}
	}
	if m.RevocationEndpoint != "" {
		if err := c.endpoint(m.RevocationEndpoint, "revocation_endpoint"); err != nil {
			return nil, bad("%v", err)
		}
	}
	switch {
	case !has(m.ResponseTypes, "code"):
		return nil, bad("no code response type")
	case !has(m.GrantTypes, "authorization_code") || !has(m.GrantTypes, "refresh_token"):
		return nil, bad("authorization_code and refresh_token grants are required")
	case !has(m.CodeChallengeMethods, "S256"):
		return nil, bad("PKCE S256 is required")
	case !has(m.Scopes, "atproto"):
		return nil, bad("the atproto scope is not supported")
	case !has(m.DPoPAlgs, "ES256"):
		return nil, bad("DPoP ES256 is required")
	case !has(m.TokenEndpointAuthMethods, "none"):
		return nil, bad("public clients (token_endpoint_auth_method none) are not supported")
	case !m.ISSParameter, !m.RequirePAR, !m.ClientIDMetadataDocumentSupport:
		return nil, bad("iss parameter, PAR and client ID metadata documents are required")
	case m.RequireRequestURIRegistrationPtr != nil && !*m.RequireRequestURIRegistrationPtr:
		return nil, bad("require_request_uri_registration must not be false")
	}
	return &m, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// StartLogin resolves the account (fresh, not from the identity cache), discovers and
// validates its authorization server and pushes the authorization request. redirectURI
// is the loopback callback the caller serves (http://127.0.0.1:<port>/callback).
func (c *Client) StartLogin(ctx context.Context, identifier, redirectURI string) (*LoginFlow, error) {
	o := c.cfg.OAuth.withDefaults()
	if strings.HasPrefix(identifier, "did:") {
		c.Forget(identifier)
	} else if strings.HasPrefix(identifier, "https://") || strings.HasPrefix(identifier, "http://") {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "log in with a handle or DID, not a server URL")
	}
	id, err := c.Resolve(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if c.cfg.Now().Sub(id.Resolved) > time.Minute {
		c.Forget(id.DID)
		if id, err = c.Resolve(ctx, identifier); err != nil {
			return nil, err
		}
	}
	m, err := c.authServer(ctx, id.PDS)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	f := &LoginFlow{DID: id.DID, Handle: id.Handle, PDS: id.PDS, RedirectURI: redirectURI, Scopes: o.Scopes,
		issuer: m.Issuer, tokenEndpoint: m.TokenEndpoint, revocationEndpoint: m.RevocationEndpoint,
		clientID: o.ClientID, state: randomToken(16), verifier: randomToken(48), key: key}
	sum := sha256.Sum256([]byte(f.verifier))
	form := url.Values{
		"client_id": {o.ClientID}, "response_type": {"code"}, "redirect_uri": {redirectURI},
		"scope": {strings.Join(o.Scopes, " ")}, "state": {f.state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"login_hint": {identifier},
	}
	var par struct {
		RequestURI string `json:"request_uri"`
	}
	nonce, err := c.authPost(ctx, key, "", &f.asNonce, m.PAREndpoint, form, &par)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "pushed authorization request to %s failed: %v", m.Issuer, err)
	}
	f.asNonce = nonce
	if par.RequestURI == "" {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s returned no request_uri", m.Issuer)
	}
	f.requestURI = par.RequestURI
	f.AuthorizeURL = m.AuthorizationEndpoint + "?" + url.Values{"client_id": {o.ClientID}, "request_uri": {par.RequestURI}}.Encode()
	return f, nil
}

// tokenResponse is an authorization server token response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Sub          string `json:"sub"`
	ExpiresIn    int64  `json:"expires_in"`
}

// FinishLogin completes the flow with the callback's query parameters (state, iss,
// code or error) and returns the session, after checking that the server issued it for
// the DID the flow was bound to and granted the atproto scope.
func (c *Client) FinishLogin(ctx context.Context, f *LoginFlow, q url.Values) (*Session, error) {
	if q.Get("state") != f.state {
		return nil, apiv0.Errorf(apiv0.CodeForbidden, "the authorization response does not belong to this login (state mismatch)")
	}
	if e := q.Get("error"); e != "" {
		return nil, apiv0.Errorf(apiv0.CodeForbidden, "the authorization server declined: %s %s", e, q.Get("error_description")).
			WithAction("approve the request in the browser, or check the account and scopes")
	}
	if q.Get("iss") != f.issuer {
		return nil, apiv0.Errorf(apiv0.CodeForbidden, "the authorization response came from %q, not the account's server %s", q.Get("iss"), f.issuer)
	}
	code := q.Get("code")
	if code == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "the authorization response has no code")
	}
	form := url.Values{"client_id": {f.clientID}, "grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {f.RedirectURI}, "code_verifier": {f.verifier}}
	var tr tokenResponse
	nonce, err := c.authPost(ctx, f.key, "", &f.asNonce, f.tokenEndpoint, form, &tr)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "token request to %s failed: %v", f.issuer, err)
	}
	if tr.Sub != f.DID {
		return nil, apiv0.Errorf(apiv0.CodeForbidden, "%s issued a session for %q, not for %s", f.issuer, tr.Sub, f.DID).
			WithAction("log in with the account whose handle or DID you gave")
	}
	scopes := strings.Fields(tr.Scope)
	if !has(scopes, "atproto") || !strings.EqualFold(tr.TokenType, "DPoP") || tr.AccessToken == "" || tr.RefreshToken == "" {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s returned an unusable token response (scope %q, type %q)", f.issuer, tr.Scope, tr.TokenType)
	}
	der, err := x509.MarshalPKCS8PrivateKey(f.key)
	if err != nil {
		return nil, err
	}
	now := c.cfg.Now()
	return &Session{DID: f.DID, Handle: f.Handle, PDS: f.PDS, Issuer: f.issuer, TokenEndpoint: f.tokenEndpoint,
		RevocationEndpoint: f.revocationEndpoint, ClientID: f.clientID, Scopes: scopes, AccessToken: tr.AccessToken,
		RefreshToken: tr.RefreshToken, ASNonce: nonce, PDSNonce: nonce, DPoPKey: base64.RawURLEncoding.EncodeToString(der),
		Created: now, Updated: now, key: f.key}, nil
}

// authPost posts a form to an authorization server endpoint with a DPoP proof,
// retrying once when the server demands a fresh nonce. It returns the latest nonce.
func (c *Client) authPost(ctx context.Context, key *ecdsa.PrivateKey, accessToken string, nonce *string, endpoint string, form url.Values, out any) (string, error) {
	cur := *nonce
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := dpopProof(key, http.MethodPost, endpoint, cur, accessToken, c.cfg.Now())
		if err != nil {
			return cur, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return cur, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("DPoP", proof)
		resp, err := c.http.Do(ctx, req, c.smallOpts())
		if err != nil {
			return cur, err
		}
		body, err := httpsrc.ReadAll(resp)
		if err != nil {
			return cur, err
		}
		if n := resp.Header.Get("DPoP-Nonce"); n != "" {
			cur = n
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			if out != nil {
				if err := json.Unmarshal(body, out); err != nil {
					return cur, fmt.Errorf("bad JSON response: %v", err)
				}
			}
			return cur, nil
		}
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if resp.StatusCode == http.StatusBadRequest && e.Error == "use_dpop_nonce" && attempt == 0 && resp.Header.Get("DPoP-Nonce") != "" {
			continue
		}
		msg := e.Error
		if e.Description != "" {
			msg += ": " + e.Description
		}
		if msg == "" {
			msg = strings.TrimSpace(string(body))
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		return cur, fmt.Errorf("HTTP %d %s", resp.StatusCode, msg)
	}
	return cur, errors.New("the server kept rejecting the DPoP nonce")
}

// dpopProof builds a DPoP proof JWT (RFC 9449) for one request.
func dpopProof(key *ecdsa.PrivateKey, method, rawURL, nonce, accessToken string, now time.Time) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.RawQuery, u.Fragment = "", ""
	pub, err := key.PublicKey.Bytes() // 0x04 ‖ X ‖ Y
	if err != nil {
		return "", err
	}
	header := map[string]any{"typ": "dpop+jwt", "alg": "ES256",
		"jwk": map[string]string{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(pub[1:33]), "y": base64.RawURLEncoding.EncodeToString(pub[33:])}}
	claims := map[string]any{"jti": randomToken(16), "htm": method, "htu": u.String(), "iat": now.Unix()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyDPoP checks a DPoP proof's signature against the JWK in its header and returns
// its claims (test servers and the fixture use it; it does not check claims).
func VerifyDPoP(proof string) (map[string]any, error) {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed DPoP proof")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var header struct {
		Typ, Alg string
		JWK      struct{ Kty, Crv, X, Y string } `json:"jwk"`
	}
	if err := json.Unmarshal(hb, &header); err != nil {
		return nil, err
	}
	if header.Typ != "dpop+jwt" || header.Alg != "ES256" || header.JWK.Kty != "EC" || header.JWK.Crv != "P-256" {
		return nil, errors.New("DPoP proof is not ES256 over a P-256 JWK")
	}
	xb, err1 := base64.RawURLEncoding.DecodeString(header.JWK.X)
	yb, err2 := base64.RawURLEncoding.DecodeString(header.JWK.Y)
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(sig) != 64 || len(xb) != 32 || len(yb) != 32 {
		return nil, errors.New("malformed DPoP key or signature")
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, xb...), yb...))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, errors.New("DPoP signature does not verify")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(cb, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// sessionLock serializes token use per DID: refresh tokens are single-use.
func (c *Client) sessionLock(did string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions == nil {
		c.sessions = map[string]*sync.Mutex{}
	}
	m, ok := c.sessions[did]
	if !ok {
		m = &sync.Mutex{}
		c.sessions[did] = m
	}
	return m
}

// Refresh exchanges the refresh token for new tokens (caller holds the session lock).
func (c *Client) refresh(ctx context.Context, s *Session) error {
	key, err := s.privateKey()
	if err != nil {
		return err
	}
	form := url.Values{"client_id": {s.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {s.RefreshToken}}
	var tr tokenResponse
	nonce, err := c.authPost(ctx, key, "", &s.ASNonce, s.TokenEndpoint, form, &tr)
	s.ASNonce = nonce
	if err != nil {
		return apiv0.Errorf(apiv0.CodeForbidden, "the session for %s could not be refreshed: %v", s.DID, err).
			WithAction("log in again")
	}
	if tr.Sub != s.DID || tr.AccessToken == "" || tr.RefreshToken == "" {
		return apiv0.Errorf(apiv0.CodeFailed, "%s returned an unusable refresh response", s.Issuer)
	}
	s.AccessToken, s.RefreshToken = tr.AccessToken, tr.RefreshToken
	if tr.Scope != "" {
		s.Scopes = strings.Fields(tr.Scope)
	}
	s.Updated = c.cfg.Now()
	return nil
}

// Revoke ends the session at the authorization server (best effort: a server without a
// revocation endpoint just forgets nothing; the local copy is deleted by the caller).
func (c *Client) Revoke(ctx context.Context, s *Session) error {
	if s.RevocationEndpoint == "" {
		return nil
	}
	lk := c.sessionLock(s.DID)
	lk.Lock()
	defer lk.Unlock()
	key, err := s.privateKey()
	if err != nil {
		return err
	}
	_, err = c.authPost(ctx, key, "", &s.ASNonce, s.RevocationEndpoint, url.Values{"client_id": {s.ClientID}, "token": {s.RefreshToken}, "token_type_hint": {"refresh_token"}}, nil)
	return err
}

// XRPCError is a PDS error response.
type XRPCError struct {
	Status  int
	Name    string `json:"error"`
	Message string `json:"message"`
}

func (e *XRPCError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Name, e.Message)
	}
	return fmt.Sprintf("HTTP %d %s", e.Status, e.Name)
}

// Call performs an authenticated XRPC call on the session's PDS: a GET with query q, or
// a POST of the JSON body in. The DPoP nonce and tokens are refreshed as the server
// demands; Save, if non-nil, persists the updated session after each change. The
// response body (at most ProofBytes) is returned for 2xx statuses; others are *XRPCError.
func (c *Client) Call(ctx context.Context, s *Session, method, nsid string, q url.Values, in any, save func(*Session) error) ([]byte, error) {
	lk := c.sessionLock(s.DID)
	lk.Lock()
	defer lk.Unlock()
	key, err := s.privateKey()
	if err != nil {
		return nil, err
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	u := xrpc(s.PDS, nsid, q)
	if q == nil {
		u = s.PDS + "/xrpc/" + nsid
	}
	refreshed := false
	for attempt := 0; attempt < 3; attempt++ {
		proof, err := dpopProof(key, method, u, s.PDSNonce, s.AccessToken, c.cfg.Now())
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "DPoP "+s.AccessToken)
		req.Header.Set("DPoP", proof)
		resp, err := c.http.Do(ctx, req, httpsrc.Options{MaxBytes: c.cfg.Limits.ProofBytes, Timeout: c.cfg.Limits.Timeout, IdleTimeout: c.cfg.Limits.Timeout})
		if err != nil {
			return nil, err
		}
		out, err := httpsrc.ReadAll(resp)
		if err != nil {
			return nil, err
		}
		changed := false
		if n := resp.Header.Get("DPoP-Nonce"); n != "" && n != s.PDSNonce {
			s.PDSNonce, changed = n, true
		}
		if resp.StatusCode/100 == 2 {
			if changed && save != nil {
				_ = save(s)
			}
			return out, nil
		}
		www := resp.Header.Get("WWW-Authenticate")
		switch {
		case resp.StatusCode == http.StatusUnauthorized && strings.Contains(www, "use_dpop_nonce") && changed:
			continue
		case resp.StatusCode == http.StatusUnauthorized && !refreshed && (strings.Contains(www, "invalid_token") || strings.Contains(www, "expired")):
			if err := c.refresh(ctx, s); err != nil {
				return nil, err
			}
			refreshed = true
			if save != nil {
				_ = save(s)
			}
			continue
		}
		e := &XRPCError{Status: resp.StatusCode}
		_ = json.Unmarshal(out, e)
		if save != nil && changed {
			_ = save(s)
		}
		return nil, e
	}
	return nil, errors.New("the PDS kept rejecting the request")
}

// WriteResult is the outcome of a record write.
type WriteResult struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
	// Rev and Commit identify the repository commit after the write ("" if omitted).
	Rev    string `json:"rev,omitempty"`
	Commit string `json:"commit,omitempty"`
}

type writeResponse struct {
	URI    string `json:"uri"`
	CID    string `json:"cid"`
	Commit *struct {
		CID string `json:"cid"`
		Rev string `json:"rev"`
	} `json:"commit"`
}

// PutRecord creates or replaces a record in the session's repository.
func (c *Client) PutRecord(ctx context.Context, s *Session, collection, rkey string, record any, save func(*Session) error) (*WriteResult, error) {
	out, err := c.Call(ctx, s, http.MethodPost, "com.atproto.repo.putRecord", nil,
		map[string]any{"repo": s.DID, "collection": collection, "rkey": rkey, "record": record, "validate": false}, save)
	if err != nil {
		return nil, writeError(s, "putRecord", err)
	}
	var w writeResponse
	if err := json.Unmarshal(out, &w); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s returned a bad putRecord response: %v", s.PDS, err)
	}
	res := &WriteResult{URI: w.URI, CID: w.CID}
	if w.Commit != nil {
		res.Rev, res.Commit = w.Commit.Rev, w.Commit.CID
	}
	return res, nil
}

// DeleteRecord removes a record; deleting an absent record is not an error.
func (c *Client) DeleteRecord(ctx context.Context, s *Session, collection, rkey string, save func(*Session) error) (*WriteResult, error) {
	out, err := c.Call(ctx, s, http.MethodPost, "com.atproto.repo.deleteRecord", nil,
		map[string]any{"repo": s.DID, "collection": collection, "rkey": rkey}, save)
	if err != nil {
		return nil, writeError(s, "deleteRecord", err)
	}
	var w writeResponse
	_ = json.Unmarshal(out, &w)
	res := &WriteResult{}
	if w.Commit != nil {
		res.Rev = w.Commit.Rev
	}
	return res, nil
}

func writeError(s *Session, method string, err error) error {
	var xe *XRPCError
	if errors.As(err, &xe) {
		code := apiv0.CodeFailed
		switch {
		case xe.Status == http.StatusUnauthorized || xe.Status == http.StatusForbidden || xe.Name == "InvalidToken" || xe.Name == "ExpiredToken":
			code = apiv0.CodeForbidden
		case xe.Status == http.StatusTooManyRequests:
			code = apiv0.CodeUnavailable
		}
		return apiv0.Errorf(code, "%s on %s for %s: %v", method, s.PDS, s.DID, xe).
			WithAction("if the session was revoked or its scope is insufficient, log in again")
	}
	var ae *apiv0.Error
	if errors.As(err, &ae) {
		return err
	}
	return apiv0.Errorf(apiv0.CodeUnavailable, "%s on %s for %s: %v", method, s.PDS, s.DID, err)
}

// ListedRecord is one entry of a public collection listing (unverified: the listing is
// a hint; each record is verified by GetRecord before it is trusted).
type ListedRecord struct {
	URI   string          `json:"uri"`
	CID   string          `json:"cid"`
	Value json.RawMessage `json:"value"`
}

// ListRecords lists a collection of an account's repository through the public
// com.atproto.repo.listRecords endpoint (at most limit entries over as many pages as
// needed; 0 means 1000).
func (c *Client) ListRecords(ctx context.Context, id *Identity, collection string, limit int) ([]ListedRecord, error) {
	if limit <= 0 {
		limit = 1000
	}
	release, err := c.acquire(ctx, id.PDS)
	if err != nil {
		return nil, err
	}
	defer release()
	var out []ListedRecord
	cursor := ""
	for len(out) < limit {
		q := url.Values{"repo": {id.DID}, "collection": {collection}, "limit": {"100"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		body, err := c.get(ctx, xrpc(id.PDS, "com.atproto.repo.listRecords", q), c.cfg.Limits.ProofBytes, nil)
		if err != nil {
			return nil, pdsError(id, "listRecords", err)
		}
		var page struct {
			Records []ListedRecord `json:"records"`
			Cursor  string         `json:"cursor"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%s returned a bad listRecords response: %v", id.PDS, err)
		}
		out = append(out, page.Records...)
		if page.Cursor == "" || len(page.Records) == 0 {
			break
		}
		cursor = page.Cursor
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
