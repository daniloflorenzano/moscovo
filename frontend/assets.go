package frontend

import "embed"

// Assets bundles the minimalist Wails v3 desktop frontend files.
//
//go:embed index.html src/*
var Assets embed.FS
