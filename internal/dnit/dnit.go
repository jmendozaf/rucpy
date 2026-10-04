// Package dnit reads the DNIT taxpayer registry ("Listado de RUC con sus equivalencias"):
// it finds the ruc0.zip … ruc9.zip links on the public page, downloads them and parses their rows.
package dnit

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// DefaultPageURL is the official listing page.
const DefaultPageURL = "https://www.dnit.gov.py/web/portal-institucional/listado-de-ruc-con-sus-equivalencias"

const userAgent = "rucpy (+https://github.com/jmendozaf/rucpy)"

var segmentName = regexp.MustCompile(`(?i)/ruc(\d)\.zip`)

// Segment is one of the ten registry files.
type Segment struct {
	Digit   int    // 0..9, the last digit of the RUCs it contains
	URL     string // absolute download URL
	Version string // Liferay version marker (?t=…); changes when DNIT republishes the file
}

// Row is one taxpayer.
type Row struct {
	RUC     string
	DV      int
	Name    string
	OldCode string
	Status  string
}

// Client talks to the DNIT site.
type Client struct {
	HTTP    *http.Client
	PageURL string
}

// NewClient returns a client for the official page.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 5 * time.Minute}, PageURL: DefaultPageURL}
}

// Segments fetches the listing page and returns the ten segments ordered by digit.
func (c *Client) Segments(ctx context.Context) ([]Segment, error) {
	body, err := c.get(ctx, c.PageURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	base, err := url.Parse(c.PageURL)
	if err != nil {
		return nil, err
	}
	segments, err := ParseSegments(body, base)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, errors.New("dnit: no ruc*.zip links found on the listing page (did the site change?)")
	}
	return segments, nil
}

// ParseSegments extracts the ruc0.zip … ruc9.zip links from the listing HTML.
func ParseSegments(r io.Reader, base *url.URL) ([]Segment, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("dnit: parse listing page: %w", err)
	}

	found := map[int]Segment{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key != "href" {
					continue
				}
				m := segmentName.FindStringSubmatch(a.Val)
				if m == nil {
					continue
				}
				ref, err := url.Parse(a.Val)
				if err != nil {
					continue
				}
				abs := base.ResolveReference(ref)
				digit, _ := strconv.Atoi(m[1])
				found[digit] = Segment{Digit: digit, URL: abs.String(), Version: abs.Query().Get("t")}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	segments := make([]Segment, 0, len(found))
	for _, s := range found {
		segments = append(segments, s)
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Digit < segments[j].Digit })
	return segments, nil
}

// Download saves a segment's zip to a temporary file and returns its path. The caller removes it.
func (c *Client) Download(ctx context.Context, s Segment) (string, error) {
	body, err := c.get(ctx, s.URL)
	if err != nil {
		return "", err
	}
	defer body.Close()

	f, err := os.CreateTemp("", fmt.Sprintf("ruc%d-*.zip", s.Digit))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("dnit: download ruc%d.zip: %w", s.Digit, err)
	}
	return f.Name(), f.Close()
}

func (c *Client) get(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dnit: GET %s: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("dnit: GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return resp.Body, nil
}

// ReadZip streams every row of the .txt files inside a segment zip to fn.
// Entries are read in memory only; nothing is written to disk, so hostile entry names are harmless.
func ReadZip(path string, fn func(Row) error) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("dnit: open %s: %w", path, err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name), ".txt") {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return err
		}
		err = ReadRows(rc, fn)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ReadRows parses pipe-separated lines: RUC|NAME|DV|OLD CODE|STATUS|
func ReadRows(r io.Reader, fn func(Row) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		row, ok := ParseRow(scanner.Text())
		if !ok {
			continue
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ParseRow reads one line. The fixed fields are taken from the right because names may contain '|'.
func ParseRow(line string) (Row, bool) {
	parts := strings.Split(strings.TrimRight(line, "\r\n"), "|")
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	n := len(parts)
	if n < 5 {
		return Row{}, false
	}
	ruc := strings.ToUpper(strings.TrimSpace(parts[0]))
	dv, err := strconv.Atoi(strings.TrimSpace(parts[n-3]))
	if ruc == "" || err != nil {
		return Row{}, false
	}
	return Row{
		RUC:     ruc,
		DV:      dv,
		Name:    strings.Join(strings.Fields(strings.Join(parts[1:n-3], "|")), " "),
		OldCode: strings.TrimSpace(parts[n-2]),
		Status:  strings.TrimSpace(parts[n-1]),
	}, true
}
