package offsite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Connecting a destination. A new connection is set up in a separate
// config file and only replaces the working one once it has been tested, so
// a failed attempt never breaks copies that were working.
//
//   - Google Drive, OneDrive and Dropbox sign in on the provider's own site.
//     rclone waits for the provider to send the browser back to
//     http://127.0.0.1:53682/, which is inside the container, so the owner's
//     browser shows an error page there; the owner pastes that page's
//     address into the panel, which passes it on to rclone.
//   - iCloud Drive takes the Apple ID and password, then asks for the code
//     Apple shows on the owner's devices.
//   - Backblaze B2 and S3 take an access key and a bucket; a folder takes a
//     path inside the container.

// ConnectRequest is what the owner chose and typed.
type ConnectRequest struct {
	Kind       string `json:"kind"`
	Folder     string `json:"folder"`
	Keep       int    `json:"keep"`
	Encrypt    bool   `json:"encrypt"`
	Passphrase string `json:"passphrase,omitempty"`
	// Path is the folder's path (folder) or the bucket (B2, S3).
	Path string `json:"path,omitempty"`
	// iCloud.
	AppleID  string `json:"appleId,omitempty"`
	Password string `json:"password,omitempty"`
	// B2 and S3.
	KeyID    string `json:"keyId,omitempty"`
	Key      string `json:"key,omitempty"`
	Provider string `json:"provider,omitempty"` // S3: AWS, Cloudflare, Wasabi or Other
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
}

// Step is what the page shows next while connecting.
type Step struct {
	// Step is "oauth" (open URL, sign in, paste the address back),
	// "question" (answer Question) or "done".
	Step     string    `json:"step"`
	URL      string    `json:"url,omitempty"`
	Question *Question `json:"question,omitempty"`
	Settings *Settings `json:"settings,omitempty"`
}

// Question is something the provider asks while connecting (iCloud's code).
type Question struct {
	Name     string `json:"name"`
	Help     string `json:"help"`
	Password bool   `json:"password,omitempty"`
}

type question struct {
	state string
	q     Question
}

// authFlow is an rclone authorize waiting for the provider's answer.
type authFlow struct {
	kind   string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	out    *lockedBuffer
	done   chan struct{}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// pending is the connection being set up.
type pending struct {
	req      ConnectRequest
	settings Settings
}

func (m *Manager) pendingConf() string { return filepath.Join(m.dir, "rclone.new.conf") }

// authPort is where rclone waits for the provider's answer.
const authPort = "53682"

// authorizeConfig asks Google Drive for access to the files rclone makes
// only, rather than the whole Drive.
var authorizeConfig = map[string]string{
	KindDrive: base64.RawStdEncoding.EncodeToString([]byte(`{"scope":"drive.file"}`)),
}

var (
	reAuthLink  = regexp.MustCompile(`https?://(?:127\.0\.0\.1|localhost):` + authPort + `/auth\?state=[A-Za-z0-9_-]+`)
	reToken     = regexp.MustCompile(`(?s)--->\s*(\{.*?\})\s*<---End paste`)
	authTimeout = 15 * time.Minute
)

func cleanRequest(r ConnectRequest) (ConnectRequest, error) {
	info, ok := kindInfo(r.Kind)
	if !ok {
		return r, errors.New("choose where to copy backups")
	}
	r.Folder = strings.Trim(strings.TrimSpace(r.Folder), "/")
	if r.Folder == "" {
		r.Folder = DefaultFolder
	}
	if strings.Contains(r.Folder, "..") || strings.ContainsAny(r.Folder, `:\`) || len(r.Folder) > 100 {
		return r, errors.New("the folder name can't contain : \\ or ..")
	}
	if r.Keep == 0 {
		r.Keep = DefaultKeep
	}
	if r.Keep < 1 || r.Keep > maxKeep {
		return r, fmt.Errorf("keep between 1 and %d backups of each server", maxKeep)
	}
	if r.Encrypt && len([]rune(r.Passphrase)) < 8 {
		return r, errors.New("the encryption passphrase needs at least 8 characters")
	}
	r.Path = strings.TrimSpace(r.Path)
	switch info.How {
	case "folder":
		if !filepath.IsAbs(r.Path) {
			return r, errors.New("type the folder's path inside the container, such as /offsite")
		}
		r.Path = filepath.Clean(r.Path)
		if fi, err := os.Stat(r.Path); err != nil || !fi.IsDir() {
			return r, fmt.Errorf("%s isn't a folder in the container; add it as a path in the container's settings first", r.Path)
		}
	case "icloud":
		r.AppleID = strings.TrimSpace(r.AppleID)
		if r.AppleID == "" || r.Password == "" {
			return r, errors.New("type your Apple ID and its password")
		}
	case "keys":
		r.KeyID, r.Key = strings.TrimSpace(r.KeyID), strings.TrimSpace(r.Key)
		if r.KeyID == "" || r.Key == "" {
			return r, errors.New("type the key ID and the key")
		}
		r.Path = strings.Trim(r.Path, "/")
		if r.Path == "" || strings.ContainsAny(r.Path, " :") {
			return r, errors.New("type the bucket's name")
		}
		if r.Kind == KindS3 {
			switch r.Provider {
			case "AWS", "Cloudflare", "Wasabi", "Other":
			default:
				return r, errors.New("choose which S3 service it is")
			}
			if r.Provider != "AWS" && strings.TrimSpace(r.Endpoint) == "" {
				return r, errors.New("type the service's endpoint (its S3 address)")
			}
		}
	}
	return r, nil
}

// Connect starts connecting a destination.
func (m *Manager) Connect(ctx context.Context, r ConnectRequest) (Step, error) {
	r, err := cleanRequest(r)
	if err != nil {
		return Step{}, err
	}
	m.stopAuth()
	m.authMu.Lock()
	m.ask = nil
	m.authMu.Unlock()
	m.pendMu.Lock()
	m.pend = &pending{req: r, settings: Settings{On: true, Kind: r.Kind, Folder: r.Folder, Path: r.Path, Keep: r.Keep, Encrypt: r.Encrypt}}
	m.pendMu.Unlock()
	_ = os.Remove(m.pendingConf())
	info, _ := kindInfo(r.Kind)
	typ := rcloneType[r.Kind]
	switch info.How {
	case "oauth":
		link, err := m.startAuth(r.Kind)
		if err != nil {
			return Step{}, err
		}
		return Step{Step: "oauth", URL: link}, nil
	case "icloud":
		m.setAccount(r.AppleID)
		return m.configure(ctx, "config", "create", "offsite", typ, "apple_id", r.AppleID, "password", r.Password, "--obscure")
	case "keys":
		args := []string{"config", "create", "offsite", typ}
		if r.Kind == KindB2 {
			args = append(args, "account", r.KeyID, "key", r.Key)
		} else {
			args = append(args, "provider", r.Provider, "env_auth", "false", "access_key_id", r.KeyID, "secret_access_key", r.Key)
			if r.Endpoint != "" {
				args = append(args, "endpoint", strings.TrimSpace(r.Endpoint))
			}
			if r.Region != "" {
				args = append(args, "region", strings.TrimSpace(r.Region))
			}
		}
		m.setAccount(r.KeyID)
		return m.configure(ctx, append(args, "--obscure")...)
	default: // folder
		return m.configure(ctx, "config", "create", "offsite", typ)
	}
}

func (m *Manager) setAccount(a string) {
	m.pendMu.Lock()
	if m.pend != nil {
		m.pend.settings.Account = a
	}
	m.pendMu.Unlock()
}

func appleError(err error) error {
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "advanced data protection") || strings.Contains(msg, "ADP") {
		return errors.New("iCloud Drive can't be reached while Advanced Data Protection is on. Turn it off, and turn on \"Access iCloud Data on the Web\" (iPhone Settings → your name → iCloud), then try again")
	}
	return err
}

// startAuth runs rclone authorize and returns the provider's sign-in page.
func (m *Manager) startAuth(kind string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	args := []string{"--config", m.pendingConf(), "authorize", rcloneType[kind]}
	if c := authorizeConfig[kind]; c != "" {
		args = append(args, c)
	}
	args = append(args, "--auth-no-open-browser")
	cmd := exec.CommandContext(ctx, m.opts.Rclone, args...)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		cancel()
		if errors.Is(err, exec.ErrNotFound) {
			return "", errNoRclone
		}
		return "", err
	}
	f := &authFlow{kind: kind, cmd: cmd, cancel: cancel, out: out, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(f.done) }()
	m.authMu.Lock()
	m.auth = f
	m.authMu.Unlock()

	// rclone prints a link to its own page, which sends the browser on to
	// the provider; the panel follows that one step itself.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if link := reAuthLink.FindString(out.String()); link != "" {
			return providerURL(link)
		}
		select {
		case <-f.done:
			m.stopAuth()
			return "", fmt.Errorf("rclone stopped: %s", lastLine(out.String()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	m.stopAuth()
	return "", errors.New("rclone didn't offer a sign-in page; try again")
}

func providerURL(link string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(link)
	if err != nil {
		return "", fmt.Errorf("couldn't reach rclone's sign-in step: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	loc := res.Header.Get("Location")
	if u, err := url.Parse(loc); err != nil || u.Scheme != "https" {
		return "", errors.New("rclone didn't offer a sign-in page; try again")
	}
	return loc, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return reLogPrefix.ReplaceAllString(strings.TrimSpace(lines[len(lines)-1]), "")
}

func (m *Manager) stopAuth() {
	m.authMu.Lock()
	f := m.auth
	m.auth = nil
	m.authMu.Unlock()
	if f != nil {
		f.cancel()
		<-f.done
	}
}

// FinishSignIn takes the address the provider sent the browser to (the
// page that didn't load) and finishes connecting.
func (m *Manager) FinishSignIn(ctx context.Context, pasted string) (Step, error) {
	m.authMu.Lock()
	f := m.auth
	m.authMu.Unlock()
	if f == nil {
		return Step{}, errors.New("the sign-in has expired; start again")
	}
	u, err := url.Parse(strings.TrimSpace(pasted))
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Port() != authPort {
		return Step{}, errors.New("paste the whole address from the page that didn't load: it starts with http://127.0.0.1:53682/ or http://localhost:53682/")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		m.stopAuth()
		return Step{}, fmt.Errorf("the sign-in didn't finish (%s); start again", e)
	}
	if q.Get("code") == "" || q.Get("state") == "" {
		return Step{}, errors.New("that address is missing the sign-in code; paste the whole address")
	}
	local := url.URL{Scheme: "http", Host: "127.0.0.1:" + authPort, Path: u.Path, RawQuery: u.RawQuery}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Get(local.String())
	if err != nil {
		m.stopAuth()
		return Step{}, errors.New("the sign-in has expired; start again")
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	select {
	case <-f.done:
	case <-time.After(60 * time.Second):
	case <-ctx.Done():
		return Step{}, ctx.Err()
	}
	mm := reToken.FindStringSubmatch(f.out.String())
	m.stopAuth()
	if mm == nil {
		return Step{}, fmt.Errorf("the provider didn't give access: %s", lastLine(f.out.String()))
	}
	token := mm[1]
	args := []string{"config", "create", "offsite", rcloneType[f.kind], "token", token}
	if f.kind == KindDrive {
		args = append(args, "scope", "drive.file")
	}
	return m.configure(ctx, args...)
}

// configState is rclone's answer to a non-interactive config step.
type configState struct {
	State  string `json:"State"`
	Option *struct {
		Name       string `json:"Name"`
		Help       string `json:"Help"`
		IsPassword bool   `json:"IsPassword"`
		Default    any    `json:"Default"`
		Examples   []struct {
			Value string `json:"Value"`
		} `json:"Examples"`
	} `json:"Option"`
	Error string `json:"Error"`
}

// autoAnswers are rclone's questions the panel answers itself: don't sign
// in again (the token is fresh), carry on with rclone's own app
// registration, no shared drive, and OneDrive's own drive.
var autoAnswers = map[string]string{
	"config_refresh_token":     "false",
	"config_shared_client_id":  "true",
	"config_change_team_drive": "false",
	"config_type":              "onedrive",
	"config_drive_ok":          "true",
}

// autoAnswer picks the answer to a question nobody needs to see: a known
// one, the default, or the first choice.
func (cs configState) autoAnswer() (string, bool) {
	o := cs.Option
	if v, ok := autoAnswers[o.Name]; ok {
		return v, true
	}
	if o.Name == "config_2fa" || o.IsPassword {
		return "", false
	}
	switch d := o.Default.(type) {
	case bool:
		return strconv.FormatBool(d), true
	case string:
		if d != "" {
			return d, true
		}
	case float64:
		return strconv.FormatFloat(d, 'f', -1, 64), true
	}
	if len(o.Examples) > 0 && o.Examples[0].Value != "" {
		return o.Examples[0].Value, true
	}
	return "", false
}

// configure runs a non-interactive rclone config step and the ones after
// it, answering what it can, until it's done (then the connection is
// finished) or the owner has to answer.
func (m *Manager) configure(ctx context.Context, args ...string) (Step, error) {
	out, err := m.rcloneConf(ctx, m.pendingConf(), nil, append(args, "--non-interactive")...)
	if err != nil {
		return Step{}, appleError(err)
	}
	return m.nextQuestion(ctx, out)
}

// nextQuestion reads rclone's config step: another question, or done.
func (m *Manager) nextQuestion(ctx context.Context, out []byte) (Step, error) {
	seen := map[string]int{}
	for {
		var cs configState
		if t := strings.TrimSpace(string(out)); t != "" {
			start := strings.Index(t, "{")
			if start < 0 || json.Unmarshal([]byte(t[start:]), &cs) != nil {
				return Step{}, fmt.Errorf("couldn't read rclone's answer: %s", lastLine(t))
			}
		}
		if cs.State == "" || cs.Option == nil {
			if cs.Error != "" {
				return Step{}, appleError(errors.New(cs.Error))
			}
			return m.finish(ctx)
		}
		if seen[cs.State]++; seen[cs.State] > 2 || len(seen) > 20 {
			msg := cs.Error
			if msg == "" {
				msg = "rclone kept asking the same question"
			}
			return Step{}, errors.New(msg)
		}
		answer, ok := cs.autoAnswer()
		if !ok {
			return m.askOwner(cs)
		}
		var err error
		out, err = m.rcloneConf(ctx, m.pendingConf(), nil, "config", "update", "offsite", "--continue",
			"--state", cs.State, "--result", answer, "--non-interactive")
		if err != nil {
			return Step{}, appleError(err)
		}
	}
}

// askOwner shows the owner rclone's question.
func (m *Manager) askOwner(cs configState) (Step, error) {
	help := strings.TrimSpace(cs.Option.Help)
	if cs.Error != "" {
		help = cs.Error + "\n" + help
	}
	q := Question{Name: cs.Option.Name, Help: help, Password: cs.Option.IsPassword}
	if cs.Option.Name == "config_2fa" {
		q.Help = "Type the 6-digit code Apple shows on your iPhone, iPad or Mac."
		if cs.Error != "" {
			q.Help = cs.Error + " " + q.Help
		}
	}
	m.authMu.Lock()
	m.ask = &question{state: cs.State, q: q}
	m.authMu.Unlock()
	return Step{Step: "question", Question: &q}, nil
}

// Answer answers the provider's question (iCloud's code).
func (m *Manager) Answer(ctx context.Context, answer string) (Step, error) {
	m.authMu.Lock()
	a := m.ask
	m.ask = nil
	m.authMu.Unlock()
	if a == nil {
		return Step{}, errors.New("nothing is waiting for an answer; start again")
	}
	answer = strings.TrimSpace(answer)
	if a.q.Name == "config_2fa" {
		answer = strings.ReplaceAll(answer, " ", "")
		if _, err := strconv.Atoi(answer); err != nil || len(answer) != 6 {
			m.authMu.Lock()
			m.ask = a
			m.authMu.Unlock()
			return Step{}, errors.New("the code is 6 digits")
		}
	}
	out, err := m.rcloneConf(ctx, m.pendingConf(), nil, "config", "update", "offsite", "--continue",
		"--state", a.state, "--result", answer, "--non-interactive")
	if err != nil {
		return Step{}, appleError(err)
	}
	return m.nextQuestion(ctx, out)
}

// finish adds encryption if wanted, checks the destination can be written,
// and switches over to it.
func (m *Manager) finish(ctx context.Context) (Step, error) {
	m.pendMu.Lock()
	p := m.pend
	m.pendMu.Unlock()
	if p == nil {
		return Step{}, errors.New("start again")
	}
	s := p.settings
	conf := m.pendingConf()
	if s.Encrypt {
		if _, err := m.rcloneConf(ctx, conf, nil, "config", "create", "offsite-crypt", "crypt",
			"remote", s.base(), "password", p.req.Passphrase, "filename_encryption", "standard",
			"directory_name_encryption", "true", "--obscure"); err != nil {
			return Step{}, err
		}
	}
	if err := m.check(ctx, conf, s); err != nil {
		return Step{}, err
	}
	m.passMu.Lock()
	err := os.Rename(conf, m.confPath())
	m.passMu.Unlock()
	if err != nil {
		return Step{}, err
	}
	_ = os.Chmod(m.confPath(), 0o600)
	s.Connected = time.Now().UTC()
	if err := m.saveSettings(s); err != nil {
		return Step{}, err
	}
	m.pendMu.Lock()
	m.pend = nil
	m.pendMu.Unlock()
	m.setStatus(func(st *Status) { *st = Status{} })
	m.mu.Lock()
	m.failed = false
	m.mu.Unlock()
	m.log.Info("off-site backups connected", "to", s.Kind, "folder", s.Folder, "encrypted", s.Encrypt)
	m.Soon()
	return Step{Step: "done", Settings: &s}, nil
}

// check makes the folder and writes and removes a small file there.
func (m *Manager) check(ctx context.Context, conf string, s Settings) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := m.rcloneConf(ctx, conf, nil, "mkdir", s.target()); err != nil {
		return fmt.Errorf("couldn't make the folder there: %w", err)
	}
	probe := join(s.target(), ".block-party-check")
	if _, err := m.rcloneConf(ctx, conf, []byte("Block Party can write here.\n"), "rcat", probe); err != nil {
		return fmt.Errorf("couldn't write there: %w", err)
	}
	if _, err := m.rcloneConf(ctx, conf, nil, "deletefile", probe); err != nil {
		return fmt.Errorf("couldn't remove a test file there: %w", err)
	}
	return nil
}

// SetOptions turns copying on or off and changes how many are kept.
func (m *Manager) SetOptions(on bool, keep int) (Settings, error) {
	s := m.Settings()
	if s.Kind == "" && on {
		return s, errors.New("choose where to copy backups first")
	}
	if keep < 1 || keep > maxKeep {
		return s, fmt.Errorf("keep between 1 and %d backups of each server", maxKeep)
	}
	s.On, s.Keep = on, keep
	if err := m.saveSettings(s); err != nil {
		return s, err
	}
	if on {
		m.Soon()
	}
	return s, nil
}

// Disconnect forgets the destination and its credentials. What was copied
// stays where it is.
func (m *Manager) Disconnect() error {
	m.stopAuth()
	m.passMu.Lock()
	defer m.passMu.Unlock()
	_ = os.Remove(m.pendingConf())
	if err := os.Remove(m.confPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s := m.Settings()
	if err := m.saveSettings(Settings{Folder: s.Folder, Keep: s.Keep}); err != nil {
		return err
	}
	m.setStatus(func(st *Status) { *st = Status{} })
	return nil
}

// Version is rclone's version, or "" if it isn't there. It's looked up
// once.
func (m *Manager) Version(ctx context.Context) string {
	m.versionOnce.Do(func() {
		out, err := m.rclone(ctx, nil, "version")
		if err != nil {
			return
		}
		line, _, _ := strings.Cut(string(out), "\n")
		m.version = strings.TrimSpace(strings.TrimPrefix(line, "rclone "))
	})
	return m.version
}
