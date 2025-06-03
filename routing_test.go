package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouting(t *testing.T) {
	// Create a temporary directory for output
	tempDir, err := os.MkdirTemp("", "test-routing-output")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Failed to remove temp dir: %v", err)
		}
	}()

	// Set up the config
	if err := os.Setenv("OUTPUT_DIR", tempDir); err != nil {
		t.Fatalf("Failed to set environment variable OUTPUT_DIR: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("OUTPUT_DIR"); err != nil {
			t.Logf("Failed to unset environment variable OUTPUT_DIR: %v", err)
		}
	}()

	// Create a test server for Tautulli API
	tautulliServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a mock response
		response := TautulliResponse{}
		response.Response.Data.Data = []MediaData{
			{
				FullTitle:        "Test Show",
				ParentMediaIndex: json.Number("1"),
				MediaIndex:       json.Number("2"),
				WatchedStatus:    1.0, // Marked as watched
				PercentComplete:  98,
			},
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("Error encoding response: %v", err)
		}
	}))
	defer tautulliServer.Close()

	// Set up the Tautulli API config
	if err := os.Setenv("API_HOST", strings.TrimPrefix(tautulliServer.URL, "http://")); err != nil {
		t.Fatalf("Failed to set environment variable API_HOST: %v", err)
	}
	if err := os.Setenv("API_KEY", "test-key"); err != nil {
		t.Fatalf("Failed to set environment variable API_KEY: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("API_HOST"); err != nil {
			t.Logf("Failed to unset environment variable API_HOST: %v", err)
		}
		if err := os.Unsetenv("API_KEY"); err != nil {
			t.Logf("Failed to unset environment variable API_KEY: %v", err)
		}
	}()

	// Test cases for routing
	testCases := []struct {
		name           string
		path           string
		contentType    string
		payload        interface{}
		expectedStatus int
		expectedFile   string
		shouldExist    bool
	}{
		{
			name:        "Plex webhook to /plex path",
			path:        "/plex",
			contentType: "multipart/form-data; boundary=X",
			payload: PlexWebhookPayload{
				Event: "media.stop",
				Metadata: struct {
					Key string `json:"key"`
				}{
					Key: "/library/metadata/12345",
				},
			},
			expectedStatus: http.StatusOK,
			expectedFile:   "Test Show - S1E2.json",
			shouldExist:    true,
		},
		{
			name:        "Jellyfin webhook to /jellyfin path",
			path:        "/jellyfin",
			contentType: "application/json",
			payload: JellyfinWebhookPayload{
				Event:    "playback.stop",
				ItemID:   "12345",
				ItemType: "Episode",
				MediaStatus: struct {
					PlaybackStatus     string `json:"PlaybackStatus"`
					PositionTicks      int64  `json:"PositionTicks"`
					IsPaused           bool   `json:"IsPaused"`
					PlayedToCompletion bool   `json:"PlayedToCompletion"`
				}{
					PlaybackStatus:     "Stopped",
					PositionTicks:      12345678,
					IsPaused:           false,
					PlayedToCompletion: true,
				},
				NotificationType: "PlaybackStop",
				Title:            "Test Episode",
				SeriesName:       "Test Series",
				SeasonNumber:     1,
				EpisodeNumber:    2,
			},
			expectedStatus: http.StatusOK,
			expectedFile:   "Test Series - S1E2.json",
			shouldExist:    true,
		},
		{
			name:        "Plex webhook to / path with multipart/form-data",
			path:        "/",
			contentType: "multipart/form-data; boundary=X",
			payload: PlexWebhookPayload{
				Event: "media.stop",
				Metadata: struct {
					Key string `json:"key"`
				}{
					Key: "/library/metadata/12345",
				},
			},
			expectedStatus: http.StatusOK,
			expectedFile:   "Test Show - S1E2.json",
			shouldExist:    true,
		},
		{
			name:        "Jellyfin webhook to / path with application/json",
			path:        "/",
			contentType: "application/json",
			payload: JellyfinWebhookPayload{
				Event:    "playback.stop",
				ItemID:   "12345",
				ItemType: "Episode",
				MediaStatus: struct {
					PlaybackStatus     string `json:"PlaybackStatus"`
					PositionTicks      int64  `json:"PositionTicks"`
					IsPaused           bool   `json:"IsPaused"`
					PlayedToCompletion bool   `json:"PlayedToCompletion"`
				}{
					PlaybackStatus:     "Stopped",
					PositionTicks:      12345678,
					IsPaused:           false,
					PlayedToCompletion: true,
				},
				NotificationType: "PlaybackStop",
				Title:            "Test Episode",
				SeriesName:       "Test Series",
				SeasonNumber:     1,
				EpisodeNumber:    2,
			},
			expectedStatus: http.StatusOK,
			expectedFile:   "Test Series - S1E2.json",
			shouldExist:    true,
		},
		{
			name:           "Unknown path",
			path:           "/unknown",
			contentType:    "application/json",
			payload:        nil,
			expectedStatus: http.StatusNotFound,
			expectedFile:   "",
			shouldExist:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Remove any existing files from previous test cases
			files, err := os.ReadDir(tempDir)
			if err != nil {
				t.Fatalf("Error reading temp dir: %v", err)
			}
			for _, file := range files {
				if err := os.Remove(filepath.Join(tempDir, file.Name())); err != nil {
					t.Fatalf("Error removing file: %v", err)
				}
			}

			// Create a request
			var req *http.Request

			if tc.payload != nil {
				if strings.Contains(tc.contentType, "multipart/form-data") {
					// For Plex, create a multipart form request
					payloadBytes, err := json.Marshal(tc.payload)
					if err != nil {
						t.Fatalf("Error marshaling payload: %v", err)
					}
					body := strings.NewReader("--X\r\nContent-Disposition: form-data; name=\"payload\"\r\n\r\n" + string(payloadBytes) + "\r\n--X--\r\n")
					req = httptest.NewRequest("POST", tc.path, body)
				} else {
					// For Jellyfin, create a JSON request
					payloadBytes, err := json.Marshal(tc.payload)
					if err != nil {
						t.Fatalf("Error marshaling payload: %v", err)
					}
					req = httptest.NewRequest("POST", tc.path, strings.NewReader(string(payloadBytes)))
				}
				req.Header.Set("Content-Type", tc.contentType)
			} else {
				req = httptest.NewRequest("GET", tc.path, nil)
			}

			// Create a response recorder
			rr := httptest.NewRecorder()

			// Create the handler
			config := loadConfig()
			mux := http.NewServeMux()

			// Set up the routes
			mux.HandleFunc("/plex", func(w http.ResponseWriter, r *http.Request) {
				handlePlexWebhook(w, r, config)
			})

			mux.HandleFunc("/jellyfin", func(w http.ResponseWriter, r *http.Request) {
				handleJellyfinWebhook(w, r, config)
			})

			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				// If the path is exactly "/", try to detect the webhook type from the content
				if r.URL.Path == "/" {
					contentType := r.Header.Get("Content-Type")

					// Plex webhooks are typically sent as multipart/form-data
					if strings.Contains(contentType, "multipart/form-data") {
						handlePlexWebhook(w, r, config)
						return
					}

					// Jellyfin webhooks are typically sent as application/json
					if strings.Contains(contentType, "application/json") {
						handleJellyfinWebhook(w, r, config)
						return
					}

					// If we can't determine the type, return an error
					http.Error(w, "Unable to determine webhook type", http.StatusBadRequest)
					return
				}

				// For any other path, return 404
				http.NotFound(w, r)
			})

			// Serve the request
			mux.ServeHTTP(rr, req)

			// Check the response status
			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", status, tc.expectedStatus)
			}

			// If we expect a file to be created, check it
			if tc.expectedFile != "" {
				expectedFilePath := filepath.Join(tempDir, tc.expectedFile)
				fileExists := true
				if _, err := os.Stat(expectedFilePath); os.IsNotExist(err) {
					fileExists = false
				}

				if fileExists != tc.shouldExist {
					if tc.shouldExist {
						t.Errorf("Expected file %s to exist, but it doesn't", expectedFilePath)
					} else {
						t.Errorf("Expected file %s not to exist, but it does", expectedFilePath)
					}
				}
			}
		})
	}
}
