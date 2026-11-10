# webcam-capture

Periodic webcam capture service. Runs in Docker on a Linux box with a V4L2
camera (e.g. `/dev/video0`).

- Takes a photo every 20 seconds
- Stores it as `data/<year>/<Month-name>/<day>/<yyyy>-<mm>-<dd>-<hh>-<mm>-<ss>.jpeg`
  e.g. `data/2026/November/08/...` (timestamps in the container's TZ — set `WEBCAM_TZ`, e.g. `America/New_York`)
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
| device       | `/dev/video0` | V4L2 device passed through |

Quality/resolution is set in `main.go` (`-video_size`, `-q:v`).

## Files

- `main.go` — capture loop, day-rollover zip sweep
- `Dockerfile` — two-stage build: Go builder → alpine + ffmpeg + tzdata
- `compose.yaml` — device passthrough, relative `./data` volume, restart policy
