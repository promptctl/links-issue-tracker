package main

import (
	_ "embed"

	packageurl "github.com/package-url/packageurl-go"
)

// Native C libraries that cgo statically links into lit's release binaries but
// that no go.mod-based tool can see: `go list -deps` reports only Go modules, so
// ICU, zstd, musl, and compiler-rt would be silently absent from the SBOM, the
// attribution bundle, the report, and the policy gate without this curated
// inventory. Their versions are pinned by the release build config, NOT
// discovered — see each entry's provenance comment; TestNativeICUVersionMatchesDockerfile
// checks the one version that lives in-repo (ICU, in build/Dockerfile.release)
// against drift.

// License texts are embedded because, unlike Go modules, there is no module
// directory to read them from at generation time. Each is the verbatim upstream
// notice, fetched from the pinned upstream tag. [LAW:one-source-of-truth] the
// embedded bytes are the one copy that ships in THIRD_PARTY_LICENSES.
//
//go:embed native/ICU-LICENSE
var icuLicenseText string

//go:embed native/zstd-LICENSE
var zstdLicenseText string

//go:embed native/musl-COPYRIGHT
var muslLicenseText string

//go:embed native/compiler-rt-LICENSE
var compilerRTLicenseText string

// nativeLib is one statically-linked native C library: its name, version, the
// SPDX license we distribute it under, and the verbatim notice text.
type nativeLib struct {
	name    string
	version string
	license string
	text    string
}

// nativeLibs is the curated native-library inventory. Versions are pinned to the
// release toolchain: ICU to build/Dockerfile.release's ICU_MAJOR.ICU_MINOR;
// zstd to the version gozstd vendors (its zstd.h ZSTD_VERSION_* — 1.5.6);
// musl and compiler-rt to what zig 0.14.0 (Dockerfile ZIG_VERSION) bundles for
// the fully-static linux/musl targets. zstd is dual BSD-3-Clause/GPLv2 upstream;
// we elect BSD-3-Clause. ICU 75.1 carries the Unicode License v3.
var nativeLibs = []nativeLib{
	{name: "icu", version: "75.1", license: "Unicode-3.0", text: icuLicenseText},
	{name: "zstd", version: "1.5.6", license: "BSD-3-Clause", text: zstdLicenseText},
	{name: "musl", version: "1.2.5", license: "MIT", text: muslLicenseText},
	{name: "compiler-rt", version: "0.14.0", license: "MIT", text: compilerRTLicenseText},
}

// nativeEntries renders the curated native libraries as Entry values so the
// bundle, report, SBOM, and policy gate all treat them exactly like Go modules —
// same []Entry, one inventory. The purl is pkg:generic (not pkg:golang): these
// are not Go modules, and a pkg:golang purl would misdirect a scanner to the Go
// module proxy. [LAW:one-type-per-behavior] a native lib is a component like any
// other; only its purl namespace and the absence of a classifier step differ.
func nativeEntries() []Entry {
	entries := make([]Entry, 0, len(nativeLibs))
	for _, n := range nativeLibs {
		entries = append(entries, Entry{
			Module:      Module{Path: n.name, Version: n.version},
			LicenseName: n.license,
			Text:        n.text,
			PackageURL:  genericPURL(n.name, n.version),
		})
	}
	return entries
}

// genericPURL builds a pkg:generic Package URL for a non-Go component.
func genericPURL(name, version string) string {
	return packageurl.NewPackageURL(packageurl.TypeGeneric, "", name, version, nil, "").ToString()
}
