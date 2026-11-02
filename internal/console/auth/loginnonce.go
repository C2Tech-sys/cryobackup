package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"time"
)

// Login nonce names. The nonce is the CSRF defence of the login form, which has
// no session to bind a token to and which cannot rely on request headers: a
// privacy-hardened browser may send Origin: null with no Referer and no
// Sec-Fetch-Site on a same-origin form post.
const (
	LoginNonceCookie = "cryo_login"
	LoginNonceField  = "login_nonce"
	loginNoncePath   = "/login"
	loginNonceTTL    = 10 * time.Minute
	// loginNonceLabel separates the login-nonce MAC from every other use of a key.
	loginNonceLabel = "cryobackup/login-nonce/v1"
)

// loginKey is the server secret behind the nonce: 32 random bytes made once per
// Manager (the process) and derived with loginNonceLabel. A restart invalidates
// outstanding nonces, which live ten minutes anyway. The session CSRF token has
// no server key (it is keyed by the session cookie, which a login does not have
// yet), so the login nonce brings its own.
func (m *Manager) loginKey() []byte {
	m.nonceOnce.Do(func() {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return
		}
		h := hmac.New(sha256.New, k)
		h.Write([]byte(loginNonceLabel))
		m.nonceKey = h.Sum(nil)
	})
	return m.nonceKey
}

// loginNonceMAC is the form-field value for a cookie value:
// base64url(HMAC-SHA256(key, cookie)). A client that invents a cookie cannot
// produce the matching field without the server key.
func (m *Manager) loginNonceMAC(cookie string) string {
	k := m.loginKey()
	if k == nil || cookie == "" {
		return ""
	}
	h := hmac.New(sha256.New, k)
	h.Write([]byte(cookie))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// IssueLoginNonce mints a fresh nonce (32 random bytes, base64url), sets it as
// the cryo_login cookie (HttpOnly, SameSite=Strict, Path /login, 10 minutes,
// Secure by the same rule as the session cookie) and returns the value for the
// page to render in the hidden login_nonce field: the server MAC of the cookie
// value, not the cookie value itself. Call it before the response header is
// written. A random-source failure returns "" and sets nothing, so the form
// falls back to the header-based origin rule.
func (m *Manager) IssueLoginNonce(w http.ResponseWriter, r *http.Request) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	field := m.loginNonceMAC(v)
	if field == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: LoginNonceCookie, Value: v, Path: loginNoncePath, MaxAge: int(loginNonceTTL / time.Second),
		HttpOnly: true, Secure: secureConn(r), SameSite: http.SameSiteStrictMode})
	return field
}

// LoginNoncePresent reports whether the request carries a non-empty login_nonce
// form value (the form must already be parsed). It lets a caller tell "no nonce
// offered" from "a nonce that did not match".
func LoginNoncePresent(r *http.Request) bool {
	return r.PostFormValue(LoginNonceField) != ""
}

// LoginNonceOK reports whether the cryo_login cookie is present and the
// login_nonce form value equals the server MAC of it (constant time). The form
// must already be parsed. A match proves the cookie was issued by this server
// and the post carries that page's field; an injected cookie with an invented
// field fails. It says nothing about request headers: callers must also refuse
// on HeadersContradict.
func (m *Manager) LoginNonceOK(r *http.Request) bool {
	field := r.PostFormValue(LoginNonceField)
	if field == "" {
		return false
	}
	c, err := r.Cookie(LoginNonceCookie)
	if err != nil || c.Value == "" {
		return false
	}
	want := m.loginNonceMAC(c.Value)
	return want != "" && subtle.ConstantTimeCompare([]byte(field), []byte(want)) == 1
}

// ClearLoginNonce expires the cryo_login cookie with the attributes
// IssueLoginNonce used.
func ClearLoginNonce(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: LoginNonceCookie, Value: "", Path: loginNoncePath, MaxAge: -1,
		HttpOnly: true, Secure: secureConn(r), SameSite: http.SameSiteStrictMode})
}
