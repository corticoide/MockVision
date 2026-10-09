package scraper

// Recorder keeps each probe's request and response as a sanitized fixture,
// so a capture can be compiled into a profile and replayed. Feature 19
// fills it in; here it is the seam the prober writes to.
type Recorder struct{}

func (r *Recorder) http(method, url string, resp *Response) {}

func (r *Recorder) rtsp(method, url string, res *RTSPResult) {}
