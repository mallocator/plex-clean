package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Config holds the application configuration
type Config struct {
	Port      int
	APIHost   string
	APIKey    string
	OutputDir string
	Debug     bool
}

// PlexWebhookPayload represents the payload received from Plex webhook
type PlexWebhookPayload struct {
	Event    string `json:"event"`
	Metadata struct {
		Key string `json:"key"`
	} `json:"Metadata"`
}

// JellyfinWebhookPayload represents the payload received from Jellyfin webhook
type JellyfinWebhookPayload struct {
	Event       string `json:"Event"`
	ItemID      string `json:"ItemId"`
	ItemType    string `json:"ItemType"`
	MediaStatus struct {
		PlaybackStatus     string `json:"PlaybackStatus"`
		PositionTicks      int64  `json:"PositionTicks"`
		IsPaused           bool   `json:"IsPaused"`
		PlayedToCompletion bool   `json:"PlayedToCompletion"`
	} `json:"MediaStatus"`
	NotificationType string `json:"NotificationType"`
	Title            string `json:"Name"`
	SeriesName       string `json:"SeriesName"`
	SeasonNumber     int    `json:"SeasonNumber"`
	EpisodeNumber    int    `json:"EpisodeNumber"`
}

// TautulliResponse represents the response from Tautulli API
type TautulliResponse struct {
	Response struct {
		Data struct {
			Data []MediaData `json:"data"`
		} `json:"data"`
	} `json:"response"`
}

// MediaData represents the media data from Tautulli
type MediaData struct {
	FullTitle        string      `json:"full_title"`
	ParentMediaIndex json.Number `json:"parent_media_index"`
	MediaIndex       json.Number `json:"media_index"`
	WatchedStatus    float64     `json:"watched_status"`
	PercentComplete  int         `json:"percent_complete"`
}

func main() {
	// Load configuration from environment variables
	config := loadConfig()

	// Create HTTP server with routing
	http.HandleFunc("/plex", func(w http.ResponseWriter, r *http.Request) {
		handlePlexWebhook(w, r, config)
	})

	http.HandleFunc("/jellyfin", func(w http.ResponseWriter, r *http.Request) {
		handleJellyfinWebhook(w, r, config)
	})

	// Default handler for backward compatibility
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// If the path is exactly "/", try to detect the webhook type from the content
		if r.URL.Path == "/" {
			contentType := r.Header.Get("Content-Type")

			// Plex webhooks are typically sent as multipart/form-data
			if strings.Contains(contentType, "multipart/form-data") {
				if config.Debug {
					log.Printf("Detected Plex webhook based on Content-Type")
				}
				handlePlexWebhook(w, r, config)
				return
			}

			// Jellyfin webhooks are typically sent as application/json
			if strings.Contains(contentType, "application/json") {
				if config.Debug {
					log.Printf("Detected Jellyfin webhook based on Content-Type")
				}
				handleJellyfinWebhook(w, r, config)
				return
			}

			// If we can't determine the type, return an error
			log.Printf("Unable to determine webhook type from request")
			http.Error(w, "Unable to determine webhook type", http.StatusBadRequest)
			return
		}

		// For any other path, return 404
		http.NotFound(w, r)
	})

	// Start server
	log.Printf("Server running on port %d", config.Port)
	log.Printf("Plex webhook support is enabled")
	log.Printf("Jellyfin webhook support is enabled")
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), nil))
}

// handlePlexWebhook processes Plex webhook requests
func handlePlexWebhook(w http.ResponseWriter, r *http.Request, config Config) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form
	err := r.ParseMultipartForm(10 << 20) // 10 MB max memory
	if err != nil {
		log.Printf("Error parsing multipart form: %v", err)
		http.Error(w, "Error parsing form", http.StatusBadRequest)
		return
	}

	// Get payload from form
	payloadStr := r.FormValue("payload")
	if payloadStr == "" {
		log.Printf("No payload found in request")
		http.Error(w, "No payload found", http.StatusBadRequest)
		return
	}

	// Parse payload
	var payload PlexWebhookPayload
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		log.Printf("Error unmarshaling Plex payload: %v", err)
		http.Error(w, "Error parsing payload", http.StatusBadRequest)
		return
	}

	// Check if this is a media.stop event
	if payload.Event != "media.stop" {
		if config.Debug {
			log.Printf("Ignoring Plex event: %s", payload.Event)
		}
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("OK"))
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	// Check if metadata is present
	if payload.Metadata.Key == "" {
		if config.Debug {
			log.Printf("Invalid Plex request, No metadata found")
		}
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("OK"))
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	// Fetch metadata from Tautulli
	mediaData, err := fetchMetadata(payload.Metadata.Key, config)
	if err != nil {
		log.Printf("Error fetching metadata from Tautulli: %v", err)
		http.Error(w, "Error fetching metadata", http.StatusInternalServerError)
		return
	}

	if len(mediaData) == 0 {
		if config.Debug {
			log.Printf("No entries found in Tautulli for metadata key: %s", payload.Metadata.Key)
		}
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("OK"))
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	} else if config.Debug {
		log.Printf("Found %d entries for %s", len(mediaData), payload.Metadata.Key)
	}

	// Process media data
	for _, data := range mediaData {
		// Convert ParentMediaIndex and MediaIndex to integers
		parentMediaIndex, err := data.ParentMediaIndex.Int64()
		if err != nil {
			log.Printf("Error converting ParentMediaIndex to int: %v", err)
			continue
		}
		mediaIndex, err := data.MediaIndex.Int64()
		if err != nil {
			log.Printf("Error converting MediaIndex to int: %v", err)
			continue
		}

		if data.WatchedStatus >= 1.0 {
			filename := fmt.Sprintf("%s - S%dE%d.json", data.FullTitle, parentMediaIndex, mediaIndex)
			log.Printf("Media marked as watched by Plex, writing to file %s", filename)

			// Create the output directory if it doesn't exist
			if err := os.MkdirAll(config.OutputDir, 0755); err != nil {
				log.Printf("Error creating output directory: %v", err)
				continue
			}

			// Write the data to a file
			jsonData, err := json.MarshalIndent(data, "", "  ")
			if err != nil {
				log.Printf("Error marshaling JSON: %v", err)
				continue
			}

			outputPath := filepath.Join(config.OutputDir, filename)
			if err := os.WriteFile(outputPath, jsonData, 0644); err != nil {
				log.Printf("Error writing file: %v", err)
			}
		} else if config.Debug {
			log.Printf("Media not marked as watched by Plex, ignoring")
		}
	}

	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte("OK"))
	if err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// handleJellyfinWebhook processes Jellyfin webhook requests
func handleJellyfinWebhook(w http.ResponseWriter, r *http.Request, config Config) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading Jellyfin request body: %v", err)
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("Error closing Jellyfin request body: %v", err)
		}
	}(r.Body)

	// Parse the JSON payload
	var payload JellyfinWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("Error unmarshaling Jellyfin payload: %v", err)
		http.Error(w, "Error parsing payload", http.StatusBadRequest)
		return
	}

	// Check if this is a playback stop event with completion
	if payload.Event != "playback.stop" && payload.NotificationType != "PlaybackStop" {
		if config.Debug {
			log.Printf("Ignoring Jellyfin event: %s/%s", payload.Event, payload.NotificationType)
		}
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("OK"))
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	// Check if the media was played to completion
	if !payload.MediaStatus.PlayedToCompletion {
		if config.Debug {
			log.Printf("Jellyfin media not played to completion, ignoring")
		}
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("OK"))
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	// For episodes, use series name, season, and episode
	if payload.ItemType == "Episode" && payload.SeriesName != "" {
		// Create a MediaData object to maintain consistency with Plex
		mediaData := MediaData{
			FullTitle:        payload.SeriesName + " - " + payload.Title,
			ParentMediaIndex: json.Number(strconv.Itoa(payload.SeasonNumber)),
			MediaIndex:       json.Number(strconv.Itoa(payload.EpisodeNumber)),
			WatchedStatus:    1.0, // Marked as watched
			PercentComplete:  100, // Assuming 100% complete
		}

		filename := fmt.Sprintf("%s - S%dE%d.json", payload.SeriesName, payload.SeasonNumber, payload.EpisodeNumber)
		log.Printf("Media marked as watched by Jellyfin, writing to file %s", filename)

		// Create the output directory if it doesn't exist
		if err := os.MkdirAll(config.OutputDir, 0755); err != nil {
			log.Printf("Error creating output directory: %v", err)
			http.Error(w, "Error creating output directory", http.StatusInternalServerError)
			return
		}

		// Write the data to a file
		jsonData, err := json.MarshalIndent(mediaData, "", "  ")
		if err != nil {
			log.Printf("Error marshaling JSON: %v", err)
			http.Error(w, "Error marshaling JSON", http.StatusInternalServerError)
			return
		}

		outputPath := filepath.Join(config.OutputDir, filename)
		if err := os.WriteFile(outputPath, jsonData, 0644); err != nil {
			log.Printf("Error writing file: %v", err)
			http.Error(w, "Error writing file", http.StatusInternalServerError)
			return
		}
	} else if payload.ItemType == "Movie" {
		// Handle movies
		mediaData := MediaData{
			FullTitle:        payload.Title,
			ParentMediaIndex: json.Number("0"), // No season for movies
			MediaIndex:       json.Number("0"), // No episode for movies
			WatchedStatus:    1.0,              // Marked as watched
			PercentComplete:  100,              // Assuming 100% complete
		}

		filename := fmt.Sprintf("%s.json", payload.Title)
		log.Printf("Movie marked as watched by Jellyfin, writing to file %s", filename)

		// Create the output directory if it doesn't exist
		if err := os.MkdirAll(config.OutputDir, 0755); err != nil {
			log.Printf("Error creating output directory: %v", err)
			http.Error(w, "Error creating output directory", http.StatusInternalServerError)
			return
		}

		// Write the data to a file
		jsonData, err := json.MarshalIndent(mediaData, "", "  ")
		if err != nil {
			log.Printf("Error marshaling JSON: %v", err)
			http.Error(w, "Error marshaling JSON", http.StatusInternalServerError)
			return
		}

		outputPath := filepath.Join(config.OutputDir, filename)
		if err := os.WriteFile(outputPath, jsonData, 0644); err != nil {
			log.Printf("Error writing file: %v", err)
			http.Error(w, "Error writing file", http.StatusInternalServerError)
			return
		}
	} else {
		if config.Debug {
			log.Printf("Unsupported Jellyfin item type: %s", payload.ItemType)
		}
	}

	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte("OK"))
	if err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// loadConfig loads configuration from environment variables
func loadConfig() Config {
	portStr := getEnv("PORT", "3333")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Printf("Invalid PORT value: %s, using default 3333", portStr)
		port = 3333
	}
	return Config{
		Port:      port,
		APIHost:   getEnv("API_HOST", ""),
		APIKey:    getEnv("API_KEY", ""),
		OutputDir: getEnv("OUTPUT_DIR", "/output"),
		Debug:     getEnv("DEBUG", "false") == "true",
	}
}

// getEnv gets an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func fetchMetadata(path string, config Config) ([]MediaData, error) {
	if path == "" {
		return nil, nil
	}

	// Extract the key from the path
	key := extractKeyFromPath(path)
	if key == "" {
		if config.Debug {
			log.Printf("Could not extract key from path: %s", path)
		}
		return nil, nil
	}

	// Construct the URL
	url := fmt.Sprintf("http://%s/api/v2?apikey=%s&cmd=get_history&rating_key=%s&order_column=started&order=desc&length=1",
		config.APIHost, config.APIKey, key)

	// Make the request
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error making HTTP request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Error closing response body: %v", closeErr)
		}
	}()

	// Check for non-200 status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("received non-200 response: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	// Preprocess the JSON to handle various edge cases in the response
	// This is necessary because the Tautulli API sometimes returns empty strings for numeric fields,
	// which causes the JSON unmarshaler to fail. We use regular expressions to handle different
	// spacing patterns in the JSON and replace empty strings with appropriate values.
	bodyStr := string(body)

	// Use regular expressions to handle different spacing patterns
	// Replace empty strings with "0" for json.Number fields
	// The \s* in the regex matches any amount of whitespace, making it flexible with spacing
	parentMediaIndexRegex := regexp.MustCompile(`"parent_media_index"\s*:\s*""`)
	bodyStr = parentMediaIndexRegex.ReplaceAllString(bodyStr, `"parent_media_index":"0"`)

	mediaIndexRegex := regexp.MustCompile(`"media_index"\s*:\s*""`)
	bodyStr = mediaIndexRegex.ReplaceAllString(bodyStr, `"media_index":"0"`)

	// Handle cases for float64 and int fields
	// Empty strings in these fields would also cause unmarshaling errors
	watchedStatusRegex := regexp.MustCompile(`"watched_status"\s*:\s*""`)
	bodyStr = watchedStatusRegex.ReplaceAllString(bodyStr, `"watched_status":0`)

	percentCompleteRegex := regexp.MustCompile(`"percent_complete"\s*:\s*""`)
	bodyStr = percentCompleteRegex.ReplaceAllString(bodyStr, `"percent_complete":0`)

	// Parse the response
	var tautulliResp TautulliResponse
	if err := json.Unmarshal([]byte(bodyStr), &tautulliResp); err != nil {
		return nil, fmt.Errorf("error unmarshaling response: %w", err)
	}

	// Return the data
	if tautulliResp.Response.Data.Data == nil {
		return []MediaData{}, nil
	}
	return tautulliResp.Response.Data.Data, nil
}

func extractKeyFromPath(path string) string {
	// Look for "/library/metadata/" and extract the numeric key
	const prefix = "/library/metadata/"
	if idx := strings.Index(path, prefix); idx != -1 { // Fixed to use strings.Index
		potentialKey := path[idx+len(prefix):]
		if _, err := strconv.Atoi(potentialKey); err == nil {
			return potentialKey
		}
	}

	// Fallback: extract the numeric key after the last slash
	if lastSlashIndex := strings.LastIndex(path, "/"); lastSlashIndex != -1 { // Fixed to use strings.LastIndex
		potentialKey := path[lastSlashIndex+1:]
		if _, err := strconv.Atoi(potentialKey); err == nil {
			return potentialKey
		}
	}

	return ""
}
