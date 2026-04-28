// Package main is the entry point for the scafctl-plugin-github plugin.
package main

import (
	"github.com/oakwood-commons/scafctl-plugin-github/internal/github"

	sdkplugin "github.com/oakwood-commons/scafctl-plugin-sdk/plugin"
)

func main() {
	sdkplugin.Serve(&github.Plugin{})
}
