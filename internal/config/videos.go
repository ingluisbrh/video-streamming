package config

import (
	"os"
	"strings"
)

// DefaultVideosDir is used when VIDEOS_DIR is unset/empty.
const DefaultVideosDir = "videos"

// VideosDir returns the video directory: env VIDEOS_DIR or "videos".
// The server flag --videos overrides this when explicitly set.
func VideosDir() string {
	if d := strings.TrimSpace(os.Getenv("VIDEOS_DIR")); d != "" {
		return d
	}
	return DefaultVideosDir
}

// DefaultImagesDir is used when IMAGES_DIR is unset/empty.
const DefaultImagesDir = "images"

// ImagesDir returns the image directory: env IMAGES_DIR or "images".
// The server flag --images overrides this when explicitly set.
func ImagesDir() string {
	if d := strings.TrimSpace(os.Getenv("IMAGES_DIR")); d != "" {
		return d
	}
	return DefaultImagesDir
}
