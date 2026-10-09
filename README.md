# plex-clean

House rules for media after it has been watched, plus torrent cleanup. A single Go binary with no dependencies.

1. **Watched events** come from Plex webhooks (`media.scrobble`, needs Plex Pass) and the Jellyfin
   [Webhook plugin](https://github.com/jellyfin/jellyfin-plugin-webhook) (Playback Stop, played to completion).
   No Tautulli needed. Every watched item is logged, and optionally gets a marker file in `OUTPUT_DIR`.
   A "movie" whose title is a release name (`Show S17E01 ...`) counts as that episode: loose episode files in a
   download folder show up as movies in Jellyfin's mixed libraries.
   **Watched poll** (every `WATCHED_POLL_INTERVAL`): webhooks get lost (a client reports a wrong stop position, the
   container restarts), so plex-clean also asks the servers which episodes are *marked* watched: every Jellyfin user,
   and the Plex owner account (other Plex accounts rely on the webhook). Marks since `WATCHED_LOOKBACK` it hasn't
   seen are queued with their watch time; an episode marked watched by hand counts too. A mark that is already older than `WATCHED_MAX_AGE` when it first appears was synced from the other server (WatchState copies the original date, and has copied wrong marks) and is only logged. Safeguards: the first run
   only records the current marks, and more than `WATCHED_BURST_MAX` new marks in one poll (a mass sync, e.g. by
   WatchState) are logged as held instead of queued (`plexclean_watched_poll_held_total`). Known marks are kept in
   `WATCHED_SEEN_FILE`.
2. **Episodes wait a grace period** (default 24 h) in a persistent queue, so you can rewatch or catch up.
   Watching the same episode again, or in the other server, doesn't restart the clock.
3. **Then the show's rule applies:**
   - Shows managed by **Sonarr** use tags: `delete-after-watch` deletes the episode file through Sonarr;
     `archive` copies it to `ARCHIVE_DIR/<show>/Season NN/` first (`ANIME_ARCHIVE_DIR` for anime series). Both
     unmonitor the episode so Sonarr won't fetch it again. If Sonarr has no file for the episode (downloaded by an
     RSS rule before the show moved to Sonarr), the tag of a monitored show applies to the file found by name, as below
     (unmonitored shows, e.g. added by an import list, are left alone).
   - Shows **not in Sonarr** (e.g. downloaded by qBittorrent RSS rules) can be listed in `ARCHIVE_SHOWS` or
     `DELETE_SHOWS`. Their file is found in `SEARCH_DIRS` by release name (`Show.Name.S14E10...`, `1x10`);
     archive moves it to `ARCHIVE_DIR/<show>/` keeping its name. These name rules also apply to a show Sonarr
     knows but has no file or tag for (e.g. added by an import list while an RSS rule downloads it).
   - Anything else is left alone.
4. **Per-user folders**: Seerr tags each request with the requester (`2-daniela`). With `USER_FOLDERS`
   (`2-daniela=/downloads/daniela,1-mallox=/downloads/ravi`; keys are Seerr's tag labels `<user id>-<name>`):
   - shows and movies carrying a user tag are moved through Sonarr/Radarr (`moveFiles=true`) into `<folder>/tv` or
     `<folder>/movies`;
   - shows and movies without a user tag that sit in a user's folder (added or imported directly) get that user's
     tag, so everything ends up tagged by owner.
   Runs on Sonarr/Radarr "Connect" webhooks (`POST /sonarr`, `/radarr`, e.g. On Series/Movie Add) and every
   `CHECK_INTERVAL`. Items outside the user folders are never touched.
5. **Simkl sync** (every `SIMKL_INTERVAL`): keeps Seerr and Radarr in line with the owner's Simkl lists.
   - Seerr blocklist (hidden from discovery and cannot be requested): dropped movies and dropped shows/anime. Shows being
     watched, on hold or completed stay requestable (their next seasons too), and completed movies remain requestable;
     blocks from earlier policies are removed without changing Simkl history. Titles Sonarr/Radarr manage are skipped.
   - The collection is Plex's movie sections below `PLEX_COLLECTION_ROOT` (Plex matches the curated library far
     better than Jellyfin) plus whatever Seerr reports as available.
   - A cinema watch does not unmonitor a wanted movie: watched history is independent of requests.
   - Collection rule: a wanted movie Seerr already reports as available (Jellyfin library) gets a Radarr exclusion
     (and is unmonitored if Radarr waits for it), so Radarr's Simkl import list doesn't fetch a second copy.
   - Each rule acts once per movie (`SIMKL_RULES_FILE` remembers them): a movie the owner monitors again in Radarr,
     or whose exclusion they delete, stays that way. Without the file, existing unmonitored movies and exclusions
     count as handled.
   - Auth: Simkl OAuth2 token file (`SIMKL_TOKEN_FILE`, from a one-time device login), renewed with its refresh
     token a day before it expires.
6. **Torrent sweep** (formerly qbittorrent-cleaner): every `SWEEP_INTERVAL`, completed torrents whose files
   are gone are removed from qBittorrent. It checks each torrent's own save path, skips categories managed by
   Sonarr/Radarr, and refuses to run if the downloads share looks unmounted or if more than `SWEEP_MAX_REMOVE`
   torrents, and over half of them, look deleted at once.
8. **Movies into the archive**: a Radarr movie tagged `MOVIE_ARCHIVE_TAG` (default `archive`), or marked as favourite
   (♥) in Jellyfin by one of `JELLYFIN_ARCHIVE_USERS`, is copied with its
   subtitles into `MOVIE_ARCHIVE_DIR` (e.g. the root of the movie archive, where the owner sorts it), then deleted in
   Radarr with its download files and an import exclusion. A file of the same name but different size already in the
   archive stops it (nothing is overwritten). Checked every `CHECK_INTERVAL`.
7. **Download health** (with the sweep): Sonarr/Radarr downloads that will never import are removed from qBittorrent
   through the app's queue, blocklisted, and searched again: fakes (an executable or archive instead of a video,
   nothing importable) and torrents without any data for `STALL_TIMEOUT`. At most `SWEEP_MAX_REMOVE` per run.
8. **Seerr orphans** (daily, `SEERR_RECONCILE_INTERVAL`): Seerr TV records stuck in "processing" whose series is no
   longer in Sonarr (by TVDB and TMDB ID), with no request and nothing available, are reset (`DELETE /api/v1/media/<id>`;
   files untouched). Seerr's own scan skips records created from TMDB alone, which otherwise stay processing forever and
   block requests for new seasons. Skipped when Sonarr reports no series; at most 50 resets per run.

**The archive is never touched.** Every deletion (Sonarr episode files, files found by name, the torrent sweep,
download health) first checks the path: it must be below `DELETE_ROOTS` (default `/downloads`) and outside
`ARCHIVE_DIR`, `ANIME_ARCHIVE_DIR`, `MOVIE_ARCHIVE_DIR` and `PROTECTED_DIRS`. Archiving only copies into the archive. Anything else is
refused and logged.

## Configuration

| Variable | Default | |
|---|---|---|
| `PORT` | `3333` | Webhook server |
| `OUTPUT_DIR` | | Marker files (one JSON per watched item); off by default, watched items are logged either way |
| `STATE_FILE` | `/data/pending.json` | Grace-period queue |
| `GRACE_PERIOD` | `24h` | Time between watching and acting |
| `CHECK_INTERVAL` | `5m` | How often due episodes are processed |
| `DRY_RUN` | `false` | Log what would happen, change nothing |
| `SONARR_URL`, `SONARR_API_KEY` | | Enables the Sonarr rules |
| `DELETE_TAG`, `ARCHIVE_TAG` | `delete-after-watch`, `archive` | Sonarr tag labels |
| `ARCHIVE_DIR` | `/archive` | Archive root |
| `ANIME_ARCHIVE_DIR` | | Archive root for Sonarr series of type anime (default: `ARCHIVE_DIR`) |
| `MOVIE_ARCHIVE_DIR`, `MOVIE_ARCHIVE_TAG` | , `archive` | Where Radarr movies tagged for the archive go; empty disables |
| `JELLYFIN_URL`, `JELLYFIN_API_KEY`, `JELLYFIN_ARCHIVE_USERS` | | Jellyfin favourites of these users archive a movie like the tag |
| `RADARR_URL`, `RADARR_API_KEY` | | Radarr for per-user movie folders |
| `USER_FOLDERS` | | `2-daniela=/base,1-mallox=/base2` (Seerr tag labels; a plain name also routes but can't be created as a tag) |
| `TV_SUBDIR`, `MOVIE_SUBDIR` | `tv`, `movies` | Subfolders below each user's base |
| `SIMKL_CLIENT_ID`, `SIMKL_TOKEN_FILE` | , `/data/simkl.json` | Simkl app and token (`access_token`, `refresh_token`, `expires_in`, `obtained_at`) |
| `SIMKL_INTERVAL` | `6h` | `0` disables the Simkl sync |
| `SIMKL_RULES_FILE` | `/data/simkl-rules.json` | Movies the collection rule already handled (includes legacy cinema decisions) |
| `SEERR_URL`, `SEERR_API_KEY`, `SEERR_USER_ID` | , , `1` | Seerr for the blocklist (entries attributed to that user) |
| `PLEX_URL`, `PLEX_COLLECTION_ROOT` | , `/volume1/Video` | Plex local API (no token from an allowed network) for the collection rule |
| `ARCHIVE_SHOWS`, `DELETE_SHOWS` | | Comma-separated show names for shows not in Sonarr |
| `SEARCH_DIRS` | `/downloads/ravi,/downloads/daniela` | Where name-based rules look for files |
| `QBT_URL`, `QBT_USER`, `QBT_PASS` | | Enables the torrent sweep; credentials optional on qBittorrent's auth whitelist |
| `SWEEP_INTERVAL` | `15m` | `0` disables the sweep |
| `SWEEP_ROOT` | `/downloads` | Only torrents saved below this path; must match qBittorrent's container path |
| `SWEEP_SKIP_CATEGORIES` | `sonarr,radarr` | Categories whose torrents their apps remove |
| `SWEEP_MAX_REMOVE` | `5` | Safety limit, see above; also the download health limit per run |
| `STALL_TIMEOUT` | `12h` | Sonarr/Radarr torrents without data this long are rejected; `0` disables |
| `STATS_INTERVAL` | `2m` | Refresh of the `/metrics` gauges; `0` disables |
| `SEERR_RECONCILE_INTERVAL` | `24h` | Reset orphaned "processing" TV records in Seerr; `0` disables (needs `SEERR_URL`/`SEERR_API_KEY` and Sonarr) |
| `WATCHED_POLL_INTERVAL` | `10m` | Poll Jellyfin/Plex watched marks; `0` disables |
| `WATCHED_LOOKBACK` | `48h` | Older marks never count |
| `WATCHED_MAX_AGE` | `4h` | A new mark older than this is treated as synced and ignored (must cover a long playback: Jellyfin dates a viewing from its start) |
| `WATCHED_BURST_MAX` | `25` | More new marks than this in one poll are held, not queued |
| `WATCHED_SEEN_FILE` | `/data/watched-seen.json` | Marks already handled |
| `DELETE_ROOTS` | `/downloads` | Deletions only below these |
| `PROTECTED_DIRS` | | Never deleted in, besides the archive dirs |
| `DEBUG` | `false` | Verbose logging |

Paths must be the same inside plex-clean, Sonarr and qBittorrent (mount the downloads share at `/downloads`
everywhere), because Sonarr and qBittorrent report their own paths.

## Endpoints

- `POST /plex`: Plex webhook (Settings → Webhooks).
- `POST /jellyfin`: Jellyfin Webhook plugin, notification type "Playback Stop".
- `POST /`: either, detected by content type.
- `POST /sonarr`, `POST /radarr`: Sonarr/Radarr Connect webhooks; trigger per-user routing.
- `GET /pending`: the queue. `GET /healthz`: liveness.
- `GET /metrics`: Prometheus metrics. Counters since start: `plexclean_watched_total{source}`,
  `plexclean_actions_total{action}` (episode_deleted, episode_archived, file_deleted, file_archived, movie_archived,
  torrent_swept, download_rejected with `reason` stalled/unusable, routed, tagged, seerr_reset), `plexclean_freed_bytes_total`.
  Gauges refreshed every `STATS_INTERVAL` from the services plex-clean already uses: `plexclean_pending_items`,
  `plexclean_qbittorrent_torrents{state}`, `plexclean_qbittorrent_speed_bytes{direction}`,
  `plexclean_arr_queue_items{app,state}`, `plexclean_arr_missing{app}`, `plexclean_arr_library_bytes{app}`,
  `plexclean_arr_library_files{app}`, `plexclean_streams{server,kind}` (Plex/Jellyfin sessions: direct, transcode_hw,
  transcode_sw) and `plexclean_stats_last_success_timestamp_seconds{source}`.

## Development

```bash
go test -race ./...
```

Releases: pushing a `v*` tag builds and publishes `mallox/plex-clean` to Docker Hub.
