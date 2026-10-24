// Command blockparty is Block Party, a home game server panel.
//
// It runs, in one program:
//   - the server manager, which installs, runs and updates game servers;
//   - the web panel (HTTPS, port 8443 by default);
//   - the console server list: built-in DNS plus the menu that lets Xbox,
//     PlayStation and Switch players join.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works even without the system's time zone files

	"github.com/ScottSchneck/block-party/internal/auth"
	"github.com/ScottSchneck/block-party/internal/notify"
	"github.com/ScottSchneck/block-party/internal/outside"
	"github.com/ScottSchneck/block-party/internal/serverlist"
	"github.com/ScottSchneck/block-party/internal/servers"
	"github.com/ScottSchneck/block-party/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: blockparty [-version]")
		fmt.Fprintln(os.Stderr, "       blockparty reset-password   make a code to set a new owner password")
		flag.PrintDefaults()
	}
	flag.Parse()
	dropRoot()
	if *showVersion {
		fmt.Println("blockparty", version)
		return
	}
	if flag.Arg(0) == "reset-password" {
		os.Exit(resetPassword())
	}

	cfg, err := serverlist.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup problem:", err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))
	slog.Info("Block Party", "version", version)
	cfg.LogListIP()

	webPort, err := envInt("WEB_PORT", 8443)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup problem:", err)
		os.Exit(2)
	}
	stopSeconds, err := envInt("STOP_TIMEOUT", 30)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup problem:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Game servers.
	notifier := notify.New(notify.Options{DataDir: cfg.DataDir})

	mgr := servers.New(servers.Options{
		Notify:      notifier.Send,
		DataDir:     cfg.DataDir,
		DownloadAPI: os.Getenv("BEDROCK_DOWNLOAD_API"),
		Version:     version,
		StopTimeout: time.Duration(stopSeconds) * time.Second,
		ImportDir:   envOr("IMPORT_DIR", "/import"),
		// Where Java servers come from; only changed for testing.
		PaperAPI:       os.Getenv("PAPER_API"),
		AdoptiumAPI:    os.Getenv("ADOPTIUM_API"),
		MojangVersions: os.Getenv("MOJANG_VERSIONS"),
		MojangProfiles: os.Getenv("MOJANG_PROFILES"),
		GeyserAPI:      os.Getenv("GEYSER_API"),
		HangarAPI:      os.Getenv("HANGAR_API"),
		XboxLookup:     os.Getenv("XBOX_LOOKUP"),
		ModrinthAPI:    os.Getenv("MODRINTH_API"),
		HytaleOAuth:    os.Getenv("HYTALE_OAUTH"),
		HytaleAccounts: os.Getenv("HYTALE_ACCOUNTS"),
		HytaleSessions: os.Getenv("HYTALE_SESSIONS"),
	})
	if err := mgr.Load(); err != nil {
		slog.Error("could not read the servers folder", "error", err)
		os.Exit(1)
	}
	zone, _ := time.Now().Zone()
	slog.Info("scheduled backups use the container's time zone; set TZ to change it", "timeZone", time.Local.String(), "abbrev", zone, "now", time.Now().Format("15:04"))

	// The console menu lists the servers the panel runs.
	cfg.PanelServers = func() []serverlist.PanelServer {
		var out []serverlist.PanelServer
		for _, j := range mgr.Joinable() {
			out = append(out, serverlist.PanelServer{Name: j.Name, Port: j.Port, Running: j.Running, NetherNetPort: j.NetherNetPort})
		}
		return out
	}

	// Players the menu sends to one of our servers show up under "Tried to
	// join" there if they aren't on its allowlist.
	// Friends outside the house.
	lookups := outside.DefaultLookupURLs
	switch v := strings.TrimSpace(os.Getenv("PUBLIC_IP_LOOKUP")); strings.ToLower(v) {
	case "":
	case "off", "none":
		lookups = nil
	default:
		lookups = strings.Split(v, ",")
	}
	dnsPort := 0
	if cfg.DNSEnabled {
		dnsPort = cfg.DNSPort
	}
	out := outside.New(outside.Options{
		DataDir:  cfg.DataDir,
		LocalIP:  cfg.ListIP,
		ListPort: cfg.ListPort,
		ListUDPPorts: func() [2]int {
			if !cfg.NetherNet {
				return [2]int{}
			}
			return [2]int{int(cfg.RTCPortMin), int(cfg.RTCPortMax)}
		}(),
		DNSPort:    dnsPort,
		StaticIP:   cfg.PublicIP,
		LookupURLs: lookups,
		GatewayURL: os.Getenv("UPNP_GATEWAY"),
		Servers:    web.OutsideServers(mgr),
	})
	// Game servers tell consoles where to connect over NetherNet: here at
	// home, and at the home's internet address while friends outside can
	// join. Set before any server starts.
	mgr.SetHostAddresses(func() servers.HostAddresses {
		h := servers.HostAddresses{Home: cfg.ListIP.String()}
		if i := out.Info(); i.Enabled && i.IP.IsValid() {
			h.Internet = i.IP.String()
		}
		return h
	})
	// With friends outside, work out the internet address before servers
	// start, so they tell consoles outside where to connect.
	if out.Settings().Enabled {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out.Refresh(c)
		cancel()
	}
	mgr.StartAutoStart()
	mgr.RunBackupSchedule()
	mgr.RunPlayTime()
	mgr.RunHytaleKeepAlive()

	cfg.OutsideInfo = func() serverlist.Outside {
		i := out.Info()
		return serverlist.Outside{Enabled: i.Enabled, Host: i.Host, IP: i.IP, Consoles: i.Consoles, Open: i.Open}
	}

	cfg.OnPick = func(gamertag, xuid string, verified bool, address string, port uint16) {
		if address == cfg.ListIP.String() || (cfg.PublicIP.IsValid() && address == cfg.PublicIP.String()) ||
			(out.Info().Host != "" && address == out.Info().Host) ||
			(out.Info().IP.IsValid() && address == out.Info().IP.String()) {
			mgr.NoteMenuPick(int(port), gamertag, xuid, verified)
		}
	}

	// Web panel.
	panel, err := web.New(web.Options{
		Port:     webPort,
		TLS:      envBool("WEB_TLS", true),
		Password: os.Getenv("PANEL_PASSWORD"),
		DataDir:  cfg.DataDir,
		HostIP:   cfg.ListIP.String(),
		ListName: cfg.ListName,
		Version:  version,
		Outside:  out,
		Notifier: notifier,
	}, mgr)
	if err != nil {
		slog.Error("web panel setup failed", "error", err)
		os.Exit(1)
	}

	var wg sync.WaitGroup
	failed := make(chan error, 2)
	wg.Add(4)
	go func() {
		defer wg.Done()
		out.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		notifier.Run(ctx)
	}()
	notifier.Send(notify.Event{Kind: notify.PanelStarted, Title: "Block Party started",
		Text: fmt.Sprintf("The panel (version %s) started, with %d server(s).", version, len(mgr.List()))})
	go func() {
		defer wg.Done()
		if err := panel.Run(ctx); err != nil {
			failed <- fmt.Errorf("web panel: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := serverlist.Run(ctx, cfg); err != nil {
			failed <- fmt.Errorf("server list: %w", err)
		}
	}()

	exitCode := 0
	select {
	case <-ctx.Done():
	case err := <-failed:
		slog.Error("stopping", "error", err)
		exitCode = 1
		stop()
	}
	slog.Info("shutting down; stopping game servers so their worlds are saved")
	mgr.StopAll()
	wg.Wait()
	os.Exit(exitCode)
}

// resetPassword makes a one-time code for setting a new owner password. Run
// it inside the container: docker exec blockparty blockparty reset-password
func resetPassword() int {
	dir := envOr("DATA_DIR", "/data")
	code, err := auth.WriteResetCode(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "couldn't make a reset code:", err)
		return 1
	}
	fmt.Println("Reset code:", code)
	fmt.Println()
	fmt.Println("Open the panel, choose \"Forgot your password?\" and enter this code with a new")
	fmt.Println("password. It works once. Every browser is signed out when it's used.")
	return 0
}

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive number, got %q", key, v)
	}
	return n, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
