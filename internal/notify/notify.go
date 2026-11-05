// Package notify sends messages about the panel to the owner: to a phone
// (ntfy), a Discord channel, an email address, or by text message (Twilio).
// Any combination can be on, and each channel has its own list of events,
// so for example texts can be kept for crashes while Discord gets
// everything.
//
// Settings, including passwords and tokens, are kept in <data>/notify.json
// (readable only by the panel) and are never sent back to the browser.
package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Kinds of event.
const (
	JoinAttempt  = "joinAttempt"  // someone not on the allowlist tried to join
	PlayerJoined = "playerJoined" // someone started playing
	Crash        = "crash"        // a server crashed
	BackupFailed = "backupFailed" // a scheduled backup failed
	Updated      = "updated"      // an overnight update finished (or failed)
	NewVersion   = "newVersion"   // a new Bedrock version is out
	PanelStarted = "panelStarted" // the panel (container) started
	UpdatesOut   = "updatesOut"   // new versions of a server's plugins, mods, add-ons or modpack
)

// EventInfo describes a kind of event for the settings page.
type EventInfo struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

// Events lists every kind of event, in the order the page shows them.
var Events = []EventInfo{
	{JoinAttempt, "Someone not on the allowlist tries to join", true},
	{Crash, "A server crashes", true},
	{BackupFailed, "A scheduled backup fails", true},
	{Updated, "An overnight update finishes or fails", true},
	{NewVersion, "A new Bedrock version is out (when overnight updates are off)", false},
	{UpdatesOut, "Updates are out for a server's plugins, mods, add-ons or modpack (checked once a day)", false},
	{PlayerJoined, "Someone starts playing", false},
	{PanelStarted, "The panel starts (after the container or Unraid restarts)", false},
}

// Event is something that happened.
type Event struct {
	Kind   string
	Server string // the server's name, if any
	Title  string // short, e.g. "Kids Survival crashed"
	Text   string // a sentence or two
	// Key groups repeats: the same key isn't sent again within a while
	// (e.g. one player trying again and again). Empty means Kind+Server.
	Key string
}

// Channel names.
const (
	ChanNtfy    = "ntfy"
	ChanDiscord = "discord"
	ChanEmail   = "email"
	ChanSMS     = "sms"
)

var channelNames = []string{ChanNtfy, ChanDiscord, ChanEmail, ChanSMS}

// Ntfy sends push notifications through an ntfy server (ntfy.sh or your own).
type Ntfy struct {
	On     bool     `json:"on"`
	Server string   `json:"server"` // https://ntfy.sh
	Topic  string   `json:"topic"`  // made up and hard to guess
	Token  string   `json:"token,omitempty"`
	Events []string `json:"events"`
}

// Discord posts to a channel through a webhook.
type Discord struct {
	On      bool     `json:"on"`
	Webhook string   `json:"webhook,omitempty"`
	Events  []string `json:"events"`
}

// Email sends through an SMTP server.
type Email struct {
	On       bool     `json:"on"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`     // 587 (STARTTLS) or 465 (TLS)
	Username string   `json:"username"` // often the email address
	Password string   `json:"password,omitempty"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Events   []string `json:"events"`
}

// SMS sends text messages through Twilio's Programmable Messaging.
type SMS struct {
	On         bool     `json:"on"`
	AccountSID string   `json:"accountSid"`
	AuthToken  string   `json:"authToken,omitempty"`
	From       string   `json:"from"` // a Twilio number (+1…) or a Messaging Service SID (MG…)
	To         []string `json:"to"`   // numbers in +1… form
	Events     []string `json:"events"`
	// MaxPerDay limits texts (they cost money); 0 means 20.
	MaxPerDay int `json:"maxPerDay"`
}

// Settings is everything about notifications.
type Settings struct {
	Ntfy    Ntfy    `json:"ntfy"`
	Discord Discord `json:"discord"`
	Email   Email   `json:"email"`
	SMS     SMS     `json:"sms"`
}

// Options configure a Notifier.
type Options struct {
	DataDir string
	Log     *slog.Logger
	// Base URLs, for tests.
	TwilioAPI string // https://api.twilio.com
}

// Notifier sends events to the channels that want them.
type Notifier struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	saveMu   sync.Mutex // one settings save at a time
	settings Settings
	recent   map[string]time.Time // dedupe key -> last sent
	smsDay   string
	smsCount int
	queue    chan Event
	sendHook func(channel string, e Event) error // tests
}

// dedupeFor is how long the same event isn't sent again.
const dedupeFor = 30 * time.Minute

// New loads the settings. Call Run to start sending.
func New(opts Options) *Notifier {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.TwilioAPI == "" {
		opts.TwilioAPI = "https://api.twilio.com"
	}
	n := &Notifier{opts: opts, log: log.With("src", "notify"), recent: map[string]time.Time{}, queue: make(chan Event, 100)}
	if b, err := os.ReadFile(n.path()); err == nil {
		if err := json.Unmarshal(b, &n.settings); err != nil {
			// Kept for the owner to look at, rather than overwritten.
			bad := n.path() + ".unreadable"
			_ = os.Rename(n.path(), bad)
			n.log.Warn("notification settings couldn't be read; starting fresh", "moved to", bad, "error", err)
			n.settings = Settings{}
		}
	}
	n.fillDefaults(&n.settings)
	return n
}

func (n *Notifier) path() string { return filepath.Join(n.opts.DataDir, "notify.json") }

func defaultEvents() []string {
	var out []string
	for _, e := range Events {
		if e.Default {
			out = append(out, e.Kind)
		}
	}
	return out
}

func randomTopic() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return "blockparty-" + hex.EncodeToString(b)
}

func (n *Notifier) fillDefaults(s *Settings) {
	if s.Ntfy.Server == "" {
		s.Ntfy.Server = "https://ntfy.sh"
	}
	if s.Ntfy.Topic == "" {
		s.Ntfy.Topic = randomTopic()
	}
	if s.Ntfy.Events == nil {
		s.Ntfy.Events = defaultEvents()
	}
	if s.Discord.Events == nil {
		s.Discord.Events = defaultEvents()
	}
	if s.Email.Events == nil {
		s.Email.Events = defaultEvents()
	}
	if s.Email.Port == 0 {
		s.Email.Port = 587
	}
	if s.Email.To == nil {
		s.Email.To = []string{}
	}
	if s.SMS.Events == nil {
		s.SMS.Events = []string{Crash, BackupFailed} // texts cost money: urgent things only
	}
	if s.SMS.To == nil {
		s.SMS.To = []string{}
	}
	if s.SMS.MaxPerDay <= 0 {
		s.SMS.MaxPerDay = 20
	}
}

// View is the settings as the page sees them: secrets are replaced by
// whether they're set.
type View struct {
	Settings
	Secrets map[string]bool `json:"secrets"` // "ntfy.token", "discord.webhook", "email.password", "sms.authToken"
	Events  []EventInfo     `json:"events"`
}

// View returns the settings without their secrets.
func (n *Notifier) View() View {
	n.mu.Lock()
	s := n.settings
	n.mu.Unlock()
	v := View{Settings: s, Events: Events, Secrets: map[string]bool{
		"ntfy.token":      s.Ntfy.Token != "",
		"discord.webhook": s.Discord.Webhook != "",
		"email.password":  s.Email.Password != "",
		"sms.authToken":   s.SMS.AuthToken != "",
	}}
	v.Ntfy.Token, v.Discord.Webhook, v.Email.Password, v.SMS.AuthToken = "", "", "", ""
	return v
}

var (
	reE164     = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	reSID      = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	reService  = regexp.MustCompile(`^MG[0-9a-fA-F]{32}$`)
	reTopic    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reHostname = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

// cleanPhone accepts "+1 (555) 123-4567" and similar, returning +15551234567.
func cleanPhone(p string) (string, error) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(p) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", fmt.Errorf("%q isn't a phone number", p)
		}
	}
	s := b.String()
	if !strings.HasPrefix(s, "+") {
		if len(s) == 10 {
			s = "+1" + s // a US number typed without the country code
		} else {
			s = "+" + s
		}
	}
	if !reE164.MatchString(s) {
		return "", fmt.Errorf("%q isn't a phone number; write it with the country code, like +1 555 123 4567", p)
	}
	return s, nil
}

func cleanEvents(list []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, k := range list {
		ok := false
		for _, e := range Events {
			ok = ok || e.Kind == k
		}
		if !ok {
			return nil, fmt.Errorf("unknown event %q", k)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

// Update saves new settings. A secret left empty keeps the one saved, unless
// its channel's "clear" flag is in clear (such as "discord.webhook").
func (n *Notifier) Update(in Settings, clear map[string]bool) (View, error) {
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	n.mu.Lock()
	old := n.settings
	n.mu.Unlock()
	s := in
	keep := func(name, v, saved string) string {
		v = strings.TrimSpace(v)
		if v == "" && !clear[name] {
			return saved
		}
		return v
	}
	s.Ntfy.Token = keep("ntfy.token", s.Ntfy.Token, old.Ntfy.Token)
	s.Discord.Webhook = keep("discord.webhook", s.Discord.Webhook, old.Discord.Webhook)
	s.Email.Password = keep("email.password", s.Email.Password, old.Email.Password)
	s.SMS.AuthToken = keep("sms.authToken", s.SMS.AuthToken, old.SMS.AuthToken)
	if err := validate(&s); err != nil {
		return View{}, err
	}
	n.fillDefaults(&s)
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := n.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return View{}, err
	}
	if err := os.Rename(tmp, n.path()); err != nil {
		return View{}, err
	}
	n.mu.Lock()
	n.settings = s
	n.mu.Unlock()
	return n.View(), nil
}

func validate(s *Settings) error {
	var err error
	// ntfy
	s.Ntfy.Server = strings.TrimRight(strings.TrimSpace(s.Ntfy.Server), "/")
	if s.Ntfy.Server == "" {
		s.Ntfy.Server = "https://ntfy.sh"
	}
	if u, e := url.Parse(s.Ntfy.Server); e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return errors.New("ntfy: the server should be a web address like https://ntfy.sh")
	}
	s.Ntfy.Topic = strings.TrimSpace(s.Ntfy.Topic)
	if s.Ntfy.Topic != "" && !reTopic.MatchString(s.Ntfy.Topic) {
		return errors.New("ntfy: the topic can only have letters, numbers, - and _")
	}
	if s.Ntfy.Events, err = cleanEvents(s.Ntfy.Events); err != nil {
		return err
	}
	// Discord
	if w := s.Discord.Webhook; w != "" {
		u, e := url.Parse(w)
		if e != nil || u.Scheme != "https" || !(u.Host == "discord.com" || u.Host == "discordapp.com" || u.Host == "ptb.discord.com" || u.Host == "canary.discord.com") || !strings.HasPrefix(u.Path, "/api/webhooks/") {
			return errors.New("Discord: paste the webhook link from the channel's settings (Integrations, Webhooks, Copy Webhook URL)")
		}
	}
	if s.Discord.On && s.Discord.Webhook == "" {
		return errors.New("Discord: paste the webhook link first")
	}
	if s.Discord.Events, err = cleanEvents(s.Discord.Events); err != nil {
		return err
	}
	// Email
	e := &s.Email
	e.Host = strings.TrimSpace(e.Host)
	if e.Host != "" && !reHostname.MatchString(e.Host) {
		return errors.New("email: the SMTP server should be a name like smtp.gmail.com")
	}
	if e.Port < 0 || e.Port > 65535 {
		return errors.New("email: the port should be 587 or 465")
	}
	e.From = strings.TrimSpace(e.From)
	if e.From != "" {
		a, err := mail.ParseAddress(e.From)
		if err != nil {
			return fmt.Errorf("email: %q isn't an email address", e.From)
		}
		e.From = a.Address
	}
	var to []string
	for _, t := range e.To {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		a, err := mail.ParseAddress(t)
		if err != nil {
			return fmt.Errorf("email: %q isn't an email address", t)
		}
		to = append(to, a.Address)
	}
	e.To = to
	if e.On && (e.Host == "" || e.From == "" || len(e.To) == 0) {
		return errors.New("email: fill in the SMTP server, the From address and at least one address to send to")
	}
	if e.Events, err = cleanEvents(e.Events); err != nil {
		return err
	}
	// SMS
	t := &s.SMS
	t.AccountSID = strings.TrimSpace(t.AccountSID)
	if t.AccountSID != "" && !reSID.MatchString(t.AccountSID) {
		return errors.New("text messages: the Account SID starts with AC and is 34 characters long (Twilio Console, Account Info)")
	}
	t.From = strings.TrimSpace(t.From)
	if t.From != "" && !reService.MatchString(t.From) {
		if t.From, err = cleanPhone(t.From); err != nil {
			return errors.New("text messages: From should be your Twilio number (like +1 555 123 4567) or a Messaging Service SID (MG…)")
		}
	}
	var phones []string
	for _, p := range t.To {
		if strings.TrimSpace(p) == "" {
			continue
		}
		c, err := cleanPhone(p)
		if err != nil {
			return errors.New("text messages: " + err.Error())
		}
		phones = append(phones, c)
	}
	t.To = phones
	if len(t.To) > 10 {
		return errors.New("text messages: at most 10 numbers")
	}
	if t.On && (t.AccountSID == "" || t.AuthToken == "" || t.From == "" || len(t.To) == 0) {
		return errors.New("text messages: fill in the Account SID, Auth Token, From and at least one number to text")
	}
	if t.MaxPerDay < 0 || t.MaxPerDay > 500 {
		return errors.New("text messages: the daily limit should be between 1 and 500")
	}
	if t.Events, err = cleanEvents(t.Events); err != nil {
		return err
	}
	return nil
}

// Send queues an event for the channels that want it. It never blocks; if
// the queue is full (the network is down), the event is dropped.
func (n *Notifier) Send(e Event) {
	if n == nil {
		return
	}
	select {
	case n.queue <- e:
	default:
		n.log.Warn("too many notifications waiting; one was dropped", "kind", e.Kind)
	}
}

// Run sends queued events until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-n.queue:
			n.deliver(ctx, e)
		}
	}
}

func wants(list []string, kind string) bool {
	for _, k := range list {
		if k == kind {
			return true
		}
	}
	return false
}

// deliver sends one event to every channel that wants it.
func (n *Notifier) deliver(ctx context.Context, e Event) {
	key := e.Key
	if key == "" {
		key = e.Kind + "/" + e.Server
	}
	now := time.Now()
	n.mu.Lock()
	if last, ok := n.recent[key]; ok && now.Sub(last) < dedupeFor {
		n.mu.Unlock()
		return
	}
	n.recent[key] = now
	for k, t := range n.recent {
		if now.Sub(t) > dedupeFor {
			delete(n.recent, k)
		}
	}
	s := n.settings
	n.mu.Unlock()
	for _, ch := range channelNames {
		if !s.on(ch) || !wants(s.events(ch), e.Kind) {
			continue
		}
		if ch == ChanSMS && !n.takeSMS(s.SMS.MaxPerDay, len(s.SMS.To), now) {
			n.log.Warn("daily text message limit reached; not texting", "kind", e.Kind)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := n.sendTo(cctx, ch, s, e)
		cancel()
		if err != nil {
			n.log.Warn("couldn't send a notification", "channel", ch, "kind", e.Kind, "error", err)
		}
	}
}

// takeSMS uses up count texts of today's limit (one per number texted).
func (n *Notifier) takeSMS(max, count int, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	day := now.Format("2006-01-02")
	if n.smsDay != day {
		n.smsDay, n.smsCount = day, 0
	}
	if n.smsCount+count > max {
		return false
	}
	n.smsCount += count
	return true
}

func (s Settings) on(ch string) bool {
	switch ch {
	case ChanNtfy:
		return s.Ntfy.On
	case ChanDiscord:
		return s.Discord.On
	case ChanEmail:
		return s.Email.On
	case ChanSMS:
		return s.SMS.On
	}
	return false
}

func (s Settings) events(ch string) []string {
	switch ch {
	case ChanNtfy:
		return s.Ntfy.Events
	case ChanDiscord:
		return s.Discord.Events
	case ChanEmail:
		return s.Email.Events
	case ChanSMS:
		return s.SMS.Events
	}
	return nil
}

func (n *Notifier) sendTo(ctx context.Context, ch string, s Settings, e Event) error {
	if n.sendHook != nil {
		return n.sendHook(ch, e)
	}
	switch ch {
	case ChanNtfy:
		return sendNtfy(ctx, s.Ntfy, e)
	case ChanDiscord:
		return sendDiscord(ctx, s.Discord, e)
	case ChanEmail:
		return sendEmail(ctx, s.Email, e)
	case ChanSMS:
		return sendSMS(ctx, n.opts.TwilioAPI, s.SMS, e)
	}
	return errors.New("unknown channel")
}

// Test sends a test message through one channel straight away, with the
// saved settings, and says what went wrong.
func (n *Notifier) Test(ctx context.Context, ch string) error {
	n.mu.Lock()
	s := n.settings
	n.mu.Unlock()
	switch ch {
	case ChanDiscord:
		if s.Discord.Webhook == "" {
			return errors.New("paste the webhook link and save first")
		}
	case ChanEmail:
		if s.Email.Host == "" || s.Email.From == "" || len(s.Email.To) == 0 {
			return errors.New("fill in the email settings and save first")
		}
	case ChanSMS:
		if s.SMS.AccountSID == "" || s.SMS.AuthToken == "" || s.SMS.From == "" || len(s.SMS.To) == 0 {
			return errors.New("fill in the Twilio settings and save first")
		}
		if !n.takeSMS(s.SMS.MaxPerDay, len(s.SMS.To), time.Now()) {
			return errors.New("today's text message limit is used up")
		}
	case ChanNtfy:
	default:
		return errors.New("unknown channel")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return n.sendTo(ctx, ch, s, Event{Kind: "test", Title: "Block Party test", Text: "Notifications from your Block Party panel work."})
}
