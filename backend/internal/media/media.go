// Package media prepares what cameras stream: every asset is encoded once
// per rendition (codec, resolution, fps, GOP and bitrate) into a stream
// file plus a JPEG snapshot, cached on disk by hash. H.264 and H.265
// renditions hold a single group of pictures and MJPEG ones a JPEG frame;
// cameras loop them, so a still image costs almost no CPU.
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
)

// Codecs a rendition can use (D35).
const (
	CodecH264  = "h264"
	CodecH265  = "h265"
	CodecMJPEG = "mjpeg"
)

// Codecs lists the supported codecs.
var Codecs = []string{CodecH264, CodecH265, CodecMJPEG}

// recipes version the encoding of each codec: a change regenerates the
// cached renditions of that codec only.
var recipes = map[string]string{
	CodecH264:  "h264-gop-v1",
	CodecH265:  "h265-gop-v1",
	CodecMJPEG: "mjpeg-v1",
}

// MaxMJPEGSize is the largest width or height RTP can carry for JPEG
// (RFC 2435 counts them in blocks of 8 pixels in one byte).
const MaxMJPEGSize = 2040

// MaxImageBytes bounds uploaded images.
const MaxImageBytes = 20 << 20

// MaxImagePixels bounds the pixels of an uploaded image: an 8K picture,
// the largest rendition. A small file can hold a huge picture, and FFmpeg
// decodes all of it (audit B2).
const MaxImagePixels = 7680 * 4320

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
	case recipes[p.Codec] == "":
		return fmt.Errorf("codec %s is not supported; use h264, h265 or mjpeg", p.Codec)
	case p.Width < 16 || p.Height < 16 || p.Width > 7680 || p.Height > 4320 || p.Width%2 != 0 || p.Height%2 != 0:
		return fmt.Errorf("invalid resolution %dx%d", p.Width, p.Height)
	case p.Codec == CodecMJPEG && !MJPEGFits(p.Width, p.Height):
		return fmt.Errorf("MJPEG over RTSP carries at most %dx%d in multiples of 8 pixels; %dx%d does not fit", MaxMJPEGSize, MaxMJPEGSize, p.Width, p.Height)
	case p.FPS < 1 || p.FPS > 60:
		return fmt.Errorf("fps must be between 1 and 60")
	case p.GOP < 1 || p.GOP > 600:
		return fmt.Errorf("gop must be between 1 and 600 frames")
	case p.Bitrate < 16 || p.Bitrate > 100_000:
		return fmt.Errorf("bitrate must be between 16 and 100000 kbit/s")
	}
	return nil
}

// MJPEGFits reports whether RTP can carry a JPEG of that size.
func MJPEGFits(width, height int) bool {
	return width <= MaxMJPEGSize && height <= MaxMJPEGSize && width%8 == 0 && height%8 == 0
}

// Key identifies a rendition of an asset; it names the cache directory.
func (p Params) Key(assetSHA string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d|%d|%d|%d", recipes[p.Codec], assetSHA, p.Codec, p.Width, p.Height, p.FPS, p.GOP, p.Bitrate)
	return hex.EncodeToString(h.Sum(nil))
}

// Files of an encoded rendition.
type Files struct {
	Dir      string
	Stream   string
	Snapshot string
}

// Library stores assets and renditions under the data directory.
type Library struct {
	AssetsDir     string
	RenditionsDir string
	FFmpeg        string
	Timeout       time.Duration
	Threads       int
	// Sandbox is the MockVision binary: when set, FFmpeg runs through its
	// sandbox-exec command, confined to the image it reads and the
	// directory it writes (audit B9).
	Sandbox string
}

// access is what one FFmpeg run may read and write.
type access struct {
	read, write []string
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

// RenditionFiles returns the files of a rendition, encoded or not. The
// stream file is named after the codec: stream.h264, stream.h265 or
// stream.mjpeg.
func (l *Library) RenditionFiles(key, codec string) Files {
	dir := filepath.Join(l.RenditionsDir, key)
	return Files{Dir: dir, Stream: filepath.Join(dir, "stream."+codec), Snapshot: filepath.Join(dir, "snapshot.jpg")}
}

// Ready reports whether a rendition is already encoded on disk.
func (l *Library) Ready(key, codec string) bool {
	f := l.RenditionFiles(key, codec)
	for _, p := range []string{f.Stream, f.Snapshot} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// StreamReady reports whether the stream of a rendition is already on
// disk, the first half of Ready.
func (l *Library) StreamReady(key, codec string) bool {
	st, err := os.Stat(l.RenditionFiles(key, codec).Stream)
	return err == nil && st.Size() > 0
}

// Encode produces the stream and snapshot of a rendition from an image.
func (l *Library) Encode(ctx context.Context, src string, p Params, key string) (Files, error) {
	if err := l.EncodeStream(ctx, src, p, key, nil); err != nil {
		return l.RenditionFiles(key, p.Codec), err
	}
	return l.RenditionFiles(key, p.Codec), l.EncodeSnapshot(ctx, src, p, key)
}

func (l *Library) renditionDir(p Params, key string) (Files, error) {
	if err := p.Validate(); err != nil {
		return Files{}, err
	}
	files := l.RenditionFiles(key, p.Codec)
	if err := os.MkdirAll(files.Dir, 0o755); err != nil {
		return files, err
	}
	// Cameras read renditions with their own user, whatever the umask.
	if err := os.Chmod(files.Dir, 0o755); err != nil {
		return files, err
	}
	return files, nil
}

func scaleFilter(p Params) string {
	return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1", p.Width, p.Height, p.Width, p.Height)
}

// EncodeStream produces the stream file of a rendition, reporting the
// frames encoded so far out of p.GOP.
func (l *Library) EncodeStream(ctx context.Context, src string, p Params, key string, progress func(frames int)) error {
	files, err := l.renditionDir(p, key)
	if err != nil {
		return err
	}
	tmp := files.Stream + ".tmp"
	if p.Codec == CodecMJPEG {
		err = l.encodeMJPEG(ctx, src, p, tmp)
		if err == nil && progress != nil {
			progress(p.GOP)
		}
	} else {
		args := l.gopArgs(src, p, tmp)
		var out io.Writer
		if progress != nil {
			args = append([]string{"-progress", "pipe:1", "-nostats"}, args...)
			out = &progressWriter{fn: progress}
		}
		err = l.runTo(ctx, args, out, access{read: []string{src}, write: []string{files.Dir}})
	}
	if err != nil {
		return fmt.Errorf("encode stream: %w", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	if _, err := ParseStream(p.Codec, data); err != nil {
		return fmt.Errorf("encoded stream is unusable: %w", err)
	}
	return publish(tmp, files.Stream)
}

// gopArgs encode one closed group of pictures of p.GOP frames: a keyframe
// with its parameter sets, then predicted frames, no B-frames, one slice
// per picture and the bitrate capped, as a camera encoder does.
func (l *Library) gopArgs(src string, p Params, dst string) []string {
	gop := strconv.Itoa(p.GOP)
	rate := fmt.Sprintf("%dk", p.Bitrate)
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-loop", "1", "-framerate", strconv.Itoa(p.FPS)}
	args = append(args, imageInput(src)...)
	args = append(args,
		"-vf", scaleFilter(p)+",format=yuv420p",
		"-frames:v", gop,
	)
	switch p.Codec {
	case CodecH265:
		args = append(args,
			"-c:v", "libx265", "-preset", "veryfast", "-profile:v", "main",
			"-b:v", rate, "-maxrate", rate, "-bufsize", fmt.Sprintf("%dk", 2*p.Bitrate),
			"-x265-params", fmt.Sprintf("keyint=%s:min-keyint=%s:scenecut=0:bframes=0:repeat-headers=1:aud=0:info=0:log-level=error:pools=%d", gop, gop, l.Threads),
			"-an", "-f", "hevc", dst)
	default:
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage", "-profile:v", "main",
			"-g", gop, "-keyint_min", gop, "-sc_threshold", "0", "-bf", "0",
			"-b:v", rate, "-maxrate", rate, "-bufsize", fmt.Sprintf("%dk", 2*p.Bitrate),
			"-x264-params", "repeat-headers=1:aud=0:slices=1",
			"-threads", strconv.Itoa(l.Threads), "-an", "-f", "h264", dst)
	}
	return args
}

// encodeMJPEG writes the JPEG frame of an MJPEG rendition. Every frame of
// MJPEG is a whole picture, so a still image needs one; its quality is
// the best whose size keeps the stream within the bitrate.
func (l *Library) encodeMJPEG(ctx context.Context, src string, p Params, dst string) error {
	budget := p.Bitrate * 1000 / 8 / p.FPS // bytes per frame
	best, last := jpegWorstQuality, 0
	lo, hi := jpegBestQuality, jpegWorstQuality
	for lo <= hi {
		q := (lo + hi) / 2
		size, err := l.jpegAt(ctx, src, p, q, dst)
		if err != nil {
			return err
		}
		last = q
		if size <= budget {
			best, hi = q, q-1
		} else {
			lo = q + 1
		}
	}
	if last != best {
		_, err := l.jpegAt(ctx, src, p, best, dst)
		return err
	}
	return nil
}

// FFmpeg's JPEG quality scale: 2 is the best, 31 the smallest.
const (
	jpegBestQuality  = 2
	jpegWorstQuality = 31
)

// jpegAt encodes the frame at a quality and returns its size. The Huffman
// tables must be the standard ones: RTP receivers rebuild the JPEG headers
// with them (RFC 2435).
func (l *Library) jpegAt(ctx context.Context, src string, p Params, q int, dst string) (int, error) {
	args := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, imageInput(src)...)
	args = append(args,
		"-vf", scaleFilter(p)+",format=yuvj420p", "-frames:v", "1",
		"-c:v", "mjpeg", "-huffman", "default", "-q:v", strconv.Itoa(q),
		"-threads", strconv.Itoa(l.Threads), "-f", "mjpeg", dst,
	)
	if err := l.runTo(ctx, args, nil, access{read: []string{src}, write: []string{filepath.Dir(dst)}}); err != nil {
		return 0, err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return int(st.Size()), nil
}

// EncodeSnapshot produces the JPEG snapshot of a rendition.
func (l *Library) EncodeSnapshot(ctx context.Context, src string, p Params, key string) error {
	files, err := l.renditionDir(p, key)
	if err != nil {
		return err
	}
	tmpJPEG := files.Snapshot + ".tmp.jpg"
	snapArgs := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, imageInput(src)...)
	snapArgs = append(snapArgs, "-vf", scaleFilter(p), "-frames:v", "1", "-q:v", "3", "-threads", strconv.Itoa(l.Threads), tmpJPEG)
	if err := l.runTo(ctx, snapArgs, nil, access{read: []string{src}, write: []string{files.Dir}}); err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	return publish(tmpJPEG, files.Snapshot)
}

// publish moves a finished file into place, readable by cameras.
func publish(tmp, dst string) error {
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// progressWriter reads FFmpeg's -progress output and reports frame=N.
type progressWriter struct {
	fn      func(frames int)
	partial []byte
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
		if v, ok := strings.CutPrefix(line, "frame="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				w.fn(n)
			}
		}
	}
	if len(w.partial) > 4096 {
		w.partial = w.partial[:0]
	}
	return len(p), nil
}

// TestPattern writes a JPEG test card, used as the built-in asset.
func (l *Library) TestPattern(ctx context.Context, dst string) error {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=1",
		"-frames:v", "1", "-q:v", "2", dst,
	}
	return l.runTo(ctx, args, nil, access{write: []string{filepath.Dir(dst)}})
}

// imageInput reads src as a still image from a local file: the demuxer is
// forced and only the file protocol is allowed, so a crafted upload cannot
// make FFmpeg read other files or reach the network (audit B2).
func imageInput(src string) []string {
	return []string{"-protocol_whitelist", "file", "-f", "image2", "-i", src}
}

// runTo runs FFmpeg, sending its standard output to stdout when set, and
// confined to what acc names when the library has a sandbox.
func (l *Library) runTo(ctx context.Context, args []string, stdout io.Writer, acc access) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	name := l.FFmpeg
	if l.Sandbox != "" {
		wrapped := []string{"sandbox-exec"}
		for _, p := range acc.read {
			wrapped = append(wrapped, "--read", p)
		}
		for _, p := range acc.write {
			wrapped = append(wrapped, "--write", p)
		}
		args = append(append(wrapped, "--", l.FFmpeg), args...)
		name = l.Sandbox
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	// Once FFmpeg is killed, do not wait for its output pipes: a child it
	// left behind may still hold them.
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	err := cmd.Run()
	if parent.Err() != nil {
		return context.Cause(parent)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("ffmpeg timed out after %s", l.Timeout)
	}
	if err != nil {
		var ee *exec.Error
		var exit *exec.ExitError
		if errors.As(err, &ee) || (l.Sandbox != "" && errors.As(err, &exit) && exit.ExitCode() == 127) {
			return fmt.Errorf("ffmpeg not found (%s): install FFmpeg or set MOCKVISION_FFMPEG", l.FFmpeg)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffmpeg: %s", l.relative(msg))
	}
	return nil
}

// relative shortens the paths FFmpeg reports to the data directory, which
// errors shown in the panel need not reveal.
func (l *Library) relative(msg string) string {
	if l.AssetsDir == "" {
		return msg
	}
	return strings.ReplaceAll(msg, filepath.Dir(l.AssetsDir)+string(filepath.Separator), "")
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
	if info.Width*info.Height > MaxImagePixels {
		return ImageInfo{}, fmt.Errorf("image is %dx%d; at most %d megapixels (an 8K picture) are accepted", info.Width, info.Height, MaxImagePixels/1_000_000)
	}
	return info, nil
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
