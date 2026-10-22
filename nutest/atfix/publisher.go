package atfix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// The writable publisher account (0045): a second did:plc on the same fake PDS with a
// generated P-256 signing key, an in-memory repository and an OAuth authorization
// server that auto-approves (or, with Deny, declines) authorization requests.
const (
	PubDID    = "did:plc:nupublishertestaccount22"
	PubHandle = "pub.test"
	// PubScopes are the scopes the fixture grants (OAuthConfig.Scopes ∩ these).
	authIssuer = "http://pds.test"
)

// Publisher is the writable account's state.
type Publisher struct {
	Key    atcrypto.PrivateKey
	Store  mst.MemStore
	Repo   *atrepo.Repo
	Head   cid.CID
	Writes int
	// Deny makes the authorization interface decline requests.
	Deny bool
	// ExpireAccess makes the next authenticated PDS request fail with an expired token
	// once, exercising refresh.
	ExpireAccess bool
	// Nonce is the current DPoP nonce; it rotates when Rotate is called.
	Nonce string
	// Status is the account's status on the PDS (0054): "" active, else deactivated,
	// takendown, suspended or deleted (the PDS then answers RepoNotFound).
	Status string
	// BreakSince makes getRepo?since= answers lack one MST node (a broken chain), so a
	// client must fall back to a full export.
	BreakSince bool
	// revs maps every committed rev to its commit (since-diffs).
	revs map[string]cid.CID
	// blobs are the account's blobs by CID; Oversized is a CID the PDS serves as
	// MaxWholeObject+1 zero bytes.
	blobs     map[cid.CID][]byte
	Oversized cid.CID

	pending  map[string]*authRequest // request_uri → request
	codes    map[string]*authRequest // code → request
	access   map[string]*token       // access token → session
	refresh  map[string]*token       // refresh token → session
	revoked  int
	nonceSeq int
}

type authRequest struct {
	ClientID, RedirectURI, State, Scope, Challenge, LoginHint, JKT string
}

type token struct {
	DID, Scope, JKT string
	Expired         bool
}

func newPublisher() *Publisher {
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		panic(err)
	}
	st := mst.MemStore{}
	p := &Publisher{Key: key, Store: st, Repo: atrepo.New(st, PubDID), Nonce: "nonce-1",
		pending: map[string]*authRequest{}, codes: map[string]*authRequest{}, access: map[string]*token{}, refresh: map[string]*token{}, revs: map[string]cid.CID{},
		blobs: map[cid.CID][]byte{}}
	p.Oversized = cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("oversized"))) // any raw CID; the body is never verified
	p.Head, err = p.Repo.Commit(key, time.Now())
	if err != nil {
		panic(err)
	}
	p.revs[p.Rev()] = p.Head
	return p
}

// KeyMultibase is the publisher's public signing key in DID-document form.
func (p *Publisher) KeyMultibase() string {
	pub, err := p.Key.PublicKey()
	if err != nil {
		panic(err)
	}
	return pub.Multibase()
}

// RotateKey replaces the signing key (the DID document advertises the new one); later
// commits are signed with it.
func (p *Publisher) RotateKey() {
	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		panic(err)
	}
	p.Key = key
}

// PutBlob stores a blob the PDS serves with getBlob and returns its CID.
func (p *Publisher) PutBlob(data []byte) cid.CID {
	c := cid.New(cid.Raw, cid.Sum(cid.SHA256, data))
	p.blobs[c] = bytes.Clone(data)
	return c
}

// BlobRef is the record form of a blob reference.
func BlobRef(c cid.CID, mime string, size int) map[string]any {
	return map[string]any{"$type": "blob", "ref": map[string]any{"$link": c.String()}, "mimeType": mime, "size": size}
}

// Rotate changes the DPoP nonce, so the next client request must retry with it.
func (p *Publisher) Rotate() {
	p.nonceSeq++
	p.Nonce = fmt.Sprintf("nonce-%d", p.nonceSeq+1)
}

// Revoked counts revocation requests.
func (p *Publisher) Revoked() int { return p.revoked }

// Put writes a record directly (as another client of the account would) and commits.
func (p *Publisher) Put(collection, rkey string, record map[string]any) cid.CID {
	js, _ := json.Marshal(record)
	v, err := drisl.FromJSON(js, drisl.JSONOptions{Atproto: true})
	if err != nil {
		panic(err)
	}
	data, err := drisl.Marshal(v)
	if err != nil {
		panic(err)
	}
	c, err := p.Repo.Put(collection+"/"+rkey, data)
	if err != nil {
		panic(err)
	}
	p.commit()
	return c
}

// Delete removes a record directly and commits.
func (p *Publisher) Delete(collection, rkey string) {
	if _, err := p.Repo.Delete(collection + "/" + rkey); err != nil {
		panic(err)
	}
	p.commit()
}

func (p *Publisher) commit() {
	c, err := p.Repo.Commit(p.Key, time.Now())
	if err != nil {
		panic(err)
	}
	p.Head = c
	p.Writes++
	p.revs[p.Rev()] = c
}

// Rev is the current commit rev.
func (p *Publisher) Rev() string { return p.Repo.Head.Rev }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// dpop checks the DPoP header: a valid proof whose nonce is current. It returns the
// key thumbprint (the JWK's x coordinate suffices for the fixture) and whether the
// caller must retry with the nonce it set.
func (p *Publisher) dpop(w http.ResponseWriter, r *http.Request, resource bool) (jkt string, retry bool) {
	w.Header().Set("DPoP-Nonce", p.Nonce)
	claims, err := atproto.VerifyDPoP(r.Header.Get("DPoP"))
	if err != nil {
		http.Error(w, `{"error":"invalid_dpop_proof"}`, 400)
		return "", true
	}
	htu, _ := claims["htu"].(string)
	htm, _ := claims["htm"].(string)
	if htm != r.Method || !strings.HasSuffix(htu, r.URL.Path) {
		http.Error(w, `{"error":"invalid_dpop_proof","error_description":"htm/htu"}`, 400)
		return "", true
	}
	if n, _ := claims["nonce"].(string); n != p.Nonce {
		if resource {
			w.Header().Set("WWW-Authenticate", `DPoP error="use_dpop_nonce"`)
			http.Error(w, `{"error":"use_dpop_nonce"}`, http.StatusUnauthorized)
		} else {
			http.Error(w, `{"error":"use_dpop_nonce"}`, 400)
		}
		return "", true
	}
	parts := strings.Split(r.Header.Get("DPoP"), ".")
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var hdr struct {
		JWK struct{ X string } `json:"jwk"`
	}
	_ = json.Unmarshal(hb, &hdr)
	return hdr.JWK.X, false
}

// serveOAuth handles the authorization server endpoints on pds.test.
func (f *Fixture) serveOAuth(w http.ResponseWriter, r *http.Request) bool {
	p := f.Pub
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource":
		writeJSON(w, 200, map[string]any{"authorization_servers": []string{authIssuer}})
	case "/.well-known/oauth-authorization-server":
		writeJSON(w, 200, map[string]any{
			"issuer": authIssuer, "authorization_endpoint": authIssuer + "/oauth/authorize",
			"token_endpoint": authIssuer + "/oauth/token", "pushed_authorization_request_endpoint": authIssuer + "/oauth/par",
			"revocation_endpoint":      authIssuer + "/oauth/revoke",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"atproto", "repo:io.ipfs.nu.name", "repo:io.ipfs.nu.node"},
			"dpop_signing_alg_values_supported": []string{"ES256"}, "authorization_response_iss_parameter_supported": true,
			"require_pushed_authorization_requests": true, "client_id_metadata_document_supported": true,
			"token_endpoint_auth_methods_supported": []string{"none", "private_key_jwt"}, "token_endpoint_auth_signing_alg_values_supported": []string{"ES256"},
		})
	case "/oauth/par":
		jkt, retry := p.dpop(w, r, false)
		if retry {
			return true
		}
		_ = r.ParseForm()
		cid := r.Form.Get("client_id")
		if !strings.HasPrefix(cid, "http://localhost") || r.Form.Get("response_type") != "code" || r.Form.Get("code_challenge_method") != "S256" ||
			r.Form.Get("state") == "" || r.Form.Get("code_challenge") == "" || !strings.HasPrefix(r.Form.Get("redirect_uri"), "http://127.0.0.1") {
			http.Error(w, `{"error":"invalid_request"}`, 400)
			return true
		}
		req := &authRequest{ClientID: cid, RedirectURI: r.Form.Get("redirect_uri"), State: r.Form.Get("state"), Scope: r.Form.Get("scope"),
			Challenge: r.Form.Get("code_challenge"), LoginHint: r.Form.Get("login_hint"), JKT: jkt}
		uri := fmt.Sprintf("urn:ietf:params:oauth:request_uri:req-%d", len(p.pending)+1)
		p.pending[uri] = req
		writeJSON(w, 201, map[string]any{"request_uri": uri, "expires_in": 300})
	case "/oauth/authorize":
		req := p.pending[r.URL.Query().Get("request_uri")]
		if req == nil || req.ClientID != r.URL.Query().Get("client_id") {
			http.Error(w, "unknown request", 400)
			return true
		}
		delete(p.pending, r.URL.Query().Get("request_uri"))
		q := url.Values{"state": {req.State}, "iss": {authIssuer}}
		if p.Deny {
			q.Set("error", "access_denied")
			q.Set("error_description", "the user declined")
		} else {
			code := fmt.Sprintf("code-%d", len(p.codes)+1)
			p.codes[code] = req
			q.Set("code", code)
		}
		http.Redirect(w, r, req.RedirectURI+"?"+q.Encode(), http.StatusFound)
	case "/oauth/token":
		jkt, retry := p.dpop(w, r, false)
		if retry {
			return true
		}
		_ = r.ParseForm()
		var tok *token
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			req := p.codes[r.Form.Get("code")]
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if req == nil || req.JKT != jkt || req.ClientID != r.Form.Get("client_id") || req.RedirectURI != r.Form.Get("redirect_uri") ||
				base64.RawURLEncoding.EncodeToString(sum[:]) != req.Challenge {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return true
			}
			delete(p.codes, r.Form.Get("code"))
			tok = &token{DID: PubDID, Scope: req.Scope, JKT: jkt}
		case "refresh_token":
			old := p.refresh[r.Form.Get("refresh_token")]
			if old == nil || old.JKT != jkt {
				http.Error(w, `{"error":"invalid_grant","error_description":"refresh token unknown or used"}`, 400)
				return true
			}
			delete(p.refresh, r.Form.Get("refresh_token"))
			tok = &token{DID: old.DID, Scope: old.Scope, JKT: jkt}
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
			return true
		}
		at, rt := fmt.Sprintf("at-%d", len(p.access)+1), fmt.Sprintf("rt-%d", len(p.access)+1)
		p.access[at], p.refresh[rt] = tok, tok
		writeJSON(w, 200, map[string]any{"access_token": at, "refresh_token": rt, "token_type": "DPoP", "scope": tok.Scope, "sub": tok.DID, "expires_in": 300})
	case "/oauth/revoke":
		_ = r.ParseForm()
		if _, ok := p.refresh[r.Form.Get("token")]; ok {
			delete(p.refresh, r.Form.Get("token"))
			p.revoked++
		}
		writeJSON(w, 200, map[string]any{})
	default:
		return false
	}
	return true
}

// authed checks the DPoP-bound access token of a resource request; it returns the token
// or writes the error.
func (p *Publisher) authed(w http.ResponseWriter, r *http.Request) *token {
	jkt, retry := p.dpop(w, r, true)
	if retry {
		return nil
	}
	at := strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP ")
	tok := p.access[at]
	if tok == nil || tok.JKT != jkt || tok.Expired {
		w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token"`)
		http.Error(w, `{"error":"InvalidToken"}`, http.StatusUnauthorized)
		return nil
	}
	if p.ExpireAccess {
		p.ExpireAccess, tok.Expired = false, true
		w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token", error_description="expired"`)
		http.Error(w, `{"error":"ExpiredToken"}`, http.StatusUnauthorized)
		return nil
	}
	return tok
}

// servePub handles XRPC requests for the publisher account.
func (f *Fixture) servePub(w http.ResponseWriter, r *http.Request, method string) {
	p := f.Pub
	q := r.URL.Query()
	switch method {
	case "com.atproto.repo.putRecord", "com.atproto.repo.deleteRecord":
		tok := p.authed(w, r)
		if tok == nil {
			return
		}
		var body struct {
			Repo, Collection, RKey string
			Record                 json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Repo != PubDID {
			http.Error(w, `{"error":"InvalidRequest"}`, 400)
			return
		}
		if !strings.Contains(tok.Scope, "repo:"+body.Collection) {
			http.Error(w, `{"error":"InvalidToken","message":"scope does not cover `+body.Collection+`"}`, http.StatusForbidden)
			return
		}
		path := body.Collection + "/" + body.RKey
		var rc cid.CID
		if method == "com.atproto.repo.putRecord" {
			v, err := drisl.FromJSON(body.Record, drisl.JSONOptions{Atproto: true})
			if err != nil {
				http.Error(w, `{"error":"InvalidRecord"}`, 400)
				return
			}
			data, _ := drisl.Marshal(v)
			if rc, err = p.Repo.Put(path, data); err != nil {
				http.Error(w, `{"error":"InvalidRecord","message":"`+err.Error()+`"}`, 400)
				return
			}
		} else if _, err := p.Repo.Delete(path); err != nil {
			http.Error(w, `{"error":"InvalidRequest"}`, 400)
			return
		}
		p.commit()
		out := map[string]any{"commit": map[string]any{"cid": p.Head.String(), "rev": p.Rev()}}
		if rc.Defined() {
			out["uri"], out["cid"] = "at://"+PubDID+"/"+path, rc.String()
		}
		writeJSON(w, 200, out)
	case "com.atproto.repo.listRecords":
		var recs []map[string]any
		_ = p.Repo.Tree.WalkPrefix(q.Get("collection")+"/", func(k string, c cid.CID) (bool, error) {
			data, _ := p.Store.Get(c)
			v, _ := drisl.Decode(data)
			js, _ := drisl.ToJSON(v)
			recs = append(recs, map[string]any{"uri": "at://" + PubDID + "/" + k, "cid": c.String(), "value": json.RawMessage(js)})
			return true, nil
		})
		if recs == nil {
			recs = []map[string]any{}
		}
		writeJSON(w, 200, map[string]any{"records": recs})
	case "com.atproto.sync.getRecord":
		blocks, err := atrepo.Prove(p.Store, p.Head, q.Get("collection")+"/"+q.Get("rkey"))
		if err != nil {
			http.Error(w, `{"error":"RecordNotFound"}`, 400)
			return
		}
		var buf bytes.Buffer
		_ = atrepo.WriteCAR(&buf, p.Head, blocks)
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(buf.Bytes())
	case "com.atproto.sync.getBlob":
		c, err := cid.Parse(q.Get("cid"))
		if err == nil && c == p.Oversized {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(cid.MaxWholeObject+1))
			_, _ = w.Write(make([]byte, cid.MaxWholeObject+1))
			return
		}
		data, ok := p.blobs[c]
		if err != nil || !ok {
			http.Error(w, `{"error":"BlobNotFound"}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	case "com.atproto.sync.getRepoStatus":
		if p.Status == "deleted" {
			http.Error(w, `{"error":"RepoNotFound"}`, 400)
			return
		}
		st := map[string]any{"did": PubDID, "active": p.Status == "", "rev": p.Rev()}
		if p.Status != "" {
			st["status"] = p.Status
		}
		writeJSON(w, 200, st)
	case "com.atproto.sync.getRepo":
		switch p.Status {
		case "":
		case "deleted":
			http.Error(w, `{"error":"RepoNotFound"}`, 400)
			return
		default:
			http.Error(w, `{"error":"RepoDeactivated"}`, 400)
			return
		}
		var buf bytes.Buffer
		if since := q.Get("since"); since != "" {
			prev, ok := p.revs[since]
			if !ok {
				http.Error(w, `{"error":"InvalidRequest","message":"unknown since rev"}`, 400)
				return
			}
			pr, _ := atrepo.Open(p.Store, prev)
			blocks, _, err := atrepo.Slice(p.Store, pr.Head.Data, p.Head)
			if err != nil {
				http.Error(w, `{"error":"InternalError"}`, 500)
				return
			}
			if p.BreakSince && len(blocks) > 1 {
				blocks = append(blocks[:1], blocks[2:]...) // drop the first MST node
			}
			_ = atrepo.WriteCAR(&buf, p.Head, blocks)
		} else {
			_ = atrepo.Export(&buf, p.Store, p.Head)
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		_, _ = w.Write(buf.Bytes())
	default:
		http.NotFound(w, r)
	}
}

// Browser is an HTTP client that reaches fixture hosts (as a user's browser would) and
// follows redirects, including to the login's loopback callback.
func (f *Fixture) Browser() *http.Client {
	addr := strings.TrimPrefix(f.Srv.URL, "http://")
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(a); strings.HasSuffix(host, ".test") {
			a = addr
		}
		return (&net.Dialer{}).DialContext(ctx, network, a)
	}}, Timeout: 10 * time.Second}
}
