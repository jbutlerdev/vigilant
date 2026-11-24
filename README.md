# Vigilant

A lightweight, modern NVR — a single static binary. Loosely inspired by
[Frigate](https://frigate.video), but with no ML, no Python, and no
containers: just `vigilant -c config.yaml`.

## Features

- **Single camera connection** — every consumer (recorder, motion detector,
  web viewers, API clients) is served by a managed
  [go2rtc](https://github.com/AlexxIT/go2rtc) child process, so the camera
  itself only ever sees one connection. go2rtc is auto-downloaded on first
  start if it isn't on `PATH`.
- **Continuous recording** — full-time MP4 segments, 1 hour each, split
  exactly on the clock hour (stream copy via ffmpeg, zero transcode).
  Default retention **7 days**, configurable.
- **Motion detection** — ffmpeg decodes a tiny grayscale frame (default
  320×180 @ 5 fps from the camera's substream) and Vigilant runs a
  vectorizable frame-differencing pass; total load is a fraction of one
  core. A **temporal filter** (a pixel only counts when it changed in two
  consecutive diffs) suppresses compression flicker, IR noise and swaying
  foliage. A **sliding window + state machine** turns per-frame results
  into stable events: N motion frames inside the window (default 4 in 5 s)
  start an event, and every new motion frame resets the hold timer (default
  15 s) before the event closes.
- **Dual retention** — when an event closes, precise stream-copy clips of
  the event (plus configurable buffer) are extracted into `data/events/…`
  under a separate **30-day** (configurable) motion policy — small
  playable MP4s, not hour-long copies. If an event is still inside the
  hour currently being recorded, extraction defers until the segment
  completes. Continuous recordings also honor a `max_total_gb` disk budget
  (oldest pruned first).
- **Masks** — ignore rectangles/polygons (normalized coordinates) in YAML,
  viewable and editable in the web UI, or via the REST API. Saved masks
  persist to the config file and hot-apply to running detectors.
- **Web UI** — live viewing (WebRTC/MSE through the go2rtc proxy), live
  motion badges via SSE, event timeline with thumbnails and clip playback,
  recording browser, and a canvas mask editor.
- **REST API** — everything above is scriptable.
- **Single user, no password** — by design; put it on a trusted network.

## Requirements

- `ffmpeg` on `PATH` (used for segmenting and motion frame decode).
- Nothing else. go2rtc is fetched automatically when missing.

## Build

```sh
make            # static binary in ./dist/vigilant (CGO_ENABLED=0)
make run        # build + run with ./config.yaml
```

Or without make:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o vigilant .
```

Cross-compile examples (the result is a fully static binary):

```sh
GOOS=linux GOARCH=arm64 make
```

## Run

```sh
cp config.example.yaml config.yaml   # edit your cameras
./vigilant -c config.yaml
```

Open http://localhost:8080.

## Configuration

Everything lives in one YAML file (see [`config.example.yaml`](config.example.yaml)):

```yaml
server:    { listen: ":8080" }
data_dir:  ./data
go2rtc:    { binary: go2rtc, rtsp_listen: "127.0.0.1:8554", api_listen: "127.0.0.1:1984" }
retention: { continuous_days: 7, motion_days: 30, max_total_gb: 60 }
cameras:
  - name: front-door
    url: "rtsp://…/main"
    motion:
      url: "rtsp://…/sub"     # optional low-res substream
      width: 320
      height: 180
      fps: 5
      threshold: 30           # per-pixel luma delta that counts as "changed"
      min_percent: 0.4        # % of the (unmasked) frame that must change for
                              # a motion frame (the main sensitivity knob;
                              # legacy min_pixels / min_ratio also accepted)
      window: 5s              # sliding window
      min_frames: 4           # motion frames in window => MOTION state
      hold: 15s               # motion frame resets; expiry closes event
      buffer: 10s             # clip padding around events
      mask:
        rectangles: [ { x: 0, y: 0.85, w: 0.25, h: 0.15 } ]
        polygons:   [ { points: [[0.7,0],[1,0],[1,0.3],[0.8,0.2]] } ]
```

## Data layout

```
data/
  recordings/<camera>/2006-01-02-15-04-05.mp4  continuous, fragmented MP4,
                                   cut on the clock hour, named by start time
  events/<camera>/<id>/meta.json           event index + metadata
  events/<camera>/<id>/thumbnail.jpg
  events/<camera>/<id>/clip-2006-01-02-15-04-05.mp4  precise event clips (motion retention)
```

A cleaner runs hourly and prunes both policies independently.

## Motion state machine

```
            min_frames motion frames in window
   IDLE ────────────────────────────────────────► MOTION
    ▲                                               │
    │        hold expires with no motion frame      │
    └───────────────────────────────────────────────┘
```

Every motion frame appends a timestamp to the sliding window, which both
re-triggers the window and resets the hold timer — a person walking across
the scene keeps the event open; a leaf flicker never leaves IDLE.

## REST API

| Method | Path | Description |
| ------ | ---- | ----------- |
| `GET` | `/api/state` | Live aggregate state (motion per camera, active events) |
| `GET` | `/api/sse` | Server-sent events stream of state snapshots |
| `GET` | `/api/config` | Current configuration |
| `GET` | `/api/cameras` | Cameras incl. resolved motion settings + mask |
| `GET` | `/api/frame.jpg?camera=NAME` | JPEG snapshot |
| `PUT` | `/api/cameras/NAME/mask` | Replace mask (JSON `{rectangles, polygons}`), persists to YAML, hot-applies |
| `PUT` | `/api/cameras/NAME/settings` | Edit camera URL / record & motion toggles / motion tuning; persists to YAML and restarts that camera's pipelines |
| `PUT` | `/api/config/retention` | Update `continuous_days` / `motion_days`; persists to YAML |
| `GET` | `/api/events?camera=&from=&to=&limit=` | List events |
| `GET` | `/api/events/{id}` | One event |
| `DELETE` | `/api/events/{id}` | Delete an event |
| `GET` | `/api/events/{id}/thumbnail` | Event thumbnail JPEG |
| `GET` | `/api/events/{id}/clip/{file}` | Event clip MP4 (range requests supported) |
| `GET` | `/api/recordings?camera=&from=&to=` | List continuous segments |
| `GET` | `/api/recordings/file?camera=&file=` | Download/play a segment |
| `/go2rtc/*` | | Proxied go2rtc API (WebRTC/MSE/WebSocket live streaming) |

`from`/`to` accept `2006-01-02`, `2006-01-02T15:04`, or RFC3339.

Example integrations:

```sh
# Is anyone home?
curl -s localhost:8080/api/state | jq '.cameras[] | {name, motion: .active_event != null}'

# Latest event for a camera
curl -s 'localhost:8080/api/events?camera=front-door&limit=1' | jq '.[0]'

# Ignore the street at the top of the frame
curl -X PUT localhost:8080/api/cameras/front-door/mask \
  -d '{"rectangles":[{"x":0,"y":0,"w":1,"h":0.3}]}'
```

## License

MIT
