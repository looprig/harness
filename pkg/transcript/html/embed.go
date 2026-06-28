package html

import _ "embed"

// The renderer is self-contained: the page template plus the stylesheet and
// script are compiled into the binary and inlined into the output, so a rendered
// transcript opens offline with no external assets. Task 9 fleshes out the layout
// in these same files; the renderer wires them by inlining (no external src/href).

//go:embed template.gohtml
var templateSource string

//go:embed styles.css
var stylesCSS string

//go:embed app.js
var appJS string
