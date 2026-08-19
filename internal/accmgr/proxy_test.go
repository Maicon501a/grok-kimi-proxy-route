package accmgr

import "testing"

func TestSignupProxyParsing(t *testing.T) {
	t.Setenv("ACCIO_SIGNUP_PROXY", "")
	if signupProxy() != nil {
		t.Fatal("empty env must yield no proxy")
	}

	t.Setenv("ACCIO_SIGNUP_PROXY", "http://user:pass@gw.example:12321")
	u := signupProxy()
	if u == nil {
		t.Fatal("valid http proxy must parse")
	}
	if got := proxyServerFlag(u); got != "http://gw.example:12321" {
		t.Fatalf("proxyServerFlag = %q", got)
	}
	user, pass, ok := proxyCredentials(u)
	if !ok || user != "user" || pass != "pass" {
		t.Fatalf("credentials = %q %q %v", user, pass, ok)
	}

	t.Setenv("ACCIO_SIGNUP_PROXY", "socks5://gw.example:1080")
	u = signupProxy()
	if u == nil {
		t.Fatal("socks5 proxy must parse")
	}
	if _, _, ok := proxyCredentials(u); ok {
		t.Fatal("no credentials expected")
	}

	t.Setenv("ACCIO_SIGNUP_PROXY", "ftp://gw.example:21")
	if signupProxy() != nil {
		t.Fatal("unsupported scheme must be rejected")
	}
	t.Setenv("ACCIO_SIGNUP_PROXY", "not a url at all ::")
	if signupProxy() != nil {
		t.Fatal("garbage must be rejected")
	}
}
