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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type hourStat struct {
	Name  string
	Link  string // query string to open this hour in the viewer
	Count int
	Bytes int64
}

type dayStat struct {
	Date  string // display date, e.g. "2026 November 10"
	Link  string // date query param for the viewer
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
			mt, err := time.Parse("January", parts[1])
			if err != nil {
				mt, err = time.Parse("01", parts[1])
			}
			if err == nil {
				ag.day.Link = fmt.Sprintf("date=%s-%02d-%s", parts[0], int(mt.Month()), parts[2])
			}
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
			if hour != "(root)" && ag.day.Link != "" {
				hs.Link = ag.day.Link + "&hour=" + hour
			}
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

// ---- viewer: /view (one photo, prev/next, hour timeline, filmstrip) ----

var imgNameRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2}(-\d+)?\.jpeg$`)

type photoRef struct {
	hour string
	name string
}

type hourLink struct {
	Name    string
	StartN  int
	Count   int
	Current bool
}

type thumb struct {
	N       int
	Src     string
	Current bool
}

type viewerData struct {
	DayLink   string // e.g. "2026-11-10"
	DayLabel  string
	HourLabel string
	Pos       int // 0-based global index within the day
	Total     int
	Src       string
	Caption   string
	HasPrev   bool
	HasNext   bool
	PrevN     int
	NextN     int
	Hours     []hourLink
	Thumbs    []thumb
}

// dayDirPath returns the filesystem dir for a yyyy-mm-dd date, or "" if invalid.
func dayDirPath(dateStr string) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return ""
	}
	return filepath.Join(dataDir, t.Format("2006"), t.Format("January"), t.Format("02"))
}

// flattenDay lists every photo of a day in chronological order (hour buckets
// sorted by their 3pm name, filenames within). Files outside hour folders
// (legacy layout) are included, sorted last.
func flattenDay(dateStr string) (photos []photoRef, hours []string) {
	dayDir := dayDirPath(dateStr)
	ents, err := os.ReadDir(dayDir)
	if err != nil {
		return nil, nil
	}
	addFiles := func(hour string) {
		fdir := dayDir
		if hour != "(root)" {
			fdir = filepath.Join(dayDir, hour)
		}
		fents, err := os.ReadDir(fdir)
		if err != nil {
			return
		}
		names := []string{}
		for _, f := range fents {
			if !f.IsDir() && imgNameRe.MatchString(f.Name()) {
				names = append(names, f.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			photos = append(photos, photoRef{hour: hour, name: n})
		}
		if len(names) > 0 && hour != "(root)" {
			hours = append(hours, hour)
		}
	}
	var hourDirs []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := parseHour(e.Name()); err == nil {
				hourDirs = append(hourDirs, e.Name())
			}
		}
	}
	sort.Slice(hourDirs, func(i, j int) bool {
		ti, _ := parseHour(hourDirs[i])
		tj, _ := parseHour(hourDirs[j])
		return ti.Before(tj)
	})
	for _, h := range hourDirs {
		addFiles(h)
	}
	addFiles("(root)") // legacy files directly in the day dir
	return photos, hours
}

func imgSrc(dateStr, hour, name string) string {
	return "/img?date=" + dateStr + "&hour=" + hour + "&name=" + name
}

// photoCaption turns a filename stamp into a friendly local-time caption.
func photoCaption(name string) string {
	t, err := time.ParseInLocation("2006-01-02-15-04-05", strings.TrimSuffix(name, ".jpeg"), loc)
	if err != nil {
		return name
	}
	return t.Format("15:04:05 · January 2, 2006")
}

func handleView(w http.ResponseWriter, r *http.Request) {
	dateStr := r.URL.Query().Get("date")
	if dayDirPath(dateStr) == "" {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	photos, hours := flattenDay(dateStr)
	if len(photos) == 0 {
		http.Error(w, "no photos that day", http.StatusNotFound)
		return
	}
	pos := 0
	if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil {
		pos = n
	} else if h := r.URL.Query().Get("hour"); h != "" {
		for i, p := range photos { // jump to first photo of that hour
			if p.hour == h {
				pos = i
				break
			}
		}
	}
	if pos < 0 {
		pos = 0
	}
	if pos >= len(photos) {
		pos = len(photos) - 1
	}
	cur := photos[pos]

	vd := viewerData{
		DayLink:   dateStr,
		DayLabel:  cur.hour,
		HourLabel: cur.hour,
		Pos:       pos,
		Total:     len(photos),
		Src:       imgSrc(dateStr, cur.hour, cur.name),
		Caption:   photoCaption(cur.name),
		HasPrev:   pos > 0,
		HasNext:   pos < len(photos)-1,
		PrevN:     pos - 1,
		NextN:     pos + 1,
	}
	for _, h := range hours {
		start := -1
		count := 0
		for i, p := range photos {
			if p.hour == h {
				if start == -1 {
					start = i
				}
				count++
			}
		}
		vd.Hours = append(vd.Hours, hourLink{Name: h, StartN: start, Count: count, Current: h == cur.hour})
	}
	for i, p := range photos {
		if p.hour == cur.hour {
			vd.Thumbs = append(vd.Thumbs, thumb{N: i, Src: imgSrc(dateStr, p.hour, p.name), Current: i == pos})
		}
	}

	t, err := time.Parse("2006-01-02", dateStr)
	if err == nil {
		vd.DayLabel = t.In(loc).Format("Monday, January 2, 2006")
	}
	if err := viewTmpl.Execute(w, vd); err != nil {
		log.Printf("view: %v", err)
	}
}

// handleImg serves a single photo; all params are strictly validated.
func handleImg(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dateStr, hourStr, name := q.Get("date"), q.Get("hour"), q.Get("name")
	ht, err := time.Parse("3pm", hourStr)
	if err != nil || !imgNameRe.MatchString(name) {
		http.Error(w, "bad params", http.StatusBadRequest)
		return
	}
	dir := dayDirPath(dateStr)
	if dir == "" {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	path := filepath.Join(dir, ht.Format("3pm"), name)
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	http.ServeFile(w, r, path)
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
  <ul class="hours">{{range .Hours}}{{if .Link}}<li><a href="/view?{{.Link}}">{{.Name}}</a> — {{.Count}} photos ({{.Bytes | human}})</li>{{else}}<li>{{.Name}} — {{.Count}} photos ({{.Bytes | human}})</li>{{end}}{{end}}</ul>
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

const viewHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>webcam-capture — viewer</title>
<style>
  :root { color-scheme: dark; }
  body { background: #111; color: #ddd; font-family: system-ui, sans-serif;
         max-width: 1100px; margin: 0 auto; padding: 1rem; }
  a { color: #7fb4ff; text-decoration: none; }
  h1 { font-size: 1.2rem; margin: 0; }
  .topbar { display: flex; align-items: baseline; gap: 1rem; }
  .nav { display: flex; align-items: center; justify-content: center; gap: 1rem;
         margin: .75rem 0; }
  .nav a, .nav button { background: #23405e; color: #cde3ff; border: 0; border-radius: 6px;
         padding: .5rem 1.1rem; font-size: 1rem; cursor: pointer; }
  .nav a.disabled, .nav button:disabled { opacity: .3; pointer-events: none; }
  #photo { max-width: 100%; max-height: 70vh; display: block; margin: 0 auto;
           border-radius: 8px; background: #000; }
  .caption { text-align: center; color: #999; margin: .4rem 0; }
  .timeline { display: flex; gap: 2px; margin: .6rem 0; }
  .timeline a { flex: 1; text-align: center; background: #1c1c1c; border: 1px solid #333;
                border-radius: 4px; padding: .25rem 0; font-size: .75rem; color: #8899aa; }
  .timeline a:hover { background: #23405e; color: #cde3ff; }
  .timeline a.cur { background: #23405e; color: #fff; border-color: #7fb4ff; }
  .strip { display: flex; gap: 4px; overflow-x: auto; padding: .5rem 0;
           scrollbar-width: thin; }
  .strip img { height: 60px; border: 2px solid transparent; border-radius: 4px; opacity: .55; }
  .strip img.cur { border-color: #7fb4ff; opacity: 1; }
</style>
</head>
<body>
<div class="topbar">
  <h1><a href="/">← webcam-capture</a></h1>
  <h1>{{.DayLabel}}</h1>
  <div class="caption">photo {{add .Pos 1}} / {{.Total}}</div>
</div>

<div class="timeline">
  {{range .Hours}}<a {{if .Current}}class="cur"{{end}} href="/view?date={{$.DayLink}}&n={{.StartN}}"
     title="{{.Count}} photos">{{.Name}} · {{.Count}}</a>{{end}}
</div>

<div class="nav">
  {{if .HasPrev}}<a id="prev" href="/view?date={{.DayLink}}&n={{.PrevN}}">⏮ Prev</a>{{else}}<button disabled>⏮ Prev</button>{{end}}
  <button onclick="reload()">↻ Latest</button>
  {{if .HasNext}}<a id="next" href="/view?date={{.DayLink}}&n={{.NextN}}">Next ⏭</a>{{else}}<button disabled>Next ⏭</button>{{end}}
</div>

<img id="photo" src="{{.Src}}" alt="capture {{add .Pos 1}}">
<div class="caption">{{.HourLabel}} — {{.Caption}}</div>

<div class="strip">
  {{range .Thumbs}}<a href="/view?date={{$.DayLink}}&n={{.N}}"><img class="{{if .Current}}cur{{end}}" loading="lazy" src="{{.Src}}"></a>{{end}}
</div>

<script>
function reload(){ location.href='/'; }
document.addEventListener('keydown', e => {
  if (e.key==='ArrowLeft' && document.getElementById('prev')) location.href=document.getElementById('prev').href;
  if (e.key==='ArrowRight' && document.getElementById('next')) location.href=document.getElementById('next').href;
});
</script>
</body>
</html>
`

var viewTmpl = template.Must(template.New("view").Funcs(template.FuncMap{
	"human": humanBytes,
	"add":   func(a, b int) int { return a + b },
}).Parse(viewHTML))

// startWeb serves the UI on addr (e.g. ":8080"). Blocks forever.
func startWeb(addr string) {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/latest.jpg", handleLatest)
	http.HandleFunc("/view", handleView)
	http.HandleFunc("/img", handleImg)
	log.Printf("web UI listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("web: %v", err)
	}
}
