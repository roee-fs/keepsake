// The admin session: one password, one signed cookie, no server-side state.
// Ported from 2de90d2:src/keepsake/server/auth.py.
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const cookieName = "keepsake_session"

// MisconfiguredAdmin is returned at startup. An enabled console with no password is worse than none.
type MisconfiguredAdmin struct{ Msg string }

func (e *MisconfiguredAdmin) Error() string { return e.Msg }

// UIEnabled reports whether the admin console is on. It defaults to on.
func UIEnabled() bool {
	v, ok := os.LookupEnv("KEEPSAKE_UI")
	return !ok || strings.ToLower(v) == "true"
}

// AdminPassword reads KEEPSAKE_ADMIN_PASSWORD, refusing when the console is on without one.
func AdminPassword() (string, error) {
	password := os.Getenv("KEEPSAKE_ADMIN_PASSWORD")
	if UIEnabled() && password == "" {
		return "", &MisconfiguredAdmin{Msg: "KEEPSAKE_ADMIN_PASSWORD is unset but the admin console is enabled"}
	}
	return password, nil
}

// Auth is the one admin account. The password is the whole credential.
type Auth struct {
	password string
	key      []byte
}

// NewAuth derives the cookie key from the password, so changing the password
// revokes every outstanding cookie.
func NewAuth(password string) *Auth {
	return &Auth{password: password, key: mac([]byte(password), "keepsake-session")}
}

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func (a *Auth) CheckPassword(supplied string) bool {
	return hmac.Equal([]byte(supplied), []byte(a.password))
}

// Issue returns a cookie of the form "<expiry>.<hexdigest>".
func (a *Auth) Issue(ttl time.Duration) string {
	expiry := strconv.FormatInt(time.Now().Unix()+int64(ttl/time.Second), 10)
	return expiry + "." + hex.EncodeToString(mac(a.key, expiry))
}

func (a *Auth) Valid(cookie string) bool {
	expiry, digest, _ := strings.Cut(cookie, ".")
	if expiry == "" || strings.Trim(expiry, "0123456789") != "" {
		return false
	}
	if !hmac.Equal([]byte(digest), []byte(hex.EncodeToString(mac(a.key, expiry)))) {
		return false
	}
	n, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && !time.Unix(n, 0).Before(time.Now())
}

// session reports whether r carries a valid session cookie.
func (a *Auth) session(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && a.Valid(c.Value)
}
