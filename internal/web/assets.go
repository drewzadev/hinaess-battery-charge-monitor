package web

import "embed"

// assetsFS holds the vendored single-page frontend (index.html, app.js) and the
// vendored uPlot library (uplot.min.js, uplot.min.css), embedded into the binary
// so the server is self-contained and works on an isolated LAN with no internet
// (FR-3, FR-10). The embed pattern must be a sibling path of this file — Go
// forbids `../` escapes — so the assets live under internal/web/assets/ rather
// than a top-level web/ directory (see PRD Current State, the go:embed constraint).
//
//go:embed assets
var assetsFS embed.FS
