// Ported from 8f2af2e:tests/test_auth.py, plus the cookie-compatibility check across the cutover.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

const password = "correct-password"

func TestTheRightPasswordIsAccepted(t *testing.T) {
	if !NewAuth(password).CheckPassword(password) {
		t.Fatal("the right password was rejected")
	}
}

func TestAWrongPasswordIsRejected(t *testing.T) {
	if NewAuth(password).CheckPassword("wrong") {
		t.Fatal("a wrong password was accepted")
	}
}

func TestANonASCIIPasswordIsRejectedRatherThanRaising(t *testing.T) {
	if NewAuth(password).CheckPassword("pässwörd") {
		t.Fatal("a wrong non-ASCII password was accepted")
	}
}

func TestANonASCIIConfiguredPasswordStillAuthenticates(t *testing.T) {
	a := NewAuth("pässwörd-Ω")
	if !a.CheckPassword("pässwörd-Ω") || a.CheckPassword("pässwörd") {
		t.Fatal("a Unicode password MUST authenticate exactly")
	}
}

func TestAFreshlyIssuedCookieIsAccepted(t *testing.T) {
	a := NewAuth(password)
	if !a.Valid(a.Issue(time.Hour)) {
		t.Fatal("a fresh cookie was rejected")
	}
}

func TestACookieSignedWithAnotherPasswordIsRejected(t *testing.T) {
	if NewAuth("old-password").Valid(NewAuth(password).Issue(time.Hour)) {
		t.Fatal("a password change MUST revoke old cookies")
	}
}

func TestAnExpiredCookieIsRejected(t *testing.T) {
	a := NewAuth(password)
	if a.Valid(a.Issue(-time.Second)) {
		t.Fatal("an expired cookie was accepted")
	}
}

func TestAMalformedCookieIsRejected(t *testing.T) {
	a := NewAuth(password)
	good := a.Issue(time.Hour)
	for _, c := range []string{"", ".", "abc", good[:len(good)-1], "+" + good, "٤" + good} {
		if a.Valid(c) {
			t.Errorf("Valid(%q) = true", c)
		}
	}
}

func TestAdminPasswordIsFatalWhenMissingAndUIEnabled(t *testing.T) {
	t.Setenv("KEEPSAKE_ADMIN_PASSWORD", "")
	os.Unsetenv("KEEPSAKE_ADMIN_PASSWORD")
	t.Setenv("KEEPSAKE_UI", "")
	os.Unsetenv("KEEPSAKE_UI")
	_, err := AdminPassword()
	var m *MisconfiguredAdmin
	if !errors.As(err, &m) || err.Error() != "KEEPSAKE_ADMIN_PASSWORD is unset but the admin console is enabled" {
		t.Fatalf("err = %v, want MisconfiguredAdmin", err)
	}
}

// `echo pw | base64` stores a newline that no password input can submit.
func TestATrailingNewlineInTheConfiguredPasswordIsIgnored(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		t.Setenv("KEEPSAKE_ADMIN_PASSWORD", password+ending)
		pw, err := AdminPassword()
		if err != nil || !NewAuth(pw).CheckPassword(password) {
			t.Errorf("a configured password ending %q rejected the password", ending)
		}
	}
}

func TestAdminPasswordIsFineWhenMissingAndUIDisabled(t *testing.T) {
	t.Setenv("KEEPSAKE_ADMIN_PASSWORD", "")
	os.Unsetenv("KEEPSAKE_ADMIN_PASSWORD")
	t.Setenv("KEEPSAKE_UI", "FALSE")
	if UIEnabled() {
		t.Fatal("KEEPSAKE_UI=FALSE MUST disable the console")
	}
	if pw, err := AdminPassword(); pw != "" || err != nil {
		t.Fatalf("AdminPassword() = %q, %v", pw, err)
	}
}

func TestRequireSessionRejectsAMissingCookie(t *testing.T) {
	a := NewAuth(password)
	if a.session(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("a request without a cookie passed the guard")
	}
}

func TestRequireSessionAcceptsAValidCookie(t *testing.T) {
	a := NewAuth(password)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: a.Issue(time.Hour)})
	if !a.session(r) {
		t.Fatal("a valid cookie was rejected")
	}
}

func TestCookiesIssuedByPythonVerify(t *testing.T) {
	raw, err := os.ReadFile("../../okf/testdata/goldens/session.json")
	if err != nil {
		t.Fatal(err)
	}
	var goldens []struct{ Password, Cookie string }
	if err := json.Unmarshal(raw, &goldens); err != nil {
		t.Fatal(err)
	}
	if len(goldens) == 0 {
		t.Fatal("no session goldens")
	}
	for _, g := range goldens {
		if !NewAuth(g.Password).Valid(g.Cookie) {
			t.Errorf("a python session cookie for %q MUST survive the cutover", g.Password)
		}
	}
}
