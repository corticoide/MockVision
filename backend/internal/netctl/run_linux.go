package netctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// RunMain is the entry point of "mockvision run", the process started by
// Docker or systemd. It becomes the network helper, the only privileged
// process, and starts the main service as an unprivileged child connected
// through a private socket. When the service exits, every camera is
// stopped and its namespace removed.
func RunMain(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	serviceUser := fs.String("service-user", envOr("MOCKVISION_SERVICE_USER", "mockvision"), "user of the main service (name or uid)")
	cameraUser := fs.String("camera-user", envOr("MOCKVISION_CAMERA_USER", "mockvision-cam"), "user of camera processes (name or uid)")
	dataDir := fs.String("data", envOr("MOCKVISION_DATA", "/data"), "data directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		log.Error("mockvision run must start as root: it keeps the network capabilities for the helper and drops them everywhere else. For development without privileges use: mockvision serve --net local")
		return 2
	}
	svcUID, svcGID, err := lookupUser(*serviceUser, 10001)
	if err != nil {
		log.Error("service user", "error", err)
		return 2
	}
	camUID, camGID, err := lookupUser(*cameraUser, 10002)
	if err != nil {
		log.Error("camera user", "error", err)
		return 2
	}
	if camUID == svcUID {
		log.Warn("cameras and the service share a user; use separate users so a camera cannot touch the database")
	}
	if err := prepareDataDir(*dataDir, svcUID, svcGID); err != nil {
		log.Error("data directory", "error", err)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		log.Error("cannot find own executable", "error", err)
		return 1
	}
	exe, _ = filepath.EvalSymlinks(exe)
	helperEnd, serviceEnd, err := seqPair()
	if err != nil {
		log.Error("helper socket", "error", err)
		return 1
	}
	serveArgs := append([]string{"serve", "--helper-fd", "3", "--uid", strconv.Itoa(svcUID), "--gid", strconv.Itoa(svcGID), "--data", *dataDir}, fs.Args()...)
	cmd := exec.Command(exe, serveArgs...)
	cmd.ExtraFiles = []*os.File{serviceEnd}
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	helper, err := NewHelper(helperEnd, HelperOptions{Exe: exe, CameraUID: camUID, CameraGID: camGID, Log: log.With("component", "nethelper")})
	helperEnd.Close()
	if err != nil {
		log.Error("network helper", "error", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		log.Error("cannot start the main service", "error", err)
		return 1
	}
	serviceEnd.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := helper.Serve(ctx); err != nil && ctx.Err() == nil {
			log.Debug("helper stopped serving", "error", err)
		}
	}()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var waitErr error
	select {
	case s := <-sigs:
		log.Info("stopping", "signal", s.String())
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case waitErr = <-waitDone:
		case <-time.After(25 * time.Second):
			_ = cmd.Process.Kill()
			waitErr = <-waitDone
		}
	case waitErr = <-waitDone:
	}
	cancel()
	helper.Shutdown()

	code := 0
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		code = ee.ExitCode()
		if code < 0 {
			code = 1
		}
	}
	return code
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// lookupUser resolves a user name or numeric uid; when the name does not
// exist, fallback is used as both uid and gid.
func lookupUser(name string, fallback int) (int, int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		if n <= 0 {
			return 0, 0, fmt.Errorf("uid %d is not allowed", n)
		}
		return n, n, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return fallback, fallback, nil
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil || uid == 0 {
		return 0, 0, fmt.Errorf("user %s must be an unprivileged user", name)
	}
	return uid, gid, nil
}

// prepareDataDir creates the data directory and gives it to the service.
func prepareDataDir(dir string, uid, gid int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}
