package explorer

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/cluster/routes"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs/dbfs"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/manager"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/manager/entitysource"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/hls"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v4/pkg/util"
	"github.com/gin-gonic/gin"
)

// hlsSignContent returns the stable string signed/checked for a given entity's HLS
// stream. It intentionally does not depend on the requested sub-path, so a single
// signature is valid for the master playlist, every variant playlist, and every
// segment - relative playlist references inherit it automatically since it lives
// in the URL path, not the query string.
func hlsSignContent(entityID int) string {
	return fmt.Sprintf("hls-entity-%d", entityID)
}

type (
	FileHLSParameterCtx struct{}
	// FileHLSService resolves whether a file is eligible for on-the-fly HLS
	// playback and, if so, mints a signed master playlist URL for it.
	FileHLSService struct {
		Uri string `form:"uri" binding:"required"`
	}
	FileHLSResponse struct {
		Available bool   `json:"available"`
		Url       string `json:"url,omitempty"`
	}
)

// Get resolves HLS availability and, if eligible, a signed master playlist URL for the file.
func (s *FileHLSService) Get(c *gin.Context) (*FileHLSResponse, error) {
	dep := dependency.FromContext(c)
	settings := dep.SettingProvider()
	if !settings.HLSEnabled(c) {
		return &FileHLSResponse{Available: false}, nil
	}

	user := inventory.UserFromContext(c)
	m := manager.NewFileManager(dep, user)
	defer m.Recycle()

	uri, err := fs.NewUriFromString(s.Uri)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeParamErr, "unknown uri", err)
	}

	file, err := m.Get(c, uri, dbfs.WithFileEntities())
	if err != nil {
		return nil, fmt.Errorf("failed to get file: %w", err)
	}

	isVideoExt := util.IsInExtensionList(settings.HLSExts(c), file.DisplayName())
	isAudioExt := util.IsInExtensionList(settings.HLSAudioExts(c), file.DisplayName())
	if file.Type() != types.FileTypeFile || (!isVideoExt && !isAudioExt) {
		return &FileHLSResponse{Available: false}, nil
	}

	entity := file.PrimaryEntity()
	if entity == nil || entity.ID() == 0 {
		return &FileHLSResponse{Available: false}, nil
	}

	// A file matching both lists (unusual, but possible with custom extension
	// settings) is treated as a video for sizing purposes.
	minSize := settings.HLSMinSize(c)
	if isAudioExt && !isVideoExt {
		minSize = settings.HLSAudioMinSize(c)
	}
	if entity.Size() < minSize {
		return &FileHLSResponse{Available: false}, nil
	}

	// Long-lived on purpose: the signature only proves the requester was allowed
	// to view this file at mint time, it does not grant any capability beyond
	// reading its (transcoded) content, mirroring the existing content route.
	expire := time.Now().Add(24 * time.Hour)
	sign := dep.GeneralAuth().Sign(hlsSignContent(entity.ID()), expire.Unix())

	url := routes.MasterFileHLSUrl(settings.SiteURL(c), hashid.EncodeEntityID(dep.HashIDEncoder(), entity.ID()), sign)

	return &FileHLSResponse{Available: true, Url: url.String()}, nil
}

type (
	HLSStreamParameterCtx struct{}
	// HLSStreamService serves the actual HLS master playlist / variant playlists /
	// segments for an entity, starting (or joining) a background transcode job as
	// needed. Access is authorized purely via the path-embedded signature - there
	// is no session requirement, matching the existing signed content route.
	HLSStreamService struct {
		ID   string `uri:"id" binding:"required"`
		Sign string `uri:"sign" binding:"required"`
		Path string `uri:"path"`
	}
)

// Serve verifies the signature, ensures a transcode job exists and is ready, and
// streams the requested playlist/segment file.
func (s *HLSStreamService) Serve(c *gin.Context) error {
	dep := dependency.FromContext(c)
	settings := dep.SettingProvider()
	if !settings.HLSEnabled(c) {
		return serializer.NewError(serializer.CodeNotFound, "hls is not enabled", nil)
	}

	entityID, err := dep.HashIDEncoder().Decode(s.ID, hashid.EntityID)
	if err != nil {
		return serializer.NewError(serializer.CodeParamErr, "unknown entity id", err)
	}

	if err := dep.GeneralAuth().Check(hlsSignContent(entityID), s.Sign); err != nil {
		return serializer.NewError(serializer.CodeCredentialInvalid, "invalid or expired signature", err)
	}

	reqPath := strings.TrimPrefix(s.Path, "/")
	if reqPath == "" {
		reqPath = "master.m3u8"
	}

	user := inventory.UserFromContext(c)
	m := manager.NewFileManager(dep, user)
	defer m.Recycle()

	hlsManager := dep.HLSManager(c)

	// Only pay for resolving the transcode input source (DB + driver lookups, and
	// possibly a signed URL round-trip for remote policies) when a new job is
	// actually likely to start - the overwhelming majority of segment/playlist
	// requests hit an already-running or already-cached job. Resolution happens
	// synchronously here, while our FileManager is still valid (it goes back to
	// the pool as soon as Serve returns); LocalPath/Url are cheap accessor calls,
	// not open file handles, so it's safe for the background transcode goroutine
	// to keep using the resolved string long after this request completes.
	input := ""
	if !hlsManager.Exists(entityID) {
		es, err := m.GetEntitySource(c, entityID)
		if err != nil {
			return fmt.Errorf("failed to get entity source: %w", err)
		}

		if es.IsLocal() && !es.Entity().Encrypted() {
			input = es.LocalPath(c)
		} else {
			expire := time.Now().Add(3 * time.Hour)
			opts := []entitysource.EntitySourceOption{
				entitysource.WithContext(c),
				entitysource.WithExpire(&expire),
			}
			if !es.Entity().Encrypted() {
				opts = append(opts, entitysource.WithNoInternalProxy())
			}
			srcUrl, err := es.Url(c, opts...)
			if err != nil {
				return fmt.Errorf("failed to resolve hls input source: %w", err)
			}
			input = srcUrl.Url
		}
	}

	job, err := hlsManager.GetOrStartJob(c, entityID, func(context.Context) (string, func(), error) {
		return input, nil, nil
	})
	if err != nil {
		return fmt.Errorf("failed to start hls transcode: %w", err)
	}

	if err := job.WaitReady(c, hls.DefaultReadyTimeout()); err != nil {
		return fmt.Errorf("hls transcode not ready: %w", err)
	}

	filePath, err := job.ResolvePath(reqPath)
	if err != nil {
		return serializer.NewError(serializer.CodeParamErr, "invalid hls resource path", err)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return serializer.NewError(serializer.CodeNotFound, "hls resource not found", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat hls resource: %w", err)
	}

	c.Header("Cache-Control", "no-cache")
	c.Header("Content-Type", hls.ContentTypeForPath(filePath))
	http.ServeContent(c.Writer, c.Request, filePath, info.ModTime(), f)
	return nil
}
