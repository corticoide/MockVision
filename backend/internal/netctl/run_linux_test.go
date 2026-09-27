package netctl

import "testing"

func TestLookupUserIsStrict(t *testing.T) {
	if uid, gid, err := lookupUser("10001"); err != nil || uid != 10001 || gid != 10001 {
		t.Fatalf("numeric: %d %d %v", uid, gid, err)
	}
	for _, name := range []string{"no-such-user-mockvision", "0", "root"} {
		if _, _, err := lookupUser(name); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
