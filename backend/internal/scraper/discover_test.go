package scraper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateCIDR(t *testing.T) {
	ok := []string{"192.168.1.0/24", "10.0.0.0/22", "127.0.0.1/32", "169.254.0.0/24"}
	for _, c := range ok {
		if _, err := ValidateCIDR(c); err != nil {
			t.Errorf("%s rejected: %v", c, err)
		}
	}
	bad := []string{"8.8.8.0/24", "1.2.3.4/16", "192.168.0.0/16", "nonsense", "2001:db8::/48"}
	for _, c := range bad {
		if _, err := ValidateCIDR(c); err == nil {
			t.Errorf("%s accepted", c)
		}
	}
}

func TestDiscoverSweep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("sweep sent %s", r.Method)
		}
		w.Header().Set("Server", "Dahua/2.0")
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	found, err := Discover(context.Background(), DiscoverOptions{CIDR: "127.0.0.1/32", Ports: []int{port}, Rate: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Host != "127.0.0.1" || found[0].Via != "sweep" || found[0].Vendor != "Dahua" {
		t.Fatalf("found=%+v", found)
	}
	if len(found[0].Services) != 1 || found[0].Services[0].Proto != "http" {
		t.Fatalf("services=%+v", found[0].Services)
	}
}
