package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"downlink/cmd/solimen/internal/converter"
	"downlink/pkg/chromium"
	"downlink/pkg/models"

	log "github.com/sirupsen/logrus"
)

type Server struct {
	*http.Server
	scraper chromium.Scraper
}

func NewServer(addr string, scraper chromium.Scraper) *Server {
	s := &Server{scraper: scraper}
	mux := http.NewServeMux()
	mux.HandleFunc("/scrape", s.handleScrape)
	mux.HandleFunc("/health", s.handleHealth)
	s.Server = &http.Server{Addr: addr, Handler: mux}
	return s
}

type scrapeRequest struct {
	URL      string              `json:"url"`
	Triggers models.HostTriggers `json:"triggers"`
	// Formats selects which output formats to return.
	// Valid values: "html", "markdown", "pdf", "pdf-simplified".
	// Defaults to ["html"] when omitted.
	Formats []string `json:"formats"`
}

type scrapeResponse struct {
	State         string `json:"state"`
	HTML          string `json:"html,omitempty"`
	Markdown      string `json:"markdown,omitempty"`
	PDF           string `json:"pdf,omitempty"`            // base64-encoded PDF bytes
	PDFSimplified string `json:"pdf_simplified,omitempty"` // base64-encoded PDF bytes
}

type errorResponse struct {
	Error string `json:"error"`
}

type healthResponse struct {
	Status    string `json:"status"`
	Connected bool   `json:"connected"`
}

func (s *Server) handleScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}

	var req scrapeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "url is required"})
		return
	}

	formats := req.Formats
	if len(formats) == 0 {
		formats = []string{"html"}
	}
	formatSet := make(map[string]bool, len(formats))
	for _, f := range formats {
		formatSet[f] = true
	}

	result, err := s.scraper.Scrape(r.Context(), req.URL, req.Triggers)
	if err != nil {
		log.WithError(err).WithField("url", req.URL).Error("solimen: scrape failed")
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: err.Error()})
		return
	}

	resp := scrapeResponse{State: result.State}

	if formatSet["html"] {
		resp.HTML = result.HTML
	}

	if formatSet["markdown"] {
		md, err := converter.ToMarkdown(result.HTML, req.URL)
		if err != nil {
			log.WithError(err).WithField("url", req.URL).Error("solimen: markdown conversion failed")
			writeJSON(w, http.StatusBadGateway, errorResponse{Error: "markdown conversion: " + err.Error()})
			return
		}
		resp.Markdown = md
	}

	if formatSet["pdf"] {
		b, err := converter.ToPDF(result.HTML)
		if err != nil {
			log.WithError(err).WithField("url", req.URL).Error("solimen: pdf conversion failed")
			writeJSON(w, http.StatusBadGateway, errorResponse{Error: "pdf conversion: " + err.Error()})
			return
		}
		resp.PDF = base64.StdEncoding.EncodeToString(b)
	}

	if formatSet["pdf-simplified"] {
		b, err := converter.ToPDFSimplified(result.HTML, req.URL)
		if err != nil {
			log.WithError(err).WithField("url", req.URL).Error("solimen: pdf-simplified conversion failed")
			writeJSON(w, http.StatusBadGateway, errorResponse{Error: "pdf-simplified conversion: " + err.Error()})
			return
		}
		resp.PDFSimplified = base64.StdEncoding.EncodeToString(b)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	connected := s.scraper.Connected()
	status := http.StatusOK
	state := "ok"
	if !connected {
		status = http.StatusServiceUnavailable
		state = "degraded"
	}
	writeJSON(w, status, healthResponse{
		Status:    state,
		Connected: connected,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
