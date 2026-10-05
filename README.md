# plex-clean

House rules for media after it has been watched, plus torrent cleanup. A single Go binary with no dependencies.

1. **Watched events** come from Plex webhooks (`media.scrobble`, needs Plex Pass) and the Jellyfin
   [Webhook plugin](https://github.com/jellyfin/jellyfin-plugin-webhook) (Playback Stop, played to completion).
   No Tautulli needed. Every watched item gets a marker file in `OUTPUT_DIR` (same format as before).
2. **Episodes wait a grace period** (default 24 h) in a persistent queue, so you can rewatch or catch up.
   Watching the same episode again, or in the other server, doesn't restart the clock.
3. **Then the show's rule applies:**
   - Shows managed by **Sonarr** use tags: `delete-after-watch` deletes the episode file through Sonarr;
     `archive` copies it to `ARCHIVE_DIR/<show>/Season NN/` first. Both unmonitor the episode so Sonarr
     won't fetch it again.
   - Shows **not in Sonarr** (e.g. downloaded by qBittorrent RSS rules) can be listed in `ARCHIVE_SHOWS` or
     `DELETE_SHOWS`. Their file is found in `SEARCH_DIRS` by release name (`Show.Name.S14E10...`, `1x10`);
     archive moves it to `ARCHIVE_DIR/<show>/` keeping its name.
   - Anything else is left alone.
4. **Torrent sweep** (formerly qbittorrent-cleaner): every `SWEEP_INTERVAL`, completed torrents whose files
   are gone are removed from qBittorrent. It checks each torrent's own save path, skips categories managed by
   Sonarr/Radarr, and refuses to run if the downloads share looks unmounted or if more than `SWEEP_MAX_REMOVE`
   torrents, and over half of them, look deleted at once.

## Configuration

| Variable | Default | |
|---|---|---|
| `PORT` | `3333` | Webhook server |
| `OUTPUT_DIR` | `/output` | Marker files |
| `STATE_FILE` | `/data/pending.json` | Grace-period queue |
| `GRACE_PERIOD` | `24h` | Time between watching and acting |
| `CHECK_INTERVAL` | `5m` | How often due episodes are processed |
| `DRY_RUN` | `false` | Log what would happen, change nothing |
| `SONARR_URL`, `SONARR_API_KEY` | | Enables the Sonarr rules |
| `DELETE_TAG`, `ARCHIVE_TAG` | `delete-after-watch`, `archive` | Sonarr tag labels |
| `ARCHIVE_DIR` | `/archive` | Archive root |
| `ARCHIVE_SHOWS`, `DELETE_SHOWS` | | Comma-separated show names for shows not in Sonarr |
| `SEARCH_DIRS` | `/downloads/ravi,/downloads/daniela` | Where name-based rules look for files |
| `QBT_URL`, `QBT_USER`, `QBT_PASS` | | Enables the torrent sweep; credentials optional on qBittorrent's auth whitelist |
| `SWEEP_INTERVAL` | `15m` | `0` disables the sweep |
| `SWEEP_ROOT` | `/downloads` | Only torrents saved below this path; must match qBittorrent's container path |
| `SWEEP_SKIP_CATEGORIES` | `sonarr,radarr` | Categories whose torrents their apps remove |
| `SWEEP_MAX_REMOVE` | `5` | Safety limit, see above |
| `DEBUG` | `false` | Verbose logging |

Paths must be the same inside plex-clean, Sonarr and qBittorrent (mount the downloads share at `/downloads`
everywhere), because Sonarr and qBittorrent report their own paths.

## Endpoints

- `POST /plex`: Plex webhook (Settings → Webhooks).
- `POST /jellyfin`: Jellyfin Webhook plugin, notification type "Playback Stop".
- `POST /`: either, detected by content type.
- `GET /pending`: the queue. `GET /healthz`: liveness.

## Development

```bash
go test -race ./...
```

Releases: pushing a `v*` tag builds and publishes `mallox/plex-clean` to Docker Hub.
