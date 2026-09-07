package adminui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed all:ui
var embeddedUI embed.FS

// NewEmbedded serves the standalone OSS SparkRoute console.
func NewEmbedded(mount string) (http.Handler, error) {
	assets, err := fs.Sub(embeddedUI, "ui")
	if err != nil {
		return nil, fmt.Errorf("sub embedded admin UI assets: %w", err)
	}
	return New(assets, mount)
}
