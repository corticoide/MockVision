package media

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/jpeg"

	"github.com/corticoide/mockvision/sdk/engine"
)

// maxFrames bounds the frames of a stream file: 600 is the largest GOP.
const maxFrames = 600

// ParseStream reads the stream file of a rendition into the frames a
// server engine loops. H.264 and H.265 streams must start with a keyframe
// and carry their parameter sets; MJPEG streams are a series of JPEG
// images that RTP can carry.
func ParseStream(codec string, data []byte) (*engine.VideoSource, error) {
	switch codec {
	case CodecH264:
		return parseH264(data)
	case CodecH265:
		return parseH265(data)
	case CodecMJPEG:
		return parseMJPEG(data)
	}
	return nil, fmt.Errorf("codec %s is not supported", codec)
}

// nal says where a NAL unit belongs when grouping them into pictures.
type nal int

const (
	nalDrop    nal = iota // not sent: access unit delimiters
	nalPrefix             // parameter sets and SEI: open the next picture
	nalSuffix             // suffix SEI, end of sequence: close the picture
	nalSlice              // a slice that continues the current picture
	nalPicture            // the first slice of a picture
)

// accessUnits groups the NAL units of an Annex-B stream into pictures.
func accessUnits(data []byte, classify func([]byte) nal) ([][][]byte, error) {
	nalus, err := splitAnnexB(data)
	if err != nil {
		return nil, err
	}
	var (
		aus      [][][]byte
		cur      [][]byte
		hasSlice bool
	)
	flush := func() {
		if hasSlice {
			aus = append(aus, cur)
			cur, hasSlice = nil, false
		}
	}
	for _, n := range nalus {
		switch classify(n) {
		case nalPrefix:
			flush()
			cur = append(cur, n)
		case nalSuffix:
			cur = append(cur, n)
		case nalPicture:
			flush()
			cur = append(cur, n)
			hasSlice = true
		case nalSlice:
			cur = append(cur, n)
			hasSlice = true
		}
	}
	flush()
	if len(aus) == 0 {
		return nil, errors.New("no frames found")
	}
	if len(aus) > maxFrames {
		return nil, fmt.Errorf("the stream has %d frames; at most %d fit", len(aus), maxFrames)
	}
	return aus, nil
}

func parseH264(data []byte) (*engine.VideoSource, error) {
	src := &engine.VideoSource{}
	aus, err := accessUnits(data, func(n []byte) nal {
		switch h264.NALUType(n[0] & 0x1f) {
		case h264.NALUTypeSPS:
			if src.SPS == nil {
				src.SPS = n
			}
		case h264.NALUTypePPS:
			if src.PPS == nil {
				src.PPS = n
			}
		case h264.NALUTypeAccessUnitDelimiter:
			return nalDrop
		case h264.NALUTypeNonIDR, h264.NALUTypeIDR, h264.NALUTypeDataPartitionA:
			// first_mb_in_slice is ue(v) right after the header: 0, the
			// first slice of a picture, is the single bit 1.
			if len(n) > 1 && n[1]&0x80 != 0 {
				return nalPicture
			}
			return nalSlice
		case h264.NALUTypeDataPartitionB, h264.NALUTypeDataPartitionC:
			return nalSlice
		case h264.NALUTypeEndOfSequence, h264.NALUTypeEndOfStream, h264.NALUTypeFillerData:
			return nalSuffix
		}
		return nalPrefix
	})
	if err != nil {
		return nil, err
	}
	if src.SPS == nil || src.PPS == nil {
		return nil, errors.New("missing SPS or PPS")
	}
	if !hasNALU(aus[0], func(n []byte) bool { return h264.NALUType(n[0]&0x1f) == h264.NALUTypeIDR }) {
		return nil, errors.New("the stream does not start with a keyframe")
	}
	src.AccessUnits = aus
	return src, nil
}

// h265Type returns the type of an H.265 NAL unit.
func h265Type(n []byte) h265.NALUType { return h265.NALUType((n[0] >> 1) & 0x3f) }

func parseH265(data []byte) (*engine.VideoSource, error) {
	src := &engine.VideoSource{}
	aus, err := accessUnits(data, func(n []byte) nal {
		if len(n) < 2 {
			return nalDrop
		}
		switch t := h265Type(n); {
		case t == h265.NALUType_VPS_NUT:
			if src.VPS == nil {
				src.VPS = n
			}
		case t == h265.NALUType_SPS_NUT:
			if src.SPS == nil {
				src.SPS = n
			}
		case t == h265.NALUType_PPS_NUT:
			if src.PPS == nil {
				src.PPS = n
			}
		case t == h265.NALUType_AUD_NUT:
			return nalDrop
		case t <= 31: // VCL
			// first_slice_segment_in_pic_flag opens the slice header.
			if len(n) > 2 && n[2]&0x80 != 0 {
				return nalPicture
			}
			return nalSlice
		case t == h265.NALUType_SUFFIX_SEI_NUT, t == h265.NALUType_EOS_NUT, t == h265.NALUType_EOB_NUT, t == h265.NALUType_FD_NUT:
			return nalSuffix
		}
		return nalPrefix
	})
	if err != nil {
		return nil, err
	}
	if src.VPS == nil || src.SPS == nil || src.PPS == nil {
		return nil, errors.New("missing VPS, SPS or PPS")
	}
	if !hasNALU(aus[0], func(n []byte) bool { t := h265Type(n); return t >= 16 && t <= 23 }) {
		return nil, errors.New("the stream does not start with a keyframe")
	}
	src.AccessUnits = aus
	return src, nil
}

func hasNALU(au [][]byte, match func([]byte) bool) bool {
	for _, n := range au {
		if len(n) > 0 && match(n) {
			return true
		}
	}
	return false
}

// maxNALUs bounds the NAL units of a stream file: the frames with their
// parameter sets and SEI.
const maxNALUs = 4 * maxFrames

// splitAnnexB splits an Annex-B byte stream on its start codes. The
// library's parser is meant for one access unit and refuses more than 50
// NAL units, fewer than a two-second GOP holds.
func splitAnnexB(data []byte) ([][]byte, error) {
	start := func(i int) int { // length of a start code at i, or 0
		switch {
		case i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1:
			return 3
		case i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1:
			return 4
		}
		return 0
	}
	n := start(0)
	if n == 0 {
		return nil, errors.New("the stream does not start with an Annex-B start code")
	}
	var out [][]byte
	begin := n
	for i := n; i < len(data); i++ {
		if data[i] != 0 {
			continue
		}
		if sc := start(i); sc > 0 {
			nalu := bytes.TrimRight(data[begin:i], "\x00") // trailing zero bytes
			if len(nalu) > 0 {
				out = append(out, nalu)
			}
			begin = i + sc
			i += sc - 1
		}
	}
	if last := bytes.TrimRight(data[begin:], "\x00"); len(last) > 0 {
		out = append(out, last)
	}
	if len(out) > maxNALUs {
		return nil, fmt.Errorf("the stream has %d NAL units; at most %d fit", len(out), maxNALUs)
	}
	return out, nil
}

// parseMJPEG splits a series of JPEG images. Each one must fit what RTP
// carries (RFC 2435): baseline, 8-bit, YUV 4:2:0 or 4:2:2, at most
// 2040x2040 in multiples of 8, and no markers the payload cannot describe.
func parseMJPEG(data []byte) (*engine.VideoSource, error) {
	src := &engine.VideoSource{}
	for len(data) > 0 {
		frame, rest, err := nextJPEG(data)
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", len(src.AccessUnits)+1, err)
		}
		src.AccessUnits = append(src.AccessUnits, [][]byte{frame})
		if len(src.AccessUnits) > maxFrames {
			return nil, fmt.Errorf("the stream has more than %d frames", maxFrames)
		}
		data = rest
	}
	if len(src.AccessUnits) == 0 {
		return nil, errors.New("no frames found")
	}
	return src, nil
}

// nextJPEG returns the first JPEG image of data and what follows it.
func nextJPEG(data []byte) (frame, rest []byte, err error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != jpeg.MarkerStartOfImage {
		return nil, nil, errors.New("not a JPEG image")
	}
	truncated := errors.New("the JPEG image is truncated")
	var sof *jpeg.StartOfFrame1
	for i := 2; ; {
		if i+4 > len(data) || data[i] != 0xFF {
			return nil, nil, truncated
		}
		marker := data[i+1]
		size := int(data[i+2])<<8 | int(data[i+3])
		if size < 2 || i+2+size > len(data) {
			return nil, nil, truncated
		}
		seg := data[i+4 : i+2+size]
		switch marker {
		case jpeg.MarkerStartOfFrame1:
			sof = &jpeg.StartOfFrame1{}
			if err := sof.Unmarshal(seg); err != nil {
				return nil, nil, err
			}
			if !MJPEGFits(sof.Width, sof.Height) {
				return nil, nil, fmt.Errorf("%dx%d does not fit RTP: at most %dx%d in multiples of 8", sof.Width, sof.Height, MaxMJPEGSize, MaxMJPEGSize)
			}
		case jpeg.MarkerDefineQuantizationTable:
			var dqt jpeg.DefineQuantizationTable
			if err := dqt.Unmarshal(seg); err != nil {
				return nil, nil, err
			}
		case jpeg.MarkerDefineRestartInterval:
			var dri jpeg.DefineRestartInterval
			if err := dri.Unmarshal(seg); err != nil {
				return nil, nil, err
			}
		case jpeg.MarkerStartOfScan:
			if sof == nil {
				return nil, nil, errors.New("the scan comes before the frame header")
			}
			if err := (jpeg.StartOfScan{}).Unmarshal(seg); err != nil {
				return nil, nil, err
			}
			// Entropy-coded data up to the end of the image: 0xFF is
			// followed by a stuffed zero or a restart marker.
			for j := i + 2 + size; j+1 < len(data); j++ {
				if data[j] != 0xFF {
					continue
				}
				switch m := data[j+1]; {
				case m == jpeg.MarkerEndOfImage:
					return data[:j+2], data[j+2:], nil
				case m == 0x00 || (m >= 0xD0 && m <= 0xD7):
					j++
				default:
					return nil, nil, fmt.Errorf("unexpected JPEG marker %#x in the scan", m)
				}
			}
			return nil, nil, truncated
		case 0xE0, 0xE1, 0xE2, jpeg.MarkerDefineHuffmanTable, jpeg.MarkerComment:
			// Skipped by the RTP payload; receivers use the standard
			// Huffman tables.
		default:
			return nil, nil, fmt.Errorf("JPEG marker %#x cannot travel over RTP", marker)
		}
		i += 2 + size
	}
}
