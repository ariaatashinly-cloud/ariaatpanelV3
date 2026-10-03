package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

type Settings struct {
	PublicURL       string `json:"publicURL"`
	Brand           string `json:"brand"`
	HostPreset      string `json:"hostPreset"`
	AutomaticDomain bool   `json:"automaticDomain"`
}
type Audit struct {
	Time    int64  `json:"time"`
	Message string `json:"message"`
	Kind    string `json:"kind"`
}
type diskState struct {
	Settings Settings `json:"settings"`
	Audit    []Audit  `json:"audit"`
}
type Snapshot struct {
	Brand      string          `json:"brand"`
	Version    string          `json:"version"`
	Ready      bool            `json:"ready"`
	Settings   Settings        `json:"settings"`
	Users      []User          `json:"users"`
	Profiles   []Profile       `json:"profiles"`
	Audit      []Audit         `json:"audit"`
	Persistent bool            `json:"persistent"`
	DataDir    string          `json:"dataDir"`
	Engine     json.RawMessage `json:"engine"`
	SyncedAt   int64           `json:"syncedAt"`
	Warning    string          `json:"warning"`
}
type session struct {
	Engine  *EngineClient
	CSRF    string
	Origin  string
	Expires time.Time
}
type attempt struct {
	Count int
	Until time.Time
}
type App struct {
	cfg          Config
	base         string
	mu           sync.RWMutex
	mutation     sync.Mutex
	syncing      sync.Mutex
	state        Snapshot
	inbounds     []Inbound
	routes       map[string]*httputil.ReverseProxy
	sessions     map[string]session
	attempts     map[string]attempt
	service      *EngineClient
	static       http.Handler
	advancedMu   sync.RWMutex
	vps          VPSConfig
	telegram     TelegramConfig
	telegramHTTP *http.Client
	coreRestart  chan struct{}
	metricsMu    sync.Mutex
	metrics      Metrics
	metricPrev   procSample
}

func NewApp(cfg Config, base string) (*App, error) {
	public := ""
	if cfg.PublicURL != "" {
		u, err := validatePublicURL(cfg.PublicURL)
		if err != nil {
			return nil, err
		}
		public = u.String()
		cfg.PublicURL = public
	}
	sub, _ := fs.Sub(assets, "web")
	a := &App{cfg: cfg, base: base, sessions: map[string]session{}, attempts: map[string]attempt{}, routes: map[string]*httputil.ReverseProxy{}, static: http.FileServer(http.FS(sub))}
	a.state = Snapshot{Brand: "ariaatashin", Version: "2.0.0", Settings: Settings{Brand: "ariaatashin", HostPreset: "auto", PublicURL: public, AutomaticDomain: public == ""}, Users: []User{}, Profiles: append([]Profile{}, profileTemplates...), Audit: []Audit{}, Persistent: cfg.Persistent, DataDir: cfg.DataDir, Engine: json.RawMessage(`{}`)}
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "atia-settings.json")); err == nil {
		var saved diskState
		if err := json.Unmarshal(b, &saved); err != nil {
			return nil, fmt.Errorf("settings file invalid: %w", err)
		}
		if public == "" {
			if _, err := validatePublicURL(saved.Settings.PublicURL); err == nil {
				a.state.Settings.PublicURL = saved.Settings.PublicURL
			}
			// Missing field means the v1 format: migrate it to automatic mode.
			var document struct {
				Settings map[string]json.RawMessage `json:"settings"`
			}
			_ = json.Unmarshal(b, &document)
			if _, exists := document.Settings["automaticDomain"]; exists {
				a.state.Settings.AutomaticDomain = saved.Settings.AutomaticDomain
			}
		}
		a.state.Settings.HostPreset = saved.Settings.HostPreset
		a.state.Audit = saved.Audit
		if len(a.state.Audit) > 80 {
			a.state.Audit = a.state.Audit[:80]
		}
	}
	a.state.Settings.HostPreset = normalizePreset(a.state.Settings.HostPreset)
	a.coreRestart = make(chan struct{}, 1)
	if err := a.loadAdvanced(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) snapshot() Snapshot { a.mu.RLock(); defer a.mu.RUnlock(); return a.state }

func (a *App) persist() error {
	s := a.snapshot()
	b, err := json.MarshalIndent(diskState{s.Settings, s.Audit}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.cfg.DataDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(a.cfg.DataDir, ".atia-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(a.cfg.DataDir, "atia-settings.json"))
}

func (a *App) audit(message, kind string) {
	a.mu.Lock()
	events := append([]Audit{{Time: time.Now().UnixMilli(), Message: message, Kind: kind}}, a.state.Audit...)
	if len(events) > 80 {
		events = events[:80]
	}
	a.state.Audit = events
	a.mu.Unlock()
}

func (a *App) syncLoop(ctx context.Context) {
	for {
		_ = a.refresh(ctx)
		a.mu.Lock()
		now := time.Now()
		for k, s := range a.sessions {
			if s.Expires.Before(now) {
				delete(a.sessions, k)
			}
		}
		for k, t := range a.attempts {
			if t.Until.Before(now) {
				delete(a.attempts, k)
			}
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

func (a *App) refresh(ctx context.Context) error {
	a.syncing.Lock()
	defer a.syncing.Unlock()
	a.mu.RLock()
	e := a.service
	a.mu.RUnlock()
	if e == nil {
		e = newEngineClient(a.base)
		if err := e.login(ctx, a.cfg.AdminUser, a.cfg.AdminPassword, ""); err != nil {
			return err
		}
		a.mu.Lock()
		a.service = e
		a.mu.Unlock()
	}
	all, err := e.inbounds(ctx)
	if err != nil {
		a.mu.Lock()
		a.state.Ready = false
		if errors.Is(err, errUnauthorized) {
			a.service = nil
		}
		a.mu.Unlock()
		return err
	}
	var engine json.RawMessage
	_ = e.call(ctx, "GET", "/panel/api/server/status", nil, &engine)
	if len(engine) == 0 {
		engine = json.RawMessage(`{}`)
	}
	a.publish(all, engine)
	return nil
}

func (a *App) publish(all []Inbound, engine json.RawMessage) {
	origin := a.snapshot().Settings.PublicURL
	users := map[string]*User{}
	profiles := a.profileCatalog()
	routes := map[string]*httputil.ReverseProxy{}
	for _, ib := range all {
		p, managed := inboundProfile(ib)
		if !managed {
			continue
		}
		for i := range profiles {
			if profiles[i].Key == p.Key {
				p.Available, p.Reason = profiles[i].Available, profiles[i].Reason
				profiles[i] = p
			}
		}
		if ib.Enable && !p.Direct {
			routes[p.Path] = makeProxy(ib.Port)
		}
		var settings struct {
			Clients []map[string]any `json:"clients"`
		}
		if nestedJSON(ib.Settings, &settings) != nil {
			continue
		}
		for _, c := range settings.Clients {
			email := textField(c, "email")
			if email == "" {
				continue
			}
			u := users[email]
			if u == nil {
				u = &User{Email: email, Name: textField(c, "comment"), Enabled: boolField(c, "enable"), Quota: numField(c, "totalGB"), Expiry: numField(c, "expiryTime"), SubID: textField(c, "subId"), Links: []Connection{}, Raw: c}
				if u.Name == "" {
					u.Name = email
				}
				users[email] = u
			}
			for _, key := range []string{"id", "password", "auth", "flow"} {
				if value := textField(c, key); value != "" {
					u.Raw[key] = value
				}
			}
			u.Links = append(u.Links, Connection{p.Name, p.Key, shareLink(p, c, origin)})
			for _, s := range ib.Stats {
				if s.Email == email {
					if s.Up > u.Up {
						u.Up = s.Up
					}
					if s.Down > u.Down {
						u.Down = s.Down
					}
					if s.LastOnline > u.LastOnline {
						u.LastOnline = s.LastOnline
					}
				}
			}
		}
	}
	list := make([]User, 0, len(users))
	for _, u := range users {
		u.Status = userStatus(*u)
		if u.SubID != "" {
			u.Subscription = strings.TrimRight(origin, "/") + "/subscription/" + u.SubID
		}
		list = append(list, *u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Email < list[j].Email })
	a.mu.Lock()
	a.state.Users = list
	a.state.Profiles = profiles
	a.state.Ready = true
	a.state.SyncedAt = time.Now().UnixMilli()
	a.state.Engine = engine
	a.inbounds = all
	a.routes = routes
	a.mu.Unlock()
}

func makeProxy(port int) *httputil.ReverseProxy {
	u, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1
	p.Transport = &http.Transport{Proxy: nil, MaxIdleConns: 64, MaxIdleConnsPerHost: 32, IdleConnTimeout: 60 * time.Second}
	old := p.Director
	p.Director = func(r *http.Request) {
		old(r)
		r.Header.Del("Cookie")
		r.Header.Del("Authorization")
		r.Header.Del("X-ATIA-CSRF")
		r.Header.Del("X-ARIA-CSRF")
		r.Header.Del("X-ATIA-Origin")
		r.Header.Del("X-ARIA-Origin")
		r.Header.Del("X-CSRF-Token")
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "connection upstream unavailable", 502)
	}
	return p
}

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, msg string) {
	jsonReply(w, status, map[string]string{"error": msg})
}
func readBody(w http.ResponseWriter, r *http.Request, out any, max int64) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("JSON content-type required")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("one JSON object expected")
	}
	return nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/connect/") || strings.HasPrefix(r.URL.Path, "/xvpnws/") || strings.HasPrefix(r.URL.Path, "/xhttp/") {
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", 405)
			return
		}
		a.mu.RLock()
		var proxy *httputil.ReverseProxy
		longest := 0
		for prefix, p := range a.routes {
			if strings.HasPrefix(r.URL.Path, prefix) && len(prefix) > longest {
				proxy = p
				longest = len(prefix)
			}
		}
		a.mu.RUnlock()
		// Legacy paths still pass through for old inbounds; no admin endpoint is forwarded.
		if proxy == nil && strings.HasPrefix(r.URL.Path, "/xvpnws/") {
			proxy = makeProxy(20868)
		}
		if proxy == nil && strings.HasPrefix(r.URL.Path, "/xhttp/") {
			proxy = makeProxy(20869)
		}
		if proxy == nil {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
	if r.URL.Path == "/health" {
		s := a.snapshot()
		if !s.Ready {
			jsonReply(w, 503, map[string]bool{"ready": false})
		} else {
			jsonReply(w, 200, map[string]bool{"ready": true})
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/subscription/") {
		a.subscription(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		a.api(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Cache-Control", "no-store")
	}
	a.static.ServeHTTP(w, r)
}

func (a *App) api(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	unsafe := r.Method != "GET"
	if unsafe {
		if r.Header.Get("X-Requested-With") != "atiaatashin" && r.Header.Get("X-Requested-With") != "ariaatashin" {
			apiError(w, 403, "درخواست نامعتبر است")
			return
		}
	}
	origin, err := a.requestOrigin(r)
	if err != nil {
		apiError(w, 403, "Origin mismatch")
		return
	}
	if r.URL.Path == "/api/login" && r.Method == "POST" {
		a.login(w, r)
		return
	}
	s, ok := a.getSession(r)
	if !ok {
		apiError(w, 401, "دوباره وارد پنل شو")
		return
	}
	if s.Origin != "" && origin != s.Origin {
		apiError(w, 403, "Session origin mismatch")
		return
	}
	if unsafe && subtle.ConstantTimeCompare([]byte(csrfHeader(r)), []byte(s.CSRF)) != 1 {
		apiError(w, 403, "Session CSRF mismatch")
		return
	}
	if r.URL.Path == "/api/state" && (r.Header.Get("X-ATIA-Origin") != "" || r.Header.Get("X-ARIA-Origin") != "") {
		if err := a.learnPublicOrigin(origin); err != nil {
			apiError(w, 500, "ذخیرهٔ آدرس خودکار انجام نشد؛ دسترسی پوشهٔ داده را بررسی کن")
			return
		}
	}
	switch {
	case r.URL.Path == "/api/session" && r.Method == "GET":
		jsonReply(w, 200, map[string]any{"csrf": s.CSRF, "username": a.cfg.AdminUser})
	case r.URL.Path == "/api/logout" && r.Method == "POST":
		cookie, _ := r.Cookie("atia_session")
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "atia_session", Value: "", Path: "/", HttpOnly: true, Secure: strings.HasPrefix(origin, "https:"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
		jsonReply(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/api/state" && r.Method == "GET":
		jsonReply(w, 200, a.runtimeSnapshot())
	case r.URL.Path == "/api/users" && r.Method == "POST":
		a.createUsers(w, r, s.Engine)
	case strings.HasPrefix(r.URL.Path, "/api/users/") && r.Method == "POST":
		a.changeUser(w, r, s.Engine)
	case r.URL.Path == "/api/settings" && r.Method == "POST":
		a.saveSettings(w, r)
	case r.URL.Path == "/api/backup" && r.Method == "GET":
		a.backup(w, r)
	case r.URL.Path == "/api/restore" && r.Method == "POST":
		a.restore(w, r, s.Engine)
	case r.URL.Path == "/api/vps" && r.Method == "POST":
		a.saveVPS(w, r)
	case r.URL.Path == "/api/telegram" && r.Method == "POST":
		a.saveTelegram(w, r)
	case r.URL.Path == "/api/telegram/test" && r.Method == "POST":
		a.testTelegram(w, r)
	case r.URL.Path == "/api/client-export" && r.Method == "GET":
		a.clientExport(w, r)
	case r.URL.Path == "/api/diagnostics" && r.Method == "GET":
		a.diagnostics(w, r, s.Engine)
	default:
		apiError(w, 404, "مسیر API یافت نشد")
	}
}

func (a *App) getSession(r *http.Request) (session, bool) {
	c, err := r.Cookie("atia_session")
	if err != nil {
		return session{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.sessions[c.Value]
	return s, ok && s.Expires.After(time.Now())
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	origin, err := a.requestOrigin(r)
	if err != nil {
		apiError(w, 403, "Origin mismatch")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if readBody(w, r, &input, 4096) != nil {
		apiError(w, 400, "فرم ورود معتبر نیست")
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.mu.Lock()
	att := a.attempts[host]
	if att.Until.Before(time.Now()) {
		att = attempt{Until: time.Now().Add(10 * time.Minute)}
	}
	att.Count++
	a.attempts[host] = att
	a.mu.Unlock()
	if att.Count > 20 {
		w.Header().Set("Retry-After", "600")
		apiError(w, 429, "تلاش‌های ورود زیاد است؛ چند دقیقه صبر کن")
		return
	}
	e := newEngineClient(a.base)
	if input.Username != a.cfg.AdminUser || len(input.Password) > 256 || e.login(r.Context(), input.Username, input.Password, input.OTP) != nil {
		apiError(w, 401, "نام کاربری یا رمز اشتباه است، یا هسته هنوز آماده نیست")
		return
	}
	if err := a.learnPublicOrigin(origin); err != nil {
		apiError(w, 500, "ذخیرهٔ آدرس خودکار انجام نشد؛ دسترسی پوشهٔ داده را بررسی کن")
		return
	}
	token, csrf := randomToken(32), randomToken(24)
	a.mu.Lock()
	if len(a.sessions) > 256 {
		a.mu.Unlock()
		apiError(w, 429, "تعداد نشست‌ها زیاد است")
		return
	}
	a.sessions[token] = session{Engine: e, CSRF: csrf, Origin: origin, Expires: time.Now().Add(12 * time.Hour)}
	// An interactive OTP login can also establish the background API session.
	a.service = e
	delete(a.attempts, host)
	a.mu.Unlock()
	_ = a.refresh(r.Context())
	http.SetCookie(w, &http.Cookie{Name: "atia_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(origin, "https:"), SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	jsonReply(w, 200, map[string]any{"csrf": csrf, "username": input.Username})
}

func (a *App) ensureProfile(ctx context.Context, e *EngineClient, key string) (Profile, error) {
	return a.ensureProfilePath(ctx, e, key, "")
}

func (a *App) ensureProfilePath(ctx context.Context, e *EngineClient, key, savedPath string) (Profile, error) {
	if isDirectKey(key) {
		return a.ensureDirectProfile(ctx, e, key, savedPath)
	}
	all, err := e.inbounds(ctx)
	if err != nil {
		return Profile{}, err
	}
	used := map[int]bool{panelPort: true, a.cfg.Port: true}
	for _, ib := range all {
		used[ib.Port] = true
		if p, ok := inboundProfile(ib); ok && p.Key == key {
			if !ib.Enable {
				return p, fmt.Errorf("اینباند %s خاموش است", key)
			}
			return p, nil
		}
	}
	var p Profile
	for _, tpl := range profileTemplates {
		if key == tpl.Key {
			p = tpl
		}
	}
	if p.Key == "" {
		return p, fmt.Errorf("unknown profile")
	}
	for used[p.Port] {
		p.Port++
		if p.Port > 30000 {
			return p, fmt.Errorf("no internal port available")
		}
	}
	p.Path = "/connect/" + p.Key + "-" + randomToken(6) + "/"
	if savedPath != "" {
		p.Path = savedPath
	}
	stream := map[string]any{"network": p.Network, "security": "none", p.Network + "Settings": map[string]any{"path": p.Path, "headers": map[string]any{}}}
	if p.Network == "xhttp" {
		stream["xhttpSettings"] = map[string]any{"path": p.Path, "mode": "auto"}
	}
	settings := map[string]any{"clients": []any{}, "decryption": "none", "fallbacks": []any{}}
	host, _, _ := publicEndpoint(a.snapshot().Settings.PublicURL)
	body := map[string]any{"remark": "aria:" + key, "enable": true, "listen": "127.0.0.1", "port": p.Port, "protocol": p.Protocol, "settings": marshalString(settings), "streamSettings": marshalString(stream), "sniffing": marshalString(map[string]any{"enabled": true, "destOverride": []string{"http", "tls"}, "routeOnly": false}), "up": 0, "down": 0, "total": 0, "expiryTime": 0, "trafficReset": "never", "shareAddrStrategy": "custom", "shareAddr": host}
	var added Inbound
	if err := e.call(ctx, "POST", "/panel/api/inbounds/add", body, &added); err != nil {
		return p, err
	}
	p.InboundID = added.ID
	p.Ready = true
	return p, nil
}

func (a *App) createUsers(w http.ResponseWriter, r *http.Request, e *EngineClient) {
	var req CreateRequest
	if readBody(w, r, &req, 8192) != nil {
		apiError(w, 400, "فرم ساخت معتبر نیست")
		return
	}
	if err := validateCreate(req); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	a.mutation.Lock()
	defer a.mutation.Unlock()
	known, err := e.clientEmails(r.Context())
	if err != nil {
		apiError(w, 502, err.Error())
		return
	}
	ids := []int{}
	for _, key := range req.Profiles {
		p, err := a.ensureProfile(r.Context(), e, key)
		if err != nil {
			apiError(w, 502, err.Error())
			return
		}
		ids = append(ids, p.InboundID)
	}
	created := []string{}
	var warning string
	for i := 0; i < req.Count; i++ {
		name := strings.TrimSpace(req.Name)
		if req.Count > 1 {
			name += " " + strconv.Itoa(i+1)
		}
		email := "aria-" + randomToken(6)
		for known[email] {
			email = "aria-" + randomToken(6)
		}
		known[email] = true
		client := map[string]any{"id": newUUID(), "password": randomToken(20), "email": email, "subId": randomToken(16), "enable": true, "security": "auto", "auth": randomToken(20), "flow": clientFlow(req.Profiles), "totalGB": int64(req.QuotaGB * float64(int64(1)<<30)), "expiryTime": int64(0), "limitIp": 0, "comment": name, "tgId": 0, "reset": 0, "trafficReset": "never"}
		if req.Days > 0 {
			client["expiryTime"] = time.Now().Add(time.Duration(req.Days) * 24 * time.Hour).UnixMilli()
		}
		if err := e.call(r.Context(), "POST", "/panel/api/clients/add", map[string]any{"client": client, "inboundIds": ids}, nil); err != nil {
			_ = e.clientCall(r.Context(), "del", email, nil)
			warning = err.Error()
			break
		}
		created = append(created, email)
	}
	if len(created) > 0 {
		a.audit(fmt.Sprintf("%d کاربر جدید ساخته شد؛ لینک‌ها خودکار آماده‌اند", len(created)), "create")
	}
	if err := a.refresh(r.Context()); err != nil {
		warning += "؛ همگام‌سازی هنوز کامل نیست"
	}
	if err := a.persist(); err != nil {
		warning += "؛ ذخیرهٔ فعالیت‌ها انجام نشد"
	}
	jsonReply(w, 200, map[string]any{"created": created, "warning": warning, "state": a.runtimeSnapshot()})
}

func (a *App) findUser(email string) (User, bool) {
	s := a.snapshot()
	for _, u := range s.Users {
		if u.Email == email {
			return u, true
		}
	}
	return User{}, false
}

func (a *App) changeUser(w http.ResponseWriter, r *http.Request, e *EngineClient) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/users/"), "/")
	if len(parts) != 2 {
		apiError(w, 404, "کاربر یافت نشد")
		return
	}
	email, verb := parts[0], parts[1]
	a.mutation.Lock()
	defer a.mutation.Unlock()
	u, ok := a.findUser(email)
	if !ok {
		apiError(w, 404, "کاربر یافت نشد")
		return
	}
	c := map[string]any{}
	for k, v := range u.Raw {
		c[k] = v
	}
	var err error
	switch verb {
	case "toggle":
		c["enable"] = !u.Enabled
		err = e.clientCall(r.Context(), "update", email, c)
	case "delete":
		err = e.clientCall(r.Context(), "del", email, nil)
	case "rotate":
		c["id"] = newUUID()
		c["password"] = randomToken(20)
		c["auth"] = randomToken(20)
		c["subId"] = randomToken(16)
		err = e.clientCall(r.Context(), "update", email, c)
	case "reset":
		err = e.clientCall(r.Context(), "resetTraffic", email, nil)
	case "edit":
		var input struct {
			Name    string  `json:"name"`
			QuotaGB float64 `json:"quotaGB"`
			Days    int     `json:"days"`
		}
		if readBody(w, r, &input, 4096) != nil || validateCreate(CreateRequest{Name: input.Name, QuotaGB: input.QuotaGB, Days: input.Days, Count: 1, Profiles: []string{"vless-ws"}}) != nil {
			apiError(w, 400, "مقادیر ویرایش معتبر نیست")
			return
		}
		c["comment"] = strings.TrimSpace(input.Name)
		c["totalGB"] = int64(input.QuotaGB * float64(int64(1)<<30))
		c["expiryTime"] = int64(0)
		if input.Days > 0 {
			c["expiryTime"] = time.Now().Add(time.Duration(input.Days) * 24 * time.Hour).UnixMilli()
		}
		err = e.clientCall(r.Context(), "update", email, c)
	default:
		apiError(w, 404, "عملیات نامعتبر است")
		return
	}
	if err != nil {
		apiError(w, 502, err.Error())
		return
	}
	labels := map[string]string{"toggle": "وضعیت تغییر کرد", "delete": "حذف شد", "rotate": "کلیدها تعویض شد", "reset": "مصرف صفر شد", "edit": "سهمیه و اعتبار ویرایش شد"}
	a.audit("کاربر «"+u.Name+"»: "+labels[verb], "update")
	_ = a.refresh(r.Context())
	_ = a.persist()
	jsonReply(w, 200, a.runtimeSnapshot())
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var input Settings
	if readBody(w, r, &input, 4096) != nil {
		apiError(w, 400, "تنظیمات معتبر نیست")
		return
	}
	if input.AutomaticDomain {
		origin, err := a.requestOrigin(r)
		if err != nil {
			apiError(w, 400, "آدرس مرورگر معتبر نیست")
			return
		}
		input.PublicURL = origin
	}
	u, err := validatePublicURL(input.PublicURL)
	if err != nil {
		apiError(w, 400, "فقط آدرس عمومی بدون مسیر را بنویس")
		return
	}
	if a.cfg.PublicURL != "" && (input.AutomaticDomain || u.String() != a.cfg.PublicURL) {
		apiError(w, 409, "برای تشخیص خودکار یا تغییر دامنه، ATIA_PUBLIC_URL را از تنظیمات میزبان حذف کن")
		return
	}
	if !validPreset(input.HostPreset) || (input.HostPreset == "vps" && a.cfg.Mode != "vps") {
		apiError(w, 400, "پروفایل میزبان با حالت اجرا سازگار نیست")
		return
	}
	a.mutation.Lock()
	defer a.mutation.Unlock()
	a.syncing.Lock()
	defer a.syncing.Unlock()
	old := a.snapshot().Settings
	a.mu.Lock()
	a.state.Settings.PublicURL = u.String()
	a.state.Settings.AutomaticDomain = input.AutomaticDomain
	a.state.Settings.HostPreset = normalizePreset(input.HostPreset)
	all := a.inbounds
	engine := a.state.Engine
	a.mu.Unlock()
	if err := a.persist(); err != nil {
		a.mu.Lock()
		a.state.Settings = old
		a.mu.Unlock()
		apiError(w, 500, "ذخیره تنظیمات انجام نشد")
		return
	}
	a.publish(all, engine)
	a.audit("حالت آدرس عمومی به‌روزرسانی شد؛ همهٔ لینک‌ها دوباره ساخته شدند", "settings")
	_ = a.persist()
	jsonReply(w, 200, a.runtimeSnapshot())
}

type Backup struct {
	VPS      *VPSConfig   `json:"vps,omitempty"`
	Version  int          `json:"version"`
	Brand    string       `json:"brand"`
	Created  int64        `json:"created"`
	Settings Settings     `json:"settings"`
	Users    []BackupUser `json:"users"`
	Profiles []Profile    `json:"profiles"`
}
type BackupUser struct {
	Client   map[string]any `json:"client"`
	Profiles []string       `json:"profiles"`
	Up       int64          `json:"up"`
	Down     int64          `json:"down"`
}

func (a *App) backup(w http.ResponseWriter, r *http.Request) {
	s := a.snapshot()
	if !s.Ready || time.Now().UnixMilli()-s.SyncedAt > 60000 {
		apiError(w, 503, "هسته همگام نیست؛ بکاپ ناقص دانلود نمی‌شود")
		return
	}
	b := Backup{Version: 1, Brand: "ariaatashin", Created: time.Now().UnixMilli(), Settings: s.Settings, Users: []BackupUser{}, Profiles: append([]Profile{}, s.Profiles...)}
	if a.cfg.Mode == "vps" {
		v := a.vpsSnapshot()
		b.VPS = &v
	}
	for _, u := range s.Users {
		p := []string{}
		for _, l := range u.Links {
			p = append(p, l.Profile)
		}
		b.Users = append(b.Users, BackupUser{Client: u.Raw, Profiles: p, Up: u.Up, Down: u.Down})
	}
	w.Header().Set("Content-Disposition", `attachment; filename="ariaatashin-backup.json"`)
	jsonReply(w, 200, b)
}

func (a *App) restore(w http.ResponseWriter, r *http.Request, e *EngineClient) {
	var backup Backup
	if readBody(w, r, &backup, 4<<20) != nil || backup.Version != 1 || (backup.Brand != "atiaatashin" && backup.Brand != "ariaatashin") || len(backup.Users) > 1000 {
		apiError(w, 400, "بکاپ معتبر atiaatashin را انتخاب کن")
		return
	}
	seen := map[string]bool{}
	paths := map[string]string{}
	for _, p := range backup.Profiles {
		if p.Path == "" {
			continue
		}
		if !regexp.MustCompile(`^/connect/[a-z0-9-]{8,80}/$`).MatchString(p.Path) {
			apiError(w, 400, "مسیر اتصال در بکاپ معتبر نیست")
			return
		}
		paths[p.Key] = p.Path
	}
	for _, u := range backup.Users {
		email := textField(u.Client, "email")
		if seen[email] || !regexp.MustCompile(`^(atia|aria)-[a-f0-9]{12}$`).MatchString(email) || !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`).MatchString(textField(u.Client, "id")) || !regexp.MustCompile(`^[a-f0-9]{32,128}$`).MatchString(textField(u.Client, "subId")) || !regexp.MustCompile(`^[a-f0-9]{24,128}$`).MatchString(textField(u.Client, "password")) || numField(u.Client, "totalGB") < 0 || numField(u.Client, "totalGB") > 1e6*(1<<30) || u.Up < 0 || u.Down < 0 || u.Up > 1<<60 || u.Down > 1<<60 || validateCreate(CreateRequest{Name: textField(u.Client, "comment"), Count: 1, Profiles: u.Profiles}) != nil {
			apiError(w, 400, "رکورد نامعتبر در بکاپ")
			return
		}
		seen[email] = true
	}
	a.mutation.Lock()
	defer a.mutation.Unlock()
	count, skipped := 0, 0
	warning := ""
	known, err := e.clientEmails(r.Context())
	if err != nil {
		apiError(w, 502, err.Error())
		return
	}
	if err := a.restoreVPS(backup); err != nil {
		apiError(w, 409, err.Error())
		return
	}
	for _, u := range backup.Users {
		if known[textField(u.Client, "email")] {
			skipped++
			continue
		}
		ids := []int{}
		for _, key := range u.Profiles {
			p, err := a.ensureProfilePath(r.Context(), e, key, paths[key])
			if err != nil {
				warning = err.Error()
				break
			}
			ids = append(ids, p.InboundID)
		}
		if warning != "" {
			break
		}
		if err := e.call(r.Context(), "POST", "/panel/api/clients/add", map[string]any{"client": u.Client, "inboundIds": ids}, nil); err != nil {
			_ = e.clientCall(r.Context(), "del", textField(u.Client, "email"), nil)
			warning = err.Error()
			break
		}
		count++
		known[textField(u.Client, "email")] = true
		if u.Up > 0 || u.Down > 0 {
			if err := e.clientCall(r.Context(), "updateTraffic", textField(u.Client, "email"), map[string]int64{"upload": u.Up, "download": u.Down}); err != nil {
				warning = "کاربر بازیابی شد اما مصرفش منتقل نشد: " + err.Error()
				break
			}
		}
	}
	_ = a.refresh(r.Context())
	a.audit(fmt.Sprintf("بازیابی بکاپ: %d کاربر افزوده شد؛ %d تکراری رد شد", count, skipped), "backup")
	_ = a.persist()
	jsonReply(w, 200, map[string]any{"restored": count, "skipped": skipped, "warning": warning, "state": a.runtimeSnapshot()})
}

func (a *App) subscription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", 405)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/subscription/")
	if len(token) < 16 || len(token) > 128 || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	s := a.snapshot()
	if !s.Ready || s.Settings.PublicURL == "" || time.Now().UnixMilli()-s.SyncedAt > 60000 {
		http.Error(w, "subscription temporarily unavailable", 503)
		return
	}
	for _, u := range s.Users {
		if subtle.ConstantTimeCompare([]byte(token), []byte(u.SubID)) != 1 {
			continue
		}
		if userStatus(u) != "active" {
			http.Error(w, "subscription inactive", 403)
			return
		}
		links := []string{}
		for _, l := range u.Links {
			links = append(links, l.Link)
		}
		body := strings.Join(links, "\n")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("subscription-userinfo", fmt.Sprintf("upload=%d; download=%d; total=%d; expire=%d", u.Up, u.Down, u.Quota, u.Expiry/1000))
		w.Header().Set("profile-update-interval", "6")
		w.Header().Set("profile-title", "base64:"+base64.StdEncoding.EncodeToString([]byte("ariaatashin · "+u.Name)))
		if r.URL.Query().Get("format") != "plain" {
			body = base64.StdEncoding.EncodeToString([]byte(body))
		}
		if r.Method != "HEAD" {
			_, _ = io.WriteString(w, body)
		}
		return
	}
	http.NotFound(w, r)
}

func (a *App) diagnostics(w http.ResponseWriter, r *http.Request, e *EngineClient) {
	s := a.snapshot()
	items := []map[string]any{{"label": "هستهٔ مدیریت", "ok": s.Ready, "detail": "3x-ui " + engineVersion}, {"label": "دامنهٔ خروجی لینک‌ها", "ok": s.Settings.PublicURL != "", "detail": s.Settings.PublicURL}, {"label": "ذخیره‌سازی پایدار", "ok": s.Persistent, "detail": "پوشهٔ داده: " + s.DataDir + "؛ حفظ Volume در Restart/Redeploy را از میزبان بررسی کن"}, {"label": "تعداد Replica موردنیاز", "ok": true, "detail": "این پنل باید روی یک Replica اجرا شود"}}
	for _, p := range s.Profiles {
		if !p.Ready {
			continue
		}
		ok := false
		if p.UDP {
			ok = udpSocketBound(p.Port)
		} else {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(p.Port), 2*time.Second)
			ok = err == nil
			if conn != nil {
				conn.Close()
			}
		}
		items = append(items, map[string]any{"label": p.Name, "ok": ok, "detail": fmt.Sprintf("127.0.0.1:%d → %s", p.Port, p.Path)})
	}
	var engine json.RawMessage
	_ = e.call(r.Context(), "GET", "/panel/api/server/status", nil, &engine)
	jsonReply(w, 200, map[string]any{"checks": items, "engine": engine, "note": "این تست فقط شنود پورت داخلی را بررسی می‌کند؛ اتصال اینترنت گوشی یا اجازهٔ خروجی هاست را تضمین نمی‌کند."})
}
