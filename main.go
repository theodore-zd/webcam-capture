package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	dataDir  = "/data"
	interval = 20 * time.Second
)

// loc is the container's local time (set via the TZ environment variable in
// compose.yaml), used for all folder/file naming and day rollover.
var loc = time.Local

func main() {
	log.SetFlags(log.LstdFlags)
	log.Printf("webcam-capture started: interval=%s tz=%s data=%s", interval, time.Local, dataDir)

	sweep()

	current := ""
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		now := time.Now().In(loc)
		today := now.Format("2006-01-02")
		if today != current {
			sweep()
			current = today
		}
		if err := capture(now); err != nil {
			log.Printf("capture failed: %v", err)
		}
		<-ticker.C
	}
}

func dayDir(t time.Time) string {
	return filepath.Join(dataDir, t.Format("2006"), t.Format("01"), t.Format("02"))
}

func capture(now time.Time) error {
	dir := dayDir(now)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	stamp := now.Format("2006-01-02-15-04-05")
	out := filepath.Join(dir, stamp+".jpeg")
	for i := 1; ; i++ {
		if _, err := os.Stat(out); errors.Is(err, fs.ErrNotExist) {
			break
		}
		out = filepath.Join(dir, fmt.Sprintf("%s-%d.jpeg", stamp, i))
	}
	tmp := out + ".part"

	// -update 1: ffmpeg 7.x requires it to write a single frame to a plain filename.
	// -f image2: the .part temp filename has no image extension, so force the muxer.
	base := []string{"-hide_banner", "-loglevel", "error", "-f", "v4l2",
		"-i", "/dev/video0", "-frames:v", "1", "-q:v", "2", "-update", "1",
		"-f", "image2", "-y", tmp}
	attempts := [][]string{
		// NexiGo N60: prefer explicit MJPEG 1080p — low CPU, full sensor res.
		append([]string{"ffmpeg", "-input_format", "mjpeg", "-video_size", "1920x1080"}, base...),
		// Fallback: let ffmpeg negotiate whatever the device offers.
		append([]string{"ffmpeg"}, base...),
	}

	var errs []string
	for _, args := range attempts {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stderr = os.Stderr // surface ffmpeg errors in container logs
		if err := cmd.Run(); err == nil {
			if err := os.Rename(tmp, out); err != nil {
				return err
			}
			return nil
		} else {
			errs = append(errs, err.Error())
			os.Remove(tmp)
		}
	}
	return fmt.Errorf("all ffmpeg attempts failed: %s", strings.Join(errs, " / "))
}

// sweep zips every completed day folder older than today and deletes the originals.
// Runs at startup and whenever the local date (TZ) rolls over.
func sweep() {
	today := time.Now().In(loc).Format("2006-01-02")

	years, err := os.ReadDir(dataDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("sweep: read %s: %v", dataDir, err)
		}
		return
	}
	for _, y := range years {
		if !y.IsDir() || !isNum(y.Name(), 4) {
			continue
		}
		months, err := os.ReadDir(filepath.Join(dataDir, y.Name()))
		if err != nil {
			continue
		}
		for _, m := range months {
			if !m.IsDir() || !isNum(m.Name(), 2) {
				continue
			}
			days, err := os.ReadDir(filepath.Join(dataDir, y.Name(), m.Name()))
			if err != nil {
				continue
			}
			for _, d := range days {
				if !d.IsDir() || !isNum(d.Name(), 2) {
					continue
				}
				date := y.Name() + "-" + m.Name() + "-" + d.Name()
				if date >= today {
					continue
				}
				if err := zipDay(filepath.Join(dataDir, y.Name(), m.Name(), d.Name()), date); err != nil {
					log.Printf("sweep: zip %s: %v", date, err)
				}
			}
		}
	}
}

func zipDay(dayDir, date string) error {
	zipPath := filepath.Join(filepath.Dir(dayDir), date+".zip")

	if err := os.MkdirAll(filepath.Dir(dayDir), 0o755); err != nil {
		return err
	}
	zf, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(zf)

	var files int
	walkErr := filepath.Walk(dayDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jpeg") {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = strings.TrimPrefix(path, dayDir+"/")
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		files++
		return nil
	})
	if err := zw.Close(); err != nil && walkErr == nil {
		walkErr = err
	}
	if err := zf.Close(); err != nil && walkErr == nil {
		walkErr = err
	}
	if walkErr != nil {
		os.Remove(zipPath)
		return walkErr
	}

	if err := os.RemoveAll(dayDir); err != nil {
		return err
	}
	log.Printf("zipped %s: %d photos -> %s", date, files, zipPath)
	return nil
}

func isNum(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
