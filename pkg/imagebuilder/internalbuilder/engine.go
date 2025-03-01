package internalbuilder

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	log "github.com/sirupsen/logrus"

	"github.com/docker-slim/docker-slim/pkg/imagebuilder"
	"github.com/docker-slim/docker-slim/pkg/util/fsutil"
)

const (
	Name = "internal.container.build.engine"
)

// Engine is the default simple build engine
type Engine struct {
	ShowBuildLogs  bool
	PushToDaemon   bool
	PushToRegistry bool
}

// New creates new Engine instances
func New(
	showBuildLogs bool,
	pushToDaemon bool,
	pushToRegistry bool) (*Engine, error) {

	engine := &Engine{
		ShowBuildLogs:  showBuildLogs,
		PushToDaemon:   pushToDaemon,
		PushToRegistry: pushToRegistry,
	}

	return engine, nil
}

func (ref *Engine) Build(options imagebuilder.SimpleBuildOptions) error {
	if len(options.Entrypoint) == 0 && len(options.Cmd) == 0 {
		return fmt.Errorf("missing startup info")
	}

	if len(options.Layers) == 0 {
		return fmt.Errorf("no layers")
	}

	if len(options.Layers) > 255 {
		return fmt.Errorf("too many layers")
	}

	switch options.Architecture {
	case "":
		options.Architecture = "amd64"
	case "arm64", "amd64":
	default:
		return fmt.Errorf("bad architecture value")
	}

	var img v1.Image
	if options.From == "" {
		//same as FROM scratch
		img = empty.Image
	} else {
		return fmt.Errorf("custom base images are not supported yet")
	}

	imgCfg := v1.Config{
		Entrypoint:   options.Entrypoint,
		Cmd:          options.Cmd,
		WorkingDir:   options.WorkDir,
		StopSignal:   options.StopSignal,
		OnBuild:      options.OnBuild,
		Labels:       options.Labels,
		Env:          options.EnvVars,
		User:         options.User,
		Volumes:      options.Volumes,
		ExposedPorts: options.ExposedPorts,
	}

	imgCfgFile := &v1.ConfigFile{
		Created:      v1.Time{Time: time.Now()},
		Author:       "docker-slim",
		Config:       imgCfg,
		Architecture: options.Architecture,
		OS:           "linux",
	}

	log.Debug("DefaultSimpleBuilder.Build: config image")

	img, err := mutate.ConfigFile(img, imgCfgFile)
	if err != nil {
		return err
	}

	var layersToAdd []v1.Layer

	for i, layerInfo := range options.Layers {
		log.Debugf("DefaultSimpleBuilder.Build: [%d] create image layer (type=%v source=%s)",
			i, layerInfo.Type, layerInfo.Source)

		if layerInfo.Source == "" {
			return fmt.Errorf("empty image layer data source")
		}

		if !fsutil.Exists(layerInfo.Source) {
			return fmt.Errorf("image layer data source path doesnt exist - %s", layerInfo.Source)
		}

		switch layerInfo.Type {
		case imagebuilder.TarSource:
			if !fsutil.IsRegularFile(layerInfo.Source) {
				return fmt.Errorf("image layer data source path is not a file - %s", layerInfo.Source)
			}

			if !fsutil.IsTarFile(layerInfo.Source) {
				return fmt.Errorf("image layer data source path is not a tar file - %s", layerInfo.Source)
			}

			layer, err := layerFromTar(layerInfo)
			if err != nil {
				return err
			}

			layersToAdd = append(layersToAdd, layer)
		case imagebuilder.DirSource:
			if !fsutil.IsDir(layerInfo.Source) {
				return fmt.Errorf("image layer data source path is not a directory - %s", layerInfo.Source)
			}

			layer, err := layerFromDir(layerInfo)
			if err != nil {
				return err
			}

			layersToAdd = append(layersToAdd, layer)
		default:
			return fmt.Errorf("unknown image data source - %v", layerInfo.Source)
		}
	}

	log.Debug("DefaultSimpleBuilder.Build: adding layers to image")
	newImg, err := mutate.AppendLayers(img, layersToAdd...)
	if err != nil {
		return err
	}

	if len(options.Tags) == 0 {
		return fmt.Errorf("missing tags")
	}

	tag, err := name.NewTag(options.Tags[0])
	if err != nil {
		return err
	}

	if ref.PushToDaemon {
		log.Debug("DefaultSimpleBuilder.Build: saving image to Docker")
		imageLoadResponseStr, err := daemon.Write(tag, newImg)
		if err != nil {
			return err
		}

		log.Debug("DefaultSimpleBuilder.Build: pushed image to daemon - %s", imageLoadResponseStr)
		if ref.ShowBuildLogs {
			//TBD (need execution context to display the build logs)
		}

		otherTags := options.Tags[1:]
		if len(otherTags) > 0 {
			log.Debug("DefaultSimpleBuilder.Build: adding other tags")

			for _, tagName := range otherTags {
				ntag, err := name.NewTag(tagName)
				if err != nil {
					log.Errorf("DefaultSimpleBuilder.Build: error creating tag: %v", err)
					continue
				}

				if err := daemon.Tag(tag, ntag); err != nil {
					log.Errorf("DefaultSimpleBuilder.Build: error tagging: %v", err)
				}
			}
		}
	}

	if ref.PushToRegistry {
		//TBD
	}

	return nil
}

func layerFromTar(input imagebuilder.LayerDataInfo) (v1.Layer, error) {
	if !fsutil.Exists(input.Source) ||
		!fsutil.IsRegularFile(input.Source) {
		return nil, fmt.Errorf("bad input data")
	}

	return tarball.LayerFromFile(input.Source)
}

func layerFromDir(input imagebuilder.LayerDataInfo) (v1.Layer, error) {
	if !fsutil.Exists(input.Source) ||
		!fsutil.IsDir(input.Source) {
		return nil, fmt.Errorf("bad input data")
	}

	var b bytes.Buffer
	tw := tar.NewWriter(&b)

	layerBasePath := "/"
	if input.Params != nil && input.Params.TargetPath != "" {
		layerBasePath = input.Params.TargetPath
	}

	err := filepath.Walk(input.Source, func(fp string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(input.Source, fp)
		if err != nil {
			return fmt.Errorf("failed to calculate relative path: %w", err)
		}

		hdr := &tar.Header{
			Name: path.Join(layerBasePath, filepath.ToSlash(rel)),
			Mode: int64(info.Mode()),
		}

		if !info.IsDir() {
			hdr.Size = info.Size()
		}

		if info.Mode().IsDir() {
			hdr.Typeflag = tar.TypeDir
		} else if info.Mode().IsRegular() {
			hdr.Typeflag = tar.TypeReg
		} else {
			return fmt.Errorf("not implemented archiving file type %s (%s)", info.Mode(), rel)
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write tar header: %w", err)
		}
		if !info.IsDir() {
			f, err := os.Open(fp)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				return fmt.Errorf("failed to read file into the tar: %w", err)
			}
			f.Close()
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to scan files: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finish tar: %w", err)
	}

	return tarball.LayerFromReader(&b)
}
