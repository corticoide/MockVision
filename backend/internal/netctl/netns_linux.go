package netctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// netnsDir is where iproute2 keeps named namespaces, so
// "ip netns exec sim-<camera> ..." works for debugging.
const netnsDir = "/run/netns"

// namespace is a camera's network namespace. Named namespaces are
// bind-mounted under /run/netns; when mounting is not allowed (for example
// by an AppArmor profile) the namespace is anonymous and lives while the
// helper holds its descriptor.
type namespace struct {
	name  string
	named bool
	fd    netns.NsHandle
}

func (n *namespace) path() string { return filepath.Join(netnsDir, n.name) }

// onThread runs fn on a dedicated OS thread. If fn leaves the thread in
// another network namespace and it cannot be switched back, the thread is
// discarded instead of returning to Go's pool.
func onThread(fn func(orig netns.NsHandle) error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			errc <- err
			return // exits while locked: the thread is destroyed
		}
		defer orig.Close()
		ferr := fn(orig)
		if err := netns.Set(orig); err != nil {
			errc <- fmt.Errorf("cannot restore the helper namespace: %w", err)
			return
		}
		runtime.UnlockOSThread()
		errc <- ferr
	}()
	return <-errc
}

// inNamespace runs fn with the calling thread inside ns.
func inNamespace(ns netns.NsHandle, fn func() error) error {
	return onThread(func(netns.NsHandle) error {
		if err := netns.Set(ns); err != nil {
			return err
		}
		return fn()
	})
}

// createNamespace creates a network namespace with the given name.
func createNamespace(name string) (*namespace, error) {
	ns := &namespace{name: name}
	path := ns.path()
	mountable := true
	if err := os.MkdirAll(netnsDir, 0o755); err != nil {
		mountable = false
	}
	if mountable {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
		switch {
		case errors.Is(err, os.ErrExist):
			return nil, errorf(CodeAlreadyExist, "network namespace %s already exists", name)
		case err != nil:
			mountable = false
		default:
			f.Close()
		}
	}
	err := onThread(func(netns.NsHandle) error {
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("unshare: %w", err)
		}
		if mountable {
			src := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
			if err := unix.Mount(src, path, "none", unix.MS_BIND, ""); err == nil {
				ns.named = true
			}
		}
		h, err := netns.Get()
		if err != nil {
			return err
		}
		ns.fd = h
		return nil
	})
	if !ns.named && mountable {
		_ = os.Remove(path)
	}
	if err != nil {
		if ns.fd.IsOpen() {
			ns.fd.Close()
		}
		if ns.named {
			_ = unix.Unmount(path, unix.MNT_DETACH)
			_ = os.Remove(path)
		}
		return nil, err
	}
	return ns, nil
}

// delete releases the namespace; the kernel destroys it, and the camera's
// interface with it, once no process uses it.
func (n *namespace) delete() error {
	if n.fd.IsOpen() {
		n.fd.Close()
	}
	if !n.named {
		return nil
	}
	if err := unix.Unmount(n.path(), unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unmount %s: %w", n.path(), err)
	}
	if err := os.Remove(n.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// removeStaleNamespaces deletes sim-* namespaces left by a previous run.
func removeStaleNamespaces() []string {
	entries, err := os.ReadDir(netnsDir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "sim-") {
			continue
		}
		n := &namespace{name: e.Name(), named: true}
		if err := n.delete(); err == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}
