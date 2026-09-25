// Package telemetry measures the node and its cameras. Node CPU, memory and
// the traffic of one network interface are sampled from /proc; camera
// metrics arrive with their heartbeats. Both
// are kept in memory for ten minutes, enough for the panel and for admission
// decisions based on sustained usage (D91, D92).
package telemetry

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Retention of in-memory samples.
const Retention = 10 * time.Minute

// NodeSample is one measurement of the node. Network rates are those of
// the interface the cameras hang from.
type NodeSample struct {
	At           time.Time `json:"at"`
	CPUPercent   float64   `json:"cpu_percent"`
	MemTotal     uint64    `json:"mem_total"`
	MemUsed      uint64    `json:"mem_used"`
	NetInterface string    `json:"net_interface"`
	NetRxBps     float64   `json:"net_rx_bps"`
	NetTxBps     float64   `json:"net_tx_bps"`
}

// NodeSampler samples the node periodically.
type NodeSampler struct {
	cpuCount int

	mu        sync.RWMutex
	samples   []NodeSample
	lastIdle  uint64
	lastTotal uint64
	iface     string
	lastNet   netCounters
}

type netCounters struct {
	at     time.Time
	iface  string
	rx, tx uint64
}

// NewNodeSampler returns a sampler; call Run to start it.
func NewNodeSampler() *NodeSampler {
	n := &NodeSampler{cpuCount: runtime.NumCPU()}
	n.sample()
	return n
}

// CPUCount is the number of CPUs.
func (n *NodeSampler) CPUCount() int { return n.cpuCount }

// SetInterface chooses the network interface whose traffic is measured.
func (n *NodeSampler) SetInterface(name string) {
	n.mu.Lock()
	n.iface = name
	n.mu.Unlock()
}

// Run samples every interval until ctx is done.
func (n *NodeSampler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.sample()
		}
	}
}

func (n *NodeSampler) sample() {
	s := NodeSample{At: time.Now()}
	idle, total, ok := readCPU()
	n.mu.Lock()
	defer n.mu.Unlock()
	if ok && n.lastTotal > 0 && total > n.lastTotal {
		busy := float64((total - n.lastTotal) - (idle - n.lastIdle))
		s.CPUPercent = busy / float64(total-n.lastTotal) * 100
	}
	if ok {
		n.lastIdle, n.lastTotal = idle, total
	}
	s.MemTotal, s.MemUsed = readMem()
	if n.iface != "" {
		s.NetInterface = n.iface
		if rx, tx, ok := readNet(n.iface); ok {
			cur := netCounters{at: s.At, iface: n.iface, rx: rx, tx: tx}
			last := n.lastNet
			if secs := cur.at.Sub(last.at).Seconds(); last.iface == cur.iface && secs > 0 && rx >= last.rx && tx >= last.tx {
				s.NetRxBps = float64(rx-last.rx) / secs
				s.NetTxBps = float64(tx-last.tx) / secs
			}
			n.lastNet = cur
		}
	}
	n.samples = append(n.samples, s)
	cut := 0
	for cut < len(n.samples) && time.Since(n.samples[cut].At) > Retention {
		cut++
	}
	n.samples = n.samples[cut:]
}

// Latest returns the last sample.
func (n *NodeSampler) Latest() NodeSample {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if len(n.samples) == 0 {
		return NodeSample{}
	}
	return n.samples[len(n.samples)-1]
}

// SustainedCPU averages CPU usage over the window.
func (n *NodeSampler) SustainedCPU(window time.Duration) float64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	var sum float64
	var count int
	for i := len(n.samples) - 1; i >= 0 && time.Since(n.samples[i].At) <= window; i-- {
		sum += n.samples[i].CPUPercent
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// History returns samples newer than since.
func (n *NodeSampler) History(since time.Time) []NodeSample {
	n.mu.RLock()
	defer n.mu.RUnlock()
	var out []NodeSample
	for _, s := range n.samples {
		if s.At.After(since) {
			out = append(out, s)
		}
	}
	return out
}

// readCPU reads the aggregate jiffies of /proc/stat.
func readCPU() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 8 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var v [8]uint64
	for i := 0; i < 8 && i+1 < len(fields); i++ {
		v[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
	}
	for _, x := range v {
		total += x
	}
	return v[3] + v[4], total, true
}

// readNet reads the received and sent bytes of an interface from
// /proc/net/dev.
func readNet(iface string) (rx, tx uint64, ok bool) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	return parseNetDev(bufio.NewScanner(f), iface)
}

func parseNetDev(sc *bufio.Scanner, iface string) (rx, tx uint64, ok bool) {
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found || strings.TrimSpace(name) != iface {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return 0, 0, false
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		return rx, tx, err1 == nil && err2 == nil
	}
	return 0, 0, false
}

// readMem returns total and used memory, used excluding reclaimable cache.
func readMem() (total, used uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return 0, 0
	}
	defer f.Close()
	var avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	if total > avail {
		used = total - avail
	}
	return total, used
}

// CameraSample is one heartbeat of a camera.
type CameraSample struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	RSSBytes   uint64    `json:"rss_bytes"`
	Clients    int       `json:"clients"`
	BytesIn    uint64    `json:"bytes_in"`
	BytesOut   uint64    `json:"bytes_out"`
	Requests   uint64    `json:"requests"`
}

// Cameras keeps recent samples of every camera.
type Cameras struct {
	mu   sync.RWMutex
	byID map[string][]CameraSample
}

// NewCameras returns an empty store.
func NewCameras() *Cameras {
	return &Cameras{byID: map[string][]CameraSample{}}
}

// Add records a sample.
func (c *Cameras) Add(id string, s CameraSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := append(c.byID[id], s)
	cut := 0
	for cut < len(list) && s.At.Sub(list[cut].At) > Retention {
		cut++
	}
	c.byID[id] = list[cut:]
}

// Latest returns the last sample of a camera.
func (c *Cameras) Latest(id string) (CameraSample, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := c.byID[id]
	if len(list) == 0 {
		return CameraSample{}, false
	}
	return list[len(list)-1], true
}

// History returns the samples of a camera newer than since.
func (c *Cameras) History(id string, since time.Time) []CameraSample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []CameraSample
	for _, s := range c.byID[id] {
		if s.At.After(since) {
			out = append(out, s)
		}
	}
	return out
}

// Remove forgets a camera.
func (c *Cameras) Remove(id string) {
	c.mu.Lock()
	delete(c.byID, id)
	c.mu.Unlock()
}

// Averages returns the mean RSS and CPU of the cameras' latest samples, the
// measured correction of the admission estimate.
func (c *Cameras) Averages() (rss uint64, cpu float64, n int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var rssSum uint64
	for _, list := range c.byID {
		if len(list) == 0 {
			continue
		}
		s := list[len(list)-1]
		if time.Since(s.At) > time.Minute {
			continue
		}
		rssSum += s.RSSBytes
		cpu += s.CPUPercent
		n++
	}
	if n == 0 {
		return 0, 0, 0
	}
	return rssSum / uint64(n), cpu / float64(n), n
}
