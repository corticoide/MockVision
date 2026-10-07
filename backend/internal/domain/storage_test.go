package domain

import (
	"errors"
	"testing"
	"time"
)

func TestValidateStorage(t *testing.T) {
	nas := []string{"nfs", "smb"}
	for _, ok := range []Storage{
		{Kind: StorageNone},
		{Kind: StorageSD, SizeMB: 64},
		{Kind: StorageNAS, NASURL: "nfs://10.0.0.5/export/cams"},
		{Kind: StorageNAS, NASURL: "smb://nas.local:4455/cams/gate"},
	} {
		if err := ValidateStorage(ok, 1024, nas); err != nil {
			t.Fatalf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []Storage{
		{Kind: "tape"},
		{Kind: StorageSD, SizeMB: 63},
		{Kind: StorageSD, SizeMB: 2048},
		{Kind: StorageNAS, NASURL: "ftp://nas/x"},
		{Kind: StorageNAS, NASURL: "smb://nas.local"},
		{Kind: StorageNAS, NASURL: "smb://u:p@nas.local/x"},
		{Kind: StorageNAS, NASURL: "nfs://nas:99999/x"},
	} {
		if err := ValidateStorage(bad, 1024, nas); err == nil {
			t.Fatalf("%+v was accepted", bad)
		}
	}
	if ValidateStorage(Storage{Kind: StorageSD, SizeMB: 64}, 0, nas) == nil {
		t.Fatal("a card in a model without a slot")
	}
	if ValidateStorage(Storage{Kind: StorageNAS, NASURL: "smb://nas/x"}, 1024, []string{"nfs"}) == nil {
		t.Fatal("SMB in a model that records over NFS only")
	}
}

func TestAdmitDisk(t *testing.T) {
	if err := AdmitDisk(1<<30, 512<<20, 256); err != nil {
		t.Fatal(err)
	}
	err := AdmitDisk(1<<30, 900<<20, 256)
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Code != RejectDisk {
		t.Fatalf("%v", err)
	}
	if AdmitDisk(100<<20, 200<<20, 64) == nil {
		t.Fatal("promises beyond the disk")
	}
}

func TestRecordingNames(t *testing.T) {
	at := time.Date(2026, 10, 7, 14, 30, 5, 0, time.FixedZone("ART", -3*3600))
	if got := RecordingName("custom:people_counting", "01M4C8X0P3E9CBY75CPEJ5QXNN", at, "jpg"); got != "20261007/173005_custom-people_counting_j5qxnn.jpg" {
		t.Fatal(got)
	}
	for _, name := range []string{"20261007/x.jpg", "a/b/c.ts", "SN123/20261007/x.ts"} {
		if !ValidRecordingName(name) {
			t.Fatalf("%s was refused", name)
		}
	}
	for _, name := range []string{"", "/etc/passwd", "../x", "a/../b", "a//b", "a/./b", `a\b`, "a/"} {
		if ValidRecordingName(name) {
			t.Fatalf("%q was accepted", name)
		}
	}
}
