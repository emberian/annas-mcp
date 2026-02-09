package anna

import (
	"fmt"
	"net/url"

	"strings"
	"sync"
	"time"

	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	colly "github.com/gocolly/colly/v2"
	"github.com/iosifache/annas-mcp/internal/env"
	"github.com/iosifache/annas-mcp/internal/logger"
	"go.uber.org/zap"
)

const (
	AnnasSearchEndpointFormat = "https://%s/search?q=%s&content=%s"
	AnnasSciDBEndpointFormat    = "https://%s/scidb/%s"
	AnnasDownloadEndpointFormat = "https://%s/dyn/api/fast_download.json?md5=%s&key=%s"
	HTTPTimeout                 = 30 * time.Second
)

var (
	// Regex to sanitize filenames - removes dangerous characters
	unsafeFilenameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
)

func extractMetaInformation(meta string) (language, format, size string) {
	// The meta format may be:
	// - "✅ English [en] · EPUB · 0.7MB · 2015 · ..."
	// - "✅ English [en] · Hindi [hi] · EPUB · 0.7MB · ..."
	parts := strings.Split(meta, " · ")
	if len(parts) < 3 {
		return "", "", ""
	}

	// Extract language from first part
	languagePart := strings.TrimSpace(parts[0])
	if idx := strings.Index(languagePart, "["); idx > 0 {
		language = strings.TrimSpace(languagePart[:idx])
		// Remove checkmark and leading spaces properly
		language = strings.TrimPrefix(language, "✅")
		language = strings.TrimSpace(language)
	}

	// Common ebook formats (case-insensitive search)
	formatRegex := regexp.MustCompile(`(?i)\b(EPUB|PDF|MOBI|AZW3|AZW|DJVU|CBZ|CBR|FB2|DOCX?|TXT)\b`)

	// Size indicators
	sizeRegex := regexp.MustCompile(`\d+\.?\d*\s*(MB|KB|GB|TB)`)

	// Search through parts for format and size
	for i := 1; i < len(parts); i++ {
		part := strings.TrimSpace(parts[i])

		// Check for size
		if size == "" && sizeRegex.MatchString(part) {
			size = part
		}

		// Check for format
		if format == "" && formatRegex.MatchString(part) {
			matches := formatRegex.FindStringSubmatch(part)
			if len(matches) > 0 {
				format = strings.ToUpper(matches[1])
			}
		}

		// Early exit if we found both
		if format != "" && size != "" {
			break
		}
	}

	return language, format, size
}

// sanitizeFilename removes dangerous characters and prevents path traversal
func sanitizeFilename(filename string) string {
	// Replace unsafe characters with underscores
	safe := unsafeFilenameChars.ReplaceAllString(filename, "_")

	// Remove any path separators and ".." sequences
	safe = strings.ReplaceAll(safe, "..", "_")
	safe = filepath.Base(safe)

	// Limit filename length (255 is typical max, leave room for extension)
	if len(safe) > 200 {
		safe = safe[:200]
	}

	return safe
}

func FindBook(query string, content string) ([]*Book, error) {
	if content == "" {
		content = "book_any"
	}
	l := logger.GetLogger()

	// Use mutex to protect concurrent slice access
	var bookListMutex sync.Mutex
	bookList := make([]*colly.HTMLElement, 0)

	c := colly.NewCollector(
		colly.Async(true),
		// Set realistic User-Agent to avoid DDoS-Guard blocking
		colly.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
	)

	c.OnHTML("a[href^='/md5/']", func(e *colly.HTMLElement) {
		// Only process the first link (the cover image link), not the duplicate title link
		if e.Attr("class") == "custom-a block mr-2 sm:mr-4 hover:opacity-80" {
			bookListMutex.Lock()
			bookList = append(bookList, e)
			bookListMutex.Unlock()
		}
	})

	c.OnRequest(func(r *colly.Request) {
		l.Info("Visiting URL", zap.String("url", r.URL.String()))
	})

	// Add error handler
	c.OnError(func(r *colly.Response, err error) {
		status := 0
		if r != nil {
			status = r.StatusCode
		}
		l.Error("Search request failed",
			zap.Int("statusCode", status),
			zap.Error(err),
		)
	})

	env, err := env.GetEnv()
	if err != nil {
		return nil, err
	}

	fullURL := fmt.Sprintf(AnnasSearchEndpointFormat, env.AnnasBaseURL, url.QueryEscape(query), url.QueryEscape(content))

	if err := c.Visit(fullURL); err != nil {
		l.Error("Failed to visit search URL", zap.String("url", fullURL), zap.Error(err))
		return nil, fmt.Errorf("failed to visit search URL: %w", err)
	}
	c.Wait()

	bookListParsed := make([]*Book, 0)
	for _, e := range bookList {
		// Validate that parent and container elements exist
		parent := e.DOM.Parent()
		if parent.Length() == 0 {
			l.Warn("Skipping book: no parent element found")
			continue
		}

		bookInfoDiv := parent.Find("div.max-w-full")
		if bookInfoDiv.Length() == 0 {
			l.Warn("Skipping book: book info container not found")
			continue
		}

		// Extract title
		titleElement := bookInfoDiv.Find("a[href^='/md5/']")
		title := strings.TrimSpace(titleElement.Text())
		if title == "" {
			l.Warn("Skipping book: title is empty")
			continue
		}

		// Extract authors (optional)
		authorsRaw := bookInfoDiv.Find("a[href^='/search'] span.icon-\\[mdi--user-edit\\]").Parent().Text()
		authors := strings.TrimSpace(authorsRaw)

		// Extract publisher (optional)
		publisherRaw := bookInfoDiv.Find("a[href^='/search'] span.icon-\\[mdi--company\\]").Parent().Text()
		publisher := strings.TrimSpace(publisherRaw)

		// Extract metadata
		meta := bookInfoDiv.Find("div.text-gray-800").Text()
		language, format, size := extractMetaInformation(meta)

		// Extract link and hash
		link := e.Attr("href")
		if link == "" {
			l.Warn("Skipping book: no link found", zap.String("title", title))
			continue
		}
		hash := strings.TrimPrefix(link, "/md5/")
		if hash == "" {
			l.Warn("Skipping book: no hash found", zap.String("title", title))
			continue
		}

		book := &Book{
			Language:  language,
			Format:    format,
			Size:      size,
			Title:     title,
			Publisher: publisher,
			Authors:   authors,
			URL:       e.Request.AbsoluteURL(link),
			Hash:      hash,
		}

		bookListParsed = append(bookListParsed, book)
	}

	// Log result count for debugging
	l.Info("Search completed",
		zap.Int("totalElements", len(bookList)),
		zap.Int("validBooks", len(bookListParsed)),
	)

	return bookListParsed, nil
}

func (b *Book) Download(secretKey, folderPath string) error {
	l := logger.GetLogger()

	env, err := env.GetEnv()
	if err != nil {
		return fmt.Errorf("failed to get environment: %w", err)
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: HTTPTimeout,
	}

	// First API call: get download URL
	annasDownloadEndpoint := fmt.Sprintf(AnnasDownloadEndpointFormat, env.AnnasBaseURL)
	apiURL := fmt.Sprintf(annasDownloadEndpoint, b.Hash, secretKey)

	l.Info("Fetching download URL", zap.String("hash", b.Hash))

	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to fetch download URL: %w", err)
	}
	defer resp.Body.Close()

	// Validate HTTP status code
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, resp.Status)
	}

	var apiResp fastDownloadResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("failed to decode API response: %w", err)
	}

	if apiResp.DownloadURL == "" {
		if apiResp.Error != "" {
			return fmt.Errorf("API error: %s", apiResp.Error)
		}
		return errors.New("API returned empty download URL")
	}

	// Second API call: download the file
	l.Info("Downloading file", zap.String("url", apiResp.DownloadURL))

	downloadResp, err := client.Get(apiResp.DownloadURL)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer downloadResp.Body.Close()

	// Validate download status code
	if downloadResp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d: %s", downloadResp.StatusCode, downloadResp.Status)
	}

	// Sanitize filename to prevent path traversal and invalid characters
	safeTitle := sanitizeFilename(b.Title)
	if safeTitle == "" {
		safeTitle = "untitled"
	}

	format := strings.ToLower(b.Format)
	if format == "" {
		format = "bin"
	}

	filename := safeTitle + "." + format
	filePath := filepath.Join(folderPath, filename)

	l.Info("Creating file", zap.String("path", filePath))

	// Create the file
	out, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	// Setup cleanup on error
	success := false
	defer func() {
		out.Close()
		if !success {
			// Delete partial file on failure
			if removeErr := os.Remove(filePath); removeErr != nil {
				l.Warn("Failed to remove partial file",
					zap.String("path", filePath),
					zap.Error(removeErr),
				)
			}
		}
	}()

	// Copy the downloaded content
	written, err := io.Copy(out, downloadResp.Body)
	if err != nil {
		return fmt.Errorf("failed to write file (wrote %d bytes): %w", written, err)
	}

	// Sync to disk to ensure data is written
	if err := out.Sync(); err != nil {
		return fmt.Errorf("failed to sync file to disk: %w", err)
	}

	success = true
	l.Info("Download completed successfully",
		zap.String("path", filePath),
		zap.Int64("bytes", written),
	)

	return nil
}

func LookupDOI(doi string) (*Paper, error) {
	l := logger.GetLogger()

	c := colly.NewCollector(
		colly.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
	)

	paper := &Paper{DOI: doi}

	// Extract authors from page title (format: "Author1; Author2 - Anna's Archive")
	c.OnHTML("title", func(e *colly.HTMLElement) {
		title := e.Text
		if idx := strings.Index(title, " - Anna"); idx > 0 {
			paper.Authors = strings.TrimSpace(title[:idx])
		}
	})

	// Extract journal info
	c.OnHTML("meta[name=description]", func(e *colly.HTMLElement) {
		paper.Journal = e.Attr("content")
	})

	// Extract size from metadata line
	c.OnHTML("div.text-gray-500", func(e *colly.HTMLElement) {
		text := e.Text
		if strings.Contains(text, "MB") || strings.Contains(text, "KB") {
			paper.Size = strings.TrimSpace(text)
		}
	})

	// Extract download link
	c.OnHTML("a[href*='/d3/']", func(e *colly.HTMLElement) {
		if paper.DownloadURL == "" {
			paper.DownloadURL = e.Attr("href")
		}
	})

	// Extract Sci-Hub link
	c.OnHTML("a[href*='sci-hub']", func(e *colly.HTMLElement) {
		paper.SciHubURL = e.Attr("href")
	})

	c.OnError(func(r *colly.Response, err error) {
		status := 0
		if r != nil {
			status = r.StatusCode
		}
		l.Error("SciDB lookup failed",
			zap.String("doi", doi),
			zap.Int("statusCode", status),
			zap.Error(err),
		)
	})

	env, err := env.GetEnv()
	if err != nil {
		return nil, err
	}

	scidbURL := fmt.Sprintf(AnnasSciDBEndpointFormat, env.AnnasBaseURL, doi)
	paper.PageURL = scidbURL

	l.Info("Looking up DOI", zap.String("url", scidbURL))

	if err := c.Visit(scidbURL); err != nil {
		return nil, fmt.Errorf("failed to lookup DOI: %w", err)
	}

	if paper.Authors == "" && paper.DownloadURL == "" {
		return nil, fmt.Errorf("no paper found for DOI: %s", doi)
	}

	return paper, nil
}

func (b *Book) String() string {
	return fmt.Sprintf("Title: %s\nAuthors: %s\nPublisher: %s\nLanguage: %s\nFormat: %s\nSize: %s\nURL: %s\nHash: %s",
		b.Title, b.Authors, b.Publisher, b.Language, b.Format, b.Size, b.URL, b.Hash)
}

func (b *Book) ToJSON() (string, error) {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", err
	}

	return string(data), nil
}
