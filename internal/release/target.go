package release

import (
	"fmt"
	"runtime"
)

// Target is a manifest paired with the artifact selected for one platform.
// It is the resolver→installer seam: the resolver fetches and parses a
// manifest, picks the artifact matching the platform, and returns this value;
// the installer consumes it.
//
// [LAW:types-are-the-program] Both pieces are required to install — the
// manifest for binary identity + schema target, the artifact for URL + SHA256.
// Bundling them prevents the resolver from emitting a half-resolved value the
// installer would have to defend against.
type Target struct {
	Manifest Manifest
	Artifact Artifact
}

// CurrentPlatform returns the "<goos>/<goarch>" string mkmanifest writes into
// Artifact.Platform. Centralising the formula keeps producer and consumer in
// lockstep — both compose runtime.GOOS+"/"+runtime.GOARCH.
//
// [LAW:one-source-of-truth] mkmanifest's collectArtifacts produces this same
// string from the build matrix; this helper is the consumer mirror.
func CurrentPlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// SelectArtifact returns the artifact in m matching platform, or an error
// listing the platforms that are present so the operator sees what this
// release shipped.
func SelectArtifact(m Manifest, platform string) (Artifact, error) {
	for _, a := range m.Artifacts {
		if a.Platform == platform {
			return a, nil
		}
	}
	available := make([]string, len(m.Artifacts))
	for i, a := range m.Artifacts {
		available[i] = a.Platform
	}
	return Artifact{}, fmt.Errorf("release %s has no artifact for platform %s (available: %v)", m.Version, platform, available)
}
