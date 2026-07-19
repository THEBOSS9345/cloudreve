package hls

import (
	"fmt"
	"strconv"
	"strings"
)

// Rendition describes a single HLS quality variant.
type Rendition struct {
	// Name is used as the sub-folder name and playlist quality label, e.g. "720p"
	// for video renditions or "192k" for audio-only renditions.
	Name string
	// Height is the target vertical resolution. The source is never upscaled.
	// Zero means this is an audio-only rendition (no video stream is encoded).
	Height int
	// VideoBitrate is passed to ffmpeg's -b:v, e.g. "2800k". Empty for audio-only renditions.
	VideoBitrate string
	// AudioBitrate is passed to ffmpeg's -b:a, e.g. "128k".
	AudioBitrate string
}

// IsAudioOnly reports whether this rendition has no video component.
func (r Rendition) IsAudioOnly() bool {
	return r.Height == 0
}

// bandwidthEstimate returns a rough combined bitrate in bits/sec, used for the
// BANDWIDTH attribute in the master playlist.
func (r Rendition) bandwidthEstimate() int {
	v := parseBitrate(r.VideoBitrate)
	a := parseBitrate(r.AudioBitrate)
	// Add ~15% overhead for container/segmenting to avoid under-reporting.
	return int(float64(v+a) * 1.15)
}

// scaleBitrateStr scales a bitrate string like "5000k" by factor and re-renders
// it in the same "Nk" form ffmpeg expects.
func scaleBitrateStr(b string, factor float64) string {
	v := parseBitrate(b)
	scaled := int(float64(v) * factor)
	return fmt.Sprintf("%dk", scaled/1000)
}

func parseBitrate(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := 1
	if strings.HasSuffix(s, "k") {
		mult = 1000
		s = strings.TrimSuffix(s, "k")
	} else if strings.HasSuffix(s, "m") {
		mult = 1000000
		s = strings.TrimSuffix(s, "m")
	}
	v, _ := strconv.Atoi(s)
	return v * mult
}

// ParseLadder parses a quality ladder spec of the form
// "height:vbitrate:abitrate,height:vbitrate:abitrate,...", e.g.
// "1080:5000k:160k,720:2800k:128k,480:1400k:128k,360:800k:96k".
// Entries are returned sorted from highest to lowest resolution.
func ParseLadder(spec string) ([]Rendition, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty resolution ladder")
	}

	parts := strings.Split(spec, ",")
	renditions := make([]Rendition, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		fields := strings.Split(p, ":")
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid resolution entry %q, expected height:vbitrate:abitrate", p)
		}

		height, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || height <= 0 {
			return nil, fmt.Errorf("invalid height in entry %q", p)
		}

		renditions = append(renditions, Rendition{
			Name:         fmt.Sprintf("%dp", height),
			Height:       height,
			VideoBitrate: strings.TrimSpace(fields[1]),
			AudioBitrate: strings.TrimSpace(fields[2]),
		})
	}

	if len(renditions) == 0 {
		return nil, fmt.Errorf("no valid resolution entries")
	}

	// Sort highest to lowest so the master playlist lists best quality first.
	for i := 0; i < len(renditions); i++ {
		for j := i + 1; j < len(renditions); j++ {
			if renditions[j].Height > renditions[i].Height {
				renditions[i], renditions[j] = renditions[j], renditions[i]
			}
		}
	}

	return renditions, nil
}

// ParseAudioLadder parses an audio-only quality ladder spec of the form
// "abitrate,abitrate,...", e.g. "320k,192k,128k,64k". Entries are returned
// sorted from highest to lowest bitrate. The resulting Renditions all have
// Height == 0 (see Rendition.IsAudioOnly).
func ParseAudioLadder(spec string) ([]Rendition, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty audio bitrate ladder")
	}

	parts := strings.Split(spec, ",")
	renditions := make([]Rendition, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		if parseBitrate(p) <= 0 {
			return nil, fmt.Errorf("invalid audio bitrate entry %q", p)
		}

		renditions = append(renditions, Rendition{
			Name:         p,
			AudioBitrate: p,
		})
	}

	if len(renditions) == 0 {
		return nil, fmt.Errorf("no valid audio bitrate entries")
	}

	// Sort highest to lowest so the master playlist lists best quality first.
	for i := 0; i < len(renditions); i++ {
		for j := i + 1; j < len(renditions); j++ {
			if parseBitrate(renditions[j].AudioBitrate) > parseBitrate(renditions[i].AudioBitrate) {
				renditions[i], renditions[j] = renditions[j], renditions[i]
			}
		}
	}

	return renditions, nil
}
