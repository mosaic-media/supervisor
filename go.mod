// The module path is `github.com/mosaic-media/supervisor`, not
// `.../platform/supervisor`, even though the directory currently sits inside
// the platform repository. The Supervisor has no repository yet and creating
// one is a decision somebody has to take deliberately; naming the module for
// where it belongs rather than where it is parked means the extraction is
// `git mv` plus a `git init`, with no import in any file to rewrite.
//
// Nothing here imports the Platform, and the boundary test enforces it.
module github.com/mosaic-media/supervisor

go 1.25.0
