package main

// Simple read-only web UI: latest photo, per-day/per-hour summaries, data size,
// and free disk space. Serve with startWeb(":8080") — see compose.yaml.

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type hourStat struct {
	Name  string
	Count int
	Bytes int64
}

type dayStat struct {
	Date  string // display date, e.g. "2026 November 10"
	Count int
	Bytes int64
	Hours []hourStat
	Open  bool // expanded in the HTML details element
}

type pageData struct {
	Now            string
	LatestPath     string
	LatestTime     string
	TotalCount     int
	TotalDataBytes int64
	FreeBytes      int64
	TotalDiskBytes int64
	Days           []dayStat
}

// buildSummary walks dataDir and aggregates counts/sizes per day and hour.
// The data layout itself encodes the grouping:
// data/<year>/<Month>/<day>/<hour>/<file> (hour optional for legacy files).
func buildSummary() (*pageData, error) {
	pd := &pageData{}

	type dayAgg struct {
		day   dayStat
		hours map[string]*hourStat
	}
	aggs := map[string]*dayAgg{}
	var latest time.Time

	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip unreadable entries silently
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 4 { // year/month/day/file minimum
			return nil
		}
		dayKey := parts[0] + "|" + parts[1] + "|" + parts[2]
		ag := aggs[dayKey]
		if ag == nil {
			ag = &dayAgg{day: dayStat{Date: parts[0] + " " + parts[1] + " " + parts[2]}, hours: map[string]*hourStat{}}
			aggs[dayKey] = ag
		}
		ag.day.Count++
		ag.day.Bytes += info.Size()
		hour := "(root)" // legacy files directly in the day dir
		if len(parts) == 5 {
			hour = parts[3]
		}
		hs := ag.hours[hour]
		if hs == nil {
			hs = &hourStat{Name: hour}
			ag.hours[hour] = hs
		}
		hs.Count++
		hs.Bytes += info.Size()
		pd.TotalCount++
		pd.TotalDataBytes += info.Size()
		if info.ModTime().After(latest) {
			latest = info.ModTime()
			pd.LatestPath = path
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pd.LatestTime = latest.In(loc).Format("2006-01-02 15:04:05")

	// Days newest first; hours chronological.
	for _, ag := range aggs {
		for _, hs := range ag.hours {
			ag.day.Hours = append(ag.day.Hours, *hs)
		}
		sort.Slice(ag.day.Hours, func(i, j int) bool {
			ti, ei := parseHour(ag.day.Hours[i].Name)
			tj, ej := parseHour(ag.day.Hours[j].Name)
			if ei == ej {
				return ag.day.Hours[i].Name < ag.day.Hours[j].Name
			}
			return ti.Before(tj)
		})
		pd.Days = append(pd.Days, ag.day)
	}
	sort.Slice(pd.Days, func(i, j int) bool {
		ki := strings.Fields(pd.Days[i].Date)
		kj := strings.Fields(pd.Days[j].Date)
		ti, ei := time.Parse("2006 January 02", strings.Join(ki, " "))
		tj, ej := time.Parse("2006 January 02", strings.Join(kj, " "))
		if ei != nil || ej != nil {
			return strings.Join(ki, " ") > strings.Join(kj, " ")
		}
		return ti.After(tj)
	})
	if len(pd.Days) > 0 {
		pd.Days[0].Open = true
	}
	if len(pd.Days) > 14 {
		pd.Days = pd.Days[:14]
	}

	if st, err := diskUsage(dataDir); err == nil {
		pd.FreeBytes = st.avail
		pd.TotalDiskBytes = st.total
	}
	return pd, nil
}

func parseHour(name string) (time.Time, error) {
	return time.Parse("3pm", name)
}

type diskInfo struct {
	avail, total int64
}

func diskUsage(path string) (diskInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskInfo{}, err
	}
	return diskInfo{avail: int64(st.Bavail) * st.Bsize, total: int64(st.Blocks) * st.Bsize}, nil
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="30">
<title>webcam-capture</title>
<style>
  :root { color-scheme: dark; }
  body { background: #111; color: #ddd; font-family: system-ui, sans-serif;
         max-width: 960px; margin: 0 auto; padding: 1rem; }
  h1, h2 { color: #7fb4ff; }
  img#latest { max-width: 100%; border-radius: 8px; }
  .cards { display: flex; gap: 1rem; flex-wrap: wrap; }
  .card { background: #1c1c1c; border: 1px solid #333; border-radius: 8px;
          padding: .75rem 1rem; }
  .card b { color: #fff; }
  details { background: #1c1c1c; border: 1px solid #333; border-radius: 8px;
            padding: .5rem 1rem; margin: .5rem 0; }
  summary { cursor: pointer; font-weight: 600; }
  .hours { columns: 2; list-style: none; padding-left: 0; }
  .hours li { padding: .1rem 0; }
  footer { color: #666; font-size: .85rem; }
</style>
</head>
<body>
<h1>webcam-capture</h1>
<h2>Latest capture {{if .LatestTime}}{{.LatestTime}}{{end}}</h2>
<img id="latest" src="/latest.jpg" alt="latest capture">
<div class="cards">
  <div class="card">Photos: <b>{{.TotalCount}}</b></div>
  <div class="card">Data size: <b>{{.TotalDataBytes | human}}</b></div>
  <div class="card">Disk free: <b>{{.FreeBytes | human}}</b> of {{.TotalDiskBytes | human}}</div>
</div>
<h2>Captured</h2>
{{if .Days}}{{range .Days}}<details{{if .Open}} open{{end}}>
  <summary>{{.Date}} — {{.Count}} photos ({{.Bytes | human}})</summary>
  <ul class="hours">{{range .Hours}}<li>{{.Name}} — {{.Count}} photos ({{.Bytes | human}})</li>{{end}}</ul>
</details>{{end}}{{else}}<p>No photos yet.</p>{{end}}
<footer>refreshes every 30s · webcam-capture web UI</footer>
</body>
</html>
`

var indexTmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"human": humanBytes,
}).Parse(indexHTML))

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	pd, err := buildSummary()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := indexTmpl.Execute(w, pd); err != nil {
		log.Printf("index: %v", err)
	}
}

func handleLatest(w http.ResponseWriter, r *http.Request) {
	pd, err := buildSummary()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pd.LatestPath == "" {
		http.Error(w, "no photos yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, pd.LatestPath)
}

// startWeb serves the UI on addr (e.g. ":8080"). Blocks forever.
func startWeb(addr string) {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/latest.jpg", handleLatest)
	log.Printf("web UI listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("web: %v", err)
	}
}
