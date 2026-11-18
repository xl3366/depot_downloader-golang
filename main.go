package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Args struct {
	InputPath  string
	DepotKey   string
	OutputPath string
	ProxyURL   string
	Retry      int
	FileNames  []string
	CDNURLs    []string
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s != "" {
			*m = append(*m, s)
		}
	}
	return nil
}

func main() {
	var a Args
	var fileNames, cdnURLs multiFlag
	fs := flag.NewFlagSet("depotdl", flag.ExitOnError)
	fs.StringVar(&a.InputPath, "m", "", "manifest 路径 或 目录路径 (required)")
	fs.StringVar(&a.DepotKey, "k", "", "depot key hex (如果目录中有 lua/acf 可省略)")
	fs.StringVar(&a.OutputPath, "o", "default", "输出目录")
	fs.StringVar(&a.ProxyURL, "p", "", "代理地址")
	fs.IntVar(&a.Retry, "r", 3, "重试次数")
	fs.Var(&fileNames, "f", "过滤文件名 (逗号分隔)")
	fs.Var(&cdnURLs, "u", "手动指定 CDN (逗号分隔)")
	fs.Parse(os.Args[1:])

	if a.InputPath == "" {
		fs.Usage()
		os.Exit(2)
	}
	a.FileNames = fileNames
	a.CDNURLs = cdnURLs

	transport := &http.Transport{
		TLSHandshakeTimeout: 20 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 32,
		ForceAttemptHTTP2:   true,
	}
	if a.ProxyURL != "" {
		pu, err := url.Parse(a.ProxyURL)
		if err != nil {
			log.Fatal(err)
		}
		transport.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: transport}

	hosts := a.CDNURLs
	if len(hosts) == 0 {
		var err error
		hosts, err = getCDNURLList(client)
		if err != nil {
			log.Fatalf("获取 CDN 失败: %v", err)
		}
	}
	cdn := &cdnList{urls: hosts}
	log.Printf("using %d CDN hosts", len(hosts))

	stat, err := os.Stat(a.InputPath)
	if err != nil {
		log.Fatal(err)
	}

	if stat.IsDir() {
		manifests, depotKeys, appID, err := ParseAppDir(a.InputPath)
		if err != nil {
			log.Fatal(err)
		}

		if a.OutputPath == "default" {
			if appID != 0 {
				a.OutputPath = fmt.Sprintf("./%d", appID)
			} else {
				a.OutputPath = "./output"
			}
		}

		for depotID, manifest := range manifests {
			key := depotKeys[depotID]
			if a.DepotKey != "" {
				k, err := hex.DecodeString(a.DepotKey)
				if err == nil && len(k) == 32 {
					key = k
				} else {
					log.Printf("外部传入的 -k 格式错误，忽略")
				}
			}

			if len(key) != 32 {
				log.Printf("[跳过] Depot %d 缺少有效的解密 Key。", depotID)
				continue
			}

			log.Printf("开始下载 Depot %d ...", depotID)
			cfg := DepotConfig{
				DepotKey:   key,
				OutputPath: a.OutputPath,
				Retry:      a.Retry,
				CDNList:    cdn,
				Client:     client,
			}
			if len(a.FileNames) > 0 {
				var parts []string
				for _, n := range a.FileNames {
					parts = append(parts, regexp.QuoteMeta(strings.ReplaceAll(n, "\\", "/")))
				}
				cfg.Pattern = strings.Join(parts, "|")
			}

			if err := RunDownloadManifest(context.Background(), cfg, manifest); err != nil {
				log.Printf("Depot %d 下载失败: %v", depotID, err)
				continue
			}
			// 注意：这里没有 os.Remove。断点文件会被永久保留用于增量更新。
		}
	} else {
		if a.DepotKey == "" {
			log.Fatal("单文件模式必须提供 -k")
		}
		key, err := hex.DecodeString(a.DepotKey)
		if err != nil || len(key) != 32 {
			log.Fatal("depot key 格式错误")
		}

		raw, err := os.ReadFile(a.InputPath)
		if err != nil {
			log.Fatal(err)
		}
		payload, meta, err := DeserializeManifest(raw)
		if err != nil {
			log.Fatal(err)
		}

		manifest := &DepotManifest{
			DepotID:  meta.DepotID,
			Payload:  payload,
			Metadata: meta,
		}

		cfg := DepotConfig{
			DepotKey:   key,
			OutputPath: a.OutputPath,
			Retry:      a.Retry,
			CDNList:    cdn,
			Client:     client,
		}
		if len(a.FileNames) > 0 {
			var parts []string
			for _, n := range a.FileNames {
				parts = append(parts, regexp.QuoteMeta(strings.ReplaceAll(n, "\\", "/")))
			}
			cfg.Pattern = strings.Join(parts, "|")
		}

		if err := RunDownloadManifest(context.Background(), cfg, manifest); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println("done")
}