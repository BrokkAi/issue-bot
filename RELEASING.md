# Releasing issue-bot

Run `make check build`, inspect `git diff`, and commit the complete change.
The shared ACP dependency must be a published version, with no local replace
or workspace override needed to build. Keep `go.mod` and `go.sum` committed.

Push master and a new semver tag such as `v0.1.1`. The Release workflow runs
Linux/macOS checks, builds all four platform archives, verifies checksums and
smoke-tests the Linux binary. It uploads a draft release and publishes it only
after uploads succeed. Monitor the workflow and verify the release assets.

The installer expects `brokk-issue-bot-VERSION-OS-ARCH.tar.gz`, containing `bib`,
and `checksums.txt`. Supported targets are Linux/macOS, amd64/arm64.
`python3 scripts/package_release.py v0.1.1` builds these archives locally.
The tag is the Go module release. Publish the npm installers using
`publish-packages.yml` from that same tag, following the README's package
publication instructions. Python publication is not configured.

npm may accept an upload several minutes before exposing its version publicly.
The publisher submits the four independent platform packages first, waits for
their exact bytes to become visible, then publishes the root launcher. It allows
up to ten minutes per registry visibility check and reports progress while waiting.
If the final public install still hits a stale registry index, retry that workflow
after propagation; matching published versions are verified without re-uploading.

If publication fails after draft creation, inspect the draft and uploaded assets.
Complete or replace that draft explicitly; do not move published version tags.
