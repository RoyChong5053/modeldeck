package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"modeldeck/internal/config"
	"modeldeck/internal/registry"
	"modeldeck/internal/sched"
)

const (
	maxJSONBody      = 32 << 20
	maxMultipartBody = 512 << 20
)

type targetKey struct{}

// Service is the data plane: OpenAI-ish endpoints plus the generic /run route.
type Service struct {
	cfg *config.Config
	reg *registry.Registry
	sch *sched.Scheduler

	rp        *httputil.ReverseProxy
	transport *http.Transport
}

func New(cfg *config.Config, reg *registry.Registry, sch *sched.Scheduler) *Service {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	s := &Service{cfg: cfg, reg: reg, sch: sch, transport: tr}
	s.rp = &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := pr.In.Context().Value(targetKey{}).(string)
			u, err := url.Parse(target)
			if err != nil {
				return
			}
			pr.SetURL(u)
			pr.Out.Host = u.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				resp.Header.Set("X-Accel-Buffering", "no")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				status = 499
			}
			writeErr(w, status, "upstream error: "+err.Error())
		},
	}
	return s
}

var standardPOST = map[string]bool{
	"/v1/chat/completions":     true,
	"/v1/completions":          true,
	"/v1/responses":            true,
	"/v1/messages":             true,
	"/v1/embeddings":           true,
	"/v1/rerank":               true,
	"/v1/reranking":            true,
	"/rerank":                  true,
	"/v1/audio/speech":         true,
	"/v1/audio/transcriptions": true,
	"/v1/images/generations":   true,
	"/sdapi/v1/txt2img":        true,
	"/sdapi/v1/img2img":        true,
	"/audioapi/v1/tasks/run":   true,
}

// ServeHTTP dispatches the data plane.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/v1/models" || p == "/models":
		if r.Method == http.MethodGet {
			s.listModels(w, r)
			return
		}
	case strings.HasPrefix(p, "/run/"):
		s.handleRun(w, r)
		return
	case standardPOST[p]:
		s.handleModel(w, r)
		return
	case p == "/health":
		writeJSON(w, 200, map[string]any{"status": "ok"})
		return
	}
	writeErr(w, http.StatusNotFound, "no route for "+p)
}

func (s *Service) handleModel(w http.ResponseWriter, r *http.Request) {
	model, cleanup, err := s.extractModel(r)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if model == "" {
		writeErr(w, http.StatusBadRequest, "model is required (send X-Deck-Model, ?model=, or a model field)")
		return
	}
	e, ok := s.reg.ByModel(model)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("unknown model %q", model))
		return
	}
	rt, err := s.sch.Acquire(r.Context(), e)
	if err != nil {
		s.acquireError(w, err)
		return
	}
	defer s.sch.Release(rt)
	if e.UpstreamModel != "" && e.UpstreamModel != model {
		cleanup, err := rewriteModel(r, e.UpstreamModel)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "rewrite model: "+err.Error())
			return
		}
	}
	s.forward(w, r, e.BaseURL(), "")
}

func (s *Service) handleRun(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/run/")
	id := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id = rest[:i]
	}
	e, ok := s.reg.ByID(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("unknown entry %q", id))
		return
	}
	rt, err := s.sch.Acquire(r.Context(), e)
	if err != nil {
		s.acquireError(w, err)
		return
	}
	defer s.sch.Release(rt)
	s.forward(w, r, e.BaseURL(), "/run/"+id)
}

func (s *Service) acquireError(w http.ResponseWriter, err error) {
	var nf *sched.NoFitError
	if errors.As(err, &nf) {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusServiceUnavailable, nf.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	writeErr(w, http.StatusBadGateway, err.Error())
}

func (s *Service) forward(w http.ResponseWriter, r *http.Request, base, stripPrefix string) {
	if stripPrefix != "" {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, stripPrefix)
		r.URL.RawPath = ""
	}
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	ctx := context.WithValue(r.Context(), targetKey{}, base)
	s.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (s *Service) listModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(s.reg.Entries())*2)
	for _, e := range s.reg.Entries() {
		if e.Mode == "job" {
			continue
		}
		data = append(data, model{ID: e.ID, Object: "model", OwnedBy: "modeldeck"})
		for _, a := range e.Aliases {
			if a != "" {
				data = append(data, model{ID: a, Object: "model", OwnedBy: "modeldeck"})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Service) extractModel(r *http.Request) (string, func(), error) {
	if v := r.Header.Get("X-Deck-Model"); v != "" {
		return v, nil, nil
	}
	if v := r.URL.Query().Get("model"); v != "" {
		return v, nil, nil
	}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		b, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody))
		r.Body.Close()
		if err != nil {
			return "", nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &probe)
		return probe.Model, nil, nil
	case strings.HasPrefix(ct, "multipart/form-data"):
		return s.extractMultipart(r, ct)
	}
	return "", nil, nil
}

func (s *Service) extractMultipart(r *http.Request, ct string) (string, func(), error) {
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return "", nil, nil
	}
	f, err := os.CreateTemp("", "deck-body-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, maxMultipartBody+1))
	r.Body.Close()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if n > maxMultipartBody {
		cleanup()
		return "", nil, fmt.Errorf("request body too large")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return "", nil, err
	}

	model := ""
	mr := multipart.NewReader(f, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			model = strings.TrimSpace(string(b))
			part.Close()
			break
		}
		part.Close()
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return "", nil, err
	}
	r.Body = f
	r.ContentLength = n
	return model, cleanup, nil
}

// rewriteModel replaces the request's `model` field with upstream, keeping the
// wire format (JSON or multipart) intact. It returns an optional cleanup func
// and a nil error when the body needs no change.
func rewriteModel(r *http.Request, upstream string) (func(), error) {
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		b, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody))
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		// Restore the original body first so the no-op paths stay lossless.
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(b, &body); err != nil {
			return nil, err
		}
		if _, ok := body["model"]; !ok {
			return nil, nil
		}
		raw, err := json.Marshal(upstream)
		if err != nil {
			return nil, err
		}
		body["model"] = raw
		nb, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(nb))
		r.ContentLength = int64(len(nb))
		r.Header.Del("Content-Length")
		return nil, nil
	case strings.HasPrefix(ct, "multipart/form-data"):
		return rewriteMultipartModel(r, ct, upstream)
	}
	return nil, nil
}

// rewriteMultipartModel rebuilds a multipart body with a different `model`
// value, reusing the original boundary. The body was already spooled to a
// temp file by extractMultipart, so this is a disk-to-disk copy.
func rewriteMultipartModel(r *http.Request, ct, upstream string) (func(), error) {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil || params["boundary"] == "" {
		return nil, nil
	}
	boundary := params["boundary"]
	in, ok := r.Body.(io.ReadSeeker)
	if !ok {
		return nil, fmt.Errorf("multipart body is not seekable")
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	out, err := os.CreateTemp("", "deck-body-rewrite-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		out.Close()
		os.Remove(out.Name())
	}
	mw := multipart.NewWriter(out)
	if err := mw.SetBoundary(boundary); err != nil {
		cleanup()
		return nil, err
	}
	mr := multipart.NewReader(in, boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		pw, err := mw.CreatePart(part.Header)
		if err != nil {
			part.Close()
			cleanup()
			return nil, err
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(pw, upstream)
		} else {
			_, err = io.Copy(pw, part)
		}
		part.Close()
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		cleanup()
		return nil, err
	}
	st, err := out.Stat()
	if err != nil {
		cleanup()
		return nil, err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	r.Body = out
	r.ContentLength = st.Size()
	r.Header.Del("Content-Length")
	return cleanup, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	if code == 499 { // client closed; nothing sensible to send
		return
	}
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "modeldeck_error"}})
}
