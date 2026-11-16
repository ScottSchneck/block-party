// Adapted from minecraft/service/signaling in github.com/hashimthearab/gophertunnel
// (commit 9331d09), MIT License, Copyright (c) 2019 Sandertv. See NOTICE.

package signaling

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/service"
)

func TestDialContextLogsServiceVersion(t *testing.T) {
	const serviceVersion = "2026.07.16.1"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Version", serviceVersion)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(context.Background())
	}))
	defer server.Close()

	serviceURI, err := url.Parse("ws" + strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatalf("parse service URL: %v", err)
	}

	var logs bytes.Buffer
	conn, err := (Dialer{
		Environment: staticConfigurationProvider{configuration: &Configuration{
			ServiceURI:    serviceURI,
			PingFrequency: time.Hour,
		}},
		Log: slog.New(slog.NewJSONHandler(&logs, nil)),
	}).DialContext(context.Background(), staticTokenSource{token: &service.Token{
		AuthorizationHeader: "test-token",
		Claims: service.Claims{
			PlayerMessagingID: uuid.New(),
		},
	}})
	if err != nil {
		t.Fatalf("dial signaling service: %v", err)
	}
	defer conn.Close()

	var entry map[string]any
	if err := json.NewDecoder(&logs).Decode(&entry); err != nil {
		t.Fatalf("decode log entry: %v", err)
	}
	if got := entry["msg"]; got != "connected to signaling service" {
		t.Fatalf("log message = %v, want connected to signaling service", got)
	}
	if got := entry["transport"]; got != "jsonrpc" {
		t.Fatalf("transport = %v, want jsonrpc", got)
	}
	if got := entry["service_version"]; got != serviceVersion {
		t.Fatalf("service_version = %v, want %s", got, serviceVersion)
	}
}

type staticConfigurationProvider struct {
	configuration *Configuration
}

func (p staticConfigurationProvider) Configuration(context.Context, *http.Client, service.TokenSource) (*Configuration, error) {
	return p.configuration, nil
}

type staticTokenSource struct {
	token *service.Token
}

func (s staticTokenSource) ServiceToken(context.Context) (*service.Token, error) {
	return s.token, nil
}
