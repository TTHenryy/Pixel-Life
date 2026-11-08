//go:build windows

// Pixel Life desktop app.
// Updates itself from github.com/TTHenryy/Pixel-Life.
// Serves the Pixel Life page on 127.0.0.1:47821, opens it in its own window,
// keeps the PC awake, starts at sign-in, and sounds reminders natively
// (with a pop-up when the window is closed).
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed index.html
var embeddedPage []byte

// Bump these when publishing an update to the repo's app/version.json.
const (
	embeddedPageVersion = 10
	exeVersion          = 5
	updateBase          = "https://raw.githubusercontent.com/TTHenryy/Pixel-Life/main/app/"
)

var (
	pageMu      sync.RWMutex
	page        []byte
	pageVersion int
	srv         *http.Server
)

const (
	port = "47821"
	addr = "127.0.0.1:" + port
	url  = "http://" + addr + "/"
)

var (
	user32         = syscall.NewLazyDLL("user32.dll")
	procMessageBox = user32.NewProc("MessageBoxW")
	procSysMetrics = user32.NewProc("GetSystemMetrics")
	winmm          = syscall.NewLazyDLL("winmm.dll")
	procPlaySound  = winmm.NewProc("PlaySoundW")
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procSetExec    = kernel32.NewProc("SetThreadExecutionState")
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procRegSetVal  = advapi32.NewProc("RegSetValueExW")
	procRegDelVal  = advapi32.NewProc("RegDeleteValueW")
)

var (
	dataDir   string
	statePath string
	mu        sync.Mutex // guards the state file
	pingMu    sync.Mutex
	lastPing  time.Time
	soundMu   sync.Mutex
	soundBuf  []byte // kept alive while Windows plays it
	boxOpen   bool
	boxMu     sync.Mutex
	lastRing  time.Time
)

func main() {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = os.TempDir()
	}
	dataDir = filepath.Join(appData, "PixelLife")
	os.MkdirAll(dataDir, 0o755)
	statePath = filepath.Join(dataDir, "state.json")
	// Bring over data from the old Diamond Vault app.
	if _, err := os.Stat(statePath); err != nil {
		if b, err := os.ReadFile(filepath.Join(appData, "DiamondVault", "state.json")); err == nil {
			os.WriteFile(statePath, b, 0o644)
		}
	}
	if f, err := os.OpenFile(filepath.Join(dataDir, "log.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(f)
	}

	noWindow, updated := false, false
	for _, a := range os.Args[1:] {
		switch a {
		case "--no-window":
			noWindow = true
		case "--updated":
			updated = true
		}
	}
	loadPage()

	ln, err := net.Listen("tcp", addr)
	if err != nil && !updated && runningIsOlder() {
		stopOldCopies()
		for i := 0; i < 20 && err != nil; i++ {
			time.Sleep(500 * time.Millisecond)
			ln, err = net.Listen("tcp", addr)
		}
	}
	if err != nil && updated {
		// The old version is still shutting down; wait for the port.
		for i := 0; i < 40 && err != nil; i++ {
			time.Sleep(500 * time.Millisecond)
			ln, err = net.Listen("tcp", addr)
		}
	}
	if err != nil {
		// Already running: just show the window.
		openWindow()
		return
	}
	cleanupOldExe()
	oneTimeWipe(appData)

	addToStartup()
	go stayAwake()
	go alarmLoop()
	go updateLoop()
	go parentLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handlePage)
	mux.HandleFunc("/api/state", handleState)
	mux.HandleFunc("/api/test", handleTest)
	mux.HandleFunc("/api/version", handleVersion)
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		w.Write([]byte(`{"ok":true}`))
		go func() { time.Sleep(200 * time.Millisecond); os.Exit(0) }()
	})
	mux.HandleFunc("/api/event", handleEvent)
	mux.HandleFunc("/api/notify-test", handleNotifyTest)
	mux.HandleFunc("/api/report-now", handleReportNow)
	srv = &http.Server{Handler: localOnly(mux)}

	if !noWindow {
		go func() { time.Sleep(400 * time.Millisecond); openWindow() }()
	}
	log.Println("Pixel Life", exeVersion, "running on", url)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	select {} // server closed for a self-update; the updater exits the process

}

// ---------- web server ----------

func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr && r.Host != "localhost:"+port {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	pageMu.RLock()
	b := page
	pageMu.RUnlock()
	w.Write(b)
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	pageMu.RLock()
	v := pageVersion
	pageMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"page":%d,"exe":%d}`, v, exeVersion)
}

func handleState(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("src") != "alarm" {
		pingMu.Lock()
		lastPing = time.Now()
		pingMu.Unlock()
	}
	switch r.Method {
	case http.MethodGet:
		mu.Lock()
		b, err := os.ReadFile(statePath)
		mu.Unlock()
		if err != nil || len(b) == 0 {
			b = []byte("{}")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read error", 400)
			return
		}
		var incoming map[string]any
		if err := json.Unmarshal(body, &incoming); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		// Keep check-offs that arrived after this page last synced.
		if cur, err := loadStateLocked(); err == nil {
			mergeNewerDoses(cur, incoming)
		}
		delete(incoming, "_base")
		if err := saveStateLocked(incoming); err != nil {
			http.Error(w, "save error", 500)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, "method", 405)
	}
}

func handleTest(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("sound")
	vol := 80.0
	mu.Lock()
	if st, err := loadStateLocked(); err == nil {
		vol = alarmVolume(st)
	}
	mu.Unlock()
	playSound(kind, vol)
	w.Write([]byte(`{"ok":true}`))
}

func mergeNewerDoses(cur, incoming map[string]any) {
	inUpdated := num(incoming["_base"])
	if inUpdated == 0 {
		inUpdated = num(incoming["updated"])
	}
	curDoses, _ := cur["doses"].(map[string]any)
	inDoses, ok := incoming["doses"].(map[string]any)
	if !ok {
		inDoses = map[string]any{}
		incoming["doses"] = inDoses
	}
	for k, v := range curDoses {
		if _, has := inDoses[k]; has {
			continue
		}
		if m, ok := v.(map[string]any); ok && num(m["t"]) > inUpdated {
			inDoses[k] = v
		}
	}
}

func loadStateLocked() (map[string]any, error) {
	b, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return st, nil
}

func saveStateLocked(st map[string]any) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath)
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

// ---------- window, startup, stay awake ----------

func browserPath() string {
	candidates := []string{
		filepath.Join(os.Getenv("LOCALAPPDATA"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Google\Chrome\Application\chrome.exe`),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func openWindow() {
	// A separate browser profile makes it a real app window (own taskbar icon,
	// no tabs or address bar) even when the browser is already open.
	if p := browserPath(); p != "" {
		cmd := exec.Command(p, "--app="+url, "--user-data-dir="+filepath.Join(dataDir, "window"),
			"--no-first-run", "--no-default-browser-check", "--window-size=1100,900")
		if err := cmd.Start(); err == nil {
			log.Println("window:", p)
			return
		}
	}
	log.Println("window: no Brave/Edge/Chrome found, using default browser")
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Start()
}

func addToStartup() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	var h syscall.Handle
	path, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Run`)
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, 0x0002 /*KEY_SET_VALUE*/, &h); err != nil {
		log.Println("startup: open key:", err)
		return
	}
	defer syscall.RegCloseKey(h)
	// Remove the old Diamond Vault entry so only one app starts.
	oldName, _ := syscall.UTF16PtrFromString("DiamondVault")
	procRegDelVal.Call(uintptr(h), uintptr(unsafe.Pointer(oldName)))
	name, _ := syscall.UTF16PtrFromString("PixelLife")
	val, _ := syscall.UTF16FromString(`"` + exe + `"`)
	r, _, _ := procRegSetVal.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0, 1 /*REG_SZ*/,
		uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2))
	if r != 0 {
		log.Println("startup: set value failed:", r)
	}
}

// Keeps Windows from going to sleep while the app runs (the screen can still turn off).
func stayAwake() {
	runtime.LockOSThread()
	const esContinuous, esSystemRequired = 0x80000000, 0x00000001
	for {
		procSetExec.Call(uintptr(esContinuous | esSystemRequired))
		time.Sleep(time.Minute)
	}
}

// ---------- reminders ----------

type reminder struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Time   string  `json:"time"`
	Every  float64 `json:"every"`
	Until  string  `json:"until"`
	Days   []int   `json:"days"`
	Sound  string  `json:"sound"`
	Snooze float64 `json:"snooze"`
	Note   string  `json:"note"`
	On     bool    `json:"on"`
	Since  float64 `json:"since"`
	Mode   string  `json:"mode"`
	Anchor float64 `json:"anchor"`
}

type occ struct {
	r    reminder
	key  string
	when time.Time
}

func toMin(hm string) int {
	p := strings.SplitN(hm, ":", 2)
	if len(p) != 2 {
		return 0
	}
	h, _ := strconv.Atoi(p[0])
	m, _ := strconv.Atoi(p[1])
	return h*60 + m
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func hasDay(days []int, d int) bool {
	for _, x := range days {
		if x == d {
			return true
		}
	}
	return false
}

func dueOccs(st map[string]any, now time.Time) []occ {
	var rems []reminder
	if raw, err := json.Marshal(st["reminders"]); err == nil {
		json.Unmarshal(raw, &rems)
	}
	doses, _ := st["doses"].(map[string]any)
	snz, _ := st["snz"].(map[string]any)
	nowMs := float64(now.UnixMilli())
	snoozed := func(k string) bool { return num(snz[k]) > nowMs }
	var out []occ
	for _, r := range rems {
		if !r.On {
			continue
		}
		if r.Mode == "timer" && r.Anchor > 0 && r.Every > 0 {
			per := math.Round(r.Every * 3600000)
			k := math.Floor((nowMs - r.Anchor) / per)
			if k < 1 {
				continue
			}
			ts := int64(math.Round(r.Anchor + k*per))
			when := time.UnixMilli(ts)
			key := dayKey(when) + "|" + r.ID + "#" + strconv.FormatInt(ts, 10)
			if _, done := doses[key]; done || snoozed(key) || nowMs >= float64(ts)+per {
				continue
			}
			out = append(out, occ{r, key, when})
			continue
		}
		if !hasDay(r.Days, int(now.Weekday())) {
			continue
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
		nowMin := now.Hour()*60 + now.Minute()
		for _, t := range slots {
			at := toMin(t)
			if nowMin < at {
				continue
			}
			key := dayKey(now) + "|" + r.ID
			if r.Every > 0 {
				key += "@" + t
			}
			sched := time.Date(now.Year(), now.Month(), now.Day(), at/60, at%60, 0, 0, now.Location())
			if _, done := doses[key]; done || snoozed(key) || r.Since > float64(sched.UnixMilli()) {
				continue
			}
			out = append(out, occ{r, key, sched})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].when.Before(out[j].when) })
	return out
}

func alarmVolume(st map[string]any) float64 {
	if a, ok := st["alarm"].(map[string]any); ok {
		if v, ok := a["volume"].(float64); ok {
			return v
		}
	}
	return 80
}

func alarmRepeat(st map[string]any) time.Duration {
	if a, ok := st["alarm"].(map[string]any); ok {
		if v, ok := a["repeat"].(float64); ok && v >= 5 {
			return time.Duration(v) * time.Second
		}
	}
	return 15 * time.Second
}

func alarmLoop() {
	for {
		time.Sleep(3 * time.Second)
		mu.Lock()
		st, err := loadStateLocked()
		mu.Unlock()
		if err != nil {
			continue
		}
		due := dueOccs(st, time.Now())
		if len(due) == 0 {
			lastRing = time.Time{}
			closePopupWhenDone()
			continue
		}
		popupMu.Lock()
		emptySince = time.Time{}
		popupMu.Unlock()
		if time.Since(lastRing) >= alarmRepeat(st) {
			lastRing = time.Now()
			kind := "chime"
			for _, o := range due {
				if o.r.Sound == "alarm" {
					kind = "alarm"
					break
				}
				if o.r.Sound == "beep" {
					kind = "beep"
				}
			}
			playSound(kind, alarmVolume(st))
		}
		pingMu.Lock()
		windowOpen := time.Since(lastPing) < 15*time.Second
		pingMu.Unlock()
		if windowOpen {
			continue // the main window shows its own alarm screen
		}
		if launchPopupIfNeeded() {
			continue
		}
		// No Brave/Edge/Chrome: fall back to a plain Windows message box.
		boxMu.Lock()
		showBox := !boxOpen
		if showBox {
			boxOpen = true
		}
		boxMu.Unlock()
		if showBox {
			go askUser(due)
		}
	}
}

func askUser(due []occ) {
	defer func() { boxMu.Lock(); boxOpen = false; boxMu.Unlock() }()
	snoozeMin := 10.0
	text := "Time to take:\n"
	for _, o := range due {
		text += fmt.Sprintf("  - %s (%s)\n", o.r.Name, o.when.Format("3:04 PM"))
		if o.r.Snooze >= 1 && o.r.Snooze < snoozeMin {
			snoozeMin = o.r.Snooze
		}
	}
	all := ""
	if len(due) > 1 {
		all = " all"
	}
	text += fmt.Sprintf("\nYes = Taken%s\nNo = Snooze%s %g min\nCancel = Skip%s", all, all, snoozeMin, all)
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString("Pixel Life reminder")
	const flags = 0x3 | 0x30 | 0x1000 | 0x10000 | 0x40000 // YESNOCANCEL, WARNING, SYSTEMMODAL, SETFOREGROUND, TOPMOST
	ret, _, _ := procMessageBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)

	mu.Lock()
	defer mu.Unlock()
	st, err := loadStateLocked()
	if err != nil {
		return
	}
	doses, ok := st["doses"].(map[string]any)
	if !ok {
		doses = map[string]any{}
		st["doses"] = doses
	}
	now := float64(time.Now().UnixMilli())
	snz, ok := st["snz"].(map[string]any)
	if !ok {
		snz = map[string]any{}
		st["snz"] = snz
	}
	var names []string
	for _, o := range due {
		if _, done := doses[o.key]; done {
			continue // already handled in the window
		}
		names = append(names, o.r.Name)
		switch ret {
		case 6: // Yes
			doses[o.key] = map[string]any{"s": "taken", "t": now}
		case 2: // Cancel
			doses[o.key] = map[string]any{"s": "skipped", "t": now}
		default: // No = snooze
			sm := o.r.Snooze
			if sm < 1 {
				sm = 10
			}
			snz[o.key] = now + sm*60000
		}
	}
	st["updated"] = now
	saveStateLocked(st)
	if ret == 6 || ret == 2 {
		lastRing = time.Time{}
		if p := parentOf(st); p.ready() {
			ev, verb, tag := "taken", "took", "white_check_mark"
			if ret == 2 {
				ev, verb, tag = "skipped", "skipped", "fast_forward"
			}
			if p.Ev[ev] && len(names) > 0 {
				shown := names
				if p.Hide {
					shown = []string{fmt.Sprintf("%d reminder(s)", len(names))}
				}
				msg := fmt.Sprintf("%s %s %s at %s.", p.kid(), verb, strings.Join(shown, ", "), time.Now().Format("3:04 PM"))
				go ntfySend(p.Topic, "Pixel Life", msg, tag)
			}
		}
	}
}

// ---------- sound ----------

func playSound(kind string, volume float64) {
	wav := makeWav(kind, volume/100)
	soundMu.Lock()
	soundBuf = wav
	soundMu.Unlock()
	const sndAsync, sndNoDefault, sndMemory = 0x0001, 0x0002, 0x0004
	procPlaySound.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndAsync|sndNoDefault|sndMemory)
}

func makeWav(kind string, vol float64) []byte {
	const rate = 22050
	if vol < 0 {
		vol = 0
	}
	if vol > 1 {
		vol = 1
	}
	type note struct {
		f, start, dur float64
		wave          string
	}
	var notes []note
	switch kind {
	case "beep":
		for i := 0; i < 3; i++ {
			notes = append(notes, note{880, float64(i) * 0.22, 0.15, "square"})
		}
	case "alarm":
		for i := 0; i < 8; i++ {
			f := 990.0
			if i%2 == 1 {
				f = 660
			}
			notes = append(notes, note{f, float64(i) * 0.22, 0.2, "saw"})
		}
	default:
		for i, f := range []float64{523.25, 659.25, 783.99, 1046.5} {
			notes = append(notes, note{f, float64(i) * 0.12, 0.5, "sine"})
		}
	}
	total := 0.0
	for _, n := range notes {
		if e := n.start + n.dur; e > total {
			total = e
		}
	}
	samples := make([]float64, int((total+0.05)*rate))
	for _, n := range notes {
		s0 := int(n.start * rate)
		for i := 0; i < int(n.dur*rate); i++ {
			t := float64(i) / rate
			ph := math.Mod(n.f*t, 1)
			var v float64
			switch n.wave {
			case "square":
				if ph < 0.5 {
					v = 0.5
				} else {
					v = -0.5
				}
			case "saw":
				v = (2*ph - 1) * 0.5
			default:
				v = math.Sin(2 * math.Pi * ph)
			}
			env := math.Min(1, t/0.01) * math.Exp(-3*t/n.dur)
			if s0+i < len(samples) {
				samples[s0+i] += v * env
			}
		}
	}
	data := new(bytes.Buffer)
	for _, s := range samples {
		x := s * vol * 0.8
		if x > 1 {
			x = 1
		} else if x < -1 {
			x = -1
		}
		binary.Write(data, binary.LittleEndian, int16(x*32767))
	}
	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+data.Len()))
	buf.WriteString("WAVEfmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(buf, binary.LittleEndian, uint32(rate))
	binary.Write(buf, binary.LittleEndian, uint32(rate*2))
	binary.Write(buf, binary.LittleEndian, uint16(2))
	binary.Write(buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(data.Len()))
	buf.Write(data.Bytes())
	return buf.Bytes()
}

// ---------- updates ----------

type versionInfo struct {
	Page      int    `json:"page"`
	Exe       int    `json:"exe"`
	ExeSha256 string `json:"exeSha256"`
}

func loadPage() {
	page, pageVersion = embeddedPage, embeddedPageVersion
	b, err := os.ReadFile(filepath.Join(dataDir, "page.html"))
	if err != nil {
		return
	}
	vb, err := os.ReadFile(filepath.Join(dataDir, "page.version"))
	if err != nil {
		return
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(vb)))
	if err == nil && v > embeddedPageVersion && looksLikePage(b) {
		page, pageVersion = b, v
	}
}

func looksLikePage(b []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(b), []byte("<!DOCTYPE html")) && bytes.Contains(b, []byte("</html>"))
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

func fetch(name string, limit int64) ([]byte, error) {
	resp, err := httpClient.Get(updateBase + name + "?t=" + strconv.FormatInt(time.Now().Unix(), 10))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func updateLoop() {
	time.Sleep(5 * time.Second)
	for {
		checkForUpdates()
		time.Sleep(3 * time.Hour)
	}
}

func checkForUpdates() {
	b, err := fetch("version.json", 64<<10)
	if err != nil {
		log.Println("update check:", err)
		return
	}
	var v versionInfo
	if err := json.Unmarshal(b, &v); err != nil {
		log.Println("update check: bad version.json:", err)
		return
	}
	pageMu.RLock()
	cur := pageVersion
	pageMu.RUnlock()
	if v.Page > cur {
		if html, err := fetch("index.html", 8<<20); err == nil && looksLikePage(html) {
			os.WriteFile(filepath.Join(dataDir, "page.html"), html, 0o644)
			os.WriteFile(filepath.Join(dataDir, "page.version"), []byte(strconv.Itoa(v.Page)), 0o644)
			pageMu.Lock()
			page, pageVersion = html, v.Page
			pageMu.Unlock()
			log.Println("page updated to", v.Page)
		} else {
			log.Println("page update failed:", err)
		}
	}
	if v.Exe > exeVersion && len(v.ExeSha256) == 64 {
		if err := selfUpdate(v.ExeSha256); err != nil {
			log.Println("exe update failed:", err)
		}
	}
}

func selfUpdate(wantSha string) error {
	bin, err := fetch("PixelLife.exe", 64<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), wantSha) {
		return fmt.Errorf("checksum mismatch")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	newPath, oldPath := exe+".new", exe+".old"
	if err := os.WriteFile(newPath, bin, 0o755); err != nil {
		return err
	}
	os.Remove(oldPath)
	if err := os.Rename(exe, oldPath); err != nil {
		os.Remove(newPath)
		return err
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(oldPath, exe)
		return err
	}
	if _, err := os.Stat(exe); err != nil {
		// The new file was removed (for example by antivirus): restore the old one.
		os.Rename(oldPath, exe)
		return fmt.Errorf("new version missing after install")
	}
	log.Println("installed new exe, restarting")
	if srv != nil {
		srv.Close()
	}
	if err := exec.Command(exe, "--updated", "--no-window").Start(); err != nil {
		os.Remove(exe)
		os.Rename(oldPath, exe)
		log.Println("new version failed to start, restored the old one:", err)
		exec.Command(exe, "--updated", "--no-window").Start()
		os.Exit(0)
	}
	os.Exit(0)
	return nil
}

func cleanupOldExe() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	go func() {
		for i := 0; i < 20; i++ {
			if err := os.Remove(exe + ".old"); err == nil || os.IsNotExist(err) {
				return
			}
			time.Sleep(time.Second)
		}
	}()
}

// ---------- themed alarm pop-up window ----------

var (
	popupMu     sync.Mutex
	popupCmd    *exec.Cmd
	popupClosed time.Time
	emptySince  time.Time
)

// Opens a small Pixel Life alarm window showing everything that's due.
// Returns false only when no suitable browser exists.
func launchPopupIfNeeded() bool {
	p := browserPath()
	if p == "" {
		return false
	}
	popupMu.Lock()
	defer popupMu.Unlock()
	if popupCmd != nil {
		return true
	}
	if time.Since(popupClosed) < 60*time.Second {
		return true // closed with the X; open it again in a minute if still due
	}
	w, h := 560, 700
	sw, _, _ := procSysMetrics.Call(0) // SM_CXSCREEN
	sh, _, _ := procSysMetrics.Call(1) // SM_CYSCREEN
	x, y := (int(sw)-w)/2, (int(sh)-h)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	cmd := exec.Command(p, "--app="+url+"#alarm", "--user-data-dir="+filepath.Join(dataDir, "alarm"),
		"--no-first-run", "--no-default-browser-check",
		fmt.Sprintf("--window-size=%d,%d", w, h), fmt.Sprintf("--window-position=%d,%d", x, y))
	if err := cmd.Start(); err != nil {
		log.Println("popup:", err)
		return false
	}
	popupCmd = cmd
	go func(c *exec.Cmd) {
		c.Wait()
		popupMu.Lock()
		if popupCmd == c {
			popupCmd = nil
			popupClosed = time.Now()
		}
		popupMu.Unlock()
	}(cmd)
	return true
}

// Closes the pop-up a few seconds after everything is handled,
// so "All done!" shows briefly.
func closePopupWhenDone() {
	popupMu.Lock()
	defer popupMu.Unlock()
	popupClosed = time.Time{}
	if popupCmd == nil {
		emptySince = time.Time{}
		return
	}
	if emptySince.IsZero() {
		emptySince = time.Now()
		return
	}
	if time.Since(emptySince) > 3*time.Second {
		popupCmd.Process.Kill()
		popupCmd = nil
		emptySince = time.Time{}
	}
}

// ---------- replacing old copies ----------

// True when the copy already running is Diamond Vault or an older Pixel Life.
func runningIsOlder() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url + "api/version")
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	var v struct{ Exe int }
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&v) != nil {
		return true // Diamond Vault has no version endpoint
	}
	return v.Exe < exeVersion
}

func stopOldCopies() {
	c := &http.Client{Timeout: 2 * time.Second}
	if resp, err := c.Post(url+"api/quit", "application/json", nil); err == nil {
		resp.Body.Close()
	}
	log.Println("asked the older copy to quit")
}

// Wipes all saved data once, so this version starts completely fresh.
const wipeID = "fresh-start-2026-11-07"

func oneTimeWipe(appData string) {
	marker := filepath.Join(dataDir, "wiped.txt")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == wipeID {
		return
	}
	for _, f := range []string{statePath, filepath.Join(dataDir, "parent.json"), filepath.Join(dataDir, "page.html"), filepath.Join(dataDir, "page.version")} {
		os.Remove(f)
	}
	os.RemoveAll(filepath.Join(appData, "DiamondVault"))
	os.WriteFile(marker, []byte(wipeID), 0o644)
	log.Println("wiped saved data for a fresh start")
}
