package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type TelegramConfig struct {
	Token   string "json:\"token\""
	ChatID  int64  "json:\"chatId\""
	Enabled bool   "json:\"enabled\""
	Notify  bool   "json:\"notify\""
	Offset  int64  "json:\"offset\""
}
type tgMessage struct {
	MessageID int64  "json:\"message_id\""
	Date      int64  "json:\"date\""
	Text      string "json:\"text\""
	Chat      struct {
		ID   int64  "json:\"id\""
		Type string "json:\"type\""
	} "json:\"chat\""
	From struct {
		ID    int64 "json:\"id\""
		IsBot bool  "json:\"is_bot\""
	} "json:\"from\""
}
type tgUpdate struct {
	ID      int64     "json:\"update_id\""
	Message tgMessage "json:\"message\""
}

func (a *App) telegramSnapshot() TelegramConfig {
	a.advancedMu.RLock()
	defer a.advancedMu.RUnlock()
	return a.telegram
}
func (a *App) publicTelegram() any {
	t := a.telegramSnapshot()
	return map[string]any{"enabled": t.Enabled, "notify": t.Notify, "chatId": t.ChatID, "tokenConfigured": t.Token != "", "commands": []string{"/help", "/status", "/users", "/sub aria-…"}}
}
func validTelegram(t TelegramConfig) bool {
	return (!t.Enabled || t.Token != "" && t.ChatID > 0) && t.ChatID >= 0 && (t.Token == "" || regexp.MustCompile("^[0-9]{6,16}:[A-Za-z0-9_-]{30,80}$").MatchString(t.Token))
}
func (a *App) saveTelegram(w http.ResponseWriter, r *http.Request) {
	var in TelegramConfig
	if readBody(w, r, &in, 4096) != nil {
		apiError(w, 400, "تنظیمات ربات معتبر نیست")
		return
	}
	a.mutation.Lock()
	defer a.mutation.Unlock()
	old := a.telegramSnapshot()
	in.Token = strings.TrimSpace(in.Token)
	if in.Token == "" {
		in.Token = old.Token
	}
	in.Offset = old.Offset
	if !validTelegram(in) {
		apiError(w, 400, "توکن معتبر و شناسهٔ عددی مثبت چت خصوصی لازم است")
		return
	}
	if in.Token != old.Token || in.ChatID != old.ChatID {
		in.Offset = 0
	}
	a.advancedMu.Lock()
	if in.Token == a.telegram.Token && in.ChatID == a.telegram.ChatID {
		in.Offset = a.telegram.Offset
	} else {
		in.Offset = 0
	}
	if err := atomicJSON(a.cfg.DataDir, "aria-telegram.json", in); err != nil {
		a.advancedMu.Unlock()
		apiError(w, 500, "تنظیمات ربات ذخیره نشد")
		return
	}
	a.telegram = in
	a.advancedMu.Unlock()
	a.audit("تنظیمات ربات تلگرام ذخیره شد", "settings")
	_ = a.persist()
	jsonReply(w, 200, a.runtimeSnapshot())
}
func (a *App) tgCall(ctx context.Context, t TelegramConfig, method string, payload any, out any) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+t.Token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("درخواست تلگرام معتبر نیست")
	}
	req.Header.Set("Content-Type", "application/json")
	cl := a.telegramHTTP
	if cl == nil {
		cl = &http.Client{Timeout: 25 * time.Second, CheckRedirect: noRedirect}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("ارتباط با Telegram Bot API برقرار نشد")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Telegram Bot API درخواست را نپذیرفت")
	}
	var e struct {
		OK     bool            "json:\"ok\""
		Result json.RawMessage "json:\"result\""
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e) != nil || !e.OK {
		return fmt.Errorf("پاسخ Telegram Bot API موفق نبود")
	}
	if out != nil {
		return json.Unmarshal(e.Result, out)
	}
	return nil
}
func (a *App) tgSend(ctx context.Context, t TelegramConfig, text string) error {
	if t.ChatID <= 0 {
		return fmt.Errorf("شناسهٔ خصوصی لازم است")
	}
	if len([]rune(text)) > 4000 {
		text = string([]rune(text)[:4000])
	}
	return a.tgCall(ctx, t, "sendMessage", map[string]any{"chat_id": t.ChatID, "text": text, "protect_content": true, "link_preview_options": map[string]bool{"is_disabled": true}}, nil)
}
func (a *App) testTelegram(w http.ResponseWriter, r *http.Request) {
	t := a.telegramSnapshot()
	if t.Token == "" || t.ChatID <= 0 {
		apiError(w, 400, "اول توکن و Chat ID را ذخیره کن")
		return
	}
	var me struct {
		Username string "json:\"username\""
	}
	if err := a.tgCall(r.Context(), t, "getMe", map[string]any{}, &me); err != nil {
		apiError(w, 502, err.Error())
		return
	}
	if err := a.tgSend(r.Context(), t, "🔥 ariaatashin\nارتباط خصوصی ربات برقرار است. /help را بفرست."); err != nil {
		apiError(w, 502, err.Error())
		return
	}
	jsonReply(w, 200, map[string]any{"ok": true, "username": me.Username})
}
func (a *App) telegramCommand(t TelegramConfig, m tgMessage) string {
	if !t.Enabled || m.Chat.Type != "private" || m.Chat.ID != t.ChatID || m.From.ID != t.ChatID || m.From.IsBot || m.Date < time.Now().Add(-5*time.Minute).Unix() || m.Date > time.Now().Add(time.Minute).Unix() {
		return ""
	}
	parts := strings.Fields(m.Text)
	if len(parts) == 0 {
		return ""
	}
	cmd := strings.Split(parts[0], "@")[0]
	s := a.snapshot()
	switch cmd {
	case "/start", "/help":
		return "🔥 ariaatashin\n/status وضعیت هسته\n/users فهرست کاربران\n/sub aria-… اشتراک کاربر\nتغییرات کاربران از پنل انجام می‌شود."
	case "/status":
		return fmt.Sprintf("ariaatashin 2.0\nهستهٔ مدیریت آماده: %t\nکاربران: %d\nآدرس: %s\nدادهٔ پایدار: %t", s.Ready, len(s.Users), s.Settings.PublicURL, s.Persistent)
	case "/users":
		lines := []string{"کاربران ariaatashin:"}
		for i, u := range s.Users {
			if i >= 35 {
				lines = append(lines, "بقیهٔ کاربران را در پنل ببین.")
				break
			}
			lines = append(lines, u.Name+" · "+u.Status+"\n"+u.Email)
		}
		return strings.Join(lines, "\n")
	case "/sub":
		if len(parts) != 2 {
			return "شناسهٔ کاربر را بنویس: /sub aria-…"
		}
		if !s.Ready || time.Now().UnixMilli()-s.SyncedAt > 60000 {
			return "هسته همگام نیست؛ دوباره تلاش کن."
		}
		for _, u := range s.Users {
			if u.Email == parts[1] && userStatus(u) == "active" {
				return u.Name + "\n" + u.Subscription
			}
		}
		return "کاربر فعال یافت نشد."
	}
	return ""
}
func (a *App) telegramLoop(ctx context.Context) {
	lastAudit := time.Now().UnixMilli()
	for {
		t := a.telegramSnapshot()
		if t.Enabled && t.Token != "" && t.ChatID > 0 {
			var updates []tgUpdate
			err := a.tgCall(ctx, t, "getUpdates", map[string]any{"offset": t.Offset, "timeout": 15, "limit": 20, "allowed_updates": []string{"message"}}, &updates)
			if err == nil {
				for _, u := range updates {
					current := a.telegramSnapshot()
					if current.Token != t.Token || current.ChatID != t.ChatID || !current.Enabled {
						break
					}
					if u.ID < t.Offset {
						continue
					}
					a.advancedMu.Lock()
					next := a.telegram
					next.Offset = u.ID + 1
					if a.telegram.Token != t.Token || a.telegram.ChatID != t.ChatID || !a.telegram.Enabled {
						a.advancedMu.Unlock()
						break
					}
					if atomicJSON(a.cfg.DataDir, "aria-telegram.json", next) != nil {
						a.advancedMu.Unlock()
						break
					}
					a.telegram.Offset = next.Offset
					a.advancedMu.Unlock()
					t.Offset = next.Offset
					if reply := a.telegramCommand(t, u.Message); reply != "" {
						_ = a.tgSend(ctx, t, reply)
					}
				}
			}
			current := a.telegramSnapshot()
			if current.Notify && current.Enabled && current.Token == t.Token && current.ChatID == t.ChatID {
				events := a.snapshot().Audit
				for i := len(events) - 1; i >= 0; i-- {
					ev := events[i]
					if ev.Time > lastAudit {
						_ = a.tgSend(ctx, current, "🔥 ariaatashin\n"+ev.Message)
						lastAudit = ev.Time
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
func telegramConfigPath(dir string) string { return filepath.Join(dir, "aria-telegram.json") }
