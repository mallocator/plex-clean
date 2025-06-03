package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetEnv(t *testing.T) {
	// Test with existing environment variable
	if err := os.Setenv("TEST_VAR", "test_value"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("TEST_VAR"); err != nil {
			t.Logf("Failed to unset environment variable: %v", err)
		}
	}()

	if val := getEnv("TEST_VAR", "default"); val != "test_value" {
		t.Errorf("getEnv returned %s, expected test_value", val)
	}

	// Test with non-existing environment variable
	if val := getEnv("NON_EXISTING_VAR", "default"); val != "default" {
		t.Errorf("getEnv returned %s, expected default", val)
	}
}

func TestLoadConfig(t *testing.T) {
	// Set environment variables for testing
	if err := os.Setenv("PORT", "8080"); err != nil {
		t.Fatalf("Failed to set environment variable PORT: %v", err)
	}
	if err := os.Setenv("API_HOST", "test-host"); err != nil {
		t.Fatalf("Failed to set environment variable API_HOST: %v", err)
	}
	if err := os.Setenv("API_KEY", "test-key"); err != nil {
		t.Fatalf("Failed to set environment variable API_KEY: %v", err)
	}
	if err := os.Setenv("OUTPUT_DIR", "/test-output"); err != nil {
		t.Fatalf("Failed to set environment variable OUTPUT_DIR: %v", err)
	}
	if err := os.Setenv("DEBUG", "true"); err != nil {
		t.Fatalf("Failed to set environment variable DEBUG: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("PORT"); err != nil {
			t.Logf("Failed to unset environment variable PORT: %v", err)
		}
		if err := os.Unsetenv("API_HOST"); err != nil {
			t.Logf("Failed to unset environment variable API_HOST: %v", err)
		}
		if err := os.Unsetenv("API_KEY"); err != nil {
			t.Logf("Failed to unset environment variable API_KEY: %v", err)
		}
		if err := os.Unsetenv("OUTPUT_DIR"); err != nil {
			t.Logf("Failed to unset environment variable OUTPUT_DIR: %v", err)
		}
		if err := os.Unsetenv("DEBUG"); err != nil {
			t.Logf("Failed to unset environment variable DEBUG: %v", err)
		}
	}()

	config := loadConfig()

	if config.Port != 8080 {
		t.Errorf("config.Port = %d, expected 8080", config.Port)
	}
	if config.APIHost != "test-host" {
		t.Errorf("config.APIHost = %s, expected test-host", config.APIHost)
	}
	if config.APIKey != "test-key" {
		t.Errorf("config.APIKey = %s, expected test-key", config.APIKey)
	}
	if config.OutputDir != "/test-output" {
		t.Errorf("config.OutputDir = %s, expected /test-output", config.OutputDir)
	}
	if !config.Debug {
		t.Errorf("config.Debug = %v, expected true", config.Debug)
	}
}

func TestFetchMetadata(t *testing.T) {
	// This test verifies that the fetchMetadata function correctly handles various edge cases
	// in the JSON response from the Tautulli API, including:
	// - Normal responses with valid numbers
	// - Empty strings for number fields (ParentMediaIndex, MediaIndex)
	// - Empty strings for other numeric fields (WatchedStatus, PercentComplete)
	// - Null values in JSON fields
	// - Missing fields in JSON response
	// - Different spacing patterns in JSON
	// - Malformed JSON responses

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if the request URL contains the expected parameters
		if !strings.Contains(r.URL.String(), "cmd=get_history") {
			t.Errorf("Unexpected request URL: %s", r.URL.String())
		}

		// Return different mock responses based on the rating_key
		response := TautulliResponse{}

		if strings.Contains(r.URL.String(), "rating_key=12345") {
			// Normal case with valid numbers
			response.Response.Data.Data = []MediaData{
				{
					FullTitle:        "Test Show - Test Episode",
					ParentMediaIndex: json.Number("1"),
					MediaIndex:       json.Number("2"),
					WatchedStatus:    1.0,
					PercentComplete:  98,
				},
			}
		} else if strings.Contains(r.URL.String(), "rating_key=67890") {
			// Case with empty strings for number fields
			// This simulates the error case we're fixing

			// We need to manually create the JSON response since our struct won't allow us to set empty strings
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"response": {
					"data": {
						"data": [
							{
								"full_title": "Test Show - Empty Numbers",
								"parent_media_index": "",
								"media_index": "",
								"watched_status": 1.0,
								"percent_complete": 98
							}
						]
					}
				}
			}`))
			return
		} else if strings.Contains(r.URL.String(), "rating_key=11111") {
			// Case with empty strings for other numeric fields (WatchedStatus, PercentComplete)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"response": {
					"data": {
						"data": [
							{
								"full_title": "Test Show - Empty Other Numbers",
								"parent_media_index": "3",
								"media_index": "4",
								"watched_status": "",
								"percent_complete": ""
							}
						]
					}
				}
			}`))
			return
		} else if strings.Contains(r.URL.String(), "rating_key=22222") {
			// Case with null values in JSON fields
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"response": {
					"data": {
						"data": [
							{
								"full_title": "Test Show - Null Values",
								"parent_media_index": null,
								"media_index": null,
								"watched_status": null,
								"percent_complete": null
							}
						]
					}
				}
			}`))
			return
		} else if strings.Contains(r.URL.String(), "rating_key=33333") {
			// Case with missing fields in JSON response
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"response": {
					"data": {
						"data": [
							{
								"full_title": "Test Show - Missing Fields"
							}
						]
					}
				}
			}`))
			return
		} else if strings.Contains(r.URL.String(), "rating_key=44444") {
			// Case with different spacing patterns in JSON
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"response": {
					"data": {
						"data": [
							{
								"full_title":"Test Show - Different Spacing",
								"parent_media_index":"",
								"media_index" : "",
								"watched_status" : 1.0,
								"percent_complete":98
							}
						]
					}
				}
			}`))
			return
		} else if strings.Contains(r.URL.String(), "rating_key=55555") {
			// Case with malformed JSON response
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"response": {
					"data": {
						"data": [
							{
								"full_title": "Test Show - Malformed JSON",
								"parent_media_index": "5",
								"media_index": "6",
								"watched_status": 1.0,
								"percent_complete": 98
							}
						]
					}
				}`)) // Missing closing brace
			return
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("Error encoding response: %v", err)
		}
	}))
	defer server.Close()

	// Create a test config
	config := Config{
		APIHost: strings.TrimPrefix(server.URL, "http://"),
		APIKey:  "test-key",
	}

	// Test with a valid path
	mediaData, err := fetchMetadata("/library/metadata/12345", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 1 {
		t.Errorf("fetchMetadata returned %d items, expected 1", len(mediaData))
	}
	if mediaData[0].WatchedStatus != 1.0 {
		t.Errorf("mediaData[0].WatchedStatus = %f, expected 1.0", mediaData[0].WatchedStatus)
	}

	// Test with an empty path
	mediaData, err = fetchMetadata("", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 0 {
		t.Errorf("fetchMetadata returned %d items, expected 0", len(mediaData))
	}

	// Test with a path that doesn't contain "/library/metadata/"
	mediaData, err = fetchMetadata("/some/other/path", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 0 {
		t.Errorf("fetchMetadata returned %d items, expected 0", len(mediaData))
	}

	// Test with a path that would return empty strings for number fields
	mediaData, err = fetchMetadata("/library/metadata/67890", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 1 {
		t.Errorf("fetchMetadata returned %d items, expected 1", len(mediaData))
	} else {
		// Check that the empty strings were handled correctly
		if mediaData[0].FullTitle != "Test Show - Empty Numbers" {
			t.Errorf("mediaData[0].FullTitle = %s, expected Test Show - Empty Numbers", mediaData[0].FullTitle)
		}
		// The empty strings should have been converted to 0
		parentMediaIndex, err := mediaData[0].ParentMediaIndex.Int64()
		if err != nil {
			t.Errorf("Error converting ParentMediaIndex to int: %v", err)
		}
		if parentMediaIndex != 0 {
			t.Errorf("mediaData[0].ParentMediaIndex = %d, expected 0", parentMediaIndex)
		}
		mediaIndex, err := mediaData[0].MediaIndex.Int64()
		if err != nil {
			t.Errorf("Error converting MediaIndex to int: %v", err)
		}
		if mediaIndex != 0 {
			t.Errorf("mediaData[0].MediaIndex = %d, expected 0", mediaIndex)
		}
	}

	// Test with a path that would return empty strings for other numeric fields (WatchedStatus, PercentComplete)
	mediaData, err = fetchMetadata("/library/metadata/11111", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 1 {
		t.Errorf("fetchMetadata returned %d items, expected 1", len(mediaData))
	} else {
		// Check that the empty strings were handled correctly
		if mediaData[0].FullTitle != "Test Show - Empty Other Numbers" {
			t.Errorf("mediaData[0].FullTitle = %s, expected Test Show - Empty Other Numbers", mediaData[0].FullTitle)
		}
		// Check that the numeric fields are set correctly
		parentMediaIndex, err := mediaData[0].ParentMediaIndex.Int64()
		if err != nil {
			t.Errorf("Error converting ParentMediaIndex to int: %v", err)
		}
		if parentMediaIndex != 3 {
			t.Errorf("mediaData[0].ParentMediaIndex = %d, expected 3", parentMediaIndex)
		}
		mediaIndex, err := mediaData[0].MediaIndex.Int64()
		if err != nil {
			t.Errorf("Error converting MediaIndex to int: %v", err)
		}
		if mediaIndex != 4 {
			t.Errorf("mediaData[0].MediaIndex = %d, expected 4", mediaIndex)
		}
		// Empty strings for WatchedStatus and PercentComplete should be handled by Go's default zero values
		if mediaData[0].WatchedStatus != 0 {
			t.Errorf("mediaData[0].WatchedStatus = %f, expected 0", mediaData[0].WatchedStatus)
		}
		if mediaData[0].PercentComplete != 0 {
			t.Errorf("mediaData[0].PercentComplete = %d, expected 0", mediaData[0].PercentComplete)
		}
	}

	// Test with a path that would return null values in JSON fields
	mediaData, err = fetchMetadata("/library/metadata/22222", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 1 {
		t.Errorf("fetchMetadata returned %d items, expected 1", len(mediaData))
	} else {
		// Check that the null values were handled correctly
		if mediaData[0].FullTitle != "Test Show - Null Values" {
			t.Errorf("mediaData[0].FullTitle = %s, expected Test Show - Null Values", mediaData[0].FullTitle)
		}
		// Null values for ParentMediaIndex and MediaIndex should be handled by json.Number
		// For null values, the ParentMediaIndex and MediaIndex should be empty strings
		if mediaData[0].ParentMediaIndex != "" {
			t.Errorf("mediaData[0].ParentMediaIndex = %s, expected empty string", mediaData[0].ParentMediaIndex)
		}
		if mediaData[0].MediaIndex != "" {
			t.Errorf("mediaData[0].MediaIndex = %s, expected empty string", mediaData[0].MediaIndex)
		}
		// Null values for WatchedStatus and PercentComplete should be handled by Go's default zero values
		if mediaData[0].WatchedStatus != 0 {
			t.Errorf("mediaData[0].WatchedStatus = %f, expected 0", mediaData[0].WatchedStatus)
		}
		if mediaData[0].PercentComplete != 0 {
			t.Errorf("mediaData[0].PercentComplete = %d, expected 0", mediaData[0].PercentComplete)
		}
	}

	// Test with a path that would return missing fields in JSON response
	mediaData, err = fetchMetadata("/library/metadata/33333", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 1 {
		t.Errorf("fetchMetadata returned %d items, expected 1", len(mediaData))
	} else {
		// Check that the missing fields were handled correctly
		if mediaData[0].FullTitle != "Test Show - Missing Fields" {
			t.Errorf("mediaData[0].FullTitle = %s, expected Test Show - Missing Fields", mediaData[0].FullTitle)
		}
		// Missing fields should be handled by Go's default zero values
		if mediaData[0].ParentMediaIndex != "" {
			t.Errorf("mediaData[0].ParentMediaIndex = %s, expected empty string", mediaData[0].ParentMediaIndex)
		}
		if mediaData[0].MediaIndex != "" {
			t.Errorf("mediaData[0].MediaIndex = %s, expected empty string", mediaData[0].MediaIndex)
		}
		if mediaData[0].WatchedStatus != 0 {
			t.Errorf("mediaData[0].WatchedStatus = %f, expected 0", mediaData[0].WatchedStatus)
		}
		if mediaData[0].PercentComplete != 0 {
			t.Errorf("mediaData[0].PercentComplete = %d, expected 0", mediaData[0].PercentComplete)
		}
	}

	// Test with a path that would return different spacing patterns in JSON
	mediaData, err = fetchMetadata("/library/metadata/44444", config)
	if err != nil {
		t.Errorf("fetchMetadata returned error: %v", err)
	}
	if len(mediaData) != 1 {
		t.Errorf("fetchMetadata returned %d items, expected 1", len(mediaData))
	} else {
		// Check that the different spacing patterns were handled correctly
		if mediaData[0].FullTitle != "Test Show - Different Spacing" {
			t.Errorf("mediaData[0].FullTitle = %s, expected Test Show - Different Spacing", mediaData[0].FullTitle)
		}
		// The empty strings should have been converted to 0
		parentMediaIndex, err := mediaData[0].ParentMediaIndex.Int64()
		if err != nil {
			t.Errorf("Error converting ParentMediaIndex to int: %v", err)
		}
		if parentMediaIndex != 0 {
			t.Errorf("mediaData[0].ParentMediaIndex = %d, expected 0", parentMediaIndex)
		}
		mediaIndex, err := mediaData[0].MediaIndex.Int64()
		if err != nil {
			t.Errorf("Error converting MediaIndex to int: %v", err)
		}
		if mediaIndex != 0 {
			t.Errorf("mediaData[0].MediaIndex = %d, expected 0", mediaIndex)
		}
		if mediaData[0].WatchedStatus != 1.0 {
			t.Errorf("mediaData[0].WatchedStatus = %f, expected 1.0", mediaData[0].WatchedStatus)
		}
		if mediaData[0].PercentComplete != 98 {
			t.Errorf("mediaData[0].PercentComplete = %d, expected 98", mediaData[0].PercentComplete)
		}
	}

	// Test with a path that would return malformed JSON response
	mediaData, err = fetchMetadata("/library/metadata/55555", config)
	if err == nil {
		t.Errorf("fetchMetadata did not return an error for malformed JSON")
	} else {
		// Check that the error message contains "error unmarshaling response"
		if !strings.Contains(err.Error(), "error unmarshaling response") {
			t.Errorf("Expected error message to contain 'error unmarshaling response', got: %v", err)
		}
	}
}

func TestJellyfinWebhookHandler(t *testing.T) {
	// Create a temporary directory for output
	tempDir, err := os.MkdirTemp("", "test-jellyfin-output")
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

	// Test cases for Jellyfin webhook
	testCases := []struct {
		name           string
		payload        JellyfinWebhookPayload
		expectedStatus int
		expectedFile   string
		shouldExist    bool
	}{
		{
			name: "Episode played to completion",
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
			name: "Movie played to completion",
			payload: JellyfinWebhookPayload{
				Event:    "playback.stop",
				ItemID:   "67890",
				ItemType: "Movie",
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
				Title:            "Test Movie",
			},
			expectedStatus: http.StatusOK,
			expectedFile:   "Test Movie.json",
			shouldExist:    true,
		},
		{
			name: "Episode not played to completion",
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
					PlayedToCompletion: false,
				},
				NotificationType: "PlaybackStop",
				Title:            "Test Episode",
				SeriesName:       "Test Series",
				SeasonNumber:     1,
				EpisodeNumber:    2,
			},
			expectedStatus: http.StatusOK,
			expectedFile:   "Test Series - S1E2.json",
			shouldExist:    false,
		},
		{
			name: "Non-playback stop event",
			payload: JellyfinWebhookPayload{
				Event:    "playback.start",
				ItemID:   "12345",
				ItemType: "Episode",
				MediaStatus: struct {
					PlaybackStatus     string `json:"PlaybackStatus"`
					PositionTicks      int64  `json:"PositionTicks"`
					IsPaused           bool   `json:"IsPaused"`
					PlayedToCompletion bool   `json:"PlayedToCompletion"`
				}{
					PlaybackStatus:     "Playing",
					PositionTicks:      0,
					IsPaused:           false,
					PlayedToCompletion: false,
				},
				NotificationType: "PlaybackStart",
				Title:            "Test Episode",
				SeriesName:       "Test Series",
				SeasonNumber:     1,
				EpisodeNumber:    2,
			},
			expectedStatus: http.StatusOK,
			expectedFile:   "Test Series - S1E2.json",
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

			// Create a request with the test payload
			payloadBytes, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("Error marshaling payload: %v", err)
			}

			req := httptest.NewRequest("POST", "/jellyfin", strings.NewReader(string(payloadBytes)))
			req.Header.Set("Content-Type", "application/json")

			// Create a response recorder
			rr := httptest.NewRecorder()

			// Create the handler
			config := loadConfig()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handleJellyfinWebhook(w, r, config)
			})

			// Serve the request
			handler.ServeHTTP(rr, req)

			// Check the response status
			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", status, tc.expectedStatus)
			}

			// Check if the file exists or not as expected
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

			// If the file should exist, check its content
			if tc.shouldExist && fileExists {
				fileContent, err := os.ReadFile(expectedFilePath)
				if err != nil {
					t.Fatalf("Error reading file: %v", err)
				}

				var fileData MediaData
				if err := json.Unmarshal(fileContent, &fileData); err != nil {
					t.Fatalf("Error unmarshaling file content: %v", err)
				}

				if fileData.WatchedStatus < 1.0 {
					t.Errorf("fileData.WatchedStatus = %f, expected >= 1.0", fileData.WatchedStatus)
				}
				if fileData.PercentComplete != 100 {
					t.Errorf("fileData.PercentComplete = %d, expected 100", fileData.PercentComplete)
				}
			}
		})
	}
}

func TestWebhookHandler(t *testing.T) {
	// Create a temporary directory for output
	tempDir, err := os.MkdirTemp("", "test-output")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Failed to remove temp dir: %v", err)
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

	// Set up the config
	if err := os.Setenv("API_HOST", strings.TrimPrefix(tautulliServer.URL, "http://")); err != nil {
		t.Fatalf("Failed to set environment variable API_HOST: %v", err)
	}
	if err := os.Setenv("API_KEY", "test-key"); err != nil {
		t.Fatalf("Failed to set environment variable API_KEY: %v", err)
	}
	if err := os.Setenv("OUTPUT_DIR", tempDir); err != nil {
		t.Fatalf("Failed to set environment variable OUTPUT_DIR: %v", err)
	}
	defer func() {
		if err := os.Unsetenv("API_HOST"); err != nil {
			t.Logf("Failed to unset environment variable API_HOST: %v", err)
		}
		if err := os.Unsetenv("API_KEY"); err != nil {
			t.Logf("Failed to unset environment variable API_KEY: %v", err)
		}
		if err := os.Unsetenv("OUTPUT_DIR"); err != nil {
			t.Logf("Failed to unset environment variable OUTPUT_DIR: %v", err)
		}
	}()

	// Create a test request with a valid payload
	payload := PlexWebhookPayload{
		Event: "media.stop",
		Metadata: struct {
			Key string `json:"key"`
		}{
			Key: "/library/metadata/12345",
		},
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Error marshaling payload: %v", err)
	}

	// Create a multipart form request
	body := strings.NewReader("--X\r\nContent-Disposition: form-data; name=\"payload\"\r\n\r\n" + string(payloadBytes) + "\r\n--X--\r\n")
	req := httptest.NewRequest("POST", "/", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=X")

	// Create a response recorder
	rr := httptest.NewRecorder()

	// Create the handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse multipart form
		err := r.ParseMultipartForm(10 << 20)
		if err != nil {
			t.Fatalf("Error parsing multipart form: %v", err)
		}

		// Get payload from form
		payloadStr := r.FormValue("payload")
		if payloadStr == "" {
			t.Fatalf("No payload found in request")
		}

		// Parse payload
		var p PlexWebhookPayload
		if err := json.Unmarshal([]byte(payloadStr), &p); err != nil {
			t.Fatalf("Error unmarshaling payload: %v", err)
		}

		// Fetch metadata
		config := loadConfig()
		mediaData, err := fetchMetadata(p.Metadata.Key, config)
		if err != nil {
			t.Fatalf("Error fetching metadata: %v", err)
		}

		// Process media data
		for _, data := range mediaData {
			if data.WatchedStatus >= 1.0 {
				// Convert ParentMediaIndex and MediaIndex to integers
				parentMediaIndex, err := data.ParentMediaIndex.Int64()
				if err != nil {
					t.Fatalf("Error converting ParentMediaIndex to int: %v", err)
				}
				mediaIndex, err := data.MediaIndex.Int64()
				if err != nil {
					t.Fatalf("Error converting MediaIndex to int: %v", err)
				}

				filename := fmt.Sprintf("%s - S%dE%d.json", data.FullTitle, parentMediaIndex, mediaIndex)

				// Create the output directory if it doesn't exist
				if err := os.MkdirAll(config.OutputDir, 0755); err != nil {
					t.Fatalf("Error creating output directory: %v", err)
				}

				// Write the data to a file
				jsonData, err := json.MarshalIndent(data, "", "  ")
				if err != nil {
					t.Fatalf("Error marshaling JSON: %v", err)
				}

				outputPath := filepath.Join(config.OutputDir, filename)
				if err := os.WriteFile(outputPath, jsonData, 0644); err != nil {
					t.Fatalf("Error writing file: %v", err)
				}
			}
		}

		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("OK"))
		if err != nil {
			t.Fatalf("Error writing response: %v", err)
		}
	})

	// Serve the request
	handler.ServeHTTP(rr, req)

	// Check the response
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	// Check if the file was created
	expectedFilePath := filepath.Join(tempDir, "Test Show - S1E2.json")
	if _, err := os.Stat(expectedFilePath); os.IsNotExist(err) {
		t.Errorf("Expected file %s was not created", expectedFilePath)
	}

	// Check the content of the file
	fileContent, err := os.ReadFile(expectedFilePath)
	if err != nil {
		t.Fatalf("Error reading file: %v", err)
	}

	var fileData MediaData
	if err := json.Unmarshal(fileContent, &fileData); err != nil {
		t.Fatalf("Error unmarshaling file content: %v", err)
	}

	if fileData.WatchedStatus < 1.0 {
		t.Errorf("fileData.WatchedStatus = %f, expected >= 1.0", fileData.WatchedStatus)
	}
	if fileData.PercentComplete != 98 {
		t.Errorf("fileData.PercentComplete = %d, expected 98", fileData.PercentComplete)
	}
}
