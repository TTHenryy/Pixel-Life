//go:build windows

// Parent updates: phone notifications through ntfy (https://ntfy.sh).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ntfyServer = "https://ntfy.sh/"

var (
	topicRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`)
	parentMu sync.Mutex // guards parent.json
)

type parentCfg struct {
	On    bool            `json:"on"`
	Kid   string          `json:"kid"`
	Topic string          `json:"topic"`
	Hide  bool            `json:"hide"`
	Ev    map[string]bool `json:"ev"`
	Day   int             `json:"day"`
	Time  string          `json:"time"`
	Since float64         `json:"since"`
}

// Things only the desktop app writes, kept apart from the page's data.
type parentLog struct {
	LastReport string           `json:"lastReport"`
	Sent       map[string]int64 `json:"sent"`
}

func parentOf(st map[string]any) parentCfg {
	var p parentCfg
	if raw, err := json.Marshal(st["parent"]); err == nil {
		json.Unmarshal(raw, &p)
	}
	if p.Ev == nil {
		p.Ev = map[string]bool{}
	}
	return p
}

func (p parentCfg) ready() bool { return p.On && topicRe.MatchString(p.Topic) }

func (p parentCfg) kid() string {
	if strings.TrimSpace(p.Kid) == "" {
		return "Your kid"
	}
	return p.Kid
}

func (p parentCfg) med(name string) string {
	if p.Hide {
		return "a reminder"
	}
	return name
}

func ntfySend(topic, title, msg, tags string) error {
	if !topicRe.MatchString(topic) {
		return fmt.Errorf("bad topic")
	}
	req, err := http.NewRequest("POST", ntfyServer+topic, strings.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	if tags != "" {
		req.Header.Set("Tags", tags)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}

func currentParent() (parentCfg, map[string]any, error) {
	mu.Lock()
	st, err := loadStateLocked()
	mu.Unlock()
	if err != nil {
		return parentCfg{}, nil, err
	}
	return parentOf(st), st, nil
}

// ---------- HTTP endpoints used by the page ----------

func handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	var ev struct{ Title, Msg, Type string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&ev); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	p, _, err := currentParent()
	if err != nil || !p.ready() || !p.Ev[ev.Type] {
		w.Write([]byte(`{"ok":false}`))
		return
	}
	tags := map[string]string{"taken": "white_check_mark", "skipped": "fast_forward", "money": "moneybag", "level": "tada"}[ev.Type]
	go func() {
		if err := ntfySend(p.Topic, clip(ev.Title, 80), clip(ev.Msg, 500), tags); err != nil {
			log.Println("parent event:", err)
		}
	}()
	w.Write([]byte(`{"ok":true}`))
}

func handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	p, _, err := currentParent()
	if err != nil || !p.ready() {
		http.Error(w, "parent updates are off", 400)
		return
	}
	if err := ntfySend(p.Topic, "Pixel Life", "Test from Pixel Life: "+p.kid()+" connected their app. You'll get updates here.", "wave"); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Write([]byte(`{"ok":true}`))
}

func handleReportNow(w http.ResponseWriter, r *http.Request) {
	p, st, err := currentParent()
	if err != nil || !p.ready() {
		http.Error(w, "parent updates are off", 400)
		return
	}
	if err := ntfySend(p.Topic, "Pixel Life weekly report", weeklyReport(st, p, time.Now()), "bar_chart"); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Write([]byte(`{"ok":true}`))
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------- background: missed doses + weekly report ----------

func parentLogPath() string { return filepath.Join(dataDir, "parent.json") }

func loadParentLog() parentLog {
	var l parentLog
	if b, err := os.ReadFile(parentLogPath()); err == nil {
		json.Unmarshal(b, &l)
	}
	if l.Sent == nil {
		l.Sent = map[string]int64{}
	}
	return l
}

func saveParentLog(l parentLog) {
	cut := time.Now().Add(-72 * time.Hour).UnixMilli()
	for k, t := range l.Sent {
		if t < cut {
			delete(l.Sent, k)
		}
	}
	if b, err := json.Marshal(l); err == nil {
		os.WriteFile(parentLogPath(), b, 0o644)
	}
}

func parentLoop() {
	time.Sleep(20 * time.Second)
	for {
		parentTick(time.Now())
		time.Sleep(time.Minute)
	}
}

func parentTick(now time.Time) {
	p, st, err := currentParent()
	if err != nil || !p.ready() {
		return
	}
	parentMu.Lock()
	defer parentMu.Unlock()
	l := loadParentLog()
	changed := false

	// Missed doses (once each).
	if p.Ev["missed"] {
		doses, _ := st["doses"].(map[string]any)
		for _, r := range remindersOf(st) {
			if !r.On {
				continue
			}
			for _, day := range []time.Time{now.AddDate(0, 0, -1), now} {
				for _, o := range occsOnDay(r, day) {
					if _, done := doses[o.key]; done {
						continue
					}
					if float64(o.when.UnixMilli()) < p.Since || now.Before(o.when.Add(grace(r))) || now.Sub(o.when) > 24*time.Hour {
						continue
					}
					if _, sent := l.Sent[o.key]; sent {
						continue
					}
					msg := fmt.Sprintf("%s hasn't checked off %s (due %s).", p.kid(), p.med(r.Name), o.when.Format("3:04 PM"))
					if err := ntfySend(p.Topic, "Pixel Life: missed reminder", msg, "warning"); err != nil {
						log.Println("missed notify:", err)
						continue
					}
					l.Sent[o.key] = now.UnixMilli()
					changed = true
				}
			}
		}
	}

	// Weekly report.
	today := dayKey(now)
	if int(now.Weekday()) == p.Day && now.Hour()*60+now.Minute() >= toMin(p.Time) && l.LastReport != today {
		if err := ntfySend(p.Topic, "Pixel Life weekly report", weeklyReport(st, p, now), "bar_chart"); err == nil {
			l.LastReport = today
			changed = true
		} else {
			log.Println("weekly report:", err)
		}
	}
	if changed {
		saveParentLog(l)
	}
}

// How long after a dose time it counts as missed.
func grace(r reminder) time.Duration {
	if r.Mode == "timer" && r.Every > 0 {
		return time.Duration(r.Every * float64(time.Hour))
	}
	return 2 * time.Hour
}

func remindersOf(st map[string]any) []reminder {
	var rems []reminder
	if raw, err := json.Marshal(st["reminders"]); err == nil {
		json.Unmarshal(raw, &rems)
	}
	return rems
}

// Every scheduled dose of r on the given calendar day.
func occsOnDay(r reminder, day time.Time) []occ {
	loc := day.Location()
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	var out []occ
	if r.Mode == "timer" && r.Anchor > 0 && r.Every > 0 {
		per := math.Round(r.Every * 3600000)
		a := float64(start.UnixMilli())
		b := float64(start.AddDate(0, 0, 1).UnixMilli()) - 1
		k0 := math.Max(1, math.Ceil((a-r.Anchor)/per))
		k1 := math.Floor((b - r.Anchor) / per)
		for k := k0; k <= k1; k++ {
			ts := int64(math.Round(r.Anchor + k*per))
			when := time.UnixMilli(ts).In(loc)
			out = append(out, occ{r, dayKey(when) + "|" + r.ID + "#" + strconv.FormatInt(ts, 10), when})
		}
		return out
	}
	if !hasDay(r.Days, int(start.Weekday())) {
		return nil
	}
	slots := []string{r.Time}
	if r.Every > 0 {
		slots = nil
		s, e := toMin(r.Time), toMin(r.Until)
		if r.Until == "" {
			e = 23*60 + 59
		}
		if e < s {
			e = s
		}
		step := int(math.Round(r.Every * 60))
		if step < 1 {
			step = 60
		}
		for m := s; m <= e; m += step {
			slots = append(slots, fmt.Sprintf("%02d:%02d", m/60, m%60))
		}
	}
	for _, t := range slots {
		at := toMin(t)
		when := time.Date(start.Year(), start.Month(), start.Day(), at/60, at%60, 0, 0, loc)
		if r.Since > float64(when.UnixMilli()) {
			continue
		}
		key := dayKey(start) + "|" + r.ID
		if r.Every > 0 {
			key += "@" + t
		}
		out = append(out, occ{r, key, when})
	}
	return out
}

func money(v float64) string {
	v = math.Round(v*100) / 100
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	if v == math.Trunc(v) {
		return fmt.Sprintf("%s$%.0f", sign, v)
	}
	return fmt.Sprintf("%s$%.2f", sign, v)
}

func weeklyReport(st map[string]any, p parentCfg, now time.Time) string {
	doses, _ := st["doses"].(map[string]any)
	created := num(st["created"])
	var b strings.Builder
	fmt.Fprintf(&b, "%s's week (%s to %s)\n\n", p.kid(), now.AddDate(0, 0, -6).Format("Jan 2"), now.Format("Jan 2"))

	type tally struct{ taken, skipped, missed int }
	totals := tally{}
	var lines []string
	for _, r := range remindersOf(st) {
		if !r.On {
			continue
		}
		t := tally{}
		for i := 6; i >= 0; i-- {
			for _, o := range occsOnDay(r, now.AddDate(0, 0, -i)) {
				if float64(o.when.UnixMilli()) < created || o.when.After(now) {
					continue
				}
				if d, ok := doses[o.key].(map[string]any); ok {
					if d["s"] == "taken" {
						t.taken++
					} else {
						t.skipped++
					}
				} else if now.After(o.when.Add(grace(r))) {
					t.missed++
				}
			}
		}
		if t.taken+t.skipped+t.missed == 0 {
			continue
		}
		totals.taken += t.taken
		totals.skipped += t.skipped
		totals.missed += t.missed
		lines = append(lines, fmt.Sprintf("- %s: %d taken, %d skipped, %d missed", p.med(r.Name), t.taken, t.skipped, t.missed))
	}
	sort.Strings(lines)
	fmt.Fprintf(&b, "Medicine: %d taken, %d skipped, %d missed\n", totals.taken, totals.skipped, totals.missed)
	if !p.Hide {
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}

	// Money over the last 7 days.
	saveAmt := 0.0
	goalName, goalAmt := "goal", 0.0
	if s, ok := st["settings"].(map[string]any); ok {
		saveAmt = num(s["save"])
		if g, ok := s["goalName"].(string); ok && g != "" {
			goalName = g
		}
		goalAmt = num(s["goalAmount"])
	}
	saved, spent, broke := 0.0, 0.0, 0.0
	cut := float64(now.AddDate(0, 0, -7).UnixMilli())
	if logs, ok := st["log"].([]any); ok {
		for _, e := range logs {
			m, ok := e.(map[string]any)
			if !ok || num(m["t"]) < cut {
				continue
			}
			switch m["k"] {
			case "pay":
				saved += saveAmt
			case "bonus", "sweep":
				saved += num(m["a"])
			case "spend":
				spent += num(m["a"])
			case "break":
				broke += num(m["a"])
			}
		}
	}
	vault := num(st["vault"])
	fmt.Fprintf(&b, "\nSavings: %s in the vault (%s saved this week)\n", money(vault), money(saved))
	fmt.Fprintf(&b, "Spent from wallet: %s\n", money(spent))
	if broke > 0 {
		fmt.Fprintf(&b, "Taken out of savings: %s\n", money(broke))
	}
	if goalAmt > 0 {
		fmt.Fprintf(&b, "Goal: %s, %d%% there\n", goalName, int(math.Min(100, vault/goalAmt*100)))
	}
	return b.String()
}
