package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"depotdownloader-go/pkg/engine"
	"depotdownloader-go/pkg/manifest"
	"depotdownloader-go/pkg/parser"
	"depotdownloader-go/pkg/steam"
	"depotdownloader-go/pkg/storage"
)

func printHelp() {
	fmt.Println(`DepotDownloader (Go edition) - High performance Steam depot downloader

Usage:
  depotdownloader [options] -p <directory>
  depotdownloader [options] -m <manifest> -k <depot-key>
  depotdownloader [options] app -p <directory>
  depotdownloader [options] depot -m <manifest> -k <depot-key>

Options:
  -p, --app-path string     Directory containing manifests, .lua and/or config.vdf
  -m, --manifest-path string Specific .manifest file path
  -k, --depot-key string     Depot decryption key (64 hex characters)
  -l, --login-anonymously   Login anonymously to request and automatically refresh CDN auth tokens
  -u, --api-host string     Steam WebAPI host (Public, China, Partner or custom host) (default: Public)
  -a, --app-id uint         App ID (optional, auto-detected from lua or directory name)
  -c, --cell-id uint        Steam Cell ID (default 0)
  -o, --output string       Output directory (default <app-id> or <depot-id>)
  -t, --thread int          Number of chunk downloads in parallel (default 32)
  -r, --retry int           Max retries per chunk (default 5)
  -i, --integrity           Verify SHA-1 checksum of downloaded files
  -d, --delete              Delete files in output directory that are not in manifest
  -pattern string           Regex pattern to match filenames for downloading
  -s, --server string       Custom CDN server list (comma-separated or multiple)
  --use-http                Use HTTP instead of HTTPS for WebAPI and content downloading
  --use-websocket           Use WebSocket for Steam network connection (default: true)

Features:
  * Robust Breakpoint Resume: re-verifies on-disk chunks and only downloads missing pieces
  * Smart Incremental Updates: zero-IO skipping for unchanged files across manifests
  * Self-healing CDN Token: auto-reconnects, heartbeats, and refreshes on 401/403 expiry`)
}

// normalizeArgs expands combined boolean flags (e.g., -lu China -> -l -u China)
// and extracts subcommands (app, depot) so flags can appear anywhere.
func normalizeArgs(rawArgs []string) (cleanedArgs []string, explicitMode string, posArgs []string) {
	boolFlags := map[byte]bool{
		'l': true,
		'd': true,
		'i': true,
	}

	var expanded []string
	for _, arg := range rawArgs {
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
			chars := arg[1:]
			allConsumed := false
			for len(chars) > 0 {
				first := chars[0]
				if boolFlags[first] {
					expanded = append(expanded, "-"+string(first))
					chars = chars[1:]
					if len(chars) == 0 {
						allConsumed = true
						break
					}
					if !boolFlags[chars[0]] {
						expanded = append(expanded, "-"+chars)
						allConsumed = true
						break
					}
				} else {
					break
				}
			}
			if allConsumed {
				continue
			}
		}
		expanded = append(expanded, arg)
	}

	for _, arg := range expanded {
		if arg == "app" || arg == "depot" {
			explicitMode = arg
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			// Might be a flag value or a positional argument
			cleanedArgs = append(cleanedArgs, arg)
			posArgs = append(posArgs, arg)
		} else {
			cleanedArgs = append(cleanedArgs, arg)
		}
	}

	return cleanedArgs, explicitMode, posArgs
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	var (
		appPath         string
		manifestPath    string
		depotKeyHex     string
		loginAnon       bool
		apiHost         string
		appID           uint
		cellID          uint
		outputDir       string
		threadNum       int
		retryNum        int
		verifyIntegrity bool
		deleteUnmatched bool
		regexPattern    string
		customServers   string
		useHTTP         bool
		useWebSocket    bool
		showHelp        bool
	)

	fs := flag.NewFlagSet("depotdownloader", flag.ExitOnError)
	fs.BoolVar(&showHelp, "h", false, "show help")
	fs.BoolVar(&showHelp, "help", false, "show help")

	// Connection options (matching Python edition)
	fs.BoolVar(&useHTTP, "use-http", false, "use HTTP instead of HTTPS for WebAPI and content downloading")
	fs.BoolVar(&useWebSocket, "use-websocket", true, "use WebSocket for Steam network connection")

	// App and Depot paths
	fs.StringVar(&appPath, "p", "", "path to directory containing manifests")
	fs.StringVar(&appPath, "app-path", "", "path to directory containing manifests")
	fs.StringVar(&manifestPath, "m", "", "manifest path")
	fs.StringVar(&manifestPath, "manifest-path", "", "manifest path")
	fs.StringVar(&depotKeyHex, "k", "", "depot key (64 hex characters)")
	fs.StringVar(&depotKeyHex, "depot-key", "", "depot key (64 hex characters)")

	// Options
	fs.StringVar(&apiHost, "u", "Public", "Steam WebAPI host (Public, China, Partner or custom host)")
	fs.StringVar(&apiHost, "api-host", "Public", "Steam WebAPI host (Public, China, Partner or custom host)")
	fs.BoolVar(&loginAnon, "l", false, "login anonymously to request cdn auth token")
	fs.BoolVar(&loginAnon, "login-anonymously", false, "login anonymously to request cdn auth token")
	fs.UintVar(&appID, "a", 0, "app id")
	fs.UintVar(&appID, "app-id", 0, "app id")
	fs.UintVar(&cellID, "c", 0, "cell id")
	fs.UintVar(&cellID, "cell-id", 0, "cell id")
	fs.StringVar(&outputDir, "o", "", "output directory")
	fs.StringVar(&outputDir, "output", "", "output directory")
	fs.IntVar(&threadNum, "t", 32, "thread count")
	fs.IntVar(&threadNum, "thread", 32, "thread count")
	fs.IntVar(&retryNum, "r", 5, "retry count")
	fs.IntVar(&retryNum, "retry", 5, "retry count")
	fs.BoolVar(&verifyIntegrity, "i", false, "verify integrity")
	fs.BoolVar(&verifyIntegrity, "integrity", false, "verify integrity")
	fs.BoolVar(&deleteUnmatched, "d", false, "delete unmatched files")
	fs.BoolVar(&deleteUnmatched, "delete", false, "delete unmatched files")
	fs.StringVar(&regexPattern, "pattern", "", "regex pattern to match file path")
	fs.StringVar(&customServers, "s", "", "custom servers")
	fs.StringVar(&customServers, "server", "", "custom servers")

	cleanedArgs, explicitMode, _ := normalizeArgs(os.Args[1:])

	if err := fs.Parse(cleanedArgs); err != nil {
		printHelp()
		os.Exit(1)
	}

	if showHelp {
		printHelp()
		return
	}

	// Auto-detect mode if not explicit
	mode := explicitMode
	if mode == "" {
		if appPath != "" {
			mode = "app"
		} else if manifestPath != "" || depotKeyHex != "" {
			mode = "depot"
		} else if fs.NArg() > 0 {
			// Check if any leftover positional arg is a directory or .zip archive
			for _, arg := range fs.Args() {
				fi, err := os.Stat(arg)
				if err == nil && (fi.IsDir() || strings.EqualFold(filepath.Ext(arg), ".zip")) {
					appPath = arg
					mode = "app"
					break
				}
			}
		}
	}

	if mode == "" {
		fmt.Fprintln(os.Stderr, "Error: missing required parameters. Specify -p <dir> or -m <manifest> -k <key>")
		fmt.Fprintln(os.Stderr)
		printHelp()
		os.Exit(1)
	}

	var manifests []*manifest.DepotManifest
	depotKeys := make(map[uint32][]byte)

	switch mode {
	case "app":
		if appPath == "" && fs.NArg() > 0 {
			appPath = fs.Arg(0)
		}
		if appPath == "" {
			fmt.Fprintln(os.Stderr, "Error: -p/--app-path is required for app directory download")
			os.Exit(1)
		}

		info, err := parser.ParseAppPath(appPath)
		if err != nil {
			log.Fatalf("Error parsing app path: %v", err)
		}

		if appID == 0 {
			appID = uint(info.AppID)
		}
		manifests = info.Manifests
		depotKeys = info.DepotKeys

		if outputDir == "" {
			if appID != 0 {
				outputDir = fmt.Sprintf("%d", appID)
			} else {
				outputDir = filepath.Base(appPath)
			}
		}

	case "depot":
		if manifestPath == "" || depotKeyHex == "" {
			fmt.Fprintln(os.Stderr, "Error: both -m and -k are required for single depot download")
			os.Exit(1)
		}

		keyBytes, err := hex.DecodeString(strings.TrimSpace(depotKeyHex))
		if err != nil || len(keyBytes) != 32 {
			log.Fatalf("Invalid depot key: must be 64 hexadecimal characters")
		}

		m, err := manifest.ParseFromFile(manifestPath)
		if err != nil {
			log.Fatalf("Failed to parse manifest: %v", err)
		}

		if m.FilenamesEncrypted {
			if err := m.DecryptFilenames(keyBytes); err != nil {
				log.Fatalf("Failed to decrypt filenames: %v", err)
			}
		}

		manifests = append(manifests, m)
		depotKeys[m.Metadata.DepotID] = keyBytes

		if outputDir == "" {
			outputDir = fmt.Sprintf("%d", m.Metadata.DepotID)
		}
	}

	// Setup Steam Client and CDN
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var steamClient *steam.Client
	if loginAnon {
		resolvedHost := steam.ResolveAPIHost(apiHost)
		log.Printf("[Main] Logging into Steam network anonymously (API host: %s, useHTTP: %v)...", resolvedHost, useHTTP)
		steamClient = steam.NewClient(uint32(cellID), apiHost, useHTTP)
		if err := steamClient.ConnectAndLogonAnonymous(); err != nil {
			log.Fatalf("[Main] Failed to login to Steam: %v", err)
		}
		defer steamClient.Close()
		log.Printf("[Main] Logged in successfully.")
	}

	var serversList []string
	if customServers != "" {
		serversList = strings.Split(customServers, ",")
	}

	pool, err := steam.NewServerPool(serversList, uint32(cellID), 20, apiHost, useHTTP)
	if err != nil {
		log.Fatalf("[Main] Failed to initialize server pool: %v", err)
	}

	tokenMgr := steam.NewTokenManager(steamClient, loginAnon)
	stateStore := storage.LoadState(outputDir)
	downloader := engine.NewDownloader(pool, tokenMgr, stateStore)

	opts := engine.DownloadOptions{
		AppID:           uint32(appID),
		OutputDir:       outputDir,
		ThreadNum:       threadNum,
		RetryNum:        retryNum,
		VerifyIntegrity: verifyIntegrity,
		DeleteUnmatched: deleteUnmatched,
		RegexPattern:    regexPattern,
	}

	log.Printf("[Main] Total %d manifest(s) to process. Output directory: %s", len(manifests), outputDir)

	for idx, m := range manifests {
		select {
		case <-ctx.Done():
			log.Println("[Main] Download cancelled by user.")
			return
		default:
		}

		key, ok := depotKeys[m.Metadata.DepotID]
		if !ok {
			log.Fatalf("[Main] Missing depot key for depot %d", m.Metadata.DepotID)
		}

		log.Printf("[Main] [%d/%d] Processing depot %d...", idx+1, len(manifests), m.Metadata.DepotID)
		if err := downloader.DownloadManifest(ctx, m, key, opts); err != nil {
			log.Fatalf("[Main] Download failed for depot %d: %v", m.Metadata.DepotID, err)
		}
	}

	log.Println("[Main] All depot downloads finished successfully!")
}
