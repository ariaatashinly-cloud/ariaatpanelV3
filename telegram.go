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
	Token         string  "json:\"token\""
	ChatID        int64   "json:\"chatId\""
	Enabled       bool    "json:\"enabled\""
	Notify        bool    "json:\"notify\""
	Offset        int64   "json:\"offset\""
	Seen          []int64 `json:"seen,omitempty"`
	Mode          string  `json:"mode"`
	WebhookSecret string  `json:"webhookSecret,omitempty"`
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
		ID        int64  "json:\"id\""
		IsBot     bool   "json:\"is_bot\""
		FirstName string `json:"first_name"`
	} "json:\"from\""
	Photo []struct {
		FileID string `json:"file_id"`
	} `json:"photo"`
	Caption string `json:"caption"`
	Payment *struct {
		Currency string `json:"currency"`
		Amount   int64  `json:"total_amount"`
		Payload  string `json:"invoice_payload"`
		Charge   string `json:"telegram_payment_charge_id"`
	} `json:"successful_payment"`
}
type tgUpdate struct {
	ID       int64     "json:\"update_id\""
	Message  tgMessage "json:\"message\""
	Callback *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From struct {
			ID   int64  `json:"id"`
			Bot  bool   `json:"is_bot"`
			Name string `json:"first_name"`
		} `json:"from"`
		Message tgMessage `json:"message"`
	} `json:"callback_query"`
	Checkout *struct {
		ID   string `json:"id"`
		From struct {
			ID  int64 `json:"id"`
			Bot bool  `json:"is_bot"`
		} `json:"from"`
		Currency string `json:"currency"`
		Amount   int64  `json:"total_amount"`
		Payload  string `json:"invoice_payload"`
	} `json:"pre_checkout_query"`
}

func (a *App) telegramSnapshot() TelegramConfig {
	a.advancedMu.RLock()
	defer a.advancedMu.RUnlock()
	t := a.telegram
	t.Seen = append([]int64{}, t.Seen...)
	return t
}
func (a *App) publicTelegram() any {
	t := a.telegramSnapshot()
	return map[string]any{"enabled": t.Enabled, "notify": t.Notify, "chatId": t.ChatID, "tokenConfigured": t.Token != "", "mode": t.Mode, "commands": []string{"/start", "/plans", "/services", "/wallet", "/shop", "/support", "/status", "/approve"}}
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
	in.Mode = old.Mode
	in.WebhookSecret = old.WebhookSecret
	in.Seen = old.Seen
	if !validTelegram(in) {
		apiError(w, 400, "توکن معتبر و شناسهٔ عددی مثبت چت خصوصی لازم است")
		return
	}
	if in.Token != old.Token || in.ChatID != old.ChatID {
		in.Offset = 0
		in.Mode = "poll"
		in.WebhookSecret = ""
		in.Seen = nil
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
	payload := map[string]any{"chat_id": t.ChatID, "text": text, "protect_content": true, "link_preview_options": map[string]bool{"is_disabled": true}}
	if a.salesSnapshot().Settings.Enabled {
		payload["reply_markup"] = map[string]any{"keyboard": [][]map[string]string{{{"text": "/plans"}, {"text": "/services"}}, {{"text": "/wallet"}, {"text": "/shop"}}, {{"text": "/help"}}}, "resize_keyboard": true, "is_persistent": true, "input_field_placeholder": "پلن، سرویس یا کیف پول را انتخاب کن"}
		// A normal URL button also works when Telegram Mini Apps or third-party
		// cookies are unavailable; the fragment token is exchanged once by /shop.
		if p := strings.LastIndex(text, "\nhttps://"); p >= 0 && strings.Contains(text[p+1:], "/shop#access=") {
			payload["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "بازکردن فروشگاه خصوصی ↗", "url": text[p+1:]}}}}
		}
	}
	return a.tgCall(ctx, t, "sendMessage", payload, nil)
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
		return fmt.Sprintf("ariaatashin 3.0\nهستهٔ مدیریت آماده: %t\nکاربران: %d\nآدرس: %s\nدادهٔ پایدار: %t", s.Ready, len(s.Users), s.Settings.PublicURL, s.Persistent)
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
		if t.Enabled && t.Token != "" && t.Mode != "webhook" {
			var updates []tgUpdate
			err := a.tgCall(ctx, t, "getUpdates", map[string]any{"offset": t.Offset, "timeout": 15, "limit": 20, "allowed_updates": []string{"message", "callback_query", "pre_checkout_query"}}, &updates)
			if err == nil {
				for _, u := range updates {
					if u.ID < t.Offset {
						continue
					}
					if err := a.processUpdate(ctx, t, u); err != nil {
						break
					}
				}
			}
		}
		if t.Enabled && t.Notify {
			for _, ev := range a.snapshot().Audit {
				if ev.Time > lastAudit {
					_ = a.queueBot(fmt.Sprintf("audit-%d", ev.Time), t.ChatID, "ariaatashin\n"+ev.Message)
				}
			}
			lastAudit = time.Now().UnixMilli()
		}
		a.flushSalesNotices(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
func telegramConfigPath(dir string) string { return filepath.Join(dir, "aria-telegram.json") }
