package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

// editConfig is the interactive "config" command: settings, filters and groups in one menu.
// Every section saves as soon as you confirm it; a running service picks changes up on its next sync.
func editConfig(path string) error {
	var counts map[string]map[string]int // playlist groups, downloaded once on first visit to Groups
	for {
		c, err := loadConfig(path)
		if err != nil {
			return err
		}
		filters := "none"
		if len(c.ExcludeTitles) > 0 {
			filters = "excluding " + strings.Join(c.ExcludeTitles, ", ")
		}
		var choice string
		err = huh.NewSelect[string]().Title("m3u8tostrm config").Options(
			huh.NewOption("Settings   playlists, output folder, schedule, ...", "settings"),
			huh.NewOption("Filters    "+filters, "filters"),
			huh.NewOption("Groups     choose movie / series / live groups", "groups"),
			huh.NewOption("Exit", "exit"),
		).Value(&choice).Run()
		if errors.Is(err, huh.ErrUserAborted) || choice == "exit" {
			fmt.Println("All changes are saved. A running sync or service applies them on its next sync.")
			return nil
		}
		if err != nil {
			return err
		}
		switch choice {
		case "settings":
			if err = editSettings(path); err == nil {
				counts = nil // playlists may have changed
			}
		case "filters":
			err = editFilters(c)
		case "groups":
			if counts == nil {
				counts, err = loadCounts(c)
			}
			if err == nil {
				err = manageGroups(c, counts)
			}
		}
		if err != nil && !errors.Is(err, huh.ErrUserAborted) {
			fmt.Println("Error:", err) // back to the menu so it can be fixed, e.g. a wrong playlist URL
		}
	}
}

func editSettings(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r := defaults() // raw values, so relative paths stay relative
	if err := json.Unmarshal(b, r); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	playlists, workers := strings.Join(r.Playlists, "\n"), strconv.Itoa(r.Workers)
	required := func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("required")
		}
		return nil
	}
	duration := func(min time.Duration) func(string) error {
		return func(s string) error {
			d, err := time.ParseDuration(strings.TrimSpace(s))
			if err != nil || d < min {
				return fmt.Errorf("use e.g. 30m, 6h or 72h (minimum %s)", min)
			}
			return nil
		}
	}
	err = huh.NewForm(
		huh.NewGroup(
			huh.NewText().Title("Playlists").Description("One URL or file path per line").Value(&playlists).Validate(required),
			huh.NewInput().Title("Output folder").Description("Where the .strm files go. Relative = next to config.json").Value(&r.Output).Validate(required),
			huh.NewMultiSelect[string]().Title("Sync").Description("space: toggle").Options(
				huh.NewOption("Movies", "movies"), huh.NewOption("Series", "series"), huh.NewOption("Live TV", "live"),
			).Value(&r.Types).Validate(func(t []string) error {
				if len(t) == 0 {
					return errors.New("pick at least one")
				}
				return nil
			}),
		),
		huh.NewGroup(
			huh.NewInput().Title("Sync every").Description("e.g. 30m, 6h, 24h").Value(&r.Interval).Validate(duration(time.Minute)),
			huh.NewInput().Title("Remove titles gone from the playlist after").
				Description("Grace period in case they come back, e.g. 72h. 0s = immediately").Value(&r.StaleAfter).Validate(duration(0)),
			huh.NewSelect[string]().Title("New groups from the provider").Options(
				huh.NewOption("Sync them automatically", "subscribe"),
				huh.NewOption("Ignore until I tick them", "ignore"),
			).Value(&r.NewGroups),
		),
		huh.NewGroup(
			huh.NewInput().Title("Parallel file writers").Description("16 is plenty; lower it for slow network shares").Value(&workers).Validate(func(s string) error {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n < 1 || n > 256 {
					return errors.New("a number from 1 to 256")
				}
				return nil
			}),
			huh.NewInput().Title("Log file").Description("Empty = console only. Takes effect after a restart").Value(&r.LogFile),
		),
	).Run()
	if err != nil {
		return err
	}
	var list []string
	for _, l := range strings.Split(playlists, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			list = append(list, l)
		}
	}
	r.Workers, _ = strconv.Atoi(strings.TrimSpace(workers))
	if err := setConfigKeys(path, map[string]any{
		"playlists": list, "types": r.Types, "output": strings.TrimSpace(r.Output), "interval": strings.TrimSpace(r.Interval),
		"stale_after": strings.TrimSpace(r.StaleAfter), "new_groups": r.NewGroups, "workers": r.Workers, "log_file": strings.TrimSpace(r.LogFile),
	}); err != nil {
		return err
	}
	fmt.Println("Settings saved.")
	return nil
}

func editFilters(c *Config) error {
	text := strings.Join(c.ExcludeTitles, ", ")
	err := huh.NewInput().Title("Exclude titles containing").
		Description("Comma separated, case-insensitive, matched anywhere in the title, in every group.\nExample: Trailer, Behind the Scenes").
		Value(&text).Run()
	if err != nil {
		return err
	}
	words := []string{}
	for _, w := range strings.Split(text, ",") {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, w)
		}
	}
	return setConfigKeys(c.path, map[string]any{"exclude_titles": words})
}

// loadCounts downloads the playlists and counts entries per kind and group,
// recording groups it has not seen before with the new_groups default.
func loadCounts(c *Config) (map[string]map[string]int, error) {
	g, err := loadGroups(c.groupsPath)
	if err != nil {
		return nil, err
	}
	counts := map[string]map[string]int{"movies": {}, "series": {}, "live": {}}
	fmt.Println("Loading playlists...")
	added := false
	for _, src := range c.Playlists {
		rc, err := open(src)
		if err != nil {
			return nil, fmt.Errorf("playlist %s: %w", redact(src), err)
		}
		err = parse(rc, func(e entry) {
			k := kindOf(e.url)
			counts[k][e.group]++
			_, a := g.on(k, e.group, c.NewGroups == "subscribe")
			added = added || a
		})
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("playlist %s: %w", redact(src), err)
		}
	}
	if added {
		return counts, g.save(c.groupsPath)
	}
	return counts, nil
}

// manageGroups shows Movies / Series / Live and their tick lists; each confirmed list is saved.
func manageGroups(c *Config, counts map[string]map[string]int) error {
	for {
		g, err := loadGroups(c.groupsPath)
		if err != nil {
			return err
		}
		var opts []huh.Option[string]
		for _, k := range kinds {
			if n := len(counts[k.key]); n > 0 {
				on := 0
				for name := range counts[k.key] {
					if g[k.key][name] {
						on++
					}
				}
				off := ""
				if !slices.Contains(c.Types, k.key) {
					off = "  (not synced: turned off in Settings)"
				}
				opts = append(opts, huh.NewOption(fmt.Sprintf("%-7s %d of %d groups on%s", k.title, on, n, off), k.key))
			}
		}
		opts = append(opts, huh.NewOption("Back", "back"))
		var choice string
		err = huh.NewSelect[string]().Title("Groups").Options(opts...).Value(&choice).Run()
		if errors.Is(err, huh.ErrUserAborted) || choice == "back" {
			return nil
		}
		if err != nil {
			return err
		}

		names := make([]string, 0, len(counts[choice]))
		for name := range counts[choice] {
			names = append(names, name)
		}
		sort.Strings(names)
		items := make([]huh.Option[string], len(names))
		for i, name := range names {
			label := name
			if label == "" {
				label = "(no group)"
			}
			items[i] = huh.NewOption(fmt.Sprintf("%s (%d)", label, counts[choice][name]), name).Selected(g[choice][name])
		}
		var picked []string
		err = huh.NewMultiSelect[string]().
			Title(choice + " groups").
			Description("space: toggle · ctrl+a: all/none · /: search · enter: save · ctrl+c: cancel").
			Options(items...).Value(&picked).Filterable(true).Height(20).Run()
		if errors.Is(err, huh.ErrUserAborted) {
			continue // this list unchanged
		}
		if err != nil {
			return err
		}
		if g[choice] == nil {
			g[choice] = map[string]bool{}
		}
		for _, name := range names {
			g[choice][name] = false
		}
		for _, name := range picked {
			g[choice][name] = true
		}
		if err := g.save(c.groupsPath); err != nil {
			return err
		}
	}
}
