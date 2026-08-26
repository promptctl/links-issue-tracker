package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	lc "github.com/google/licenseclassifier"
)

// canonicalMIT is the standard OSI-published MIT License template text.
// Embedded literally so classify_test.go doesn't depend on GOMODCACHE
// contents (which module happens to be MIT-licensed on whichever machine
// runs `go test` is not a stable fact to test against).
const canonicalMIT = `MIT License

Copyright (c) 2026 Example Author

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

// canonicalApache2 is the standard Apache License 2.0 text (the same text
// github.com/dolthub/dolt/go ships). A second, genuinely different license
// from canonicalMIT — needed so the offset-selection test below actually
// exercises picking one license type over another, not just finding "a"
// match in text that only contains one type repeated.
const canonicalApache2 = `
                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/

   TERMS AND CONDITIONS FOR USE, REPRODUCTION, AND DISTRIBUTION

   1. Definitions.

      "License" shall mean the terms and conditions for use, reproduction,
      and distribution as defined by Sections 1 through 9 of this document.

      "Licensor" shall mean the copyright owner or entity authorized by
      the copyright owner that is granting the License.

      "Legal Entity" shall mean the union of the acting entity and all
      other entities that control, are controlled by, or are under common
      control with that entity. For the purposes of this definition,
      "control" means (i) the power, direct or indirect, to cause the
      direction or management of such entity, whether by contract or
      otherwise, or (ii) ownership of fifty percent (50%) or more of the
      outstanding shares, or (iii) beneficial ownership of such entity.

      "You" (or "Your") shall mean an individual or Legal Entity
      exercising permissions granted by this License.

      "Source" form shall mean the preferred form for making modifications,
      including but not limited to software source code, documentation
      source, and configuration files.

      "Object" form shall mean any form resulting from mechanical
      transformation or translation of a Source form, including but
      not limited to compiled object code, generated documentation,
      and conversions to other media types.

      "Work" shall mean the work of authorship, whether in Source or
      Object form, made available under the License, as indicated by a
      copyright notice that is included in or attached to the work
      (an example is provided in the Appendix below).

      "Derivative Works" shall mean any work, whether in Source or Object
      form, that is based on (or derived from) the Work and for which the
      editorial revisions, annotations, elaborations, or other modifications
      represent, as a whole, an original work of authorship. For the purposes
      of this License, Derivative Works shall not include works that remain
      separable from, or merely link (or bind by name) to the interfaces of,
      the Work and Derivative Works thereof.

      "Contribution" shall mean any work of authorship, including
      the original version of the Work and any modifications or additions
      to that Work or Derivative Works thereof, that is intentionally
      submitted to Licensor for inclusion in the Work by the copyright owner
      or by an individual or Legal Entity authorized to submit on behalf of
      the copyright owner. For the purposes of this definition, "submitted"
      means any form of electronic, verbal, or written communication sent
      to the Licensor or its representatives, including but not limited to
      communication on electronic mailing lists, source code control systems,
      and issue tracking systems that are managed by, or on behalf of, the
      Licensor for the purpose of discussing and improving the Work, but
      excluding communication that is conspicuously marked or otherwise
      designated in writing by the copyright owner as "Not a Contribution."

      "Contributor" shall mean Licensor and any individual or Legal Entity
      on behalf of whom a Contribution has been received by Licensor and
      subsequently incorporated within the Work.

   2. Grant of Copyright License. Subject to the terms and conditions of
      this License, each Contributor hereby grants to You a perpetual,
      worldwide, non-exclusive, no-charge, royalty-free, irrevocable
      copyright license to reproduce, prepare Derivative Works of,
      publicly display, publicly perform, sublicense, and distribute the
      Work and such Derivative Works in Source or Object form.

   3. Grant of Patent License. Subject to the terms and conditions of
      this License, each Contributor hereby grants to You a perpetual,
      worldwide, non-exclusive, no-charge, royalty-free, irrevocable
      (except as stated in this section) patent license to make, have made,
      use, offer to sell, sell, import, and otherwise transfer the Work,
      where such license applies only to those patent claims licensable
      by such Contributor that are necessarily infringed by their
      Contribution(s) alone or by combination of their Contribution(s)
      with the Work to which such Contribution(s) was submitted. If You
      institute patent litigation against any entity (including a
      cross-claim or counterclaim in a lawsuit) alleging that the Work
      or a Contribution incorporated within the Work constitutes direct
      or contributory patent infringement, then any patent licenses
      granted to You under this License for that Work shall terminate
      as of the date such litigation is filed.

   4. Redistribution. You may reproduce and distribute copies of the
      Work or Derivative Works thereof in any medium, with or without
      modifications, and in Source or Object form, provided that You
      meet the following conditions:

      (a) You must give any other recipients of the Work or
          Derivative Works a copy of this License; and

      (b) You must cause any modified files to carry prominent notices
          stating that You changed the files; and

      (c) You must retain, in the Source form of any Derivative Works
          that You distribute, all copyright, patent, trademark, and
          attribution notices from the Source form of the Work,
          excluding those notices that do not pertain to any part of
          the Derivative Works; and

      (d) If the Work includes a "NOTICE" text file as part of its
          distribution, then any Derivative Works that You distribute must
          include a readable copy of the attribution notices contained
          within such NOTICE file, excluding those notices that do not
          pertain to any part of the Derivative Works, in at least one
          of the following places: within a NOTICE text file distributed
          as part of the Derivative Works; within the Source form or
          documentation, if provided along with the Derivative Works; or,
          within a display generated by the Derivative Works, if and
          wherever such third-party notices normally appear. The contents
          of the NOTICE file are for informational purposes only and
          do not modify the License. You may add Your own attribution
          notices within Derivative Works that You distribute, alongside
          or as an addendum to the NOTICE text from the Work, provided
          that such additional attribution notices cannot be construed
          as modifying the License.

      You may add Your own copyright statement to Your modifications and
      may provide additional or different license terms and conditions
      for use, reproduction, or distribution of Your modifications, or
      for any such Derivative Works as a whole, provided Your use,
      reproduction, and distribution of the Work otherwise complies with
      the conditions stated in this License.

   5. Submission of Contributions. Unless You explicitly state otherwise,
      any Contribution intentionally submitted for inclusion in the Work
      by You to the Licensor shall be under the terms and conditions of
      this License, without any additional terms or conditions.
      Notwithstanding the above, nothing herein shall supersede or modify
      the terms of any separate license agreement you may have executed
      with Licensor regarding such Contributions.

   6. Trademarks. This License does not grant permission to use the trade
      names, trademarks, service marks, or product names of the Licensor,
      except as required for reasonable and customary use in describing the
      origin of the Work and reproducing the content of the NOTICE file.

   7. Disclaimer of Warranty. Unless required by applicable law or
      agreed to in writing, Licensor provides the Work (and each
      Contributor provides its Contributions) on an "AS IS" BASIS,
      WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
      implied, including, without limitation, any warranties or conditions
      of TITLE, NON-INFRINGEMENT, MERCHANTABILITY, or FITNESS FOR A
      PARTICULAR PURPOSE. You are solely responsible for determining the
      appropriateness of using or redistributing the Work and assume any
      risks associated with Your exercise of permissions under this License.

   8. Limitation of Liability. In no event and under no legal theory,
      whether in tort (including negligence), contract, or otherwise,
      unless required by applicable law (such as deliberate and grossly
      negligent acts) or agreed to in writing, shall any Contributor be
      liable to You for damages, including any direct, indirect, special,
      incidental, or consequential damages of any character arising as a
      result of this License or out of the use or inability to use the
      Work (including but not limited to damages for loss of goodwill,
      work stoppage, computer failure or malfunction, or any and all
      other commercial damages or losses), even if such Contributor
      has been advised of the possibility of such damages.

   9. Accepting Warranty or Additional Liability. While redistributing
      the Work or Derivative Works thereof, You may choose to offer,
      and charge a fee for, acceptance of support, warranty, indemnity,
      or other liability obligations and/or rights consistent with this
      License. However, in accepting such obligations, You may act only
      on Your own behalf and on Your sole responsibility, not on behalf
      of any other Contributor, and only if You agree to indemnify,
      defend, and hold each Contributor harmless for any liability
      incurred by, or claims asserted against, such Contributor by reason
      of your accepting any such warranty or additional liability.

   END OF TERMS AND CONDITIONS
`

// nonStandardWTFPL is the short, profanely-worded WTFPL variant shipped by
// github.com/kch42/buzhash — real text from a former dependency (removed in
// links-licensing-c0ce.6), kept as the fixture that pins the classifier's
// below-threshold "Unknown" behavior against text a real module actually
// shipped rather than a hypothetical.
const nonStandardWTFPL = `           DO WHATEVER THE FUCK YOU WANT, PUBLIC LICENSE
   TERMS AND CONDITIONS FOR COPYING, DISTRIBUTION AND MODIFICATION

            0. You just DO WHATEVER THE FUCK YOU WANT.
`

func TestClassify(t *testing.T) {
	t.Parallel()
	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		t.Fatalf("build classifier: %v", err)
	}

	t.Run("classifies canonical license text with high confidence", func(t *testing.T) {
		name, confidence := Classify(classifier, canonicalMIT)
		if name != "MIT" {
			t.Errorf("name = %q, want MIT", name)
		}
		if confidence < lc.DefaultConfidenceThreshold {
			t.Errorf("confidence = %v, want >= %v", confidence, lc.DefaultConfidenceThreshold)
		}
	})

	t.Run("falls back to Unknown for text no known license matches", func(t *testing.T) {
		name, confidence := Classify(classifier, nonStandardWTFPL)
		if name != unclassifiedLicense {
			t.Errorf("name = %q, want %q", name, unclassifiedLicense)
		}
		if confidence != 0 {
			t.Errorf("confidence = %v, want 0 for an unclassified match", confidence)
		}
	})

	t.Run("picks the earliest-offset match as the primary license when a file bundles two different licenses", func(t *testing.T) {
		// ASF-convention shape: the module's own license first (Apache-2.0),
		// a bundled third party's license appended after (MIT) — the exact
		// shape observed in this tree for github.com/apache/thrift and
		// go.opentelemetry.io/otel. Using two DIFFERENT license texts (not
		// the same one twice) is the point: it's the only way to tell "picked
		// the earliest offset" apart from "there was only one license name to
		// return" — a regression in the offset-minimum loop would surface
		// this as MIT instead of Apache-2.0.
		bundled := canonicalApache2 + "\n---\nBundled component license:\n\n" + canonicalMIT
		name, _ := Classify(classifier, bundled)
		if name != "Apache-2.0" {
			t.Errorf("name = %q, want Apache-2.0 (the earlier-offset license, not MIT)", name)
		}
	})
}

// TestFindLicenseFileAcceptReject is the accept/reject table for
// FindLicenseFile against a synthetic module directory.
// [LAW:types-are-the-program]
func TestFindLicenseFileAcceptReject(t *testing.T) {
	writeFiles := func(t *testing.T, names ...string) string {
		t.Helper()
		dir := t.TempDir()
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("content"), 0o644); err != nil {
				t.Fatalf("write fixture %s: %v", name, err)
			}
		}
		return dir
	}

	t.Run("finds a bare LICENSE file", func(t *testing.T) {
		dir := writeFiles(t, "LICENSE", "README.md", "main.go")
		got, err := FindLicenseFile(dir)
		if err != nil {
			t.Fatalf("FindLicenseFile: %v", err)
		}
		if filepath.Base(got) != "LICENSE" {
			t.Errorf("got %q, want LICENSE", got)
		}
	})

	t.Run("prefers every canonical bare root name over a second license-shaped file", func(t *testing.T) {
		// Regression table, not a one-off case: bareLicenseNamePattern and
		// licenseFilePattern both derive from licenseRootNames precisely so
		// every name recognized by one is bare-preferred by the other. This
		// table pins that property for the full current set rather than
		// re-adding one hand-picked subtest per name every time a gap is
		// found (LICENCE, then COPYING/UNLICENSE, were each missed that way
		// before the two checks were unified). The real shape this mirrors:
		// gopkg.in/yaml.v2 ships both LICENSE and LICENSE.libyaml (the latter
		// for a vendored C dependency) and LICENSE unambiguously wins.
		for _, name := range []string{"LICENSE", "LICENCE", "COPYING", "UNLICENSE"} {
			dir := writeFiles(t, name, name+".libyaml")
			got, err := FindLicenseFile(dir)
			if err != nil {
				t.Fatalf("FindLicenseFile(%s + %s.libyaml): %v", name, name, err)
			}
			if filepath.Base(got) != name {
				t.Errorf("got %q, want %s preferred over %s.libyaml", got, name, name)
			}
		}
	})

	t.Run("resolves a single match with no bare LICENSE present", func(t *testing.T) {
		dir := writeFiles(t, "LICENSE.txt")
		got, err := FindLicenseFile(dir)
		if err != nil {
			t.Fatalf("FindLicenseFile: %v", err)
		}
		if filepath.Base(got) != "LICENSE.txt" {
			t.Errorf("got %q, want LICENSE.txt", got)
		}
	})

	t.Run("errors on multiple candidates with no bare LICENSE to prefer", func(t *testing.T) {
		// The real shape this guards: a dual-license repo shipping
		// LICENSE-APACHE and LICENSE-MIT with no bare LICENSE — picking one
		// silently would drop a real license option from the bundle with no
		// record a choice was ever made. Naming both candidates in the error
		// forces a human decision instead.
		dir := writeFiles(t, "LICENSE-APACHE", "LICENSE-MIT")
		_, err := FindLicenseFile(dir)
		if err == nil {
			t.Fatal("want error: ambiguous — two license files, neither is the bare LICENSE")
		}
		for _, want := range []string{"LICENSE-APACHE", "LICENSE-MIT"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q doesn't name candidate %q", err, want)
			}
		}
	})

	t.Run("accepts COPYING and UNLICENSE spellings", func(t *testing.T) {
		for _, name := range []string{"COPYING", "UNLICENSE", "LICENCE"} {
			dir := writeFiles(t, name)
			got, err := FindLicenseFile(dir)
			if err != nil {
				t.Fatalf("FindLicenseFile(%s): %v", name, err)
			}
			if filepath.Base(got) != name {
				t.Errorf("got %q, want %q", got, name)
			}
		}
	})

	t.Run("accepts hyphen and underscore separators, not just dot", func(t *testing.T) {
		// Real-world dual-license convention: LICENSE-APACHE / LICENSE-MIT
		// side by side, or LICENSE_MIT alone. filepath.Base equality (not
		// FindLicenseFile's "no bare LICENSE, fall back" branch) confirms
		// each is recognized as a license file at all — bare-LICENSE
		// preference is already covered above.
		for _, name := range []string{"LICENSE-APACHE", "LICENSE_MIT", "LICENSE-Apache-2.0"} {
			dir := writeFiles(t, name)
			got, err := FindLicenseFile(dir)
			if err != nil {
				t.Fatalf("FindLicenseFile(%s): %v", name, err)
			}
			if filepath.Base(got) != name {
				t.Errorf("got %q, want %q", got, name)
			}
		}
	})

	t.Run("does not match README or NOTICE alone", func(t *testing.T) {
		dir := writeFiles(t, "README.md", "NOTICE")
		if _, err := FindLicenseFile(dir); err == nil {
			t.Fatal("want error: README/NOTICE alone is not a license grant")
		}
	})

	t.Run("errors on a directory with no license-shaped file", func(t *testing.T) {
		dir := writeFiles(t, "main.go", "go.mod")
		if _, err := FindLicenseFile(dir); err == nil {
			t.Fatal("want error for a directory with no license file")
		}
	})

	t.Run("ignores subdirectories matching the license pattern", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "LICENSE"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := FindLicenseFile(dir); err == nil {
			t.Fatal("want error: a directory named LICENSE is not a license file")
		}
	})
}

func TestFindLicenseFileMissingDir(t *testing.T) {
	if _, err := FindLicenseFile(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("want error for a nonexistent module dir")
	} else if !strings.Contains(err.Error(), "read module dir") {
		t.Errorf("error = %v, want it to name the failing operation", err)
	}
}
