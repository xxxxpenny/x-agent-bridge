package openclaw

// wsclient.go — OpenClaw Gateway WS 协议的纯 Go 客户端（wire v4）。
// 移植自 @openclaw/gateway-client 的握手/签名逻辑：
//   1. dial → 等 connect.challenge {nonce, ts}
//   2. req connect（Ed25519 签 v3 载荷 + bootstrap/device token）
//   3. res hello-ok（含 deviceToken → 持久化，后续连接优先用）
//   4. 之后：{type:"req"} 请求 + {type:"event"} 订阅

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const gwProtocolVersion = 4
const gwClientVersion = "0.1.0"

type deviceIdentity struct {
	DeviceID      string `json:"deviceId"`
	PrivateKeyPEM string `json:"privateKeyPem"`
	PublicKeyPEM  string `json:"publicKeyPem"`
}

type gwFrame struct {
	Type    string          `json:"type"` // req | res | event
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *gwError        `json:"error,omitempty"`
	Event   string          `json:"event,omitempty"`
	Seq     *int            `json:"seq,omitempty"`
}

type gwError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

func (e *gwError) Error() string { return e.Code + ": " + e.Message }

type gwConn struct {
	ws        *websocket.Conn
	mu        sync.Mutex // 写锁
	reqMu     sync.Mutex
	pending   map[string]chan gwFrame
	nextID    int
	onEvent   func(event string, payload json.RawMessage)
	closed    chan struct{}
	closeOnce sync.Once
	challenge chan challengeMsg
}

type challengeMsg struct {
	Nonce string
	Ts    int64
}

// loadOrCreateIdentity：deviceId = sha256(raw ed25519 pubkey) hex（对齐网关指纹校验）。
func loadOrCreateIdentity(dir string) (*deviceIdentity, ed25519.PrivateKey, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "identity.json")
	if b, err := os.ReadFile(path); err == nil {
		var id deviceIdentity
		if json.Unmarshal(b, &id) == nil {
			key, err := parseEd25519(id.PrivateKeyPEM)
			if err == nil {
				return &id, key, nil
			}
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	id := deviceIdentity{
		DeviceID:      fmt.Sprintf("%x", sha256.Sum256(pub)),
		PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
		PublicKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
	}
	b, _ := json.Marshal(id)
	_ = os.WriteFile(path, b, 0o600)
	return &id, priv, nil
}

func parseEd25519(pemStr string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("bad pem")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	k, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not ed25519")
	}
	return k, nil
}

func rawPubB64(pubPEM string) string {
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		return ""
	}
	// SPKI DER：ed25519 公钥 = 末 32 字节
	if len(block.Bytes) >= 32 {
		return base64.RawURLEncoding.EncodeToString(block.Bytes[len(block.Bytes)-32:])
	}
	return ""
}

// buildPayloadV3：v3|deviceId|clientId|clientMode|role|scopes|signedAtMs|token|nonce|platform|deviceFamily
func buildPayloadV3(deviceID, clientID, clientMode, role string, scopes []string, signedAt int64, token, nonce, platform, deviceFamily string) string {
	return strings.Join([]string{
		"v3", deviceID, clientID, clientMode, role,
		strings.Join(scopes, ","),
		fmt.Sprint(signedAt), token, nonce,
		strings.ToLower(strings.TrimSpace(platform)),
		strings.ToLower(strings.TrimSpace(deviceFamily)),
	}, "|")
}

type gwTokenFile struct {
	Token  string   `json:"token"`
	Scopes []string `json:"scopes"`
}

// dialGateway 完成握手；deviceToken 落盘复用。
func dialGateway(ctx context.Context, url, bootstrapToken, dataDir string, onEvent func(string, json.RawMessage)) (*gwConn, json.RawMessage, error) {
	id, key, err := loadOrCreateIdentity(dataDir)
	if err != nil {
		return nil, nil, err
	}
	tokPath := filepath.Join(dataDir, "device-token.json")
	var stored gwTokenFile
	if b, err := os.ReadFile(tokPath); err == nil {
		_ = json.Unmarshal(b, &stored)
	}

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		return nil, nil, fmt.Errorf("ws dial: %w", err)
	}
	gc := &gwConn{
		ws: ws, pending: map[string]chan gwFrame{}, closed: make(chan struct{}),
		onEvent: onEvent, challenge: make(chan challengeMsg, 1),
	}
	go gc.readLoop()

	// 等 challenge
	nonce, ts, err := gc.waitChallenge(ctx, 20*time.Second)
	if err != nil {
		gc.Close()
		return nil, nil, err
	}

	scopes := []string{"operator.read", "operator.write", "operator.approvals"}
	// deviceToken 优先，否则 bootstrap token（对齐 selectGatewayConnectAuth）
	sigToken := stored.Token
	auth := map[string]string{}
	if stored.Token != "" {
		auth["deviceToken"] = stored.Token
	} else {
		auth["token"] = bootstrapToken
		sigToken = bootstrapToken
	}
	payload := buildPayloadV3(id.DeviceID, "gateway-client", "backend", "operator",
		scopes, ts, sigToken, nonce, runtime.GOOS, "")
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(payload)))
	device := map[string]any{
		"id": id.DeviceID, "publicKey": rawPubB64(id.PublicKeyPEM),
		"signature": sig, "signedAt": ts, "nonce": nonce,
	}
	client := map[string]any{
		"id": "gateway-client", "version": gwClientVersion,
		"platform": runtime.GOOS, "mode": "backend",
	}
	params := map[string]any{
		"minProtocol": gwProtocolVersion, "maxProtocol": gwProtocolVersion,
		"client": client, "caps": []string{"tool-events", "exec-approvals", "approvals"},
		"auth": auth, "role": "operator", "scopes": scopes, "device": device,
	}
	res, err := gc.Request(ctx, "connect", params)
	if err != nil {
		gc.Close()
		return nil, nil, err
	}
	// hello-ok：持久化 deviceToken
	var hello struct {
		Auth struct {
			DeviceToken string   `json:"deviceToken"`
			Scopes      []string `json:"scopes"`
		} `json:"auth"`
	}
	_ = json.Unmarshal(res, &hello)
	if hello.Auth.DeviceToken != "" && hello.Auth.DeviceToken != stored.Token {
		_ = os.WriteFile(tokPath, mustMarshal(gwTokenFile{Token: hello.Auth.DeviceToken, Scopes: hello.Auth.Scopes}), 0o600)
	}
	return gc, res, nil
}

func mustMarshal(v any) []byte { b, _ := json.Marshal(v); return b }

func (g *gwConn) waitChallenge(ctx context.Context, timeout time.Duration) (nonce string, ts int64, err error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-t.C:
			return "", 0, errors.New("timeout waiting connect.challenge")
		case c := <-g.challenge:
			return c.Nonce, c.Ts, nil
		}
	}
}

func (g *gwConn) readLoop() {
	defer g.Close()
	for {
		_, data, err := g.ws.ReadMessage()
		if err != nil {
			return
		}
		var f gwFrame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Type {
		case "res":
			g.reqMu.Lock()
			ch := g.pending[f.ID]
			g.reqMu.Unlock()
			if ch != nil {
				ch <- f
			}
		case "event":
			if f.Event == "connect.challenge" {
				var p struct {
					Nonce string `json:"nonce"`
					Ts    int64  `json:"ts"`
				}
				_ = json.Unmarshal(f.Payload, &p)
				if p.Nonce != "" {
					select {
					case g.challenge <- challengeMsg{Nonce: p.Nonce, Ts: p.Ts}:
					default:
					}
				}
			}
			if g.onEvent != nil {
				g.onEvent(f.Event, f.Payload)
			}
		}
	}
}

func (g *gwConn) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	g.reqMu.Lock()
	g.nextID++
	id := fmt.Sprintf("r%d", g.nextID)
	ch := make(chan gwFrame, 1)
	g.pending[id] = ch
	g.reqMu.Unlock()
	defer func() {
		g.reqMu.Lock()
		delete(g.pending, id)
		g.reqMu.Unlock()
	}()

	frame := gwFrame{Type: "req", ID: id, Method: method, Params: mustMarshal(params)}
	g.mu.Lock()
	err := g.ws.WriteJSON(frame)
	g.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("ws write %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.closed:
		return nil, errors.New("gateway connection closed")
	case f := <-ch:
		if f.OK != nil && !*f.OK {
			e := &gwError{Code: "UNKNOWN", Message: "request failed"}
			if f.Error != nil {
				e = f.Error
			}
			return f.Payload, e
		}
		return f.Payload, nil
	}
}

func (g *gwConn) Close() {
	g.closeOnce.Do(func() {
		close(g.closed)
		_ = g.ws.Close()
	})
}
