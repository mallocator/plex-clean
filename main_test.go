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
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if the request URL contains the expected parameters
		if !strings.Contains(r.URL.String(), "cmd=get_history") ||
			!strings.Contains(r.URL.String(), "rating_key=12345") {
			t.Errorf("Unexpected request URL: %s", r.URL.String())
		}

		// Return a mock response
		response := TautulliResponse{}
		response.Response.Data.Data = []MediaData{
			{
				FullTitle:        "Test Show - Test Episode",
				ParentMediaIndex: json.Number("1"), // Fixed to use json.Number
				MediaIndex:       json.Number("2"), // Fixed to use json.Number
				WatchedStatus:    1.0,
				PercentComplete:  98,
			},
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
				ParentMediaIndex: json.Number("1"), // Fixed to use json.Number
				MediaIndex:       json.Number("2"), // Fixed to use json.Number
				WatchedStatus:    1.0,              // Marked as watched
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
