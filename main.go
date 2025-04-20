package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	FullTitle        string  `json:"full_title"`
	ParentMediaIndex int     `json:"parent_media_index"`
	MediaIndex       int     `json:"media_index"`
	WatchedStatus    float64 `json:"watched_status"`
	PercentComplete  int     `json:"percent_complete"`
}

func main() {
	// Load configuration from environment variables
	config := loadConfig()

	// Create HTTP server
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
			log.Printf("Error unmarshaling payload: %v", err)
			http.Error(w, "Error parsing payload", http.StatusBadRequest)
			return
		}

		// Check if this is a media.stop event
		if payload.Event != "media.stop" {
			if config.Debug {
				log.Printf("Ignoring event: %s", payload.Event)
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
				log.Printf("Invalid request, No metadata found")
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
			log.Printf("Error fetching metadata: %v", err)
			http.Error(w, "Error fetching metadata", http.StatusInternalServerError)
			return
		}

		if len(mediaData) == 0 {
			log.Printf("No entries found in Tautulli for metadata key: %s - This is normal for newly added content", payload.Metadata.Key)
			if config.Debug {
				log.Printf("Make sure Tautulli is properly configured and the content has been played at least once")
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
			if data.WatchedStatus >= 1.0 {
				filename := fmt.Sprintf("%s - S%dE%d.json", data.FullTitle, data.ParentMediaIndex, data.MediaIndex)
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
	})

	// Start server
	log.Printf("Server v1.2 running on port %d", config.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), nil))
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

// fetchMetadata fetches metadata from Tautulli API
func fetchMetadata(path string, config Config) ([]MediaData, error) {
	if path == "" {
		return nil, nil
	}

	// Extract the key from the path
	// The path could be in various formats, but we need to extract the numeric ID
	// Common formats: "/library/metadata/12345", "library/metadata/12345", etc.
	key := ""

	// First try to find "/library/metadata/" pattern
	for i := 0; i < len(path); i++ {
		if i+18 < len(path) && path[i:i+18] == "/library/metadata/" {
			potentialKey := path[i+18:]
			// Check if the key is numeric
			if _, err := strconv.Atoi(potentialKey); err == nil {
				key = potentialKey
				break
			}
		}
	}

	// If not found, try to extract just the numeric ID from the end of the path
	if key == "" {
		// Find the last slash and extract everything after it
		lastSlashIndex := -1
		for i := len(path) - 1; i >= 0; i-- {
			if path[i] == '/' {
				lastSlashIndex = i
				break
			}
		}

		if lastSlashIndex != -1 && lastSlashIndex < len(path)-1 {
			potentialKey := path[lastSlashIndex+1:]
			// Check if the key is numeric
			if _, err := strconv.Atoi(potentialKey); err == nil {
				key = potentialKey
			}
		}
	}

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
		return nil, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Error closing response body: %v", closeErr)
		}
	}()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse the response
	var tautulliResp TautulliResponse
	if err := json.Unmarshal(body, &tautulliResp); err != nil {
		return nil, err
	}

	// Return the data
	if tautulliResp.Response.Data.Data == nil {
		return []MediaData{}, nil
	}
	return tautulliResp.Response.Data.Data, nil
}
