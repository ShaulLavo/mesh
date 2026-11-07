package release

import _ "embed"

//go:embed brotli-LICENSE
var brotliNotice string

// BrotliNotice returns the exact bundled decoder's copyright and MIT notice.
func BrotliNotice() string { return brotliNotice }
