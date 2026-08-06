// Package pjeoffice implements the HTTP server that speaks the PJeOffice 1.0
// protocol. It is a faithful port of a Python reference implementation
// to Go, consuming the signer.Signer interface instead of a PKCS12Token directly.
package pjeoffice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"crypto/tls"
	"strings"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/MrSchrodingers/pje_headless/internal/signer"
)

const (
	pjeVersion     = "2.5.16"
	connectTimeout = 3 * time.Second
	readTimeout    = 10 * time.Second
)

// gifOK is the 1x1 transparent GIF used as the success health/ack response.
// Matches GIF_OK in the Python reference.
var gifOK = mustDecodeBase64("R0lGODlhAQABAPAAAP///wAAACH5BAAAAAAALAAAAAABAAEAAAICRAEAOw==")

// gifErr is the 2x1 transparent GIF used as the error response.
// Matches GIF_ERR in the Python reference.
var gifErr = mustDecodeBase64("R0lGODlhAgABAPAAAP///wAAACH5BAAAAAAALAAAAAACAAEAAAICRAEAOw==")

func mustDecodeBase64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic("pjeoffice: invalid built-in GIF base64: " + err.Error())
	}
	return b
}

// Server is the PJeOffice HTTP server. Create via NewServer; start via Start or Serve.
type Server struct {
	signer   signer.Signer
	port     string
	bindAddr string // interface to bind; defaults to 127.0.0.1 (loopback)
	log      *slog.Logger
	handler  http.Handler
	// httpClient is used for the outbound POST to the tribunal endpoint.
	// It is configured once at construction and shared across requests.
	httpClient *http.Client
}

// NewServer creates a Server that will accept requests on the given port and
// bind address. port "0" lets the OS pick a free port (useful for tests).
// bindAddr controls the interface to bind: use "127.0.0.1" (loopback) for
// local-only access or "0.0.0.0" to expose on all interfaces.
// The signer must be safe for concurrent use.
func NewServer(s signer.Signer, port, bindAddr string) *Server {
	srv := &Server{
		signer:   s,
		port:     port,
		bindAddr: bindAddr,
		log:      slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		httpClient: &http.Client{
			Timeout: connectTimeout + readTimeout,
			// Do not follow redirects — the protocol considers 302/304 as success codes.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/pjeOffice/", srv.route)
	srv.handler = mux
	return srv
}

// ServeHTTP makes *Server implement http.Handler directly, enabling httptest usage.
func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	srv.handler.ServeHTTP(w, r)
}

// Start listens on the configured port and bind address, then blocks until
// the server exits. The bind address is resolved from cfg.BindAddr (default
// "127.0.0.1", i.e. loopback only). When the primary bind fails and the
// configured address is an IPv4 loopback, a single IPv6 loopback ("::1")
// fallback is attempted so the server still works on IPv6-only hosts.
func (srv *Server) Start() error {
	addr := net.JoinHostPort(srv.bindAddr, srv.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Fallback: if the caller requested IPv4 loopback but the host only
		// has IPv6 loopback, try ::1 so the service stays local-only.
		if srv.bindAddr == "127.0.0.1" {
			ln, err = net.Listen("tcp6", net.JoinHostPort("::1", srv.port))
			if err != nil {
				return fmt.Errorf("pjeoffice: listen: %w", err)
			}
		} else {
			return fmt.Errorf("pjeoffice: listen: %w", err)
		}
	}
	return srv.Serve(ln)
}

// Serve accepts connections on ln. Useful when the caller owns the listener.
func (srv *Server) Serve(ln net.Listener) error {
	hs := &http.Server{
		Handler:           srv.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      readTimeout + connectTimeout,
		MaxHeaderBytes:    2 << 20, // 2 MiB
	}

	// Generate self-signed certificate for HTTPS
	cert, err := generateSelfSignedCert()
	if err != nil {
		srv.log.Error("failed to generate TLS certificate", "err", err)
	} else {
		go func() {
			tlsConfig := &tls.Config{
				Certificates: []tls.Certificate{cert},
			}
			httpsServer := &http.Server{
				Addr:              "127.0.0.1:8801",
				Handler:           srv.handler,
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       readTimeout,
				WriteTimeout:      readTimeout + connectTimeout,
				MaxHeaderBytes:    2 << 20,
				TLSConfig:         tlsConfig,
			}
			srv.log.Info("PJeOffice headless HTTPS pronto", "addr", "127.0.0.1:8801")
			if err := httpsServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				srv.log.Error("HTTPS server failed", "err", err)
			}
		}()
	}

	srv.log.Info("PJeOffice headless pronto", "addr", ln.Addr().String())
	return hs.Serve(ln)
}

// route dispatches to the correct sub-handler based on path and method.
func (srv *Server) route(w http.ResponseWriter, r *http.Request) {
	// Preflight always handled first regardless of path.
	if r.Method == http.MethodOptions {
		srv.handleOptions(w, r)
		return
	}

	switch r.URL.Path {
	case "/pjeOffice/":
		if r.Method == http.MethodGet {
			srv.applyCORS(w, r)
			writeGIF(w, gifOK)
			return
		}
		http.NotFound(w, r)

	case "/pjeOffice/requisicao/":
		switch r.Method {
		case http.MethodGet:
			srv.handleGET(w, r)
		case http.MethodPost:
			srv.handlePOST(w, r)
		default:
			srv.applyCORS(w, r)
			http.NotFound(w, r)
		}

	default:
		srv.applyCORS(w, r)
		http.NotFound(w, r)
	}
}

// handleOptions mirrors do_OPTIONS in the Python reference.
func (srv *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	srv.applyCORS(w, r)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if reqH := r.Header.Get("Access-Control-Request-Headers"); reqH != "" {
		w.Header().Set("Access-Control-Allow-Headers", reqH)
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// handleGET mirrors do_GET for /pjeOffice/requisicao/ in the Python reference.
// The payload is received as ?r=<url-encoded JSON>.
func (srv *Server) handleGET(w http.ResponseWriter, r *http.Request) {
	srv.applyCORS(w, r)
	rValues := r.URL.Query()["r"]
	if len(rValues) == 0 {
		writeGIF(w, gifErr)
		return
	}

	decoded, err := url.QueryUnescape(rValues[0])
	if err != nil {
		srv.log.Error("GET /requisicao/: QueryUnescape failed", "err", err)
		writeGIF(w, gifErr)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(decoded), &payload); err != nil {
		srv.log.Error("GET /requisicao/: JSON decode failed", "err", err)
		writeGIF(w, gifErr)
		return
	}

	authHeader := r.Header.Get("Authorization")
	if err := srv.process(r.Context(), payload, authHeader); err != nil {
		srv.log.Error("GET /requisicao/: process failed", "err", err)
		writeGIF(w, gifErr)
		return
	}
	writeGIF(w, gifOK)
}

// handlePOST mirrors do_POST in the Python reference.
// The payload is received as a JSON body.
func (srv *Server) handlePOST(w http.ResponseWriter, r *http.Request) {
	srv.applyCORS(w, r)

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB max
	if err != nil {
		srv.log.Error("POST /requisicao/: read body failed", "err", err)
		writeGIF(w, gifErr)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		srv.log.Error("POST /requisicao/: JSON decode failed", "err", err)
		writeGIF(w, gifErr)
		return
	}

	authHeader := r.Header.Get("Authorization")

	if err := srv.process(r.Context(), payload, authHeader); err != nil {
		srv.log.Error("POST /requisicao/: process failed", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"sucesso":false,"erro":"falha no processamento"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"sucesso":true,"mensagem":"Assinatura realizada com sucesso"}`))
}

// envelope is the outer wrapper sent by the browser/PJe client.
type envelope struct {
	Servidor string `json:"servidor"`
	Versao   string `json:"versao"`
	Sessao   string `json:"sessao"`
	Tarefa   string `json:"tarefa"` // JSON string (double-encoded)
}

// task is the inner payload embedded as a JSON string inside envelope.Tarefa.
type task struct {
	Mensagem            string `json:"mensagem"`
	EnviarPara          string `json:"enviarPara"`
	Token               string `json:"token"`
	AlgoritmoAssinatura string `json:"algoritmoAssinatura"`
	UploadUrl           string `json:"uploadUrl"`
	Arquivos            []struct {
		ID     string `json:"id"`
		CodIni string `json:"codIni"`
		Hash   string `json:"hash"`
		IsBin  string `json:"isBin"`
	} `json:"arquivos"`
}

// successCodes mirrors SUCCESS_CODES in the Python reference.
var successCodes = map[int]bool{200: true, 201: true, 202: true, 204: true, 302: true, 304: true, 307: true}

// process is the faithful Go port of Authenticator.process() in the Python reference.
// It:
//  1. Parses the outer envelope and inner task.
//  2. Calls signer.Login -> signer.Sign -> signer.CertChainPKIPath.
//  3. POSTs {uuid, mensagem, assinatura, certChain} to servidor+enviarPara
//     with the versao and Cookie headers.
func (srv *Server) process(ctx context.Context, raw map[string]any, authHeader string) error {
	// Marshal back to JSON so we can unmarshal into the typed struct.
	// This avoids field-by-field type-asserting on map[string]any.
	data, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("process: re-marshal: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("process: parse envelope: %w", err)
	}

	srv.log.Info("process: raw tarefa", "tarefa", env.Tarefa)
		srv.log.Info("process: envelope COMPLETO", "envelope_sessao", env.Sessao, "envelope_servidor", env.Servidor)

	var t task
	if err := json.Unmarshal([]byte(env.Tarefa), &t); err != nil {
		return fmt.Errorf("process: parse tarefa: %w", err)
	}

	algorithm := t.AlgoritmoAssinatura
	if algorithm == "" {
		algorithm = "MD5withRSA"
	}

	srv.log.Info("process: algorithm", "alg", algorithm, "msgLen", len(t.Mensagem), "msgSample", fmt.Sprintf("%x", t.Mensagem))
	if err := srv.signer.Login(ctx); err != nil {
		return fmt.Errorf("process: login: %w", err)
	}

	certChain, err := srv.signer.CertChainPKIPath(ctx)
	if err != nil {
		return fmt.Errorf("process: certchain: %w", err)
	}

	var outJSON []byte
	var target string

	var bodyReader io.Reader
	contentType := "application/json"

	if t.UploadUrl != "" && len(t.Arquivos) > 0 {
		// Batch signature mode
		var responses []map[string]any
		for _, arq := range t.Arquivos {
			hashBytes, err := hex.DecodeString(arq.Hash)
			if err != nil {
				return fmt.Errorf("process: decode hash %q: %w", arq.Hash, err)
			}

			assinatura, err := srv.signer.Sign(ctx, string(hashBytes), algorithm)
			if err != nil {
				return fmt.Errorf("process: sign batch: %w", err)
			}

			responses = append(responses, map[string]any{
				"id":                arq.ID,
				"assinatura":        assinatura,
				"cadeiaCertificado": []string{certChain},
			})
		}

		outJSON, err = json.Marshal(responses)
		if err != nil {
			return fmt.Errorf("process: marshal batch out: %w", err)
		}

		if strings.HasPrefix(t.UploadUrl, "http://") || strings.HasPrefix(t.UploadUrl, "https://") {
			target = t.UploadUrl
		} else {
			target = env.Servidor + t.UploadUrl
		}

		if strings.Contains(target, ".seam") {
			// Send form-urlencoded for .seam endpoints even in batch mode
			values := url.Values{}
			for i, resp := range responses {
				arq := t.Arquivos[i]
				values.Add("id", arq.ID)
				if arq.CodIni != "" {
					values.Add("codIni", arq.CodIni)
				}
				values.Add("hash", arq.Hash)
				if arq.IsBin != "" {
					values.Add("isBin", arq.IsBin)
				}
				values.Add("assinatura", resp["assinatura"].(string))
				values.Add("cadeiaCertificado", certChain)
			}
			bodyReader = strings.NewReader(values.Encode())
			contentType = "application/x-www-form-urlencoded"
		} else {
			bodyReader = bytes.NewReader(outJSON)
		}
	} else {
		// Single document signature mode
		assinatura, err := srv.signer.Sign(ctx, t.Mensagem, algorithm)
		if err != nil {
			return fmt.Errorf("process: sign: %w", err)
		}

		outBody := map[string]any{
			"uuid":       t.Token,
			"mensagem":   t.Mensagem,
			"assinatura": assinatura,
			"certChain":  certChain,
		}
		
		target = env.Servidor + t.EnviarPara
		
		if strings.Contains(target, ".seam") {
			// Old JSF endpoints expect form-data
			values := url.Values{}
			values.Set("uuid", t.Token)
			values.Set("token", t.Token)
			values.Set("mensagem", t.Mensagem)
			values.Set("assinatura", assinatura)
			values.Set("certChain", certChain)
			values.Set("cadeiaCertificado", certChain) // sending as string for form-data
			bodyReader = strings.NewReader(values.Encode())
			contentType = "application/x-www-form-urlencoded"
		} else {
			outJSON, err = json.Marshal(outBody)
			if err != nil {
				return fmt.Errorf("process: marshal out: %w", err)
			}
			bodyReader = bytes.NewReader(outJSON)
		}
	}

	// B-2: Validate the target URL to prevent SSRF via non-HTTP schemes
	// (file://, gopher://, ftp://, etc.). Only http and https are accepted.
	parsedTarget, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("process: invalid target URL %q: %w", target, err)
	}
	if s := parsedTarget.Scheme; s != "http" && s != "https" {
		return fmt.Errorf("process: target URL scheme %q is not allowed (only http/https)", s)
	}

	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bodyReader)
	if err != nil {
		return fmt.Errorf("process: new request: %w", err)
	}

	versao := env.Versao
	if versao == "" {
		versao = pjeVersion
	}

	outReq.Header.Set("versao", versao)
	outReq.Header.Set("Content-Type", contentType)
	outReq.Header.Set("Accept", "application/json, text/plain, */*")
	outReq.Header.Set("User-Agent", "PJeOffice/"+pjeVersion)
	outReq.Header.Set("Accept-Encoding", "gzip,deflate")
	if env.Sessao != "" {
		outReq.Header.Set("Cookie", env.Sessao)
	}
	if authHeader != "" {
		outReq.Header.Set("Authorization", authHeader)
	}

	resp, err := srv.httpClient.Do(outReq)
	if err != nil {
		return fmt.Errorf("process: remote post to %q: %w", target, err)
	}
	defer resp.Body.Close()
	
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10000))
	
	if successCodes[resp.StatusCode] {
		srv.log.Info("remote_post OK", "code", resp.StatusCode, "target", target, "body_snippet", string(respBody[:min(len(respBody), 1000)]))
		return nil
	}

	srv.log.Error("remote_post FAIL", "code", resp.StatusCode, "target", target, "body_snippet", string(respBody[:min(len(respBody), 1000)]))
	return fmt.Errorf("process: remote post %q returned %d", target, resp.StatusCode)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// applyCORS writes the CORS headers expected by the PJeOffice protocol.
// Mirrors _cors() in the Python reference.
func (srv *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Private-Network", "true")
	h.Set("Vary", "Origin")
}

// writeGIF writes a GIF response with the no-cache headers used by the Python reference.
func writeGIF(w http.ResponseWriter, blob []byte) {
	h := w.Header()
	h.Set("Content-Type", "image/gif")
	h.Set("Content-Length", fmt.Sprintf("%d", len(blob)))
	h.Set("Connection", "close")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// SetLogger replaces the default stderr logger. Useful in tests or when
// integrating with an existing audit logger.
func (srv *Server) SetLogger(l *slog.Logger) {
	srv.log = l
}

