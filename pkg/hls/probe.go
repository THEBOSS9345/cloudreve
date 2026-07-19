package hls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const probeTimeout = 20 * time.Second

type probeStream struct {
	CodecType   string `json:"codec_type"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

type probeOutput struct {
	Streams []probeStream `json:"streams"`
}

// hasVideoStream reports whether ffprobe detects a genuine video stream in
// input, so the manager can choose between the video and audio-only quality
// ladders regardless of what extension the source file happens to have.
// Streams flagged as an attached picture (e.g. embedded cover art in an mp3
// or flac file) are not counted as video.
func hasVideoStream(ffprobeBin, input string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-select_streams", "v",
		"-show_entries", "stream=codec_type:stream_disposition=attached_pic",
		"-of", "json",
		input,
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("ffprobe failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var probe probeOutput
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		return false, fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	for _, s := range probe.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			return true, nil
		}
	}
	return false, nil
}
