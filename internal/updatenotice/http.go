package updatenotice

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path/filepath"
)

const maximumResponseBytes = 1 << 20

type cachedResponse struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Body         []byte `json:"body"`
}

type conditionalTransport struct {
	base      http.RoundTripper
	directory string
}

func (t *conditionalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if request.Method != http.MethodGet {
		return base.RoundTrip(request)
	}
	digest := sha256.Sum256([]byte(request.URL.String()))
	path := filepath.Join(t.directory, "http-"+hex.EncodeToString(digest[:])+".json")
	var cached cachedResponse
	_ = readJSON(path, &cached)
	request = request.Clone(request.Context())
	if cached.URL == request.URL.String() {
		addConditions(request, cached)
	}
	response, err := base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotModified && cached.URL == request.URL.String() {
		_ = response.Body.Close()
		response.StatusCode = http.StatusOK
		response.Status = "200 OK"
		response.Body = io.NopCloser(bytes.NewReader(cached.Body))
		response.ContentLength = int64(len(cached.Body))
		return response, nil
	}
	if response.StatusCode != http.StatusOK {
		return response, nil
	}
	return cacheResponse(path, request.URL.String(), response)
}

func addConditions(request *http.Request, cached cachedResponse) {
	if cached.ETag != "" && len(cached.ETag) <= 1024 {
		request.Header.Set("If-None-Match", cached.ETag)
	}
	if cached.LastModified != "" && len(cached.LastModified) <= 1024 {
		request.Header.Set("If-Modified-Since", cached.LastModified)
	}
}

func cacheResponse(path, url string, response *http.Response) (*http.Response, error) {
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > maximumResponseBytes {
		return nil, errors.New("release notice response exceeds size limit")
	}
	cache := cachedResponse{URL: url, ETag: response.Header.Get("ETag"), LastModified: response.Header.Get("Last-Modified"), Body: data}
	if err := writeJSON(path, cache); err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	return response, nil
}
