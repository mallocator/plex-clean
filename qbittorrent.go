package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Qbittorrent is a minimal client for the qBittorrent WebUI API.
type Qbittorrent struct {
	BaseURL  string
	Username string
	Password string
	Client   *http.Client
}

type Torrent struct {
	Hash       string `json:"hash"`
	Name       string `json:"name"`
	AmountLeft int64  `json:"amount_left"`
	State      string `json:"state"`
	Category   string `json:"category"`
	SavePath   string `json:"save_path"`
}

type TorrentFile struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

func NewQbittorrent(baseURL, user, pass string) *Qbittorrent {
	jar, _ := cookiejar.New(nil)
	return &Qbittorrent{BaseURL: baseURL, Username: user, Password: pass, Client: &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		// qBittorrent's WebUI typically uses a self-signed certificate on the LAN.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
}

// Login authenticates if credentials are configured (subnets on qBittorrent's auth whitelist need none).
// qBittorrent 5 answers a successful login with 204 No Content.
func (q *Qbittorrent) Login() error {
	if q.Username == "" {
		return nil
	}
	resp, err := q.Client.PostForm(q.BaseURL+"/api/v2/auth/login", url.Values{"username": {q.Username}, "password": {q.Password}})
	if err != nil {
		return fmt.Errorf("qbittorrent login: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
	if !isSuccess(resp.StatusCode) || strings.TrimSpace(string(body)) == "Fails." {
		return fmt.Errorf("qbittorrent login failed: %s", resp.Status)
	}
	return nil
}

func (q *Qbittorrent) get(path string, out any) error {
	resp, err := q.Client.Get(q.BaseURL + path)
	if err != nil {
		return fmt.Errorf("qbittorrent GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return fmt.Errorf("qbittorrent GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (q *Qbittorrent) Torrents() ([]Torrent, error) {
	var t []Torrent
	return t, q.get("/api/v2/torrents/info", &t)
}

func (q *Qbittorrent) Files(hash string) ([]TorrentFile, error) {
	var f []TorrentFile
	return f, q.get("/api/v2/torrents/files?hash="+url.QueryEscape(hash), &f)
}

// Remove deletes a torrent together with whatever is left of its files.
func (q *Qbittorrent) Remove(hash string) error {
	resp, err := q.Client.PostForm(q.BaseURL+"/api/v2/torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {"true"}})
	if err != nil {
		return fmt.Errorf("qbittorrent delete: %w", err)
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return fmt.Errorf("qbittorrent delete: %s", resp.Status)
	}
	return nil
}
