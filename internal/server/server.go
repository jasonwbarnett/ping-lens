// Package server exposes the local HTTP surface: /healthz and the
// dashboard pages. All templates are embedded; no external assets.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jasonwbarnett/ping-lens/internal/config"
	"github.com/jasonwbarnett/ping-lens/internal/db"
	"github.com/jasonwbarnett/ping-lens/internal/grade"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Server holds the routes + dependencies.
type Server struct {
	cfg   *config.Config
	store *db.Store
	tpl   *template.Template
}

// New compiles templates and returns a Server.
func New(cfg *config.Config, store *db.Store) (*Server, error) {
	tpl, err := template.New("").Funcs(funcMap()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{cfg: cfg, store: store, tpl: tpl}, nil
}

// Handler returns the configured http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/report", s.handleReport)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

// Serve runs the HTTP server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, listen string) error {
	srv := &http.Server{
		Addr:              listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("http listening on %s", listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"status":     "ok",
		"source":     s.cfg.Source,
		"mode":       s.cfg.Mode,
		"isp":        s.cfg.ISP.Name,
		"targets":    len(s.cfg.Targets),
		"now":        time.Now().UTC().Format(time.RFC3339),
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	winKey, since, until, winLabel := parseWindow(r.URL.Query().Get("window"))

	ctx := r.Context()
	rollups, err := s.store.Rollups(ctx, since, until, "")
	if err != nil {
		httpError(w, "load rollups", err)
		return
	}
	outages, err := s.store.Outages(ctx, since, until, "")
	if err != nil {
		httpError(w, "load outages", err)
		return
	}

	bySource, perTarget := grade.Summarize(rollups, outages)
	sources := sortedSources(bySource)
	mode := s.detectMode(sources)

	base := pageBase{
		Active:        "home",
		WindowKey:     winKey,
		WindowLabel:   winLabel,
		WindowOptions: windowOptions(),
		GeneratedAt:   time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
	}
	if len(sources) == 1 {
		base.ShowReport = true
		base.ReportSource = sources[0].Source
	}

	switch mode {
	case "single_isp":
		s.renderSingle(w, base, sources, rollups, outages, perTarget)
	case "multi_isp":
		s.renderMulti(w, base, sources, rollups, outages, perTarget)
	default:
		// Empty: no data yet.
		base.Title = "ping-lens"
		s.renderEmpty(w, base)
	}
}

func (s *Server) detectMode(sources []*grade.SourceStats) string {
	switch s.cfg.Mode {
	case "single_isp", "multi_isp":
		return s.cfg.Mode
	}
	switch {
	case len(sources) == 0:
		return ""
	case len(sources) == 1:
		return "single_isp"
	default:
		return "multi_isp"
	}
}

func sortedSources(by map[string]*grade.SourceStats) []*grade.SourceStats {
	out := make([]*grade.SourceStats, 0, len(by))
	for _, v := range by {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func (s *Server) renderEmpty(w http.ResponseWriter, base pageBase) {
	page := emptyPage{pageBase: base, Source: s.cfg.Source, ISP: s.cfg.ISP.Name}
	if err := renderWithContent(s.tpl, w, "empty", page); err != nil {
		log.Printf("render empty: %v", err)
	}
}

func (s *Server) renderSingle(w http.ResponseWriter, base pageBase, sources []*grade.SourceStats, rollups []db.RollupRow, outages []db.OutageRow, perTarget []grade.PerTargetStats) {
	stats := sources[0]
	overall := grade.Grade(stats, s.cfg.Thresholds)
	reasons := grade.GradeReasons(stats, s.cfg.Thresholds)
	conf := grade.SingleConfidence(stats)

	sourceOutages := filterOutagesBySource(outages, stats.Source)
	sourcePerTarget := filterPerTargetBySource(perTarget, stats.Source)

	latencyJSON, lossJSON := buildSingleSeriesJSON(rollups, stats.Source)

	base.Title = stats.Source
	page := singlePage{
		pageBase:         base,
		Stats:            stats,
		Overall:          string(overall),
		Reasons:          stringReasons(reasons),
		Confidence:       string(conf),
		PerTarget:        sourcePerTarget,
		Outages:          sourceOutages,
		LatencyChartJSON: template.JS(latencyJSON),
		LossChartJSON:    template.JS(lossJSON),
	}
	_ = renderWithContent(s.tpl, w, "single", page)
}

func (s *Server) renderMulti(w http.ResponseWriter, base pageBase, sources []*grade.SourceStats, rollups []db.RollupRow, _ []db.OutageRow, perTarget []grade.PerTargetStats) {
	winner := pickBestWinner(sources, perTarget)

	latencyJSON, lossJSON := buildMultiSeriesJSON(rollups)
	base.Title = "Compare ISPs"
	page := multiPage{
		pageBase:         base,
		Sources:          dereferenceSources(sources),
		PerTarget:        perTarget,
		Winner:           winner,
		LatencyChartJSON: template.JS(latencyJSON),
		LossChartJSON:    template.JS(lossJSON),
	}
	_ = renderWithContent(s.tpl, w, "multi", page)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("source")
	if src == "" {
		http.Error(w, "source is required", http.StatusBadRequest)
		return
	}
	winKey, since, until, winLabel := parseWindow(r.URL.Query().Get("window"))

	ctx := r.Context()
	rollups, err := s.store.Rollups(ctx, since, until, src)
	if err != nil {
		httpError(w, "load rollups", err)
		return
	}
	outages, err := s.store.Outages(ctx, since, until, src)
	if err != nil {
		httpError(w, "load outages", err)
		return
	}
	bySource, perTarget := grade.Summarize(rollups, outages)
	stats := bySource[src]
	if stats == nil {
		http.Error(w, "no data for source", http.StatusNotFound)
		return
	}
	overall := grade.Grade(stats, s.cfg.Thresholds)
	conf := grade.SingleConfidence(stats)

	base := pageBase{
		Active:        "report",
		Title:         "Evidence report",
		WindowKey:     winKey,
		WindowLabel:   winLabel,
		WindowOptions: windowOptions(),
		GeneratedAt:   time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		ShowReport:    true,
		ReportSource:  src,
	}
	page := reportPage{
		pageBase:   base,
		Stats:      stats,
		Overall:    string(overall),
		Confidence: string(conf),
		PerTarget:  filterPerTargetBySource(perTarget, src),
		Outages:    outages,
		Since:      since,
		Until:      until,
		Conclusion: writeConclusion(stats, overall),
	}
	_ = renderWithContent(s.tpl, w, "report", page)
}

func writeConclusion(s *grade.SourceStats, overall grade.Label) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Across the observed window we recorded %s samples. ", fmtIntStr(s.Samples))
	switch overall {
	case grade.Excellent, grade.Good:
		fmt.Fprintf(&b, "Service was %s with %s packet loss and p95 latency of %.0fms. ", overall, formatLossPct(s.LossPct), s.P95MS)
	default:
		fmt.Fprintf(&b, "Service was %s. ", overall)
	}
	if s.OutageCount > 0 {
		fmt.Fprintf(&b, "We observed %d full outage event(s); the longest lasted %s. ", s.OutageCount, fmtDurStr(s.LongestOutage))
	} else {
		b.WriteString("No full ISP outages were observed. ")
	}
	if s.DNSFailurePct > 0.5 {
		fmt.Fprintf(&b, "DNS failure rate was elevated at %.2f%%. ", s.DNSFailurePct)
	}
	return b.String()
}

func pickBestWinner(sources []*grade.SourceStats, perTarget []grade.PerTargetStats) grade.Winner {
	if len(sources) < 2 {
		return grade.Winner{Tie: true, Confidence: grade.ConfLow}
	}
	// Compare first two sources alphabetically. Future: pairwise matrix.
	return grade.PickWinner(sources[0], sources[1], perTarget)
}

func httpError(w http.ResponseWriter, label string, err error) {
	log.Printf("%s: %v", label, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// ----- chart series ---------------------------------------------------------

type chartData struct {
	Labels   []string         `json:"labels"`
	Datasets []chartDataset   `json:"datasets"`
}

type chartDataset struct {
	Label           string    `json:"label"`
	Data            []float64 `json:"data"`
	BorderColor     string    `json:"borderColor"`
	BackgroundColor string    `json:"backgroundColor"`
	Tension         float64   `json:"tension"`
	PointRadius     float64   `json:"pointRadius"`
}

var palette = []string{"#4ea8de", "#57c98f", "#f4c430", "#e67e22", "#e74c3c", "#9b59b6"}

func buildSingleSeriesJSON(rollups []db.RollupRow, source string) (string, string) {
	type bucketAgg struct {
		samples int
		failures int
		p95Sum float64
	}
	byBucket := map[time.Time]*bucketAgg{}
	var keys []time.Time
	for _, r := range rollups {
		if r.Source != source {
			continue
		}
		b, ok := byBucket[r.Bucket]
		if !ok {
			b = &bucketAgg{}
			byBucket[r.Bucket] = b
			keys = append(keys, r.Bucket)
		}
		b.samples += r.Samples
		b.failures += r.PingFailures
		b.p95Sum += r.P95MS * float64(r.Samples)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	labels := make([]string, len(keys))
	p95 := make([]float64, len(keys))
	loss := make([]float64, len(keys))
	for i, k := range keys {
		labels[i] = k.UTC().Format("01-02 15:04")
		b := byBucket[k]
		if b.samples > 0 {
			p95[i] = round(b.p95Sum/float64(b.samples), 2)
			loss[i] = round(100.0*float64(b.failures)/float64(b.samples), 4)
		}
	}
	latency := chartData{
		Labels: labels,
		Datasets: []chartDataset{{
			Label: "p95 latency (ms)", Data: p95,
			BorderColor: palette[0], BackgroundColor: palette[0], Tension: 0.2, PointRadius: 0,
		}},
	}
	lossChart := chartData{
		Labels: labels,
		Datasets: []chartDataset{{
			Label: "Packet loss (%)", Data: loss,
			BorderColor: palette[4], BackgroundColor: palette[4], Tension: 0.2, PointRadius: 0,
		}},
	}
	return mustJSON(latency), mustJSON(lossChart)
}

func buildMultiSeriesJSON(rollups []db.RollupRow) (string, string) {
	type key struct{ source string; bucket time.Time }
	byKey := map[key]*struct{ samples, failures int; p95Sum float64 }{}
	bucketSet := map[time.Time]struct{}{}
	sourceSet := map[string]struct{}{}
	for _, r := range rollups {
		k := key{source: r.Source, bucket: r.Bucket}
		bucketSet[r.Bucket] = struct{}{}
		sourceSet[r.Source] = struct{}{}
		v, ok := byKey[k]
		if !ok {
			v = &struct{ samples, failures int; p95Sum float64 }{}
			byKey[k] = v
		}
		v.samples += r.Samples
		v.failures += r.PingFailures
		v.p95Sum += r.P95MS * float64(r.Samples)
	}
	buckets := make([]time.Time, 0, len(bucketSet))
	for b := range bucketSet {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Before(buckets[j]) })
	sources := make([]string, 0, len(sourceSet))
	for s := range sourceSet {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	labels := make([]string, len(buckets))
	for i, b := range buckets {
		labels[i] = b.UTC().Format("01-02 15:04")
	}
	latDatasets := make([]chartDataset, 0, len(sources))
	lossDatasets := make([]chartDataset, 0, len(sources))
	for si, src := range sources {
		color := palette[si%len(palette)]
		latData := make([]float64, len(buckets))
		lossData := make([]float64, len(buckets))
		for i, b := range buckets {
			v := byKey[key{source: src, bucket: b}]
			if v != nil && v.samples > 0 {
				latData[i] = round(v.p95Sum/float64(v.samples), 2)
				lossData[i] = round(100.0*float64(v.failures)/float64(v.samples), 4)
			}
		}
		latDatasets = append(latDatasets, chartDataset{
			Label: src + " p95", Data: latData,
			BorderColor: color, BackgroundColor: color, Tension: 0.2, PointRadius: 0,
		})
		lossDatasets = append(lossDatasets, chartDataset{
			Label: src + " loss", Data: lossData,
			BorderColor: color, BackgroundColor: color, Tension: 0.2, PointRadius: 0,
		})
	}
	return mustJSON(chartData{Labels: labels, Datasets: latDatasets}),
		mustJSON(chartData{Labels: labels, Datasets: lossDatasets})
}

func round(v float64, dp int) float64 {
	m := math.Pow(10, float64(dp))
	return math.Round(v*m) / m
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ----- page data ------------------------------------------------------------

type pageBase struct {
	Title         string
	Active        string
	WindowKey     string
	WindowLabel   string
	WindowOptions []windowOpt
	GeneratedAt   string
	ShowReport    bool
	ReportSource  string
}

type windowOpt struct {
	Key   string
	Label string
}

type emptyPage struct {
	pageBase
	Source string
	ISP    string
}

type singlePage struct {
	pageBase
	Stats            *grade.SourceStats
	Overall          string
	Reasons          map[string]string
	Confidence       string
	PerTarget        []grade.PerTargetStats
	Outages          []db.OutageRow
	LatencyChartJSON template.JS
	LossChartJSON    template.JS
}

type multiPage struct {
	pageBase
	Sources          []grade.SourceStats
	PerTarget        []grade.PerTargetStats
	Winner           grade.Winner
	LatencyChartJSON template.JS
	LossChartJSON    template.JS
}

type reportPage struct {
	pageBase
	Stats      *grade.SourceStats
	Overall    string
	Confidence string
	PerTarget  []grade.PerTargetStats
	Outages    []db.OutageRow
	Since      time.Time
	Until      time.Time
	Conclusion string
}

func stringReasons(r map[string]grade.Label) map[string]string {
	out := make(map[string]string, len(r))
	for k, v := range r {
		out[k] = string(v)
	}
	return out
}

func dereferenceSources(in []*grade.SourceStats) []grade.SourceStats {
	out := make([]grade.SourceStats, len(in))
	for i, p := range in {
		out[i] = *p
	}
	return out
}

func filterPerTargetBySource(rows []grade.PerTargetStats, src string) []grade.PerTargetStats {
	out := rows[:0:0]
	for _, r := range rows {
		if r.Source == src {
			out = append(out, r)
		}
	}
	return out
}

func filterOutagesBySource(rows []db.OutageRow, src string) []db.OutageRow {
	out := rows[:0:0]
	for _, r := range rows {
		if r.Source == src {
			out = append(out, r)
		}
	}
	return out
}

// renderWithContent picks one of the *.html template files (which each
// define {{define "content"}}) and layers it under "layout".
func renderWithContent(root *template.Template, w http.ResponseWriter, name string, data any) error {
	// Clone so we don't pollute the original with redefined "content".
	t, err := root.Clone()
	if err != nil {
		return err
	}
	// Re-parse the named content template last so it wins over any other.
	src, err := templatesFS.ReadFile("templates/" + name + ".html")
	if err != nil {
		return err
	}
	if _, err := t.Parse(string(src)); err != nil {
		return err
	}
	return t.ExecuteTemplate(w, "layout", data)
}

// ----- window helpers -------------------------------------------------------

func parseWindow(key string) (string, time.Time, time.Time, string) {
	now := time.Now().UTC()
	switch key {
	case "1h":
		return key, now.Add(-1 * time.Hour), now, "last 1 hour"
	case "24h":
		return key, now.Add(-24 * time.Hour), now, "last 24 hours"
	case "30d":
		return key, now.Add(-30 * 24 * time.Hour), now, "last 30 days"
	case "evening_peak":
		return key, eveningStart(now), eveningEnd(now), "tonight's peak hours"
	case "7d", "":
		return "7d", now.Add(-7 * 24 * time.Hour), now, "last 7 days"
	}
	return "7d", now.Add(-7 * 24 * time.Hour), now, "last 7 days"
}

func windowOptions() []windowOpt {
	return []windowOpt{
		{"1h", "Last 1 hour"},
		{"24h", "Last 24 hours"},
		{"7d", "Last 7 days"},
		{"30d", "Last 30 days"},
		{"evening_peak", "Evening peak (today)"},
	}
}

func eveningStart(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 19, 0, 0, 0, now.Location())
	if now.Before(t) {
		t = t.Add(-24 * time.Hour)
	}
	return t
}
func eveningEnd(now time.Time) time.Time {
	return eveningStart(now).Add(4 * time.Hour)
}

// ----- template funcs -------------------------------------------------------

func funcMap() template.FuncMap {
	return template.FuncMap{
		"fmtInt":        fmtIntStr,
		"fmtPct":        formatLossPct,
		"fmtMS":         fmtMS,
		"fmtDur":        fmtDurStr,
		"fmtOutageDur":  fmtOutageDur,
		"fmtDays":       fmtDays,
		"fmtTime":       func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"fmtTimePtr":    func(t *time.Time) string { if t == nil { return "—" }; return t.UTC().Format("2006-01-02 15:04 UTC") },
		"fmtDate":       func(t time.Time) string { return t.UTC().Format("2006-01-02") },
		"join":          func(s []string, sep string) string { return strings.Join(s, sep) },
		"or":            func(a, b string) string { if a != "" { return a }; return b },
	}
}

func fmtIntStr(n int) string {
	s := fmt.Sprintf("%d", n)
	// thousands separators
	out := []byte{}
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func formatLossPct(v float64) string {
	if v == 0 {
		return "0%"
	}
	if v < 0.01 {
		return fmt.Sprintf("%.4f%%", v)
	}
	return fmt.Sprintf("%.2f%%", v)
}

func fmtMS(v float64) string {
	if v == 0 {
		return "—"
	}
	if v < 10 {
		return fmt.Sprintf("%.1f ms", v)
	}
	return fmt.Sprintf("%.0f ms", v)
}

func fmtDurStr(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	return fmt.Sprintf("%dh %dm", h, m)
}

func fmtOutageDur(secs *int) string {
	if secs == nil {
		return "open"
	}
	return fmtDurStr(time.Duration(*secs) * time.Second)
}

func fmtDays(d float64) string {
	if d == 0 {
		return "—"
	}
	if d < 1 {
		return fmt.Sprintf("%.1fh", d*24)
	}
	return fmt.Sprintf("%.1f d", d)
}

