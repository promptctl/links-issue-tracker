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
// notice, fetched from the pinned upstream tag; compiler-rt's file concatenates
// two such notices (Zig's LICENSE and LLVM compiler-rt's LICENSE.TXT) because
// the component genuinely carries material under both — see its entry below.
// [LAW:one-source-of-truth] the embedded bytes are the one copy that ships in
// THIRD_PARTY_LICENSES.
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
// SPDX expression we distribute it under, the verbatim notice text, and a note
// carrying whatever a reader of the shipped artifacts needs to accept the
// license claim without consulting our source — a dual-license election, or the
// provenance behind a compound expression. The note travels into
// LICENSE-REPORT.md and the SBOM (Entry.Note), never only into a Go comment.
type nativeLib struct {
	name    string
	version string
	license string
	text    string
	note    string
	// acknowledgement is acknowledgementConcluded for a lib whose license row
	// lit arrived at rather than read off the notice — see Entry.Acknowledgement.
	acknowledgement string
}

// nativeLibs is the curated native-library inventory. Versions are pinned to the
// release toolchain: ICU to build/Dockerfile.release's ICU_MAJOR.ICU_MINOR;
// zstd to the version gozstd vendors (its zstd.h ZSTD_VERSION_* — 1.5.6);
// musl and compiler-rt to what zig 0.14.0 (Dockerfile ZIG_VERSION) bundles for
// the fully-static linux/musl targets. ICU 75.1 carries the Unicode License v3
// (its LICENSE at release-75-1 is byte-identical to the embedded copy), and
// zig 0.14.0's bundled musl source identifies itself as 1.2.5
// (src/internal/version.h) with a COPYRIGHT byte-identical to ours.
//
// compiler-rt here is not LLVM's compiler-rt: it is Zig's own implementation of
// the builtins (lib/compiler_rt/*.zig in the zig tarball), MIT-licensed as part
// of Zig — but its files cite routines ported from llvm-project compiler-rt
// sources whose headers at the cited commits read "Apache-2.0 WITH
// LLVM-exception" (fp_add_impl.inc@02d85149, divdf3.c/divsf3.c@d674d96b,
// clear_cache.c@cf54cae2, os_version_check.c@llvmorg-13.0.0; others cite the
// pre-relicense era, when compiler_rt was dual licensed under the University of
// Illinois/NCSA license and MIT with the choice left to the user — lit elects
// MIT for those, which is why the expression carries two arms and not an NCSA
// third). The compound expression names both grants lit actually distributes
// under; flatly asserting MIT alone would misstate the Apache-licensed ported
// material, and adding NCSA would claim a grant lit declined.
//
// ICU and musl each bundle third-party material too, and each KEEPS its single
// identifier — the asymmetry with compiler-rt is deliberate, so don't "fix" it
// into a compound. Zig's LICENSE claims MIT over Zig's own code and is silent
// on the LLVM-derived routines, whose real grant (Apache-2.0 WITH
// LLVM-exception) carries patent and notice terms MIT does not: two materially
// different licenses, so the expression names both. ICU's and musl's notices
// are upstream's own grant for the WHOLE work, and the components they fold in
// are notice-only permissive ones imposing the same kind of obligation the
// stated license already does. Every license database resolves musl to MIT and
// ICU 75.1 to Unicode-3.0; emitting a compound here would make lit's SBOM
// disagree with what any scanner independently resolves for the coordinate,
// which is the contradiction this epic refused when it rejected a `replace`
// shim. What those components need is disclosure, not a different expression —
// so they carry notes, and the notes ship in all three artifacts.
var nativeLibs = []nativeLib{
	{name: "icu", version: "75.1", license: "Unicode-3.0", text: icuLicenseText,
		note: "ICU 75.1 is Unicode-3.0; its LICENSE at release-75-1 is byte-identical to the embedded copy. That file's Third-Party Software Licenses section carries further notices for components included within the ICU libraries — the pre-57 ICU License and the BSD-licensed cjdict.txt word-break data among them — and the embedded copy reproduces each verbatim. The two GNU GPL blocks the same file lists cover aclocal.m4 and config.guess, and the install-sh block beside them is MIT/X11: autotools build scripts, not code in any linked library."},
	{name: "zstd", version: "1.5.6", license: "BSD-3-Clause", text: zstdLicenseText,
		acknowledgement: acknowledgementConcluded,
		note:            "zstd is dual-licensed upstream (BSD-3-Clause OR GPL-2.0-only) at the user's election; lit elects and distributes under BSD-3-Clause."},
	{name: "musl", version: "1.2.5", license: "MIT", text: muslLicenseText,
		note: "musl's COPYRIGHT is upstream's own grant for the library as a whole: MIT, with portions derived from third-party works under terms upstream states are compatible with that MIT license — the TRE regular-expression code (2-clause BSD), the ARM and AArch64 memcpy/memset routines, and David Burren's DES implementation (BSD) among them. The embedded COPYRIGHT reproduces every one of those notices verbatim, so the attribution each requires travels with the binary."},
	{name: "compiler-rt", version: "0.14.0", license: "MIT AND Apache-2.0 WITH LLVM-exception", text: compilerRTLicenseText,
		acknowledgement: acknowledgementConcluded,
		note:            "Zig 0.14.0's implementation of the compiler-rt builtins, versioned by the Zig toolchain that bundles it: MIT as part of Zig, and it includes routines ported from LLVM compiler-rt sources licensed Apache-2.0 WITH LLVM-exception. Routines ported from pre-relicense LLVM sources are dual-licensed (University of Illinois/NCSA OR MIT) at the user's election; lit elects and distributes those under MIT, which the expression's MIT arm covers, so no NCSA grant is claimed. The embedded notice carries the full text of every license named here."},
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
			Module:          Module{Path: n.name, Version: n.version},
			LicenseName:     n.license,
			Text:            n.text,
			Note:            n.note,
			Acknowledgement: n.acknowledgement,
			PackageURL:      genericPURL(n.name, n.version),
		})
	}
	return entries
}

// genericPURL builds a pkg:generic Package URL for a non-Go component.
func genericPURL(name, version string) string {
	return packageurl.NewPackageURL(packageurl.TypeGeneric, "", name, version, nil, "").ToString()
}
