package qbittorrent

import "testing"

func TestClient_New_TrimsTrailingSlash(t *testing.T) {
	c := New("http://qb.example.com:8080/", "u", "p", "")
	if c.baseURL != "http://qb.example.com:8080" {
		t.Errorf("trailing slash not trimmed: got %q", c.baseURL)
	}
}

func TestClient_New_TrimsAPIKeyWhitespace(t *testing.T) {
	c := New("http://qb.example.com", "", "", "  qbt_abc123  ")
	if c.apiKey != "qbt_abc123" {
		t.Errorf("apiKey whitespace not trimmed: got %q", c.apiKey)
	}
	if !c.usesAPIKey() {
		t.Error("expected usesAPIKey=true")
	}
}

func TestClient_New_NoAPIKey(t *testing.T) {
	c := New("http://qb.example.com", "u", "p", "")
	if c.usesAPIKey() {
		t.Error("expected usesAPIKey=false when empty")
	}
}

func TestClient_SetCredentials_HotSwapClearsCookie(t *testing.T) {
	c := New("http://old.host", "olduser", "oldpass", "")
	c.cookie = "fake-session=cached"
	if c.cookie == "" {
		t.Fatal("precondition: cookie should be set")
	}

	c.SetCredentials("https://new.host/", "newuser", "newpass", "qbt_newkey")

	if c.baseURL != "https://new.host" {
		t.Errorf("baseURL not swapped: got %q", c.baseURL)
	}
	if c.user != "newuser" || c.pass != "newpass" {
		t.Error("user/pass not swapped")
	}
	if c.apiKey != "qbt_newkey" {
		t.Errorf("apiKey not swapped: got %q", c.apiKey)
	}
	if c.cookie != "" {
		t.Errorf("cookie should be cleared on credential swap, got %q", c.cookie)
	}
	if !c.usesAPIKey() {
		t.Error("expected usesAPIKey=true after swap")
	}
}

func TestClient_SetCredentials_TrimsAPIKey(t *testing.T) {
	c := New("http://x", "", "", "")
	c.SetCredentials("http://x", "", "", "  kkk  ")
	if c.apiKey != "kkk" {
		t.Errorf("SetCredentials should trim apiKey: got %q", c.apiKey)
	}
}

func TestClient_mustParse_ValidURL(t *testing.T) {
	u := mustParse("http://qb.example.com:8080/path")
	if u == nil {
		t.Fatal("mustParse should not return nil on valid URL")
	}
	if u.Host != "qb.example.com:8080" {
		t.Errorf("host mismatch: got %q", u.Host)
	}
}

func TestClient_mustParse_InvalidURL(t *testing.T) {
	// mustParse 丢弃 url.Parse 错误，非法输入时返回 nil 是当前实现行为
	u := mustParse("not a url %%%%")
	if u != nil {
		t.Errorf("expected nil on invalid URL, got %v", u)
	}
}
