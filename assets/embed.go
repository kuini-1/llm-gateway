// Package assets embeds the admin dashboard and the OpenAPI spec.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed dashboard openapi.yaml
var files embed.FS

func Dashboard() fs.FS {
	sub, err := fs.Sub(files, "dashboard")
	if err != nil {
		panic(err)
	}
	return sub
}

func OpenAPI() []byte {
	b, err := files.ReadFile("openapi.yaml")
	if err != nil {
		panic(err)
	}
	return b
}
