package scraper

import (
	"strings"
	"testing"
)

func TestCompileRedactsAndBuilds(t *testing.T) {
	cap := CaptureResult{
		Program: "milesight/demo-capture", Vendor: "Milesight", Steps: 3, OK: 3,
		Fixtures: []Fixture{
			{StepID: "device-info", Kind: "http", Method: "GET", Path: "/cgi-bin/x.cgi", Query: map[string]string{"action": "info"},
				Status: 200, ContentType: "application/json",
				Body: `{"serialNumber":"6C0012ABCDEF","macAddress":"1C:C3:16:00:00:01","ipAddress":"192.168.1.64"}`},
			{StepID: "snapshot", Kind: "http", Method: "GET", Path: "/snap.cgi", Status: 200, Binary: true, Bytes: 1000},
			{StepID: "rtsp-main", Kind: "rtsp", Method: "DESCRIBE", Path: "/main", Status: 200,
				Body: "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=framesize:96 1280-720\r\n"},
			{StepID: "line-crossing-event", Kind: "event", Stream: "line_crossing", Status: 200, ContentType: "application/json",
				Body: `{"eventType":"LineCrossing","serialNumber":"6C0012ABCDEF"}`},
		},
	}
	res, err := Compile(CompileInput{ProfileID: "acme/x", Version: "0.1.0", Name: "X", Vendor: "Milesight", Username: "admin", Host: "192.168.1.64"}, cap)
	if err != nil {
		t.Fatal(err)
	}
	prof := string(res.ProfileYAML)
	for _, secret := range []string{"6C0012ABCDEF", "1C:C3:16:00:00:01", "192.168.1.64"} {
		if strings.Contains(prof, secret) {
			t.Fatalf("the profile leaks %q", secret)
		}
	}
	if strings.Contains(string(res.FixturesYAML), "6C0012ABCDEF") {
		t.Fatalf("the fixtures leak the serial")
	}
	if res.Routes != 2 || res.Events != 1 || len(res.Streams) != 1 {
		t.Fatalf("result=%+v", res)
	}
	// Codec from the SDP, a snapshot route, the redacted body.
	if !strings.Contains(prof, "h265") || !strings.Contains(prof, "1280x720") || !strings.Contains(prof, "handler: snapshot") ||
		!strings.Contains(prof, "REDACTEDSERIAL") {
		t.Fatalf("profile missing expected content:\n%s", prof)
	}
}
