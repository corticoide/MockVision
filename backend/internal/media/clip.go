package media

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"

	"github.com/corticoide/mockvision/sdk/engine"
)

// MaxClip bounds a recorded clip.
const MaxClip = 5 * time.Minute

// ErrNoClip is returned for a stream whose codec has no clip: MJPEG.
var ErrNoClip = errors.New("clips need an H.264 or H.265 stream")

// Clip writes d of a precoded stream as MPEG-TS, the way a camera records
// an event to its card: the stream's loop played from its keyframe, with
// timestamps that go on across loops.
func Clip(src *engine.VideoSource, d time.Duration) ([]byte, error) {
	if d <= 0 || d > MaxClip {
		return nil, fmt.Errorf("a clip lasts between 1 s and %s", MaxClip)
	}
	if len(src.AccessUnits) == 0 || src.Info.FPS <= 0 {
		return nil, errors.New("the stream is empty")
	}
	var track *mpegts.Track
	switch src.Info.Codec {
	case CodecH264:
		track = &mpegts.Track{Codec: &codecs.H264{}}
	case CodecH265:
		track = &mpegts.Track{Codec: &codecs.H265{}}
	default:
		return nil, ErrNoClip
	}
	var buf bytes.Buffer
	w := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{track}}
	if err := w.Initialize(); err != nil {
		return nil, err
	}
	frames := int(d.Seconds() * float64(src.Info.FPS))
	step := int64(90000 / src.Info.FPS)
	for i := 0; i < frames; i++ {
		au := src.AccessUnits[i%len(src.AccessUnits)]
		pts := int64(i) * step
		var err error
		if src.Info.Codec == CodecH265 {
			err = w.WriteH265(track, pts, pts, au)
		} else {
			err = w.WriteH264(track, pts, pts, au)
		}
		if err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
