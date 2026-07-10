// Package embedded bundles the go-code-agent documentation into the
// compiled binary so the REPL command `/readme` can render README_zh.md
// without depending on the file being present on disk at runtime.
//
// NOTE: //go:embed cannot use ".." and rejects symlinks, so the asset is
// kept as a real copy inside this directory. Refresh it after editing the
// root README with:   go generate ./embedded
package embedded

import _ "embed"

//go:generate cp ../README_zh.md README_zh.md

// Content holds the raw bytes of README_zh.md baked into the binary.
//
//go:embed README_zh.md
var Content []byte

// Path is the logical name used for display in the rendered page.
const Path = "README_zh.md"
