// m3u8tostrm converts m3u/m3u_plus playlists into .strm files, keeps them in
// sync, purges stale ones, and can run as a system service on any OS.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kardianos/service"
)

type Config struct {
	Playlists  []string `json:"playlists"`   // URLs or file paths
	Output     string   `json:"output"`      // root folder for .strm files
	Interval   string   `json:"interval"`    // time between syncs, e.g. "6h"
	StaleAfter string   `json:"stale_after"` // delete files missing from the playlist this long, "0s" = immediately
	Workers    int      `json:"workers"`     // parallel file writers
	LogFile    string   `json:"log_file"`    // optional; services on Windows have no console
	NewGroups  string   `json:"new_groups"`  // "ignore" or "subscribe": what a sync does with groups it has not seen before
	// Titles containing any of these words (case-insensitive) are never synced, whatever group they are in.
	ExcludeTitles []string `json:"exclude_titles"`
	Types         []string `json:"types"` // which of "movies", "series", "live" to sync at all

	interval, staleAfter time.Duration
	groupsPath, path     string // groups.json next to the config, and the config itself
}

// excluded reports whether a title matches one of the exclude_titles words.
func (c *Config) excluded(name string) bool {
	name = strings.ToLower(name)
	for _, w := range c.ExcludeTitles {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" && strings.Contains(name, w) {
			return true
		}
	}
	return false
}

type entry struct{ name, group, url string }

type rec struct {
	T int64  `json:"t"` // last seen, unix seconds
	H uint64 `json:"h"` // FNV-1a hash of the URL written
}

const stateName = ".m3u8tostrm-state.json"

func defaults() *Config {
	return &Config{Interval: "6h", StaleAfter: "72h", Workers: 16, NewGroups: "subscribe", Types: []string{"movies", "series"}}
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := defaults()
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Playlists) == 0 || c.Output == "" {
		return nil, fmt.Errorf("%s: playlists and output are required", path)
	}
	if c.interval, err = time.ParseDuration(c.Interval); err != nil || c.interval < time.Minute {
		return nil, fmt.Errorf("interval: must be a duration of at least 1m, e.g. 6h")
	}
	if c.staleAfter, err = time.ParseDuration(c.StaleAfter); err != nil {
		return nil, fmt.Errorf("stale_after: %w", err)
	}
	if c.NewGroups != "ignore" && c.NewGroups != "subscribe" {
		return nil, fmt.Errorf("new_groups must be \"ignore\" or \"subscribe\"")
	}
	for _, t := range c.Types {
		if t != "movies" && t != "series" && t != "live" {
			return nil, fmt.Errorf("types: %q is not one of movies, series, live", t)
		}
	}
	if c.Workers < 1 {
		c.Workers = 1
	}
	// Services start in a system dir, so resolve relative paths against the config file.
	dir := filepath.Dir(path)
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) || strings.Contains(p, "://") {
			return p
		}
		return filepath.Join(dir, p)
	}
	c.Output, c.LogFile, c.groupsPath, c.path = abs(c.Output), abs(c.LogFile), filepath.Join(dir, "groups.json"), path
	for i, p := range c.Playlists {
		c.Playlists[i] = abs(p)
	}
	return c, nil
}

// parse streams a playlist and calls fn for every #EXTINF + URL pair.
func parse(r io.Reader, fn func(entry)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var e entry
	have := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF"):
			e, have = entry{name: extinfName(line), group: attr(line, "group-title")}, true
			if e.name == "" {
				e.name = attr(line, "tvg-name")
			}
		case line == "" || line[0] == '#':
		case have:
			e.url, have = line, false
			fn(e)
		}
	}
	return sc.Err()
}

// extinfName returns the title after the first comma that is not inside quotes.
func extinfName(line string) string {
	q := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			q = !q
		case ',':
			if !q {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

func attr(line, key string) string {
	i := strings.Index(line, " "+key+`="`)
	if i < 0 {
		return ""
	}
	v := line[i+len(key)+3:]
	if j := strings.IndexByte(v, '"'); j >= 0 {
		return v[:j]
	}
	return ""
}

var episodeRe = regexp.MustCompile(`(?i)^(.*?)[\s._-]*S(\d{1,3})[\s._-]*E\d{1,4}`)

func kindOf(url string) string {
	switch {
	case strings.Contains(url, "/movie/"):
		return "movies"
	case strings.Contains(url, "/series/"):
		return "series"
	}
	return "live"
}

// relPath decides where an entry lives; groups are not folders.
// Xtream-style URLs (/movie/, /series/) are split into Movies and Series so media servers
// can scan them separately. Each movie gets its own folder, so versions labelled
// "Title - Group" next to it show up in Emby/Jellyfin/Plex as one item with a version picker.
func relPath(e entry) string {
	name := clean(e.name) + ".strm"
	switch kindOf(e.url) {
	case "movies":
		return filepath.Join("Movies", clean(e.name), name)
	case "series":
		if m := episodeRe.FindStringSubmatch(e.name); m != nil && strings.TrimSpace(m[1]) != "" {
			season, _ := strconv.Atoi(m[2])
			return filepath.Join("Series", clean(m[1]), fmt.Sprintf("Season %02d", season), name)
		}
		return filepath.Join("Series", name)
	default:
		return filepath.Join("Live", name)
	}
}

var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "LPT1": true, "LPT2": true, "LPT3": true}

// clean makes a string safe as a file name on every OS.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	s = strings.Trim(s, " .")
	if s == "" {
		return "_"
	}
	if reserved[strings.ToUpper(s)] {
		return "_" + s
	}
	return s
}

// redact hides credentials: playlist URLs usually carry the provider username and password.
func redact(src string) string {
	if u, err := url.Parse(src); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/..."
	}
	return src
}

func open(src string) (io.ReadCloser, error) {
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		return os.Open(src)
	}
	req, err := http.NewRequest("GET", src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "VLC/3.0.20 LibVLC/3.0.20") // many IPTV panels reject unknown clients
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if ue, ok := err.(*url.Error); ok {
		ue.URL = redact(src) // Go puts the full URL, password included, in the error
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return resp.Body, nil
}

// sync does one full pass: write new/changed files, then purge stale ones.
func syncOnce(c *Config) error {
	start := time.Now()
	statePath := filepath.Join(c.Output, stateName)
	state := map[string]rec{} // relative path -> last seen + URL hash
	if b, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(b, &state); err != nil {
			return fmt.Errorf("corrupt state file %s: %w", statePath, err)
		}
	}

	type job struct {
		rel, url string
		known    bool // state says this exact URL was already written here
	}
	jobs := make(chan job, 4096)
	var wg sync.WaitGroup
	var dirs, badWrites sync.Map
	var written, unchanged, failed atomic.Int64
	for range c.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				p := filepath.Join(c.Output, j.rel)
				if j.known {
					if _, err := os.Stat(p); err == nil {
						unchanged.Add(1)
						continue
					}
				} else if b, err := os.ReadFile(p); err == nil && string(b) == j.url {
					unchanged.Add(1)
					continue
				}
				d := filepath.Dir(p)
				if _, ok := dirs.Load(d); !ok {
					if err := os.MkdirAll(d, 0o755); err != nil {
						log.Print(err)
						failed.Add(1)
						badWrites.Store(j.rel, true)
						continue
					}
					dirs.Store(d, true)
				}
				if err := os.WriteFile(p, []byte(j.url), 0o644); err != nil {
					log.Print(err)
					failed.Add(1)
					badWrites.Store(j.rel, true)
					continue
				}
				written.Add(1)
			}
		}()
	}

	groups, err := loadGroups(c.groupsPath)
	if err != nil {
		return err
	}
	// Progress for long first syncs: one line every 5s while files are being written.
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				log.Printf("  ... %d files processed (%d written)", written.Load()+unchanged.Load()+failed.Load(), written.Load())
			}
		}
	}()

	newGroups := map[string]int{}
	ticked := map[string]int{}
	seen := map[string]rec{}        // rel -> URL hash
	inPlaylist := map[uint64]bool{} // every URL in the playlist, ticked or not
	written1 := map[uint64]bool{}   // URLs already given a file this run
	parsed, excludedN, sourceFailed := 0, 0, false
	for _, src := range c.Playlists {
		log.Printf("fetching playlist %s", redact(src))
		before := parsed
		rc, err := open(src)
		if err == nil {
			err = parse(rc, func(e entry) {
				parsed++
				h := fnv.New64a()
				h.Write([]byte(e.url))
				hash := h.Sum64()
				inPlaylist[hash] = true
				k := kindOf(e.url)
				on, added := groups.on(k, e.group, c.NewGroups == "subscribe")
				if added {
					newGroups[k]++
				}
				if !on || written1[hash] || !slices.Contains(c.Types, k) {
					return
				}
				if c.excluded(e.name) {
					excludedN++
					return
				}
				ticked[k]++
				written1[hash] = true
				// Same title from another group is a different stream (often another language):
				// keep it, labelled with its group.
				rel := relPath(e)
				if _, dup := seen[rel]; dup {
					base := strings.TrimSuffix(rel, ".strm") + " - " + clean(e.group)
					rel = base + ".strm"
					for n := 2; ; n++ {
						if _, dup := seen[rel]; !dup {
							break
						}
						rel = fmt.Sprintf("%s (%d).strm", base, n)
					}
				}
				seen[rel] = rec{H: hash}
				jobs <- job{rel, e.url, state[rel].H == hash}
			})
			rc.Close()
		}
		if err != nil {
			log.Printf("playlist %s: %v", redact(src), err)
			sourceFailed = true
		} else {
			log.Printf("  %d entries", parsed-before)
		}
	}
	close(jobs)
	wg.Wait()
	close(done) // writing finished; purge below reports its own count

	log.Printf("in ticked groups: %d movies, %d episodes, %d live channels (%d excluded by title)", ticked["movies"], ticked["series"], ticked["live"], excludedN)
	if len(seen) == 0 && parsed > 0 {
		log.Printf("no groups are ticked, so nothing to write. Run \"%s config\" to choose.", filepath.Base(os.Args[0]))
	}

	now := time.Now().Unix()
	for rel, r := range seen {
		r.T = now
		if _, bad := badWrites.Load(rel); bad {
			r.H = 0 // force a re-check next run
		}
		state[rel] = r
	}
	if len(newGroups) > 0 {
		log.Printf("new groups found: %d movies, %d series, %d live (%s). Run \"%s config\" to review.",
			newGroups["movies"], newGroups["series"], newGroups["live"], map[string]string{"ignore": "not synced", "subscribe": "synced"}[c.NewGroups], filepath.Base(os.Args[0]))
		if err := groups.save(c.groupsPath); err != nil {
			log.Print(err)
		}
	}

	// Never purge after a failed or empty fetch: a provider outage must not wipe the library.
	purged := 0
	if !sourceFailed && parsed > 0 {
		for rel, r := range state {
			if _, ok := seen[rel]; ok {
				continue
			}
			// Still in the playlist but not wanted (unticked, excluded, moved) goes now;
			// gone from the playlist waits out the grace period in case it comes back.
			if !inPlaylist[r.H] && now-r.T < int64(c.staleAfter.Seconds()) {
				continue
			}
			p := filepath.Join(c.Output, rel)
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				log.Print(err)
				continue
			}
			delete(state, rel)
			purged++
			for d := filepath.Dir(p); d != c.Output && os.Remove(d) == nil; d = filepath.Dir(d) {
			} // drop now-empty folders
		}
	} else {
		log.Print("skipping purge: a playlist failed or was empty")
	}

	if err := os.MkdirAll(c.Output, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(state)
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, statePath); err != nil {
		return err
	}
	log.Printf("sync done in %s: %d files, %d written, %d unchanged, %d purged, %d errors",
		time.Since(start).Round(time.Millisecond), len(seen), written.Load(), unchanged.Load(), purged, failed.Load())
	return nil
}

type program struct {
	cfg  *Config
	stop chan struct{}
}

func (p *program) Start(service.Service) error { go p.loop(); return nil }
func (p *program) Stop(service.Service) error  { close(p.stop); return nil }

func (p *program) loop() {
	log.Printf("syncing %d playlist(s) into %s every %s", len(p.cfg.Playlists), p.cfg.Output, p.cfg.interval)
	if service.Interactive() {
		log.Print("press Ctrl+C to stop")
	}
	for {
		// Re-read the config each round so edits made with "config" apply without a restart.
		if c, err := loadConfig(p.cfg.path); err == nil {
			p.cfg = c
		} else {
			log.Printf("keeping previous settings: %v", err)
		}
		if err := syncOnce(p.cfg); err != nil {
			log.Print(err)
		}
		log.Printf("next sync at %s", time.Now().Add(p.cfg.interval).Format("15:04 (Mon)"))
		select {
		case <-p.stop:
			return
		case <-time.After(p.cfg.interval):
		}
	}
}

func main() {
	exe, _ := os.Executable()
	defaultCfg := filepath.Join(filepath.Dir(exe), "config.json")
	cfgPath := flag.String("config", defaultCfg, "path to config file")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `m3u8tostrm - turn m3u / m3u_plus playlists into .strm files and keep them in sync

Usage:
  %[1]s [-config FILE] [COMMAND]

Getting started:
  1. %[1]s              first run asks for your playlist URL and creates config.json
  2. %[1]s config       tick groups, set filters, change settings
  3. %[1]s install      run it in the background as a service
     %[1]s start

Commands:
  (none)      sync now, then again every "interval" (foreground; Ctrl+C to stop)
  once        sync one time and exit (for cron / Task Scheduler)
  config      settings, title filters and groups (interactive menu)
  install     install as a service: systemd, launchd or Windows service
  uninstall   remove the service
  start       start the service
  stop        stop the service
  restart     restart the service
              (service commands ask for admin rights via UAC / sudo when needed)
  help        show this help

Options:
  -config FILE   config file (default: %[2]s)

Files:
  config.json                       playlists, output folder, interval, stale_after, new_groups, ...
  groups.json                       next to config.json; which groups are ticked
  <output>/.m3u8tostrm-state.json   files this tool owns, used to purge stale ones

Docs: README.md
`, filepath.Base(os.Args[0]), defaultCfg)
	}
	flag.Parse()
	commands := []string{"", "once", "config", "install", "uninstall", "start", "stop", "restart", "service"}
	if flag.NArg() > 1 || !slices.Contains(commands, flag.Arg(0)) {
		if cmd := flag.Arg(0); cmd != "help" {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", strings.Join(flag.Args(), " "))
			flag.Usage()
			os.Exit(2)
		}
		flag.Usage()
		return
	}
	path, err := filepath.Abs(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	cmd := flag.Arg(0)
	cfg, err := loadConfig(path)
	firstRun := false
	if errors.Is(err, fs.ErrNotExist) {
		if firstRun, err = createConfig(path); err == nil && !firstRun {
			return // template written; the user edits it first
		}
		if err == nil {
			cfg, err = loadConfig(path)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		log.SetOutput(io.MultiWriter(f, os.Stdout)) // file first: a service may have no stdout
	}

	prg := &program{cfg: cfg, stop: make(chan struct{})}
	svc, err := service.New(prg, &service.Config{
		Name:        "m3u8tostrm",
		DisplayName: "m3u8tostrm",
		Description: "Converts m3u/m3u_plus playlists into .strm files.",
		Arguments:   []string{"-config", path},
	})
	if err != nil {
		log.Fatal(err)
	}

	// First run in a terminal with no command: go straight on to picking groups and what to do next.
	if firstRun && cmd == "" {
		counts, err := loadCounts(cfg)
		if err == nil {
			err = manageGroups(cfg, counts)
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("More settings and title filters: %s config\n", filepath.Base(os.Args[0]))
		if cmd, err = askNext(); err != nil {
			return
		}
	}

	// Service management needs admin/root: re-run this same command elevated (UAC / sudo) instead of failing.
	if slices.Contains([]string{"service", "install", "uninstall", "start", "stop", "restart"}, cmd) && !isAdmin() {
		fmt.Println("Administrator rights needed, asking for them...")
		code, err := runElevated([]string{"-config", path, cmd})
		if err != nil {
			log.Fatalf("%s: %v", cmd, err)
		}
		if code != 0 {
			where := "the output above"
			if cfg.LogFile != "" {
				where = cfg.LogFile
			}
			log.Fatalf("%s failed (exit code %d), see %s", cmd, code, where)
		}
		if runtime.GOOS == "windows" { // the elevated copy ran hidden, so report for it
			fmt.Println(okMsg(cmd))
		}
		return
	}

	switch cmd {
	case "exit":
	case "service":
		for _, a := range []string{"install", "start"} {
			if err := service.Control(svc, a); err != nil {
				log.Fatalf("%s: %v", a, err)
			}
		}
		fmt.Println(okMsg(cmd))
	case "":
		if err := svc.Run(); err != nil {
			log.Fatal(err)
		}
	case "config":
		if err := editConfig(path); err != nil {
			log.Fatal(err)
		}
	case "once":
		if err := syncOnce(cfg); err != nil {
			log.Fatal(err)
		}
	default:
		if err := service.Control(svc, cmd); err != nil {
			log.Fatalf("%s: %v", cmd, err)
		}
		fmt.Println(okMsg(cmd))
	}
}

func okMsg(cmd string) string {
	if cmd == "service" {
		return "Service installed and started."
	}
	return cmd + " ok"
}
