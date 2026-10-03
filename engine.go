package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

var errUnauthorized = errors.New("engine session expired")

type EngineClient struct {
	base string
	http *http.Client
	csrf string
	mu   sync.Mutex
}

func newEngineClient(base string) *EngineClient {
	jar, _ := cookiejar.New(nil)
	return &EngineClient{base: strings.TrimRight(base, "/"), http: &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

type engineResponse struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

func (e *EngineClient) call(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	e.mu.Lock()
	token := e.csrf
	e.mu.Unlock()
	if token != "" {
		req.Header.Set("X-CSRF-Token", token)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("engine is unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return errUnauthorized
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("engine API HTTP %d", resp.StatusCode)
	}
	var envelope engineResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("invalid engine response: %w", err)
	}
	if !envelope.Success {
		return fmt.Errorf("engine: %s", envelope.Msg)
	}
	if out != nil && len(envelope.Obj) > 0 && string(envelope.Obj) != "null" {
		return json.Unmarshal(envelope.Obj, out)
	}
	return nil
}

func (e *EngineClient) login(ctx context.Context, user, password, otp string) error {
	var token string
	if err := e.call(ctx, "GET", "/csrf-token", nil, &token); err != nil {
		return err
	}
	e.mu.Lock()
	e.csrf = token
	e.mu.Unlock()
	if err := e.call(ctx, "POST", "/login", map[string]string{"username": user, "password": password, "twoFactorCode": otp}, nil); err != nil {
		return err
	}
	if err := e.call(ctx, "GET", "/csrf-token", nil, &token); err != nil {
		return err
	}
	e.mu.Lock()
	e.csrf = token
	e.mu.Unlock()
	return nil
}

type Inbound struct {
	ID       int             `json:"id"`
	Remark   string          `json:"remark"`
	Listen   string          `json:"listen"`
	Port     int             `json:"port"`
	Protocol string          `json:"protocol"`
	Enable   bool            `json:"enable"`
	Settings json.RawMessage `json:"settings"`
	Stream   json.RawMessage `json:"streamSettings"`
	Stats    []Traffic       `json:"clientStats"`
}

type Traffic struct {
	Email      string `json:"email"`
	Up         int64  `json:"up"`
	Down       int64  `json:"down"`
	LastOnline int64  `json:"lastOnline"`
}

// Accept both legacy JSON-encoded strings and nested objects.
func nestedJSON(b json.RawMessage, out any) error {
	if len(b) == 0 || string(b) == "null" {
		b = []byte("{}")
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = []byte(s)
	}
	return json.Unmarshal(b, out)
}

func (e *EngineClient) inbounds(ctx context.Context) ([]Inbound, error) {
	var all []Inbound
	err := e.call(ctx, "GET", "/panel/api/inbounds/list", nil, &all)
	return all, err
}

func marshalString(v any) string { b, _ := json.Marshal(v); return string(b) }

func (e *EngineClient) clientCall(ctx context.Context, verb, email string, body any) error {
	return e.call(ctx, "POST", "/panel/api/clients/"+verb+"/"+url.PathEscape(email), body, nil)
}

func (e *EngineClient) clientEmails(ctx context.Context) (map[string]bool, error) {
	var clients []struct {
		Email string `json:"email"`
	}
	if err := e.call(ctx, "GET", "/panel/api/clients/list", nil, &clients); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, c := range clients {
		known[c.Email] = true
	}
	return known, nil
}
