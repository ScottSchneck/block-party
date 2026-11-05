package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 20 * time.Second}

// answer turns an unsuccessful HTTP answer into an error with the service's
// own message when it gives one.
func answer(res *http.Response, service string) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var m struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(b, &m)
	msg := m.Message
	if msg == "" {
		msg = m.Error
	}
	if msg == "" {
		msg = res.Status
	}
	return fmt.Errorf("%s said: %s", service, msg)
}

func plain(e Event) string {
	if e.Text == "" {
		return e.Title
	}
	return e.Text
}

// ---- ntfy ----

var ntfyTags = map[string]string{
	JoinAttempt: "wave", PlayerJoined: "video_game", Crash: "rotating_light", BackupFailed: "warning",
	Updated: "arrow_up", NewVersion: "sparkles", PanelStarted: "white_check_mark", "test": "tada",
}

func sendNtfy(ctx context.Context, c Ntfy, e Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server+"/"+url.PathEscape(c.Topic), strings.NewReader(plain(e)))
	if err != nil {
		return err
	}
	// Header values must be plain ASCII; ntfy reads RFC 2047 for the rest.
	req.Header.Set("Title", mime.QEncoding.Encode("utf-8", e.Title))
	if tag := ntfyTags[e.Kind]; tag != "" {
		req.Header.Set("Tags", tag)
	}
	if e.Kind == Crash || e.Kind == BackupFailed {
		req.Header.Set("Priority", "high")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return answer(res, "ntfy")
}

// ---- Discord ----

// discordColors mark each kind of event in Discord (the stripe beside the
// message): red for trouble, amber for someone at the door, green for
// players, blue for updates.
var discordColors = map[string]int{
	Crash: 0xd83c3e, BackupFailed: 0xd83c3e, JoinAttempt: 0xe8a33d, PlayerJoined: 0x3ba55d,
	Updated: 0x4f7cff, UpdatesOut: 0x4f7cff, NewVersion: 0x4f7cff, PanelStarted: 0x8a8f98,
}

func sendDiscord(ctx context.Context, c Discord, e Event) error {
	content := "**" + e.Title + "**"
	if e.Text != "" && e.Text != e.Title {
		content += "\n" + e.Text
	}
	if len(content) > 1900 {
		content = content[:1900] + "…"
	}
	msg := map[string]any{
		"username": "Block Party", "content": content,
		"allowed_mentions": map[string]any{"parse": []string{}}, // never ping @everyone
	}
	if color, ok := discordColors[e.Kind]; ok {
		// A coloured card: the title in bold, the text under it.
		desc := e.Text
		if desc == e.Title {
			desc = ""
		}
		if len(desc) > 1900 {
			desc = desc[:1900] + "…"
		}
		title := e.Title
		if len(title) > 250 {
			title = title[:250] + "…"
		}
		embed := map[string]any{"title": title, "color": color}
		if desc != "" {
			embed["description"] = desc
		}
		if e.Server != "" {
			embed["footer"] = map[string]string{"text": e.Server}
		}
		msg["content"], msg["embeds"] = "", []any{embed}
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return redact(err, c.Webhook)
	}
	defer res.Body.Close()
	return answer(res, "Discord")
}

// redact keeps a secret URL out of error messages (and so the log).
func redact(err error, secret string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = "(webhook)"
		return ue
	}
	if secret == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "(webhook)"))
}

// ---- email ----

func sendEmail(ctx context.Context, c Email, e Event) error {
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\n", (&mailAddr{"Block Party", c.From}).String())
	fmt.Fprintf(&msg, "To: %s\r\n", strings.Join(c.To, ", "))
	fmt.Fprintf(&msg, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", e.Title))
	fmt.Fprintf(&msg, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(strings.ReplaceAll(plain(e), "\n", "\r\n"))
	msg.WriteString("\r\n\r\n-- \r\nSent by your Block Party panel. Change what it sends under Help, Notifications.\r\n")

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if c.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: c.Host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("couldn't reach %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	sc, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer sc.Close()
	if c.Port != 465 {
		if ok, _ := sc.Extension("STARTTLS"); ok {
			if err := sc.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
				return fmt.Errorf("couldn't secure the connection: %w", err)
			}
		} else if c.Password != "" {
			return errors.New("the server doesn't offer encryption, so the password wasn't sent")
		}
	}
	if c.Password != "" {
		user := c.Username
		if user == "" {
			user = c.From
		}
		if err := sc.Auth(smtp.PlainAuth("", user, c.Password, c.Host)); err != nil {
			return fmt.Errorf("the email server refused the username or password: %w", err)
		}
	}
	if err := sc.Mail(c.From); err != nil {
		return err
	}
	for _, t := range c.To {
		if err := sc.Rcpt(t); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	w, err := sc.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return sc.Quit()
}

type mailAddr struct{ name, addr string }

func (a *mailAddr) String() string {
	return mime.QEncoding.Encode("utf-8", a.name) + " <" + a.addr + ">"
}

// ---- text messages (Twilio) ----

func sendSMS(ctx context.Context, api string, c SMS, e Event) error {
	body := e.Title
	if e.Text != "" && e.Text != e.Title {
		body += ": " + e.Text
	}
	if r := []rune(body); len(r) > 300 {
		body = string(r[:300]) + "…"
	}
	var errs []string
	for _, to := range c.To {
		form := url.Values{"To": {to}, "Body": {body}}
		if strings.HasPrefix(c.From, "MG") {
			form.Set("MessagingServiceSid", c.From)
		} else {
			form.Set("From", c.From)
		}
		u := api + "/2010-04-01/Accounts/" + url.PathEscape(c.AccountSID) + "/Messages.json"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.AccountSID, c.AuthToken)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := client.Do(req)
		if err != nil {
			errs = append(errs, to+": "+err.Error())
			continue
		}
		err = answer(res, "Twilio")
		res.Body.Close()
		if err != nil {
			errs = append(errs, to+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
