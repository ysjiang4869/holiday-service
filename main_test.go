package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func call(a *App, path, method string, body any, cookie *http.Cookie, token string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Calendar-Request", "1")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func TestCalendar(t *testing.T) {
	b, e := assets.ReadFile("data/years/2026.json")
	if e != nil {
		t.Fatal(e)
	}
	y, e := validateYear(b, 2026)
	if e != nil {
		t.Fatal(e)
	}
	u := &User{Overrides: map[string]Override{}}
	cases := []struct {
		date string
		kind string
		rest bool
	}{{"2026-09-30", "workday", false}, {"2026-10-01", "holiday", true}, {"2026-10-10", "makeup_after", false}, {"2026-09-20", "makeup_before", false}, {"2026-09-19", "weekend", true}}
	for _, c := range cases {
		d, _ := time.Parse("2006-01-02", c.date)
		v := classify(d, y, u)
		if v.Kind != c.kind || v.Rest != c.rest {
			t.Errorf("%s: %+v", c.date, v)
		}
	}
	u.Overrides["2026-10-10"] = Override{true, "请假"}
	d, _ := time.Parse("2006-01-02", "2026-10-10")
	v := classify(d, y, u)
	if v.Kind != "personal_rest" || v.Source != "manual" || v.BaseType != 4 {
		t.Fatal(v)
	}
}
func TestAuthPersistenceAndIsolation(t *testing.T) {
	dir := t.TempDir()
	a, e := newApp(dir)
	if e != nil {
		t.Fatal(e)
	}
	w := call(a, "/api/setup", "POST", map[string]string{"username": "alice", "password": "a-long-password-123"}, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	w = call(a, "/api/users", "POST", map[string]string{"username": "bob", "password": "another-password-123"}, cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call(a, "/api/tokens", "POST", map[string]string{"name": "node-red"}, cookie, "")
	var v map[string]string
	json.Unmarshal(w.Body.Bytes(), &v)
	token := v["token"]
	if len(token) != 64 {
		t.Fatal(w.Body.String())
	}
	w = call(a, "/api/override", "POST", map[string]any{"date": "2026-10-10", "isRestDay": true, "reason": "年假"}, cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call(a, "/api/holiday?date=2026-10-10", "GET", nil, nil, token)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("personal_rest")) {
		t.Fatal(w.Body.String())
	}
	w = call(a, "/api/holiday?date=2026-09", "GET", nil, nil, token)
	var month struct{ Data []Detail }
	json.Unmarshal(w.Body.Bytes(), &month)
	if len(month.Data) != 30 {
		t.Fatal(len(month.Data))
	}
	w = call(a, "/api/holiday?date=invalid", "GET", nil, nil, token)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = call(a, "/api/override", "POST", map[string]any{}, nil, token)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = call(a, "/api/login", "POST", map[string]string{"username": "bob", "password": "another-password-123"}, nil, "")
	bob := w.Result().Cookies()[0]
	w = call(a, "/api/holiday?date=2026-10-10", "GET", nil, bob, "")
	if bytes.Contains(w.Body.Bytes(), []byte("personal_rest")) {
		t.Fatal("用户标记泄漏")
	}
	a2, e := newApp(dir)
	if e != nil {
		t.Fatal(e)
	}
	w = call(a2, "/api/holiday?date=2026-10-10", "GET", nil, nil, token)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("personal_rest")) {
		t.Fatal("重启持久化失败", w.Body.String())
	}
	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte("a-long-password-123")) {
		t.Fatal("明文凭证落盘")
	}
	call(a, "/api/tokens", "DELETE", map[string]string{"name": "node-red"}, cookie, "")
	if call(a, "/api/holiday", "GET", nil, nil, token).Code != 401 {
		t.Fatal("撤销未生效")
	}
	r := httptest.NewRequest("POST", "/api/override", bytes.NewBufferString(`{}`))
	r.AddCookie(cookie)
	out := httptest.NewRecorder()
	a.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("跨站请求未拦截")
	}
}
