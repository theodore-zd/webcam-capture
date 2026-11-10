# webcam-capture

Periodic webcam capture service. Runs in Docker on a Linux box with a V4L2
camera (e.g. `/dev/video0`).

- Takes a photo every 20 seconds into an hour subfolder:
  `data/<year>/<Month-name>/<day>/<1am-11pm>/<yyyy>-<mm>-<dd>-<hh>-<mm>-<ss>.jpeg`
  e.g. `data/2026/November/08/1pm/...` (timestamps in the container's TZ — set
  `WEBCAM_TZ`, e.g. `America/New_York`)
- Serves a small web UI on `:8080` (`WEBCAM_HTTP_ADDR`): latest capture,
  per-day/per-hour summaries, data-folder size, free disk space
- On date rollover, zips the finished day's folder into the month folder and
  deletes the originals; also sweeps missed days at startup

## Run

Requires docker + docker compose, and `git` for deploys. On the target box:

```bash
# one-time: bare repo for deploys
git init --bare ~/webcam-capture.git
```

From a machine with ssh access (see `scripts/webcam-deploy.sh`):

```bash
./scripts/webcam-deploy.sh <ssh-host>
```

## Config (compose.yaml)

| Variable     | Default | Meaning                          |
|--------------|---------|----------------------------------|
| `WEBCAM_TZ`  | `UTC`   | timezone for naming + rollover   |
| `WEBCAM_HTTP_ADDR` | `:8080` | web UI listen address (map in compose.yaml) |
| device       | `/dev/video0` | V4L2 device passed through |

Quality/resolution is set in `main.go` (`-video_size`, `-q:v`).

## Files

- `main.go` — capture loop (hourly subfolders), day-rollover zip sweep, web UI startup
- `web.go` — web UI: latest photo, day/hour summaries, data size, disk free
- `Dockerfile` — two-stage build: Go builder → alpine + ffmpeg + tzdata
- `compose.yaml` — device passthrough, relative `./data` volume, restart policy
