package steam

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ContentServer struct {
	Type                     string  `json:"type"`
	Host                     string  `json:"host"`
	HttpsSupport             string  `json:"https_support"`
	Load                     int     `json:"load"`
	WeightedLoad             float64 `json:"weighted_load"`
	NumEntriesInClientList   int     `json:"num_entries_in_client_list"`
	SteamChinaOnly           bool    `json:"steam_china_only"`
	PriorityClass            int     `json:"priority_class"`
}

type ServerPool struct {
	mu      sync.RWMutex
	servers []string
	idx     atomic.Uint64
}

func NewServerPool(customServers []string, cellID uint32, maxServers int, apiHost string, useHTTP bool) (*ServerPool, error) {
	pool := &ServerPool{}

	defaultScheme := "http"
	if !useHTTP {
		defaultScheme = "http" // Default to http for custom servers unless specified or mandatory, matching Python
	}

	if len(customServers) > 0 {
		for _, s := range customServers {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
				s = fmt.Sprintf("http://%s", s)
			}
			pool.servers = append(pool.servers, s)
		}
		if len(pool.servers) > 0 {
			return pool, nil
		}
	}

	servers, err := fetchContentServers(cellID, maxServers, apiHost, useHTTP)
	if err != nil || len(servers) == 0 {
		// Default public fast CDNs fallback
		pool.servers = []string{
			fmt.Sprintf("%s://steampipe.akamaized.net", defaultScheme),
			fmt.Sprintf("%s://google.cdn.steampipe.steamcontent.com", defaultScheme),
			fmt.Sprintf("%s://level3.cdn.steampipe.steamcontent.com", defaultScheme),
		}
		return pool, nil
	}

	sort.SliceStable(servers, func(i, j int) bool {
		if servers[i].PriorityClass != servers[j].PriorityClass {
			return servers[i].PriorityClass < servers[j].PriorityClass
		}
		return servers[i].WeightedLoad < servers[j].WeightedLoad
	})

	for _, s := range servers {
		if s.Type == "OpenCache" || s.SteamChinaOnly {
			continue
		}
		scheme := "http"
		if !useHTTP && s.HttpsSupport == "mandatory" {
			scheme = "https"
		}
		pool.servers = append(pool.servers, fmt.Sprintf("%s://%s", scheme, s.Host))
	}

	// Fallback if all servers were steam_china_only (e.g., specific steam china app)
	if len(pool.servers) == 0 {
		for _, s := range servers {
			if s.Type == "OpenCache" {
				continue
			}
			scheme := "http"
			if !useHTTP && s.HttpsSupport == "mandatory" {
				scheme = "https"
			}
			pool.servers = append(pool.servers, fmt.Sprintf("%s://%s", scheme, s.Host))
		}
	}

	if len(pool.servers) == 0 {
		pool.servers = []string{
			fmt.Sprintf("%s://steampipe.akamaized.net", defaultScheme),
			fmt.Sprintf("%s://google.cdn.steampipe.steamcontent.com", defaultScheme),
		}
	}

	return pool, nil
}

func (p *ServerPool) GetServer() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.servers) == 0 {
		return "https://steampipe.akamaized.net"
	}
	idx := p.idx.Add(1) - 1
	return p.servers[idx%uint64(len(p.servers))]
}

// RemoveServer removes a malfunctioning or forbidden server from rotation.
func (p *ServerPool) RemoveServer(server string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.servers) <= 1 {
		return // Keep at least one
	}
	var filtered []string
	for _, s := range p.servers {
		if s != server {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) > 0 {
		p.servers = filtered
	}
}

func (p *ServerPool) GetAllHosts() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var hosts []string
	seen := make(map[string]bool)
	for _, s := range p.servers {
		u, err := url.Parse(s)
		if err == nil && !seen[u.Host] {
			seen[u.Host] = true
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}

func fetchContentServers(cellID uint32, maxServers int, apiHost string, useHTTP bool) ([]ContentServer, error) {
	if maxServers <= 0 {
		maxServers = 20
	}
	apiHost = ResolveAPIHost(apiHost)
	scheme := "https"
	if useHTTP {
		scheme = "http"
	}
	reqURL := fmt.Sprintf("%s://%s/IContentServerDirectoryService/GetServersForSteamPipe/v1/?cell_id=%d&max_servers=%d", scheme, apiHost, cellID, maxServers)
	client := &http.Client{Timeout: 6 * time.Second}

	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var root struct {
		Response struct {
			Servers []ContentServer `json:"servers"`
		} `json:"response"`
	}

	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}

	servers := root.Response.Servers
	sort.Slice(servers, func(i, j int) bool {
		// Prefer non-SteamChinaOnly over SteamChinaOnly so global depots don't hit China-only 403 nodes first
		if servers[i].SteamChinaOnly != servers[j].SteamChinaOnly {
			return !servers[i].SteamChinaOnly
		}
		if servers[i].PriorityClass != servers[j].PriorityClass {
			return servers[i].PriorityClass < servers[j].PriorityClass
		}
		// Prefer CDN over non-CDN, and lower weighted load
		if (servers[i].Type == "CDN") != (servers[j].Type == "CDN") {
			return servers[i].Type == "CDN"
		}
		return servers[i].WeightedLoad < servers[j].WeightedLoad
	})

	return servers, nil
}
