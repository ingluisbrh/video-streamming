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
