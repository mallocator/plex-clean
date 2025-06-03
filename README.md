# Plex Clean

A Go application that listens for Plex and Jellyfin webhook events and writes metadata to files when media is marked as watched.

## Overview

This application:
1. Listens for webhook events from Plex and/or Jellyfin
2. For Plex: When a media.stop event is received, it fetches metadata from Tautulli
3. For Jellyfin: When a playback.stop event is received with PlayedToCompletion flag
4. If the media is marked as watched, it writes the metadata to a JSON file

## Usage

### Docker

The easiest way to run the application is using Docker:

```bash
docker run -p 3333:3333 \
  -e API_HOST=your-tautulli-host:port \
  -e API_KEY=your-tautulli-api-key \
  -v /path/to/output:/output \
  mallocator/plex-clean
```

### Environment Variables

- `PORT`: The port on which the webhook server listens (default: 3333)
- `API_HOST`: The hostname and port of your Tautulli server (required for Plex)
- `API_KEY`: Your Tautulli API key (required for Plex)
- `OUTPUT_DIR`: The directory where output files will be written (default: /output)
- `DEBUG`: Enable debug logging (default: false)

### Endpoints

The application provides the following endpoints:

- `/plex`: Dedicated endpoint for Plex webhooks
- `/jellyfin`: Dedicated endpoint for Jellyfin webhooks
- `/`: Default endpoint that attempts to detect the webhook type based on the Content-Type header

For Jellyfin, you'll need to configure the webhook plugin to send events to the `/jellyfin` endpoint.

## Changes from JavaScript Version

The original JavaScript version used the `percent_complete` field to determine if media was watched. This Go version uses the `watched_status` field provided by Tautulli, which offers several advantages:

- Simplicity: The logic becomes a simple boolean check rather than a threshold comparison
- Reliability: Plex itself determines when content is "watched" based on its internal algorithms
- Consistency: Using Plex's own determination ensures consistency with the Plex UI

## Jellyfin Configuration

To use this application with Jellyfin:

1. Install the [Jellyfin Webhook Plugin](https://github.com/jellyfin/jellyfin-plugin-webhook)
2. Configure a new webhook with the following settings:
   - Server URL: `http://your-server:3333/jellyfin`
   - Notification Type: Select at least "Playback Stop"
   - Item Types: Select the media types you want to track (e.g., "Episodes", "Movies")
   - User Filter: Optionally filter by specific users

The application will process playback.stop events from Jellyfin and write metadata to files when media is marked as watched (PlayedToCompletion = true).

## References

* Python API has a list of properties that can be useful: https://python-plexapi.readthedocs.io/en/latest/modules/video.html
* Community sourced info around the Plex API: https://github.com/Arcanemagus/plex-api/wiki
* Official Plex API documentation: https://support.plex.tv/articles/201638786-plex-media-server-url-commands/
* Tautulli API Reference: https://github.com/Tautulli/Tautulli/wiki/Tautulli-API-Reference
* Jellyfin Webhook Plugin: https://github.com/jellyfin/jellyfin-plugin-webhook
