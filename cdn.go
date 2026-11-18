package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type cdnList struct {
	urls []string
	idx  atomic.Uint64
}

func getCDNURLList(client *http.Client) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u := "https://api.steampowered.com/icontentserverdirectoryservice/getserversforsteampipe/v1"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getserversforsteampipe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("getserversforsteampipe: http %d", resp.StatusCode)
	}

	var r struct {
		Response struct {
			Servers []struct {
				Host         string  `json:"host"`
				WeightedLoad float64 `json:"weighted_load"`
			} `json:"servers"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	strict := []string{}
	relaxed := []string{}
	seen := map[string]bool{}
	for _, s := range r.Response.Servers {
		if s.Host == "" {
			continue
		}
		if seen[s.Host] {
			continue
		}
		seen[s.Host] = true
		if s.WeightedLoad <= 130 && strings.Contains(s.Host, "steampipe") {
			strict = append(strict, s.Host)
		} else {
			relaxed = append(relaxed, s.Host)
		}
	}

	if len(strict) > 0 {
		sort.Strings(strict)
		return strict, nil
	}
	if len(relaxed) > 0 {
		sort.Strings(relaxed)
		return relaxed, nil
	}
	return nil, errors.New("no CDN hosts available")
}

func (c *cdnList) pick(avoid int) int {
	n := len(c.urls)
	if n == 0 {
		return -1
	}
	i := int(c.idx.Add(1) % uint64(n))
	if n > 1 && i == avoid {
		i = (i + 1) % n
	}
	return i
}

func (c *cdnList) urlFor(i int, depotID uint32, chunkHex string) string {
	return fmt.Sprintf("http://%s/depot/%d/chunk/%s", c.urls[i], depotID, chunkHex)
}