package main

// Simple read-only web UI, all on one page (/):
//   photo viewer (prev/next, hour timeline, filmstrip) +
//   stats (totals, data size, disk free) + day/hour summaries.
// Query params swap the displayed photo in place: /?date=YYYY-MM-DD&n=IDX
// No params = newest photo of the newest day (the live view).
// Serve with startWeb(":8080") — see compose.yaml.

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

// ---------- data model ----------

type hourStat struct {
	Name  string
	Link  string // hour value for viewer URLs, e.g. "1pm" (empty = not browsable)
	Count int
	Bytes int64
}

type dayStat struct {
	Date  string // display date, e.g. "2026 November 10"
	Link  string // yyyy-mm-dd date value for viewer URLs
	Count int
	Bytes int64
	Hours []hourStat
	Open  bool
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

// pageData drives the single combined page.
type pageData struct {
	// viewer
	DayLink   string
	DayLabel  string
	HourLabel string
	Pos       int
	Total     int
	Src       string
	Caption   string
	HasPrev   bool
	HasNext   bool
	PrevURL   string
	NextURL   string
	LiveURL   string
	Hours     []hourLink
	Thumbs    []thumb
	// stats
	Now            string
	TotalCount     int
	TotalDataBytes int64
	FreeBytes      int64
	TotalDiskBytes int64
	Days           []dayStat
}

// ---------- filesystem helpers ----------

var imgNameRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2}(-\d+)?\.jpeg$`)

type photoRef struct {
	hour string
	name string
}

// dayDirPath returns the filesystem dir for a yyyy-mm-dd date, or "" if invalid.
func dayDirPath(dateStr string) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return ""
	}
	return filepath.Join(dataDir, t.Format("2006"), t.Format("January"), t.Format("02"))
}

// listDays returns every day that has photos, as yyyy-mm-dd, oldest first.
func listDays() []string {
	var days []string
	years, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}
	for _, y := range years {
		if !y.IsDir() {
			continue
		}
		months, err := os.ReadDir(filepath.Join(dataDir, y.Name()))
		if err != nil {
			continue
		}
		for _, m := range months {
			if !m.IsDir() {
				continue
			}
			days2, err := os.ReadDir(filepath.Join(dataDir, y.Name(), m.Name()))
			if err != nil {
				continue
			}
			for _, d := range days2 {
				if !d.IsDir() {
					continue
				}
				mt, err := time.Parse("January", m.Name())
				if err != nil {
					mt, err = time.Parse("01", m.Name())
				}
				if err != nil {
					continue
				}
				days = append(days, fmt.Sprintf("%s-%02d-%s", y.Name(), int(mt.Month()), d.Name()))
			}
		}
	}
	sort.Strings(days)
	return days
}

// flattenDay lists every photo of a day in chronological order (hour buckets
// sorted by their 3pm name, filenames within, legacy root files last).
// Also returns the hours (with photos) in the same order.
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
			if _, err := time.Parse("3pm", e.Name()); err == nil {
				hourDirs = append(hourDirs, e.Name())
			}
		}
	}
	sort.Slice(hourDirs, func(i, j int) bool {
		ti, _ := time.Parse("3pm", hourDirs[i])
		tj, _ := time.Parse("3pm", hourDirs[j])
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

// ---------- summary aggregation ----------

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
	latest := time.Time{}

	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
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
			mt, err := time.Parse("January", parts[1])
			if err != nil {
				mt, err = time.Parse("01", parts[1])
			}
			if err == nil {
				ag.day.Link = fmt.Sprintf("%s-%02d-%s", parts[0], int(mt.Month()), parts[2])
			}
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
				hs.Link = hour
			}
			ag.hours[hour] = hs
		}
		hs.Count++
		hs.Bytes += info.Size()
		pd.TotalCount++
		pd.TotalDataBytes += info.Size()
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, ag := range aggs {
		for _, hs := range ag.hours {
			ag.day.Hours = append(ag.day.Hours, *hs)
		}
		sort.Slice(ag.day.Hours, func(i, j int) bool {
			ti, ei := time.Parse("3pm", ag.day.Hours[i].Name)
			tj, ej := time.Parse("3pm", ag.day.Hours[j].Name)
			if ei == ej {
				return ag.day.Hours[i].Name < ag.day.Hours[j].Name
			}
			return ti.Before(tj)
		})
		pd.Days = append(pd.Days, ag.day)
	}
	sort.Slice(pd.Days, func(i, j int) bool {
		return pd.Days[i].Date > pd.Days[j].Date
	})
	if len(pd.Days) > 14 {
		pd.Days = pd.Days[:14]
	}

	if st, err := diskUsage(dataDir); err == nil {
		pd.FreeBytes = st.avail
		pd.TotalDiskBytes = st.total
	}
	return pd, nil
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

// ---------- handlers ----------

// handleIndex serves the combined UI. Query params:
//
//	date=YYYY-MM-DD   day to browse (default: newest day with photos)
//	n=IDX             photo index within the day (default: newest photo)
//	hour=1pm          jump to the first photo of that hour
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	q := r.URL.Query()

	days := listDays()
	if len(days) == 0 {
		stats, err := buildSummary()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := indexTmpl.Execute(w, stats); err != nil {
			log.Printf("index: %v", err)
		}
		return
	}

	dateStr := q.Get("date")
	if dateStr == "" {
		dateStr = days[len(days)-1] // newest day
	} else if !containsDay(days, dateStr) {
		http.Error(w, "bad date", http.StatusBadRequest)
		return
	}
	photos, dayHours := flattenDay(dateStr)
	if len(photos) == 0 {
		http.Error(w, "no photos that day", http.StatusNotFound)
		return
	}

	pos := -1
	if n, err := strconv.Atoi(q.Get("n")); err == nil {
		pos = n
	} else if h := q.Get("hour"); h != "" {
		for i, p := range photos { // jump to first photo of that hour
			if p.hour == h {
				pos = i
				break
			}
		}
	}
	if pos < 0 {
		pos = len(photos) - 1 // newest photo of the day (live view)
	}
	if pos < 0 {
		pos = 0
	}
	if pos >= len(photos) {
		pos = len(photos) - 1
	}
	cur := photos[pos]

	pd := pageData{
		DayLink:   dateStr,
		HourLabel: cur.hour,
		Pos:       pos,
		Total:     len(photos),
		Src:       imgSrc(dateStr, cur.hour, cur.name),
		Caption:   photoCaption(cur.name),
		HasPrev:   true,
		HasNext:   true,
		LiveURL:   "/",
	}

	// prev/next, flowing continuously across days
	prevDay, nextDay := neighborDays(days, dateStr)
	switch {
	case pos > 0:
		pd.PrevURL = fmt.Sprintf("/?date=%s&n=%d", dateStr, pos-1)
	case prevDay != "":
		prevPhotos, _ := flattenDay(prevDay)
		pd.PrevURL = fmt.Sprintf("/?date=%s&n=%d", prevDay, len(prevPhotos)-1)
	default:
		pd.HasPrev = false
	}
	switch {
	case pos < len(photos)-1:
		pd.NextURL = fmt.Sprintf("/?date=%s&n=%d", dateStr, pos+1)
	case nextDay != "":
		pd.NextURL = fmt.Sprintf("/?date=%s&n=0", nextDay)
	default:
		pd.HasNext = false
	}

	// hour timeline for the current day
	for _, h := range dayHours {
		start, count := -1, 0
		for i, p := range photos {
			if p.hour == h {
				if start == -1 {
					start = i
				}
				count++
			}
		}
		pd.Hours = append(pd.Hours, hourLink{Name: h, StartN: start, Count: count, Current: h == cur.hour})
	}
	// filmstrip of the current hour
	for i, p := range photos {
		if p.hour == cur.hour {
			pd.Thumbs = append(pd.Thumbs, thumb{N: i, Src: imgSrc(dateStr, p.hour, p.name), Current: i == pos})
		}
	}

	if t, err := time.Parse("2006-01-02", dateStr); err == nil {
		pd.DayLabel = t.In(loc).Format("Monday, January 2, 2006")
	}

	stats, err := buildSummary()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pd.TotalCount = stats.TotalCount
	pd.TotalDataBytes = stats.TotalDataBytes
	pd.FreeBytes = stats.FreeBytes
	pd.TotalDiskBytes = stats.TotalDiskBytes
	pd.Days = stats.Days
	pd.Now = time.Now().In(loc).Format("2006-01-02 15:04:05 MST")

	if err := indexTmpl.Execute(w, pd); err != nil {
		log.Printf("index: %v", err)
	}
}

func containsDay(days []string, date string) bool {
	for _, d := range days {
		if d == date {
			return true
		}
	}
	return false
}

// neighborDays returns the previous and next day around date in the day list.
func neighborDays(days []string, date string) (prev, next string) {
	for i, d := range days {
		if d == date {
			if i > 0 {
				prev = days[i-1]
			}
			if i < len(days)-1 {
				next = days[i+1]
			}
			return prev, next
		}
	}
	return "", ""
}

// handleView redirects legacy /view links into the combined page.
func handleView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	http.Redirect(w, r, "/?date="+q.Get("date")+"&n="+q.Get("n")+"&hour="+q.Get("hour"), http.StatusFound)
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

// handleLatest serves the newest photo by path (newest day, last file).
func handleLatest(w http.ResponseWriter, r *http.Request) {
	days := listDays()
	for i := len(days) - 1; i >= 0; i-- {
		photos, _ := flattenDay(days[i])
		if len(photos) > 0 {
			last := photos[len(photos)-1]
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(dayDirPath(days[i]), last.hour, last.name))
			return
		}
	}
	http.Error(w, "no photos yet", http.StatusNotFound)
}

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

// ---------- templates ----------

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="60">
<title>webcam-capture</title>
<style>
  :root { color-scheme: dark; }
  body { background: #111; color: #ddd; font-family: system-ui, sans-serif;
         max-width: 1100px; margin: 0 auto; padding: 1rem; }
  a { color: #7fb4ff; text-decoration: none; }
  h1 { font-size: 1.2rem; margin: 0; color: #7fb4ff; }
  .topbar { display: flex; align-items: baseline; gap: 1rem; flex-wrap: wrap; }
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
  .cards { display: flex; gap: 1rem; flex-wrap: wrap; margin: .75rem 0; }
  .card { background: #1c1c1c; border: 1px solid #333; border-radius: 8px;
          padding: .75rem 1rem; }
  .card b { color: #fff; }
  h2 { color: #7fb4ff; }
  details { background: #1c1c1c; border: 1px solid #333; border-radius: 8px;
            padding: .5rem 1rem; margin: .5rem 0; }
  summary { cursor: pointer; font-weight: 600; }
  .hours { columns: 2; list-style: none; padding-left: 0; }
  .hours li { padding: .1rem 0; }
  footer { color: #666; font-size: .85rem; }
</style>
</head>
<body>
<div class="topbar">
  <h1>webcam-capture</h1>
  <h1>{{if .DayLabel}}{{.DayLabel}}{{else}}Latest capture{{end}}</h1>
  <div class="caption">photo {{inc .Pos}} / {{.Total}}</div>
</div>

{{if .Hours}}
<div class="timeline">
  {{range .Hours}}<a {{if .Current}}class="cur"{{end}} href="/?date={{$.DayLink}}&n={{.StartN}}"
     title="{{.Count}} photos">{{.Name}} · {{.Count}}</a>{{end}}
</div>

<div class="nav">
  {{if .HasPrev}}<a id="prev" href="{{.PrevURL}}">⏮ Prev</a>{{else}}<button disabled>⏮ Prev</button>{{end}}
  <a href="{{.LiveURL}}">↻ Latest</a>
  {{if .HasNext}}<a id="next" href="{{.NextURL}}">Next ⏭</a>{{else}}<button disabled>Next ⏭</button>{{end}}
</div>

<img id="photo" src="{{.Src}}" alt="capture {{inc .Pos}}">
<div class="caption">{{.HourLabel}} — {{.Caption}}</div>

<div class="strip">
  {{range .Thumbs}}<a href="/?date={{$.DayLink}}&n={{.N}}"><img class="{{if .Current}}cur{{end}}" loading="lazy" src="{{.Src}}"></a>{{end}}
</div>
{{else}}<p>No photos yet.</p>{{end}}

<div class="cards">
  <div class="card">Photos: <b>{{.TotalCount}}</b></div>
  <div class="card">Data size: <b>{{.TotalDataBytes | human}}</b></div>
  <div class="card">Disk free: <b>{{.FreeBytes | human}}</b> of {{.TotalDiskBytes | human}}</div>
</div>

<h2>Captured</h2>
{{if .Days}}{{range .Days}}{{$dl := .Link}}<details{{if .Open}} open{{end}}>
  <summary><a href="/?date={{$dl}}&n=0">{{.Date}}</a> — {{.Count}} photos ({{.Bytes | human}})</summary>
  <ul class="hours">{{range .Hours}}{{if .Link}}<li><a href="/?date={{$dl}}&hour={{.Link}}">{{.Name}}</a> — {{.Count}} photos ({{.Bytes | human}})</li>{{else}}<li>{{.Name}} — {{.Count}} photos ({{.Bytes | human}})</li>{{end}}{{end}}</ul>
</details>{{end}}{{else}}<p>No photos yet.</p>{{end}}

<footer>refreshes every 60s when idle · {{.Now}} · webcam-capture web UI</footer>

<script>
document.addEventListener('keydown', e => {
  if (e.key==='ArrowLeft' && document.getElementById('prev')) location.href=document.getElementById('prev').href;
  if (e.key==='ArrowRight' && document.getElementById('next')) location.href=document.getElementById('next').href;
});
</script>
</body>
</html>
`

var indexTmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"human": humanBytes,
	"inc":   func(i int) int { return i + 1 },
}).Parse(indexHTML))
