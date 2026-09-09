# Releasing issue-bot

Run `make check build`, inspect `git diff`, and commit the complete change.
The shared ACP dependency must be a published version, with no local replace
or workspace override needed to build. Keep `go.mod` and `go.sum` committed.

Push master and a new semver tag such as `v0.3.1`. That is the only release
trigger needed. `publish-packages.yml` runs Linux/macOS CI through the reusable
`release.yml`, builds and publishes the native GitHub assets, then builds/tests
and uploads all five npm packages at the exact same tag and commit. A failed
native release prevents npm publication. Follow the **Publish packages** run for
the complete result. npm's trusted publisher remains `publish-packages.yml` in
the `packages-publish` environment.

The installer expects `brokk-issue-bot-VERSION-OS-ARCH.tar.gz`, containing `bib`,
and `checksums.txt`. Supported targets are Linux/macOS, amd64/arm64.
`python3 scripts/package_release.py v0.1.1` builds these archives locally.
The tag is also the Go module release. No separate Go upload is needed.
Python publication is not configured.

The package job validates native checksums, package contents, local installs and
existing-version integrity before uploading. Platform packages are submitted
before the launcher. Upload errors fail the job; successful npm uploads do not
wait for public version indexes or run immediate public-install smoke tests.

For a partial npm failure, rerun failed jobs or dispatch `publish-packages.yml`
from the exact existing tag with its tag input and `publish=true`. Use
`publish=false` for validation without uploads. Matching existing package bytes
are retained; conflicting versions stop publication. The explicit
`python3 scripts/package_registry.py verify dist/packages` command remains
available for later public-integrity checks after registry propagation.

If publication fails after draft creation, inspect the draft and uploaded assets.
Complete or replace that draft explicitly; do not move published version tags.

## License validation

Before committing release preparation, run `python3 scripts/licenses.py`.
For dependency or Go version changes, follow [licenses/README.md](licenses/README.md)
to review the policy and regenerate notices. Native packaging repeats this
check and includes the exact project license, notice, and dependency report.
Every npm package retains these files from the verified native assets. The
package smoke test inspects their bytes as well as exercising installation.
