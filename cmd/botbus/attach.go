package main

// attach.go — the `botbus attach <url>` subcommand: fetch the role-aware
// briefing the hub serves for a channel/agent URL and print it to stdout. Lets a
// human or agent grab their briefing in one command. The fetch is factored into
// fetchBriefing(httpClient, url) so it's testable with httptest (no live
// network). Dispatched only when argv[1] == "attach".

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

// fetchBriefing GETs url with this CLI's User-Agent and returns the response
// body. A transport failure or a non-2xx status surfaces an error.
func fetchBriefing(client *http.Client, url string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	return string(body), nil
}

// attachCmd handles `botbus attach <url>`: resolve the arg to a full URL, fetch
// the briefing, and print it to stdout.
func attachCmd(args []string) {
	if len(args) < 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: botbus attach <url>")
		os.Exit(2)
	}
	u, err := resolveURL(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "attach:", err)
		os.Exit(1)
	}
	body, err := fetchBriefing(http.DefaultClient, u)
	if err != nil {
		fmt.Fprintln(os.Stderr, "attach:", err)
		os.Exit(1)
	}
	fmt.Print(body)
}
