# m3u8tostrm

Single binary, no runtime. Turns m3u / m3u_plus playlists into `.strm` files for Jellyfin, Emby, Plex or Kodi, keeps them in sync, and removes ones that drop out of the playlist.

## Setup

1. Pick the binary for your platform from `dist/` and put it in a folder.
2. Run it once. If there's no `config.json`, it asks for your playlist URL and output folder and creates one next to the binary. Without a terminal, it writes a template for you to edit instead. The file is created readable by its owner only, because the URL usually contains your provider login. All options:

| key           | meaning                                                                 | default |
|---------------|-------------------------------------------------------------------------|---------|
| `playlists`   | list of URLs or file paths                                              | required |
| `output`      | folder for `.strm` files (relative = next to config)                    | required |
| `types`       | what to sync: any of `movies`, `series`, `live`. Turning one off removes its files on the next sync | `["movies", "series"]` |
| `interval`    | time between syncs (`30m`, `6h`, ...)                                   | `6h`    |
| `stale_after` | delete a file once it has been missing from the playlist this long (`0s` = at once) | `72h` |
| `workers`     | parallel file writers                                                   | `16`    |
| `log_file`    | optional log file (useful on Windows, where services have no console)   | none    |
| `new_groups`  | what a sync does with a group it hasn't seen before: `subscribe` or `ignore` | `subscribe` |
| `exclude_titles` | words that drop a title from every group, case-insensitive, e.g. `["Trailer", "Behind the Scenes"]` | none |

3. First run continues straight into choosing groups. After that, use `m3u8tostrm config` for everything. With the default `new_groups: subscribe`, every group starts ticked, so untick what you don't want before the first sync.

## Configuring

```
m3u8tostrm config
```

One menu for everything. Each section saves when you confirm it. A running sync or service picks up the changes on its next sync, except `log_file`, which needs a restart.

- **Settings:** playlists, output folder, what to sync (Movies / Series / Live TV checkboxes), sync interval, grace period, new-group behaviour, workers, log file. Values are checked before saving.
- **Filters:** words that drop a title from every group (`exclude_titles`). Use it for things a group can't separate, e.g. sports events mixed into a "New Releases" group.
- **Groups:** downloads the playlist once, then Movies / Series / Live tick lists. Space toggles, ctrl+a selects all or none, `/` searches, enter saves.

Notes:
- Files for unticked groups and excluded titles are removed on the next sync. The `stale_after` grace period does not apply.
- New groups that appear at the provider are added to `groups.json` with the `new_groups` default and logged.
- `config.json` and `groups.json` are plain JSON, so you can also edit them by hand.

## Run

```
m3u8tostrm config        # settings, filters and groups
m3u8tostrm once          # one sync, then exit (cron / Task Scheduler)
m3u8tostrm               # sync every <interval> in the foreground
m3u8tostrm install       # install as a service: systemd, launchd or Windows service (prompts UAC / sudo)
m3u8tostrm start | stop | restart | uninstall
```

Use `-config path/to/config.json` to point at a config somewhere else. `install` records the absolute config path.

## Output layout

```
Live/<name>.strm
Movies/<name>/<name>.strm               URL contains /movie/
Series/<show>/Season NN/<name>.strm     URL contains /series/ and the name has SxxEyy
```

Groups are not folders. Each movie gets its own folder, which is the layout Emby, Jellyfin and Plex recommend.
- **Same stream in several groups:** written once.
- **Same title in several groups, different streams** (e.g. an English and a Dutch-dubbed version): the first copy in the playlist keeps the plain name, and the others get their group appended, e.g. `Some Film (2019)/Some Film (2019) - Kids Dubbed.strm`. Media servers show these as one movie with a version picker.

## Stale / purge

`output/.m3u8tostrm-state.json` tracks every file the tool wrote. Only those files are ever deleted, so your own files are safe. Nothing is purged if any playlist fails to download or comes back empty, so a provider outage won't wipe your library. Folders left empty are removed.

## Build

```
go test ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/m3u8tostrm-linux-amd64 .
```
