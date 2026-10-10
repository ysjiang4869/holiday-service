package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

//go:embed web/* data/years/*
var assets embed.FS
var zone, _ = time.LoadLocation("Asia/Shanghai")

type Override struct {
	Rest   bool   `json:"isRestDay"`
	Reason string `json:"reason"`
}
type User struct {
	Name      string              `json:"name"`
	Salt      string              `json:"salt"`
	Password  string              `json:"password"`
	Admin     bool                `json:"admin"`
	Overrides map[string]Override `json:"overrides"`
	Tokens    map[string]string   `json:"tokens"`
}
type State struct {
	Users map[string]*User `json:"users"`
}
type Session struct {
	User    string
	Expires time.Time
}
type Day struct {
	Date string `json:"date"`
	Name string `json:"name"`
	Off  bool   `json:"isOffDay"`
}
type Year struct {
	Year   int      `json:"year"`
	Papers []string `json:"papers"`
	Days   []Day    `json:"days"`
}
type Detail struct {
	Date     string `json:"date"`
	Rest     bool   `json:"isRestDay"`
	Type     int    `json:"type"`
	Kind     string `json:"kind"`
	TypeName string `json:"typeName"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	BaseType int    `json:"baseType"`
	BaseName string `json:"baseName"`
	Reason   string `json:"reason,omitempty"`
}
type App struct {
	mu       sync.Mutex
	state    State
	sessions map[string]Session
	years    map[int]Year
	dir      string
	client   *http.Client
	attempts map[string][]time.Time
}

func random() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func password(salt, p string) string {
	h, e := pbkdf2.Key(sha256.New, p, []byte(salt), 600000, 32)
	if e != nil {
		panic(e)
	}
	return hex.EncodeToString(h)
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func atomic(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	tmp := path + ".tmp"
	if e := os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}
func (a *App) save() error {
	b, e := json.MarshalIndent(a.state, "", "  ")
	if e != nil {
		return e
	}
	return atomic(filepath.Join(a.dir, "state.json"), b)
}
func newApp(dir string) (*App, error) {
	a := &App{dir: dir, attempts: map[string][]time.Time{}, state: State{Users: map[string]*User{}}, sessions: map[string]Session{}, years: map[int]Year{}, client: &http.Client{Timeout: 15 * time.Second}}
	b, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e == nil {
		e = json.Unmarshal(b, &a.state)
	} else if os.IsNotExist(e) {
		e = nil
	}
	return a, e
}
func validateYear(b []byte, y int) (Year, error) {
	var v Year
	if e := json.Unmarshal(b, &v); e != nil {
		return v, e
	}
	if v.Year != y || len(v.Days) == 0 {
		return v, errors.New("年度数据缺失")
	}
	seen := map[string]bool{}
	for _, d := range v.Days {
		t, e := time.Parse("2006-01-02", d.Date)
		if e != nil || t.Year() != y || seen[d.Date] || d.Name == "" {
			return v, errors.New("年度数据无效")
		}
		seen[d.Date] = true
	}
	sort.Slice(v.Days, func(i, j int) bool { return v.Days[i].Date < v.Days[j].Date })
	return v, nil
}
func (a *App) year(y int, refresh bool) (Year, error) {
	if v, ok := a.years[y]; ok && !refresh {
		return v, nil
	}
	path := filepath.Join(a.dir, "years", fmt.Sprintf("%d.json", y))
	if !refresh {
		b, e := os.ReadFile(path)
		if e != nil {
			b, e = assets.ReadFile(fmt.Sprintf("data/years/%d.json", y))
		}
		if e == nil {
			v, e := validateYear(b, y)
			if e == nil {
				if e = atomic(path, b); e != nil {
					return Year{}, e
				}
				a.years[y] = v
				return v, nil
			}
		}
	}
	url := fmt.Sprintf("https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/%d.json", y)
	r, e := a.client.Get(url)
	if e != nil {
		return Year{}, errors.New("获取年度数据失败，请使用已有缓存或稍后重试")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return Year{}, errors.New("该年份尚无可用日历")
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if e != nil {
		return Year{}, e
	}
	v, e := validateYear(b, y)
	if e != nil {
		return v, e
	}
	if e = atomic(path, b); e != nil {
		return v, e
	}
	a.years[y] = v
	return v, nil
}

var kinds = []string{"workday", "weekend", "holiday", "makeup_before", "makeup_after", "personal_rest", "personal_work"}

func classify(t time.Time, y Year, u *User) Detail {
	date := t.Format("2006-01-02")
	d := Detail{Date: date, Source: "week_rule", Name: "工作日"}
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		d.Type = 1
		d.Name = "周末"
	}
	for _, v := range y.Days {
		if v.Date != date {
			continue
		}
		d.Name = v.Name
		d.Source = "official"
		d.Type = 2
		if !v.Off {
			d.Type = 3
			for _, h := range y.Days {
				if h.Name == v.Name && h.Off && h.Date < date {
					d.Type = 4
					break
				}
			}
		}
		break
	}
	d.BaseType = d.Type
	d.BaseName = d.Name
	d.Rest = d.Type == 1 || d.Type == 2
	if o, ok := u.Overrides[date]; ok {
		d.Rest = o.Rest
		d.Type = 6
		if o.Rest {
			d.Type = 5
		}
		d.Source = "manual"
		d.Reason = o.Reason
		d.Name = o.Reason
		if d.Name == "" {
			if o.Rest {
				d.Name = "个人休息"
			} else {
				d.Name = "个人工作"
			}
		}
	}
	d.Kind = kinds[d.Type]
	d.TypeName = []string{"普通工作日", "普通周末", "节假日放假", "节前补班", "节后补班", "个人休息", "个人工作"}[d.Type]
	return d
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, s string) {
	reply(w, status, map[string]any{"code": -1, "message": s})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid JSON")
	}
	return nil
}
func (a *App) auth(r *http.Request) (string, *User, bool) {
	if c, e := r.Cookie("calendar_session"); e == nil {
		if s, ok := a.sessions[c.Value]; ok && time.Now().Before(s.Expires) {
			return s.User, a.state.Users[s.User], true
		}
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		hash := digest(strings.TrimPrefix(h, "Bearer "))
		for n, u := range a.state.Users {
			for _, v := range u.Tokens {
				if equal(v, hash) {
					return n, u, false
				}
			}
		}
	}
	return "", nil, false
}
func validName(s string) bool {
	if len(s) < 1 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" {
		reply(w, 200, map[string]string{"status": "ok"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		p := r.URL.Path
		if p == "/" {
			p = "/index.html"
		}
		if p != "/index.html" && p != "/app.js" && p != "/style.css" {
			http.NotFound(w, r)
			return
		}
		b, e := assets.ReadFile("web" + p)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		switch p {
		case "/app.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case "/style.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		w.Write(b)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	path := r.URL.Path
	if r.Method != "GET" {
		if r.Header.Get("X-Calendar-Request") != "1" {
			fail(w, 403, "缺少请求校验头")
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			fail(w, 415, "需要 JSON 请求")
			return
		}
	}
	if path == "/api/status" && r.Method == "GET" {
		reply(w, 200, map[string]any{"setupRequired": len(a.state.Users) == 0})
		return
	}
	if (path == "/api/setup" || path == "/api/login") && r.Method == "POST" {
		var b struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if decode(w, r, &b) != nil {
			fail(w, 400, "请求无效")
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		now := time.Now()
		for key, times := range a.attempts {
			fresh := times[:0]
			for _, at := range times {
				if now.Sub(at) < time.Minute {
					fresh = append(fresh, at)
				}
			}
			if len(fresh) == 0 {
				delete(a.attempts, key)
			} else {
				a.attempts[key] = fresh
			}
		}
		if len(a.attempts[ip]) >= 10 {
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "登录尝试过于频繁，请一分钟后重试")
			return
		}
		a.attempts[ip] = append(a.attempts[ip], now)
		for key, session := range a.sessions {
			if now.After(session.Expires) {
				delete(a.sessions, key)
			}
		}
		if path == "/api/setup" {
			if len(a.state.Users) != 0 {
				fail(w, 409, "已初始化")
				return
			}
			if !validName(b.Username) || len(b.Password) < 12 || len(b.Password) > 256 {
				fail(w, 400, "用户名使用字母数字或 _-，密码至少12字符")
				return
			}
			salt := random()
			a.state.Users[b.Username] = &User{Name: b.Username, Salt: salt, Password: password(salt, b.Password), Admin: true, Overrides: map[string]Override{}, Tokens: map[string]string{}}
			if e := a.save(); e != nil {
				delete(a.state.Users, b.Username)
				fail(w, 500, "保存失败")
				return
			}
		}
		u := a.state.Users[b.Username]
		if u == nil || !equal(u.Password, password(u.Salt, b.Password)) {
			time.Sleep(250 * time.Millisecond)
			fail(w, 401, "用户名或密码错误")
			return
		}
		sid := random()
		a.sessions[sid] = Session{b.Username, time.Now().Add(24 * time.Hour)}
		http.SetCookie(w, &http.Cookie{Name: "calendar_session", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: 86400})
		reply(w, 200, map[string]any{"username": b.Username, "admin": u.Admin})
		return
	}
	name, u, session := a.auth(r)
	if u == nil {
		fail(w, 401, "请登录或提供有效的 Bearer Token")
		return
	}
	if path == "/api/holiday" && r.Method == "GET" {
		s := r.URL.Query().Get("date")
		if s == "" {
			s = time.Now().In(zone).Format("2006-01-02")
		}
		layout := "2006-01-02"
		switch len(s) {
		case 4:
			layout = "2006"
		case 7:
			layout = "2006-01"
		}
		t, e := time.ParseInLocation(layout, s, zone)
		if e != nil || t.Year() < 2004 || t.Year() > 2100 {
			fail(w, 400, "日期格式须为 YYYY、YYYY-MM 或 YYYY-MM-DD")
			return
		}
		y, e := a.year(t.Year(), false)
		if e != nil {
			fail(w, 503, e.Error())
			return
		}
		end := t.AddDate(0, 0, 1)
		if layout == "2006" {
			end = t.AddDate(1, 0, 0)
		} else if layout == "2006-01" {
			end = t.AddDate(0, 1, 0)
		}
		ds := []Detail{}
		for d := t; d.Before(end); d = d.AddDate(0, 0, 1) {
			ds = append(ds, classify(d, y, u))
		}
		v := r.URL.Query().Get("verbose")
		if len(ds) == 1 && (v == "0" || v == "1") {
			w.Header().Set("Content-Type", "text/plain")
			if v == "1" {
				fmt.Fprint(w, ds[0].Type)
			} else if ds[0].Rest {
				fmt.Fprint(w, 1)
			} else {
				fmt.Fprint(w, 0)
			}
			return
		}
		reply(w, 200, map[string]any{"code": 0, "message": "success", "timezone": "Asia/Shanghai", "user": name, "data": ds})
		return
	}
	if !session {
		fail(w, 403, "API Token 仅允许查询日历")
		return
	}
	switch path {
	case "/api/me":
		if r.Method == "GET" {
			tokens := []string{}
			for k := range u.Tokens {
				tokens = append(tokens, k)
			}
			sort.Strings(tokens)
			reply(w, 200, map[string]any{"username": name, "admin": u.Admin, "tokens": tokens})
			return
		}
	case "/api/logout":
		if r.Method == "POST" {
			c, _ := r.Cookie("calendar_session")
			if c != nil {
				delete(a.sessions, c.Value)
			}
			http.SetCookie(w, &http.Cookie{Name: "calendar_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			reply(w, 200, map[string]bool{"ok": true})
			return
		}
	case "/api/override":
		if r.Method == "POST" {
			var b struct {
				Date   string `json:"date"`
				Rest   *bool  `json:"isRestDay"`
				Reason string `json:"reason"`
			}
			if decode(w, r, &b) != nil || len(b.Reason) > 240 {
				fail(w, 400, "请求无效")
				return
			}
			t, e := time.ParseInLocation("2006-01-02", b.Date, zone)
			if e != nil || t.Year() < 2004 || t.Year() > 2100 {
				fail(w, 400, "日期无效")
				return
			}
			old, exists := u.Overrides[b.Date]
			if b.Rest == nil {
				delete(u.Overrides, b.Date)
			} else {
				u.Overrides[b.Date] = Override{*b.Rest, b.Reason}
			}
			if a.save() != nil {
				if exists {
					u.Overrides[b.Date] = old
				} else {
					delete(u.Overrides, b.Date)
				}
				fail(w, 500, "保存失败")
				return
			}
			reply(w, 200, map[string]bool{"ok": true})
			return
		}
	case "/api/tokens":
		var b struct {
			Name string `json:"name"`
		}
		if decode(w, r, &b) != nil || !validName(b.Name) {
			fail(w, 400, "Token 名称使用字母数字或 _-")
			return
		}
		if r.Method == "POST" {
			if _, ok := u.Tokens[b.Name]; ok {
				fail(w, 409, "名称已存在")
				return
			}
			token := random()
			u.Tokens[b.Name] = digest(token)
			if a.save() != nil {
				delete(u.Tokens, b.Name)
				fail(w, 500, "保存失败")
				return
			}
			reply(w, 200, map[string]string{"name": b.Name, "token": token})
			return
		}
		if r.Method == "DELETE" {
			old, ok := u.Tokens[b.Name]
			delete(u.Tokens, b.Name)
			if a.save() != nil {
				if ok {
					u.Tokens[b.Name] = old
				}
				fail(w, 500, "保存失败")
				return
			}
			reply(w, 200, map[string]bool{"ok": true})
			return
		}
	case "/api/users":
		if !u.Admin {
			fail(w, 403, "需要管理员")
			return
		}
		if r.Method == "GET" {
			names := []string{}
			for n := range a.state.Users {
				names = append(names, n)
			}
			sort.Strings(names)
			reply(w, 200, names)
			return
		}
		if r.Method == "POST" {
			var b struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if decode(w, r, &b) != nil || !validName(b.Username) || len(b.Password) < 12 || len(b.Password) > 256 {
				fail(w, 400, "用户名或密码无效，密码至少12字符")
				return
			}
			if _, ok := a.state.Users[b.Username]; ok {
				fail(w, 409, "用户已存在")
				return
			}
			salt := random()
			a.state.Users[b.Username] = &User{Name: b.Username, Salt: salt, Password: password(salt, b.Password), Overrides: map[string]Override{}, Tokens: map[string]string{}}
			if a.save() != nil {
				delete(a.state.Users, b.Username)
				fail(w, 500, "保存失败")
				return
			}
			reply(w, 200, map[string]bool{"ok": true})
			return
		}
	case "/api/refresh":
		if r.Method == "POST" && u.Admin {
			var b struct {
				Year int `json:"year"`
			}
			if decode(w, r, &b) != nil || b.Year < 2004 || b.Year > 2100 {
				fail(w, 400, "年份无效")
				return
			}
			if _, e := a.year(b.Year, true); e != nil {
				fail(w, 503, e.Error())
				return
			}
			reply(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 404, "接口或方法不存在")
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "initdata" {
		dir := os.Getenv("DATA_DIR")
		if dir != "/data" {
			log.Fatal("initdata requires DATA_DIR=/data")
		}
		if e := os.MkdirAll(dir, 0700); e != nil {
			log.Fatal(e)
		}
		if e := os.Chown(dir, 10001, 10001); e != nil {
			log.Fatal(e)
		}
		if e := os.Chmod(dir, 0700); e != nil {
			log.Fatal(e)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		c := http.Client{Timeout: 2 * time.Second}
		r, e := c.Get("http://127.0.0.1:8080/healthz")
		if e != nil || r.StatusCode != 200 {
			os.Exit(1)
		}
		r.Body.Close()
		return
	}

	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./var"
	}
	a, e := newApp(dir)
	if e != nil {
		log.Fatal("无法读取持久化数据")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: a, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("calendar listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
