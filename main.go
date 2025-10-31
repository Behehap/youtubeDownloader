package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kkdai/youtube/v2"
)

type DownloadConfig struct {
	OutputDir    string
	DownloadType string
	MaxQuality   string
	Parallel     int
}

type PlaylistDownloader struct {
	client   youtube.Client
	config   DownloadConfig
	progress map[string]bool
	mutex    sync.RWMutex
}

func NewPlaylistDownloader(config DownloadConfig) *PlaylistDownloader {
	return &PlaylistDownloader{
		client:   youtube.Client{},
		config:   config,
		progress: make(map[string]bool),
	}
}

func (pd *PlaylistDownloader) DownloadPlaylist(playlistURL string) error {
	fmt.Printf("Fetching playlist info: %s\n", playlistURL)

	playlist, err := pd.client.GetPlaylist(playlistURL)
	if err != nil {
		return fmt.Errorf("failed to get playlist: %w", err)
	}

	fmt.Printf("Playlist: %s\n", playlist.Title)
	fmt.Printf("Videos: %d\n", len(playlist.Videos))

	// Create output directory
	if err := os.MkdirAll(pd.config.OutputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Download videos with limited parallelism
	semaphore := make(chan struct{}, pd.config.Parallel)
	var wg sync.WaitGroup
	var failedDownloads []string

	for i, video := range playlist.Videos {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(index int, v *youtube.PlaylistEntry) {
			defer wg.Done()
			defer func() { <-semaphore }()

			fmt.Printf("\n[%d/%d] Downloading: %s\n", index+1, len(playlist.Videos), v.Title)

			start := time.Now()
			err := pd.DownloadVideo(v.ID)
			elapsed := time.Since(start)

			if err != nil {
				log.Printf("❌ Failed to download %s: %v", v.Title, err)
				failedDownloads = append(failedDownloads, v.Title)
			} else {
				fmt.Printf("✓ Completed in %v: %s\n", elapsed.Round(time.Second), v.Title)
			}
		}(i, video)
	}

	wg.Wait()

	// Print summary
	fmt.Printf("\n=== Download Summary ===\n")
	fmt.Printf("Total videos: %d\n", len(playlist.Videos))
	fmt.Printf("Successful: %d\n", len(playlist.Videos)-len(failedDownloads))
	fmt.Printf("Failed: %d\n", len(failedDownloads))

	if len(failedDownloads) > 0 {
		fmt.Printf("Failed videos:\n")
		for _, title := range failedDownloads {
			fmt.Printf("  - %s\n", title)
		}
	}

	return nil
}

func (pd *PlaylistDownloader) DownloadVideo(videoID string) error {
	video, err := pd.client.GetVideo(videoID)
	if err != nil {
		return fmt.Errorf("failed to get video info: %w", err)
	}

	format := pd.selectFormat(video)
	if format == nil {
		return fmt.Errorf("no suitable format found")
	}

	fileName := sanitizeFileName(video.Title)
	fileExt := pd.getFileExtension(format)
	filePath := filepath.Join(pd.config.OutputDir, fileName+fileExt)

	stream, _, err := pd.client.GetStream(video, format)
	if err != nil {
		return fmt.Errorf("failed to get stream: %w", err)
	}
	defer stream.Close()

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	_, err = file.ReadFrom(stream)
	if err != nil {
		return fmt.Errorf("failed to save video: %w", err)
	}

	return nil
}

func (pd *PlaylistDownloader) selectFormat(video *youtube.Video) *youtube.Format {
	switch pd.config.DownloadType {
	case "audio":
		return pd.selectBestAudioFormat(video)
	case "video":
		return pd.selectBestVideoFormat(video)
	default:
		return &video.Formats[0]
	}
}

func (pd *PlaylistDownloader) selectBestAudioFormat(video *youtube.Video) *youtube.Format {
	// Look for audio-only formats first
	for i := range video.Formats {
		if strings.HasPrefix(video.Formats[i].MimeType, "audio/") {
			return &video.Formats[i]
		}
	}
	// Fallback to any format with audio
	for i := range video.Formats {
		if video.Formats[i].AudioChannels > 0 {
			return &video.Formats[i]
		}
	}
	return nil
}

func (pd *PlaylistDownloader) selectBestVideoFormat(video *youtube.Video) *youtube.Format {
	var bestFormat *youtube.Format

	for i := range video.Formats {
		format := &video.Formats[i]

		if strings.HasPrefix(format.MimeType, "video/") {
			// Use Quality label or check if it has video
			if bestFormat == nil || format.Quality != "tiny" {
				bestFormat = format
			}
		}
	}

	return bestFormat
}

func (pd *PlaylistDownloader) getFileExtension(format *youtube.Format) string {
	if pd.config.DownloadType == "audio" {
		return ".mp3"
	}

	switch {
	case strings.Contains(format.MimeType, "mp4"):
		return ".mp4"
	case strings.Contains(format.MimeType, "webm"):
		return ".webm"
	default:
		return ".mp4"
	}
}

func sanitizeFileName(name string) string {
	invalidChars := []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|"}
	for _, char := range invalidChars {
		name = strings.ReplaceAll(name, char, "_")
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return strings.TrimSpace(name)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	playlistURL := os.Args[1]
	config := DownloadConfig{
		OutputDir:    "./downloads",
		DownloadType: "video",
		Parallel:     3,
	}

	// Parse command line arguments
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-o", "--output":
			if i+1 < len(os.Args) {
				config.OutputDir = os.Args[i+1]
				i++
			}
		case "-t", "--type":
			if i+1 < len(os.Args) {
				config.DownloadType = os.Args[i+1]
				i++
			}
		case "-p", "--parallel":
			if i+1 < len(os.Args) {
				if parallel, err := strconv.Atoi(os.Args[i+1]); err == nil {
					config.Parallel = parallel
				}
				i++
			}
		}
	}

	// Validate download type
	if config.DownloadType != "audio" && config.DownloadType != "video" {
		fmt.Println("Error: Download type must be 'audio' or 'video'")
		os.Exit(1)
	}

	downloader := NewPlaylistDownloader(config)

	fmt.Printf("Starting download with configuration:\n")
	fmt.Printf("  Output directory: %s\n", config.OutputDir)
	fmt.Printf("  Download type: %s\n", config.DownloadType)
	fmt.Printf("  Parallel downloads: %d\n", config.Parallel)
	fmt.Println()

	if err := downloader.DownloadPlaylist(playlistURL); err != nil {
		log.Fatal(err)
	}
}

func printUsage() {
	fmt.Println("YouTube Playlist Downloader")
	fmt.Println("Usage: go run main.go <playlist_url> [options]")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  -o, --output DIR    Output directory (default: ./downloads)")
	fmt.Println("  -t, --type TYPE     Download type: audio or video (default: video)")
	fmt.Println("  -p, --parallel N    Number of parallel downloads (default: 3)")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  go run main.go https://youtube.com/playlist?list=PL...")
	fmt.Println("  go run main.go https://youtube.com/playlist?list=PL... -o ./music -t audio")
	fmt.Println("  go run main.go https://youtube.com/playlist?list=PL... -p 5 -t video")
}
