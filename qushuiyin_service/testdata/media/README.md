# Synthetic media fixtures

The WebP fixtures contain generated 32 × 24 pixel patterns, with no platform media or metadata. `opaque.webp` and `alpha.webp` use FFmpeg's `libwebp` encoder; `*-lossless.webp` use `-lossless 1`. The alpha pattern has one half at alpha 80/255 and one half opaque. `animated.webp` contains two frames generated with `testsrc2=size=32x24:rate=2:duration=1` and `libwebp_anim`.

Tests decode the committed bytes directly; FFmpeg is not required to run image tests. They cover lossy/lossless conversion, preservation of transparency and dimensions, animation rejection, malformed containers, and input/output resource limits.
