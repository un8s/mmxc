package mmxc

// auth.go implements an optional OIDC SSO "login gate" for MMXC.
//
// The app has no user accounts of its own: this is a binary gate that requires
// a visitor to have authenticated with an OIDC provider (built for Pocket ID,
// but any standards-compliant provider works). Which users are allowed is
// configured on the provider side via per-client user/group assignment.
//
// The flow is the standard Authorization Code flow with PKCE + state + nonce,
// hand-rolled with the standard library to keep the project dependency-light.
//
// ID-token trust model: we accept the ID token's claims WITHOUT verifying its
// JWT signature against the provider's JWKS. This is safe ONLY because the
// token is obtained over a back-channel: a direct server-to-server TLS POST
// that *we* initiate to the configured token endpoint. We never accept a token
// that arrived via the browser/front-channel. We additionally validate the
// nonce, issuer, and audience claims as defense in depth. If token acceptance
// ever moves to a front-channel path, JWKS signature verification MUST be added.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	sessionCookie = "mmxc_session" // the auth proof
	flowCookie    = "mmxc_oidc"    // transient PKCE/state between login and callback
	flowMaxAge    = 600            // seconds the login flow may take
)

// oidcAuth holds the resolved runtime auth configuration. It is nil on the app
// struct unless config.AuthEnabled is set, so tests and local dev stay offline.
type oidcAuth struct {
	enabled      bool
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string
	scopes       string
	authURL      string // resolved (via discovery or explicit config)
	tokenURL     string // resolved
	hmacKey      []byte
	ttl          time.Duration
	trustXFP     bool
	httpClient   *http.Client
}

// sessionClaims is the payload of the signed mmxc_session cookie.
type sessionClaims struct {
	Sub   string `json:"sub"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Exp   int64  `json:"exp"`
}

// flowState is the payload of the signed, short-lived mmxc_oidc cookie. It
// carries the values that must survive the redirect to the provider and back.
type flowState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
}

// audience decodes the OIDC "aud" claim, which may be a string or a []string.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = audience{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err != nil {
		return err
	}
	*a = ss
	return nil
}

// newOIDCAuth validates the auth config, resolves the provider endpoints
// (discovering them from the issuer when not given explicitly), and resolves
// the cookie-signing key. It fails loud so the app never starts half-gated.
func newOIDCAuth(cfg config) (*oidcAuth, error) {
	if cfg.OIDCClientID == "" || cfg.OIDCClientSecret == "" {
		return nil, fmt.Errorf("oidc_client_id and oidc_client_secret are required")
	}
	if cfg.OIDCRedirectURL == "" {
		return nil, fmt.Errorf("oidc_redirect_url is required")
	}

	a := &oidcAuth{
		enabled:      true,
		issuer:       strings.TrimRight(cfg.OIDCIssuer, "/"),
		clientID:     cfg.OIDCClientID,
		clientSecret: cfg.OIDCClientSecret,
		redirectURL:  cfg.OIDCRedirectURL,
		scopes:       cfg.OIDCScopes,
		authURL:      cfg.OIDCAuthURL,
		tokenURL:     cfg.OIDCTokenURL,
		ttl:          time.Duration(cfg.SessionTTLHours) * time.Hour,
		trustXFP:     cfg.TrustForwardedProto,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}
	if a.scopes == "" {
		a.scopes = "openid profile email"
	}
	if a.ttl <= 0 {
		a.ttl = 24 * time.Hour
	}

	if a.authURL == "" || a.tokenURL == "" {
		if a.issuer == "" {
			return nil, fmt.Errorf("set oidc_issuer (for discovery) or both oidc_auth_url and oidc_token_url")
		}
		authURL, tokenURL, err := discoverOIDC(a.httpClient, a.issuer)
		if err != nil {
			return nil, fmt.Errorf("OIDC discovery from %s: %w", a.issuer, err)
		}
		a.authURL, a.tokenURL = authURL, tokenURL
	}

	if cfg.SessionSecret != "" {
		a.hmacKey = []byte(cfg.SessionSecret)
	} else {
		a.hmacKey = make([]byte, 32)
		if _, err := rand.Read(a.hmacKey); err != nil {
			return nil, fmt.Errorf("generate session key: %w", err)
		}
		log.Printf("auth: no session_secret configured; generated an ephemeral key — all sessions invalidate on restart")
	}
	return a, nil
}

// discoverOIDC fetches the provider's well-known configuration document and
// returns its authorization and token endpoints.
func discoverOIDC(client *http.Client, issuer string) (authURL, tokenURL string, err error) {
	resp, err := client.Get(issuer + "/.well-known/openid-configuration")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("provider returned %s", resp.Status)
	}
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", err
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return "", "", fmt.Errorf("discovery document missing endpoints")
	}
	return doc.AuthorizationEndpoint, doc.TokenEndpoint, nil
}

// --- cookie signing -------------------------------------------------------

// signValue encodes payload and appends an HMAC-SHA256 tag:
//
//	base64url(payload) "." base64url(HMAC(key, base64url(payload)))
func signValue(key, payload []byte) string {
	b := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(b))
	tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return b + "." + tag
}

// verifyValue checks the HMAC tag in constant time and returns the payload.
func verifyValue(key []byte, signed string) (payload []byte, ok bool) {
	b, tag, found := strings.Cut(signed, ".")
	if !found {
		return nil, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(b))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(tag), []byte(want)) != 1 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return nil, false
	}
	return payload, true
}

func signJSON(key []byte, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return signValue(key, b), nil
}

// --- crypto helpers -------------------------------------------------------

func randomURLSafe(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkcePair returns a PKCE verifier and its S256 challenge.
func pkcePair() (verifier, challenge string, err error) {
	verifier, err = randomURLSafe(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// --- request helpers ------------------------------------------------------

// secureFlag reports whether cookies should carry the Secure attribute: true
// over TLS, or behind a trusted reverse proxy reporting X-Forwarded-Proto:
// https. X-Forwarded-Proto is trusted only for this flag, never for an auth
// decision.
func (auth *oidcAuth) secureFlag(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return auth.trustXFP && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (auth *oidcAuth) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   auth.secureFlag(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (auth *oidcAuth) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	auth.setCookie(w, r, name, "", -1)
}

// currentSession returns the verified, unexpired session for the request.
func (auth *oidcAuth) currentSession(r *http.Request) (sessionClaims, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return sessionClaims{}, false
	}
	payload, ok := verifyValue(auth.hmacKey, c.Value)
	if !ok {
		return sessionClaims{}, false
	}
	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return sessionClaims{}, false
	}
	if claims.Exp <= time.Now().Unix() {
		return sessionClaims{}, false
	}
	return claims, true
}

// sanitizeReturnTo accepts only local same-origin paths to prevent open
// redirects: a single leading slash, not protocol-relative ("//host"), no
// backslashes. Anything else falls back to "/".
func sanitizeReturnTo(raw string) string {
	if raw == "" || raw[0] != '/' {
		return "/"
	}
	if strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "/"
	}
	return raw
}

// --- middleware -----------------------------------------------------------

func isAuthBypass(path string) bool {
	switch path {
	case "/auth/login", "/auth/callback", "/auth/logout", "/favicon.svg":
		return true
	}
	return strings.HasPrefix(path, "/static/")
}

// requireAuth gates every request behind a valid session when the login gate
// is enabled. When disabled it is a transparent pass-through, so tests and
// local dev are unaffected.
func (a *app) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.auth == nil || !a.auth.enabled {
			next.ServeHTTP(w, r)
			return
		}
		if isAuthBypass(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.auth.currentSession(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		dest := "/auth/login?return_to=" + url.QueryEscape(sanitizeReturnTo(r.URL.RequestURI()))
		http.Redirect(w, r, dest, http.StatusSeeOther)
	})
}

// --- handlers -------------------------------------------------------------

func (a *app) authLogin(w http.ResponseWriter, r *http.Request) {
	if a.auth == nil || !a.auth.enabled {
		http.NotFound(w, r)
		return
	}
	if _, ok := a.auth.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	state, err := randomURLSafe(32)
	if err != nil {
		a.authError(w, http.StatusInternalServerError, "Could not start sign-in.")
		return
	}
	nonce, err := randomURLSafe(32)
	if err != nil {
		a.authError(w, http.StatusInternalServerError, "Could not start sign-in.")
		return
	}
	verifier, challenge, err := pkcePair()
	if err != nil {
		a.authError(w, http.StatusInternalServerError, "Could not start sign-in.")
		return
	}

	flow := flowState{
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
		ReturnTo: sanitizeReturnTo(r.URL.Query().Get("return_to")),
	}
	signed, err := signJSON(a.auth.hmacKey, flow)
	if err != nil {
		a.authError(w, http.StatusInternalServerError, "Could not start sign-in.")
		return
	}
	a.auth.setCookie(w, r, flowCookie, signed, flowMaxAge)

	u, err := url.Parse(a.auth.authURL)
	if err != nil {
		a.authError(w, http.StatusInternalServerError, "Sign-in is misconfigured.")
		return
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", a.auth.clientID)
	q.Set("redirect_uri", a.auth.redirectURL)
	q.Set("scope", a.auth.scopes)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()

	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (a *app) authCallback(w http.ResponseWriter, r *http.Request) {
	if a.auth == nil || !a.auth.enabled {
		http.NotFound(w, r)
		return
	}

	c, err := r.Cookie(flowCookie)
	if err != nil {
		a.authError(w, http.StatusBadRequest, "Your sign-in session expired. Please try again.")
		return
	}
	payload, ok := verifyValue(a.auth.hmacKey, c.Value)
	if !ok {
		a.authError(w, http.StatusBadRequest, "Your sign-in session was invalid. Please try again.")
		return
	}
	var flow flowState
	if err := json.Unmarshal(payload, &flow); err != nil {
		a.authError(w, http.StatusBadRequest, "Your sign-in session was invalid. Please try again.")
		return
	}
	a.auth.clearCookie(w, r, flowCookie)

	query := r.URL.Query()
	if errCode := query.Get("error"); errCode != "" {
		msg := query.Get("error_description")
		if msg == "" {
			msg = errCode
		}
		a.authError(w, http.StatusForbidden, "Access denied: "+msg)
		return
	}

	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(flow.State)) != 1 {
		a.authError(w, http.StatusBadRequest, "Sign-in could not be verified. Please try again.")
		return
	}
	code := query.Get("code")
	if code == "" {
		a.authError(w, http.StatusBadRequest, "Sign-in returned no authorization code.")
		return
	}

	claims, err := a.auth.exchangeCode(code, flow.Verifier)
	if err != nil {
		log.Printf("auth: token exchange failed: %v", err)
		a.authError(w, http.StatusBadGateway, "Could not complete sign-in with the identity provider.")
		return
	}
	if claims.Nonce != flow.Nonce {
		a.authError(w, http.StatusBadRequest, "Sign-in could not be verified (nonce mismatch).")
		return
	}
	if a.auth.issuer != "" && claims.Iss != "" && strings.TrimRight(claims.Iss, "/") != a.auth.issuer {
		a.authError(w, http.StatusBadRequest, "Sign-in could not be verified (issuer mismatch).")
		return
	}
	if len(claims.Aud) > 0 && !contains(claims.Aud, a.auth.clientID) {
		a.authError(w, http.StatusBadRequest, "Sign-in could not be verified (audience mismatch).")
		return
	}

	session := sessionClaims{
		Sub:   claims.Sub,
		Name:  claims.Name,
		Email: claims.Email,
		Exp:   time.Now().Add(a.auth.ttl).Unix(),
	}
	signed, err := signJSON(a.auth.hmacKey, session)
	if err != nil {
		a.authError(w, http.StatusInternalServerError, "Could not complete sign-in.")
		return
	}
	a.auth.setCookie(w, r, sessionCookie, signed, int(a.auth.ttl.Seconds()))
	a.audit(r, auditEntry{
		Action: "session start",
		Detail: fmt.Sprintf("sub=%q name=%q email=%q", session.Sub, session.Name, session.Email),
	})

	http.Redirect(w, r, sanitizeReturnTo(flow.ReturnTo), http.StatusSeeOther)
}

// authLogout clears MMXC's local session and lands on a standalone "signed
// out" page with a Sign in button. It deliberately does NOT redirect into the
// provider: doing so would silently re-authenticate against the still-live SSO
// session and bounce the user straight back in. We also do not terminate the
// provider session (OIDC end_session_endpoint), since that is a global SSO
// logout affecting every other app sharing the provider. Signing back in stays
// seamless (no password prompt) while the provider session remains valid.
func (a *app) authLogout(w http.ResponseWriter, r *http.Request) {
	if a.auth == nil || !a.auth.enabled {
		http.NotFound(w, r)
		return
	}
	if session, ok := a.auth.currentSession(r); ok {
		a.audit(r, auditEntry{
			Action: "logout",
			Detail: fmt.Sprintf("sub=%q name=%q email=%q", session.Sub, session.Name, session.Email),
		})
	}
	a.auth.clearCookie(w, r, sessionCookie)
	a.render(w, "signed_out", pageData{Title: "Signed out"})
}

// idTokenClaims are the ID-token fields we read (no signature verification —
// see the trust note at the top of this file).
type idTokenClaims struct {
	Sub   string   `json:"sub"`
	Name  string   `json:"name"`
	Email string   `json:"email"`
	Nonce string   `json:"nonce"`
	Iss   string   `json:"iss"`
	Aud   audience `json:"aud"`
}

// exchangeCode performs the back-channel code-for-token POST and decodes the
// ID token payload.
func (auth *oidcAuth) exchangeCode(code, verifier string) (idTokenClaims, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", auth.redirectURL)
	form.Set("client_id", auth.clientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequest(http.MethodPost, auth.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return idTokenClaims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(auth.clientID), url.QueryEscape(auth.clientSecret))

	resp, err := auth.httpClient.Do(req)
	if err != nil {
		return idTokenClaims{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return idTokenClaims{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return idTokenClaims{}, fmt.Errorf("token endpoint returned %s: %s", resp.Status, short(string(body)))
	}

	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return idTokenClaims{}, fmt.Errorf("parse token response: %w", err)
	}
	if tok.IDToken == "" {
		return idTokenClaims{}, fmt.Errorf("token response had no id_token")
	}
	return decodeIDToken(tok.IDToken)
}

// decodeIDToken base64url-decodes the JWT payload segment and unmarshals the
// claims. It does NOT verify the signature (see the file-level trust note).
func decodeIDToken(token string) (idTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return idTokenClaims{}, fmt.Errorf("malformed id_token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idTokenClaims{}, fmt.Errorf("decode id_token payload: %w", err)
	}
	var claims idTokenClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return idTokenClaims{}, fmt.Errorf("parse id_token claims: %w", err)
	}
	if claims.Sub == "" {
		return idTokenClaims{}, fmt.Errorf("id_token has no subject")
	}
	return claims, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// authError renders the shared error template with an explicit status code.
func (a *app) authError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	a.render(w, "error", pageData{Title: "Sign in", Info: a.info(), Error: msg})
}
