// Package hls generates on-the-fly adaptive-bitrate HLS renditions of video
// files using ffmpeg, caches them on local disk, and serves them back with a
// short "first play" wait while the first segments are produced.
package hls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudreve/Cloudreve/v4/pkg/logging"
	"github.com/cloudreve/Cloudreve/v4/pkg/setting"
	"github.com/cloudreve/Cloudreve/v4/pkg/util"
)

var (
	// ErrTranscodeTimeout is returned when a caller gives up waiting for a job to become ready.
	ErrTranscodeTimeout = errors.New("timed out waiting for hls transcode to become ready")
	// ErrInvalidPath is returned when a requested cache sub-path does not match any known file.
	ErrInvalidPath = errors.New("invalid hls cache path")
)

const (
	completeMarker      = ".complete"
	masterPlaylistName  = "master.m3u8"
	cleanupMinInterval  = time.Hour
	defaultReadyTimeout = 25 * time.Second
)

var segFileRe = regexp.MustCompile(`^seg_\d{5}\.ts$`)

var closedChan = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// JobStatus describes the lifecycle stage of a transcode job.
type JobStatus int32

const (
	JobPending JobStatus = iota
	JobRunning
	JobReady
	JobDone
	JobFailed
)

// Job tracks a single file's HLS transcode, in-memory bookkeeping plus its on-disk cache directory.
type Job struct {
	entityID   int
	dir        string
	renditions []Rendition

	mu     sync.Mutex
	status JobStatus
	err    error

	readyCh   chan struct{}
	doneCh    chan struct{}
	readyOnce sync.Once
	doneOnce  sync.Once
}

func (j *Job) markReady() {
	j.readyOnce.Do(func() {
		j.mu.Lock()
		if j.status == JobPending || j.status == JobRunning {
			j.status = JobReady
		}
		j.mu.Unlock()
		close(j.readyCh)
	})
}

func (j *Job) markDone(err error) {
	j.mu.Lock()
	if err != nil {
		j.status = JobFailed
		j.err = err
	} else {
		j.status = JobDone
	}
	j.mu.Unlock()

	// A job that finished (even with an error) is by definition no longer worth waiting on.
	j.markReady()
	j.doneOnce.Do(func() {
		close(j.doneCh)
	})
}

// WaitReady blocks until the job has produced at least the first segment of every
// rendition, or failed, or the timeout/context elapses.
func (j *Job) WaitReady(ctx context.Context, timeout time.Duration) error {
	select {
	case <-j.readyCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return ErrTranscodeTimeout
	}

	j.mu.Lock()
	status, err := j.status, j.err
	j.mu.Unlock()
	if status == JobFailed {
		return fmt.Errorf("hls transcode failed: %w", err)
	}
	return nil
}

// Dir returns the on-disk cache directory for this job.
func (j *Job) Dir() string { return j.dir }

// ResolvePath validates a requested sub-path (as taken from the URL) against the
// known set of files a job can produce, and returns the corresponding absolute
// path on disk. It never allows path traversal outside the job's own directory.
func (j *Job) ResolvePath(reqPath string) (string, error) {
	reqPath = strings.TrimPrefix(reqPath, "/")
	if reqPath == masterPlaylistName {
		return filepath.Join(j.dir, masterPlaylistName), nil
	}

	parts := strings.Split(reqPath, "/")
	if len(parts) != 2 {
		return "", ErrInvalidPath
	}

	renditionName, file := parts[0], parts[1]
	valid := false
	for _, r := range j.renditions {
		if r.Name == renditionName {
			valid = true
			break
		}
	}
	if !valid {
		return "", ErrInvalidPath
	}

	if file != "playlist.m3u8" && !segFileRe.MatchString(file) {
		return "", ErrInvalidPath
	}

	return filepath.Join(j.dir, renditionName, file), nil
}

// Manager orchestrates HLS transcode jobs: dedup, concurrency limiting, and disk cache eviction.
type Manager struct {
	l         logging.Logger
	settings  setting.Provider
	cacheRoot string

	mu   sync.Mutex
	jobs map[int]*Job

	capMu   sync.Mutex
	capCond *sync.Cond
	running int

	lastCleanup int64
}

// NewManager creates a new HLS job manager.
func NewManager(l logging.Logger, settings setting.Provider) *Manager {
	m := &Manager{
		l:         l,
		settings:  settings,
		cacheRoot: util.DataPath("hls_cache"),
		jobs:      make(map[int]*Job),
	}
	m.capCond = sync.NewCond(&m.capMu)
	_ = util.CreatNestedFolder(m.cacheRoot)
	return m
}

// Exists reports whether a job for entityID is already tracked in memory or fully
// cached on disk, without creating anything. Callers can use this to avoid doing
// expensive source-resolution work before calling GetOrStartJob when it's very
// likely a cached result will be served instead.
func (m *Manager) Exists(entityID int) bool {
	m.mu.Lock()
	_, ok := m.jobs[entityID]
	m.mu.Unlock()
	if ok {
		return true
	}

	dir := filepath.Join(m.cacheRoot, strconv.Itoa(entityID))
	return util.Exists(filepath.Join(dir, completeMarker))
}

// GetOrStartJob returns the existing (or already-completed on disk) job for entityID,
// or starts a new background transcode using inputFn to resolve the source once the
// job actually begins running. inputFn's returned cleanup func (if any) is invoked
// once the job finishes, successfully or not.
func (m *Manager) GetOrStartJob(ctx context.Context, entityID int, inputFn func(context.Context) (string, func(), error)) (*Job, error) {
	m.maybeCleanup(ctx)

	m.mu.Lock()
	if job, ok := m.jobs[entityID]; ok {
		m.mu.Unlock()
		return job, nil
	}
	m.mu.Unlock()

	dir := filepath.Join(m.cacheRoot, strconv.Itoa(entityID))

	if util.Exists(filepath.Join(dir, completeMarker)) {
		if renditions, err := discoverRenditions(dir); err == nil {
			job := &Job{
				entityID:   entityID,
				dir:        dir,
				renditions: renditions,
				status:     JobDone,
				readyCh:    closedChan,
				doneCh:     closedChan,
			}
			m.mu.Lock()
			m.jobs[entityID] = job
			m.mu.Unlock()
			_ = os.Chtimes(dir, time.Now(), time.Now())
			return job, nil
		}
	}

	// Anything left on disk at this point is a stale/partial cache from a
	// previous crash or config change - start clean.
	_ = os.RemoveAll(dir)

	segDur := m.settings.HLSSegmentDuration(ctx)
	if segDur <= 0 {
		segDur = 6
	}

	ffmpegBin := m.settings.FFMpegPath(ctx)
	ffprobeBin := m.settings.MediaMetaFFProbePath(ctx)
	var extraArgs []string
	if raw := strings.TrimSpace(m.settings.HLSExtraArgs(ctx)); raw != "" {
		extraArgs = strings.Fields(raw)
	}

	if err := util.CreatNestedFolder(dir); err != nil {
		return nil, fmt.Errorf("failed to create hls cache folder: %w", err)
	}

	job := &Job{
		entityID: entityID,
		dir:      dir,
		status:   JobPending,
		readyCh:  make(chan struct{}),
		doneCh:   make(chan struct{}),
	}

	m.mu.Lock()
	m.jobs[entityID] = job
	m.mu.Unlock()

	go m.runJob(job, ffmpegBin, ffprobeBin, extraArgs, segDur, inputFn)

	return job, nil
}

// resolveRenditions probes input to decide whether it has a genuine video
// stream and parses the matching (video or audio-only) quality ladder
// setting. It is separate from ParseLadder/ParseAudioLadder so a single file
// extension list (HLSExts/HLSAudioExts) doesn't have to be trusted for
// picking the transcode strategy - e.g. an audio file with embedded cover art
// must still be treated as audio-only.
func (m *Manager) resolveRenditions(ctx context.Context, ffprobeBin, input string) ([]Rendition, error) {
	hasVideo, err := hasVideoStream(ffprobeBin, input)
	if err != nil {
		return nil, fmt.Errorf("failed to probe hls input source: %w", err)
	}

	if hasVideo {
		renditions, err := ParseLadder(m.settings.HLSResolutions(ctx))
		if err != nil {
			return nil, fmt.Errorf("failed to parse hls resolution ladder: %w", err)
		}
		return renditions, nil
	}

	renditions, err := ParseAudioLadder(m.settings.HLSAudioBitrates(ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to parse hls audio bitrate ladder: %w", err)
	}
	return renditions, nil
}

func (m *Manager) runJob(job *Job, ffmpegBin, ffprobeBin string, extraArgs []string, segDur int, inputFn func(context.Context) (string, func(), error)) {
	m.acquireSlot()
	defer m.releaseSlot()

	job.mu.Lock()
	job.status = JobRunning
	job.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()

	input, cleanup, err := inputFn(ctx)
	if err != nil {
		m.failJob(job, fmt.Errorf("failed to resolve hls input source: %w", err))
		return
	}
	if cleanup != nil {
		defer cleanup()
	}

	renditions, err := m.resolveRenditions(ctx, ffprobeBin, input)
	if err != nil {
		m.failJob(job, err)
		return
	}
	job.renditions = renditions

	if err := os.WriteFile(filepath.Join(job.dir, masterPlaylistName), buildMasterPlaylist(renditions), 0600); err != nil {
		m.failJob(job, fmt.Errorf("failed to write hls master playlist: %w", err))
		return
	}

	go m.pollReady(job)

	var wg sync.WaitGroup
	errCh := make(chan error, len(renditions))
	for _, r := range renditions {
		wg.Add(1)
		go func(r Rendition) {
			defer wg.Done()

			renditionDir := filepath.Join(job.dir, r.Name)
			if err := util.CreatNestedFolder(renditionDir); err != nil {
				errCh <- fmt.Errorf("failed to create rendition folder %q: %w", r.Name, err)
				return
			}

			args := buildRenditionArgs(input, r, renditionDir, segDur, extraArgs)
			cmd := exec.CommandContext(ctx, ffmpegBin, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				errCh <- fmt.Errorf("ffmpeg failed for rendition %s: %w: %s", r.Name, err, lastLines(stderr.String(), 20))
				return
			}
		}(r)
	}

	wg.Wait()
	close(errCh)

	var firstErr error
	for e := range errCh {
		if firstErr == nil {
			firstErr = e
		}
	}

	if firstErr != nil {
		m.l.Warning("HLS transcode failed for entity %d: %s", job.entityID, firstErr)
		m.failJob(job, firstErr)
		return
	}

	if err := os.WriteFile(filepath.Join(job.dir, completeMarker), []byte(time.Now().Format(time.RFC3339)), 0600); err != nil {
		m.l.Warning("Failed to write hls completion marker for entity %d: %s", job.entityID, err)
	}

	job.markDone(nil)
}

func (m *Manager) failJob(job *Job, err error) {
	_ = os.RemoveAll(job.dir)
	job.markDone(err)
	m.mu.Lock()
	delete(m.jobs, job.entityID)
	m.mu.Unlock()
}

// pollReady watches a running job's cache directory and flips it to "ready" as soon
// as the first segment of every rendition exists on disk, so playback can start
// before the whole transcode finishes.
func (m *Manager) pollReady(job *Job) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-job.doneCh:
			return
		case <-ticker.C:
			allReady := true
			for _, r := range job.renditions {
				if !util.Exists(filepath.Join(job.dir, r.Name, "seg_00000.ts")) {
					allReady = false
					break
				}
			}
			if allReady {
				job.markReady()
				return
			}
		}
	}
}

func (m *Manager) acquireSlot() {
	m.capMu.Lock()
	defer m.capMu.Unlock()
	for m.running >= maxInt(1, m.settings.HLSMaxConcurrentJobs(context.Background())) {
		m.capCond.Wait()
	}
	m.running++
}

func (m *Manager) releaseSlot() {
	m.capMu.Lock()
	m.running--
	m.capCond.Broadcast()
	m.capMu.Unlock()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// maybeCleanup opportunistically evicts idle HLS caches beyond their configured
// TTL. It runs at most once per hour, kicked off from request handling rather
// than a dedicated cron job.
func (m *Manager) maybeCleanup(ctx context.Context) {
	now := time.Now().Unix()
	last := atomic.LoadInt64(&m.lastCleanup)
	if now-last < int64(cleanupMinInterval.Seconds()) {
		return
	}
	if !atomic.CompareAndSwapInt64(&m.lastCleanup, last, now) {
		return
	}
	go m.cleanup(ctx)
}

func (m *Manager) cleanup(ctx context.Context) {
	ttlHours := m.settings.HLSCacheTTL(ctx)
	if ttlHours <= 0 {
		return
	}

	entries, err := os.ReadDir(m.cacheRoot)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-time.Duration(ttlHours) * time.Hour)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		m.mu.Lock()
		_, active := m.jobs[id]
		m.mu.Unlock()
		if active {
			continue
		}

		full := filepath.Join(m.cacheRoot, e.Name())
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(full)
		}
	}
}

func discoverRenditions(dir string) ([]Rendition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var renditions []Rendition
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if util.Exists(filepath.Join(dir, e.Name(), "playlist.m3u8")) {
			renditions = append(renditions, Rendition{Name: e.Name()})
		}
	}
	if len(renditions) == 0 {
		return nil, fmt.Errorf("no renditions found in %s", dir)
	}
	return renditions, nil
}

func buildMasterPlaylist(renditions []Rendition) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for _, r := range renditions {
		if r.IsAudioOnly() {
			fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,NAME=\"%s\"\n", r.bandwidthEstimate(), r.Name)
		} else {
			width := (r.Height * 16 / 9 / 2) * 2
			fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,NAME=\"%s\"\n", r.bandwidthEstimate(), width, r.Height, r.Name)
		}
		fmt.Fprintf(&b, "%s/playlist.m3u8\n", r.Name)
	}
	return []byte(b.String())
}

func buildRenditionArgs(input string, r Rendition, renditionDir string, segDur int, extraArgs []string) []string {
	args := []string{"-y"}
	args = append(args, extraArgs...)
	args = append(args, "-i", input)

	if r.IsAudioOnly() {
		args = append(args,
			"-map", "0:a:0",
			"-vn",
			"-c:a", "aac",
			"-ar", "48000",
			"-ac", "2",
			"-b:a", r.AudioBitrate,
			"-f", "hls",
			"-hls_time", strconv.Itoa(segDur),
			"-hls_list_size", "0",
			"-hls_playlist_type", "event",
			"-hls_flags", "independent_segments+temp_file",
			"-hls_segment_type", "mpegts",
			"-hls_segment_filename", filepath.Join(renditionDir, "seg_%05d.ts"),
			filepath.Join(renditionDir, "playlist.m3u8"),
		)
		return args
	}

	args = append(args,
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-vf", fmt.Sprintf("scale=-2:min(ih\\,%d)", r.Height),
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-profile:v", "main",
		"-pix_fmt", "yuv420p",
		"-sc_threshold", "0",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segDur),
		"-b:v", r.VideoBitrate,
		"-maxrate", scaleBitrateStr(r.VideoBitrate, 1.07),
		"-bufsize", scaleBitrateStr(r.VideoBitrate, 1.5),
		"-c:a", "aac",
		"-ar", "48000",
		"-ac", "2",
		"-b:a", r.AudioBitrate,
		"-f", "hls",
		"-hls_time", strconv.Itoa(segDur),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(renditionDir, "seg_%05d.ts"),
		filepath.Join(renditionDir, "playlist.m3u8"),
	)
	return args
}

// ContentTypeForPath returns the MIME type to send for a resolved cache file path.
func ContentTypeForPath(p string) string {
	switch {
	case strings.HasSuffix(p, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.HasSuffix(p, ".ts"):
		return "video/mp2t"
	default:
		return "application/octet-stream"
	}
}

// DefaultReadyTimeout is how long a stream request should wait for a fresh job to
// become ready before giving up.
func DefaultReadyTimeout() time.Duration { return defaultReadyTimeout }

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
