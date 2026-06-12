# Demo media

Recorded demo clips, served at the site root — a file `revoke-cascade.mp4` is
referenced from the docs as `/media/revoke-cascade.mp4`.

Current clips (referenced by `docs/demos.md`), each shipped as **both** WebM/VP9
and H.264 MP4:

| Clip | Shows |
|------|-------|
| `general-demo.{webm,mp4}` | A user chatting with the long-lived `weather-monitor` agent. |
| `revoke-cascade.{webm,mp4}` | Chained agents: CIBA spawn consent + real-time revocation cascade. |

**Why two formats:** the page lists the WebM source first, MP4 second. VP9/WebM
is royalty-free and decodes in every Chromium browser (including Opera on Linux,
which often lacks proprietary H.264) and Firefox; Safari falls back to the MP4.
Raw SimpleScreenRecorder output is H.264-only, so it failed to play in Opera —
hence both. Re-encode raw captures with (square pixels + faststart):

    ffmpeg -i raw.mkv -vf "setsar=1" -c:v libvpx-vp9 -crf 33 -b:v 0 -row-mt 1 -an out.webm
    ffmpeg -i raw.mkv -vf "setsar=1" -c:v libx264 -profile:v high -pix_fmt yuv420p \
      -crf 23 -preset medium -an -movflags +faststart out.mp4

Specs (see `website/recording-guide.md` for the shot-by-shot scripts):

- **Format:** H.264 MP4, ~720p, target a few MB each (trim dead air hard).
- **Posters (optional):** the page relies on the browser showing the first
  frame (`preload="metadata"`). To pin a specific frame, add a same-named `.jpg`
  (e.g. `general-demo.jpg`) and a `poster="/media/general-demo.jpg"` attribute on
  the `<video>` in `docs/demos.md`.
- These clips double as native LinkedIn uploads.
