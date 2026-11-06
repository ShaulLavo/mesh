package serve

import (
	"io"
	"strings"

	"github.com/shaul/mesh/internal/htmlrender"
)

func renderDirectory(w io.Writer, page directoryPage) {
	var body strings.Builder
	title := htmlrender.Text(page.Title)
	body.WriteString(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>`)
	body.WriteString(title)
	body.WriteString(`</title></head>
<body><h1>`)
	body.WriteString(title)
	body.WriteString(`</h1><ul>`)
	for _, entry := range page.Entries {
		body.WriteString("\n<li><a href=\"")
		body.WriteString(htmlrender.Href(entry.Href))
		body.WriteString(`">`)
		body.WriteString(htmlrender.Text(entry.Name))
		body.WriteString(`</a></li>`)
	}
	body.WriteString("\n</ul></body>\n</html>\n")
	_, _ = io.WriteString(w, body.String())
}
