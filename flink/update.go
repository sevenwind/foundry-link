package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const releasesAPI = "https://api.github.com/repos/sevenwind/foundry-link/releases/latest"

type UpdateView struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

var latestUpdate atomic.Pointer[UpdateView]

// startUpdateCheck раз в несколько часов спрашивает GitHub о свежем релизе.
// Только уведомляет: ничего не скачивает и не устанавливает. Ошибки сети молча игнорируются.
func (c *Core) startUpdateCheck() {
	go func() {
		for {
			c.checkUpdate()
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(6 * time.Hour):
			}
		}
	}()
}

func (c *Core) checkUpdate() {
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "foundry-link/"+c.opts.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Draft  bool   `json:"draft"`
		Prerel bool   `json:"prerelease"`
	}
	if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&rel) != nil || rel.Draft || rel.Prerel {
		return
	}
	if !strings.HasPrefix(rel.URL, "https://github.com/") {
		return
	}
	if versionNewer(rel.Tag, c.opts.Version) {
		latestUpdate.Store(&UpdateView{Version: strings.TrimPrefix(rel.Tag, "v"), URL: rel.URL})
		c.logf("Доступна новая версия: %s", rel.Tag)
		c.changed()
	}
}

func parseVersion(s string) [3]int {
	var v [3]int
	for i, p := range strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3) {
		n, _ := strconv.Atoi(strings.TrimRight(p, "-+abcdefghijklmnopqrstuvwxyz"))
		v[i] = n
	}
	return v
}

func versionNewer(a, b string) bool {
	x, y := parseVersion(a), parseVersion(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}
