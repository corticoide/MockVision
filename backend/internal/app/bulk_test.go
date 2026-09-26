package app

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

func TestNextHost(t *testing.T) {
	subnet := netip.MustParsePrefix("192.168.1.0/24")
	a := netip.MustParseAddr
	used := map[netip.Addr]bool{a("192.168.1.11"): true, a("192.168.1.12"): true}
	if got, ok := nextHost(subnet, a("192.168.1.10"), used); !ok || got != a("192.168.1.13") {
		t.Fatalf("after .10: %v %v", got, ok)
	}
	// Wraps around, skipping the broadcast and network addresses.
	if got, ok := nextHost(subnet, a("192.168.1.254"), nil); !ok || got != a("192.168.1.1") {
		t.Fatalf("after .254: %v %v", got, ok)
	}
	small := netip.MustParsePrefix("10.0.0.0/30") // hosts .1 and .2
	if got, ok := nextHost(small, a("10.0.0.1"), map[netip.Addr]bool{a("10.0.0.1"): true}); !ok || got != a("10.0.0.2") {
		t.Fatalf("/30: %v %v", got, ok)
	}
	if _, ok := nextHost(small, a("10.0.0.1"), map[netip.Addr]bool{a("10.0.0.1"): true, a("10.0.0.2"): true}); ok {
		t.Fatal("a full subnet has no free host")
	}
	if _, ok := nextHost(netip.MustParsePrefix("10.0.0.7/32"), a("10.0.0.7"), nil); ok {
		t.Fatal("a /32 has no other host")
	}
}

func TestCameraFilter(t *testing.T) {
	c := &CameraView{
		Name: "Gate North", Serial: "MS-123", Tags: []string{"Parking", "lpr"},
		Profile: ProfileRef{ID: "milesight/demo", Version: "0.1.0"},
		Network: NetworkView{IP: "192.168.1.50", MAC: "02:aa:bb:cc:dd:ee"},
		Status:  StatusView{State: "running"},
	}
	for _, tc := range []struct {
		f    CameraFilter
		want bool
	}{
		{CameraFilter{}, true},
		{CameraFilter{State: "running"}, true},
		{CameraFilter{State: "error"}, false},
		{CameraFilter{Profile: "milesight/demo"}, true},
		{CameraFilter{Profile: "milesight/demo@0.1.0"}, true},
		{CameraFilter{Profile: "milesight/demo@0.2.0"}, false},
		{CameraFilter{Tag: "parking"}, true},
		{CameraFilter{Tag: "park"}, false},
		{CameraFilter{Query: "north"}, true},
		{CameraFilter{Query: "1.50"}, true},
		{CameraFilter{Query: "DD:EE"}, true},
		{CameraFilter{Query: "ms-1"}, true},
		{CameraFilter{Query: "LPR"}, true},
		{CameraFilter{Query: "south"}, false},
		{CameraFilter{Query: "north", State: "stopped"}, false},
	} {
		if got := tc.f.Match(c); got != tc.want {
			t.Errorf("%+v: got %v", tc.f, got)
		}
	}
}

func TestBulkCameras(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	a := createCamera(t, svc, "Bulk A", false)
	b := createCamera(t, svc, "Bulk B", false)

	if _, err := svc.BulkCameras(ctx, testActor, BulkInput{Action: "explode", IDs: []string{a.ID}}); err == nil {
		t.Fatal("unknown action accepted")
	}
	_, err := svc.BulkCameras(ctx, testActor, BulkInput{Action: BulkStart})
	wantInvalid(t, err, "ids")

	res, err := svc.BulkCameras(ctx, testActor, BulkInput{Action: BulkStart, IDs: []string{a.ID, b.ID, "missing", a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("duplicates are dropped: %d results", len(res))
	}
	if res[0].Err != nil || res[1].Err != nil || !errors.Is(res[2].Err, domain.ErrNotFound) {
		t.Fatalf("start: %v, %v, %v", res[0].Err, res[1].Err, res[2].Err)
	}
	waitState(t, svc, a.ID, domain.StateRunning)
	waitState(t, svc, b.ID, domain.StateRunning)

	list, err := svc.ListCameras(ctx, CameraFilter{State: "running"})
	if err != nil || len(list) != 2 {
		t.Fatalf("running cameras: %d, %v", len(list), err)
	}

	res, err = svc.BulkCameras(ctx, testActor, BulkInput{Action: BulkClone, IDs: []string{a.ID, b.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("clone %s: %v", r.ID, r.Err)
		}
	}
	if res[0].Camera.Name != "Bulk A-copy" || res[1].Camera.Name != "Bulk B-copy" || res[0].Camera.ID == a.ID {
		t.Fatalf("clone names: %s, %s", res[0].Camera.Name, res[1].Camera.Name)
	}
	again, err := svc.BulkCameras(ctx, testActor, BulkInput{Action: BulkClone, IDs: []string{a.ID}})
	if err != nil || again[0].Err != nil || again[0].Camera.Name != "Bulk A-copy-2" {
		t.Fatalf("second copy: %+v, %v", again, err)
	}
	copies := []string{res[0].Camera.ID, res[1].Camera.ID, again[0].Camera.ID}

	res, err = svc.BulkCameras(ctx, testActor, BulkInput{Action: BulkStop, IDs: []string{a.ID, b.ID}})
	if err != nil || res[0].Err != nil || res[1].Err != nil {
		t.Fatalf("stop: %+v, %v", res, err)
	}
	for _, r := range res {
		if r.Camera.Status.State != string(domain.StateStopped) {
			t.Fatalf("%s is %s after stop", r.Camera.Name, r.Camera.Status.State)
		}
	}

	res, err = svc.BulkCameras(ctx, testActor, BulkInput{Action: BulkDelete, IDs: copies})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Err != nil || r.Camera != nil {
			t.Fatalf("delete %s: %+v", r.ID, r)
		}
	}
	all, _ := svc.ListCameras(ctx, CameraFilter{})
	if len(all) != 2 {
		t.Fatalf("%d cameras left", len(all))
	}

	// Every camera changed by the bulk actions is audited one by one.
	audit, err := svc.ListAudit(ctx, AuditFilter{Action: "camera", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range audit.Items {
		counts[e.Action]++
		if e.Action == "camera.delete" && !strings.HasSuffix(e.Entity.Name, "-copy") && !strings.HasSuffix(e.Entity.Name, "-copy-2") {
			t.Fatalf("deleted camera keeps its name: %+v", e.Entity)
		}
	}
	if counts["camera.start"] != 2 || counts["camera.clone"] != 3 || counts["camera.stop"] != 2 || counts["camera.delete"] != 3 {
		t.Fatalf("audit counts: %v", counts)
	}
}

func TestFreeCloneName(t *testing.T) {
	svc, _ := newBareService(t)
	ctx := context.Background()
	long := strings.Repeat("é", domain.MaxCameraNameLength)
	name, err := svc.freeCloneName(ctx, long)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, "-copy") || len([]rune(name)) != domain.MaxCameraNameLength {
		t.Fatalf("long name: %q (%d runes)", name, len([]rune(name)))
	}
	if err := domain.ValidateCameraName(name); err != nil {
		t.Fatal(err)
	}
}
