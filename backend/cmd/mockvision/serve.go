package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/corticoide/mockvision/backend/internal/api"
	"github.com/corticoide/mockvision/backend/internal/app"
	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/netctl/privdrop"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/frontend"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// serve runs the main service: panel, API, database, reconciler and the
// supervision of cameras. It never keeps privileges.
func serve(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	helperFD := fs.Int("helper-fd", -1, "descriptor of the network helper socket (set by run)")
	uid := fs.Int("uid", -1, "user to switch to when started as root (set by run)")
	gid := fs.Int("gid", -1, "group to switch to")
	defaultData := "./data"
	if os.Getenv("MOCKVISION_DATA") == "" && fileExists("/data") {
		defaultData = "/data"
	}
	dataDir := fs.String("data", env("MOCKVISION_DATA", defaultData), "data directory (database, assets, renditions)")
	listen := fs.String("listen", env("MOCKVISION_LISTEN", ":8080"), "address of the panel and API")
	netMode := fs.String("net", env("MOCKVISION_NET", ""), "camera runtime: netns (needs mockvision run) or local (development)")
	parent := fs.String("parent", env("MOCKVISION_PARENT_IF", ""), "parent interface of cameras (default: the default route's)")
	ffmpeg := fs.String("ffmpeg", env("MOCKVISION_FFMPEG", "ffmpeg"), "FFmpeg binary")
	origins := fs.String("allowed-origins", env("MOCKVISION_ALLOWED_ORIGINS", ""), "extra origins (host:port, comma separated), for a dev server")
	secure := fs.Bool("secure-cookies", os.Getenv("MOCKVISION_SECURE_COOKIES") == "1", "mark cookies Secure (behind HTTPS)")
	hosts := fs.String("allowed-hosts", env("MOCKVISION_ALLOWED_HOSTS", ""), "host names the panel answers to (comma separated); empty accepts any")
	proxies := fs.String("trusted-proxies", env("MOCKVISION_TRUSTED_PROXIES", ""), "reverse proxies (IPs or CIDRs, comma separated) whose X-Forwarded-For is trusted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid >= 0 {
		if err := privdrop.Drop(*uid, *gid); err != nil {
			log.Error("cannot drop privileges", "error", err)
			return 1
		}
	}
	if os.Geteuid() == 0 {
		log.Warn("the main service is running as root; use mockvision run, which drops privileges")
	}
	// The database, packages and the node key stay private to the service;
	// renditions are made readable for cameras explicitly.
	syscall.Umask(0o077)
	mode := *netMode
	if mode == "" {
		mode = "local"
		if *helperFD >= 0 {
			mode = "netns"
		}
	}

	exe, err := os.Executable()
	if err != nil {
		log.Error("cannot find own executable", "error", err)
		return 1
	}
	exe, _ = filepath.EvalSymlinks(exe)

	var rt netctl.Runtime
	var helperGone <-chan struct{}
	switch mode {
	case "netns":
		if *helperFD < 0 {
			log.Error("netns mode needs the network helper: start the node with mockvision run")
			return 2
		}
		hr, err := netctl.NewHelperRuntime(os.NewFile(uintptr(*helperFD), "helper"), log.With("component", "helper-client"))
		if err != nil {
			log.Error("network helper", "error", err)
			return 1
		}
		rt, helperGone = hr, hr.Closed()
	case "local":
		lr := netctl.NewLocalRuntime(exe, log.With("component", "local-runtime"))
		defer lr.Shutdown()
		rt = lr
		log.Warn("local mode: cameras run on 127.0.0.1 without their own IP or MAC; use it for development only")
	default:
		log.Error("unknown --net mode", "mode", mode)
		return 2
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Error("data directory", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	st, err := store.Open(ctx, filepath.Join(*dataDir, "db.sqlite"))
	if err != nil {
		log.Error("database", "error", err)
		return 1
	}
	defer st.Close()

	hub := api.NewHub()
	svc, err := app.New(app.Options{
		DataDir: *dataDir, FFmpeg: *ffmpeg, Exe: exe, ParentInterface: *parent, Listen: *listen, Runtime: rt, Log: log,
	}, st, hub)
	if err != nil {
		log.Error("service", "error", err)
		return 1
	}
	var allowed []string
	for _, o := range strings.Split(*origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed = append(allowed, o)
		}
	}
	trusted, err := parsePrefixes(*proxies)
	if err != nil {
		log.Error("trusted proxies", "error", err)
		return 2
	}
	var allowedHosts []string
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			allowedHosts = append(allowedHosts, h)
		}
	}
	srv := api.New(api.Config{AllowedOrigins: allowed, SecureCookies: *secure, TrustedProxies: trusted, AllowedHosts: allowedHosts},
		svc, hub, frontend.Dist(), log)
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("cannot listen", "address", *listen, "error", err)
		return 1
	}

	svcDone := make(chan error, 1)
	svcCtx, cancelSvc := context.WithCancel(context.Background())
	go func() { svcDone <- svc.Run(svcCtx) }()
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			stop()
		}
	}()
	log.Info("MockVision is ready", "version", buildinfo.Version, "listen", *listen, "runtime", rt.Kind(), "data", *dataDir)

	select {
	case <-ctx.Done():
	case <-helperGone:
		log.Error("the network helper went away; stopping")
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = httpSrv.Shutdown(shutdownCtx)
	cancel()
	cancelSvc()
	select {
	case <-svcDone:
	case <-time.After(20 * time.Second):
		log.Warn("cameras did not stop in time")
	}
	return 0
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// parsePrefixes reads a comma separated list of IPs and CIDRs.
func parsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an IP nor a CIDR", item)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}
