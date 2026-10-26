// Package web serves the panel's web page and its API.
//
// Sign-in is the owner account from package auth, with a session cookie.
// The page itself (HTML, script, styles) is public; every API call except
// sign-in needs a session.
package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ScottSchneck/block-party/internal/activity"
	"github.com/ScottSchneck/block-party/internal/auth"
	"github.com/ScottSchneck/block-party/internal/notify"
	"github.com/ScottSchneck/block-party/internal/offsite"
	"github.com/ScottSchneck/block-party/internal/outside"
	"github.com/ScottSchneck/block-party/internal/servers"
)

//go:embed static
var static embed.FS

// Options configures the web server.
type Options struct {
	Port     int    // 8443 by default
	TLS      bool   // serve HTTPS with a self-signed certificate
	Password string // PANEL_PASSWORD from before accounts; accepted as the setup code
	DataDir  string
	HostIP   string // the container's LAN IP, added to the certificate
	ListName string // what consoles show under LAN Games
	Version  string
	// Outside is outside access (friends outside the house). Tests may
	// leave it nil; the panel then makes one that isn't running.
	Outside *outside.Manager
	// Offsite copies backups elsewhere; tests may leave it nil (one that
	// isn't running is made).
	Offsite *offsite.Manager
	// Notifier sends notifications; tests may leave it nil (one that
	// doesn't send is made).
	Notifier *notify.Notifier
}

// Server is the panel's web server.
type Server struct {
	opts     Options
	mgr      *servers.Manager
	auth     *auth.Store
	activity *activity.Log
	out      *outside.Manager
	off      *offsite.Manager
	notifier *notify.Notifier
	log      *slog.Logger
	update   panelUpdate // the newest Block Party release seen
}

// New prepares the web server and the owner account store.
func New(opts Options, mgr *servers.Manager) (*Server, error) {
	if opts.Port == 0 {
		opts.Port = 8443
	}
	s := &Server{opts: opts, mgr: mgr, log: slog.Default().With("src", "web")}
	// Before accounts, the panel had one shared password. Until the owner
	// account exists it works as the setup code.
	legacy := opts.Password
	if legacy == "" {
		if b, err := os.ReadFile(filepath.Join(opts.DataDir, "panel-password")); err == nil {
			legacy = strings.TrimSpace(string(b))
		}
	}
	store, err := auth.Open(opts.DataDir, legacy)
	if err != nil {
		return nil, err
	}
	s.auth = store
	s.activity = activity.Open(filepath.Join(opts.DataDir, "activity.jsonl"))
	s.notifier = opts.Notifier
	if s.notifier == nil {
		s.notifier = notify.New(notify.Options{DataDir: opts.DataDir})
	}
	s.off = opts.Offsite
	if s.off == nil {
		s.off = offsite.New(offsite.Options{DataDir: opts.DataDir, Backups: func() []offsite.LocalBackup { return nil }})
	}
	s.out = opts.Outside
	if s.out == nil {
		local, _ := netip.ParseAddr(opts.HostIP)
		s.out = outside.New(outside.Options{DataDir: opts.DataDir, LocalIP: local, ListPort: 19132, Servers: OutsideServers(mgr)})
	}
	code, usesOld, err := store.EnsureSetupCode()
	if err != nil {
		return nil, fmt.Errorf("couldn't make a setup code: %w", err)
	}
	url := fmt.Sprintf("https://%s:%d", hostOr(opts.HostIP), opts.Port)
	if !opts.TLS {
		url = fmt.Sprintf("http://%s:%d", hostOr(opts.HostIP), opts.Port)
	}
	switch {
	case usesOld:
		s.log.Warn("open the panel to create the owner account; the setup code is your old panel password", "url", url)
	case code != "":
		s.log.Warn("open the panel to create the owner account", "url", url, "setup code", code)
	case store.ResetPending():
		s.log.Warn("a password reset is waiting; open the panel and choose \"Forgot your password?\"", "url", url)
	}
	return s, nil
}

// serverLog passes the HTTP server's own messages to the panel's log. TLS
// handshake errors are routine with the panel's self-made certificate (a
// browser that doesn't trust it yet hangs up, or opens spare connections it
// then drops), so they're only logged at debug level.
type serverLog struct{ log *slog.Logger }

func (l serverLog) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if strings.Contains(msg, "TLS handshake error") {
		l.log.Debug(msg)
	} else {
		l.log.Warn(msg)
	}
	return len(p), nil
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.opts.Port),
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(serverLog{s.log}, "", 0),
	}
	go s.checkPanelUpdates(ctx)
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	scheme := "http"
	var err error
	if s.opts.TLS {
		scheme = "https"
		cert, cerr := s.certificate()
		if cerr != nil {
			return cerr
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		s.log.Info("web panel ready", "url", fmt.Sprintf("%s://%s:%d", scheme, hostOr(s.opts.HostIP), s.opts.Port))
		err = srv.ListenAndServeTLS("", "")
	} else {
		s.log.Info("web panel ready", "url", fmt.Sprintf("%s://%s:%d", scheme, hostOr(s.opts.HostIP), s.opts.Port))
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func hostOr(ip string) string {
	if ip == "" {
		return "<this container's IP>"
	}
	return ip
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The page is tiny and changes with every update, so browsers must
		// check for a new copy each time rather than show a stale one.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("GET /api/servers", s.listServers)
	mux.HandleFunc("POST /api/servers", s.createServer)
	mux.HandleFunc("POST /api/servers/{id}/{action}", s.serverAction)
	mux.HandleFunc("GET /api/servers/{id}/console", s.console)
	s.playerRoutes(mux)
	s.settingsRoutes(mux)
	s.authRoutes(mux)
	s.importRoutes(mux)
	s.backupRoutes(mux)
	s.peopleRoutes(mux)
	s.outsideRoutes(mux)
	s.addonRoutes(mux)
	s.pluginRoutes(mux)
	s.hytaleRoutes(mux)
	s.playtimeRoutes(mux)
	s.offsiteRoutes(mux)
	s.lifecycleRoutes(mux)
	s.notifyRoutes(mux)
	return s.secure(mux)
}

// cookieName is the session cookie's name. Over HTTPS the __Host- prefix
// makes browsers refuse it unless it's secure and for this whole site, so
// nothing else on the same host can set or shadow it.
func (s *Server) cookieName() string {
	if s.opts.TLS {
		return "__Host-bh_session"
	}
	return "bh_session"
}

// publicAPI can be used without signing in.
var publicAPI = map[string]bool{
	"/api/auth/state": true, "/api/auth/setup": true, "/api/auth/login": true, "/api/auth/login/code": true, "/api/auth/reset": true,
}

// secure checks the session and blocks cross-site requests.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data: https://cdn.modrinth.com https://hangarcdn.papermc.io https://hangar.papermc.io")
		// A custom header can't be sent cross-site without the browser asking
		// first, and we never allow that, so other websites can't press
		// buttons (or sign you in to their account) through your browser.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Block-Party") != "1" {
			http.Error(w, "missing X-Block-Party header", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !publicAPI[r.URL.Path] {
			p := s.who(r)
			if p == nil {
				writeError(w, http.StatusUnauthorized, errors.New("sign in first"))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), whoKey{}, p))
		}
		next.ServeHTTP(w, r)
	})
}

type whoKey struct{}

// who returns who's signed in on this request (nil for nobody).
func (s *Server) who(r *http.Request) *auth.Principal {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return nil
	}
	return s.auth.Who(c.Value)
}

// user returns the name of who's signed in on this request ("" for nobody).
func (s *Server) user(r *http.Request) string {
	if p := s.who(r); p != nil {
		return p.Name
	}
	return ""
}

// me returns who made an API request (set by secure).
func me(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(whoKey{}).(*auth.Principal)
	return p
}

// Levels a request needs on a server (see auth.RoleLevel).
const (
	levelSee   = 1 // see it and turn it on
	levelRun   = 2 // stop it, players, settings, console, back up
	levelCare  = 3 // update, restore, backup schedule, download and delete backups
	levelOwner = auth.OwnerLevel
)

var errNotAllowed = errors.New("your account can't do that on this server; ask the owner")

// ownerOnly wraps a handler that only the owner may use.
func ownerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := me(r); p == nil || !p.Owner {
			writeError(w, http.StatusForbidden, errors.New("only the panel's owner can do that"))
			return
		}
		h(w, r)
	}
}

// note records something someone did in the activity log.
func (s *Server) note(r *http.Request, srv *servers.Server, text string) {
	e := activity.Entry{Text: text}
	if p := me(r); p != nil {
		e.User = p.Name
	}
	if srv != nil {
		st := srv.Status()
		e.ServerID, e.Server = st.ID, st.Name
	}
	s.activity.Add(e)
}

// did notes text if err is nil, and returns err.
func (s *Server) did(r *http.Request, srv *servers.Server, err error, text string) error {
	if err == nil {
		s.note(r, srv, text)
	}
	return err
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.opts.Version, "hostIP": s.opts.HostIP, "listName": s.opts.ListName, "owner": s.auth.Username(),
		"panelUpdate": s.newerPanel(), "panelLatest": s.update.get()})
}

// listedServer is a server as the list shows it: its status and what the
// person asking may do with it.
type listedServer struct {
	servers.Status
	Access int `json:"access"`
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	out := []listedServer{}
	for _, st := range s.mgr.List() {
		if lv := p.Level(st.ID); lv > 0 {
			out = append(out, listedServer{st, lv})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Preview    bool   `json:"preview"`
		AcceptEULA bool   `json:"acceptEula"`
		Owner      string `json:"ownerGamertag"`
		Edition    string `json:"edition"`  // "bedrock" (default), "java" or "hytale"
		JavaName   string `json:"javaName"` // Java: the adder's Minecraft Java username
		MemoryMB   int    `json:"memoryMB"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad request"))
		return
	}
	p := me(r)
	if !p.Owner && !p.CanAdd {
		writeError(w, http.StatusForbidden, errors.New("your account can't add servers; ask the owner"))
		return
	}
	if !req.AcceptEULA {
		writeError(w, http.StatusBadRequest, errors.New("accept the Minecraft EULA to download the server"))
		return
	}
	if req.Edition == servers.TypeHytale {
		st, err := s.mgr.CreateHytale(req.Name, req.MemoryMB)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.finishCreate(w, r, p, st)
		return
	}
	if req.Edition == servers.TypeJava {
		var names []string
		if n := strings.TrimSpace(req.JavaName); n != "" {
			names = append(names, n)
			if p.Owner && n != s.mgr.Settings().OwnerJavaName {
				_ = s.mgr.SetOwnerJavaName(n) // remembered for the next Java server
			}
		}
		st, err := s.mgr.CreateJava(req.Name, req.MemoryMB, names...)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.finishCreate(w, r, p, st)
		return
	}
	var extra []string
	if gt := strings.TrimSpace(req.Owner); gt != "" {
		if p.Owner {
			// The owner's gamertag is a panel setting: it goes on every new server.
			if gt != s.mgr.Settings().OwnerGamertag {
				if err := s.mgr.SetOwnerGamertag(gt); err != nil {
					writeError(w, http.StatusBadRequest, err)
					return
				}
			}
		} else {
			extra = append(extra, gt) // someone else's: just this server
		}
	}
	st, err := s.mgr.Create(req.Name, req.Preview, extra...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.finishCreate(w, r, p, st)
}

// finishCreate gives a new server to the kid who added it and answers.
func (s *Server) finishCreate(w http.ResponseWriter, r *http.Request, p *auth.Principal, st servers.Status) {
	if !p.Owner {
		// Whoever adds a server looks after it (but can't delete it).
		if err := s.auth.Grant(p.Name, st.ID, auth.RoleCare); err != nil {
			s.log.Error("could not give the new server to its maker", "user", p.Name, "error", err)
		}
	}
	if srv, ok := s.mgr.Get(st.ID); ok {
		s.note(r, srv, "added the server")
	}
	s.log.Info("server created from the panel", "id", st.ID, "name", st.Name, "by", p.Name, "from", r.RemoteAddr)
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) serverAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	srv, ok := s.mgr.Get(id)
	lv := me(r).Level(id)
	if !ok || lv == 0 {
		writeError(w, http.StatusNotFound, errors.New("no such server"))
		return
	}
	action := r.PathValue("action")
	need := map[string]int{"start": levelSee, "stop": levelRun, "restart": levelRun, "command": levelRun, "update": levelCare}[action]
	if need == 0 {
		writeError(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	if lv < need {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	if srv.Importing() {
		writeError(w, http.StatusConflict, servers.ErrImporting)
		return
	}
	// Outside a server's hours only the owner can start it, and then it
	// stays open until it would next open anyway.
	if action == "start" || action == "restart" {
		if why := srv.ClosedNow(); why != "" {
			if !me(r).Owner {
				writeError(w, http.StatusConflict, errors.New("can't start it now: "+why))
				return
			}
			if st := srv.HoursNow(); st != nil && st.Opens != nil {
				if err := srv.KeepOpenUntil(*st.Opens); err != nil {
					writeError(w, http.StatusConflict, err)
					return
				}
				s.note(r, srv, "opened it outside its hours")
			}
		}
	}
	var err error
	switch action {
	case "start":
		err = s.did(r, srv, srv.Start(), "started the server")
	case "stop":
		err = s.did(r, srv, srv.Stop(), "stopped the server")
	case "restart":
		s.note(r, srv, "restarted the server")
		go func() { _ = srv.Restart() }()
	case "update":
		err = s.did(r, srv, srv.Update(), "started an update")
	case "command":
		var req struct {
			Command string `json:"command"`
		}
		if jerr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); jerr != nil {
			writeError(w, http.StatusBadRequest, errors.New("bad request"))
			return
		}
		err = s.did(r, srv, srv.Command(req.Command), "ran the command: "+strings.TrimSpace(req.Command))
	}
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, srv.Status())
}

// console streams a server's console as server-sent events: everything so
// far, then new lines as they happen.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	srv, ok := s.mgr.Get(id)
	lv := me(r).Level(id)
	if !ok || lv == 0 {
		writeError(w, http.StatusNotFound, errors.New("no such server"))
		return
	}
	if lv < levelRun {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	backlog, lines, stop := srv.Subscribe()
	defer stop()
	send := func(line string) {
		fmt.Fprintf(w, "data: %s\n\n", strings.NewReplacer("\r", " ", "\n", " ").Replace(line))
	}
	for _, l := range backlog {
		send(l)
	}
	flusher.Flush()
	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case l := <-lines:
			send(l)
			flusher.Flush()
		case <-keepAlive.C:
			if p := s.who(r); p == nil || p.Level(id) < levelRun {
				return // signed out, or no longer allowed
			}
			if cur, ok := s.mgr.Get(id); !ok || cur != srv {
				return // deleted (or deleted and brought back)
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

// certificate loads or creates the panel's self-signed certificate.
func (s *Server) certificate() (tls.Certificate, error) {
	certPath := filepath.Join(s.opts.DataDir, "panel-cert.pem")
	keyPath := filepath.Join(s.opts.DataDir, "panel-key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Block Party"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "blockparty"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	if ip := net.ParseIP(s.opts.HostIP); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	s.log.Info("created a self-signed certificate for the web panel; your browser will ask you to trust it once")
	return tls.X509KeyPair(certPEM, keyPEM)
}
