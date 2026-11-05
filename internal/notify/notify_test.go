package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSettingsKeepSecrets(t *testing.T) {
	dir := t.TempDir()
	n := New(Options{DataDir: dir})
	v := n.View()
	if !strings.HasPrefix(v.Ntfy.Topic, "blockparty-") || v.Ntfy.Server != "https://ntfy.sh" || len(v.SMS.Events) != 2 {
		t.Errorf("defaults: %+v", v)
	}
	in := v.Settings
	in.Discord = Discord{On: true, Webhook: "https://discord.com/api/webhooks/1/abc", Events: []string{Crash}}
	in.SMS = SMS{On: true, AccountSID: "AC" + strings.Repeat("a", 32), AuthToken: "secret-token", From: "+1 (555) 123-0000", To: []string{"555-123-4567", ""}, Events: []string{Crash}}
	v, err := n.Update(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Discord.Webhook != "" || v.SMS.AuthToken != "" || !v.Secrets["discord.webhook"] || !v.Secrets["sms.authToken"] {
		t.Errorf("secrets shown: %+v", v)
	}
	if v.SMS.From != "+15551230000" || len(v.SMS.To) != 1 || v.SMS.To[0] != "+15551234567" {
		t.Errorf("numbers: %+v", v.SMS)
	}
	// Saving again with the secrets blank keeps them.
	in2 := v.Settings
	in2.SMS.On = false
	if _, err := n.Update(in2, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "notify.json"))
	if !strings.Contains(string(b), "secret-token") || !strings.Contains(string(b), "api/webhooks/1/abc") {
		t.Errorf("secrets lost: %s", b)
	}
	if st, _ := os.Stat(filepath.Join(dir, "notify.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	// Clearing works.
	in2.Discord.On = false
	if v, _ := n.Update(in2, map[string]bool{"discord.webhook": true}); v.Secrets["discord.webhook"] {
		t.Error("not cleared")
	}
	// Bad input is refused.
	for name, bad := range map[string]func(*Settings){
		"webhook":   func(s *Settings) { s.Discord.Webhook = "https://evil.example/api/webhooks/1" },
		"sid":       func(s *Settings) { s.SMS.AccountSID = "nope" },
		"phone":     func(s *Settings) { s.SMS.To = []string{"call me"} },
		"email":     func(s *Settings) { s.Email.To = []string{"not an address"} },
		"emailOn":   func(s *Settings) { s.Email.On = true },
		"event":     func(s *Settings) { s.Ntfy.Events = []string{"everything"} },
		"topic":     func(s *Settings) { s.Ntfy.Topic = "a/b" },
		"ntfyServr": func(s *Settings) { s.Ntfy.Server = "ftp://x" },
	} {
		s := n.View().Settings
		bad(&s)
		if _, err := n.Update(s, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A restart reads them back.
	if n2 := New(Options{DataDir: dir}); n2.View().SMS.From != "+15551230000" || n2.View().Ntfy.Topic != v.Ntfy.Topic {
		t.Error("not saved")
	}
}

func TestChannelsGetTheirEvents(t *testing.T) {
	n := New(Options{DataDir: t.TempDir()})
	var mu sync.Mutex
	got := map[string][]string{}
	n.sendHook = func(ch string, e Event) error {
		mu.Lock()
		got[ch] = append(got[ch], e.Kind)
		mu.Unlock()
		return nil
	}
	s := n.View().Settings
	s.Ntfy.On, s.Ntfy.Events = true, []string{JoinAttempt, Crash}
	s.Discord = Discord{On: true, Webhook: "https://discord.com/api/webhooks/1/x", Events: []string{JoinAttempt}}
	s.SMS = SMS{On: true, AccountSID: "AC" + strings.Repeat("b", 32), AuthToken: "t", From: "+15550000000", To: []string{"+15551111111"}, Events: []string{Crash}, MaxPerDay: 2}
	if _, err := n.Update(s, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	n.deliver(ctx, Event{Kind: JoinAttempt, Server: "A", Key: "join/A/Steve", Title: "Steve wants in"})
	n.deliver(ctx, Event{Kind: JoinAttempt, Server: "A", Key: "join/A/Steve", Title: "again"}) // a repeat
	n.deliver(ctx, Event{Kind: Crash, Server: "A", Title: "A crashed"})
	n.deliver(ctx, Event{Kind: Crash, Server: "B", Title: "B crashed"})
	n.deliver(ctx, Event{Kind: Crash, Server: "C", Title: "C crashed"}) // over the text limit
	n.deliver(ctx, Event{Kind: PlayerJoined, Server: "A", Title: "nobody wants this"})
	want := map[string]string{
		ChanNtfy:    "joinAttempt crash crash crash",
		ChanDiscord: "joinAttempt",
		ChanSMS:     "crash crash",
	}
	for ch, w := range want {
		if strings.Join(got[ch], " ") != w {
			t.Errorf("%s got %v, want %s", ch, got[ch], w)
		}
	}
	if len(got[ChanEmail]) != 0 {
		t.Error("email is off")
	}
}

func TestSenders(t *testing.T) {
	var mu sync.Mutex
	var reqs []*http.Request
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, r)
		bodies = append(bodies, string(b))
		mu.Unlock()
		if strings.Contains(r.URL.Path, "fail") {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":20003,"message":"Authenticate"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	e := Event{Kind: Crash, Title: "Ava’s Server crashed", Text: "It stopped unexpectedly."}

	if err := sendNtfy(ctx, Ntfy{Server: srv.URL, Topic: "blockparty-x", Token: "tk"}, e); err != nil {
		t.Fatal(err)
	}
	r := reqs[0]
	if r.URL.Path != "/blockparty-x" || r.Header.Get("Priority") != "high" || r.Header.Get("Authorization") != "Bearer tk" || !strings.HasPrefix(r.Header.Get("Title"), "=?utf-8?") || bodies[0] != "It stopped unexpectedly." {
		t.Errorf("ntfy: %v %v %q", r.URL, r.Header, bodies[0])
	}

	if err := sendDiscord(ctx, Discord{Webhook: srv.URL + "/api/webhooks/1/x"}, e); err != nil {
		t.Fatal(err)
	}
	var d struct {
		Content  string `json:"content"`
		Mentions any    `json:"allowed_mentions"`
		Embeds   []struct {
			Title, Description string
			Color              int
			Footer             struct{ Text string }
		} `json:"embeds"`
	}
	json.Unmarshal([]byte(bodies[1]), &d)
	if d.Content != "" || d.Mentions == nil || len(d.Embeds) != 1 || d.Embeds[0].Title != "Ava’s Server crashed" ||
		d.Embeds[0].Description != "It stopped unexpectedly." || d.Embeds[0].Color != 0xd83c3e || d.Embeds[0].Footer.Text != e.Server {
		t.Errorf("discord: %s", bodies[1])
	}

	sid := "AC" + strings.Repeat("c", 32)
	if err := sendSMS(ctx, srv.URL, SMS{AccountSID: sid, AuthToken: "tok", From: "MG" + strings.Repeat("d", 32), To: []string{"+15551111111", "+15552222222"}}, e); err != nil {
		t.Fatal(err)
	}
	r = reqs[2]
	user, pass, _ := r.BasicAuth()
	if r.URL.Path != "/2010-04-01/Accounts/"+sid+"/Messages.json" || user != sid || pass != "tok" || len(reqs) != 4 ||
		!strings.Contains(bodies[2], "MessagingServiceSid=MG") || !strings.Contains(bodies[2], "To=%2B15551111111") {
		t.Errorf("twilio: %v %s", r.URL, bodies[2])
	}
	err := sendSMS(ctx, srv.URL+"/fail", SMS{AccountSID: sid, AuthToken: "bad", From: "+15550000000", To: []string{"+15551111111"}}, e)
	if err == nil || !strings.Contains(err.Error(), "Authenticate") {
		t.Errorf("twilio error: %v", err)
	}
}

// fakeSMTP accepts one message without encryption or a password.
func fakeSMTP(t *testing.T) (port int, got chan string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		rd := bufio.NewReader(c)
		say := func(s string) { c.Write([]byte(s + "\r\n")) }
		say("220 fake")
		var data strings.Builder
		inData := false
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					got <- data.String()
					say("250 ok")
					continue
				}
				data.WriteString(line)
				continue
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake")
				say("250 8BITMIME")
			case cmd == "DATA":
				inData = true
				say("354 go")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, got
}

func TestEmail(t *testing.T) {
	port, got := fakeSMTP(t)
	c := Email{Host: "127.0.0.1", Port: port, From: "panel@example.com", To: []string{"dad@example.com"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sendEmail(ctx, c, Event{Kind: Crash, Title: "Ava’s Server crashed", Text: "Line one\nLine two"}); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	if !strings.Contains(msg, "Subject: =?utf-8?") || !strings.Contains(msg, "To: dad@example.com") || !strings.Contains(msg, "Line one\r\nLine two") {
		t.Errorf("message:\n%s", msg)
	}
	// A password is never sent without encryption.
	port2, _ := fakeSMTP(t)
	c.Port, c.Password = port2, "hunter2"
	if err := sendEmail(ctx, c, Event{Title: "x"}); err == nil || !strings.Contains(err.Error(), "encryption") {
		t.Errorf("sent a password in the clear: %v", err)
	}
}
