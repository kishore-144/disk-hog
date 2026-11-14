// Package web holds the dashboard, compiled into the binary.
package web

import "embed"

// Assets is the dashboard's static files.
//
//go:embed index.html app.js style.css icon.svg
var Assets embed.FS
