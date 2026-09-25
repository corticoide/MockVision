package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

// CameraFilter selects cameras; empty fields match every camera. Query
// looks in the name, address, MAC, serial and tags.
type CameraFilter struct {
	Query   string
	State   string
	Profile string
	Tag     string
}

// Match reports whether a camera passes the filter. Profile matches the
// profile ID or ID@version; tags and text ignore case.
func (f CameraFilter) Match(c *CameraView) bool {
	if f.State != "" && c.Status.State != f.State {
		return false
	}
	if f.Profile != "" && c.Profile.ID != f.Profile && c.Profile.ID+"@"+c.Profile.Version != f.Profile {
		return false
	}
	if f.Tag != "" && !slices.ContainsFunc(c.Tags, func(t string) bool { return strings.EqualFold(t, f.Tag) }) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		fields := append([]string{c.Name, c.Network.IP, c.Network.MAC, c.Serial}, c.Tags...)
		return slices.ContainsFunc(fields, func(s string) bool { return strings.Contains(strings.ToLower(s), q) })
	}
	return true
}

// Bulk actions on cameras.
const (
	BulkStart   = "start"
	BulkStop    = "stop"
	BulkRestart = "restart"
	BulkClone   = "clone"
	BulkDelete  = "delete"
)

// MaxBulkCameras bounds one bulk request.
const MaxBulkCameras = 200

// bulkWorkers is how many cameras a bulk action handles at once: stopping
// waits for each namespace to go away, so a few run in parallel.
const bulkWorkers = 4

// BulkInput applies one action to several cameras. Clones get the next free
// name and address after their source and start when Start is set.
type BulkInput struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
	Start  bool     `json:"start"`
}

// BulkResult is the outcome for one camera. Camera is the camera after the
// action (the new copy for a clone); a deleted camera has none.
type BulkResult struct {
	ID     string
	Camera *CameraView
	Err    error
}

// BulkCameras applies an action to every listed camera and reports each
// outcome; one failure does not stop the others. Every change is audited
// as if it had been made one by one.
func (s *Service) BulkCameras(ctx context.Context, actor Actor, in BulkInput) ([]BulkResult, error) {
	var op func(ctx context.Context, id string) (*CameraView, error)
	workers := bulkWorkers
	switch in.Action {
	case BulkStart:
		op = func(ctx context.Context, id string) (*CameraView, error) { return s.StartCamera(ctx, actor, id) }
	case BulkStop:
		op = func(ctx context.Context, id string) (*CameraView, error) { return s.StopCamera(ctx, actor, id) }
	case BulkRestart:
		op = func(ctx context.Context, id string) (*CameraView, error) { return s.RestartCamera(ctx, actor, id) }
	case BulkDelete:
		op = func(ctx context.Context, id string) (*CameraView, error) { return nil, s.DeleteCamera(ctx, actor, id) }
	case BulkClone:
		// One at a time: each copy takes the next free name and address.
		workers = 1
		op = func(ctx context.Context, id string) (*CameraView, error) {
			return s.cloneNext(ctx, actor, id, in.Start)
		}
	default:
		return nil, domain.Invalid("action", "must be start, stop, restart, clone or delete")
	}
	ids := make([]string, 0, len(in.IDs))
	seen := map[string]bool{}
	for _, id := range in.IDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	switch {
	case len(ids) == 0:
		return nil, domain.Invalid("ids", "select at least one camera")
	case len(ids) > MaxBulkCameras:
		return nil, domain.Invalid("ids", "at most %d cameras at once", MaxBulkCameras)
	}

	out := make([]BulkResult, len(ids))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i] = BulkResult{ID: id, Err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			v, err := op(ctx, id)
			out[i] = BulkResult{ID: id, Camera: v, Err: err}
		}()
	}
	wg.Wait()
	return out, nil
}

// cloneNext clones a camera with the next free name ("<name>-copy",
// "<name>-copy-2"…) and the next free address of its subnet.
func (s *Service) cloneNext(ctx context.Context, actor Actor, id string, start bool) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	name, err := s.freeCloneName(ctx, b.cam.Name)
	if err != nil {
		return nil, err
	}
	in := CloneCameraInput{Name: name, Start: start, Network: NetworkInput{
		Parent: b.net.ParentIf, Netmask: b.net.Netmask, Gateway: b.net.Gateway,
	}}
	_ = json.Unmarshal([]byte(b.net.DnsJson), &in.Network.DNS)
	if s.rt.Kind() != "local" {
		ip, err := s.nextFreeIP(ctx, b.net.Ip, b.net.Netmask)
		if err != nil {
			return nil, err
		}
		in.Network.IP = ip
	}
	return s.CloneCamera(ctx, actor, id, in)
}

func (s *Service) freeCloneName(ctx context.Context, base string) (string, error) {
	for n := 1; n <= 99; n++ {
		suffix := "-copy"
		if n > 1 {
			suffix = fmt.Sprintf("-copy-%d", n)
		}
		stem := base
		for utf8.RuneCountInString(stem)+len(suffix) > domain.MaxCameraNameLength {
			_, size := utf8.DecodeLastRuneInString(stem)
			stem = stem[:len(stem)-size]
		}
		name := strings.TrimSpace(stem) + suffix
		if _, err := s.store.R().CameraIDByName(ctx, name); err != nil {
			if notFound(err) {
				return name, nil
			}
			return "", err
		}
	}
	return "", domain.Conflict("name", "no free name left for a copy of %s", base)
}

// nextFreeIP returns the first host address after ip, wrapping around its
// subnet, that no camera, gateway or node interface uses. Whether another
// device of the LAN answers on it is checked when the copy starts (RN-06).
func (s *Service) nextFreeIP(ctx context.Context, ip, netmask string) (string, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return "", domain.Invalid("network.ip", "the source camera has no IPv4 address to copy from")
	}
	bits := 24
	if netmask != "" {
		if bits, err = domain.MaskToPrefix(netmask); err != nil {
			return "", domain.Invalid("network.netmask", "%v", err)
		}
	}
	subnet := netip.PrefixFrom(addr, bits).Masked()
	used := map[netip.Addr]bool{}
	nets, err := s.store.R().ListCameraNetworks(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nets {
		if a, err := netip.ParseAddr(n.Ip); err == nil {
			used[a] = true
		}
		if a, err := netip.ParseAddr(n.Gateway); err == nil {
			used[a] = true
		}
	}
	if info, err := nodeInfo(); err == nil {
		if a, err := netip.ParseAddr(info.DefaultGateway); err == nil {
			used[a] = true
		}
		for _, i := range info.Interfaces {
			for _, a := range i.Addrs {
				if p, err := netip.ParsePrefix(a); err == nil {
					used[p.Addr()] = true
				}
			}
		}
	}
	if ip, ok := nextHost(subnet, addr, used); ok {
		return ip.String(), nil
	}
	return "", domain.Conflict("network.ip", "no free address left in %s", subnet)
}

// nextHost walks the host addresses of subnet after from, wrapping around,
// and returns the first one not in used.
func nextHost(subnet netip.Prefix, from netip.Addr, used map[netip.Addr]bool) (netip.Addr, bool) {
	size := uint64(1) << (32 - subnet.Bits())
	if size < 4 {
		return netip.Addr{}, false
	}
	base := subnet.Addr().As4()
	first := uint64(base[0])<<24 | uint64(base[1])<<16 | uint64(base[2])<<8 | uint64(base[3])
	f := from.As4()
	cur := uint64(f[0])<<24 | uint64(f[1])<<16 | uint64(f[2])<<8 | uint64(f[3])
	for i := uint64(1); i < size; i++ {
		off := (cur - first + i) % size
		if off == 0 || off == size-1 { // network and broadcast
			continue
		}
		v := first + off
		a := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if !used[a] {
			return a, true
		}
	}
	return netip.Addr{}, false
}
