// Package media prepares what cameras stream: every asset is encoded once
// per rendition (resolution, codec, fps, GOP and bitrate) into a single
// H.264 group of pictures plus a JPEG snapshot, cached on disk by hash.
// Cameras then loop the GOP, so a still image costs almost no CPU.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register decoders for ProbeImage
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

	"github.com/corticoide/mockvision/sdk/engine"
)

// EncoderVersion changes whenever the encoding recipe changes, so cached
// renditions are regenerated.
const EncoderVersion = "h264-gop-v1"

// MaxImageBytes bounds uploaded images.
const MaxImageBytes = 20 << 20

// Params define a rendition.
type Params struct {
	Codec   string
	Width   int
	Height  int
	FPS     int
	GOP     int
	Bitrate int // kbit/s
}

// Validate checks rendition parameters.
func (p Params) Validate() error {
	switch {
	case p.Codec != "h264":
		return fmt.Errorf("codec %s is not supported yet; only h264 is", p.Codec)
	case p.Width < 16 || p.Height < 16 || p.Width > 7680 || p.Height > 4320 || p.Width%2 != 0 || p.Height%2 != 0:
		return fmt.Errorf("invalid resolution %dx%d", p.Width, p.Height)
	case p.FPS < 1 || p.FPS > 60:
		return fmt.Errorf("fps must be between 1 and 60")
	case p.GOP < 1 || p.GOP > 600:
		return fmt.Errorf("gop must be between 1 and 600 frames")
	case p.Bitrate < 16 || p.Bitrate > 100_000:
		return fmt.Errorf("bitrate must be between 16 and 100000 kbit/s")
	}
	return nil
}

// Key identifies a rendition of an asset; it names the cache directory.
func (p Params) Key(assetSHA string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d|%d|%d|%d", EncoderVersion, assetSHA, p.Codec, p.Width, p.Height, p.FPS, p.GOP, p.Bitrate)
	return hex.EncodeToString(h.Sum(nil))
}

// Files of an encoded rendition.
type Files struct {
	Dir      string
	GOP      string
	Snapshot string
}

// Library stores assets and renditions under the data directory.
type Library struct {
	AssetsDir     string
	RenditionsDir string
	FFmpeg        string
	Timeout       time.Duration
	Threads       int
}

// NewLibrary prepares the directories under dataDir.
func NewLibrary(dataDir, ffmpeg string) (*Library, error) {
	l := &Library{
		AssetsDir:     filepath.Join(dataDir, "assets"),
		RenditionsDir: filepath.Join(dataDir, "renditions"),
		FFmpeg:        ffmpeg,
		Timeout:       3 * time.Minute,
		Threads:       2,
	}
	if l.FFmpeg == "" {
		l.FFmpeg = "ffmpeg"
	}
	// Cameras read renditions with their own unprivileged user.
	for _, d := range []string{l.AssetsDir, l.RenditionsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
		_ = os.Chmod(d, 0o755)
	}
	return l, nil
}

// AssetPath returns where an asset with the given hash and extension lives.
func (l *Library) AssetPath(sha, ext string) string {
	return filepath.Join(l.AssetsDir, sha+ext)
}

// RenditionFiles returns the files of a rendition, encoded or not.
func (l *Library) RenditionFiles(key string) Files {
	dir := filepath.Join(l.RenditionsDir, key)
	return Files{Dir: dir, GOP: filepath.Join(dir, "stream.h264"), Snapshot: filepath.Join(dir, "snapshot.jpg")}
}

// Ready reports whether a rendition is already encoded on disk.
func (l *Library) Ready(key string) bool {
	f := l.RenditionFiles(key)
	for _, p := range []string{f.GOP, f.Snapshot} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// Encode produces the GOP and snapshot of a rendition from an image.
func (l *Library) Encode(ctx context.Context, src string, p Params, key string) (Files, error) {
	if err := p.Validate(); err != nil {
		return Files{}, err
	}
	files := l.RenditionFiles(key)
	if err := os.MkdirAll(files.Dir, 0o755); err != nil {
		return files, err
	}
	// Cameras read renditions with their own user, whatever the umask.
	if err := os.Chmod(files.Dir, 0o755); err != nil {
		return files, err
	}
	scale := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1", p.Width, p.Height, p.Width, p.Height)
	tmpGOP := files.GOP + ".tmp"
	gopArgs := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-loop", "1", "-framerate", strconv.Itoa(p.FPS), "-i", src,
		"-vf", scale + ",format=yuv420p",
		"-frames:v", strconv.Itoa(p.GOP),
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage", "-profile:v", "main",
		"-g", strconv.Itoa(p.GOP), "-keyint_min", strconv.Itoa(p.GOP), "-sc_threshold", "0", "-bf", "0",
		"-b:v", fmt.Sprintf("%dk", p.Bitrate), "-maxrate", fmt.Sprintf("%dk", p.Bitrate), "-bufsize", fmt.Sprintf("%dk", 2*p.Bitrate),
		"-x264-params", "repeat-headers=1:aud=0:slices=1",
		"-threads", strconv.Itoa(l.Threads), "-an", "-f", "h264", tmpGOP,
	}
	if err := l.run(ctx, gopArgs); err != nil {
		return files, fmt.Errorf("encode stream: %w", err)
	}
	data, err := os.ReadFile(tmpGOP)
	if err != nil {
		return files, err
	}
	if _, err := ParseGOP(data); err != nil {
		return files, fmt.Errorf("encoded stream is unusable: %w", err)
	}
	tmpJPEG := files.Snapshot + ".tmp.jpg"
	snapArgs := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", src, "-vf", scale, "-frames:v", "1", "-q:v", "3", "-threads", strconv.Itoa(l.Threads), tmpJPEG,
	}
	if err := l.run(ctx, snapArgs); err != nil {
		return files, fmt.Errorf("encode snapshot: %w", err)
	}
	for _, mv := range [][2]string{{tmpGOP, files.GOP}, {tmpJPEG, files.Snapshot}} {
		if err := os.Chmod(mv[0], 0o644); err != nil {
			return files, err
		}
		if err := os.Rename(mv[0], mv[1]); err != nil {
			return files, err
		}
	}
	return files, nil
}

// TestPattern writes a JPEG test card, used as the built-in asset.
func (l *Library) TestPattern(ctx context.Context, dst string) error {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=1",
		"-frames:v", "1", "-q:v", "2", dst,
	}
	return l.run(ctx, args)
}

func (l *Library) run(ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.FFmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("ffmpeg timed out after %s", l.Timeout)
	}
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return fmt.Errorf("ffmpeg not found (%s): install FFmpeg or set MOCKVISION_FFMPEG", l.FFmpeg)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffmpeg: %s", msg)
	}
	return nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, _ = l.w.Write(q)
	return len(p), nil
}

// ImageInfo describes an uploaded image.
type ImageInfo struct {
	Format string
	MIME   string
	Ext    string
	Width  int
	Height int
}

// ProbeImage reads the header of a JPEG or PNG image.
func ProbeImage(r io.Reader) (ImageInfo, error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return ImageInfo{}, errors.New("the file is not a JPEG or PNG image")
	}
	info := ImageInfo{Format: format, Width: cfg.Width, Height: cfg.Height}
	switch format {
	case "jpeg":
		info.MIME, info.Ext = "image/jpeg", ".jpg"
	case "png":
		info.MIME, info.Ext = "image/png", ".png"
	default:
		return ImageInfo{}, fmt.Errorf("unsupported image format %s", format)
	}
	if info.Width < 16 || info.Height < 16 || info.Width > 16384 || info.Height > 16384 {
		return ImageInfo{}, fmt.Errorf("image is %dx%d; it must be between 16x16 and 16384x16384", info.Width, info.Height)
	}
	return info, nil
}

// ParseGOP splits an H.264 Annex-B stream into access units and extracts
// the parameter sets. The stream must start with a keyframe.
func ParseGOP(data []byte) (*engine.VideoSource, error) {
	var nalus h264.AnnexB
	if err := nalus.Unmarshal(data); err != nil {
		return nil, err
	}
	src := &engine.VideoSource{}
	var pending [][]byte
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		switch h264.NALUType(n[0] & 0x1f) {
		case h264.NALUTypeSPS:
			if src.SPS == nil {
				src.SPS = n
			}
			pending = append(pending, n)
		case h264.NALUTypePPS:
			if src.PPS == nil {
				src.PPS = n
			}
			pending = append(pending, n)
		case h264.NALUTypeAccessUnitDelimiter:
			// dropped; the RTP payload does not need it
		case h264.NALUTypeNonIDR, h264.NALUTypeIDR:
			au := append(pending, n)
			pending = nil
			src.AccessUnits = append(src.AccessUnits, au)
		default:
			pending = append(pending, n) // SEI and others travel with the next frame
		}
	}
	if len(src.AccessUnits) == 0 {
		return nil, errors.New("no frames found")
	}
	if src.SPS == nil || src.PPS == nil {
		return nil, errors.New("missing SPS or PPS")
	}
	if !isIDR(src.AccessUnits[0]) {
		return nil, errors.New("the stream does not start with a keyframe")
	}
	return src, nil
}

func isIDR(au [][]byte) bool {
	for _, n := range au {
		if len(n) > 0 && h264.NALUType(n[0]&0x1f) == h264.NALUTypeIDR {
			return true
		}
	}
	return false
}

// SHA256File hashes a file.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
