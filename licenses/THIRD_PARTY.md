# Third-party software

The Audiobookshelf pak bundles the following:

| Component | Used for | License |
|-----------|----------|---------|
| [Apostrophe](https://github.com/Helaas/Apostrophe) | UI toolkit (header-only, vendored in `ui/third_party/apostrophe`) | MIT, see `apostrophe-MIT.txt` |
| [cJSON](https://github.com/DaveGamble/cJSON) v1.7.18 | JSON parsing in the UI | MIT, see `cjson-MIT.txt` |
| [FFmpeg](https://ffmpeg.org) 7.1.2 | Audio decoding and output (`bin/ffmpeg`) | LGPL-2.1-or-later |
| [Go x/crypto x509roots/fallback](https://pkg.go.dev/golang.org/x/crypto/x509roots/fallback) | Mozilla CA bundle for HTTPS when the device has none | BSD-3-Clause |

FFmpeg is built from the unmodified upstream source release
(`https://ffmpeg.org/releases/ffmpeg-7.1.2.tar.xz`) without `--enable-gpl` or
`--enable-nonfree`, using the configuration in `scripts/build-ffmpeg.sh` of this
repository. That script, together with the upstream source, is everything
needed to rebuild the bundled binary.
