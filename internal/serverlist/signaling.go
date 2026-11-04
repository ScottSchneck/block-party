package serverlist

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sniffListener accepts TCP connections on one port and sorts them into plain
// HTTP and TLS by peeking at the first byte (0x16 starts a TLS handshake).
// That lets one port answer both "https://host:19132" and "http://host:19132",
// the first two addresses a console tries for a NetherNet join.
type sniffListener struct {
	net.Listener
	plain, tls chan net.Conn
	closed     chan struct{}
	once       sync.Once
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func newSniffListener(l net.Listener) *sniffListener {
	s := &sniffListener{Listener: l, plain: make(chan net.Conn), tls: make(chan net.Conn), closed: make(chan struct{})}
	go s.loop()
	return s
}

func (s *sniffListener) loop() {
	for {
		c, err := s.Listener.Accept()
		if err != nil {
			s.once.Do(func() { close(s.closed) })
			return
		}
		go func() {
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(c)
			first, err := br.Peek(1)
			_ = c.SetReadDeadline(time.Time{})
			if err != nil {
				_ = c.Close()
				return
			}
			pc := &peekedConn{Conn: c, r: br}
			ch := s.plain
			if first[0] == 0x16 {
				ch = s.tls
			}
			select {
			case ch <- pc:
			case <-s.closed:
				_ = c.Close()
			}
		}()
	}
}

type chanListener struct {
	ch     chan net.Conn
	closed chan struct{}
	addr   net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// startSignaling serves the NetherNet join endpoint (GET /v1/join and
// POST /v1/join/{id}) on TCP ListPort and on each extra port, in the mode
// chosen by SIGNALING. With LOG_LEVEL=debug every TCP connection is logged,
// so the log shows where a console tries to reach us even if it never sends
// a request.
func startSignaling(cfg *Config, handler http.Handler) error {
	var cert tls.Certificate
	serveTLS := cfg.Signaling == "auto" || cfg.Signaling == "tls"
	servePlain := cfg.Signaling == "auto" || cfg.Signaling == "http"
	if serveTLS {
		var err error
		if cert, err = loadOrCreateCert(cfg); err != nil {
			return err
		}
	}

	ports := append([]int{cfg.ListPort}, cfg.ExtraSignalingPorts...)
	for _, port := range ports {
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
		if err != nil {
			if port == cfg.ListPort {
				return fmt.Errorf("listen on TCP %d for NetherNet joins: %w", port, err)
			}
			slog.Warn("could not listen on an extra NetherNet port; continuing without it", "port", port, "error", err)
			continue
		}
		serveSignalingPort(cfg, loggingListener{Listener: ln, port: port}, port, handler, cert, servePlain, serveTLS)
	}

	names := "all names"
	if len(cfg.NetherNetNames) > 0 {
		names = strings.Join(sortedKeys(cfg.NetherNetNames), ", ")
	}
	slog.Info("NetherNet join endpoint listening", "tcpPorts", ports, "plainHTTP", servePlain, "https", serveTLS,
		"offeredFor", names)
	return nil
}

func serveSignalingPort(cfg *Config, ln net.Listener, port int, handler http.Handler, cert tls.Certificate, servePlain, serveTLS bool) {
	h := logRequests(port, filterNames(cfg, handler))
	sniff := newSniffListener(ln)
	plainL := &chanListener{ch: sniff.plain, closed: sniff.closed, addr: ln.Addr()}
	tlsRaw := &chanListener{ch: sniff.tls, closed: sniff.closed, addr: ln.Addr()}

	if servePlain {
		go func() { _ = (&http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}).Serve(plainL) }()
	} else {
		go drain(plainL, "plain HTTP join attempt refused (SIGNALING=tls)", port)
	}
	if !serveTLS {
		go drain(tlsRaw, "encrypted join attempt refused (SIGNALING=http)", port)
		return
	}
	tcfg := &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if !cfg.allowsNetherNet(hello.ServerName) {
				slog.Info("encrypted NetherNet join turned away (name not in NETHERNET_NAMES); the console should fall back to RakNet",
					"from", hello.Conn.RemoteAddr(), "port", port, "hostname", hello.ServerName)
				return nil, fmt.Errorf("NetherNet not offered for %q", hello.ServerName)
			}
			slog.Info("console started an encrypted NetherNet join", "from", hello.Conn.RemoteAddr(),
				"port", port, "hostname", hello.ServerName)
			return &cert, nil
		},
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(slogBridge("TLS handshake"), "", 0),
	}
	go func() { _ = srv.Serve(tls.NewListener(tlsRaw, tcfg)) }()
}

// loggingListener logs every TCP connection (at debug level) before anything
// is read from it.
type loggingListener struct {
	net.Listener
	port int
}

func (l loggingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		slog.Debug("TCP connection", "from", c.RemoteAddr(), "port", l.port)
	}
	return c, err
}

// filterNames answers 404 to consoles joining a name that isn't offered
// NetherNet. Mojang's guide says a non-2xx reply to the ping means the server
// doesn't support NetherNet, so the console should fall back to RakNet.
func filterNames(cfg *Config, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !cfg.allowsNetherNet(req.Host) {
			w.Header().Set("X-Reason", "NetherNet not offered for this name")
			http.NotFound(w, req)
			return
		}
		h.ServeHTTP(w, req)
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func drain(l *chanListener, msg string, port int) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		slog.Debug(msg, "from", c.RemoteAddr(), "port", port)
		_ = c.Close()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) { r.code = code; r.ResponseWriter.WriteHeader(code) }

func logRequests(port int, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		start := time.Now()
		h.ServeHTTP(rec, req)
		step := "HTTP request to the NetherNet endpoint"
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v1/join":
			step = "NetherNet ping (GET /v1/join)"
		case req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/v1/join/"):
			step = "NetherNet connection offer (POST /v1/join/{id})"
		}
		if rec.code == http.StatusNotFound && req.URL.Path == "/v1/join" {
			step += ", turned away: name not in NETHERNET_NAMES"
		}
		level := slog.LevelDebug
		if rec.code >= 400 {
			level = slog.LevelInfo // something went wrong: worth seeing
		}
		slog.Log(req.Context(), level, step, "from", req.RemoteAddr, "port", port, "method", req.Method, "path", req.URL.Path,
			"host", req.Host, "encrypted", req.TLS != nil, "status", rec.code,
			"took", time.Since(start).Round(time.Millisecond), "userAgent", req.UserAgent())
	})
}

// slogBridge turns net/http's error log (which reports failed TLS handshakes,
// including a console rejecting our certificate) into structured log lines.
func slogBridge(what string) *logAdapter { return &logAdapter{what: what} }

type logAdapter struct{ what string }

func (l *logAdapter) Write(p []byte) (int, error) {
	slog.Info(l.what+" did not complete", "detail", string(p))
	return len(p), nil
}

// loadOrCreateCert keeps a self-signed certificate in DATA_DIR so the console
// sees the same one after restarts.
func loadOrCreateCert(cfg *Config) (tls.Certificate, error) {
	certPath := filepath.Join(cfg.DataDir, "signaling-cert.pem")
	keyPath := filepath.Join(cfg.DataDir, "signaling-key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Game server list"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     cfg.RedirectNames,
		IPAddresses:  []net.IP{net.IP(cfg.ListIP.AsSlice())},
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
	if err := writeFile(certPath, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFile(keyPath, keyPEM); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// loadOrCreateIdentityKey keeps the NetherNet identity key in DATA_DIR. The
// console pins this key after the first "trust this server?" prompt, so it
// must survive restarts or players would be asked again.
func loadOrCreateIdentityKey(cfg *Config) (*ecdsa.PrivateKey, error) {
	path := filepath.Join(cfg.DataDir, "nethernet-identity.pem")
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, errors.New("nethernet-identity.pem is damaged; delete it to create a new one")
		}
		return x509.ParseECPrivateKey(block.Bytes)
	}
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}
	slog.Info("created a new NetherNet identity key; consoles may ask players to trust the server once", "file", path)
	return key, nil
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
