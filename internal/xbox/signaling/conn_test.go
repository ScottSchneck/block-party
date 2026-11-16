// Adapted from minecraft/service/signaling in github.com/hashimthearab/gophertunnel
// (commit 9331d09), MIT License, Copyright (c) 2019 Sandertv. See NOTICE.

package signaling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/channel"
	"github.com/creachadair/jrpc2/handler"
	"github.com/df-mc/go-nethernet"
	"github.com/google/uuid"
)

func TestCredentialsRejectsWarmCacheAfterClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(net.ErrClosed)
	conn := &Conn{
		ctx:               ctx,
		credentials:       &nethernet.Credentials{ExpirationInSeconds: 60},
		credentialsExpiry: time.Now().Add(time.Minute),
	}
	if _, err := conn.Credentials(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Credentials() error = %v, want net.ErrClosed", err)
	}
}

func TestCredentialsRejectsNonPositiveExpiry(t *testing.T) {
	t.Parallel()

	for _, expiry := range []int{0, -1} {
		expiry := expiry
		t.Run(fmt.Sprint(expiry), func(t *testing.T) {
			t.Parallel()
			clientChannel, serverChannel := channel.Direct()
			server := jrpc2.NewServer(handler.Map{
				MethodSignalingCredentials: handler.New(func(context.Context, map[string]any) (*nethernet.Credentials, error) {
					return &nethernet.Credentials{ExpirationInSeconds: expiry}, nil
				}),
			}, nil).Start(serverChannel)
			defer server.Stop()
			client := jrpc2.NewClient(clientChannel, nil)
			defer client.Close()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(net.ErrClosed)
			conn := &Conn{client: client, ctx: ctx}

			if _, err := conn.Credentials(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid credentials") {
				t.Fatalf("Credentials() error = %v, want invalid credentials for expiry %d", err, expiry)
			}
		})
	}
}

// acceptingNotifier accepts every signal delivered by a test.
type acceptingNotifier struct{}

// NotifySignal accepts the signal.
func (acceptingNotifier) NotifySignal(*nethernet.Signal) bool { return true }

// Rejected or undecodable signals are dropped unacknowledged, without an error that would log
// the raw payload.
func TestConnHandleInnerMessageDoesNotAcknowledgeRejectedSignal(t *testing.T) {
	for _, typ := range []string{nethernet.SignalTypeOffer, "remote-controlled-type"} {
		t.Run(typ, func(t *testing.T) {
			signal := &nethernet.Signal{
				Type:         typ,
				ConnectionID: 42,
				Data:         "sensitive-payload",
			}
			params, err := json.Marshal(map[string]any{
				"netherNetId": "network",
				"message":     signal.String(),
			})
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}
			var logs bytes.Buffer
			conn := &Conn{
				d: Dialer{Log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{
					Level: slog.LevelDebug,
				}))},
				notifiers: make(map[uint32]nethernet.Notifier),
			}
			err = conn.handleInnerMessage(context.Background(), &envelope{
				From: uuid.New(),
				ID:   uuid.New(),
				Message: &jrpc2.ParsedRequest{
					Method: MethodSignalingWebRTC,
					Params: params,
				},
			})
			if err != nil {
				t.Fatalf("handleInnerMessage() error = %v, want nil", err)
			}
			if strings.Contains(logs.String(), signal.Data) {
				t.Fatal("rejected signal log contains the signal payload")
			}
			if strings.Contains(logs.String(), signal.Type) {
				t.Fatal("rejected signal log contains the remote-controlled signal type")
			}
			var entry map[string]any
			if err := json.NewDecoder(&logs).Decode(&entry); err != nil {
				t.Fatalf("decode log entry: %v", err)
			}
			if got := entry["msg"]; got != "incoming signal was not accepted" {
				t.Fatalf("log message = %v, want incoming signal was not accepted", got)
			}
		})
	}
}

func TestConnHandleInnerMessageAcknowledgesAcceptedSignal(t *testing.T) {
	type sendMessageParams struct {
		ToPlayerID uuid.UUID `json:"toPlayerId"`
		MessageID  uuid.UUID `json:"messageId"`
		Message    string    `json:"message"`
	}
	received := make(chan sendMessageParams, 1)
	clientChannel, serverChannel := channel.Direct()
	server := jrpc2.NewServer(handler.Map{
		MethodSignalingSendMessage: handler.New(func(_ context.Context, params *sendMessageParams) error {
			received <- *params
			return nil
		}),
	}, nil).Start(serverChannel)
	defer server.Stop()
	client := jrpc2.NewClient(clientChannel, nil)
	defer client.Close()

	signal := &nethernet.Signal{
		Type:         nethernet.SignalTypeOffer,
		ConnectionID: 42,
		Data:         "offer",
	}
	params, err := json.Marshal(map[string]any{
		"netherNetId": "network",
		"message":     signal.String(),
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	messageID, senderID := uuid.New(), uuid.New()
	conn := &Conn{
		client:    client,
		d:         Dialer{Log: slog.Default()},
		notifiers: map[uint32]nethernet.Notifier{0: acceptingNotifier{}},
	}
	if err := conn.handleInnerMessage(context.Background(), &envelope{
		From: senderID,
		ID:   messageID,
		Message: &jrpc2.ParsedRequest{
			Method: MethodSignalingWebRTC,
			Params: params,
		},
	}); err != nil {
		t.Fatalf("handleInnerMessage() error = %v", err)
	}

	request := <-received
	if request.ToPlayerID != senderID {
		t.Fatalf("delivery recipient = %s, want %s", request.ToPlayerID, senderID)
	}
	var delivery struct {
		Method string `json:"method"`
		Params struct {
			MessageID uuid.UUID `json:"messageId"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(request.Message), &delivery); err != nil {
		t.Fatalf("decode delivery notification: %v", err)
	}
	if delivery.Method != MethodSignalingDeliveryNotification {
		t.Fatalf("delivery method = %q, want %q", delivery.Method, MethodSignalingDeliveryNotification)
	}
	if delivery.Params.MessageID != messageID {
		t.Fatalf("delivery message ID = %s, want %s", delivery.Params.MessageID, messageID)
	}
}

// A delivery error from the service fails the signal it names instead of being refused as an
// unknown method; an error naming no pending signal is ignored.
func TestReceiveErrorFailsThePendingSignal(t *testing.T) {
	conn := &Conn{d: Dialer{Log: slog.New(slog.DiscardHandler)}, pending: newPendingMap()}
	id := uuid.New()
	ch := conn.pending.Add(id)
	params := []byte(`{"messageId":"` + id.String() + `","code":404,"message":"recipient offline"}`)
	if err := conn.handleReceiveError(params); err != nil {
		t.Fatalf("handleReceiveError() error = %v", err)
	}
	select {
	case err := <-ch:
		var serviceErr *Error
		if !errors.As(err, &serviceErr) || serviceErr.Code != 404 || serviceErr.Message != "recipient offline" {
			t.Fatalf("pending signal failed with %v", err)
		}
	default:
		t.Fatal("pending signal was not failed")
	}
	if err := conn.handleReceiveError([]byte(`{"code":1}`)); err != nil {
		t.Fatalf("an error naming no signal: %v", err)
	}
}

// recordingNotifier records calls and returns a fixed acceptance result.
type recordingNotifier struct {
	accept bool
	calls  int
}

// NotifySignal records the signal and returns the configured result.
func (n *recordingNotifier) NotifySignal(*nethernet.Signal) bool {
	n.calls++
	return n.accept
}

// A signal reaches every notifier and counts as accepted when any one of them accepts it.
func TestConnNotifySignalReportsCollectiveAcceptance(t *testing.T) {
	reject := &recordingNotifier{}
	accept := &recordingNotifier{accept: true}

	if (&Conn{notifiers: map[uint32]nethernet.Notifier{0: reject}}).notifySignal(new(nethernet.Signal)) {
		t.Fatal("notifySignal(all rejected) = true, want false")
	}
	if !(&Conn{notifiers: map[uint32]nethernet.Notifier{0: reject, 1: accept}}).notifySignal(new(nethernet.Signal)) {
		t.Fatal("notifySignal(one accepted) = false, want true")
	}
	if reject.calls != 2 || accept.calls != 1 {
		t.Fatalf("notifier calls = (%d, %d), want (2, 1)", reject.calls, accept.calls)
	}
}
