package anna

import "fmt"

type Book struct {
	Language  string `json:"language"`
	Format    string `json:"format"`
	Size      string `json:"size"`
	Title     string `json:"title"`
	Publisher string `json:"publisher"`
	Authors   string `json:"authors"`
	URL       string `json:"url"`
	Hash      string `json:"hash"`
}

type Paper struct {
	DOI         string `json:"doi"`
	Authors     string `json:"authors"`
	Journal     string `json:"journal"`
	Size        string `json:"size"`
	DownloadURL string `json:"download_url"`
	SciHubURL   string `json:"scihub_url"`
	PageURL     string `json:"page_url"`
}

func (p *Paper) String() string {
	return fmt.Sprintf("DOI: %s\nAuthors: %s\nJournal: %s\nSize: %s\nDownload URL: %s\nSci-Hub: %s\nPage: %s",
		p.DOI, p.Authors, p.Journal, p.Size, p.DownloadURL, p.SciHubURL, p.PageURL)
}

type fastDownloadResponse struct {
	DownloadURL string `json:"download_url"`
	Error       string `json:"error"`
}
