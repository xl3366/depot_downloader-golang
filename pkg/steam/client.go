package steam

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Fallback Steam CM websocket endpoints
var (
	defaultChinaCMList = []string{
		"cmp1-sha1.steamchina.com:443",
		"cmp2-sha1.steamchina.com:443",
		"cmp1-sha1.steamchina.com:27018",
		"cmp2-sha1.steamchina.com:27018",
		"cmp2-sha1.steamchina.com:27019",
		"cmp1-sha1.steamchina.com:27020",
	}

	defaultGlobalCMList = []string{
		"cmp1-tyo3.steamserver.net:443",
		"cmp2-tyo3.steamserver.net:443",
		"cmp1-hkg1.steamserver.net:443",
		"cmp2-hkg1.steamserver.net:443",
		"cmp3-hkg1.steamserver.net:443",
		"cmp1-sgp1.steamserver.net:443",
		"cmp2-sgp1.steamserver.net:443",
		"cmp1-lax1.steamserver.net:443",
		"cmp2-lax1.steamserver.net:443",
		"cmp1-ord1.steamserver.net:443",
		"cmp1-dfw2.steamserver.net:443",
	}
)

type pendingJob struct {
	ch chan *jobResponse
}

type jobResponse struct {
	hdr  *ProtoBufHeader
	body []byte
	err  error
}

// Client represents a connection to the Steam network via WebSocket CM.
type Client struct {
	mu           sync.Mutex
	conn         *websocket.Conn
	sessionID    int32
	steamID      uint64
	jobSeq       atomic.Uint64
	jobsMu       sync.Mutex
	pendingJobs  map[uint64]*pendingJob
	closed       bool
	heartbeatStop chan struct{}
	cellID        uint32
	apiHost       string
	useHTTP       bool
}

func (c *Client) getFallbackCMList() []string {
	if strings.Contains(strings.ToLower(c.apiHost), "steamchina") {
		return append([]string(nil), defaultChinaCMList...)
	}
	return append([]string(nil), defaultGlobalCMList...)
}

func NewClient(cellID uint32, apiHost string, useHTTP bool) *Client {
	if apiHost == "" {
		apiHost = "api.steampowered.com"
	}
	c := &Client{
		cellID:      cellID,
		apiHost:     ResolveAPIHost(apiHost),
		useHTTP:     useHTTP,
		pendingJobs: make(map[uint64]*pendingJob),
	}
	return c
}

// ConnectAndLogonAnonymous connects to Steam CM and logs in anonymously.
func (c *Client) ConnectAndLogonAnonymous() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.connectLocked()
}

func (c *Client) connectLocked() error {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	if c.heartbeatStop != nil {
		close(c.heartbeatStop)
		c.heartbeatStop = nil
	}

	cmList := c.fetchCMList()
	if len(cmList) == 0 {
		cmList = c.getFallbackCMList()
	}

	// Randomize server order to distribute load and improve resiliency (matches Python's CMServerList)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Shuffle(len(cmList), func(i, j int) {
		cmList[i], cmList[j] = cmList[j], cmList[i]
	})

	var lastErr error
	var wsConn *websocket.Conn

	dialer := websocket.Dialer{
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: 10 * time.Second,
	}

	for _, cm := range cmList {
		wsURL := fmt.Sprintf("wss://%s/cmsocket/", cm)
		conn, resp, err := dialer.Dial(wsURL, nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			wsConn = conn
			break
		}
		lastErr = err
	}

	if wsConn == nil {
		return fmt.Errorf("failed to connect to any Steam CM: %w", lastErr)
	}

	c.conn = wsConn
	c.heartbeatStop = make(chan struct{})

	// Wait for logon response (register pendingJob before readLoop and write)
	loginDone := make(chan error, 1)
	c.jobsMu.Lock()
	c.pendingJobs[0] = &pendingJob{
		ch: make(chan *jobResponse, 1),
	}
	jobCh := c.pendingJobs[0].ch
	c.jobsMu.Unlock()

	// Start read loop
	go c.readLoop(wsConn)

	// Send ClientLogon
	logon := &ClientLogonMessage{
		ProtocolVersion:      65580,
		ClientOsType:         16, // Windows 10
		CellID:               c.cellID,
		ClientPackageVersion: 1561159470,
	}
	logonHdr := &ProtoBufHeader{
		SteamID: 117093590311632896, // SteamID(type='AnonUser', universe='Public')
	}
	packet := EncodeSteamMessage(EMsgClientLogon, logonHdr, logon.Marshal())

	if err := wsConn.WriteMessage(websocket.BinaryMessage, packet); err != nil {
		_ = wsConn.Close()
		return fmt.Errorf("failed to send logon message: %w", err)
	}

	go func() {
		select {
		case resp := <-jobCh:
			if resp.err != nil {
				loginDone <- resp.err
				return
			}
			logonResp, err := unmarshalClientLogOnResponse(resp.body)
			if err != nil {
				loginDone <- err
				return
			}
			if logonResp.EResult != 1 {
				loginDone <- fmt.Errorf("steam anonymous login failed with EResult: %d", logonResp.EResult)
				return
			}
			if logonResp.ClientSuppliedSteamID != 0 {
				c.steamID = logonResp.ClientSuppliedSteamID
			} else if resp.hdr.SteamID != 0 {
				c.steamID = resp.hdr.SteamID
			}
			if resp.hdr.ClientSessionID != 0 {
				c.sessionID = resp.hdr.ClientSessionID
			}
			loginDone <- nil
		case <-time.After(15 * time.Second):
			loginDone <- errors.New("steam anonymous login timed out")
		}
	}()

	if err := <-loginDone; err != nil {
		_ = wsConn.Close()
		return err
	}

	// Start heartbeat loop
	go c.heartbeatLoop(c.heartbeatStop)

	return nil
}

// ResolveAPIHost canonicalizes an apihost string (Public, Partner, China or custom host).
func ResolveAPIHost(host string) string {
	host = strings.TrimSpace(host)
	switch strings.ToLower(host) {
	case "public", "":
		return "api.steampowered.com"
	case "partner":
		return "partner.steam-api.com"
	case "china":
		return "api.steamchina.com"
	default:
		host = strings.TrimPrefix(host, "https://")
		host = strings.TrimPrefix(host, "http://")
		return strings.TrimRight(host, "/")
	}
}

func (c *Client) fetchCMList() []string {
	client := &http.Client{Timeout: 5 * time.Second}
	scheme := "https"
	if c.useHTTP {
		scheme = "http"
	}
	url := fmt.Sprintf("%s://%s/ISteamDirectory/GetCMListForConnect/v1/?cellid=%d&cmtype=websockets", scheme, c.apiHost, c.cellID)
	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var result struct {
		Response struct {
			ServerList []struct {
				Endpoint string `json:"endpoint"`
				Type     string `json:"type"`
			} `json:"serverlist"`
		} `json:"response"`
	}

	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}

	var list []string
	for _, s := range result.Response.ServerList {
		if strings.HasPrefix(s.Type, "websocket") || s.Type == "" {
			list = append(list, s.Endpoint)
		}
	}
	return list
}

func (c *Client) heartbeatLoop(stopCh chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				return
			}
			hdr := &ProtoBufHeader{
				SteamID:         c.steamID,
				ClientSessionID: c.sessionID,
			}
			pkt := EncodeSteamMessage(EMsgClientHeartBeat, hdr, nil)
			_ = conn.WriteMessage(websocket.BinaryMessage, pkt)
		}
	}
}

func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			c.handleDisconnect(err)
			return
		}
		c.handlePacket(msg)
	}
}

func (c *Client) handlePacket(packet []byte) {
	emsg, hdr, body, err := DecodeSteamMessage(packet)
	if err != nil {
		return
	}

	if emsg == EMsgMulti {
		multi, err := unmarshalCMsgMulti(body)
		if err != nil {
			return
		}
		payload := multi.MessageBody
		if multi.SizeUnzipped > 0 {
			zr, err := gzip.NewReader(bytes.NewReader(multi.MessageBody))
			if err != nil {
				return
			}
			decompressed, err := io.ReadAll(zr)
			_ = zr.Close()
			if err != nil {
				return
			}
			payload = decompressed
		}

		for len(payload) >= 4 {
			subSize := binary.LittleEndian.Uint32(payload[:4])
			payload = payload[4:]
			if int(subSize) > len(payload) {
				break
			}
			subMsg := payload[:subSize]
			payload = payload[subSize:]
			c.handlePacket(subMsg)
		}
		return
	}

	if emsg == EMsgClientLogOnResponse {
		c.jobsMu.Lock()
		if pj, ok := c.pendingJobs[0]; ok {
			pj.ch <- &jobResponse{hdr: hdr, body: body}
			delete(c.pendingJobs, 0)
		}
		c.jobsMu.Unlock()
		return
	}

	if emsg == EMsgServiceMethodSendToClient || emsg == EMsgServiceMethodResponse {
		c.jobsMu.Lock()
		if pj, ok := c.pendingJobs[hdr.JobIDTarget]; ok {
			pj.ch <- &jobResponse{hdr: hdr, body: body}
			delete(c.pendingJobs, hdr.JobIDTarget)
		}
		c.jobsMu.Unlock()
		return
	}
}

func (c *Client) handleDisconnect(err error) {
	c.jobsMu.Lock()
	for id, pj := range c.pendingJobs {
		pj.ch <- &jobResponse{err: fmt.Errorf("connection disconnected: %w", err)}
		delete(c.pendingJobs, id)
	}
	c.jobsMu.Unlock()
}

// CallServiceMethod sends a unified service method request and awaits the response.
// Automatically reconnects if connection was lost.
func (c *Client) CallServiceMethod(method string, reqBody []byte, timeout time.Duration) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		respBody, err := c.callServiceMethodOnce(method, reqBody, timeout)
		if err == nil {
			return respBody, nil
		}

		// Reconnect and retry on connection failure
		log.Printf("[Steam] Service method %s failed (attempt %d/3): %v. Reconnecting...", method, attempt+1, err)
		c.mu.Lock()
		reconnErr := c.connectLocked()
		c.mu.Unlock()
		if reconnErr != nil {
			log.Printf("[Steam] Reconnect failed: %v", reconnErr)
			time.Sleep(1 * time.Second)
		}
	}

	return nil, fmt.Errorf("service method %s failed after 3 attempts", method)
}

func (c *Client) callServiceMethodOnce(method string, reqBody []byte, timeout time.Duration) ([]byte, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return nil, errors.New("steam client not connected")
	}

	jobID := c.jobSeq.Add(1)
	hdr := &ProtoBufHeader{
		SteamID:         c.steamID,
		ClientSessionID: c.sessionID,
		JobIDSource:     jobID,
		TargetJobName:   method,
	}

	packet := EncodeSteamMessage(EMsgServiceMethodCallFromClient, hdr, reqBody)

	pj := &pendingJob{ch: make(chan *jobResponse, 1)}
	c.jobsMu.Lock()
	c.pendingJobs[jobID] = pj
	c.jobsMu.Unlock()

	defer func() {
		c.jobsMu.Lock()
		delete(c.pendingJobs, jobID)
		c.jobsMu.Unlock()
	}()

	if err := conn.WriteMessage(websocket.BinaryMessage, packet); err != nil {
		return nil, fmt.Errorf("failed to send packet: %w", err)
	}

	select {
	case res := <-pj.ch:
		if res.err != nil {
			return nil, res.err
		}
		if res.hdr.EResult != 1 {
			return nil, fmt.Errorf("service method returned EResult %d (%s)", res.hdr.EResult, res.hdr.ErrorMessage)
		}
		return res.body, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("service method %s timed out after %v", method, timeout)
	}
}

// Close closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	if c.heartbeatStop != nil {
		close(c.heartbeatStop)
		c.heartbeatStop = nil
	}
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}
